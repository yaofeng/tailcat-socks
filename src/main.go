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
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "tailcat-socks"))
	var (
		listen        = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (host:port; empty host binds all interfaces)")
		dnsFile       = flag.String("dns-file", "data/dns.txt", "domain->token mapping file")
		clientKeyFile = flag.String("client-key-file", "data/tailcat-client-key", "persistent tailcat client identity (JSON)")
		noWatch       = flag.Bool("no-watch", false, "disable dns.txt hot-reload")
	)
	flag.Parse()
	os.Exit(run(*listen, *dnsFile, *clientKeyFile, *noWatch))
}

func run(listen, dnsFile, clientKeyFile string, noWatch bool) int {
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
	cancel()       // stops the dns watcher
	srv.Close()    // stop accepting; in-flight relays end on their own
	dialer.Close() // tailcat clients go last (spec §10 shutdown order)
	return code
}
