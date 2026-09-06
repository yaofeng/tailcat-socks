// tailcat-dns-proxy: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
// upstream. This is a behavioral replica of the Python version
// (python/tailcat_dns_proxy.py); see README.md for usage.
package main

import (
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
	// Install the signal handler before doing anything else, so SIGINT/SIGTERM
	// arriving during launch cannot orphan the tailcat child.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

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

	var child *exec.Cmd
	if !noAutolaunch {
		child = spawnTailcatSocks(tailcatBin, upAddr)
		if child == nil {
			srv.Close()
			return 1
		}
		if !waitReady(upAddr, 15*time.Second) {
			slog.Error("upstream not ready; aborting", "addr", upAddr)
			terminate(child)
			srv.Close()
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
	case <-sig:
	case err := <-serveErr:
		// srv.Close() during shutdown makes Serve() report net.ErrClosed;
		// that is not a real failure, so keep signal exits quiet.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("server error", "err", err)
		}
	}
	close(stopWatch)
	srv.Close()
	terminate(child)
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

// spawnTailcatSocks launches `tailcat socks --listen=addr` and returns the
// cmd, or nil on failure. addr is passed verbatim, so IPv6 keeps its brackets.
// Child output goes to our stderr so it lands in the same log file.
func spawnTailcatSocks(binPath, addr string) *exec.Cmd {
	cmd := exec.Command(binPath, "socks", "--listen="+addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("failed to launch tailcat socks", "bin", binPath, "err", err)
		return nil
	}
	return cmd
}

// terminate stops the child: SIGTERM, then SIGKILL after 5s.
func terminate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck — best effort
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck — best effort
		<-done
	}
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
