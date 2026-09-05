package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

const (
	upstreamDialTimeout = 15 * time.Second
	// relayIdleTimeout mirrors the Python version: the relay breaks after
	// 300s with no data in either direction.
	relayIdleTimeout = 300 * time.Second
)

// SOCKS5 success/failure replies (BOUND.ADDR 0.0.0.0:0, as in Python).
var (
	socksOKReply   = []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	socksFailReply = []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
)

var errUpstreamRefused = errors.New("upstream refused the CONNECT")

// Server is the client-facing SOCKS5 front proxy. On each CONNECT it
// rewrites the target host via DNSMap and forwards the request (as a SOCKS5
// client) to the single `tailcat socks` upstream.
type Server struct {
	DNSMap       *DNSMap
	UpstreamAddr string
	ln           net.Listener
	// relayIdle is how long the relay tolerates silence in either direction
	// before tearing the session down.
	relayIdle time.Duration
}

// NewServer binds the listen address. Use ActualAddr for the resolved port
// (listening on port 0 is useful in tests).
func NewServer(dnsMap *DNSMap, upstreamAddr, listen string) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &Server{DNSMap: dnsMap, UpstreamAddr: upstreamAddr, ln: ln, relayIdle: relayIdleTimeout}, nil
}

// ActualAddr returns the bound address once NewServer has succeeded.
func (s *Server) ActualAddr() net.Addr { return s.ln.Addr() }

// Close stops the listener, ending Serve.
func (s *Server) Close() error { return s.ln.Close() }

// Serve accepts connections until the listener closes; each connection is
// handled on its own goroutine.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	s.serveConn(conn)
}

func socksFail(client net.Conn) { client.Write(socksFailReply) }

// serveConn runs the SOCKS5 exchange on one connection, mirroring the Python
// version byte-for-byte (greeting, CONNECT-only, ATYP 1/3/4, socks5h).
func (s *Server) serveConn(client net.Conn) {
	// --- greeting ---
	var v [1]byte
	if _, err := io.ReadFull(client, v[:]); err != nil {
		return
	}
	if v[0] != 0x05 {
		return
	}
	var nm [1]byte
	if _, err := io.ReadFull(client, nm[:]); err != nil {
		return
	}
	methods := make([]byte, nm[0])
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	noAuth := false
	for _, m := range methods {
		if m == 0x00 {
			noAuth = true
			break
		}
	}
	if !noAuth {
		client.Write([]byte{0x05, 0xFF}) // no acceptable methods
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// --- request ---
	var req [4]byte
	if _, err := io.ReadFull(client, req[:]); err != nil {
		return
	}
	cmd, atyp := req[1], req[3]
	host, err := readATYPAddr(client, atyp)
	if err != nil {
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(client, pb[:]); err != nil {
		return
	}
	port := uint16(pb[0])<<8 | uint16(pb[1])

	if cmd != 0x01 { // only CONNECT
		socksFail(client)
		return
	}

	target := RewriteHost(host, s.DNSMap.Load())

	// --- forward to upstream as a SOCKS5 client ---
	up, err := net.DialTimeout("tcp", s.UpstreamAddr, upstreamDialTimeout)
	if err != nil {
		socksFail(client)
		return
	}
	up.SetDeadline(time.Now().Add(upstreamDialTimeout)) // bound the handshake, like create_connection(timeout=15)
	if err := upstreamConnect(up, target, port); err != nil {
		up.Close()
		socksFail(client)
		return
	}
	up.SetDeadline(time.Time{}) // relay phase: shared idle timeout only

	client.Write(socksOKReply)
	relay(client, up, s.relayIdle)
	up.Close()
}

// readATYPAddr reads a SOCKS5 address of the given ATYP and returns it as a
// string (domain or textual IP).
func readATYPAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01: // IPv4
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	case 0x03: // domain
		// Deliberate divergence from Python: Python's ascii decode drops
		// non-ASCII domain bytes, Go forwards them raw and lets the upstream
		// reject — both fail the session, no wrong-destination risk.
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 0x04: // IPv6
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	}
	return "", fmt.Errorf("unsupported ATYP %d", atyp)
}

// upstreamConnect performs the SOCKS5 client handshake with the upstream,
// asking it to connect to host:port. Domains are sent as ATYP 0x03 so the
// upstream resolves them (socks5h semantics); IPs use ATYP 0x01/0x04.
func upstreamConnect(up net.Conn, host string, port uint16) error {
	if _, err := up.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	var greet [2]byte
	if _, err := io.ReadFull(up, greet[:]); err != nil {
		return err
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		return errUpstreamRefused
	}
	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append([]byte{0x05, 0x01, 0x00, 0x01}, ip4...)
		} else {
			req = append([]byte{0x05, 0x01, 0x00, 0x04}, ip.To16()...)
		}
	} else {
		req = append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := up.Write(req); err != nil {
		return err
	}
	var rep [4]byte
	if _, err := io.ReadFull(up, rep[:]); err != nil {
		return err
	}
	if rep[0] != 0x05 || rep[1] != 0x00 {
		return errUpstreamRefused
	}
	// drain the bound address
	if _, err := readATYPAddr(up, rep[3]); err != nil {
		return err
	}
	var bnd [2]byte
	if _, err := io.ReadFull(up, bnd[:]); err != nil {
		return err
	}
	return nil
}

// relay copies data between client and upstream in both directions until
// either side closes or the idle timeout fires. Like the Python version's
// select() loop, the idle clock is SHARED: traffic in either direction
// resets the window for both, so a healthy one-way transfer (e.g. a long
// download with no client upload) is never killed as idle.
func relay(client, upstream net.Conn, idle time.Duration) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	go func() { copyWithIdleTimeout(upstream, client, idle, &clock); done <- struct{}{} }()
	go func() { copyWithIdleTimeout(client, upstream, idle, &clock); done <- struct{}{} }()
	<-done
	// One direction finished (EOF, error or idle timeout). The peer's socket
	// may still hold unread bytes in our kernel receive queue; a plain Close
	// there makes Linux send RST instead of FIN (tcp_close: data_was_unread),
	// which destroys data the other side has not read yet. Python's _relay
	// instead does shutdown(SHUT_RDWR) on both sockets, which emits FIN (and
	// drops queued bytes without RST), and only then closes. Mirror that:
	// half-close both ends and only then close the descriptors. The Close
	// stays before the final reap so a copy direction stuck writing to a
	// stalled peer is still unblocked, exactly as before the fix.
	halfClose(client)
	halfClose(upstream)
	client.Close()
	upstream.Close()
	<-done
}

// halfClose is the Go equivalent of shutdown(fd, SHUT_RDWR). The anti-RST
// property comes from CloseWrite: once FIN is queued, tcp_close no longer
// treats queued unread data as "the peer still wants it", so Close ends in
// FIN instead of RST. (CloseRead alone does not give that guarantee; this was
// verified empirically.) CloseRead mirrors Python's shutdown(SHUT_RD) — it
// drops further input, at the same cost Python pays: data the peer sends in
// the window before Close earns the peer an RST. Errors are ignored on
// purpose, like Python's `except OSError: pass` around the shutdowns. Non-TCP
// conns have no such half-close; leave them for Close.
func halfClose(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	tc.CloseWrite() // FIN to the peer, like shutdown(SHUT_WR)
	tc.CloseRead()  // stop caring about further input, like shutdown(SHUT_RD)
}

// copyWithIdleTimeout copies src to dst, giving up if no data arrives from
// anywhere (either direction) within idle. The deadline is derived from the
// shared activity clock (last activity + idle), reproducing the Python
// relay's single select() window; writes stay deadline-free like in Python.
// A blocked Read cannot observe clock updates, so on a timeout we re-arm and
// keep waiting whenever another direction has moved the clock past the
// deadline that just fired — only true idleness returns.
func copyWithIdleTimeout(dst, src net.Conn, idle time.Duration, clock *atomic.Int64) error {
	buf := make([]byte, 65536)
	for {
		deadline := time.Unix(0, clock.Load()).Add(idle)
		src.SetReadDeadline(deadline)
		n, rerr := src.Read(buf)
		if n > 0 {
			clock.Store(time.Now().UnixNano())
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() &&
				time.Unix(0, clock.Load()).Add(idle).After(deadline) {
				continue // activity elsewhere extended the window; keep waiting
			}
			return rerr
		}
	}
}
