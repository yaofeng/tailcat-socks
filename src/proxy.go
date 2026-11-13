package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	upstreamDialTimeout = 15 * time.Second
	// relayIdleTimeout mirrors the Python version: the relay breaks after
	// 300s with no reads of real bytes in either direction (only reads
	// re-arm the window; writes never do).
	relayIdleTimeout = 300 * time.Second
	// handshakeTimeout bounds the client-facing SOCKS5 handshake (greeting,
	// request parse, upstream CONNECT, and our reply) so a silent or stalled
	// client cannot pin a goroutine and fd forever. Cleared before relaying.
	handshakeTimeout = 15 * time.Second
)

// SOCKS5 success/failure replies (BOUND.ADDR 0.0.0.0:0, as in Python).
var (
	socksOKReply   = []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	socksFailReply = []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
)

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
	// handshakeTimeout bounds the client-facing SOCKS5 handshake; see the
	// const.
	handshakeTimeout time.Duration
}

// NewServer binds the listen address. Use ActualAddr for the resolved port
// (listening on port 0 is useful in tests).
func NewServer(dnsMap *DNSMap, upstreamAddr, listen string) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &Server{DNSMap: dnsMap, UpstreamAddr: upstreamAddr, ln: ln, relayIdle: relayIdleTimeout, handshakeTimeout: handshakeTimeout}, nil
}

// ActualAddr returns the bound address once NewServer has succeeded.
func (s *Server) ActualAddr() net.Addr { return s.ln.Addr() }

// Close stops the listener, ending Serve.
func (s *Server) Close() error { return s.ln.Close() }

// Serve accepts connections until the listener closes; each connection is
// handled on its own goroutine. Transient accept errors back off exponentially
// (net/http's pattern) instead of ending the loop; net.ErrClosed is the normal
// shutdown path and any other error stops Serve (a genuinely broken listener
// should not spin here).
func (s *Server) Serve() error {
	var tempDelay time.Duration
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				slog.Warn("accept failed; backing off", "err", err, "retry_in", tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		tempDelay = 0
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	s.serveConn(conn)
}

func socksFail(client net.Conn) {
	// Best-effort: the client may already be gone; there is nothing useful
	// to do with a write error on a reply.
	_, _ = client.Write(socksFailReply)
}

// serveConn runs the SOCKS5 exchange on one connection, mirroring the Python
// version byte-for-byte (greeting, CONNECT-only, ATYP 1/3/4, socks5h).
func (s *Server) serveConn(client net.Conn) {
	// Bound the client-facing handshake reads; the early return paths keep
	// it armed — we close the conn right after anyway. The upstream dial is
	// bounded by upstreamDialTimeout, and the reply below re-arms this
	// deadline so a slow dial cannot silently consume the reply window.
	client.SetDeadline(time.Now().Add(s.handshakeTimeout))

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
		_, _ = client.Write([]byte{0x05, 0xFF}) // no acceptable methods; best-effort like the failure replies
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
	ctx, cancel := context.WithTimeout(context.Background(), upstreamDialTimeout)
	defer cancel()
	up, err := s.dialUpstream(ctx, target, port)
	// The upstream phase may have consumed the handshake budget; give the
	// reply a fresh window (success or failure) so a slow dial cannot
	// silently eat it.
	client.SetDeadline(time.Now().Add(s.handshakeTimeout))
	if err != nil {
		socksFail(client)
		return
	}
	if _, err := client.Write(socksOKReply); err != nil {
		up.Close() // client never learned CONNECT succeeded; no relay for it
		return
	}

	// Handshake over; relay's idle timer governs both legs from here.
	client.SetDeadline(time.Time{})
	up.SetDeadline(time.Time{})    // dialUpstream hands back a deadline-free conn; kept for clarity
	relay(client, up, s.relayIdle) // relay owns both conns and closes them
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

// dialUpstream opens the chained SOCKS5 connection to the upstream, passing
// host through as a domain (ATYP 0x03) when it is not an IP literal. The
// handshake is hand-rolled rather than using golang.org/x/net/proxy because
// that dialer hands back an opaque conn wrapper without CloseWrite/CloseRead
// (and not a *net.TCPConn), which would silently disable the relay's
// FIN-first, anti-RST teardown on the upstream leg. The CONNECT request is
// encoded before the TCP connect, so an over-long domain is rejected without
// a single byte leaving here; the ctx deadline bounds the whole
// dial+CONNECT exchange.
func (s *Server) dialUpstream(ctx context.Context, host string, port uint16) (net.Conn, error) {
	req, err := socksConnectRequest(host, port)
	if err != nil {
		return nil, fmt.Errorf("upstream CONNECT %s: %w", net.JoinHostPort(host, strconv.Itoa(int(port))), err)
	}
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", s.UpstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s: %w", s.UpstreamAddr, err)
	}
	if err := socksClientConnect(ctx, conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("upstream CONNECT %s: %w", net.JoinHostPort(host, strconv.Itoa(int(port))), err)
	}
	return conn, nil
}

// socksConnectRequest encodes a SOCKS5 CONNECT request for host:port: IPv4
// and IPv6 literals go out as ATYP 0x01/0x04, anything else as a domain
// (ATYP 0x03) so the upstream resolves it (socks5h semantics). A domain
// longer than the 255-byte length field is rejected here — the old encoder
// wrapped byte(len(host)) silently and shipped a truncated token upstream.
func socksConnectRequest(host string, port uint16) ([]byte, error) {
	var addr []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			addr = append([]byte{0x01}, ip4...)
		} else {
			addr = append([]byte{0x04}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("target %q: FQDN too long", host)
		}
		addr = append([]byte{0x03, byte(len(host))}, host...)
	}
	return append(append([]byte{0x05, 0x01, 0x00}, addr...), byte(port>>8), byte(port)), nil
}

// socksClientConnect performs the greeting + CONNECT exchange on an already
// dialed upstream conn using the pre-encoded request. The conn's deadline is
// set from ctx for the handshake and cleared on success, so the relay phase
// is governed by the shared idle timer alone. net.Conn.Write is
// full-write-or-error by its io.Writer contract, so the err checks below are
// also the short-write checks.
func socksClientConnect(ctx context.Context, conn net.Conn, req []byte) error {
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return err
		}
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	var greet [2]byte
	if _, err := io.ReadFull(conn, greet[:]); err != nil {
		return err
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		return errors.New("upstream refused the greeting")
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var rep [4]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		return err
	}
	if rep[0] != 0x05 || rep[1] != 0x00 {
		return fmt.Errorf("upstream refused (reply 0x%02x)", rep[1])
	}
	// drain BND.ADDR/BND.PORT, reusing the server-side address parser
	if _, err := readATYPAddr(conn, rep[3]); err != nil {
		return err
	}
	var bnd [2]byte
	if _, err := io.ReadFull(conn, bnd[:]); err != nil {
		return err
	}
	return conn.SetDeadline(time.Time{})
}

// relay copies data between client and upstream in both directions until
// either side closes. One shared idle timer covers both directions: any read
// of real bytes re-arms it, so a healthy one-way transfer (e.g. a long
// download with no client upload) is never killed as idle. When the timer
// fires, or both directions have ended, the connection is torn down with
// half-closes (FIN first) so the peers see clean EOF instead of RST.
//
// This deliberately diverges from Python's _relay (and from the old Go
// relay), which tore down BOTH legs on the first empty recv: here the
// surviving direction gets to finish. The cost of letting it — the price of
// the forward half-close property — is that a peer which half-closes and
// then falls silent keeps the relay (both goroutines plus the timer) alive
// for up to the full idle window.
func relay(client, upstream net.Conn, idle time.Duration) {
	var once sync.Once
	// stop closes the conns relay was handed; halfClose reaches the socket
	// through tcpOf, which unwraps the idleConn wrappers below, so the
	// teardown works on wrapped or raw conns alike.
	stop := func() {
		// The peer's socket may still hold unread bytes in our kernel receive
		// queue; a plain Close there makes Linux send RST instead of FIN
		// (tcp_close: data_was_unread), which destroys data the other side
		// has not read yet. Python's _relay instead does shutdown(SHUT_RDWR)
		// on both sockets, which emits FIN (and drops queued bytes without
		// RST), and only then closes. Mirror that: half-close both ends and
		// only then close the descriptors. The Close stays after the
		// half-closes so a copy direction stuck writing to a stalled peer is
		// still unblocked.
		halfClose(client)
		halfClose(upstream)
		client.Close()
		upstream.Close()
	}
	idleTimer := time.AfterFunc(idle, func() { once.Do(stop) })
	defer idleTimer.Stop()
	wake := func() { idleTimer.Reset(idle) }
	wrappedClient := &idleConn{Conn: client, onActivity: wake}
	wrappedUpstream := &idleConn{Conn: upstream, onActivity: wake}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(wrappedUpstream, wrappedClient); err == nil {
			closeWrite(wrappedUpstream)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(wrappedClient, wrappedUpstream); err == nil {
			closeWrite(wrappedClient)
		}
	}()
	wg.Wait()
	once.Do(stop)
}

// idleConn counts real bytes as activity, re-arming the shared idle timer.
//
// WARNING: do not "optimize" the relay's io.Copy by giving idleConn a
// ReadFrom that delegates to the raw conn (or by copying with the raw conns
// instead): the splice fast path would bypass Read entirely, reads would
// stop re-arming the idle timer, and long transfers would be killed as idle.
// Every byte the relay reads must pass through idleConn.Read.
type idleConn struct {
	net.Conn
	onActivity func()
	// count 非 nil 时，每个成功 Read 的字节数累加进去——管理页的每会话
	// 流量计数就挂在这里（方向天然正确：读客户端腿=上行、读目标腿=下行）。
	count *atomic.Uint64
}

func (c *idleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.onActivity()
		if c.count != nil {
			c.count.Add(uint64(n))
		}
	}
	return n, err
}

// halfOf 取出连接背后的原始 conn（拆掉 relay 的 idleConn 包装一层）。
func halfOf(conn net.Conn) net.Conn {
	if c, ok := conn.(*idleConn); ok {
		return c.Conn
	}
	return conn
}

// tcpOf 返回 conn 背后的 *net.TCPConn；其余连接类型返回 nil。库化后
// tailcat 交回的是 netstack 虚拟连接，不是 *net.TCPConn，nil 是常态。
func tcpOf(conn net.Conn) *net.TCPConn {
	tc, _ := halfOf(conn).(*net.TCPConn)
	return tc
}

// closeWrite 发送 FIN：真 TCP 走 CloseWrite；虚拟连接按接口探测
// （tailscale netstack 的 conn 提供同签名的 CloseWrite/CloseRead）。
// 两者都没有则不是错误——随后的 Close 会兜底。
func closeWrite(conn net.Conn) {
	if tc := tcpOf(conn); tc != nil {
		tc.CloseWrite()
		return
	}
	if cw, ok := halfOf(conn).(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// halfClose 是 Go 版的 shutdown(fd, SHUT_RDWR)。anti-RST 性质来自
// CloseWrite：FIN 入队后 tcp_close 不再把未读的排队数据当作"对端还要"，
// 于是 Close 发 FIN 而不是 RST。（单独 CloseRead 给不了这个保证,
// 已实测。）CloseRead 对应 Python 的 shutdown(SHUT_RD)——丢弃后续输入,
// 代价与 Python 相同。错误一律忽略，同 Python 的 except OSError: pass。
// 真 TCP 走方法对；虚拟连接走接口回退；都不支持的留给 Close。
func halfClose(conn net.Conn) {
	c := halfOf(conn)
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite() // FIN to the peer, like shutdown(SHUT_WR)
		tc.CloseRead()  // stop caring about further input, like shutdown(SHUT_RD)
		return
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	if cr, ok := c.(interface{ CloseRead() error }); ok {
		_ = cr.CloseRead()
	}
}
