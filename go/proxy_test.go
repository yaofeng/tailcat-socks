package main

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// fakeUpstream is a minimal SOCKS5 server (stand-in for `tailcat socks`) that
// records the CONNECT target on the received channel and echoes all data
// back prefixed with "ECHO:".
func fakeUpstream(t *testing.T, received chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		hdr := make([]byte, 2)
		io.ReadFull(conn, hdr)
		io.ReadFull(conn, make([]byte, hdr[1]))
		conn.Write([]byte{0x05, 0x00})
		req := make([]byte, 4)
		io.ReadFull(conn, req)
		var host string
		switch req[3] {
		case 0x03:
			l := make([]byte, 1)
			io.ReadFull(conn, l)
			b := make([]byte, l[0])
			io.ReadFull(conn, b)
			host = string(b)
		case 0x01:
			b := make([]byte, 4)
			io.ReadFull(conn, b)
			host = net.IP(b).String()
		default:
			host = "?"
		}
		pb := make([]byte, 2)
		io.ReadFull(conn, pb)
		received <- net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				conn.Write(append([]byte("ECHO:"), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

// socksConnect dials the proxy and completes greeting + CONNECT.
func socksConnect(t *testing.T, proxyAddr, host string, port uint16) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		t.Fatalf("greeting reply: %v", err)
	}
	if rep[0] != 0x05 || rep[1] != 0x00 {
		c.Close()
		t.Fatalf("greeting refused: %v", rep)
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	c.Write(req)
	head := make([]byte, 10)
	if _, err := io.ReadFull(c, head); err != nil {
		c.Close()
		t.Fatalf("connect reply: %v", err)
	}
	if head[1] != 0x00 {
		c.Close()
		t.Fatalf("CONNECT failed: %v", head)
	}
	return c
}

func startProxy(t *testing.T, mapping map[string]string, upstream string) *Server {
	t.Helper()
	dm := &DNSMap{}
	dm.Store(mapping)
	srv, err := NewServer(dm, upstream, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestSocks5ProxyRewritesHostAndRelays(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, up)

	c := socksConnect(t, srv.ActualAddr().String(), "www.example.com", 8081)
	c.Write([]byte("hello"))
	buf := make([]byte, len("ECHO:hello"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("relay read: %v", err)
	}
	c.Close()
	if string(buf) != "ECHO:hello" {
		t.Errorf("relay got %q", buf)
	}

	select {
	case got := <-received:
		if got != net.JoinHostPort("tcXYZabc123", "8081") {
			t.Errorf("upstream saw %q, want %q", got, "tcXYZabc123:8081")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the CONNECT")
	}
}

func TestSocks5ProxyRewritesCaseInsensitive(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, up)

	c := socksConnect(t, srv.ActualAddr().String(), "WWW.Example.COM", 80)
	c.Close()
	select {
	case got := <-received:
		if got != "tcXYZabc123:80" {
			t.Errorf("upstream saw %q, want tcXYZabc123:80", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the CONNECT")
	}
}

func TestSocks5ProxyPassesUnmatchedHostThrough(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, up)

	c := socksConnect(t, srv.ActualAddr().String(), "other.com", 9999)
	c.Close()
	select {
	case got := <-received:
		if got != "other.com:9999" {
			t.Errorf("upstream saw %q, want other.com:9999", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the CONNECT")
	}
}

func TestSocks5ProxyRejectsBind(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{}, up)

	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	io.ReadFull(c, rep)
	// CMD=2 (BIND) with an IPv4 ATYP
	c.Write([]byte{0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0x1F, 0x90})
	head := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if head[1] != 0x01 {
		t.Errorf("BIND should fail with 0x01, got %v", head)
	}
	select {
	case got := <-received:
		t.Errorf("upstream must not receive anything, got %q", got)
	default:
	}
}

func TestSocks5ProxyNoAcceptableMethod(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{}, up)

	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0xFF}) // only user/pass auth
	rep := make([]byte, 2)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if rep[0] != 0x05 || rep[1] != 0xFF {
		t.Errorf("want 05 FF, got %v", rep)
	}
	select {
	case got := <-received:
		t.Errorf("upstream must not receive anything, got %q", got)
	default:
	}
}

func TestUpstreamConnectUsesDomainATYP(t *testing.T) {
	// A raw TCP listener that captures the upstream request bytes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type result struct {
		data []byte
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte{0x05, 0x00})
		// Read exactly the greeting (3) plus the full CONNECT request
		// (VER,CMD,RSV,ATYP + len + host + port = 4+1+11+2 for
		// "tcXYZabc123":443). A single short Read would race with
		// upstreamConnect, which can only send the request after receiving
		// our 05 00 reply.
		buf := make([]byte, 3+4+1+len("tcXYZabc123")+2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Logf("capture: %v", err)
		}
		ch <- result{data: buf}
	}()

	up, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if err := upstreamConnect(up, "tcXYZabc123", 443); err != nil {
		// the fake upstream never sends a final reply; upstreamConnect will
		// block on the reply read — so instead only validate the request
		// bytes we captured before the error surfaces.
		_ = err
	}
	select {
	case r := <-ch:
		got := r.data
		if len(got) < 3 || string(got[:3]) != "\x05\x01\x00" {
			t.Fatalf("bad greeting bytes: %v", got)
		}
		// full request: VER,CMD,RSV,ATYP + len + host + big-endian port
		want := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len("tcXYZabc123"))}, []byte("tcXYZabc123")...)
		want = append(want, 0x01, 0xBB) // 443
		req := got[3:]
		if string(req) != string(want) {
			t.Errorf("request = %v, want %v", req, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no request captured")
	}
}
