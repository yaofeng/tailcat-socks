# 监听配置化 + HTTP 代理 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把三个监听地址迁进 `config.yaml`，新增一个与 SOCKS5 共享认证/授权/历史的 HTTP/HTTPS CONNECT 代理入口，版本号升到 1.0.0。

**Architecture:** `config.go` 新增 `Listen{HTTPProxy, SOCKSProxy, WebConsole}` 段。`proxy.go` 把「路由 → 授权 → 注册 → 拨号 → 回执 → relay → 记历史」抽成 `serveTunnel`，SOCKS5 与新的 `httpproxy.go` 各自只做协议解析与回执写入。`main.go` 从配置读三个地址，分别监听，任一失败即 fail-fast。

**Tech Stack:** Go 1.27.1、`net/http` 的 `http.ReadRequest`（仅解析，不复用 server）、现有 `Users`/`DNSMap`/`Dialer`/`History`/`relay`。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `src/config.go`（改） | 新增 `Listen` 段、默认模板、地址校验 |
| `src/config_test.go`（改） | listen 解析/默认值/空串/非法格式 |
| `src/proxy.go`（改） | 抽出 `serveTunnel`；SOCKS5 只保留协议层 |
| `src/proxy_test.go`（改） | 现有用例保持通过（回归防线） |
| `src/httpproxy.go`（新） | HTTP 代理：认证、CONNECT、绝对 URI 转发、501 |
| `src/httpproxy_test.go`（新） | 407/403/200/501、路由与授权断言、历史写入 |
| `src/main.go`（改） | 删 `--listen`/`--admin-addr`；从配置监听三个入口 |
| `.version`（改） | `1.0.0` |
| `docker/Dockerfile`（改） | EXPOSE 7890；CMD 只传 `--data-path` |
| `docker/docker-compose.yml`（改） | 加 7890 端口；删 LISTEN/ADMIN_ADDR |
| `bin/start.sh`（改） | 删 LISTEN 与 `--listen` |
| `README.md`（改） | 监听配置、HTTP 用法、参数表 |

---

### Task 1: 配置新增 listen 段

**Files:**
- Modify: `src/config.go`
- Modify: `src/config_test.go`

- [ ] **Step 1.1 写失败测试（追加到 `src/config_test.go`）**

```go
func TestLoadConfigParsesListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `listen:
  http-proxy: "0.0.0.0:7890"
  socks-proxy: "127.0.0.1:1080"
  web-console: ""
users:
  tailcat: pw
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen.HTTPProxy != "0.0.0.0:7890" {
		t.Errorf("http-proxy = %q", cfg.Listen.HTTPProxy)
	}
	if cfg.Listen.SOCKSProxy != "127.0.0.1:1080" {
		t.Errorf("socks-proxy = %q", cfg.Listen.SOCKSProxy)
	}
	// 空串是合法值，表示不监听该入口。
	if cfg.Listen.WebConsole != "" {
		t.Errorf("web-console = %q, want empty", cfg.Listen.WebConsole)
	}
}

// 生成的默认模板必须带上三个默认地址，否则新装用户不知道有哪些入口。
func TestLoadConfigDefaultTemplateHasListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"http-proxy":  "0.0.0.0:7890",
		"socks-proxy": "0.0.0.0:1080",
		"web-console": "127.0.0.1:8080",
	}
	got := map[string]string{
		"http-proxy":  cfg.Listen.HTTPProxy,
		"socks-proxy": cfg.Listen.SOCKSProxy,
		"web-console": cfg.Listen.WebConsole,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

// 缺 listen 段时同样用默认值：旧配置文件不该因为少了这一段就起不来。
func TestLoadConfigMissingListenUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("users:\n  tailcat: pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.SOCKSProxy != "0.0.0.0:1080" || cfg.Listen.HTTPProxy != "0.0.0.0:7890" {
		t.Errorf("missing listen should fall back to defaults, got %+v", cfg.Listen)
	}
}

// 地址格式错必须报出是哪个入口，不能等到监听时才炸。
func TestLoadConfigRejectsBadListenAddr(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no port", "listen:\n  socks-proxy: \"127.0.0.1\"\n", "socks-proxy"},
		{"bad port", "listen:\n  socks-proxy: \"127.0.0.1:abc\"\n", "socks-proxy"},
		{"port range", "listen:\n  http-proxy: \"0.0.0.0:70000\"\n", "http-proxy"},
		{"garbage", "listen:\n  web-console: \"!!!\"\n", "web-console"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := tt.body + "users:\n  tailcat: pw\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("bad listen address must be rejected")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %q, got %v", tt.want, err)
			}
		})
	}
}
```

- [ ] **Step 1.2 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestLoadConfigParsesListen|TestLoadConfigDefaultTemplateHasListen|TestLoadConfigMissingListenUsesDefaults|TestLoadConfigRejectsBadListenAddr'
```

Expected: FAIL — `cfg.Listen undefined`。

- [ ] **Step 1.3 修改 `src/config.go`**

a) 模板加 `listen:` 段（放在 `users:` 之前，因为它描述部署拓扑）：

```go
const configTemplate = `# tailcat-proxy 配置。此文件含明文密码，权限应为 0600。
listen:
  # 三个入口，空串表示不监听该入口。改完需重启。
  http-proxy: "0.0.0.0:7890"
  socks-proxy: "0.0.0.0:1080"
  web-console: "127.0.0.1:8080"

users:
  # tailcat: 只能访问 dns.txt 命中的域名（走隧道）
  tailcat: "changeme"
  # all: 可访问任意目标（命中走隧道，未命中直连）
  all: "changeme"

history:
  # 历史库固定落在 --data-path 目录下的 history.db。
  enabled: true
`
```

b) `Config` 增加 `Listen` 字段：

```go
// listenDefaults 是三个入口的默认地址。它们同时出现在 configTemplate 里；
// 缺 listen 段的旧配置也回退到这里，不至于因为少一段就起不来。
var listenDefaults = Listen{
	HTTPProxy:  "0.0.0.0:7890",
	SOCKSProxy: "0.0.0.0:1080",
	WebConsole: "127.0.0.1:8080",
}

// Listen 是三个监听入口的地址。空串表示不监听该入口——比如只想要 SOCKS5，
// 就把 http-proxy 设成 ""。
type Listen struct {
	HTTPProxy  string `yaml:"http-proxy"`
	SOCKSProxy string `yaml:"socks-proxy"`
	WebConsole string `yaml:"web-console"`
}

type Config struct {
	Listen  Listen            `yaml:"listen"`
	Users   map[string]string `yaml:"users"`
	History struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"history"`
}
```

c) 解码后补默认值与校验。在 `dec.Decode(&cfg)` 之后、用户表检查之前插入：

```go
	// listen 段缺失的字段（含整段缺失）回退到默认值；YAML 里显式写的 ""
	// 是"不监听"，必须与"没写"区分开——只给没出现的字段填默认值。
	if cfg.Listen.HTTPProxy == "" && !listenKeyPresent(data, "http-proxy") {
		cfg.Listen.HTTPProxy = listenDefaults.HTTPProxy
	}
	if cfg.Listen.SOCKSProxy == "" && !listenKeyPresent(data, "socks-proxy") {
		cfg.Listen.SOCKSProxy = listenDefaults.SOCKSProxy
	}
	if cfg.Listen.WebConsole == "" && !listenKeyPresent(data, "web-console") {
		cfg.Listen.WebConsole = listenDefaults.WebConsole
	}
	for _, l := range []struct{ name, addr string }{
		{"http-proxy", cfg.Listen.HTTPProxy},
		{"socks-proxy", cfg.Listen.SOCKSProxy},
		{"web-console", cfg.Listen.WebConsole},
	} {
		if l.addr == "" {
			continue // 空串 = 不监听
		}
		if err := validateListenAddr(l.addr); err != nil {
			return nil, fmt.Errorf("config %s: listen.%s: %w", path, l.name, err)
		}
	}
```

d) 同文件追加两个辅助函数：

```go
// listenKeyPresent 报告 YAML 里 listen.<key> 是否被显式写过。用它区分"没写"
// 与"写成空串"：前者要补默认值，后者是"不监听"的明确意图。
func listenKeyPresent(data []byte, key string) bool {
	var probe struct {
		Listen map[string]any `yaml:"listen"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	_, ok := probe.Listen[key]
	return ok
}

// validateListenAddr 校验 host:port 形状与端口范围。空 host（":1080"）合法，
// 表示绑所有接口。
func validateListenAddr(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q: %w (want host:port)", addr, err)
	}
	_ = host // 空 host 合法
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%q: port %q is not a number", addr, portStr)
	}
	if p < 0 || p > 65535 {
		return fmt.Errorf("%q: port %d out of range", addr, p)
	}
	return nil
}
```

`config.go` 的 import 需补 `"net"` 与 `"strconv"`。

- [ ] **Step 1.4 运行确认通过**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestLoadConfig' -v
```

Expected: 全部 PASS（含新增 4 个）。

- [ ] **Step 1.5 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/config.go src/config_test.go
git commit -m "feat(config): add listen section for the three entry points

http-proxy, socks-proxy and web-console move into config.yaml — they are
deployment topology and belong where the rest of the deployment is
described. An explicit empty string means \"do not listen\", which is why
the default fill only touches keys the YAML never mentioned.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: 抽出 serveTunnel，SOCKS5 改为调用它

**Files:**
- Modify: `src/proxy.go`

- [ ] **Step 2.1 先确认现有测试全绿（回归基线）**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -count=1
```

Expected: PASS。后续步骤结束后必须仍然 PASS。

- [ ] **Step 2.2 修改 `src/proxy.go`：新增 `tunnelHandlers` 与 `serveTunnel`**

在 `serveConn` 之前插入：

```go
// tunnelHandlers 是协议层提供的两个钩子：拨号失败时怎么回包，拨号成功后
// 怎么回执。分开是因为两者形式不同——SOCKS5 是两串固定字节，HTTP 是一个
// 状态行加头部；而且失败回执必须由协议层写，serveTunnel 不该知道任何协议。
type tunnelHandlers struct {
	onDialError func(err error)
	onReady     func(client net.Conn) error
}

// serveTunnel 是一条已授权连接的完整生命周期：注册会话、拨号、回执、
// relay、记历史。它拥有 client 与 up 两条连接并负责关闭。
//
// 授权必须在调用本函数之前完成——这里不再看用户权限，只执行已经批准的目标。
// 被拒的请求绝不能走到这里：否则会被记成一次失败访问。
func (s *Server) serveTunnel(client net.Conn, host string, port uint16, target Target, h tunnelHandlers) {
	sess := s.Registry.New(client.RemoteAddr().String(), host, target.Host, int(target.Port), target.Via)
	defer s.Registry.Drop(sess.ID)
	// 连接结束时并入历史。failed 取反 Opened：没进 relay 的会话就是失败
	// （拨号失败或客户端在回执后立刻断开）。
	defer func() {
		if s.History == nil {
			return
		}
		ip := clientIP(client.RemoteAddr())
		s.History.Record(time.Now().Format("2006-01-02"), ip, host, int(port),
			!sess.Opened, sess.BytesUp.Load(), sess.BytesDown.Load())
	}()

	up, err := s.Dialer.Dial(context.Background(), target)
	if err != nil {
		slog.Warn("dial failed", "via", target.Via, "host", host, "port", target.Port, "err", err)
		if h.onDialError != nil {
			h.onDialError(err)
		}
		return
	}
	if h.onReady != nil {
		if err := h.onReady(client); err != nil {
			up.Close()
			return
		}
	}
	s.Registry.MarkOpen(sess)

	client.SetDeadline(time.Time{})
	up.SetDeadline(time.Time{})
	relay(client, up, s.relayIdle, sess)
}
```

- [ ] **Step 2.3 让 `serveConn` 委托给 `serveTunnel`**

把 `serveConn` 尾部从 `sess := s.Registry.New(...)` 到函数结束整段替换为：

```go
	s.serveTunnel(client, host, port, target, tunnelHandlers{
		onDialError: func(error) { socksFail(client) },
		onReady: func(c net.Conn) error {
			_, err := c.Write(socksOKReply)
			return err
		},
	})
```

（`target` 与授权检查在上一段已经算好，保持不变。）

- [ ] **Step 2.4 删掉 `serveConn` 里已经搬走的 deadline 重设**

`serveConn` 原本在拨号后有一句 `client.SetDeadline(time.Now().Add(s.handshakeTimeout))` 用来给回执一个新窗口——现在回执发生在 `serveTunnel` 内部、紧接着拨号，所以把这句移到 `serveTunnel` 的 `onReady` 之前。在 `serveTunnel` 里 `up, err := s.Dialer.Dial(...)` 之后、`if err != nil` 之前插入：

```go
	// 拨号可能吃满了握手预算；给回执一个新窗口，与重构前一致。
	client.SetDeadline(time.Now().Add(s.handshakeTimeout))
```

同时确认 `serveConn` 里不再有重复的 `client.SetDeadline(...Add(s.handshakeTimeout))`。

- [ ] **Step 2.5 运行确认无回归**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -count=1 && GOPROXY=off go test ./... -race -count=1
```

Expected: 全部 PASS，**现有 SOCKS5 用例一条都不能改**。若有失败，说明重构改变了行为，必须修实现而不是改测试。

- [ ] **Step 2.6 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/proxy.go
git commit -m "refactor(proxy): extract serveTunnel shared by both entry points

Routing, authorization, session accounting and history are the same for
SOCKS5 and the upcoming HTTP proxy; only the wire format differs. The
protocol layer now supplies two hooks (dial failure, ready reply) and
serveTunnel owns registration, dialing, relay and the history row, so the
two entries cannot drift apart on who is allowed to reach what.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: HTTP 代理入口

**Files:**
- Create: `src/httpproxy.go`
- Create: `src/httpproxy_test.go`

- [ ] **Step 3.1 写失败测试 `src/httpproxy_test.go`**

```go
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

```

- [ ] **Step 3.2 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestHTTPProxy'
```

Expected: FAIL — `undefined: HTTPProxy`、`undefined: NewHTTPProxy`。

- [ ] **Step 3.3 实现 `src/httpproxy.go`**

```go
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
	srv      *Server
	ln       net.Listener
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

// writeHTTPStatus 写一个只有状态行与头部的响应。Head 为 nil 时给个最小集合。
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
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadPort
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return 0, errBadPort
		}
	}
	if n == 0 {
		return 0, errBadPort
	}
	return uint16(n), nil
}

var errBadPort = errors.New("invalid port")
```

- [ ] **Step 3.4 运行确认通过**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestHTTPProxy' -v
```

Expected: 7 个测试全部 PASS。

- [ ] **Step 3.5 全量 + race**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -count=1 && GOPROXY=off go test ./... -race -count=1 && GOPROXY=off go vet ./... && gofmt -l .
```

Expected: 全 PASS，vet/gofmt 无输出。

- [ ] **Step 3.6 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/httpproxy.go src/httpproxy_test.go
git commit -m "feat(http): HTTP/HTTPS CONNECT proxy sharing socks5 policy

A second entry point so browsers and tools that speak HTTP proxying get
the same reach rules: CONNECT tunnels and absolute-URI requests both go
through route() + Mode.Allows() and then the shared serveTunnel, so a
tailcat user cannot reach an unmatched host through this door either.
407 for auth, 403 for policy, 502 for upstream failure — the HTTP
equivalents of the SOCKS reply codes.

Absolute-URI requests are rewritten to origin-form and have
Proxy-Authorization stripped before forwarding; anything else is 501.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: main 接线、版本号与部署面

**Files:**
- Modify: `src/main.go`
- Modify: `.version`
- Modify: `docker/Dockerfile`、`docker/docker-compose.yml`、`bin/start.sh`、`README.md`

- [ ] **Step 4.1 修改 `src/main.go`**

a) flag 块删掉 `listen` 与 `adminAddr`，`dataFiles` 保持不变：

```go
	var (
		dataPath    = flag.String("data-path", "data", "directory holding dns.txt, config.yaml, tailcat-client-key and history.db")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("component", "tailcat-proxy"))
	files, err := newDataFiles(*dataPath)
	if err != nil {
		slog.Error("bad --data-path", "value", *dataPath, "err", err)
		os.Exit(1)
	}
	os.Exit(run(files))
```

b) `run` 签名改为 `func run(files dataFiles) int`，并删掉原有的 `--listen` 校验块（地址校验已在 config 层做完）。

c) 在配置加载之后、构造 dialer 之前插入三个入口的启动。**顺序很重要**：先监听，全部成功后再起 goroutine，这样任一个绑定失败都能干净退出：

```go
	// 三个入口都先绑定再启动：任一个端口被占都是部署错误，静默少起一个
	// 入口比直接报错更糟。
	var (
		socksSrv *Server
		httpSrv  *HTTPProxy
		haveProxy bool
	)
	if cfg.Listen.SOCKSProxy != "" {
		socksSrv, err = NewServerWithUsers(dnsMap, dialer, registry, cfg.Listen.SOCKSProxy, users, history)
		if err != nil {
			slog.Error("cannot listen socks", "addr", cfg.Listen.SOCKSProxy, "err", err)
			return 1
		}
		haveProxy = true
	}
	if cfg.Listen.HTTPProxy != "" {
		httpSrv, err = NewHTTPProxy(dnsMap, dialer, registry, cfg.Listen.HTTPProxy, users, history)
		if err != nil {
			slog.Error("cannot listen http", "addr", cfg.Listen.HTTPProxy, "err", err)
			if socksSrv != nil {
				socksSrv.Close()
			}
			return 1
		}
		haveProxy = true
	}
	// 只有管理页的配置没有意义：没有任何代理入口等于这个进程什么都不做。
	if !haveProxy {
		slog.Error("no proxy entry point configured; set listen.socks-proxy and/or listen.http-proxy in config.yaml")
		return 1
	}
```

d) admin 段改为读 `cfg.Listen.WebConsole`：

```go
	var adminSrv *http.Server
	if cfg.Listen.WebConsole != "" {
		api := NewAdmin(registry, dnsMap, files.dns)
		api.SetHistory(history)
		adminLn, err := net.Listen("tcp", cfg.Listen.WebConsole)
		if err != nil {
			slog.Error("cannot listen admin", "addr", cfg.Listen.WebConsole, "err", err)
			if socksSrv != nil {
				socksSrv.Close()
			}
			if httpSrv != nil {
				httpSrv.Close()
			}
			return 1
		}
		adminSrv = &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server error", "err", err)
			}
		}()
		slog.Info("admin listening", "url", "http://"+cfg.Listen.WebConsole+"/")
	}
```

e) 启动两个代理的 Serve goroutine，并汇总错误：

```go
	serveErr := make(chan error, 2)
	if socksSrv != nil {
		go func() { serveErr <- socksSrv.Serve() }()
		host, port, _ := net.SplitHostPort(socksSrv.ActualAddr().String())
		slog.Info("socks listening", "url", "socks5h://"+net.JoinHostPort(host, port))
	}
	if httpSrv != nil {
		go func() { serveErr <- httpSrv.Serve() }()
		host, port, _ := net.SplitHostPort(httpSrv.ActualAddr().String())
		slog.Info("http listening", "url", "http://"+net.JoinHostPort(host, port))
	}
```

f) 退出段关闭两个 listener：

```go
	cancel() // stops the dns watcher
	if socksSrv != nil {
		socksSrv.Close()
	}
	if httpSrv != nil {
		httpSrv.Close()
	}
```

- [ ] **Step 4.2 编译与全量验证**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go build ./... && GOPROXY=off go test ./... -count=1 && GOPROXY=off go test ./... -race -count=1 && GOPROXY=off go vet ./... && gofmt -l .
```

Expected: 全 PASS。若 `main_test.go` 引用了已删的 flag，删掉对应测试。

- [ ] **Step 4.3 冒烟：三个入口都在**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
rm -rf /tmp/smoke6 && mkdir -p /tmp/smoke6
cat > /tmp/smoke6/config.yaml <<'EOF'
listen:
  http-proxy: "127.0.0.1:17890"
  socks-proxy: "127.0.0.1:17108"
  web-console: "127.0.0.1:17080"
users:
  tailcat: "pw1"
  all: "pw2"
history:
  enabled: true
EOF
cp data/dns.txt.example /tmp/smoke6/dns.txt
GOPROXY=off go build -o /tmp/smoke6/tcp ./src 2>/dev/null || (cd src && GOPROXY=off go build -o /tmp/smoke6/tcp .)
/tmp/smoke6/tcp --data-path /tmp/smoke6 > /tmp/smoke6/run.log 2>&1 &
sleep 2
echo '--- socks ---'; curl -sS -m 10 --socks5-hostname 127.0.0.1:17108 --proxy-user all:pw2 http://example.com/ -o /dev/null -w 'socks http=%{http_code}\n'
echo '--- http ---';   curl -sS -m 10 -x http://127.0.0.1:17890 --proxy-user all:pw2 http://example.com/ -o /dev/null -w 'http http=%{http_code}\n'
echo '--- admin ---';  curl -sS -m 5 -o /dev/null -w 'admin http=%{http_code}\n' http://127.0.0.1:17080/
echo '--- admin history ---'; curl -sS -m 5 http://127.0.0.1:17080/api/history | head -c 300; echo
kill %1 2>/dev/null
```

Expected: `socks http=200`、`http http=200`、`admin http=200`，历史 JSON 里有记录。

- [ ] **Step 4.4 版本号与部署面**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
printf '1.0.0\n' > .version && cat .version
```

`docker/Dockerfile`：`EXPOSE` 增加 7890，CMD 去掉 `--listen`/`--admin-addr`：

```dockerfile
EXPOSE 1080 7890
CMD ["sh", "-c", "exec tailcat-proxy --data-path \"$DATA_PATH\""]
```

并删除 `ENV` 里的 `LISTEN` 与 `ADMIN_ADDR`（地址现在在 config.yaml）：

```dockerfile
ENV DATA_PATH=/data
```

`docker/docker-compose.yml`：端口加 7890，删 `LISTEN`/`ADMIN_ADDR` 环境变量：

```yaml
    ports:
      - "1080:1080"                     # SOCKS5
      - "7890:7890"                     # HTTP/HTTPS CONNECT
      - "127.0.0.1:8080:8080"           # admin page, host loopback only
    environment:
      DATA_PATH: "/data"
```

`bin/start.sh`：删 `LISTEN` 变量与 `--listen` 参数：

```bash
DATA_PATH="${DATA_PATH:-$ROOT/data}"
...
ARGS=(--data-path "$DATA_PATH")
```

- [ ] **Step 4.5 更新 `README.md`**

参数表删掉 `--listen` 与 `--admin-addr` 两行，保留 `--data-path` / `--log-level` / `--version`。新增一节：

```markdown
## 监听配置

三个入口都在 `<data-path>/config.yaml` 里配置，**空串表示不监听该入口**：

```yaml
listen:
  http-proxy: "0.0.0.0:7890"    # HTTP/HTTPS CONNECT 代理
  socks-proxy: "0.0.0.0:1080"   # SOCKS5 代理
  web-console: "127.0.0.1:8080" # 管理页
```

- 至少要配置一个代理入口（socks-proxy 或 http-proxy），否则启动失败。
- 任一入口绑定失败都是 fail-fast，不会静默少起一个。
- 地址改了需重启。

## HTTP 代理

```bash
# 普通 HTTP
curl -x http://127.0.0.1:7890 --proxy-user all:密码 http://example.com/
# HTTPS 走 CONNECT（curl 自动处理）
curl -x http://127.0.0.1:7890 --proxy-user all:密码 https://example.com/
```

认证与授权与 SOCKS5 完全一致：`tailcat` 用户只能访问 dns.txt 命中的域名（未命中返回 `403`），`all` 用户不限。认证失败返回 `407`，上游拨号失败返回 `502`。
```

- [ ] **Step 4.6 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/main.go .version docker/Dockerfile docker/docker-compose.yml bin/start.sh README.md
git commit -m "feat!: listen from config, HTTP entry in main, bump to 1.0.0

--listen and --admin-addr (and the ADMIN_ADDR env var) are gone; the three addresses
now live in config.yaml under listen:. main binds all of them before
starting any goroutine so a taken port fails the whole start instead of
silently dropping an entry, and refuses to run with no proxy entry at all.

1.0.0 rather than a minor bump: the CLI changed incompatibly (existing
--listen invocations now fail on an unknown flag) and there is a new
public entry point.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 自查

**1. Spec 覆盖**

| Spec 章节 | 对应任务 |
|---|---|
| §3 配置结构、默认值、空串语义、地址校验 | Task 1 |
| §4.1 协议范围（CONNECT + 绝对 URI + 501） | Task 3 |
| §4.2 认证 407 | Task 3 |
| §4.3 授权 403 | Task 3 |
| §4.4 复用 serveTunnel | Task 2、Task 3 |
| §4.5 新增文件与类型 | Task 3 |
| §5 启动、fail-fast、至少一个入口、退出顺序 | Task 4 |
| §6 历史规则 | Task 3（测试覆盖）、Task 4（接线） |
| §8 部署面 | Task 4 |
| §9 兼容性（1.0.0） | Task 4 |

**2. 占位符扫描**：无 TBD/TODO；每个代码步骤都有完整代码。

**3. 类型一致性**：`Listen`（Task 1）在 Task 4 用 `cfg.Listen.SOCKSProxy` 等字段；`HTTPProxy`/`NewHTTPProxy`/`ActualAddr`/`Serve`/`Close`（Task 3）在 Task 4 一致；`tunnelHandlers{onDialError, onReady}`（Task 2）在 Task 3 一致；`parsePort` 在 Task 3 内部一致。
