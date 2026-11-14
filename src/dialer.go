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
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/envknob"
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

	// derpOnlyEnv 是 tailscale 的 magicsock 开关：非空且为真值时禁用 UDP，
	// 所有对端流量强制走 DERP 中继。见 pinDERPDefault。
	derpOnlyEnv = "TS_DEBUG_ALWAYS_USE_DERP"
)

// pinDERPDefault 默认强制隧道走 DERP 中继，除非用户自己设了该环境变量。
//
// 为什么默认强制：本机有 9 个本地接口（bond0 + docker0 + br-* + cali* +
// flannel.1），magicsock 会把它们全部上报成 candidate endpoint。实测
// （2026-11-14，a100-2-01.yidong.tail）打洞成功后 magicsock 会把路径从
// DERP "升级"成直连，而这条直连只有 ~17 KB/s，同一 token 走 DERP 中继
// 则是 1.4–2.7 MB/s——一旦升级就再也不回落，吞吐永久塌陷。首次拨号
// （在打洞完成之前）总是走 DERP，所以表现为"第一次快、之后全慢"。
//
// 代价：放弃直连可能带来的更低 RTT。对 SOCKS 代理的批量传输场景，
// 稳定的 2 MB/s 远胜于偶尔打通的 17 KB/s。
//
// 必须在任何 tailscale 包代码读取该 envknob 之前执行。envknob 在
// RegisterBool 首次调用时就把 os.Getenv 的结果固化进一个内部指针
// （envknob.go:165-176），此后 os.Setenv 一律无效；能改写该指针的只有
// envknob.Setenv（同文件 :94）。所以这里必须走 envknob.Setenv，而不是
// os.Setenv——后者实测无论放在 main 还是本包 init 都不生效。
func pinDERPDefault() {
	if _, ok := os.LookupEnv(derpOnlyEnv); ok {
		return // 用户显式设置（含空串/0/false），尊重之
	}
	envknob.Setenv(derpOnlyEnv, "1")
}

func init() { pinDERPDefault() }

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

// route 决定去向：命中 dns.txt 的域名交隧道（大小写不敏感匹配、token 原样
// 返回），其余——未命中域名与 IP 字面量一律——直接 TCP 拨号。用显式 ok 查找
// 而非 RewriteHost 的 token!=host 哨兵：手滑条目 token 恰等于 domain 时，
// 哨兵会把本该隧道化的流量静默放行成明文直连。
func route(host string, port uint16, m map[string]string) Target {
	if token, ok := m[strings.ToLower(host)]; ok {
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
		perr := json.Unmarshal(data, &conf)
		if perr == nil && !conf.Private.IsZero() {
			return conf.Private
		}
		// perr==nil 而 IsZero 时 err 记 null：两种坏法都值得一行日志原文。
		slog.Warn("client key file is corrupt; using an ephemeral key (file left untouched)", "path", path, "err", perr)
		return key.NewNode()
	case errors.Is(err, fs.ErrNotExist):
		conf := tailcat.NewPrivateKey()
		if b, merr := json.MarshalIndent(conf, "", "  "); merr != nil {
			slog.Warn("cannot encode client key; identity will not survive restarts", "path", path, "err", merr)
		} else if werr := os.WriteFile(path, b, 0o600); werr != nil {
			slog.Warn("cannot persist client key; identity will not survive restarts", "path", path, "err", werr)
		} else if cerr := os.Chmod(path, 0o600); cerr != nil {
			// WriteFile 的 perm 会被 umask 过滤；Chmod 钉死 0600（私钥文件）。
			slog.Warn("cannot pin client key file mode", "path", path, "err", cerr)
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
