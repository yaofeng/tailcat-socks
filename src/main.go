// tailcat-proxy: a SOCKS5 front proxy that rewrites real domain names to
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
	"path/filepath"
	"syscall"
	"time"
)

// version 由构建时的 -ldflags "-X main.version=..." 注入；版本号本身只
// 存在于仓库根的 .version 文件（CI 从那里读并传进来）。直接 go build 时
// 保持 "dev"，不猜版本。
var version = "dev"

// dataFiles 是把 --data-path 展开成的那一组固定文件名。所有需要持久化的
// 东西都从这一个目录里取，不再为每个文件单开一个 flag：路径全部相对 cwd
// 时，一旦换个目录启动就会一半文件读旧位置、一半写新位置，历史更是会悄悄
// 建到别处。一个 --data-path 定死全部。
type dataFiles struct {
	dir       string
	dns       string
	config    string
	clientKey string
	history   string
}

// newDataFiles 把目录展开成各文件的绝对路径。绝对路径是刻意的：后面所有
// 读写都以它为基准，不再受进程 cwd 影响。
func newDataFiles(dir string) (dataFiles, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dataFiles{}, fmt.Errorf("resolve --data-path %q: %w", dir, err)
	}
	return dataFiles{
		dir:       abs,
		dns:       filepath.Join(abs, "dns.txt"),
		config:    filepath.Join(abs, "config.yaml"),
		clientKey: filepath.Join(abs, "tailcat-client-key"),
		history:   filepath.Join(abs, "history.db"),
	}, nil
}

func main() {
	var (
		dataPath    = flag.String("data-path", "data", "directory holding dns.txt, config.yaml, tailcat-client-key and history.db")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print version and exit")
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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("component", "tailcat-proxy"))
	files, err := newDataFiles(*dataPath)
	if err != nil {
		slog.Error("bad --data-path", "value", *dataPath, "err", err)
		os.Exit(1)
	}
	os.Exit(run(files))
}

func run(files dataFiles) int {
	// Install the signal handler before doing anything else: stopNotify is
	// deferred FIRST so it runs LAST — the handler stays installed until
	// run() actually returns, so a repeat Ctrl-C during shutdown is
	// swallowed instead of hitting the default kill disposition
	// mid-cleanup.
	notifyCtx, stopNotify := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopNotify()
	ctx, cancel := context.WithCancel(notifyCtx)
	defer cancel()

	first, err := LoadDNSFile(files.dns)
	if err != nil {
		slog.Error("cannot load dns file", "path", files.dns, "err", err)
		return 1
	}
	dnsMap := &DNSMap{}
	dnsMap.Store(first)

	// 认证是强制项，配置必须在监听之前就位：解析失败或用户表不合法时
	// 直接退出，不能先开端口再发现没人能连。
	cfg, err := LoadConfig(files.config)
	if err != nil {
		slog.Error("cannot load config", "path", files.config, "err", err)
		return 1
	}
	users := make(Users, len(cfg.Users))
	for name, pw := range cfg.Users {
		// LoadConfig 已经校验过用户名的合法性，这里不会失败；仍检查是因为
		// 授权范围不允许"猜一个默认值"。
		mode, merr := modeForUser(name)
		if merr != nil {
			slog.Error("bad config", "path", files.config, "err", merr)
			return 1
		}
		users[name] = User{password: pw, Mode: mode}
	}

	// 历史是旁路功能：打不开就降级为不可用并继续服务，绝不因此拒绝启动。
	// 库的位置由 --data-path 决定，不读配置：路径只该有一个来源，否则
	// config.yaml 里写相对路径又会按 cwd 解析到别处。
	var history *History
	if cfg.History.Enabled {
		history, err = OpenHistory(files.history)
		if err != nil {
			slog.Warn("history disabled: cannot open database", "path", files.history, "err", err)
			history = nil
		}
	}
	if history != nil {
		defer history.Close()
	}

	// The client key cannot break startup: a corrupt or unwritable file only
	// WARNs and falls back to an ephemeral identity (dialer.go), never
	// clobbers the file.
	dialer := NewAppDialer(files.clientKey)
	registry := NewConnRegistry()

	// 三个入口都先绑定再启动：任一个端口被占都是部署错误，静默少起一个
	// 入口比直接报错更糟。
	var (
		socksSrv  *Server
		httpSrv   *HTTPProxy
		haveProxy bool
	)
	if cfg.Listen.SOCKSProxy != "" {
		socksSrv, err = NewServerWithUsers(dnsMap, dialer, registry, cfg.Listen.SOCKSProxy, users, history)
		if err != nil {
			slog.Error("cannot listen socks", "addr", cfg.Listen.SOCKSProxy, "err", err)
			return 1
		}
		haveProxy = true
	}
	if cfg.Listen.HTTPProxy != "" {
		httpSrv, err = NewHTTPProxy(dnsMap, dialer, registry, cfg.Listen.HTTPProxy, users, history)
		if err != nil {
			slog.Error("cannot listen http", "addr", cfg.Listen.HTTPProxy, "err", err)
			if socksSrv != nil {
				socksSrv.Close()
			}
			return 1
		}
		haveProxy = true
	}
	// 只有管理页的配置没有意义：没有任何代理入口等于这个进程什么都不做。
	if !haveProxy {
		slog.Error("no proxy entry point configured; set listen.socks-proxy and/or listen.http-proxy in config.yaml")
		return 1
	}

	// Admin page (spec §8): an empty addr disables it entirely; a failed
	// bind is fail-fast — a dead admin means the config/deploy is broken,
	// and silently running without the page is worse.
	var adminSrv *http.Server
	if cfg.Listen.WebConsole != "" {
		api := NewAdmin(registry, dnsMap, files.dns)
		api.SetHistory(history)
		adminLn, err := net.Listen("tcp", cfg.Listen.WebConsole)
		if err != nil {
			slog.Error("cannot listen admin", "addr", cfg.Listen.WebConsole, "err", err)
			if socksSrv != nil {
				socksSrv.Close()
			}
			if httpSrv != nil {
				httpSrv.Close()
			}
			return 1
		}
		adminSrv = &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server error", "err", err)
			}
		}()
		slog.Info("admin listening", "url", "http://"+cfg.Listen.WebConsole+"/")
	}

	// dns.txt 的热加载不再可关（--no-watch 已删除）：它是这个代理的核心
	// 交互，"改了文件不生效"只会被当成 bug。
	go WatchDNSFile(ctx, files.dns, dnsMap)

	serveErr := make(chan error, 2)
	if socksSrv != nil {
		go func() { serveErr <- socksSrv.Serve() }()
		host, port, _ := net.SplitHostPort(socksSrv.ActualAddr().String())
		slog.Info("socks listening", "url", "socks5h://"+net.JoinHostPort(host, port))
	}
	if httpSrv != nil {
		go func() { serveErr <- httpSrv.Serve() }()
		host, port, _ := net.SplitHostPort(httpSrv.ActualAddr().String())
		slog.Info("http listening", "url", "http://"+net.JoinHostPort(host, port))
	}
	slog.Info("dns map loaded", "domains", len(first), "tokens", len(tokenSet(first)))

	code := 0
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		// Close() during shutdown makes Serve() report net.ErrClosed; that is
		// not a real failure, so keep signal exits quiet. Any other error
		// means a listener broke after a successful bind — that is worth a
		// nonzero exit.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("server error", "err", err)
			code = 1
		}
	}
	cancel() // stops the dns watcher
	if socksSrv != nil {
		socksSrv.Close()
	}
	if httpSrv != nil {
		httpSrv.Close()
	}
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
