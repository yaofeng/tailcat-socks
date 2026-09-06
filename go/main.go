// tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
// upstream. This is a behavioral replica of the Python version
// (python/tailcat_socks.py); see README.md for usage.
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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "tailcat-socks"))
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
	//
	// Two layers: notifyCtx is canceled by SIGINT/SIGTERM, and ctx is derived
	// from it plus our own cancel() calls. stopNotify is deferred FIRST so it
	// runs LAST: the handler stays installed until run() actually returns, so
	// repeat signals keep being swallowed while we stop and reap the child —
	// a second Ctrl-C must never hit the default disposition and kill us
	// mid-reap, orphaning the child.
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
			// A signal landing before the spawn shows up here as a start
			// failure (exec refuses to fork with an already-canceled ctx, so
			// there is no child to reap): that is a quiet signal exit, not a
			// launch failure.
			if ctx.Err() != nil {
				srv.Close()
				return 0
			}
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
		if !waitReady(ctx, upAddr, 15*time.Second) {
			// Two ways to land here: a signal arrived mid-wait (ctx is
			// canceled, the child is already being stopped — a clean, quiet
			// shutdown, exit 0), or the child genuinely failed to come up
			// (loud, exit 1).
			code := 0
			if ctx.Err() == nil {
				slog.Error("upstream not ready; aborting", "addr", upAddr)
				code = 1
			}
			cancel() // SIGTERM to the child, SIGKILL after childKillGrace
			srv.Close()
			if child != nil {
				<-childDone
			}
			return code
		}
		slog.Info("auto-launched tailcat socks", "bin", tailcatBin, "addr", upAddr)
	}

	if !noWatch {
		go WatchDNSFile(ctx, dnsFile, dnsMap)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	host, port, _ := net.SplitHostPort(srv.ActualAddr().String())
	slog.Info("listening", "url", "socks5h://"+net.JoinHostPort(host, port))
	slog.Info("dns map loaded", "domains", len(first), "tokens", len(tokenSet(first)))
	slog.Info("using upstream", "addr", upAddr)

	// Determinism when events race: if the child is already gone by the time
	// we reach the select, attribute the shutdown to the child even if a
	// signal arrived too — otherwise select would pick between the two cases
	// at random and the exit code would be a coin flip.
	code := 0
	select {
	case <-childDone:
		logChildExit(child, childErr)
		code = 1
	default:
	}

	// Only wait for events if the pre-check did not already attribute the
	// shutdown: with a dead child the select's childDone case is the only
	// ready one, and running it would log the child's exit twice.
	if code == 0 {
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
			logChildExit(child, childErr)
			code = 1
		}
	}
	cancel() // stops the child if alive: SIGTERM, then SIGKILL; also stops the dns watcher
	srv.Close()
	if child != nil {
		<-childDone // reap the child before exiting so it cannot outlive us
	}
	return code
}

// logChildExit reports the tailcat child's death. It must only be called once
// childDone is closed: that close is the happens-before edge ordering
// childErr and child.ProcessState (both written by Wait) before any read.
func logChildExit(child *exec.Cmd, childErr error) {
	if childErr != nil {
		slog.Error("tailcat socks exited", "err", childErr)
	} else {
		slog.Error("tailcat socks exited unexpectedly", "code", child.ProcessState.ExitCode())
	}
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

// waitReady polls TCP-connect to addr until it accepts, the timeout hits, or
// ctx is canceled — cancellation returns promptly so a shutdown during launch
// is not held hostage by the remaining deadline.
func waitReady(ctx context.Context, addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	d := net.Dialer{Timeout: time.Second}
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if !time.Now().Before(deadline) {
			return false
		}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}
