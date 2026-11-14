// tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and dials mapped tokens in-process through the
// tailcat library; targets not found in dns.txt (plain domains and IP
// literals) are dialed directly. See README.md for usage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// version 由构建时的 -ldflags "-X main.version=..." 注入；版本号本身只
// 存在于仓库根的 .version 文件（CI 从那里读并传进来）。直接 go build 时
// 保持 "dev"，不猜版本。
var version = "dev"

func main() {
	var (
		listen        = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (host:port; empty host binds all interfaces)")
		dnsFile       = flag.String("dns-file", "data/dns.txt", "domain->token mapping file")
		clientKeyFile = flag.String("client-key-file", "data/tailcat-client-key", "persistent tailcat client identity (JSON)")
		adminAddr     = flag.String("admin-addr", "127.0.0.1:8080", "admin page/API address (host:port; empty disables)")
		noWatch       = flag.Bool("no-watch", false, "disable dns.txt hot-reload")
		logLevel      = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("component", "tailcat-socks"))
	// flag.Visit reports only flags actually set on the command line, so an
	// explicit flag keeps priority even when its value equals the default.
	// Env fallbacks: TAILCAT_CLIENT_KEY_FILE, ADMIN_ADDR ("" disables).
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	os.Exit(run(*listen, *dnsFile,
		resolveSetting(explicit["client-key-file"], "TAILCAT_CLIENT_KEY_FILE", *clientKeyFile),
		resolveSetting(explicit["admin-addr"], "ADMIN_ADDR", *adminAddr),
		*noWatch))
}

func run(listen, dnsFile, clientKeyFile, adminAddr string, noWatch bool) int {
	// Install the signal handler before doing anything else: stopNotify is
	// deferred FIRST so it runs LAST — the handler stays installed until
	// run() actually returns, so a repeat Ctrl-C during shutdown is
	// swallowed instead of hitting the default kill disposition
	// mid-cleanup.
	notifyCtx, stopNotify := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopNotify()
	ctx, cancel := context.WithCancel(notifyCtx)
	defer cancel()

	first, err := LoadDNSFile(dnsFile)
	if err != nil {
		slog.Error("cannot load dns file", "path", dnsFile, "err", err)
		return 1
	}
	dnsMap := &DNSMap{}
	dnsMap.Store(first)

	// Validate --listen: SplitHostPort for the host:port shape plus a numeric
	// in-range port, so junk like "host:abc" is reported here instead of by
	// net.Listen later. An empty host (":8080") binds every interface. The
	// "(want host:port)" hint belongs to the shape error only, not the port.
	lHost, lPort, err := net.SplitHostPort(listen)
	if err != nil {
		slog.Error("bad --listen", "value", listen, "err", fmt.Errorf("%q: %w (want host:port)", listen, err))
		return 1
	}
	if p, perr := strconv.Atoi(lPort); perr != nil {
		slog.Error("bad --listen", "value", listen, "err", fmt.Errorf("%q: %w", listen, perr))
		return 1
	} else if p < 0 || p > 65535 {
		slog.Error("bad --listen", "value", listen, "err", fmt.Errorf("%q: port %d out of range", listen, p))
		return 1
	}
	listen = net.JoinHostPort(lHost, lPort)

	// The client key cannot break startup: a corrupt or unwritable file only
	// WARNs and falls back to an ephemeral identity (dialer.go), never
	// clobbers the file.
	dialer := NewAppDialer(clientKeyFile)
	registry := NewConnRegistry()
	srv, err := NewServer(dnsMap, dialer, registry, listen)
	if err != nil {
		slog.Error("cannot listen", "addr", listen, "err", err)
		return 1
	}

	// Admin page (spec §8): an empty addr disables it entirely; a failed
	// bind is fail-fast — a dead admin means the config/deploy is broken,
	// and silently running without the page is worse.
	var adminSrv *http.Server
	if adminAddr != "" {
		api := NewAdmin(registry, dnsMap, dnsFile)
		adminLn, err := net.Listen("tcp", adminAddr)
		if err != nil {
			slog.Error("cannot listen admin", "addr", adminAddr, "err", err)
			srv.Close()
			return 1
		}
		adminSrv = &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server error", "err", err)
			}
		}()
		slog.Info("admin listening", "url", "http://"+adminAddr+"/")
	}

	if !noWatch {
		go WatchDNSFile(ctx, dnsFile, dnsMap)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	host, port, _ := net.SplitHostPort(srv.ActualAddr().String())
	slog.Info("listening", "url", "socks5h://"+net.JoinHostPort(host, port))
	slog.Info("dns map loaded", "domains", len(first), "tokens", len(tokenSet(first)))

	code := 0
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		// srv.Close() during shutdown makes Serve() report net.ErrClosed;
		// that is not a real failure, so keep signal exits quiet. Any other
		// error means the listener broke after a successful bind — that is
		// worth a nonzero exit.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("server error", "err", err)
			code = 1
		}
	}
	cancel()    // stops the dns watcher
	srv.Close() // stop accepting; in-flight relays end on their own
	if adminSrv != nil {
		// Graceful: finish in-flight requests, hard-stop after 1s
		// (spec §10). context.Background: notifyCtx is already canceled.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutCancel()
		if err := adminSrv.Shutdown(shutCtx); err != nil {
			slog.Error("admin shutdown", "err", err)
		}
	}
	dialer.Close() // tailcat clients go last (spec §10 shutdown order)
	return code
}

// resolveSetting implements the flag > env > default precedence used by
// every env-configurable knob. `explicit` comes from flag.Visit, so a flag
// set to its default value on the command line still wins; LookupEnv (not
// Getenv) makes an empty env value count as set — ADMIN_ADDR="" disables.
func resolveSetting(explicit bool, envName, flagVal string) string {
	if explicit {
		return flagVal
	}
	if v, ok := os.LookupEnv(envName); ok {
		return v
	}
	return flagVal
}
