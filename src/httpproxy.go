package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPProxy 是 HTTP/HTTPS CONNECT 入口。它与 SOCKS5 入口共享路由、用户表、
// 历史与 relay，只负责 HTTP 语义的解析与回包——授权、计数、写历史都交给
// serveTunnel，两个入口因此不会各有一套规则。
type HTTPProxy struct {
	srv       *Server
	ln        net.Listener
	handshake time.Duration
}

// NewHTTPProxy 绑定监听地址。dnsMap/dialer/registry/users/history 与 SOCKS5
// 入口是同一组对象——它们本来就该是同一份，而不是每个入口一份。
func NewHTTPProxy(dnsMap *DNSMap, d Dialer, reg *ConnRegistry, listen string, users Users, hist *History) (*HTTPProxy, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &HTTPProxy{
		srv: &Server{DNSMap: dnsMap, Dialer: d, Registry: reg, Users: users, History: hist,
			relayIdle: relayIdleTimeout, handshakeTimeout: handshakeTimeout},
		ln:        ln,
		handshake: handshakeTimeout,
	}, nil
}

func (h *HTTPProxy) ActualAddr() net.Addr { return h.ln.Addr() }
func (h *HTTPProxy) Close() error         { return h.ln.Close() }

// Serve 接受连接直到 listener 关闭。退避逻辑与 SOCKS5 入口一致。
func (h *HTTPProxy) Serve() error {
	var tempDelay time.Duration
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if tempDelay > time.Second {
				tempDelay = time.Second
			}
			slog.Warn("http accept failed; backing off", "err", err, "retry_in", tempDelay)
			time.Sleep(tempDelay)
			continue
		}
		tempDelay = 0
		go h.handle(conn)
	}
}

func (h *HTTPProxy) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(h.handshake))

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	user, ok := h.authenticate(req)
	if !ok {
		writeHTTPStatus(conn, http.StatusProxyAuthRequired, map[string]string{
			"Proxy-Authenticate": `Basic realm="tailcat-proxy"`,
		})
		return
	}

	if req.Method == http.MethodConnect {
		h.serveConnect(conn, req, user)
		return
	}
	h.serveAbsoluteURI(conn, br, req, user)
}

// authenticate 校验 Proxy-Authorization: Basic。缺失与错误都返回 false——
// 不区分"用户不存在"与"密码错误"，免得变成用户名枚举。
func (h *HTTPProxy) authenticate(req *http.Request) (User, bool) {
	hdr := req.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(hdr, prefix) {
		return User{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(hdr[len(prefix):]))
	if err != nil {
		return User{}, false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return User{}, false
	}
	return h.srv.Users.Authenticate(user, pass)
}

// serveConnect 处理 CONNECT：授权后建隧道，回 200 再原样转发。
func (h *HTTPProxy) serveConnect(conn net.Conn, req *http.Request, user User) {
	host, portStr, err := net.SplitHostPort(req.Host)
	if err != nil {
		writeHTTPStatus(conn, http.StatusBadRequest, nil)
		return
	}
	port, err := parsePort(portStr)
	if err != nil {
		writeHTTPStatus(conn, http.StatusBadRequest, nil)
		return
	}

	target := route(host, port, h.srv.DNSMap.Load())
	if !user.Mode.Allows(target.Via) {
		writeHTTPStatus(conn, http.StatusForbidden, nil)
		slog.Warn("http target denied by user policy", "host", host, "port", port, "via", target.Via)
		return
	}

	conn.SetDeadline(time.Now().Add(h.handshake))
	h.srv.serveTunnel(conn, host, port, target, tunnelHandlers{
		onDialError: func(error) { writeHTTPStatus(conn, http.StatusBadGateway, nil) },
		onReady: func(c net.Conn) error {
			_, werr := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			return werr
		},
	})
}

// serveAbsoluteURI 处理普通 HTTP 请求（GET http://host/path）。只做最小实现：
// 把请求行改写成 origin-form 后转发，响应原样回传。不做缓存、不解析响应。
func (h *HTTPProxy) serveAbsoluteURI(conn net.Conn, br *bufio.Reader, req *http.Request, user User) {
	if req.URL == nil || !req.URL.IsAbs() || req.URL.Host == "" {
		writeHTTPStatus(conn, http.StatusNotImplemented, nil)
		return
	}
	host := req.URL.Hostname()
	port := req.URL.Port()
	var p uint16
	if port == "" {
		p = 80
	} else {
		var perr error
		p, perr = parsePort(port)
		if perr != nil {
			writeHTTPStatus(conn, http.StatusBadRequest, nil)
			return
		}
	}

	target := route(host, p, h.srv.DNSMap.Load())
	if !user.Mode.Allows(target.Via) {
		writeHTTPStatus(conn, http.StatusForbidden, nil)
		return
	}

	conn.SetDeadline(time.Now().Add(h.handshake))
	h.srv.serveTunnel(conn, host, p, target, tunnelHandlers{
		onDialError: func(error) { writeHTTPStatus(conn, http.StatusBadGateway, nil) },
		onReady: func(c net.Conn) error {
			// 改写请求行：绝对 URI -> origin-form。其余头部原样保留，
			// 但要剥掉 Proxy-Authorization（那是给代理的，不该泄露给目标）。
			out := new(strings.Builder)
			out.WriteString(req.Method + " " + req.URL.RequestURI() + " " + req.Proto + "\r\n")
			// Host 必须单独写：http.ReadRequest 把 Host 从 Header 里摘出来了，
			// 用 Header.Write 不会输出它，转发出去就成了没有 Host 的 HTTP/1.1。
			out.WriteString("Host: " + req.Host + "\r\n")
			for k, vs := range req.Header {
				// Proxy-Authorization 是给代理的凭据，绝不能泄露给目标；
				// Proxy-Connection 是逐跳头，同理由代理消费掉。
				if strings.EqualFold(k, "Proxy-Authorization") || strings.EqualFold(k, "Proxy-Connection") {
					continue
				}
				for _, v := range vs {
					out.WriteString(k + ": " + v + "\r\n")
				}
			}
			out.WriteString("\r\n")
			_, werr := c.Write([]byte(out.String()))
			return werr
		},
	})
}

// writeHTTPStatus 写一个只有状态行与头部的响应。extra 为 nil 时给个最小集合。
func writeHTTPStatus(w net.Conn, code int, extra map[string]string) {
	sb := new(strings.Builder)
	sb.WriteString("HTTP/1.1 " + strconv.Itoa(code) + " " + http.StatusText(code) + "\r\n")
	sb.WriteString("Content-Length: 0\r\n")
	sb.WriteString("Connection: close\r\n")
	for k, v := range extra {
		sb.WriteString(k + ": " + v + "\r\n")
	}
	sb.WriteString("\r\n")
	_, _ = w.Write([]byte(sb.String()))
}

// parsePort 把字符串端口转成 uint16，拒绝 0 与越界值。
func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, errBadPort
	}
	return uint16(n), nil
}

var errBadPort = errors.New("invalid port")
