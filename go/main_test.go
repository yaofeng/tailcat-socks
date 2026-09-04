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

func TestParseAddr(t *testing.T) {
	cases := []struct {
		in       string
		def      int
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"127.0.0.1:1080", 0, "127.0.0.1", 1080, false},
		{":8080", 0, "127.0.0.1", 8080, false},
		{"127.0.0.1:0", 0, "127.0.0.1", 0, false},
		{"example.com:9999", 0, "example.com", 9999, false},
		{"[::1]:1080", 0, "[::1]", 1080, false},
		{"1080", 0, "127.0.0.1", 1080, false}, // bare port, empty host -> default
		{"127.0.0.1", 0, "", 0, true},         // bare IP -> Atoi fails (parity with Python)
		{"host:abc", 0, "", 0, true},
	}
	for _, tc := range cases {
		host, port, err := parseAddr(tc.in, tc.def)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAddr(%q): want error, got %s:%d", tc.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddr(%q): %v", tc.in, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("parseAddr(%q) = %s:%d, want %s:%d", tc.in, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestFreeHighPort(t *testing.T) {
	p := freeHighPort("127.0.0.1")
	if p < 20000 || p > 60999 {
		t.Fatalf("port %d out of high range", p)
	}
	// typically still bindable right after
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err == nil {
		ln.Close()
	}
}

func TestSpawnTailcatSocksAndWaitReady(t *testing.T) {
	port := freeHighPort("127.0.0.1")
	child := spawnTailcatSocks(fakeTailcatBin, "127.0.0.1", port)
	if child == nil {
		t.Fatal("spawn failed")
	}
	t.Cleanup(func() { terminate(child) })
	if !waitReady(fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
}

func TestTerminateStopsChild(t *testing.T) {
	port := freeHighPort("127.0.0.1")
	child := spawnTailcatSocks(fakeTailcatBin, "127.0.0.1", port)
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
