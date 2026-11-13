package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

const (
	// Target.Via / Session.Via 的取值。
	ViaTunnel = "tunnel"
	ViaDirect = "direct"

	// dialTimeout 是一次拨号的独立预算。隧道首拨需要：取 DERP map（地址未
	// 内嵌时）、magicsock 打洞、WireGuard 握手重传——官方 CLI 的 socks 模式
	// 同样脱离调用方 ctx 另挂 15s（cmd/tailcat/tailcat.go 的 socks dialer），
	// 理由照搬：客户端侧的 deadline 不该掐断握手重传。
	dialTimeout = 15 * time.Second
)

// Target 是一条路由决策的结果：如何抵达 host:port。
type Target struct {
	Via  string // ViaTunnel 或 ViaDirect
	Host string // tunnel: tailcat token（原样保留大小写）；direct: 原始 host
	Port uint16
}

// Dialer 把 Target 变成一条已建立的连接。测试注入 fakeDialer。
type Dialer interface {
	Dial(ctx context.Context, t Target) (net.Conn, error)
	Close() error
}

// route 决定去向：命中 dns.txt 的域名交隧道（RewriteHost 大小写不敏感匹配、
// 返回原样 token），其余——未命中域名与 IP 字面量一律——直接 TCP 拨号。
func route(host string, port uint16, m map[string]string) Target {
	if token := RewriteHost(host, m); token != host {
		return Target{Via: ViaTunnel, Host: token, Port: port}
	}
	return Target{Via: ViaDirect, Host: host, Port: port}
}

// AppDialer 是生产 Dialer：不再有 tailcat 子进程。隧道目标用按 token
// 缓存的 tailcat.Client（惰性创建，官方 CLI 同款 mutex+map 模式），直连
// 目标用普通 net.Dialer。所有 client 共享一个持久化的节点身份，服务器
// 侧 allowlist 重启后仍然有效。
type AppDialer struct {
	mu      sync.Mutex
	clients map[tailcat.Addr]*tailcat.Client
	priv    key.NodePrivate
	direct  *net.Dialer
}

// NewAppDialer 加载（或创建并持久化）keyPath 处的客户端身份。
func NewAppDialer(keyPath string) *AppDialer {
	return &AppDialer{
		clients: map[tailcat.Addr]*tailcat.Client{},
		priv:    loadOrCreateClientKey(keyPath),
		direct:  &net.Dialer{Timeout: dialTimeout},
	}
}

// loadOrCreateClientKey 读取 path 处 JSON 化的 tailcat.PrivateKey（与官方
// CLI `genkey --client` 写出的文件格式相同），首次使用时创建并以 0600
// 落盘。读/解析/写失败一律降级为临时 key + WARN（spec §10）：隧道照常，
// 只是身份不跨重启。已存在但损坏的文件**绝不覆盖**——宁可临时身份，
// 不烧用户的手改过的 key。
func loadOrCreateClientKey(path string) key.NodePrivate {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var conf tailcat.PrivateKey
		if err := json.Unmarshal(data, &conf); err == nil && !conf.Private.IsZero() {
			return conf.Private
		}
		slog.Warn("client key file is corrupt; using an ephemeral key (file left untouched)", "path", path)
		return key.NewNode()
	case errors.Is(err, fs.ErrNotExist):
		conf := tailcat.NewPrivateKey()
		if b, merr := json.MarshalIndent(conf, "", "  "); merr == nil {
			if werr := os.WriteFile(path, b, 0o600); werr != nil {
				slog.Warn("cannot persist client key; identity will not survive restarts", "path", path, "err", werr)
			}
		}
		return conf.Private
	default:
		slog.Warn("cannot read client key file; using an ephemeral key", "path", path, "err", err)
		return key.NewNode()
	}
}

// clientFor 返回该 token 对应的缓存 client，先做 ParseAddr 校验，坏
// token 立刻失败而不是烧完一个拨号超时。client 拨号失败后**保留**在
// 缓存里：magicsock 的端点缓存不丢，下一次拨更快。
func (a *AppDialer) clientFor(token string) (*tailcat.Client, error) {
	addr := tailcat.Addr(token)
	if _, err := tailcat.ParseAddr(addr); err != nil {
		return nil, fmt.Errorf("bad tailcat token %q: %w", token, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[addr]; ok {
		return c, nil
	}
	c := tailcat.NewClient(addr)
	c.Key = a.priv
	c.Logf = func(format string, args ...any) {
		slog.Debug("tailcat: "+fmt.Sprintf(format, args...), "token", token)
	}
	a.clients[addr] = c
	return c, nil
}

// Dial 建立一条路由后的连接。隧道拨号脱离调用方 ctx 另挂 dialTimeout
// （见 const 注释）；直连拨号尊重调用方 ctx，net.Dialer.Timeout 同值兜底。
func (a *AppDialer) Dial(ctx context.Context, t Target) (net.Conn, error) {
	switch t.Via {
	case ViaTunnel:
		c, err := a.clientFor(t.Host)
		if err != nil {
			return nil, err
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dialTimeout)
		defer cancel()
		return c.DialTCPPort(dctx, t.Port)
	case ViaDirect:
		return a.direct.DialContext(ctx, "tcp", net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port))))
	default:
		return nil, fmt.Errorf("unknown via %q", t.Via)
	}
}

// Close 关掉所有缓存的 client（DERP 连接、网络栈），进程退出时调用。
func (a *AppDialer) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var errs []error
	for addr, c := range a.clients {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close tailcat client %s: %w", addr, err))
		}
		delete(a.clients, addr)
	}
	return errors.Join(errs...)
}
