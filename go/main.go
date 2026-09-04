// tailcat-dns-proxy: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
// upstream. This is a behavioral replica of the Python version
// (python/tailcat_dns_proxy.py); see README.md for usage.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0) // messages already carry the [tailcat-dns-proxy] prefix
	var (
		listen       = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen addr:port")
		dnsFile      = flag.String("dns-file", "dns.txt", "domain->token mapping file")
		upstream     = flag.String("upstream", "127.0.0.1:0", "upstream tailcat socks addr:port; port 0 (default) = high random free port")
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
		log.Printf("[tailcat-dns-proxy] cannot load %s: %v", dnsFile, err)
		return 1
	}
	dnsMap := &DNSMap{}
	dnsMap.Store(first)

	// Normalize --listen with Python's parse_addr semantics: a bare port or an
	// empty host means loopback, so ":8080" cannot bind every interface, and
	// bracketed IPv6 comes out well-formed for net.Listen.
	lHost, lPort, err := parseAddr(listen, 0)
	if err != nil {
		log.Printf("[tailcat-dns-proxy] bad --listen: %v", err)
		return 1
	}
	listen = net.JoinHostPort(strings.Trim(lHost, "[]"), strconv.Itoa(lPort))

	// Resolve the tailcat socks listen port: explicit port wins, else high random.
	upHost, upPort, err := parseAddr(upstream, 0)
	if err != nil {
		log.Printf("[tailcat-dns-proxy] bad --upstream: %v", err)
		return 1
	}
	if upPort == 0 {
		upPort = freeHighPort(strings.Trim(upHost, "[]"))
	}
	upAddr := net.JoinHostPort(strings.Trim(upHost, "[]"), strconv.Itoa(upPort))

	srv, err := NewServer(dnsMap, upAddr, listen)
	if err != nil {
		log.Printf("[tailcat-dns-proxy] cannot listen on %s: %v", listen, err)
		return 1
	}

	var child *exec.Cmd
	if !noAutolaunch {
		child = spawnTailcatSocks(tailcatBin, upHost, upPort)
		if child == nil {
			srv.Close()
			return 1
		}
		if !waitReady(upAddr, 15*time.Second) {
			log.Printf("[tailcat-dns-proxy] upstream %s not ready; aborting", upAddr)
			terminate(child)
			srv.Close()
			return 1
		}
		log.Printf("[tailcat-dns-proxy] auto-launched %s socks on %s", tailcatBin, upAddr)
	}

	stopWatch := make(chan struct{})
	if !noWatch {
		go WatchDNSFile(dnsFile, dnsMap, time.Second, stopWatch)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	host, port, _ := net.SplitHostPort(srv.ActualAddr().String())
	log.Printf("[tailcat-dns-proxy] listening socks5h://%s:%s", host, port)
	log.Printf("[tailcat-dns-proxy] %d domain(s) mapped -> %d token(s)",
		len(first), len(tokenSet(first)))
	log.Printf("[tailcat-dns-proxy] upstream %s", upAddr)

	select {
	case <-sig:
	case err := <-serveErr:
		// srv.Close() during shutdown makes Serve() report net.ErrClosed;
		// that is not a real failure, so keep signal exits quiet.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("[tailcat-dns-proxy] server error: %v", err)
		}
	}
	close(stopWatch)
	srv.Close()
	terminate(child)
	return 0
}

// parseAddr splits "host:port" with Python's rpartition(":") semantics:
// everything after the last colon is the port, everything before is the host
// (empty -> 127.0.0.1); a string with no colon is treated as the port part.
func parseAddr(s string, defaultPort int) (string, int, error) {
	host, portStr := "", s // no colon: Python rpartition puts the whole string in the port part
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, portStr = s[:i], s[i+1:]
	}
	port := defaultPort
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return "", 0, fmt.Errorf("bad addr %q: %w", s, err)
		}
		port = p
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

// freeHighPort returns a free high port (ephemeral range, randomly probed,
// mirroring the Python version), falling back to an OS-assigned port.
func freeHighPort(host string) int {
	for i := 0; i < 20; i++ {
		p := 20000 + rand.Intn(41000) // 20000..60999
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			ln.Close()
			return p
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		log.Fatalf("[tailcat-dns-proxy] cannot find a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// spawnTailcatSocks launches `tailcat socks --listen=host:port` and returns
// the cmd, or nil on failure. Child output goes to our stderr so it lands in
// the same log file.
func spawnTailcatSocks(binPath, host string, port int) *exec.Cmd {
	cmd := exec.Command(binPath, "socks", fmt.Sprintf("--listen=%s:%d", host, port))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[tailcat-dns-proxy] failed to launch %s: %v", binPath, err)
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
