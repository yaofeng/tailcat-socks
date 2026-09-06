// tailcat-dns-proxy: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
// upstream. This is a behavioral replica of the Python version
// (python/tailcat_dns_proxy.py); see README.md for usage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "tailcat-dns-proxy"))
	var (
		listen       = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (host:port; empty host binds all interfaces)")
		dnsFile      = flag.String("dns-file", "dns.txt", "domain->token mapping file")
		upstream     = flag.String("upstream", "127.0.0.1:0", "upstream tailcat socks address (host:port; empty host binds all interfaces; port 0 = OS-assigned free port)")
		tailcatBin   = flag.String("tailcat-bin", "tailcat", "path to tailcat binary (auto-launched)")
		noAutolaunch = flag.Bool("no-autolaunch", false, "do not spawn tailcat socks; use an already-running upstream")
		noWatch      = flag.Bool("no-watch", false, "disable dns.txt hot-reload")
	)
	flag.Parse()
	os.Exit(run(*listen, *dnsFile, *upstream, *tailcatBin, *noAutolaunch, *noWatch))
}

func run(listen, dnsFile, upstream, tailcatBin string, noAutolaunch, noWatch bool) int {
	// Install the signal handler before doing anything else: the child's life
	// is bound to ctx, so SIGINT/SIGTERM arriving during launch must already
	// cancel it — otherwise the tailcat child would be orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	// Resolve the upstream tailcat socks address: explicit port wins, port 0
	// gets an OS-assigned free port.
	upAddr, err := upstreamAddr(upstream)
	if err != nil {
		slog.Error("bad --upstream", "value", upstream, "err", err)
		return 1
	}

	srv, err := NewServer(dnsMap, upAddr, listen)
	if err != nil {
		slog.Error("cannot listen", "addr", listen, "err", err)
		return 1
	}

	var (
		child     *exec.Cmd
		childErr  error
		childDone chan struct{} // closed by the monitor goroutine once Wait returns
	)
	if !noAutolaunch {
		child, err = spawnTailcatSocks(ctx, tailcatBin, upAddr)
		if err != nil {
			slog.Error("failed to launch tailcat socks", "bin", tailcatBin, "err", err)
			srv.Close()
			return 1
		}
		// Reap the child in the background — cmd.Wait must always run, both
		// to release its resources and to let exec's Cancel/WaitDelay
		// escalation complete. Writing childErr before closing childDone is
		// the happens-before edge that makes reading childErr race-free.
		childDone = make(chan struct{})
		go func() {
			childErr = child.Wait()
			close(childDone)
		}()
		if !waitReady(upAddr, 15*time.Second) {
			slog.Error("upstream not ready; aborting", "addr", upAddr)
			stop() // cancels ctx: SIGTERM to the child, SIGKILL after childKillGrace
			srv.Close()
			if child != nil {
				<-childDone
			}
			return 1
		}
		slog.Info("auto-launched tailcat socks", "bin", tailcatBin, "addr", upAddr)
	}

	stopWatch := make(chan struct{})
	if !noWatch {
		go WatchDNSFile(dnsFile, dnsMap, time.Second, stopWatch)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	host, port, _ := net.SplitHostPort(srv.ActualAddr().String())
	slog.Info("listening", "url", "socks5h://"+net.JoinHostPort(host, port))
	slog.Info("dns map loaded", "domains", len(first), "tokens", len(tokenSet(first)))
	slog.Info("using upstream", "addr", upAddr)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		// srv.Close() during shutdown makes Serve() report net.ErrClosed;
		// that is not a real failure, so keep signal exits quiet.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("server error", "err", err)
		}
	case <-childDone:
		// The tailcat child died on its own; without an upstream we are
		// useless, so surface why and exit nonzero. (childDone is nil in
		// --no-autolaunch mode, which a select never readies on.)
		if childErr != nil {
			slog.Error("tailcat socks exited", "err", childErr)
		} else {
			slog.Error("tailcat socks exited unexpectedly")
		}
		srv.Close()
		return 1
	}
	stop() // cancels ctx: SIGTERM to the child (then SIGKILL), harmless without one
	close(stopWatch)
	srv.Close()
	if child != nil {
		<-childDone // reap the child before exiting so it cannot outlive us
	}
	return 0
}

// upstreamAddr validates the --upstream flag value and returns the address to
// dial. A port of 0 asks the OS for a free port and returns the concrete
// address that was probed (there is an unavoidable race between closing the
// probe listener and the child binding that port, accepted because the window
// is tiny and failure is reported normally).
func upstreamAddr(s string) (string, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("%q: %w (want host:port)", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("%q: port %d out of range", s, port)
	}
	if port != 0 {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", fmt.Errorf("pick free port on %q: %w", host, err)
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

// childKillGrace is how long a canceled spawn waits for the child to honor
// SIGTERM before exec escalates to SIGKILL.
const childKillGrace = 5 * time.Second

// spawnTailcatSocks launches `tailcat socks --listen=addr` bound to ctx: when
// ctx is canceled the child gets SIGTERM, escalating to SIGKILL after
// childKillGrace (both handled by exec.CommandContext; the caller must still
// call cmd.Wait to reap it). addr is passed verbatim, so IPv6 keeps its
// brackets. Child output goes to our stderr so it lands in the same log file.
func spawnTailcatSocks(ctx context.Context, binPath, addr string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, binPath, "socks", "--listen="+addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = childKillGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch %s: %w", binPath, err)
	}
	return cmd, nil
}

// waitReady polls TCP-connect to addr until it accepts or the timeout hits.
func waitReady(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
