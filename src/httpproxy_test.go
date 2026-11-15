package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newHTTPProxy 起一个 HTTP 代理入口，复用 proxy_test.go 的 fakeDialer 与
// echo server。
func newHTTPProxy(t *testing.T, mapping map[string]string, d Dialer, users Users) *HTTPProxy {
	t.Helper()
	dm := &DNSMap{}
	dm.Store(mapping)
	hp, err := NewHTTPProxy(dm, d, NewConnRegistry(), "127.0.0.1:0", users, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hp.Close() })
	go hp.Serve()
	return hp
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// httpConnect 发一个 CONNECT 请求，返回连接与响应。
func httpConnect(t *testing.T, proxyAddr, target, auth string) (net.Conn, *http.Response) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	req += "\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
	if err != nil {
		c.Close()
		t.Fatalf("read response: %v", err)
	}
	return c, resp
}

func httpUsers() Users {
	return Users{
		"tailcat": {password: "pw1", Mode: ModeMatchedOnly},
		"all":     {password: "pw2", Mode: ModeAny},
	}
}

func TestHTTPProxyRequiresAuth(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())
	c, resp := httpConnect(t, hp.ActualAddr().String(), "a.example.com:443", "")
	defer c.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want 407", resp.StatusCode)
	}
	if got := resp.Header.Get("Proxy-Authenticate"); !strings.Contains(got, "Basic") {
		t.Errorf("Proxy-Authenticate = %q, want a Basic challenge", got)
	}
	select {
	case tg := <-fd.targets:
		t.Fatalf("unauthenticated request must never dial, saw %+v", tg)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestHTTPProxyRejectsWrongPassword(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())
	c, resp := httpConnect(t, hp.ActualAddr().String(), "a.example.com:443", basicAuth("tailcat", "wrong"))
	defer c.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want 407", resp.StatusCode)
	}
}

// tailcat 用户只能访问命中域名：未命中必须 403 且不拨号。
func TestHTTPProxyMatchedOnlyRejectsUnmatched(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())
	c, resp := httpConnect(t, hp.ActualAddr().String(), "other.com:443", basicAuth("tailcat", "pw1"))
	defer c.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	select {
	case tg := <-fd.targets:
		t.Fatalf("denied target must never dial, saw %+v", tg)
	case <-time.After(150 * time.Millisecond):
	}
}

// all 用户访问未命中域名：200 且隧道可用（echo 回显）。
func TestHTTPProxyAnyUserTunnels(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())
	c, resp := httpConnect(t, hp.ActualAddr().String(), "other.com:443", basicAuth("all", "pw2"))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if tg := nextTarget(t, fd); tg.Via != ViaDirect {
		t.Errorf("via = %s, want direct", tg.Via)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "ECHO:") {
		t.Errorf("got %q, want ECHO prefix", buf[:n])
	}
}

// tailcat 用户访问命中域名：200 且路由为 tunnel。
func TestHTTPProxyMatchedOnlyTunnelsMatchedHost(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())
	c, resp := httpConnect(t, hp.ActualAddr().String(), "a.example.com:443", basicAuth("tailcat", "pw1"))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if tg := nextTarget(t, fd); tg.Via != ViaTunnel || tg.Host != "tcAAA" {
		t.Errorf("target = %+v, want tunnel tcAAA", tg)
	}
}

// 普通 HTTP 请求：绝对 URI 要改写成 origin-form 再转发。
func TestHTTPProxyForwardsAbsoluteURIAsOriginForm(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())

	c, err := net.DialTimeout("tcp", hp.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	req := "GET http://a.example.com:8080/path?q=1 HTTP/1.1\r\nHost: a.example.com:8080\r\n" +
		"Proxy-Authorization: " + basicAuth("tailcat", "pw1") + "\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	// echo server 会把收到的字节原样回显（带 ECHO: 前缀），从里面能看到经
	// 改写的请求行。
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "GET /path?q=1 HTTP/1.1") {
		t.Errorf("forwarded request line should be origin-form, got %q", got)
	}
	if strings.Contains(got, "http://a.example.com") {
		t.Errorf("absolute URI must be stripped, got %q", got)
	}
}

// 不支持的方法：501。
func TestHTTPProxyUnsupportedMethod(t *testing.T) {
	fd := dialerToEcho(t)
	hp := newHTTPProxy(t, map[string]string{"a.example.com": "tcAAA"}, fd, httpUsers())
	c, err := net.DialTimeout("tcp", hp.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	req := "POST a.example.com:443 HTTP/1.1\r\nHost: a.example.com\r\n" +
		"Proxy-Authorization: " + basicAuth("tailcat", "pw1") + "\r\n\r\n"
	c.Write([]byte(req))
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

// HTTP 连接也要写历史；认证失败不写。
func TestHTTPProxyRecordsHistory(t *testing.T) {
	fd := dialerToEcho(t)
	dir := t.TempDir()
	h, err := OpenHistory(dir + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	dm := &DNSMap{}
	dm.Store(map[string]string{"a.example.com": "tcAAA"})
	hp, err := NewHTTPProxy(dm, fd, NewConnRegistry(), "127.0.0.1:0", httpUsers(), h)
	if err != nil {
		t.Fatal(err)
	}
	defer hp.Close()
	go hp.Serve()

	c, resp := httpConnect(t, hp.ActualAddr().String(), "a.example.com:443", basicAuth("tailcat", "pw1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	c.Write([]byte("hi"))
	buf := make([]byte, 64)
	c.Read(buf)
	c.Close()

	// 历史在连接结束时写，给它一点时间。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _, qerr := h.Query(time.Now().Format("2006-01-02"))
		if qerr == nil && len(entries) == 1 && entries[0].Count == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries, _, _ := h.Query(time.Now().Format("2006-01-02"))
	t.Fatalf("history row not recorded, got %+v", entries)
}
