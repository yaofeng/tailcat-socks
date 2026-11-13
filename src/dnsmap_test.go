package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dns.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDNSFileBasic(t *testing.T) {
	path := writeTemp(t, `
# comment line
tcXYZabc123   www.example.com  api.example.com
tcDEF456ghi   foo.com

tcGGG         bar.com	baz.com
`)
	m, err := LoadDNSFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"www.example.com": "tcXYZabc123",
		"api.example.com": "tcXYZabc123",
		"foo.com":         "tcDEF456ghi",
		"bar.com":         "tcGGG",
		"baz.com":         "tcGGG",
	}
	for d, tok := range want {
		if m[d] != tok {
			t.Errorf("m[%q] = %q, want %q", d, m[d], tok)
		}
	}
	if _, ok := m["tcxyzabc123"]; ok {
		t.Error("token line itself must not become a domain key")
	}
}

func TestLoadDNSFileMissingDomains(t *testing.T) {
	m, err := LoadDNSFile(writeTemp(t, "tcONLYONE\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("token-only line should contribute nothing, got %v", m)
	}
}

func TestLoadDNSFileLaterOverrides(t *testing.T) {
	m, err := LoadDNSFile(writeTemp(t, "tcA dup.com\ntcB dup.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["dup.com"] != "tcB" {
		t.Errorf("later line should win, got %q", m["dup.com"])
	}
}

func TestLoadDNSFileMissingFile(t *testing.T) {
	if _, err := LoadDNSFile(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestRewriteHost(t *testing.T) {
	m := map[string]string{"www.example.com": "tcXYZ"}
	if got := RewriteHost("www.example.com", m); got != "tcXYZ" {
		t.Errorf("exact match: got %q", got)
	}
	if got := RewriteHost("WWW.Example.COM", m); got != "tcXYZ" {
		t.Errorf("case-insensitive match: got %q", got)
	}
	if got := RewriteHost("other.com", m); got != "other.com" {
		t.Errorf("no match should return host unchanged, got %q", got)
	}
}

func TestWatchDNSFileReloads(t *testing.T) {
	path := writeTemp(t, "tcA alpha.com\n")
	m := &DNSMap{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchDNSFile(ctx, path, m)

	// initial load
	waitFor(t, 3*time.Second, func() bool { return m.Load()["alpha.com"] == "tcA" })

	// fsnotify fires on the content write itself; no mtime bump needed
	if err := os.WriteFile(path, []byte("tcB beta.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return m.Load()["beta.com"] == "tcB" })
	if _, ok := m.Load()["alpha.com"]; ok {
		t.Error("removed domain should be gone after reload")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestWatchDNSFileKeepsPreviousMapOnReadError(t *testing.T) {
	path := writeTemp(t, "tcA keep.com\n")
	m := &DNSMap{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchDNSFile(ctx, path, m)
	waitFor(t, 3*time.Second, func() bool { return m.Load()["keep.com"] == "tcA" })

	// Make reads fail while events keep coming: replace the file with a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// A few ticks later the old mapping must still be served.
	time.Sleep(300 * time.Millisecond)
	if m.Load()["keep.com"] != "tcA" {
		t.Fatal("read error must keep previous mapping")
	}
}
