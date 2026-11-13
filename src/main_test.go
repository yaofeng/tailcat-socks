package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// spawnTestChild launches fakeTailcatBin on addr bound to a fresh cancellable
// context and starts its cmd.Wait reap in a goroutine (the child is only
// reaped while Wait runs). It returns the cmd, a channel closed once the child
// is reaped, and the context's cancel func. Cleanup cancels the context
// (SIGTERM now, SIGKILL after childKillGrace) and waits for the reap, so
// tests never leak children or zombies.
func spawnTestChild(t *testing.T, addr string) (cmd *exec.Cmd, reaped <-chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := spawnTailcatSocks(ctx, fakeTailcatBin, addr)
	if err != nil {
		cancel()
		t.Fatalf("spawnTailcatSocks(%q, %q): %v", fakeTailcatBin, addr, err)
	}
	done := make(chan struct{})
	go func() {
		// A child stopped via ctx cancellation exits on a signal (an
		// *exec.ExitError) or, if the WaitDelay path fired, with
		// context.Canceled; anything else is a real failure.
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) && !errors.Is(err, context.Canceled) {
				t.Errorf("cmd.Wait: unexpected error: %v", err)
			}
		}
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(childKillGrace + 3*time.Second):
			t.Errorf("child not reaped within %v of ctx cancel", childKillGrace+3*time.Second)
		}
	})
	return cmd, done, cancel
}

func TestSpawnTailcatSocks(t *testing.T) {
	// Success: non-nil cmd, nil error, and the child listens on addr.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t, "127.0.0.1")))
	cmd, _, _ := spawnTestChild(t, addr)
	if cmd == nil {
		t.Fatal("spawnTailcatSocks returned nil cmd with nil error")
	}
	if !waitReady(context.Background(), addr, 5*time.Second) {
		t.Fatalf("fake tailcat never became ready on %s", addr)
	}

	// Failure: a bad binPath returns a wrapped error naming the path —
	// never a nil cmd with nil error, never os.Exit.
	if badCmd, err := spawnTailcatSocks(context.Background(), "/nonexistent/tailcat", addr); err == nil {
		t.Errorf("spawnTailcatSocks with bad binPath: want error, got nil (cmd %v)", badCmd)
	} else if badCmd != nil {
		t.Error("spawnTailcatSocks with bad binPath: want nil cmd, got non-nil")
	} else if !strings.Contains(err.Error(), "/nonexistent/tailcat") {
		t.Errorf("error %v does not mention the bin path", err)
	}
}

func TestSpawnTailcatSocksAndWaitReady(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t, "127.0.0.1")))
	cmd, _, _ := spawnTestChild(t, addr)
	if cmd == nil {
		t.Fatal("spawn failed")
	}
	if !waitReady(context.Background(), addr, 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
}

// TestSpawnTailcatSocksIPv6 pins the regression where the child received an
// unbracketed IPv6 --listen (e.g. ::1:1234) and never bound the port.
func TestSpawnTailcatSocksIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	addr := ln.Addr().String() // "[::1]:port", brackets included
	ln.Close()
	cmd, _, _ := spawnTestChild(t, addr)
	if cmd == nil {
		t.Fatal("spawn failed")
	}
	if !waitReady(context.Background(), addr, 5*time.Second) {
		t.Fatalf("fake tailcat never became ready on %s", addr)
	}
}

// TestSpawnedChildDiesWithContext pins the SIGTERM leg of the
// Cancel→SIGTERM→WaitDelay→SIGKILL ladder: canceling the spawn context must
// reap the (SIGTERM-honoring) child well within childKillGrace, proving the
// escalation can never hang.
func TestSpawnedChildDiesWithContext(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t, "127.0.0.1")))
	cmd, reaped, cancel := spawnTestChild(t, addr)
	if !waitReady(context.Background(), addr, 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
	start := time.Now()
	cancel()
	select {
	case <-reaped:
		if d := time.Since(start); d > childKillGrace+3*time.Second {
			t.Errorf("child took %v to die after ctx cancel, want <= %v", d, childKillGrace+3*time.Second)
		}
	case <-time.After(childKillGrace + 3*time.Second):
		t.Fatal("child still alive after ctx cancel + grace")
	}
	if cmd.ProcessState == nil {
		t.Error("child reaped but cmd.ProcessState is nil")
	}
}

// TestSpawnedChildDiesWithSIGKILL pins the SIGKILL leg: a child that ignores
// SIGTERM (FAKE_TAILCAT_IGNORE_TERM, inherited via the environment) must
// still die — WaitDelay fires Kill at childKillGrace — and the reap must
// show death by SIGKILL.
func TestSpawnedChildDiesWithSIGKILL(t *testing.T) {
	t.Setenv("FAKE_TAILCAT_IGNORE_TERM", "1")
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t, "127.0.0.1")))
	cmd, reaped, cancel := spawnTestChild(t, addr)
	if !waitReady(context.Background(), addr, 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
	start := time.Now()
	cancel()
	select {
	case <-reaped:
	case <-time.After(childKillGrace + 3*time.Second):
		t.Fatal("child still alive after ctx cancel + grace")
	}
	d := time.Since(start)
	if d < childKillGrace-500*time.Millisecond {
		t.Errorf("child reaped after %v, want >= ~%v (SIGTERM should have been ignored)", d, childKillGrace)
	}
	if d > childKillGrace+3*time.Second {
		t.Errorf("child reaped after %v, want <= %v", d, childKillGrace+3*time.Second)
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("ProcessState.Sys() = %T, want syscall.WaitStatus", cmd.ProcessState.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("child termination = %v, want death by SIGKILL", ws)
	}
}
