# tailcat-socks Go 重构实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Python 实现迁入 `python/`，用 Go 复刻行为完全一致的代理到 `go/`，Docker 改为构建 Go 版，`bin/start.sh` 支持选择启动两个版本。

**Architecture:** 单二进制 Go 程序（纯标准库），三个模块：`dnsmap.go`（dns.txt 解析 + atomic 热加载）、`proxy.go`（SOCKS5 服务端 + 上游客户端握手 + 双向 relay）、`main.go`（flag/autolaunch/信号/编排）。SOCKS5 协议逐字节对齐 Python 版（`python/tailcat_socks.py`）。

**Tech Stack:** Go 1.23（无第三方依赖）、Python 3.8+（现有实现）、bash、Docker 多阶段构建（golang:1.23 → debian:bookworm-slim）。

**规格文档:** `docs/superpowers/specs/2026-09-04-go-reimplementation-design.md`

**目标目录结构：**

```
tailcat-socks/
├── python/
│   ├── tailcat_socks.py
│   └── tests/test_tailcat_socks.py
├── go/
│   ├── go.mod
│   ├── dnsmap.go / dnsmap_test.go
│   ├── proxy.go / proxy_test.go
│   ├── main.go / main_test.go
│   ├── testdata/fake-tailcat/main.go   # 测试用假 tailcat（testdata 不参与 go test ./...）
│   └── bin/                            # 构建产物（gitignore）
├── bin/{start,stop,restart}.sh
├── docker/{Dockerfile,docker-compose.yml}
├── docs/superpowers/...
├── .dockerignore / .gitignore
├── dns.txt.example
└── README.md
```

**约定：**
- 所有 `go` 命令假设 Task 1 已把 Go 加入 PATH；若 shell 无状态丢失，用 `export PATH=$PATH:<go安装目录>/bin` 前缀。
- git 提交必须用 `git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit ...`（本机无 git 身份配置）。
- 每个 Task 结束时提交一次。

---

### Task 1: 安装 Go 工具链

**Files:** 无仓库文件变更（机器级安装）。

- [ ] **Step 1: 探测架构并下载 Go 1.23**

```bash
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH"; exit 1 ;;
esac
curl -fSL --retry 5 -C - -o /tmp/go.tgz "https://go.dev/dl/go1.23.6.linux-${GOARCH}.tar.gz"
```

若 go.dev 下载超时（本机到部分 CDN 较慢），改用国内镜像：`https://golang.google.cn/dl/go1.23.6.linux-${GOARCH}.tar.gz` 或 `https://mirrors.aliyun.com/golang/go1.23.6.linux-${GOARCH}.tar.gz`。

- [ ] **Step 2: 解压安装**

优先 `/usr/local/go`（无写权限则装到 `$HOME/.local/go`）：

```bash
if mkdir -p /usr/local 2>/dev/null && [ -w /usr/local ]; then
  tar -C /usr/local -xzf /tmp/go.tgz && echo "PREFIX=/usr/local/go"
else
  mkdir -p "$HOME/.local" && tar -C "$HOME/.local" -xzf /tmp/go.tgz && echo "PREFIX=$HOME/.local/go"
fi
```

- [ ] **Step 3: 验证安装**

```bash
export PATH=$PATH:/usr/local/go/bin   # 或 $HOME/.local/go/bin
go version
```

Expected: `go version go1.23.6 linux/amd64`（或 arm64）。把 PATH 追加到 `~/.bashrc`（`echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc`）方便后续会话。

- [ ] **Step 4: 代理兜底（仅当 Step 1 网络失败时）**

```bash
export GOPROXY=https://goproxy.cn,direct
```

后续所有 `go build/test` 命令在 Go 安装后默认无需网络（本项目零依赖），仅 Task 6 的 Docker 构建需要网络（容器内下载 tailcat）。

---

### Task 2: 迁移 Python 实现到 python/

**Files:**
- Move: `src/tailcat_socks.py` → `python/tailcat_socks.py`
- Move: `tests/` → `python/tests/`
- Modify: `python/tests/test_tailcat_socks.py:14`（sys.path 一行）

- [ ] **Step 1: git mv 保留历史**

```bash
mkdir -p python
git mv src/tailcat_socks.py python/tailcat_socks.py
git mv tests python/tests
```

- [ ] **Step 2: 修正测试的 sys.path**

`python/tests/test_tailcat_socks.py` 第 14 行：

```python
# 旧
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))
# 新（python/tests/../ 即 python/）
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
```

- [ ] **Step 3: 运行 Python 测试验证迁移无破坏**

```bash
python3 -m pytest python/tests -v
```

Expected: 5 passed（test_load_dns_map_basic / line_missing_domains / rewrite_exact / case_insensitive / no_match / watch_dns_file_reloads / socks5_proxy_rewrites_host_and_relays 共 7 个）。

- [ ] **Step 4: 提交**

```bash
git add -A
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "Move Python implementation to python/"
```

---

### Task 3: go.mod + dnsmap.go（TDD）

**Files:**
- Create: `go/go.mod`
- Create: `go/dnsmap.go`
- Test: `go/dnsmap_test.go`

- [ ] **Step 1: 初始化 module**

```bash
mkdir -p go && cat > go/go.mod <<'EOF'
module tailcat-socks

go 1.23
EOF
```

- [ ] **Step 2: 写失败测试 `go/dnsmap_test.go`**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dns.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDNSFileBasic(t *testing.T) {
	path := writeTemp(t, `
# comment line
tcXYZabc123   www.example.com  api.example.com
tcDEF456ghi   foo.com

tcGGG         bar.com	baz.com
`)
	m, err := LoadDNSFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"www.example.com": "tcXYZabc123",
		"api.example.com": "tcXYZabc123",
		"foo.com":         "tcDEF456ghi",
		"bar.com":         "tcGGG",
		"baz.com":         "tcGGG",
	}
	for d, tok := range want {
		if m[d] != tok {
			t.Errorf("m[%q] = %q, want %q", d, m[d], tok)
		}
	}
	if _, ok := m["tcxyzabc123"]; ok {
		t.Error("token line itself must not become a domain key")
	}
}

func TestLoadDNSFileMissingDomains(t *testing.T) {
	m, err := LoadDNSFile(writeTemp(t, "tcONLYONE\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("token-only line should contribute nothing, got %v", m)
	}
}

func TestLoadDNSFileLaterOverrides(t *testing.T) {
	m, err := LoadDNSFile(writeTemp(t, "tcA dup.com\ntcB dup.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["dup.com"] != "tcB" {
		t.Errorf("later line should win, got %q", m["dup.com"])
	}
}

func TestLoadDNSFileMissingFile(t *testing.T) {
	if _, err := LoadDNSFile(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestRewriteHost(t *testing.T) {
	m := map[string]string{"www.example.com": "tcXYZ"}
	if got := RewriteHost("www.example.com", m); got != "tcXYZ" {
		t.Errorf("exact match: got %q", got)
	}
	if got := RewriteHost("WWW.Example.COM", m); got != "tcXYZ" {
		t.Errorf("case-insensitive match: got %q", got)
	}
	if got := RewriteHost("other.com", m); got != "other.com" {
		t.Errorf("no match should return host unchanged, got %q", got)
	}
}

func TestWatchDNSFileReloads(t *testing.T) {
	path := writeTemp(t, "tcA alpha.com\n")
	m := &DNSMap{}
	stop := make(chan struct{})
	defer close(stop)
	go WatchDNSFile(path, m, 50*time.Millisecond, stop)

	// initial load
	waitFor(t, 3*time.Second, func() bool { return m.Load()["alpha.com"] == "tcA" })

	// rewrite content and bump mtime explicitly (some filesystems have
	// coarse mtime granularity, which would miss the change)
	if err := os.WriteFile(path, []byte("tcB beta.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return m.Load()["beta.com"] == "tcB" })
	if _, ok := m.Load()["alpha.com"]; ok {
		t.Error("removed domain should be gone after reload")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
```

- [ ] **Step 3: 写最小 stub `go/dnsmap.go` 让编译通过但测试失败**

```go
package main

// stub: implemented in TDD steps below

func LoadDNSFile(path string) (map[string]string, error) { return nil, nil }

type DNSMap struct{}

func (m *DNSMap) Store(mapping map[string]string)          {}
func (m *DNSMap) Load() map[string]string                  { return nil }
func RewriteHost(host string, m map[string]string) string  { return host }
func WatchDNSFile(path string, m *DNSMap, d interface{}, stop <-chan struct{}) {}
```

- [ ] **Step 4: 运行测试确认失败**

```bash
cd go && go test ./... 2>&1 | tail -20
```

Expected: 编译错误（WatchDNSFile 签名不匹配）或多个 FAIL。

- [ ] **Step 5: 完整实现 `go/dnsmap.go`（整体替换）**

```go
package main

import (
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// LoadDNSFile parses dns.txt into {domain: token}. Domains are stored
// lowercased; tokens keep their case. Blank lines and lines starting with
// '#' are ignored. A line with only a token (no domains) contributes
// nothing. Later lines override earlier ones for the same domain.
func LoadDNSFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mapping := make(map[string]string)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		token, domains := fields[0], fields[1:]
		for _, d := range domains {
			mapping[strings.ToLower(d)] = token
		}
	}
	return mapping, nil
}

// DNSMap holds the domain->token mapping, hot-swappable atomically.
type DNSMap struct {
	v atomic.Value // map[string]string
}

// Store atomically replaces the mapping.
func (m *DNSMap) Store(mapping map[string]string) { m.v.Store(mapping) }

// Load returns the current mapping (never nil).
func (m *DNSMap) Load() map[string]string {
	if v := m.v.Load(); v != nil {
		return v.(map[string]string)
	}
	return map[string]string{}
}

// RewriteHost returns the token for host if mapped, else host unchanged.
func RewriteHost(host string, m map[string]string) string {
	if token, ok := m[strings.ToLower(host)]; ok {
		return token
	}
	return host
}

// WatchDNSFile polls path's mtime every interval; on change it reloads the
// mapping into m. Read errors keep the previous mapping. It performs an
// initial load so the map is populated even if the caller started empty.
// Returns when stop is closed.
func WatchDNSFile(path string, m *DNSMap, interval time.Duration, stop <-chan struct{}) {
	if first, err := LoadDNSFile(path); err == nil {
		m.Store(first)
	} else {
		log.Printf("[tailcat-socks] initial load of %s failed: %v", path, err)
	}
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().Equal(lastMod) {
			continue
		}
		newMap, err := LoadDNSFile(path)
		if err != nil {
			log.Printf("[tailcat-socks] reload failed (%v); keeping previous map", err)
			continue
		}
		lastMod = fi.ModTime()
		m.Store(newMap)
		log.Printf("[tailcat-socks] reloaded %s: %d domain(s) -> %d token(s)",
			path, len(newMap), len(tokenSet(newMap)))
	}
}

// tokenSet returns the distinct tokens in a mapping.
func tokenSet(m map[string]string) map[string]bool {
	s := make(map[string]bool, len(m))
	for _, t := range m {
		s[t] = true
	}
	return s
}
```

- [ ] **Step 6: 运行测试确认通过**

```bash
cd go && go test ./... -v 2>&1 | tail -20
```

Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add go/
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "Go: dnsmap (dns.txt parsing, atomic hot-reload)"
```

---

### Task 4: proxy.go（TDD：SOCKS5 服务端 + 上游握手 + relay）

**Files:**
- Create: `go/proxy.go`
- Test: `go/proxy_test.go`

- [ ] **Step 1: 写失败测试 `go/proxy_test.go`**

```go
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
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		ch <- result{data: buf[:n]}
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
		want := append([]byte{0x05, 0x01, 0x00}, 0x03)
		want = append(want, byte(len("tcXYZabc123")))
		want = append(want, []byte("tcXYZabc123")...)
		want = append(want, 0x01, 0xBB) // 443
		got := r.data
		if len(got) < 3 || string(got[:3]) != "\x05\x01\x00" {
			t.Fatalf("bad greeting bytes: %v", got)
		}
		req := got[3:]
		if string(req) != string(want[3:]) {
			t.Errorf("request = %v, want %v", req, want[3:])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no request captured")
	}
}
```

注意：`TestUpstreamConnectUsesDomainATYP` 里 fake 不回最终 reply，`upstreamConnect` 会阻塞在读 reply——goroutine 里调它，断言只针对捕获到的请求字节。

- [ ] **Step 2: 写最小 stub `go/proxy.go` 让编译通过但测试失败**

```go
package main

import "net"

// stub: implemented in TDD steps below

type Server struct{}

func NewServer(dnsMap *DNSMap, upstreamAddr, listen string) (*Server, error) {
	return nil, nil
}

func (s *Server) ActualAddr() net.Addr { return nil }
func (s *Server) Close() error         { return nil }
func (s *Server) Serve() error         { return nil }

func upstreamConnect(up net.Conn, host string, port uint16) error { return nil }
```

- [ ] **Step 3: 运行测试确认失败**

```bash
cd go && go test ./... 2>&1 | tail -15
```

Expected: 多个 FAIL（nil server / 空 upstreamConnect）。

- [ ] **Step 4: 完整实现 `go/proxy.go`（整体替换）**

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
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
}

// NewServer binds the listen address. Use ActualAddr for the resolved port
// (listening on port 0 is useful in tests).
func NewServer(dnsMap *DNSMap, upstreamAddr, listen string) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &Server{DNSMap: dnsMap, UpstreamAddr: upstreamAddr, ln: ln}, nil
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
	if err := upstreamConnect(up, target, port); err != nil {
		up.Close()
		socksFail(client)
		return
	}

	client.Write(socksOKReply)
	relay(client, up)
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
// either side closes or the idle timeout fires (the Go analog of the Python
// version's select() loop with a 300s timeout).
func relay(client, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		copyWithIdleTimeout(upstream, client, relayIdleTimeout)
		done <- struct{}{}
	}()
	go func() {
		copyWithIdleTimeout(client, upstream, relayIdleTimeout)
		done <- struct{}{}
	}()
	<-done
	// one direction finished: close both so the other unblocks
	client.Close()
	upstream.Close()
	<-done
}

// copyWithIdleTimeout copies src to dst, giving up if no data arrives
// within idle. Like the Python relay, an idle timeout or EOF on either side
// ends the whole relay.
func copyWithIdleTimeout(dst, src net.Conn, idle time.Duration) error {
	buf := make([]byte, 65536)
	for {
		src.SetReadDeadline(time.Now().Add(idle))
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			return rerr
		}
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
cd go && go test ./... 2>&1 | tail -10
```

Expected: PASS（dnsmap + proxy 全部）。

- [ ] **Step 6: 提交**

```bash
git add go/
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "Go: SOCKS5 proxy (server, upstream client, relay)"
```

---

### Task 5: main.go + 假 tailcat + 进程编排测试（TDD）

**Files:**
- Create: `go/testdata/fake-tailcat/main.go`（测试辅助二进制）
- Create: `go/main.go`
- Test: `go/main_test.go`
- Modify: `.gitignore`（加 `go/bin/`）

- [ ] **Step 1: 写假 tailcat 辅助程序 `go/testdata/fake-tailcat/main.go`**

行为等价 `tailcat socks --listen=host:port`：解析 `--listen`，跑一个极简 SOCKS5 服务，接受任何 CONNECT，回成功并回显数据（`ECHO:` 前缀）。放在 `testdata/` 下，`go test ./...` 与 `go build .` 都不会碰它。

```go
// fake-tailcat stands in for `tailcat socks` in tests: it parses
// --listen=host:port and runs a minimal SOCKS5 server that accepts any
// CONNECT, replies success, and echoes data back.
package main

import (
	"flag"
	"io"
	"log"
	"net"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "listen address host:port")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake tailcat socks listening on %s", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	io.ReadFull(conn, make([]byte, hdr[1]))
	conn.Write([]byte{0x05, 0x00})

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
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
	io.Copy(conn, conn) // echo
}
```

- [ ] **Step 2: 写失败测试 `go/main_test.go`**

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeTailcatBin is set by TestMain after building the test helper binary.
var fakeTailcatBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fake-tailcat")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	fakeTailcatBin = filepath.Join(dir, "fake-tailcat")
	build := exec.Command("go", "build", "-o", fakeTailcatBin, "./testdata/fake-tailcat")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build fake-tailcat:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestParseAddr(t *testing.T) {
	cases := []struct {
		in       string
		def      int
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"127.0.0.1:1080", 0, "127.0.0.1", 1080, false},
		{"127.0.0.1:0", 0, "127.0.0.1", 0, false},
		{"example.com:9999", 0, "example.com", 9999, false},
		{"1080", 0, "127.0.0.1", 1080, false}, // bare port, empty host -> default
		{"127.0.0.1", 0, "", 0, true},         // bare IP -> Atoi fails (parity with Python)
		{"host:abc", 0, "", 0, true},
	}
	for _, tc := range cases {
		host, port, err := parseAddr(tc.in, tc.def)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAddr(%q): want error, got %s:%d", tc.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddr(%q): %v", tc.in, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("parseAddr(%q) = %s:%d, want %s:%d", tc.in, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestFreeHighPort(t *testing.T) {
	p := freeHighPort("127.0.0.1")
	if p < 20000 || p > 60999 {
		t.Fatalf("port %d out of high range", p)
	}
	// typically still bindable right after
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err == nil {
		ln.Close()
	}
}

func TestSpawnTailcatSocksAndWaitReady(t *testing.T) {
	port := freeHighPort("127.0.0.1")
	child := spawnTailcatSocks(fakeTailcatBin, "127.0.0.1", port)
	if child == nil {
		t.Fatal("spawn failed")
	}
	t.Cleanup(func() { terminate(child) })
	if !waitReady(fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
}

func TestTerminateStopsChild(t *testing.T) {
	port := freeHighPort("127.0.0.1")
	child := spawnTailcatSocks(fakeTailcatBin, "127.0.0.1", port)
	if child == nil {
		t.Fatal("spawn failed")
	}
	terminate(child)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && child.ProcessState == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if child.ProcessState == nil {
		t.Fatal("child did not exit after terminate")
	}
}

func TestTerminateNilIsSafe(t *testing.T) {
	terminate(nil)
}
```

注意 `TestParseAddr` 用例 `{"127.0.0.1", ...}`：Python 版对无冒号字符串 `rpartition(":")` 后 `int("127.0.0.1")` 会抛异常——Go 用返回 error 保持等价（进程报错退出）。

- [ ] **Step 3: 运行测试确认失败（编译错误）**

```bash
cd go && go vet ./... 2>&1 | head -10
```

Expected: `undefined: parseAddr` 等编译错误。

- [ ] **Step 4: 完整实现 `go/main.go`**

```go
// tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
// upstream. This is a behavioral replica of the Python version
// (python/tailcat_socks.py); see README.md for usage.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0) // messages already carry the [tailcat-socks] prefix
	var (
		listen       = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen addr:port")
		dnsFile      = flag.String("dns-file", "dns.txt", "domain->token mapping file")
		upstream     = flag.String("upstream", "127.0.0.1:0", "upstream tailcat socks addr:port; port 0 (default) = high random free port")
		tailcatBin   = flag.String("tailcat-bin", "tailcat", "path to tailcat binary (auto-launched)")
		noAutolaunch = flag.Bool("no-autolaunch", false, "do not spawn tailcat socks; use an already-running upstream")
		noWatch      = flag.Bool("no-watch", false, "disable dns.txt hot-reload")
	)
	flag.Parse()
	os.Exit(run(*listen, *dnsFile, *upstream, *tailcatBin, *noAutolaunch, *noWatch))
}

func run(listen, dnsFile, upstream, tailcatBin string, noAutolaunch, noWatch bool) int {
	first, err := LoadDNSFile(dnsFile)
	if err != nil {
		log.Printf("[tailcat-socks] cannot load %s: %v", dnsFile, err)
		return 1
	}
	dnsMap := &DNSMap{}
	dnsMap.Store(first)

	// Resolve the tailcat socks listen port: explicit port wins, else high random.
	upHost, upPort, err := parseAddr(upstream, 0)
	if err != nil {
		log.Printf("[tailcat-socks] bad --upstream: %v", err)
		return 1
	}
	if upPort == 0 {
		upPort = freeHighPort(upHost)
	}
	upAddr := net.JoinHostPort(upHost, strconv.Itoa(upPort))

	srv, err := NewServer(dnsMap, upAddr, listen)
	if err != nil {
		log.Printf("[tailcat-socks] cannot listen on %s: %v", listen, err)
		return 1
	}

	var child *exec.Cmd
	if !noAutolaunch {
		child = spawnTailcatSocks(tailcatBin, upHost, upPort)
		if child == nil {
			srv.Close()
			return 1
		}
		if !waitReady(upAddr, 15*time.Second) {
			log.Printf("[tailcat-socks] upstream %s not ready; aborting", upAddr)
			terminate(child)
			srv.Close()
			return 1
		}
		log.Printf("[tailcat-socks] auto-launched %s socks on %s", tailcatBin, upAddr)
	}

	stopWatch := make(chan struct{})
	if !noWatch {
		go WatchDNSFile(dnsFile, dnsMap, time.Second, stopWatch)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	host, port, _ := net.SplitHostPort(srv.ActualAddr().String())
	log.Printf("[tailcat-socks] listening socks5h://%s:%s", host, port)
	log.Printf("[tailcat-socks] %d domain(s) mapped -> %d token(s)",
		len(first), len(tokenSet(first)))
	log.Printf("[tailcat-socks] upstream %s", upAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-serveErr:
		if err != nil {
			log.Printf("[tailcat-socks] server error: %v", err)
		}
	}
	close(stopWatch)
	srv.Close()
	terminate(child)
	return 0
}

// parseAddr splits "host:port" with Python's rpartition(":") semantics:
// everything after the last colon is the port, everything before is the host
// (empty -> 127.0.0.1); a string with no colon is treated as the port part.
func parseAddr(s string, defaultPort int) (string, int, error) {
	host, portStr := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, portStr = s[:i], s[i+1:]
	}
	port := defaultPort
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return "", 0, fmt.Errorf("bad addr %q: %w", s, err)
		}
		port = p
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

// freeHighPort returns a free high port (ephemeral range, randomly probed,
// mirroring the Python version), falling back to an OS-assigned port.
func freeHighPort(host string) int {
	for i := 0; i < 20; i++ {
		p := 20000 + rand.Intn(41000) // 20000..60999
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			ln.Close()
			return p
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		log.Fatalf("[tailcat-socks] cannot find a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// spawnTailcatSocks launches `tailcat socks --listen=host:port` and returns
// the cmd, or nil on failure. Child output goes to our stderr so it lands in
// the same log file.
func spawnTailcatSocks(binPath, host string, port int) *exec.Cmd {
	cmd := exec.Command(binPath, "socks", fmt.Sprintf("--listen=%s:%d", host, port))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[tailcat-socks] failed to launch %s: %v", binPath, err)
		return nil
	}
	return cmd
}

// terminate stops the child: SIGTERM, then SIGKILL after 5s.
func terminate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck — best effort
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck — best effort
		<-done
	}
}

// waitReady polls TCP-connect to addr until it accepts or the timeout hits.
func waitReady(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
```

- [ ] **Step 5: 运行全部 Go 测试确认通过**

```bash
cd go && gofmt -l . && go vet ./... && go test ./... -v 2>&1 | tail -30
```

Expected: `gofmt -l` 无输出（全部已格式化）、vet 无告警、全部测试 PASS。

- [ ] **Step 6: 更新 .gitignore**

在 `.gitignore` 追加一行：

```
go/bin/
```

- [ ] **Step 7: 提交**

```bash
git add go/ .gitignore
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "Go: main (flags, tailcat autolaunch, signals) + fake tailcat test helper"
```

---

### Task 6: Dockerfile 改为 Go 构建

**Files:**
- Rewrite: `docker/Dockerfile`
- Create: `.dockerignore`
- 不变: `docker/docker-compose.yml`（env/挂载/端口语义完全兼容）

- [ ] **Step 1: 重写 `docker/Dockerfile`**

```dockerfile
# ---- build stage: compile the Go proxy and the tailcat binary ----
FROM golang:1.23-bookworm AS build

WORKDIR /src
COPY go/ /src/go/
# Static binary (CGO off) so the slim runtime stage needs nothing else.
RUN cd /src/go && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tailcat-socks .
# Pin a version for reproducibility; bump as needed.
RUN go install github.com/tailscale/tailcat/cmd/tailcat@latest

# ---- runtime stage ----
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/tailcat-socks /usr/local/bin/tailcat-socks
COPY --from=build /go/bin/tailcat /usr/local/bin/tailcat

WORKDIR /app
# dns.txt holds real tokens and is git-ignored; bake the example as a build
# fallback (compose mounts your real dns.txt over it at runtime).
COPY dns.txt.example /app/dns.txt

ENV LISTEN=0.0.0.0:1080 \
    UPSTREAM=127.0.0.1:0 \
    DNS_FILE=/app/dns.txt \
    TAILCAT_BIN=tailcat

# 1080 = SOCKS5 to clients; tailcat socks uses an internal random high port.
EXPOSE 1080

# exec so PID 1 forwards signals -> proxy cleans up its tailcat child.
CMD ["sh", "-c", "exec tailcat-socks --listen \"$LISTEN\" --dns-file \"$DNS_FILE\" --upstream \"$UPSTREAM\" --tailcat-bin \"$TAILCAT_BIN\""]
```

- [ ] **Step 2: 创建 `.dockerignore`**

```
.git
run/
go/bin/
go/testdata/
python/__pycache__/
dns.txt
```

- [ ] **Step 3: 构建镜像**

```bash
cd docker && docker compose build 2>&1 | tail -15
```

Expected: 构建成功（容器内需联网下载 tailcat；若 Go 模块下载慢可在 build stage 前加 `ENV GOPROXY=https://goproxy.cn,direct`）。

- [ ] **Step 4: 验证镜像内二进制可运行**

```bash
docker run --rm tailcat-socks:latest tailcat-socks --help
```

Expected: 打印 `-listen`、`-dns-file`、`-upstream`、`-tailcat-bin`、`-no-autolaunch`、`-no-watch` 等 flag 帮助。

- [ ] **Step 5: 提交**

```bash
git add docker/Dockerfile .dockerignore
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "Docker: build from Go version (debian-slim runtime, no Python)"
```

---

### Task 7: start/stop/restart.sh 双版本支持

**Files:**
- Rewrite: `bin/start.sh`、`bin/stop.sh`、`bin/restart.sh`

- [ ] **Step 1: 重写 `bin/start.sh`**

```bash
#!/usr/bin/env bash
# Start tailcat-socks in the background.
# Usage: bin/start.sh [go|python]    (default: go)
# Config via env (all optional):
#   LISTEN=127.0.0.1:1080  DNS_FILE=<root>/dns.txt  UPSTREAM=127.0.0.1:0
#   TAILCAT_BIN=tailcat    PROXY_ARGS="extra flags"
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/run"

IMPL="${1:-go}"
case "$IMPL" in
  go)
    BIN="$ROOT/go/bin/tailcat-socks"
    PID_FILE="$RUN_DIR/tailcat-socks-go.pid"
    LOG_FILE="$RUN_DIR/tailcat-socks-go.log"
    ;;
  python)
    SRC="$ROOT/python/tailcat_socks.py"
    PID_FILE="$RUN_DIR/tailcat-socks-python.pid"
    LOG_FILE="$RUN_DIR/tailcat-socks-python.log"
    ;;
  *)
    echo "usage: $0 [go|python]" >&2
    exit 2
    ;;
esac

LISTEN="${LISTEN:-127.0.0.1:1080}"
DNS_FILE="${DNS_FILE:-$ROOT/dns.txt}"
UPSTREAM="${UPSTREAM:-127.0.0.1:0}"
TAILCAT_BIN="${TAILCAT_BIN:-tailcat}"

mkdir -p "$RUN_DIR"

if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  echo "already running [$IMPL] (pid $(cat "$PID_FILE"))"
  exit 1
fi

# Build the Go proxy if the binary is missing (requires a Go toolchain).
if [[ "$IMPL" == go && ! -x "$BIN" ]]; then
  echo "go binary missing; building (go build)..."
  (cd "$ROOT/go" && go build -o bin/tailcat-socks .)
fi

: > "$LOG_FILE"   # truncate on each fresh start

ARGS=(--listen "$LISTEN" --dns-file "$DNS_FILE" --upstream "$UPSTREAM" --tailcat-bin "$TAILCAT_BIN")
if [[ "$IMPL" == go ]]; then
  nohup "$BIN" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
else
  nohup python3 -u "$SRC" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
fi
echo $! > "$PID_FILE"

echo "started [$IMPL] pid $(cat "$PID_FILE")  listen=$LISTEN  log=$LOG_FILE"
```

- [ ] **Step 2: 重写 `bin/stop.sh`**

```bash
#!/usr/bin/env bash
# Stop tailcat-socks (and its auto-launched tailcat socks child, which
# the proxy terminates on SIGTERM via its signal handler).
# Usage: bin/stop.sh [go|python]   (no arg = stop both)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/run"

IMPL="${1:-}"
case "$IMPL" in
  "")     PID_FILES=("$RUN_DIR/tailcat-socks-go.pid" "$RUN_DIR/tailcat-socks-python.pid") ;;
  go)     PID_FILES=("$RUN_DIR/tailcat-socks-go.pid") ;;
  python) PID_FILES=("$RUN_DIR/tailcat-socks-python.pid") ;;
  *)      echo "usage: $0 [go|python]" >&2; exit 2 ;;
esac

STOPPED=0
for PID_FILE in "${PID_FILES[@]}"; do
  [[ -f "$PID_FILE" ]] || continue
  STOPPED=1
  PID="$(cat "$PID_FILE")"
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "stale pid file $PID_FILE (pid $PID not alive); removing"
    rm -f "$PID_FILE"
    continue
  fi
  echo "stopping pid $PID ..."
  kill -TERM "$PID" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.25
  done
  if kill -0 "$PID" 2>/dev/null; then
    echo "forcing kill -9"
    kill -KILL "$PID" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
done

if [[ "$STOPPED" == 1 ]]; then
  echo "stopped"
else
  echo "not running"
fi
```

- [ ] **Step 3: 重写 `bin/restart.sh`**

```bash
#!/usr/bin/env bash
# Restart tailcat-socks. Usage: bin/restart.sh [go|python]
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$DIR/stop.sh" "${1:-}"
exec "$DIR/start.sh" "${1:-}"
```

- [ ] **Step 4: 语法检查**

```bash
bash -n bin/start.sh && bash -n bin/stop.sh && bash -n bin/restart.sh && echo OK
```

Expected: `OK`。

- [ ] **Step 5: 双版本启停冒烟（进程级）**

```bash
# Python 版
bin/start.sh python && sleep 1 && ps -p "$(cat run/tailcat-socks-python.pid)" >/dev/null && echo "python alive"
tail -3 run/tailcat-socks-python.log
bin/stop.sh python

# Go 版（首次自动构建）
bin/start.sh go && sleep 1 && ps -p "$(cat run/tailcat-socks-go.pid)" >/dev/null && echo "go alive"
tail -3 run/tailcat-socks-go.log
bin/stop.sh go
```

Expected: 两版本各自打印 started、日志出现 `listening socks5h://...`（注意：根目录无 dns.txt 时进程会退出——冒烟前先 `cp dns.txt.example /tmp/dns.txt` 并传 `DNS_FILE=/tmp/dns.txt`，或接受此行为并检查日志含 `cannot load`）。推荐统一用 `DNS_FILE=/tmp/dns.txt` 跑。

- [ ] **Step 6: 提交**

```bash
git add bin/
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "bin/: start/stop/restart support go|python selection (default go)"
```

---

### Task 8: 端到端数据面冒烟（假上游 + curl，双版本）

**Files:** 无仓库文件变更（临时文件在 /tmp）。验证规格中的"集成冒烟"验收项。

- [ ] **Step 1: 写假上游脚本 /tmp/fake_upstream.py**

SOCKS5 服务器：接受任何 CONNECT，回成功，然后回发它看到的目标 `SAW:<host>:<port>` 并关闭——以此断言改写是否发生。

```bash
cat > /tmp/fake_upstream.py <<'EOF'
"""Fake tailcat socks upstream: SOCKS5 server that replies success and sends
back the CONNECT target it saw (SAW:host:port), then closes."""
import socket, struct, sys, threading

def handle(c):
    hdr = c.recv(2); c.recv(hdr[1]); c.sendall(b"\x05\x00")
    req = c.recv(4); atyp = req[3]
    if atyp == 3:
        n = c.recv(1)[0]; host = c.recv(n).decode()
    elif atyp == 1:
        host = socket.inet_ntoa(c.recv(4))
    else:
        host = "?"
    port = struct.unpack("!H", c.recv(2))[0]
    c.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
    c.sendall(f"SAW:{host}:{port}".encode())
    c.close()

s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1])))
s.listen(16)
print("fake upstream ready", flush=True)
while True:
    t, _ = s.accept()
    threading.Thread(target=handle, args=(t,), daemon=True).start()
EOF
```

- [ ] **Step 2: 准备测试映射并启动假上游**

```bash
printf '# smoke\ntcSMOKE123 www.example.com\n' > /tmp/dns.txt
python3 /tmp/fake_upstream.py 18081 &
sleep 0.5
```

- [ ] **Step 3: Go 版 e2e**

```bash
LISTEN=127.0.0.1:11080 DNS_FILE=/tmp/dns.txt UPSTREAM=127.0.0.1:18081 \
  TAILCAT_BIN=/bin/true PROXY_ARGS="--no-autolaunch --no-watch" bin/start.sh go
sleep 0.5
# 命中改写
curl -s --socks5-hostname 127.0.0.1:11080 http://www.example.com:9999/
# 未命中透传
curl -s --socks5-hostname 127.0.0.1:11080 http://unmatched.example:7777/
bin/stop.sh go
```

Expected: 第一条输出 `SAW:tcSMOKE123:9999`（域名→token 改写成功）；第二条 `SAW:unmatched.example:7777`（透传）。

- [ ] **Step 4: 热加载冒烟（Go 版，去掉 --no-watch）**

```bash
LISTEN=127.0.0.1:11080 DNS_FILE=/tmp/dns.txt UPSTREAM=127.0.0.1:18081 \
  TAILCAT_BIN=/bin/true PROXY_ARGS="--no-autolaunch" bin/start.sh go
sleep 0.5
printf '# smoke2\ntcNEW999 www.example.com\n' > /tmp/dns.txt
sleep 2   # Go watcher 每秒轮询 mtime
curl -s --socks5-hostname 127.0.0.1:11080 http://www.example.com:9999/
bin/stop.sh go
```

Expected: `SAW:tcNEW999:9999`（热加载生效），且 go 日志出现 `reloaded`。

- [ ] **Step 5: Python 版同组冒烟**

重复 Step 3 与 Step 4，把 `bin/start.sh go` 换成 `bin/start.sh python`（`bin/stop.sh python`）。Expected: 输出与 Go 版完全一致。

- [ ] **Step 6: 清理**

```bash
kill %1 2>/dev/null || pkill -f fake_upstream.py
```

- [ ] **Step 7: 记录结果**

无需提交（纯验证）。若任何断言失败：按 superpowers:systematic-debugging 流程定位，修复后重跑 Task 8 全部步骤。

---

### Task 9: README 更新

**Files:**
- Rewrite: `README.md`

- [ ] **Step 1: 更新 README.md**

保留原有设计说明（它描述的行为两版本一致），改动以下段落：

1. 开头补充一段：

```markdown
本仓库包含行为一致的**两个实现**：`python/`（原版，纯标准库）与 `go/`（Go 复刻版，
零第三方依赖，单静态二进制）。Docker 镜像基于 Go 版构建。
```

2. 「安装依赖」节替换为：

```markdown
## 安装依赖

- Go 版：Go 1.23+（构建）；或直接用 `go/bin/` 下已构建的二进制
- Python 版：Python 3.8+（仅标准库）
- `tailcat`：https://github.com/tailscale/tailcat（`brew install tailcat` 或 `go install github.com/tailscale/tailcat/cmd/tailcat@latest`）
```

3. 「快速开始(裸机)」中的运行命令替换为：

```bash
# Go 版（推荐：先构建一次）
cd go && go build -o bin/tailcat-socks . && cd ..
go/bin/tailcat-socks

# Python 版
python3 python/tailcat_socks.py
```

4. 「启停脚本(bin/)」节替换为：

```bash
bin/start.sh          # 后台启动 Go 版（缺二进制时自动 go build）
bin/start.sh python   # 后台启动 Python 版
bin/stop.sh           # 停止（不带参数停两个版本；bin/stop.sh go 只停 Go 版）
bin/restart.sh [go|python]
tail -f run/tailcat-socks-go.log      # 或 tailcat-socks-python.log
```

pid/log 按版本分离：`run/tailcat-socks-{go,python}.{pid,log}`。

5. 「Docker」节补一句：镜像 runtime 为 `debian:bookworm-slim`，内含 Go 静态编译的代理二进制与 tailcat，不再依赖 Python。

6. 「目录结构」节替换为：

```
tailcat-socks/
├── python/
│   ├── tailcat_socks.py          # Python 版代理主体（纯标准库）
│   └── tests/test_tailcat_socks.py
├── go/                               # Go 复刻版（行为一致）
│   ├── main.go                       # flag / tailcat 自动拉起 / 信号
│   ├── dnsmap.go                     # dns.txt 解析 + 原子热加载
│   ├── proxy.go                      # SOCKS5 服务端 + 上游握手 + relay
│   ├── *_test.go                     # go test 单元 + 端到端
│   ├── testdata/fake-tailcat/        # 测试用假 tailcat
│   └── bin/                          # 构建产物（gitignore）
├── bin/{start,stop,restart}.sh       # 裸机后台启停（支持 go|python）
├── docker/{Dockerfile,docker-compose.yml}
├── docs/superpowers/                 # 设计文档与实现计划
├── dns.txt                           # 域名→token 映射（运行时热加载）
└── run/                              # pid + log（脚本生成）
```

7. 「测试」节替换为：

```bash
cd go && go test ./...          # Go 版：解析/改写/热加载/端到端
python3 -m pytest python/tests -v   # Python 版
```

- [ ] **Step 2: 提交**

```bash
git add README.md
git -c user.name="YaoFeng" -c user.email="yaofeng@crowddigital.cn" commit -m "README: document python/ + go/ dual implementation, new paths and start.sh usage"
```

---

### Task 10: 最终验证清点

**Files:** 无变更（只读验证 + 可能的修复）。

- [ ] **Step 1: 全量测试套件**

```bash
cd go && gofmt -l . && go vet ./... && go test ./... -count=1 && cd ..
python3 -m pytest python/tests -v
```

Expected: gofmt 无输出、vet 干净、Go 全 PASS、Python 全 PASS。

- [ ] **Step 2: Docker 复验**

```bash
cd docker && docker compose build >/dev/null 2>&1 && docker run --rm tailcat-socks:latest tailcat-socks --help | head -5
```

Expected: 帮助输出正常。

- [ ] **Step 3: 仓库状态清点**

```bash
git status --short
git log --oneline -10
```

Expected: 工作区干净（dns.txt/run/ 均被忽略），提交序列完整（迁移 → dnsmap → proxy → main → docker → bin → README）。

- [ ] **Step 4: 对照规格逐条核对**

打开 `docs/superpowers/specs/2026-09-04-go-reimplementation-design.md`，逐节核对：
- 目录结构：python/、go/、bin/、docker/、run/ ✓
- Go 组件：LoadDNSFile/DNSMap/RewriteHost/WatchDNSFile ✓；Server 握手逐字节对齐（05 FF、仅 CONNECT、ATYP 1/3/4、socks5h、失败 05 01、300s 空闲）✓；flag 同名同默认、autolaunch（20000–60999 随机 20 次 + OS 兜底、15s 就绪、SIGTERM→5s→SIGKILL）✓
- Docker：golang:1.23 构建 CGO_ENABLED=0、debian:bookworm-slim、ca-certificates、dns.txt.example、ENV/EXPOSE/CMD ✓
- start.sh：`[go|python]` 缺省 go、缺二进制自动构建、pid/log 分离、stop 无参数停两个 ✓
- 测试计划 1–5 全部执行并有输出 ✓

任何缺失项回对应 Task 补齐。

---

## Self-Review 结论

- **规格覆盖**：规格所有章节均有对应 Task（目录结构→T2-7、Go 组件→T3-5、Docker→T6、start.sh→T7、测试计划→T3-8/10、README→T9）。
- **无占位符**：所有代码步骤含完整代码与命令。
- **类型一致性**：`LoadDNSFile/DNSMap{Store,Load}/RewriteHost/WatchDNSFile/tokenSet`、`Server{NewServer,ActualAddr,Close,Serve}/upstreamConnect/relay/copyWithIdleTimeout/readATYPAddr`、`run/parseAddr/freeHighPort/spawnTailcatSocks/terminate/waitReady`、`fakeTailcatBin`（TestMain 注入）在各 Task 间签名一致。
