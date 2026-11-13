package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

func TestRoute(t *testing.T) {
	m := map[string]string{"www.example.com": "tcXYZabc", "dup.com": "tcSecond", "self.example.com": "self.example.com"}
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
		// 手滑条目 token 恰等于 domain：必须仍按命中处理（隧道），不许静默直连。
		{"self-equal token still tunnels", "self.example.com", 443, ViaTunnel, "self.example.com"},
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

// TestClientKeyFileModePinned：私钥文件必须钉死 0600——WriteFile 的 perm 受
// umask 过滤，严格 umask（0377）下不 Chmod 会落成 0400。
// （计划的 0177 屏蔽的是 owner 的 x 位而非 w 位，对 0600 文件无损，
// 起不到检测作用，故改用 0377。）
func TestClientKeyFileModePinned(t *testing.T) {
	// 先取 t.TempDir() 再设 umask：严格 umask 下 MkdirTemp 自己的
	// 0700 目录位会被过滤（缺 owner x），TempDir 直接 permission denied。
	path := filepath.Join(t.TempDir(), "client-key")
	old := syscall.Umask(0o377)
	defer syscall.Umask(old)
	d := NewAppDialer(path)
	defer d.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %O under umask 0377, want 0600", fi.Mode().Perm())
	}
}

// TestClientForConcurrentSameToken：并发 clientFor 同一 token 只许创建一个
// client（查+建全程在锁内；锁不跨网络拨号）。
func TestClientForConcurrentSameToken(t *testing.T) {
	d := NewAppDialer(filepath.Join(t.TempDir(), "k"))
	defer d.Close()
	var mu sync.Mutex
	seen := map[*tailcat.Client]bool{}
	var wg sync.WaitGroup
	// 必须用能通过 ParseAddr 的合法地址："tcXYZabc123" 的尾串不是
	// base64(CBOR) wire 编码，会被 clientFor 直接拒掉。NewPrivateKey
	// 纯离线生成，Public.Addr() 是其官方序列化，绝不触网。
	token := string(tailcat.NewPrivateKey().Public.Addr())
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.clientFor(token)
			if err != nil {
				t.Errorf("clientFor: %v", err)
				return
			}
			mu.Lock()
			seen[c] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Errorf("created %d distinct clients for one token, want 1", len(seen))
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
