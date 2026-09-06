package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
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
		// The received channel records only ACCEPTED, fully parsed CONNECTs:
		// a client that opens the connection and closes it mid-handshake (e.g.
		// rejecting an over-long FQDN before sending the request) must leave
		// nothing on the channel, so every read failure returns here.
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, hdr[1])); err != nil {
			return
		}
		conn.Write([]byte{0x05, 0x00})
		req := make([]byte, 4)
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		var host string
		switch req[3] {
		case 0x03:
			l := make([]byte, 1)
			if _, err := io.ReadFull(conn, l); err != nil {
				return
			}
			b := make([]byte, l[0])
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = string(b)
		case 0x01:
			b := make([]byte, 4)
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = net.IP(b).String()
		default:
			host = "?"
		}
		pb := make([]byte, 2)
		if _, err := io.ReadFull(conn, pb); err != nil {
			return
		}
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
	srv := newProxy(t, mapping, upstream)
	go srv.Serve()
	return srv
}

// startProxyWithIdle is startProxy with a short relay idle timeout, for
// exercising the shared idle clock without waiting 300s.
func startProxyWithIdle(t *testing.T, mapping map[string]string, upstream string, idle time.Duration) *Server {
	t.Helper()
	srv := newProxy(t, mapping, upstream)
	srv.relayIdle = idle
	go srv.Serve()
	return srv
}

func newProxy(t *testing.T, mapping map[string]string, upstream string) *Server {
	t.Helper()
	dm := &DNSMap{}
	dm.Store(mapping)
	srv, err := NewServer(dm, upstream, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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

// silentUpstream completes the SOCKS5 handshake and then swallows everything
// it is sent without ever writing back, counting payload bytes in got.
func silentUpstream(t *testing.T, got *atomic.Int64) string {
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
		switch req[3] {
		case 0x03:
			l := make([]byte, 1)
			io.ReadFull(conn, l)
			io.ReadFull(conn, make([]byte, l[0]))
		case 0x01:
			io.ReadFull(conn, make([]byte, 4))
		case 0x04:
			io.ReadFull(conn, make([]byte, 16))
		}
		io.ReadFull(conn, make([]byte, 2))
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

// TestRelayIdleClockIsSharedAcrossDirections pins Python's select() behavior:
// one shared 300s clock reset by traffic in EITHER direction. A one-way
// transfer (here: client pumps, upstream never answers) must not be killed by
// the idle timeout just because one side is silent.
func TestRelayIdleClockIsSharedAcrossDirections(t *testing.T) {
	var got atomic.Int64
	up := silentUpstream(t, &got)
	// 200ms idle; the pump below runs 1.2s (6x idle) in one direction only.
	srv := startProxyWithIdle(t, map[string]string{}, up, 200*time.Millisecond)

	c := socksConnect(t, srv.ActualAddr().String(), "plain.example", 80)
	defer c.Close()

	const pings = 24
	for i := 0; i < pings; i++ {
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("PING\n")); err != nil {
			t.Fatalf("relay died during one-way transfer at ping %d/%d: %v", i+1, pings, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Every ping must have made it through the relay to the upstream.
	deadline := time.Now().Add(2 * time.Second)
	for got.Load() < pings*5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := got.Load(); n < pings*5 {
		t.Fatalf("upstream got %d bytes, want %d (relay dropped one-way traffic)", n, pings*5)
	}

	// Silence now: the relay must tear down within ~2x idle.
	quiet := time.Now()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	one := make([]byte, 1)
	if _, err := c.Read(one); err == nil {
		t.Fatal("expected teardown after silence, but read data instead")
	}
	if elapsed := time.Since(quiet); elapsed > 2*200*time.Millisecond+150*time.Millisecond {
		t.Errorf("teardown took %v after silence, want <= ~2x idle (550ms)", elapsed)
	}
}

// earlyCloseUpstream is the e2e-smoke fake upstream: it completes the SOCKS5
// handshake, writes one short payload and closes WITHOUT ever reading anything
// the client sends after the handshake (curl's HTTP GET with --http0.9).
func earlyCloseUpstream(t *testing.T, payload []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				hdr := make([]byte, 2)
				io.ReadFull(conn, hdr)
				io.ReadFull(conn, make([]byte, hdr[1]))
				conn.Write([]byte{0x05, 0x00})
				req := make([]byte, 4)
				io.ReadFull(conn, req)
				skip := 0
				switch req[3] {
				case 0x03:
					l := make([]byte, 1)
					io.ReadFull(conn, l)
					skip = int(l[0])
				case 0x01:
					skip = 4
				case 0x04:
					skip = 16
				}
				io.ReadFull(conn, make([]byte, skip+2))
				conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				conn.Write(payload)
				// ...and close without reading the client's request bytes.
			}()
		}
	}()
	return ln.Addr().String()
}

// TestRelayEarlyClosingUpstreamDeliversDataAndCleanEOF pins the e2e-smoke
// failure: an upstream that answers the CONNECT and closes without reading the
// client's request. The client (curl) has already sent its GET by then, so the
// relay must still deliver the payload AND end the session with a clean FIN —
// closing the client socket while those request bytes are still unread in our
// receive queue makes Linux answer with RST instead, which destroys data curl
// has not read yet (curl exit 56, empty body).
func TestRelayEarlyClosingUpstreamDeliversDataAndCleanEOF(t *testing.T) {
	payload := []byte("SAW:tcSMOKE123:9999\n")
	up := earlyCloseUpstream(t, payload)
	srv := startProxy(t, map[string]string{"www.example.com": "tcSMOKE123"}, up)

	const iterations = 200
	var failures []string
	for i := 0; i < iterations; i++ {
		c := socksConnect(t, srv.ActualAddr().String(), "www.example.com", 9999)
		// curl sends its GET the moment the SOCKS5 success reply lands; the
		// upstream is already writing its payload and closing.
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))

		var buf []byte
		one := make([]byte, 256)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			n, err := c.Read(one)
			buf = append(buf, one[:n]...)
			if err != nil {
				if err != io.EOF {
					failures = append(failures, fmt.Sprintf("iteration %d: read after %d bytes: %v (want clean EOF)", i+1, len(buf), err))
				}
				break
			}
		}
		c.Close()
		if !bytes.Equal(buf, payload) {
			failures = append(failures, fmt.Sprintf("iteration %d: got %q, want %q", i+1, buf, payload))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d/%d iterations failed:\n%s", len(failures), iterations, strings.Join(failures, "\n"))
	}
}

// TestSocks5ProxyForwardsIPv6AsATYP4 checks the ATYP 0x04 client path: the
// upstream must receive ATYP 4 plus the raw 16 address bytes and the port.
func TestSocks5ProxyForwardsIPv6AsATYP4(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte{0x05, 0x00})
		buf := make([]byte, 3+4+16+2) // greeting + req hdr + IPv6 + port
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		ch <- buf
	}()

	srv := startProxy(t, map[string]string{}, ln.Addr().String())
	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	ip := net.ParseIP("2001:db8::1").To16()
	req := append([]byte{0x05, 0x01, 0x00, 0x04}, ip...)
	req = append(req, 0x01, 0xBB) // 443
	c.Write(req)

	select {
	case got := <-ch:
		if got[6] != 0x04 {
			t.Errorf("upstream ATYP = %#x, want 0x04", got[6])
		}
		if !bytes.Equal(got[7:23], ip) {
			t.Errorf("upstream addr = %v, want %v (2001:db8::1)", got[7:23], ip)
		}
		if got[23] != 0x01 || got[24] != 0xBB {
			t.Errorf("upstream port = %v, want [1 187]", got[23:25])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no request captured")
	}
}

// TestSocks5ProxyUpstreamDialFailureRepliesFail: when the upstream is
// unreachable the client must get the generic failure reply.
func TestSocks5ProxyUpstreamDialFailureRepliesFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close() // nothing is listening there anymore

	srv := startProxy(t, map[string]string{}, closed)
	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	c.Write([]byte{0x05, 0x01, 0x00, 0x03, 3, 'a', 'b', 'c', 0x00, 0x50})
	head := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if want := []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}; !bytes.Equal(head, want) {
		t.Errorf("reply = %v, want %v", head, want)
	}
}

// TestSocks5ProxyOversizedTokenFailsConnect: a rewritten token longer than
// the 255-byte SOCKS5 domain field must fail the CONNECT. The hand-rolled
// upstream encoder wrapped the length byte (byte(300) == 44) and silently
// shipped a truncated token upstream; x/net must reject the over-long FQDN
// before any CONNECT byte reaches the upstream.
func TestSocks5ProxyOversizedTokenFailsConnect(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{"big.example": strings.Repeat("t", 300)}, up)

	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	// The client-facing name is short; only the REWRITTEN token is oversized.
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len("big.example"))}, "big.example"...)
	req = append(req, 0x00, 0x50) // port 80
	c.Write(req)
	head := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if head[1] == 0x00 {
		t.Errorf("oversized token must not yield a success reply, got %v", head)
	}
	select {
	case got := <-received:
		t.Errorf("upstream must not receive a CONNECT, got %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSocks5ProxyUpstreamRefusalRepliesFail: a reachable upstream that
// rejects the CONNECT must also produce the generic failure reply.
func TestSocks5ProxyUpstreamRefusalRepliesFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
		io.ReadFull(conn, make([]byte, 4+2))                         // ATYP 1 addr + port
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // refuse
	}()

	srv := startProxy(t, map[string]string{}, ln.Addr().String())
	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	c.Write([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50})
	head := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if want := []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}; !bytes.Equal(head, want) {
		t.Errorf("reply = %v, want %v", head, want)
	}
}
