package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

func TestRoute(t *testing.T) {
	m := map[string]string{"www.example.com": "tcXYZabc", "dup.com": "tcSecond"}
	tests := []struct {
		name       string
		host       string
		port       uint16
		wantVia    string
		wantTarget string
	}{
		{"mapped", "www.example.com", 443, ViaTunnel, "tcXYZabc"},
		{"mapped case-insensitive keeps token case", "WWW.Example.COM", 443, ViaTunnel, "tcXYZabc"},
		{"later entry wins", "dup.com", 8080, ViaTunnel, "tcSecond"},
		{"unmapped domain direct", "other.com", 9999, ViaDirect, "other.com"},
		{"IPv4 literal direct", "93.184.216.34", 80, ViaDirect, "93.184.216.34"},
		{"IPv6 literal direct", "2001:db8::1", 443, ViaDirect, "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := route(tt.host, tt.port, m)
			if got.Via != tt.wantVia || got.Host != tt.wantTarget || got.Port != tt.port {
				t.Errorf("route(%q,%d) = %+v, want {%s %s %d}", tt.host, tt.port, got, tt.wantVia, tt.wantTarget, tt.port)
			}
		})
	}
}

func TestClientKeyCreatedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-key")
	d1 := NewAppDialer(path)
	defer d1.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %o, want 600", fi.Mode().Perm())
	}
	var conf tailcat.PrivateKey
	if err := json.Unmarshal(data, &conf); err != nil {
		t.Fatalf("persisted key must parse as tailcat.PrivateKey: %v", err)
	}
	d2 := NewAppDialer(path)
	defer d2.Close()
	if d2.priv.Public() != conf.Private.Public() {
		t.Error("second load must reuse the persisted identity")
	}
}

func TestClientKeyCorruptNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-key")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewAppDialer(path)
	defer d.Close()
	if d.priv.IsZero() {
		t.Fatal("want usable ephemeral fallback key")
	}
	kept, _ := os.ReadFile(path)
	if string(kept) != "not json" {
		t.Errorf("corrupt key file must be left untouched, got %q", kept)
	}
}

func TestClientForRejectsBadToken(t *testing.T) {
	d := NewAppDialer(filepath.Join(t.TempDir(), "k"))
	defer d.Close()
	const garbage = "definitely not a tailcat address"
	if _, err := d.clientFor(garbage); err == nil {
		t.Fatal("ParseAddr validation must reject garbage tokens")
	}
	if _, err := d.Dial(context.Background(), Target{Via: ViaTunnel, Host: garbage, Port: 80}); err == nil {
		t.Fatal("Dial must surface the bad-token error without touching the network")
	}
}

func TestAppDialerDirectDialsTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
			close(accepted)
		}
	}()
	d := NewAppDialer(filepath.Join(t.TempDir(), "k"))
	defer d.Close()
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Dial(context.Background(), Target{Via: ViaDirect, Host: host, Port: uint16(p)})
	if err != nil {
		t.Fatalf("direct dial: %v", err)
	}
	conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("local listener never accepted")
	}
	if _, err := d.Dial(context.Background(), Target{Via: "bogus"}); err == nil {
		t.Error("unknown via must error")
	}
}
