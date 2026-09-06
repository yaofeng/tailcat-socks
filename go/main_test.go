package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeTailcatBin is set by TestMain after building the test helper binary.
var fakeTailcatBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fake-tailcat")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeTailcatBin = filepath.Join(dir, "fake-tailcat")
	build := exec.Command("go", "build", "-o", fakeTailcatBin, "./testdata/fake-tailcat")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build fake-tailcat:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestUpstreamAddr(t *testing.T) {
	// Explicit port is preserved verbatim.
	got, err := upstreamAddr("127.0.0.1:9999")
	if err != nil {
		t.Fatalf(`upstreamAddr("127.0.0.1:9999"): %v`, err)
	}
	if got != "127.0.0.1:9999" {
		t.Errorf(`upstreamAddr("127.0.0.1:9999") = %q, want "127.0.0.1:9999"`, got)
	}

	// Port 0 asks the OS for a free port and returns a concrete address.
	got, err = upstreamAddr("127.0.0.1:0")
	if err != nil {
		t.Fatalf(`upstreamAddr("127.0.0.1:0"): %v`, err)
	}
	host, portStr, err := net.SplitHostPort(got)
	if err != nil {
		t.Fatalf("upstreamAddr(\"127.0.0.1:0\") = %q: SplitHostPort: %v", got, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q in %q is not numeric: %v", portStr, got, err)
	}
	if port < 1 || port > 65535 {
		t.Errorf("port %d out of range 1..65535", port)
	}

	// Bracketed IPv6 keeps its brackets (JoinHostPort restores them).
	got, err = upstreamAddr("[::1]:1080")
	if err != nil {
		t.Fatalf(`upstreamAddr("[::1]:1080"): %v`, err)
	}
	if got != "[::1]:1080" {
		t.Errorf(`upstreamAddr("[::1]:1080") = %q, want "[::1]:1080"`, got)
	}

	// Empty host with port 0 still yields a concrete, parseable address.
	got, err = upstreamAddr(":0")
	if err != nil {
		t.Fatalf(`upstreamAddr(":0"): %v`, err)
	}
	_, portStr, err = net.SplitHostPort(got)
	if err != nil {
		t.Fatalf(`upstreamAddr(":0") = %q: SplitHostPort: %v`, got, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf(`upstreamAddr(":0") = %q: port %q is not numeric: %v`, got, portStr, err)
	}
	if port < 1 || port > 65535 {
		t.Errorf(`upstreamAddr(":0") = %q: port %d out of range 1..65535`, got, port)
	}

	// Malformed inputs are rejected.
	for _, bad := range []string{"127.0.0.1", ":0x22", "[::1]", "::1", "host:abc", "127.0.0.1:99999", "127.0.0.1:-1"} {
		if _, err := upstreamAddr(bad); err == nil {
			t.Errorf("upstreamAddr(%q): want error, got nil", bad)
		}
	}
}

// freePort asks the OS for a free port on host by briefly binding host:0.
func freePort(t *testing.T, host string) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("reserve free port on %s: %v", host, err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestSpawnTailcatSocksAndWaitReady(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t, "127.0.0.1")))
	child := spawnTailcatSocks(fakeTailcatBin, addr)
	if child == nil {
		t.Fatal("spawn failed")
	}
	t.Cleanup(func() { terminate(child) })
	if !waitReady(addr, 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
}

// TestSpawnTailcatSocksIPv6 pins the regression where the child received an
// unbracketed IPv6 --listen (e.g. ::1:1234) and never bound the port.
func TestSpawnTailcatSocksIPv6(t *testing.T) {
	addr := net.JoinHostPort("::1", strconv.Itoa(freePort(t, "::1")))
	child := spawnTailcatSocks(fakeTailcatBin, addr)
	if child == nil {
		t.Fatal("spawn failed")
	}
	t.Cleanup(func() { terminate(child) })
	if !waitReady(addr, 5*time.Second) {
		t.Fatalf("fake tailcat never became ready on %s", addr)
	}
}

func TestTerminateStopsChild(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t, "127.0.0.1")))
	child := spawnTailcatSocks(fakeTailcatBin, addr)
	if child == nil {
		t.Fatal("spawn failed")
	}
	terminate(child)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && child.ProcessState == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if child.ProcessState == nil {
		t.Fatal("child did not exit after terminate")
	}
}

func TestTerminateNilIsSafe(t *testing.T) {
	terminate(nil)
}
