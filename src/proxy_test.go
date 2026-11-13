package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// plainServer runs a plain (no-SOCKS) TCP server: handle owns each accepted
// conn and must close it. Returns the listen address.
func plainServer(t *testing.T, handle func(net.Conn)) string {
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
			go handle(conn)
		}
	}()
	return ln.Addr().String()
}

// echoHandler echoes every read back prefixed with "ECHO:".
func echoHandler(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(append([]byte("ECHO:"), buf[:n]...)); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// fakeDialer is the injected Dialer: it records every routed Target and
// proxies the actual connection to a local listener registered per via.
// fail, when it answers yes, makes the dial error *after* recording — it
// stands in for the tailcat library rejecting a token, without any network.
type fakeDialer struct {
	targets chan Target
	addrs   map[string]string // ViaTunnel / ViaDirect -> "127.0.0.1:port"
	fail    func(Target) bool
}

func (f *fakeDialer) Dial(ctx context.Context, t Target) (net.Conn, error) {
	f.targets <- t
	addr, ok := f.addrs[t.Via]
	if !ok {
		return nil, fmt.Errorf("fakeDialer: no address for via %q", t.Via)
	}
	if f.fail != nil && f.fail(t) {
		return nil, errors.New("fake dial failure")
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

func (f *fakeDialer) Close() error { return nil }

// dialerToEcho wires both vias to a local echo server.
func dialerToEcho(t *testing.T) *fakeDialer {
	t.Helper()
	addr := plainServer(t, echoHandler)
	return &fakeDialer{targets: make(chan Target, 64), addrs: map[string]string{ViaTunnel: addr, ViaDirect: addr}}
}

// nextTarget waits for the next recorded dial decision.
func nextTarget(t *testing.T, fd *fakeDialer) Target {
	t.Helper()
	select {
	case got := <-fd.targets:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("dialer never saw a target")
		return Target{}
	}
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

func newProxy(t *testing.T, mapping map[string]string, d Dialer) *Server {
	t.Helper()
	dm := &DNSMap{}
	dm.Store(mapping)
	srv, err := NewServer(dm, d, NewConnRegistry(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func startProxy(t *testing.T, mapping map[string]string, d Dialer) *Server {
	t.Helper()
	srv := newProxy(t, mapping, d)
	go srv.Serve()
	return srv
}

// startProxyWithIdle is startProxy with a short relay idle timeout, for
// exercising the shared idle timer without waiting 300s.
func startProxyWithIdle(t *testing.T, mapping map[string]string, d Dialer, idle time.Duration) *Server {
	t.Helper()
	srv := newProxy(t, mapping, d)
	srv.relayIdle = idle
	go srv.Serve()
	return srv
}

func TestSocks5ProxyRewritesHostAndRelays(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, fd)

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
	got := nextTarget(t, fd)
	if got.Via != ViaTunnel || got.Host != "tcXYZabc123" || got.Port != 8081 {
		t.Errorf("dialed %+v, want {%s tcXYZabc123 8081}", got, ViaTunnel)
	}
}

func TestSocks5ProxyRewritesCaseInsensitive(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, fd)

	c := socksConnect(t, srv.ActualAddr().String(), "WWW.Example.COM", 80)
	c.Close()
	got := nextTarget(t, fd)
	if got.Via != ViaTunnel || got.Host != "tcXYZabc123" || got.Port != 80 {
		t.Errorf("dialed %+v, want {%s tcXYZabc123 80}", got, ViaTunnel)
	}
}

func TestSocks5ProxyUnmatchedHostDialsDirect(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, fd)

	c := socksConnect(t, srv.ActualAddr().String(), "other.com", 9999)
	c.Close()
	got := nextTarget(t, fd)
	if got.Via != ViaDirect || got.Host != "other.com" || got.Port != 9999 {
		t.Errorf("dialed %+v, want {%s other.com 9999}", got, ViaDirect)
	}
}

// TestSocks5ProxyIPLiteralDialsDirect replaces the old ATYP4-forwarding
// test: an IPv6 literal now means "dial directly", and the recorded Target
// is the observable contract (bytes reach the dialer, not a SOCKS upstream).
func TestSocks5ProxyIPLiteralDialsDirect(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{}, fd)

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
	head := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if head[1] != 0x00 {
		t.Fatalf("CONNECT failed for IP literal: %v", head)
	}
	got := nextTarget(t, fd)
	if got.Via != ViaDirect || got.Host != "2001:db8::1" || got.Port != 443 {
		t.Errorf("dialed %+v, want {%s 2001:db8::1 443}", got, ViaDirect)
	}
}

func TestSocks5ProxyRejectsBind(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{}, fd)

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
	case got := <-fd.targets:
		t.Errorf("dialer must not be called for BIND, got %+v", got)
	default:
	}
}

func TestSocks5ProxyNoAcceptableMethod(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{}, fd)

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
	case got := <-fd.targets:
		t.Errorf("dialer must not be called, got %+v", got)
	default:
	}
}

// TestSocks5ProxyDialFailureRepliesFail: a failed dial must give the client
// the generic failure reply, byte for byte.
func TestSocks5ProxyDialFailureRepliesFail(t *testing.T) {
	fd := dialerToEcho(t)
	fd.fail = func(Target) bool { return true }
	srv := startProxy(t, map[string]string{}, fd)

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
	if !bytes.Equal(head, socksFailReply) {
		t.Errorf("reply = %v, want %v", head, socksFailReply)
	}
}

// TestSocks5ProxyOversizedTokenFailsConnect: a rewritten token longer than
// 255 bytes no longer hits any SOCKS-client encoder — the real AppDialer
// rejects it at tailcat.ParseAddr instead. The fake emulates that
// rejection; the proxy must fail the CONNECT cleanly. (The old "no byte
// left here" pre-check died with the upstream chain, per spec §12.)
func TestSocks5ProxyOversizedTokenFailsConnect(t *testing.T) {
	big := strings.Repeat("t", 300)
	fd := dialerToEcho(t)
	fd.fail = func(g Target) bool { return g.Via == ViaTunnel && len(g.Host) > 255 }
	srv := startProxy(t, map[string]string{"big.example": big}, fd)

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
	got := nextTarget(t, fd)
	if got.Via != ViaTunnel || got.Host != big {
		t.Errorf("dialed %+v, want tunnel with the 300-byte token", got)
	}
}

// TestRelayIdleWindowSharedAcrossDirections pins Python's select() behavior:
// one shared window that traffic in EITHER direction re-arms. A one-way
// transfer (here: client pumps, target never answers) must not be killed by
// the idle timeout just because one side is silent.
func TestRelayIdleWindowSharedAcrossDirections(t *testing.T) {
	var got atomic.Int64
	addr := plainServer(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	fd := &fakeDialer{targets: make(chan Target, 64), addrs: map[string]string{ViaTunnel: addr, ViaDirect: addr}}
	// 200ms idle; the pump below runs ~1.2s (6x idle) in one direction only.
	srv := startProxyWithIdle(t, map[string]string{}, fd, 200*time.Millisecond)

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

	// Every ping must have made it through the relay to the target.
	deadline := time.Now().Add(2 * time.Second)
	for got.Load() < pings*5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := got.Load(); n < pings*5 {
		t.Fatalf("target got %d bytes, want %d (relay dropped one-way traffic)", n, pings*5)
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

// TestRelayEarlyClosingTargetDeliversDataAndCleanEOF pins the e2e-smoke
// failure: a target that answers the CONNECT and closes without reading the
// client's request. The client (curl) has already sent its GET by then, so
// the relay must still deliver the payload AND end the session with a clean
// FIN — closing the client socket while those request bytes are still
// unread in our receive queue makes Linux answer with RST instead.
func TestRelayEarlyClosingTargetDeliversDataAndCleanEOF(t *testing.T) {
	payload := []byte("SAW:tcSMOKE123:9999\n")
	addr := plainServer(t, func(conn net.Conn) {
		defer conn.Close()
		conn.Write(payload)
		// ...and close without reading the client's request bytes.
	})
	fd := &fakeDialer{targets: make(chan Target, 512), addrs: map[string]string{ViaTunnel: addr, ViaDirect: addr}}
	srv := startProxy(t, map[string]string{"www.example.com": "tcSMOKE123"}, fd)

	const iterations = 200
	var failures []string
	for i := 0; i < iterations; i++ {
		c := socksConnect(t, srv.ActualAddr().String(), "www.example.com", 9999)
		// curl sends its GET the moment the SOCKS5 success reply lands; the
		// target is already writing its payload and closing.
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

// TestRelayClientEOFPropagatesToTarget pins the forward leg of the
// half-close: when the client half-closes (FIN, its read side still open),
// the target must see EOF on its read side yet still be able to send data
// back and have it delivered.
func TestRelayClientEOFPropagatesToTarget(t *testing.T) {
	eofSeen := make(chan int, 64)
	addr := plainServer(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		total := 0
		for {
			n, err := conn.Read(buf)
			total += n
			if err != nil {
				eofSeen <- total
				break
			}
		}
		conn.Write([]byte("ANSWER"))
	})
	fd := &fakeDialer{targets: make(chan Target, 64), addrs: map[string]string{ViaTunnel: addr, ViaDirect: addr}}
	srv := startProxy(t, map[string]string{}, fd)

	c := socksConnect(t, srv.ActualAddr().String(), "plain.example", 80)
	defer c.Close()
	tc, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("client conn is %T, want *net.TCPConn (CloseWrite needed)", c)
	}
	c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("HELLO")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tc.CloseWrite(); err != nil { // FIN downstream, read side stays open
		t.Fatalf("CloseWrite: %v", err)
	}

	var seen int
	select {
	case seen = <-eofSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("target never saw EOF: client FIN was not propagated")
	}
	if seen < len("HELLO") {
		t.Errorf("target saw %d payload bytes before EOF, want >= %d", seen, len("HELLO"))
	}

	// The reverse direction must still work after the forward half-close.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len("ANSWER"))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("reply after half-close not delivered: %v", err)
	}
	if string(buf) != "ANSWER" {
		t.Errorf("reply = %q, want ANSWER", buf)
	}
	// The target closed after the reply; the relay must follow with a
	// clean FIN, not an RST.
	one := make([]byte, 1)
	if _, err := c.Read(one); err != io.EOF {
		t.Errorf("after reply: read err = %v, want clean EOF", err)
	}
}

// TestSocks5ProxyHandshakeIdleClosesSlowClient: a client that connects but
// never advances the handshake must be dropped once the handshake deadline
// expires, instead of pinning a goroutine+fd forever.
func TestSocks5ProxyHandshakeIdleClosesSlowClient(t *testing.T) {
	srv := newProxy(t, map[string]string{}, dialerToEcho(t))
	srv.handshakeTimeout = 200 * time.Millisecond
	go srv.Serve()

	conn, err := net.Dial("tcp", srv.ActualAddr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	start := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("slow client closed after %v, want ~handshakeTimeout (200ms)", elapsed)
	} else if elapsed < 100*time.Millisecond {
		t.Errorf("slow client dropped after %v, before the 200ms handshake deadline could expire", elapsed)
	}
	if err == nil {
		t.Fatalf("silent client got data (%d bytes) instead of being dropped", n)
	}
}

// TestServeConnRegistersAndCountsSession pins the registry lifecycle over a
// real proxied session: open flip, per-leg byte counts, and full cleanup
// after the client closes.
func TestServeConnRegistersAndCountsSession(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxy(t, map[string]string{"www.example.com": "tcXYZabc123"}, fd)

	c := socksConnect(t, srv.ActualAddr().String(), "www.example.com", 8081)
	c.Write([]byte("HELLO"))
	buf := make([]byte, len("ECHO:HELLO"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("relay read: %v", err)
	}

	var snaps []SessionSnapshot
	waitFor(t, 3*time.Second, func() bool {
		snaps, _ = srv.Registry.Snapshot()
		return len(snaps) == 1
	})
	s := snaps[0]
	if s.Host != "www.example.com" || s.Target != "tcXYZabc123" || s.Port != 8081 || s.Via != ViaTunnel || !s.Opened {
		t.Errorf("session = %+v", s)
	}
	if s.ID == 0 || s.Started.IsZero() || s.Client == "" {
		t.Errorf("session identity fields missing: %+v", s)
	}
	waitFor(t, 3*time.Second, func() bool {
		snaps, _ = srv.Registry.Snapshot()
		return len(snaps) == 1 && snaps[0].BytesUp >= 5 && snaps[0].BytesDown >= uint64(len("ECHO:HELLO"))
	})

	c.Close()
	waitFor(t, 3*time.Second, func() bool {
		_, tot := srv.Registry.Snapshot()
		return tot.Active == 0
	})
	_, tot := srv.Registry.Snapshot()
	if tot.SessionsTotal != 1 {
		t.Errorf("sessionsTotal = %d, want 1", tot.SessionsTotal)
	}
}
