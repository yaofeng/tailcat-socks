# Web 管理页 + tailcat 库化 + 未命中直连 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 `docs/superpowers/specs/2026-11-13-web-admin-and-tailcat-library-design.md`：内嵌 Web 管理页（当前连接 + DNS 配置编辑）、tailcat 从子进程改为进程内 Go 库、未命中 dns.txt 的目标直连。

**Architecture:** SOCKS5 前置代理解析 CONNECT 后经 `route()` 分流：命中 dns.txt 的域名交给进程内缓存的 `tailcat.Client` 打洞，其余走 `net.Dialer` 直连。relay 的 `idleConn` 顺带原子计数字节，汇入 `ConnRegistry`；admin HTTP 服务从 registry/DNSMap 出 JSON API，并 `go:embed` 单页前端（1s 轮询）。dns.txt 升级为带备注（token 行上方独立 `#` 行）的条目格式，页面保存即原子落盘并立即热加载。

**Tech Stack:** Go 1.27.1（tailcat v0.7.0 的 go.mod 要求 go ≥ 1.27.1，主模块 go 指令必须跟着升；本机 `go` 1.23.6 + GOTOOLCHAIN=auto，go1.27.1 工具链已在本地缓存）、`github.com/tailscale/tailcat v0.7.0`、`github.com/fsnotify/fsnotify`、标准库 `net/http` + `go:embed`、vanilla JS。

---

## 环境须知（每个任务都适用）

- 仓库根：`/data/yaofeng/workspace/popeye/tailcat-socks`，Go 模块在 `src/`（module `tailcat-socks`，go 指令现为 1.25.0，Task 2.0 将升到 1.27.1）。本机 `go` 是 1.23.6，但 `GOTOOLCHAIN=auto` 且 go1.25.0/1.26.8/**1.27.1**/1.27.4 工具链均已在本地 module cache（`/data/yaofeng/go/pkg/mod/golang.org/toolchain@…`），命令照常敲 `go …` 即可自动切工具链。
- **proxy.golang.org 本机不通**（用户已批准 goproxy.cn）。所有会触网的 go 命令必须带 `GOPROXY=https://goproxy.cn,direct` 前缀：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go get github.com/tailscale/tailcat@v0.7.0
  ```
  依赖进本地 module cache 后 test/build 不再触网，但保留前缀无害。
- 每次提交前 `cd src && GOPROXY=https://goproxy.cn,direct go test ./...` 必须全绿；提交信息末尾带
  `Co-Authored-By: Claude Code <noreply@anthropic.com>`。
- `data/dns.txt` 含真实 token、已被 git 忽略，任何命令与提交物都不要带上它；测试一律用 `t.TempDir()` 里的临时文件。
- 已核实的 tailcat v0.7.0 API（`/data/yaofeng/go/pkg/mod/github.com/tailscale/tailcat@v0.7.0/tailcat.go`）：
  `NewClient(server Addr) *Client`（1879）；`Client{Server Addr; Key key.NodePrivate; Logf func(format string, args ...any); …}`（1823）；`(*Client).DialTCPPort(ctx, port uint16) (net.Conn, error)`（2205）；`(*Client).Close() error`（2003）；`ParseAddr(Addr) (ConnInfo, error)`（1134）；`type PrivateKey struct { Private key.NodePrivate; Public ConnInfo }`（311，可 JSON——官方 CLI 就是这么持久化客户端 key 的，见 cmd/tailcat/tailcat.go:799-803）；`NewPrivateKey() *PrivateKey`（319）。

## 文件结构总览

| 任务 | 文件 | 动作 | 责任 |
|---|---|---|---|
| 1 | `src/dnsmap.go` | 修改 | 新增 Entry 模型：解析/校验/原子保存带备注的 dns.txt；`reload` 改为返回 error |
| 1 | `src/dnsmap_test.go` | 修改 | entries 往返 + 校验用例 |
| 1 | `data/dns.txt.example` | 修改 | 演示备注行格式 |
| 2 | `src/dialer.go` | 新建 | `route()` 路由决策 + `Dialer` 接口 + `AppDialer`（进程内 tailcat / 直连）+ 客户端 key 持久化 |
| 2 | `src/dialer_test.go` | 新建 | route 纯函数表驱动测试（离线） |
| 3 | `src/stats.go` | 新建 | `ConnRegistry`/`Session`/`SessionSnapshot`/`Totals` |
| 3 | `src/proxy.go` | 修改 | `idleConn` 增加可选字节计数；`halfClose`/`closeWrite` 支持非 TCPConn 的接口回退 |
| 3 | `src/stats_test.go` | 新建 | registry 与 idleConn 计数测试 |
| 4 | `src/proxy.go` | 重写 | serveConn 走 route+Dialer；删除上游 SOCKS 客户端链；relay 传入 Session |
| 4 | `src/main.go` | 重写 | 删除 tailcat 子进程/autolaunch/upstream，改用 AppDialer |
| 4 | `src/proxy_test.go` | 重写 | fakeDialer 注入，删上游握手相关用例 |
| 4 | `src/main_test.go`、`src/testdata/fake-tailcat/` | 删除 | 子进程测试随功能一并删除 |
| 5 | `src/admin.go` | 新建 | admin API：connections / dns 三端点（页面在任务 6） |
| 5 | `src/admin_test.go` | 新建 | httptest 覆盖 API 契约 |
| 6 | `src/static/index.html` | 新建 | 单页管理 UI（两个 Tab），go:embed |
| 7 | `src/main.go` | 修改 | 起 admin 监听、优雅 Shutdown、`resolveSetting` flag>env>默认 |
| 8 | `docker/Dockerfile`、`docker/docker-compose.yml`、`bin/start.sh`、`.gitignore`、`README.md` | 修改 | 部署面与文档对齐库化 + 直连 + 管理页 |

任务顺序即提交顺序；每个任务结束时测试全绿、单独提交。

---

### Task 1: dns.txt 条目化（Entry / 备注 / 校验 / 原子保存）

**Files:**
- Modify: `src/dnsmap.go`
- Modify: `src/dnsmap_test.go`
- Modify: `data/dns.txt.example`

规则回顾（spec §7）：备注 = token 行**紧邻上方**的连续 `#` 行（多行以空格合并为一行）；空行重置 pending note（文件头部说明块与首条 entry 之间必须有空行，否则头部注释会挂到第一条目的备注上）；token 行仍用 `strings.Fields`，行尾 `#` 无特殊语义。

- [ ] **Step 1.1 写失败测试**：在 `src/dnsmap_test.go` 末尾追加：

```go
func TestLoadDNSEntriesGroupsNotes(t *testing.T) {
	path := writeTemp(t, `# 头部说明第一行
# 头部说明第二行

# 生产 web 服务器
# 机房 A
tcXYZabc123 www.example.com api.example.com
tcDEF456ghi foo.com # 行尾井号不是注释
`)
	entries, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	e0 := entries[0]
	if e0.Token != "tcXYZabc123" || strings.Join(e0.Domains, ",") != "www.example.com,api.example.com" {
		t.Errorf("entry0 = %+v", e0)
	}
	if e0.Note != "生产 web 服务器 机房 A" {
		t.Errorf("entry0.Note = %q, want %q", e0.Note, "生产 web 服务器 机房 A")
	}
	e1 := entries[1]
	if e1.Note != "" {
		t.Errorf("entry1.Note = %q, want empty", e1.Note)
	}
	// 行尾 # 只是普通字段（Fields 语义），由校验层在保存时拒绝，而不是解析层。
	// Fields splits the trailing "# 行尾井号不是注释" into two MORE fields, so
	// Domains is [foo.com # 行尾井号不是注释]: the "#" is just a field.
	if len(e1.Domains) != 3 || e1.Domains[1] != "#" {
		t.Errorf("entry1.Domains = %v", e1.Domains)
	}
}

func TestLoadDNSEntriesHeaderWithoutBlankLine(t *testing.T) {
	// 无空行分隔时，头部注释会并入第一条目的 note —— 这正是保存示例文件
	// 时要求空行分隔的原因。
	path := writeTemp(t, "# 就是备注\ntcA a.com\n")
	entries, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Note != "就是备注" {
		t.Errorf("note = %q", entries[0].Note)
	}
}

func TestEntriesToMap(t *testing.T) {
	m := EntriesToMap([]Entry{
		{Token: "tcA", Domains: []string{"A.com"}},
		{Token: "tcB", Domains: []string{"a.com", "b.com"}},
	})
	if m["a.com"] != "tcB" || m["b.com"] != "tcB" {
		t.Errorf("later entry must win, domains must lowercase: %v", m)
	}
}

func TestValidateEntries(t *testing.T) {
	ok := []Entry{{Token: "tcA", Domains: []string{"a.com"}, Note: "备注"}}
	if err := ValidateEntries(ok); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	// spec §7: a missing "tc" prefix is warn-only, never an error.
	if err := ValidateEntries([]Entry{{Token: "XYZabc", Domains: []string{"a.com"}}}); err != nil {
		t.Fatalf("tc-prefix must warn only, got: %v", err)
	}
	bad := [][]Entry{
		{{Token: "", Domains: []string{"a.com"}}},
		{{Token: "#x", Domains: []string{"a.com"}}},
		{{Token: "a b", Domains: []string{"x.com"}}},
		{{Token: "tcA", Domains: nil}},
		{{Token: "tcA", Domains: []string{"a#b"}}},
		{{Token: "tcA", Domains: []string{"a b"}}},
		{{Token: "tcA", Domains: []string{"a.com"}, Note: "两\n行"}},
	}
	for i, b := range bad {
		if err := ValidateEntries(b); err == nil {
			t.Errorf("bad case %d accepted", i)
		} else if !strings.HasPrefix(err.Error(), "条目1") {
			t.Errorf("bad case %d: message %q should name the entry", i, err)
		}
	}
}

func TestLoadDNSEntriesSkipsEmptyCommentBodies(t *testing.T) {
	path := writeTemp(t, "# a\n#\n#  \n# b\ntcA a.com\n")
	entries, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Note != "a b" {
		t.Fatalf("empty comment bodies leaked into note: %+v", entries)
	}
}

func TestSaveDNSEntriesRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns.txt")
	// 旧格式 + 头部散注释：保存后应只剩 entries 派生的行。
	if err := os.WriteFile(path, []byte("# 将被丢弃的头部\n\ntcA a.com\n\ntcB b.com c.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := []Entry{
		{Token: "tcA", Domains: []string{"a.com"}, Note: ""},
		{Token: "tcB", Domains: []string{"b.com", "c.com"}, Note: "双域名 站点"},
	}
	if err := SaveDNSEntries(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Note != "双域名 站点" {
		t.Fatalf("roundtrip lost data: %+v", out)
	}
	if got := EntriesToMap(out); got["c.com"] != "tcB" {
		t.Errorf("map after roundtrip: %v", got)
	}
	// 目录里不留临时文件。
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("save left extra files: %v", ents)
	}
}

func TestSaveDNSEntriesRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns.txt")
	if err := os.WriteFile(path, []byte("tcA a.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveDNSEntries(path, []Entry{{Token: "tcB", Domains: []string{"bad#dom"}}}); err == nil {
		t.Fatal("invalid entries must be rejected")
	}
	kept, _ := os.ReadFile(path)
	if string(kept) != "tcA a.com\n" {
		t.Errorf("rejected save must not touch the file, got %q", kept)
	}
}
```

（`strings` 已在该文件 import——没有就补上。）

- [ ] **Step 1.2 运行确认失败**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./... -run 'DNSEntries|EntriesToMap|ValidateEntries|SaveDNSEntries'
  ```
  Expected: FAIL（编译错误 `undefined: LoadDNSEntries` 等）。

- [ ] **Step 1.3 实现**：在 `src/dnsmap.go` 中：

1) import 增加 `"fmt"`、`"path/filepath"`。
2) `LoadDNSFile` 函数体改为 entries 的薄包装（消除双份解析逻辑；行为完全不变——`#`/空行跳过、大小写规则、后行覆盖前行、token-only 行不贡献映射均保持，`TestLoadDNSFile*` 三个既有用例照常通过）：

```go
func LoadDNSFile(path string) (map[string]string, error) {
	entries, err := LoadDNSEntries(path)
	if err != nil {
		return nil, err
	}
	return EntriesToMap(entries), nil
}
```
原 doc comment 保留，末句改为"映射由 `LoadDNSEntries` + `EntriesToMap` 派生"。
3) 文件末尾追加：

```go
// Entry is one dns.txt record: a token, its domains, and an optional single-line
// note persisted as "# ..." comment lines directly above the token line.
type Entry struct {
	Token   string   `json:"token"`
	Domains []string `json:"domains"`
	Note    string   `json:"note"`
}

// LoadDNSEntries parses dns.txt into entries. Consecutive "#..." lines are held
// as a pending note group and attach to the next token line (joined by single
// spaces); a blank line drops the pending group so file-header comments do not
// become the first entry's note. Token lines keep strings.Fields semantics: a
// trailing '#' is just another field, and the UI validator is what forbids '#'
// inside domains. Files without comments load as all-empty notes (old format).
func LoadDNSEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	var note []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			note = nil
		case strings.HasPrefix(line, "#"):
			// 空注释体（"#"/"#   "）跳过：append 空串会让 join 后带上前导
			// 空格，破坏 round-trip。
			if body := strings.TrimSpace(strings.TrimLeft(line, "#")); body != "" {
				note = append(note, body)
			}
		default:
			fields := strings.Fields(line)
			entries = append(entries, Entry{
				Token:   fields[0],
				Domains: fields[1:],
				Note:    strings.Join(note, " "),
			})
			note = nil
		}
	}
	return entries, nil
}

// EntriesToMap derives the domain->token mapping from entries: domains are
// lowercased, tokens keep their case, later entries win.
func EntriesToMap(entries []Entry) map[string]string {
	mapping := make(map[string]string)
	for _, e := range entries {
		for _, d := range e.Domains {
			mapping[strings.ToLower(d)] = e.Token
		}
	}
	return mapping
}

// ValidateEntries rejects anything SaveDNSEntries could not round-trip through
// LoadDNSEntries verbatim. Messages are user-facing (shown on the admin page),
// so they are Chinese and name the offending entry (1-based). A missing "tc"
// prefix only WARNs (spec §7): real token files use it, but nothing about
// parsing breaks without it, so it is never an error.
func ValidateEntries(entries []Entry) error {
	for i, e := range entries {
		n := i + 1
		if e.Token == "" {
			return fmt.Errorf("条目%d: token 不能为空", n)
		}
		if strings.HasPrefix(e.Token, "#") {
			return fmt.Errorf("条目%d: token 不能以 # 开头", n)
		}
		if strings.ContainsAny(e.Token, " \t\r\n") {
			return fmt.Errorf("条目%d: token 不能包含空白", n)
		}
		if !strings.HasPrefix(e.Token, "tc") {
			slog.Warn("token lacks the tc prefix (tailcat addresses normally start with it)", "entry", n, "token", e.Token)
		}
		if len(e.Domains) == 0 {
			return fmt.Errorf("条目%d: 至少需要一个域名", n)
		}
		for _, d := range e.Domains {
			if d == "" {
				return fmt.Errorf("条目%d: 域名不能为空", n)
			}
			if strings.ContainsAny(d, "# \t") {
				return fmt.Errorf("条目%d: 域名 %q 不能包含 # 或空白", n, d)
			}
		}
		if strings.ContainsAny(e.Note, "\r\n") {
			return fmt.Errorf("条目%d: 备注必须是单行", n)
		}
	}
	return nil
}

// SaveDNSEntries validates and atomically rewrites path: one optional
// "# note" line per entry, then "token dom1 dom2..." — so any comment not
// attached to an entry (file headers, stray notes) is dropped on save.
// temp file + rename keeps the fsnotify watcher and any concurrent reader on
// whole-file transitions; the temp is always removed.
func SaveDNSEntries(path string, entries []Entry) error {
	if err := ValidateEntries(entries); err != nil {
		return err
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeded
	var sb strings.Builder
	for _, e := range entries {
		if e.Note != "" {
			sb.WriteString("# " + e.Note + "\n")
		}
		sb.WriteString(strings.Join(append([]string{e.Token}, e.Domains...), " ") + "\n")
	}
	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
```

4) 把 `reload` 签名改为 `func reload(path string, m *DNSMap) error`：

```go
// reload parses path and swaps it into m, keeping the old map on error. The
// error is returned so PUT /api/dns can report "saved but reload failed";
// the fsnotify path ignores it (it only logs), as before.
func reload(path string, m *DNSMap) error {
	newMap, err := LoadDNSFile(path)
	if err != nil {
		slog.Warn("reload failed; keeping previous map", "path", path, "err", err)
		return err
	}
	m.Store(newMap)
	slog.Info("reloaded", "path", path, "domains", len(newMap), "tokens", len(tokenSet(newMap)))
	return nil
}
```
调用点 `WatchDNSFile` 里的 `debounce = time.AfterFunc(50*time.Millisecond, func() { reload(path, m) })` 保持原样即可（Go 允许作为语句忽略返回值）。

- [ ] **Step 1.4 更新示例文件**：`data/dns.txt.example` 整文件替换为（注意头部块与条目之间必须空行分隔）：

```
# token                 domains (空格/Tab 分隔,可多个;一个 token 可对应多域名)
#
# 一行一个 tailcat token,后跟它对应的任意多个域名;空行忽略。
# token 行紧邻上方的 # 行是这一条目的备注,Web 管理页(DNS 配置 Tab)会显示并编辑它。
# 真实 token 请写入被 git 忽略的 data/dns.txt(不要提交到仓库)。
# 经管理页保存会重写整个文件:不属于任何条目的散注释会被丢弃。

# 示例备注:生产 web 服务器
REPLACE_WITH_TAILCAT_TOKEN    www.example.com  api.example.com
REPLACE_WITH_TAILCAT_TOKEN_2  foo.com  bar.com
```

- [ ] **Step 1.5 运行确认通过**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./...
  ```
  Expected: PASS（含既有用例）。

- [ ] **Step 1.6 提交**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add src/dnsmap.go src/dnsmap_test.go data/dns.txt.example
  git commit -m "feat(dnsmap): entry model with standalone-line notes, validation, atomic save

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 2: dialer.go —— 路由决策 + 进程内 tailcat + 直连

**Files:**
- Create: `src/dialer.go`
- Create: `src/dialer_test.go`
- Modify: `src/go.mod`、`src/go.sum`（新增依赖，go 指令升 1.27.1）
- Modify: `.gitignore`

- [ ] **Step 2.0 拉依赖并升级 go 指令**（tailcat v0.7.0 的 go.mod 要求 `go 1.27.1`，主模块必须跟上）：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src
  GOPROXY=https://goproxy.cn,direct go mod edit -go=1.27.1
  GOPROXY=https://goproxy.cn,direct go get github.com/tailscale/tailcat@v0.7.0
  GOPROXY=https://goproxy.cn,direct go mod tidy
  GOPROXY=https://goproxy.cn,direct go build ./...
  ```
  Expected: `go.mod` 出现 `github.com/tailscale/tailcat v0.7.0`（及 tailscale.com / gvisor / wireguard 间接依赖），`go.sum` 大幅扩充；`go build ./...` 无输出（成功）。依赖此前已下载到本地 module cache，正常不会再走网络。

- [ ] **Step 2.1 写失败测试**：新建 `src/dialer_test.go`：

```go
package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

func TestRoute(t *testing.T) {
	m := map[string]string{"www.example.com": "tcXYZabc", "dup.com": "tcSecond"}
	tests := []struct {
		name       string
		host       string
		port       uint16
		wantVia    string
		wantTarget string
	}{
		{"mapped", "www.example.com", 443, ViaTunnel, "tcXYZabc"},
		{"mapped case-insensitive keeps token case", "WWW.Example.COM", 443, ViaTunnel, "tcXYZabc"},
		{"later entry wins", "dup.com", 8080, ViaTunnel, "tcSecond"},
		{"unmapped domain direct", "other.com", 9999, ViaDirect, "other.com"},
		{"IPv4 literal direct", "93.184.216.34", 80, ViaDirect, "93.184.216.34"},
		{"IPv6 literal direct", "2001:db8::1", 443, ViaDirect, "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := route(tt.host, tt.port, m)
			if got.Via != tt.wantVia || got.Host != tt.wantTarget || got.Port != tt.port {
				t.Errorf("route(%q,%d) = %+v, want {%s %s %d}", tt.host, tt.port, got, tt.wantVia, tt.wantTarget, tt.port)
			}
		})
	}
}

func TestClientKeyCreatedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-key")
	d1 := NewAppDialer(path)
	defer d1.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %o, want 600", fi.Mode().Perm())
	}
	var conf tailcat.PrivateKey
	if err := json.Unmarshal(data, &conf); err != nil {
		t.Fatalf("persisted key must parse as tailcat.PrivateKey: %v", err)
	}
	d2 := NewAppDialer(path)
	defer d2.Close()
	if d2.priv.Public() != conf.Private.Public() {
		t.Error("second load must reuse the persisted identity")
	}
}

func TestClientKeyCorruptNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-key")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewAppDialer(path)
	defer d.Close()
	if d.priv.IsZero() {
		t.Fatal("want usable ephemeral fallback key")
	}
	kept, _ := os.ReadFile(path)
	if string(kept) != "not json" {
		t.Errorf("corrupt key file must be left untouched, got %q", kept)
	}
}

func TestClientForRejectsBadToken(t *testing.T) {
	d := NewAppDialer(filepath.Join(t.TempDir(), "k"))
	defer d.Close()
	const garbage = "definitely not a tailcat address"
	if _, err := d.clientFor(garbage); err == nil {
		t.Fatal("ParseAddr validation must reject garbage tokens")
	}
	if _, err := d.Dial(context.Background(), Target{Via: ViaTunnel, Host: garbage, Port: 80}); err == nil {
		t.Fatal("Dial must surface the bad-token error without touching the network")
	}
}

func TestAppDialerDirectDialsTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
			close(accepted)
		}
	}()
	d := NewAppDialer(filepath.Join(t.TempDir(), "k"))
	defer d.Close()
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Dial(context.Background(), Target{Via: ViaDirect, Host: host, Port: uint16(p)})
	if err != nil {
		t.Fatalf("direct dial: %v", err)
	}
	conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("local listener never accepted")
	}
	if _, err := d.Dial(context.Background(), Target{Via: "bogus"}); err == nil {
		t.Error("unknown via must error")
	}
}
```

- [ ] **Step 2.2 运行确认失败**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./... -run 'Route|ClientKey|ClientFor|DirectDials'
  ```
  Expected: FAIL（`undefined: route`、`undefined: ViaTunnel`、`undefined: NewAppDialer`、`undefined: Target` 等）。

- [ ] **Step 2.3 实现**：新建 `src/dialer.go`：

```go
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
```

- [ ] **Step 2.4 运行确认通过**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./...
  ```
  Expected: PASS。

- [ ] **Step 2.5 .gitignore**：在 `/data/yaofeng/workspace/popeye/tailcat-socks/.gitignore` 追加一行：
  ```
  data/tailcat-client-key
  ```

- [ ] **Step 2.6 提交**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add src/dialer.go src/dialer_test.go src/go.mod src/go.sum .gitignore
  git commit -m "feat(dialer): in-process tailcat clients with direct-fallback routing

Adopt github.com/tailscale/tailcat v0.7.0 as a library (go directive ->
1.27.1 per its go.mod). route() sends dns.txt hits to cached
tailcat.Clients and everything else to a plain net.Dialer; the client
node identity persists as JSON at data/tailcat-client-key (0600,
git-ignored, corrupt files never overwritten).

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 3: stats.go —— ConnRegistry 与字节计数

**Files:**
- Create: `src/stats.go`
- Create: `src/stats_test.go`
- Modify: `src/proxy.go`（`idleConn` 加 count、`halfClose`/`closeWrite` 支持虚拟连接）

设计要点（spec §6）：锁只出现在注册/注销（每连接各一次）与 API 快照（每秒一次）；数据路径只有原子计数。`Snapshot` 返回**值拷贝** `SessionSnapshot`——若把活的 `*Session` 指针交出去，admin 编码时的 `Opened` 读与 relay goroutine 的写会构成数据竞争。

- [ ] **Step 3.1 写失败测试**：新建 `src/stats_test.go`：

```go
package main

import (
	"net"
	"sync/atomic"
	"testing"
)

func TestConnRegistryLifecycle(t *testing.T) {
	reg := NewConnRegistry()
	s := reg.New("127.0.0.1:5000", "www.example.com", "tcXYZ", 443, ViaTunnel)

	snaps, tot := reg.Snapshot()
	if len(snaps) != 1 || snaps[0].Opened {
		t.Fatalf("fresh session must be registered un-opened: %+v", snaps)
	}
	if tot.Active != 1 || tot.SessionsTotal != 0 {
		t.Errorf("handshake-stage totals = %+v", tot)
	}

	s.BytesUp.Add(100)
	s.BytesDown.Add(250)
	reg.MarkOpen(s)
	reg.MarkOpen(s) // second call must not double-count
	_, tot = reg.Snapshot()
	if tot.SessionsTotal != 1 || tot.BytesUp != 100 || tot.BytesDown != 250 {
		t.Errorf("opened totals = %+v, want {1 100 250}", tot)
	}

	reg.Drop(s.ID)
	snaps, tot = reg.Snapshot()
	if len(snaps) != 0 || tot.Active != 0 || tot.SessionsTotal != 1 {
		t.Errorf("after drop: snaps %v totals %+v", snaps, tot)
	}
}

func TestConnRegistryIDsUnique(t *testing.T) {
	reg := NewConnRegistry()
	seen := map[uint64]bool{}
	for i := 0; i < 100; i++ {
		s := reg.New("c", "h", "t", 1, ViaDirect)
		if seen[s.ID] {
			t.Fatalf("duplicate id %d", s.ID)
		}
		seen[s.ID] = true
		reg.Drop(s.ID)
	}
}

func TestIdleConnCountsBytes(t *testing.T) {
	client, proxy := net.Pipe()
	defer client.Close()
	var up, wakes atomic.Uint64
	ic := &idleConn{Conn: proxy, onActivity: func() { wakes.Add(1) }, count: &up}
	go func() {
		client.Write([]byte("12345"))
		client.Close()
	}()
	buf := make([]byte, 4) // 小读缓冲，顺带证明逐次累加
	for {
		if _, err := ic.Read(buf); err != nil {
			break
		}
	}
	if up.Load() != 5 {
		t.Errorf("count = %d, want 5", up.Load())
	}
	if wakes.Load() == 0 {
		t.Error("onActivity never fired")
	}
	// nil count 必须继续可用（relay 接线在 Task 4，本任务先保持默认）。
	ic2 := &idleConn{Conn: proxy, onActivity: func() {}}
	if _, err := ic2.Read(make([]byte, 1)); err == nil {
		t.Error("closed pipe: want read error")
	}
}
```

- [ ] **Step 3.2 运行确认失败**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./... -run 'ConnRegistry|IdleConnCounts'
  ```
  Expected: FAIL（`undefined: NewConnRegistry`、`idleConn` 无 `count` 字段等）。

- [ ] **Step 3.3 实现**：新建 `src/stats.go`：

```go
package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// Session 是一条被代理连接在管理页上的档案：从路由确定起注册，到
// serveConn 结束注销。字节数走原子计数，relay 热路径不碰 registry 锁；
// Opened 由 registry 互斥保护（只在 SOCKS 成功回执时翻转一次）。
type Session struct {
	ID      uint64
	Client  string // 客户端 socket 的对端地址
	Host    string // 客户端请求的原始域名
	Target  string // token（tunnel）或 host（direct）
	Port    int
	Via     string // ViaTunnel / ViaDirect
	Opened  bool   // 已发成功回执；计入过 sessionsTotal
	Started time.Time

	BytesUp   atomic.Uint64 // client -> target
	BytesDown atomic.Uint64 // target -> client
}

// SessionSnapshot 是 Snapshot 在锁内做的值拷贝。绝不能把活的 *Session
// 指针交给锁外读者——Opened 会构成数据竞争。
type SessionSnapshot struct {
	ID        uint64
	Client    string
	Host      string
	Target    string
	Port      int
	Via       string
	Opened    bool
	Started   time.Time
	BytesUp   uint64
	BytesDown uint64
}

// Totals 是汇总行：当前活跃数、累计成功会话数、活跃会话字节和。
type Totals struct {
	Active        int    `json:"active"`
	SessionsTotal uint64 `json:"sessionsTotal"`
	BytesUp       uint64 `json:"bytesUp"`
	BytesDown     uint64 `json:"bytesDown"`
}

// ConnRegistry 登记活跃会话。锁只出现在注册/注销（每连接各一次）与
// Snapshot（随 admin 轮询，约每秒一次）；不新增包装层、不在数据路径加锁。
type ConnRegistry struct {
	mu            sync.Mutex
	conns         map[uint64]*Session
	nextID        uint64
	sessionsTotal uint64
}

func NewConnRegistry() *ConnRegistry {
	return &ConnRegistry{conns: make(map[uint64]*Session)}
}

// New 注册一条已路由、尚未回执的会话。accept 时不注册（半开/扫描连接
// 不污染表格），这是 spec §6 的生命周期约定。
func (r *ConnRegistry) New(clientAddr, host, target string, port int, via string) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	s := &Session{
		ID:      r.nextID,
		Client:  clientAddr,
		Host:    host,
		Target:  target,
		Port:    port,
		Via:     via,
		Started: time.Now(),
	}
	r.conns[s.ID] = s
	return s
}

// MarkOpen 翻成 open 并把 sessionsTotal 加一；按 s.Opened 去重，
// 拨号失败的会话永远不进总数。
func (r *ConnRegistry) MarkOpen(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !s.Opened {
		s.Opened = true
		r.sessionsTotal++
	}
}

// Drop 移除结束的会话（serveConn 的 defer 统一调用，成功失败皆回收）。
func (r *ConnRegistry) Drop(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, id)
}

// Snapshot 在锁内拷贝全部活跃会话与总数，让 admin API 可以在锁外编码。
func (r *ConnRegistry) Snapshot() ([]SessionSnapshot, Totals) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snaps := make([]SessionSnapshot, 0, len(r.conns))
	tot := Totals{Active: len(r.conns), SessionsTotal: r.sessionsTotal}
	for _, s := range r.conns {
		snap := SessionSnapshot{
			ID:        s.ID,
			Client:    s.Client,
			Host:      s.Host,
			Target:    s.Target,
			Port:      s.Port,
			Via:       s.Via,
			Opened:    s.Opened,
			Started:   s.Started,
			BytesUp:   s.BytesUp.Load(),
			BytesDown: s.BytesDown.Load(),
		}
		tot.BytesUp += snap.BytesUp
		tot.BytesDown += snap.BytesDown
		snaps = append(snaps, snap)
	}
	return snaps, tot
}
```

- [ ] **Step 3.4 修改 `src/proxy.go`**（本步保持既有行为完全不变：count 未接线前 relay 传 nil，虚拟连接回退在裸 TCP 测试环境不走该分支，既有用例仍绿）：

a) import 增加 `"sync/atomic"`。

b) `idleConn` 及其 `Read` 整体替换为：

```go
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
```

c) `tcpOf` / `closeWrite` / `halfClose` 三个函数整体替换为（`relay` 内注释提到 tcpOf 的部分不动）：

```go
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
```

- [ ] **Step 3.5 运行确认通过**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./... && GOPROXY=https://goproxy.cn,direct go vet ./...
  ```
  Expected: PASS + vet 无输出（vet 会跑 race detector? 不会——竞争在 Step 3.6 单独查）。

- [ ] **Step 3.6 带 race 跑一遍**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test -race ./...
  ```
  Expected: PASS，无 DATA RACE 报告。

- [ ] **Step 3.7 提交**：
  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add src/stats.go src/stats_test.go src/proxy.go
  git commit -m "feat(stats): ConnRegistry session table and idleConn byte counters

Snapshot hands out by-value copies so Opened reads never race relay
goroutines outside the mutex; halfClose/closeWrite gain an
interface-based fallback for the netstack conns the in-process tailcat
client will return (Task 4).

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 4: serveConn 走 route+Dialer；删除子进程与上游链路（proxy.go / main.go / 测试整体切换）

本任务是一致性大切换：`NewServer` 签名、`Server` 字段、`main.run` 参数、proxy 测试同时变——单独任何一半都编译不过，所以删除、测试、实现放在同一个提交里。

**Files:**
- Delete: `src/main_test.go`、`src/testdata/fake-tailcat/`（整个目录）
- Rewrite: `src/proxy_test.go`、`src/proxy.go`、`src/main.go`

- [ ] **Step 4.1 删除子进程时代的测试与夹具**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git rm -q src/main_test.go
  git rm -q -r src/testdata/fake-tailcat
  ```

  说明：`main_test.go` 的 TestMain（构建 fake-tailcat）、`TestUpstreamAddr`、spawn/waitReady/SIGTERM/SIGKILL 系列全部随上游子进程机制作废；`proxy_test.go` 里 `fakeUpstream`/`silentUpstream`/`earlyCloseUpstream`/`eofThenReplyUpstream` 是说 SOCKS5 的假上游，一并被 Step 4.2 的 plainServer 取代；`TestDialUpstreamReturnsTCPConnForHalfClose` 与 `TestSocks5ProxyUpstreamRefusalRepliesFail` 测试的对象已不存在，删除。

- [ ] **Step 4.2 整体替换 `src/proxy_test.go`** 为：

```go
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
```

- [ ] **Step 4.3 运行确认编译失败**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./...
  ```
  Expected: FAIL — `too many arguments in call to NewServer`、`srv.Registry undefined`、`undefined: Dialer/Target` 等（实现尚未切换，属预期红灯）。

- [ ] **Step 4.4 整体替换 `src/proxy.go`** 为（`Serve`/`handle`/`socksFail`/`readATYPAddr`/relay 主体逐字保留；删去 `upstreamDialTimeout`/`dialUpstream`/`socksConnectRequest`/`socksClientConnect`；两处注释里 "upstream" 改为 "dial"；文件尾部辅助函数保持 Task 3 落定的最终形态）：

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// relayIdleTimeout mirrors the Python version: the relay breaks after
	// 300s with no reads of real bytes in either direction (only reads
	// re-arm the window; writes never do).
	relayIdleTimeout = 300 * time.Second
	// handshakeTimeout bounds the client-facing SOCKS5 handshake (greeting,
	// request parse, and our reply) so a silent or stalled client cannot pin
	// a goroutine and fd forever. Cleared before relaying. The dial budget
	// lives with the Dialer (dialTimeout in dialer.go).
	handshakeTimeout = 15 * time.Second
)

// SOCKS5 success/failure replies (BOUND.ADDR 0.0.0.0:0, as in Python).
var (
	socksOKReply   = []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	socksFailReply = []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
)

// Server is the client-facing SOCKS5 front proxy. On each CONNECT it routes
// the target (see route in dialer.go): domains mapped in dns.txt are dialed
// through the in-process tailcat library, everything else directly; the
// established pair is relayed and accounted in Registry for the admin page.
type Server struct {
	DNSMap   *DNSMap
	Dialer   Dialer
	Registry *ConnRegistry
	ln       net.Listener
	// relayIdle is how long the relay tolerates silence in either direction
	// before tearing the session down.
	relayIdle time.Duration
	// handshakeTimeout bounds the client-facing SOCKS5 handshake; see the
	// const.
	handshakeTimeout time.Duration
}

// NewServer binds the listen address. Use ActualAddr for the resolved port
// (listening on port 0 is useful in tests).
func NewServer(dnsMap *DNSMap, d Dialer, reg *ConnRegistry, listen string) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &Server{DNSMap: dnsMap, Dialer: d, Registry: reg, ln: ln, relayIdle: relayIdleTimeout, handshakeTimeout: handshakeTimeout}, nil
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
// version byte-for-byte (greeting, CONNECT-only, ATYP 1/3/4, socks5h), then
// routes the target through the Dialer and relays the pair.
func (s *Server) serveConn(client net.Conn) {
	// Bound the client-facing handshake reads; the early return paths keep
	// it armed — we close the conn right after anyway. The dial is bounded
	// by the Dialer's own budget, and the reply below re-arms this deadline
	// so a slow dial cannot silently consume the reply window.
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

	target := route(host, port, s.DNSMap.Load())
	sess := s.Registry.New(client.RemoteAddr().String(), host, target.Host, int(target.Port), target.Via)
	defer s.Registry.Drop(sess.ID)

	up, err := s.Dialer.Dial(context.Background(), target)
	// The dial may have consumed the handshake budget; give the reply a
	// fresh window (success or failure) so a slow dial cannot silently eat
	// it.
	client.SetDeadline(time.Now().Add(s.handshakeTimeout))
	if err != nil {
		slog.Debug("dial failed", "via", target.Via, "host", host, "err", err)
		socksFail(client) // 未 MarkOpen：失败的会话不进 sessionsTotal
		return
	}
	if _, err := client.Write(socksOKReply); err != nil {
		up.Close() // client never learned CONNECT succeeded; no relay for it
		return
	}
	s.Registry.MarkOpen(sess)

	// Handshake over; relay's idle timer governs both legs from here.
	client.SetDeadline(time.Time{})
	up.SetDeadline(time.Time{})         // the Dialer hands back deadline-free conns; kept for clarity
	relay(client, up, s.relayIdle, sess) // relay owns both conns and closes them
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
		// non-ASCII domain bytes, Go forwards them raw and lets the dial
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
func relay(client, upstream net.Conn, idle time.Duration, sess *Session) {
	var once sync.Once
	// stop closes the conns relay was handed; halfClose reaches the socket
	// through halfOf, which unwraps the idleConn wrappers below, so the
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
	// 方向天然正确：读客户端腿=上行、读目标腿=下行（spec §6）。
	wrappedClient := &idleConn{Conn: client, onActivity: wake, count: &sess.BytesUp}
	wrappedUpstream := &idleConn{Conn: upstream, onActivity: wake, count: &sess.BytesDown}

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
// 于是 Close 发 FIN 而不是 RST。（单独 CloseRead 给不了这个保证，已实测。）
// CloseRead 对应 Python 的 shutdown(SHUT_RD)——丢弃后续输入，代价与 Python
// 相同。错误一律忽略，同 Python 的 except OSError: pass。真 TCP 走方法对；
// 虚拟连接走接口回退；都不支持的留给 Close。
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
```

- [ ] **Step 4.5 整体替换 `src/main.go`** 为（`--listen` 校验块逐字保留自旧文件；admin 接线在 Task 7 加入，此处不留占位）：

```go
// tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
// tailcat tokens (tc...) and dials mapped tokens in-process through the
// tailcat library; targets not found in dns.txt (plain domains and IP
// literals) are dialed directly. See README.md for usage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "tailcat-socks"))
	var (
		listen        = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (host:port; empty host binds all interfaces)")
		dnsFile       = flag.String("dns-file", "data/dns.txt", "domain->token mapping file")
		clientKeyFile = flag.String("client-key-file", "data/tailcat-client-key", "persistent tailcat client identity (JSON)")
		noWatch       = flag.Bool("no-watch", false, "disable dns.txt hot-reload")
	)
	flag.Parse()
	os.Exit(run(*listen, *dnsFile, *clientKeyFile, *noWatch))
}

func run(listen, dnsFile, clientKeyFile string, noWatch bool) int {
	// Install the signal handler before doing anything else: stopNotify is
	// deferred FIRST so it runs LAST — the handler stays installed until
	// run() actually returns, so a repeat Ctrl-C during shutdown is
	// swallowed instead of hitting the default kill disposition
	// mid-cleanup.
	notifyCtx, stopNotify := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopNotify()
	ctx, cancel := context.WithCancel(notifyCtx)
	defer cancel()

	first, err := LoadDNSFile(dnsFile)
	if err != nil {
		slog.Error("cannot load dns file", "path", dnsFile, "err", err)
		return 1
	}
	dnsMap := &DNSMap{}
	dnsMap.Store(first)

	// Validate --listen: SplitHostPort for the host:port shape plus a numeric
	// in-range port, so junk like "host:abc" is reported here instead of by
	// net.Listen later. An empty host (":8080") binds every interface. The
	// "(want host:port)" hint belongs to the shape error only, not the port.
	lHost, lPort, err := net.SplitHostPort(listen)
	if err != nil {
		slog.Error("bad --listen", "value", listen, "err", fmt.Errorf("%q: %w (want host:port)", listen, err))
		return 1
	}
	if p, perr := strconv.Atoi(lPort); perr != nil {
		slog.Error("bad --listen", "value", listen, "err", fmt.Errorf("%q: %w", listen, perr))
		return 1
	} else if p < 0 || p > 65535 {
		slog.Error("bad --listen", "value", listen, "err", fmt.Errorf("%q: port %d out of range", listen, p))
		return 1
	}
	listen = net.JoinHostPort(lHost, lPort)

	// The client key cannot break startup: a corrupt or unwritable file only
	// WARNs and falls back to an ephemeral identity (dialer.go), never
	// clobbers the file.
	dialer := NewAppDialer(clientKeyFile)
	registry := NewConnRegistry()
	srv, err := NewServer(dnsMap, dialer, registry, listen)
	if err != nil {
		slog.Error("cannot listen", "addr", listen, "err", err)
		return 1
	}

	if !noWatch {
		go WatchDNSFile(ctx, dnsFile, dnsMap)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	host, port, _ := net.SplitHostPort(srv.ActualAddr().String())
	slog.Info("listening", "url", "socks5h://"+net.JoinHostPort(host, port))
	slog.Info("dns map loaded", "domains", len(first), "tokens", len(tokenSet(first)))

	code := 0
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		// srv.Close() during shutdown makes Serve() report net.ErrClosed;
		// that is not a real failure, so keep signal exits quiet. Any other
		// error means the listener broke after a successful bind — that is
		// worth a nonzero exit.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("server error", "err", err)
			code = 1
		}
	}
	cancel()       // stops the dns watcher
	srv.Close()    // stop accepting; in-flight relays end on their own
	dialer.Close() // tailcat clients go last (spec §10 shutdown order)
	return code
}
```

行为变化说明（相对旧 main.go）：`--upstream`/`--tailcat-bin`/`--no-autolaunch` 三个 flag 与 `upstreamAddr`/`spawnTailcatSocks`/`waitReady`/`logChildExit`/`childKillGrace` 及 `childDone` select 分支整体删除；"using upstream" 日志删除；import 去掉 `os/exec` 与 `time`；`serveErr` 非 ErrClosed 错误现在置 `code = 1`（旧实现只记日志仍退 0——绑定成功后监听器真坏了，退出码应当非零）。

- [ ] **Step 4.6 全量测试 + race + vet**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src
  GOPROXY=https://goproxy.cn,direct go test ./... -race
  GOPROXY=https://goproxy.cn,direct go vet ./...
  ```
  Expected: 全部 PASS（race 干净）、vet 无输出。

- [ ] **Step 4.7 提交**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add -A src/
  git commit -m "feat!: route by dns.txt; tailcat in-process; direct dial for misses

Drop the upstream SOCKS chain and the tailcat child process entirely:
serveConn consults route() and dials through the Dialer interface
(tailcat library for mapped tokens, plain TCP for everything else),
registering each session in ConnRegistry for the admin page. main.go
loses --upstream/--tailcat-bin/--no-autolaunch and the spawn/ready/reap
machinery; the SOCKS-speaking fake upstreams and the fake-tailcat e2e
fixture go with them.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 5: admin HTTP API（src/admin.go + src/admin_test.go）

`Handler()` 本任务先只注册三个 API 路由；`/`（嵌入页面）在 Task 6 加，main 接线在 Task 7 加。`GET /api/connections` 响应严格按 spec §8：`state` 为 handshake|open、`startedAt` 为 epoch 毫秒——SessionSnapshot 不带 JSON tag，所以用 `connJSON` DTO 转一道。

**Files:**
- Create: `src/admin.go`
- Create: `src/admin_test.go`

- [ ] **Step 5.1 先写失败测试 `src/admin_test.go`**：

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAdminFixture backs an Admin with a temp dns.txt (saved through the real
// writer) and a live DNSMap seeded from it.
func newAdminFixture(t *testing.T) (*Admin, *ConnRegistry, *DNSMap, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dns.txt")
	seed := []Entry{
		{Token: "tcAAA111", Domains: []string{"a.example.com"}, Note: "备注一"},
		{Token: "tcBBB222", Domains: []string{"b.example.com", "c.example.com"}},
	}
	if err := SaveDNSEntries(path, seed); err != nil {
		t.Fatal(err)
	}
	dnsMap := &DNSMap{}
	dnsMap.Store(EntriesToMap(seed))
	reg := NewConnRegistry()
	return NewAdmin(reg, dnsMap, path), reg, dnsMap, path
}

// connItem mirrors one spec §8 active[] entry. A named type keeps the decode
// target and the byID map the SAME struct type — anonymous structs that
// differ only in tags are not identical.
type connItem struct {
	ID        uint64 `json:"id"`
	Client    string `json:"client"`
	Host      string `json:"host"`
	Target    string `json:"target"`
	Port      int    `json:"port"`
	Via       string `json:"via"`
	State     string `json:"state"`
	StartedAt int64  `json:"startedAt"`
	BytesUp   uint64 `json:"bytesUp"`
	BytesDown uint64 `json:"bytesDown"`
}

type connectionsDoc struct {
	Active []connItem `json:"active"`
	Totals Totals     `json:"totals"`
}

func TestHandleConnectionsShapeAndCounts(t *testing.T) {
	admin, reg, _, _ := newAdminFixture(t)
	s1 := reg.New("127.0.0.1:5001", "a.example.com", "tcAAA111", 443, ViaTunnel)
	reg.MarkOpen(s1)
	s1.BytesUp.Add(10)
	s1.BytesDown.Add(20)
	s2 := reg.New("127.0.0.1:5002", "plain.example", "plain.example", 80, ViaDirect)
	// s2 deliberately left in handshake state

	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/connections", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc connectionsDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body)
	}
	if len(doc.Active) != 2 {
		t.Fatalf("active = %d, want 2: %s", len(doc.Active), rec.Body)
	}
	byID := map[uint64]connItem{}
	for _, c := range doc.Active {
		byID[c.ID] = c
	}
	c1, c2 := byID[s1.ID], byID[s2.ID]
	if c1.State != "open" || c2.State != "handshake" {
		t.Errorf("states = %q/%q, want open/handshake", c1.State, c2.State)
	}
	if c1.Host != "a.example.com" || c1.Target != "tcAAA111" || c1.Port != 443 || c1.Via != ViaTunnel || c1.Client != "127.0.0.1:5001" {
		t.Errorf("c1 = %+v", c1)
	}
	if c1.BytesUp != 10 || c1.BytesDown != 20 {
		t.Errorf("c1 bytes = %d/%d, want 10/20", c1.BytesUp, c1.BytesDown)
	}
	if now := time.Now().UnixMilli(); c1.StartedAt <= 0 || c1.StartedAt > now+1000 {
		t.Errorf("startedAt = %d, want epoch-ms near now (%d)", c1.StartedAt, now)
	}
	if doc.Totals.Active != 2 || doc.Totals.SessionsTotal != 1 || doc.Totals.BytesUp != 10 || doc.Totals.BytesDown != 20 {
		t.Errorf("totals = %+v, want {2 1 10 20}", doc.Totals)
	}
}

// An empty active list must serialize as [] not null: the page indexes it
// without nil-checks.
func TestHandleConnectionsEmptyActiveIsArray(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/connections", nil))
	if !strings.Contains(rec.Body.String(), `"active":[]`) {
		t.Errorf("body = %s, want \"active\":[]", rec.Body)
	}
}

func TestHandlerMethodAndPathRouting(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	h := admin.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/nope = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/dns", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/dns = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/dns", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/dns = %d, want 405", rec.Code)
	}
}

func TestHandleGetDNS(t *testing.T) {
	admin, _, _, path := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/dns", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var doc struct {
		Path    string  `json:"path"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body)
	}
	if doc.Path != path {
		t.Errorf("path = %q, want %q", doc.Path, path)
	}
	if len(doc.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(doc.Entries))
	}
	if doc.Entries[0].Token != "tcAAA111" || doc.Entries[0].Note != "备注一" || len(doc.Entries[0].Domains) != 1 {
		t.Errorf("entry0 = %+v", doc.Entries[0])
	}
	if doc.Entries[1].Token != "tcBBB222" || doc.Entries[1].Note != "" || len(doc.Entries[1].Domains) != 2 {
		t.Errorf("entry1 = %+v", doc.Entries[1])
	}
}

func TestHandlePutDNSAppliesImmediately(t *testing.T) {
	admin, _, dnsMap, path := newAdminFixture(t)
	newEntries := []Entry{{Token: "tcCCC333", Domains: []string{"d.example.com"}, Note: "换绑"}}
	body, err := json.Marshal(newEntries)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/dns", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp struct {
		Domains int `json:"domains"`
		Tokens  int `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body)
	}
	if resp.Domains != 1 || resp.Tokens != 1 {
		t.Errorf("resp = %+v, want {1 1}", resp)
	}

	// 保存即生效：httptest 里 fsnotify 监听根本没跑，map 也必须立变。
	m := dnsMap.Load()
	if m["d.example.com"] != "tcCCC333" {
		t.Errorf("dnsMap = %v, want d.example.com -> tcCCC333", m)
	}
	if _, ok := m["a.example.com"]; ok {
		t.Errorf("old entries still in map: %v", m)
	}

	// 落盘格式：备注是独立 # 行，紧跟 token 行。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# 换绑\ntcCCC333 d.example.com\n") {
		t.Errorf("file =\n%s", raw)
	}
}

func TestHandlePutDNSInvalidLeavesEverythingAlone(t *testing.T) {
	admin, _, dnsMap, path := newAdminFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeLen := len(dnsMap.Load())

	body, _ := json.Marshal([]Entry{{Token: "tcX", Domains: []string{"bad#domain"}}})
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/dns", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == "" {
		t.Fatalf("body = %s, want {\"error\": \"...\"}", rec.Body)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("file was modified despite 400:\n%s", after)
	}
	if len(dnsMap.Load()) != beforeLen {
		t.Errorf("dnsMap changed despite 400")
	}
}

func TestHandlePutDNSMalformedJSON(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/dns", strings.NewReader("{oops")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
```

- [ ] **Step 5.2 运行确认编译失败**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./...
  ```
  Expected: FAIL — `undefined: NewAdmin`、`undefined: Admin`。

- [ ] **Step 5.3 写 `src/admin.go`**：

```go
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Admin 承载管理页的 HTTP API（spec §8）。页面路由 "/" 在 Task 6 注册，
// 接线在 Task 7；本类型只持有 registry/dnsMap/dnsPath 三个句柄，读取
// 全部走锁内快照或原子加载，不需要自己的锁。
type Admin struct {
	registry *ConnRegistry
	dnsMap   *DNSMap
	dnsPath  string
}

func NewAdmin(registry *ConnRegistry, dnsMap *DNSMap, dnsPath string) *Admin {
	return &Admin{registry: registry, dnsMap: dnsMap, dnsPath: dnsPath}
}

// Handler 返回 admin mux，用 Go 1.22 的方法+路径模式：未注册路径 404，
// 同路径异方法由 mux 直接给 405。
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/connections", a.handleConnections)
	mux.HandleFunc("GET /api/dns", a.handleGetDNS)
	mux.HandleFunc("PUT /api/dns", a.handlePutDNS)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	// header 已发出，编码失败只能记日志（spec §10：无远端依赖的理论路径）。
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("admin encode failed", "err", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// connJSON 是 spec §8 的 active 条目 DTO：SessionSnapshot 不带 JSON tag，
// state/startedAt 又需要转换，所以在这里转一道。
type connJSON struct {
	ID        uint64 `json:"id"`
	Client    string `json:"client"`
	Host      string `json:"host"`
	Target    string `json:"target"`
	Port      int    `json:"port"`
	Via       string `json:"via"`
	State     string `json:"state"`
	StartedAt int64  `json:"startedAt"`
	BytesUp   uint64 `json:"bytesUp"`
	BytesDown uint64 `json:"bytesDown"`
}

type connectionsDoc struct {
	Active []connJSON `json:"active"`
	Totals Totals     `json:"totals"`
}

func (a *Admin) handleConnections(w http.ResponseWriter, r *http.Request) {
	snaps, totals := a.registry.Snapshot()
	doc := connectionsDoc{Active: make([]connJSON, 0, len(snaps)), Totals: totals}
	for _, s := range snaps {
		state := "handshake"
		if s.Opened {
			state = "open"
		}
		doc.Active = append(doc.Active, connJSON{
			ID:        s.ID,
			Client:    s.Client,
			Host:      s.Host,
			Target:    s.Target,
			Port:      s.Port,
			Via:       s.Via,
			State:     state,
			StartedAt: s.Started.UnixMilli(),
			BytesUp:   s.BytesUp,
			BytesDown: s.BytesDown,
		})
	}
	writeJSON(w, http.StatusOK, doc)
}

func (a *Admin) handleGetDNS(w http.ResponseWriter, r *http.Request) {
	entries, err := LoadDNSEntries(a.dnsPath)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "读取 dns 文件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": a.dnsPath, "entries": entries})
}

// handlePutDNS：校验 → 原子落盘 → 立即 reload 进 DNSMap——不等 fsnotify
// 的 50ms 防抖，"保存即生效"（spec §7）。校验失败/写失败都不碰映射；
// 写成功但 reload 失败时文件已是新内容而 map 还是旧内容，如实告诉页面。
func (a *Admin) handlePutDNS(w http.ResponseWriter, r *http.Request) {
	var entries []Entry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		httpError(w, http.StatusBadRequest, "请求体不是合法的 entries JSON: "+err.Error())
		return
	}
	if err := ValidateEntries(entries); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := SaveDNSEntries(a.dnsPath, entries); err != nil {
		httpError(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	if err := reload(a.dnsPath, a.dnsMap); err != nil {
		httpError(w, http.StatusInternalServerError, "已保存但热加载失败: "+err.Error())
		return
	}
	domains := 0
	for _, e := range entries {
		domains += len(e.Domains)
	}
	writeJSON(w, http.StatusOK, map[string]int{"domains": domains, "tokens": len(entries)})
}
```

- [ ] **Step 5.4 运行确认通过 + race**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src
  GOPROXY=https://goproxy.cn,direct go test ./... -race
  ```
  Expected: 全部 PASS。

- [ ] **Step 5.5 提交**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add src/admin.go src/admin_test.go
  git commit -m "feat(admin): JSON API for connections snapshot and dns edit

GET /api/connections maps the registry snapshot to the spec §8 document
(handshake|open state, epoch-ms startedAt); GET/PUT /api/dns read and
write dns.txt through the Task 1 entry layer, PUT validating before the
atomic write and reloading the live DNSMap synchronously so a save is
effective without waiting for the fsnotify debounce.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 6: 内嵌管理页（src/static/index.html + go:embed）

单文件 vanilla JS 页面（spec §9）：两个 Tab、1s 轮询、速率由前端对相邻两次 (t, bytes) 求差——服务端只报累计值。全部动态文本经 `esc()` 转义。

**Files:**
- Create: `src/static/index.html`
- Modify: `src/admin.go`（加 embed 与 `GET /` 路由）
- Test: `src/admin_test.go`（追加）

- [ ] **Step 6.1 追加失败测试**：在 `src/admin_test.go` 末尾追加：

```go
func TestHandleIndexServesEmbeddedPage(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html...", ct)
	}
	body := rec.Body.String()
	for _, marker := range []string{"当前连接", "DNS 配置", "/api/connections", "/api/dns", "备注"} {
		if !strings.Contains(body, marker) {
			t.Errorf("page missing %q", marker)
		}
	}
}
```

- [ ] **Step 6.2 运行确认失败**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./...
  ```
  Expected: FAIL — `TestHandleIndexServesEmbeddedPage: GET / = 404`。

- [ ] **Step 6.3 新建 `src/static/index.html`**，完整内容：

```html
<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tailcat-socks 管理</title>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { margin: 0; background: #12141a; color: #d8dce6; font: 14px/1.5 system-ui, "Segoe UI", sans-serif; }
header { display: flex; align-items: center; gap: 16px; padding: 12px 20px; border-bottom: 1px solid #262a33; }
header h1 { font-size: 16px; margin: 0; font-weight: 600; }
.tabs { display: flex; gap: 4px; }
.tabs button { background: none; border: 0; color: #8b93a5; padding: 8px 14px; font-size: 14px; cursor: pointer; border-bottom: 2px solid transparent; }
.tabs button.active { color: #e6edf3; border-bottom-color: #3d7efd; }
main { padding: 16px 20px; max-width: 1200px; }
.cards { display: flex; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
.card { background: #1a1d24; border: 1px solid #262a33; border-radius: 8px; padding: 10px 16px; min-width: 150px; }
.card .k { color: #8b93a5; font-size: 12px; }
.card .v { font-size: 20px; font-weight: 600; margin-top: 2px; }
table { width: 100%; border-collapse: collapse; background: #1a1d24; border: 1px solid #262a33; border-radius: 8px; }
th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid #23262e; white-space: nowrap; }
th { color: #8b93a5; font-weight: 500; font-size: 12px; background: #171a20; }
tr:last-child td { border-bottom: 0; }
.badge { padding: 1px 8px; border-radius: 999px; font-size: 12px; }
.badge.tunnel { background: #1d3a63; color: #9cc3ff; }
.badge.direct { background: #274a2c; color: #9fe0a8; }
.badge.handshake { background: #4a3d1f; color: #e6c979; }
.badge.open { background: #274a2c; color: #9fe0a8; }
.muted { color: #6b7280; }
button.act { background: #262a33; color: #d8dce6; border: 1px solid #343a46; border-radius: 6px; padding: 5px 12px; cursor: pointer; }
button.act:hover { background: #2f3440; }
button.primary { background: #2c5fba; border-color: #2c5fba; }
button.primary:hover { background: #3d7efd; }
input.txt { background: #12141a; color: #d8dce6; border: 1px solid #343a46; border-radius: 6px; padding: 6px 8px; width: 100%; }
#status { margin-left: auto; font-size: 12px; color: #8b93a5; }
#status.err { color: #ff7b72; }
.msg { margin: 10px 0; font-size: 13px; }
.msg.err { color: #ff7b72; }
.msg.ok { color: #7ee787; }
.bar { display: flex; gap: 8px; margin-top: 12px; align-items: center; }
</style>
</head>
<body>
<header>
  <h1>tailcat-socks</h1>
  <nav class="tabs">
    <button id="tab-conn" class="active">当前连接</button>
    <button id="tab-dns">DNS 配置</button>
  </nav>
  <span id="status">加载中…</span>
</header>
<main>
  <section id="view-conn">
    <div class="cards">
      <div class="card"><div class="k">活跃连接</div><div class="v" id="sum-active">–</div></div>
      <div class="card"><div class="k">累计会话</div><div class="v" id="sum-total">–</div></div>
      <div class="card"><div class="k">上行合计</div><div class="v" id="sum-up">–</div></div>
      <div class="card"><div class="k">下行合计</div><div class="v" id="sum-down">–</div></div>
      <div class="card"><div class="k">聚合速率 ↑ / ↓</div><div class="v" id="sum-rate" style="font-size:15px">– / –</div></div>
    </div>
    <table>
      <thead><tr>
        <th>#</th><th>域名</th><th>目标</th><th>端口</th><th>通道</th><th>状态</th>
        <th>客户端</th><th>时长</th><th>↑ 上/下</th><th>↑ 速率</th><th>↓ 字节</th><th>↓ 速率</th>
      </tr></thead>
      <tbody id="conn-body"><tr><td colspan="12" class="muted">加载中…</td></tr></tbody>
    </table>
  </section>
  <section id="view-dns" hidden>
    <p class="muted" id="dns-path"></p>
    <table>
      <thead><tr>
        <th style="width:260px">token</th><th>domains（逗号或空格分隔）</th>
        <th style="width:220px">备注</th><th style="width:76px"></th>
      </tr></thead>
      <tbody id="dns-body"></tbody>
    </table>
    <div class="bar">
      <button class="act" id="dns-add">＋ 新增条目</button>
      <button class="act primary" id="dns-save">保存并生效</button>
    </div>
    <div id="dns-msg" class="msg"></div>
  </section>
</main>
<script>
"use strict";
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"']/g, (ch) =>
  ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"})[ch]);
const fmtBytes = (n) => {
  if (n < 1024) return n + " B";
  const u = ["KB", "MB", "GB", "TB"];
  let i = -1, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 ? 0 : 1) + " " + u[i];
};
const fmtRate = (bps) => fmtBytes(Math.max(0, bps)) + "/s"; // clamp: id 复用/重启后差值可能为负
const fmtDur = (ms) => {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m" + (s % 60) + "s";
  return Math.floor(s / 3600) + "h" + Math.floor((s % 3600) / 60) + "m";
};

// ---- Tab 切换 ----
function showTab(name) {
  $("tab-conn").classList.toggle("active", name === "tab-conn");
  $("tab-dns").classList.toggle("active", name === "tab-dns");
  $("view-conn").hidden = name !== "tab-conn";
  $("view-dns").hidden = name !== "tab-dns";
}
$("tab-conn").onclick = () => showTab("tab-conn");
$("tab-dns").onclick = () => { showTab("tab-dns"); loadDns(); };

// ---- 当前连接：1s 轮询，速率 = 相邻两次采样差（spec §6） ----
let prev = new Map();     // id -> {t, up, down}
let prevTot = null;       // {t, up, down}

async function pollConn() {
  let doc;
  try {
    const r = await fetch("/api/connections", {cache: "no-store"});
    if (!r.ok) throw new Error("HTTP " + r.status);
    doc = await r.json();
  } catch (e) {
    $("status").textContent = "管理 API 失联: " + e.message;
    $("status").className = "err";
    return;
  }
  $("status").textContent = "更新于 " + new Date().toLocaleTimeString();
  $("status").className = "";
  const now = Date.now();

  const cur = new Map();
  const rows = doc.active.map((c) => {
    const p = prev.get(c.id);
    let ur = "–", dr = "–";
    if (p && now > p.t) {
      const dt = (now - p.t) / 1000;
      ur = fmtRate((c.bytesUp - p.up) / dt);
      dr = fmtRate((c.bytesDown - p.down) / dt);
    }
    cur.set(c.id, {t: now, up: c.bytesUp, down: c.bytesDown});
    const stateBadge = c.state === "open"
      ? '<span class="badge open">open</span>'
      : '<span class="badge handshake">handshake</span>';
    return "<tr><td>" + c.id +
      "</td><td>" + esc(c.host) +
      "</td><td class=\"muted\">" + esc(c.target) +
      "</td><td>" + c.port +
      "</td><td><span class=\"badge " + esc(c.via) + "\">" + esc(c.via) + "</span>" +
      "</td><td>" + stateBadge +
      "</td><td class=\"muted\">" + esc(c.client) +
      "</td><td>" + fmtDur(now - c.startedAt) +
      "</td><td>" + fmtBytes(c.bytesUp) +
      "</td><td>" + ur +
      "</td><td>" + fmtBytes(c.bytesDown) +
      "</td><td>" + dr + "</td></tr>";
  });
  prev = cur;
  $("conn-body").innerHTML = rows.length
    ? rows.join("")
    : '<tr><td colspan="12" class="muted">暂无活跃连接</td></tr>';

  const t = doc.totals;
  $("sum-active").textContent = t.active;
  $("sum-total").textContent = t.sessionsTotal;
  $("sum-up").textContent = fmtBytes(t.bytesUp);
  $("sum-down").textContent = fmtBytes(t.bytesDown);
  if (prevTot && now > prevTot.t) {
    const dt = (now - prevTot.t) / 1000;
    $("sum-rate").textContent =
      fmtRate((t.bytesUp - prevTot.up) / dt) + " / " + fmtRate((t.bytesDown - prevTot.down) / dt);
  }
  prevTot = {t: now, up: t.bytesUp, down: t.bytesDown};
}

// ---- DNS 配置 ----
function dnsMsg(text, isErr) {
  const m = $("dns-msg");
  m.textContent = text;
  m.className = "msg " + (isErr ? "err" : "ok");
}

async function loadDns() {
  try {
    const r = await fetch("/api/dns", {cache: "no-store"});
    const doc = await r.json();
    if (!r.ok) { dnsMsg("读取失败: " + (doc.error || ("HTTP " + r.status)), true); return; }
    $("dns-path").textContent = "文件: " + doc.path;
    $("dns-body").innerHTML = "";
    for (const e of doc.entries) addDnsRow(e.token, (e.domains || []).join(", "), e.note);
    dnsMsg("");
  } catch (e) {
    dnsMsg("读取失败: " + e.message, true);
  }
}

function addDnsRow(token, domains, note) {
  const tr = document.createElement("tr");
  tr.innerHTML =
    '<td><input class="txt" data-f="token" value="' + esc(token) + '" placeholder="tc..."></td>' +
    '<td><input class="txt" data-f="domains" value="' + esc(domains) + '" placeholder="a.example.com, b.example.com"></td>' +
    '<td><input class="txt" data-f="note" value="' + esc(note) + '" placeholder="备注"></td>' +
    '<td><button class="act" data-act="del">删除</button></td>';
  tr.querySelector('[data-act="del"]').onclick = () => tr.remove();
  $("dns-body").appendChild(tr);
}

function collectDns() {
  const out = [];
  for (const tr of $("dns-body").children) {
    const token = tr.querySelector('[data-f="token"]').value.trim();
    const domains = tr.querySelector('[data-f="domains"]').value.split(/[\s,]+/).filter(Boolean);
    const note = tr.querySelector('[data-f="note"]').value.trim();
    if (!token && !domains.length && !note) continue; // 全空行静默忽略
    out.push({token: token, domains: domains, note: note});
  }
  return out;
}

async function saveDns() {
  const entries = collectDns();
  try {
    const r = await fetch("/api/dns", {
      method: "PUT",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify(entries),
    });
    const doc = await r.json().catch(() => ({}));
    if (!r.ok) { dnsMsg("保存失败: " + (doc.error || ("HTTP " + r.status)), true); return; }
    dnsMsg("已保存并生效（" + doc.domains + " 域名 / " + doc.tokens + " token）", false);
    loadDns(); // 拉回落盘后的真身：last-writer-wins（spec §7）
  } catch (e) {
    dnsMsg("保存失败: " + e.message, true);
  }
}

$("dns-add").onclick = () => addDnsRow("", "", "");
$("dns-save").onclick = saveDns;

setInterval(pollConn, 1000);
pollConn();
</script>
</body>
</html>
```

- [ ] **Step 6.4 修改 `src/admin.go`**：

a) 不新增 import：`//go:embed` 嵌单个文件进 `string` 类型不要求导入 `embed` 包（只有 `embed.FS` 才需要）。

b) 在 `// Handler 返回 admin mux...` 注释之前插入：

```go
//go:embed static/index.html
var staticIndexHTML string // 编译期打进二进制；单个嵌入模式允许直接嵌进 string
```

c) `Handler()` 里三条 API 路由之前加一行：

```go
	mux.HandleFunc("GET /", a.handleIndex)
```

d) `handleConnections` 之前加方法：

```go
func (a *Admin) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// go:embed 在编译期保证内容存在；写失败只可能是客户端已断开。
	_, _ = w.Write([]byte(staticIndexHTML))
}
```

注意：`//go:embed` 要求 `src/static/index.html` 在编译时已存在——Step 6.3 先建文件，此处才编译得过。

- [ ] **Step 6.5 运行确认通过**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src
  GOPROXY=https://goproxy.cn,direct go test ./... -race
  ```
  Expected: 全部 PASS（`TestHandleIndexServesEmbeddedPage` 通过）。

- [ ] **Step 6.6 提交**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add src/static/index.html src/admin.go src/admin_test.go
  git commit -m "feat(admin): embedded single-page admin (connections + dns editor)

Vanilla-JS page served via go:embed: 1s connection polling with
client-side rate deltas keyed by session id, summary cards, and a
dns.txt table editor whose save PUTs through the validating API and
reports the server's domain/token counts. All dynamic text escaped.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 7: main.go 接线 admin（flag > env > 默认；fail-fast；退出顺序）

新增 `--admin-addr`（env `ADMIN_ADDR`）与 `--client-key-file` 的 env 回退（`TAILCAT_CLIENT_KEY_FILE`），优先级统一由 `resolveSetting` 实现（flag.Visit 只报告命令行显式设置的 flag）。admin 绑定失败 = ERROR + 退出 1（fail-fast，spec §8）；空串完全禁用。退出顺序：watcher 停 → SOCKS 关 → admin Shutdown(1s) → tailcat clients 关（spec §10）。

**Files:**
- Modify: `src/main.go`
- Create: `src/main_test.go`（新文件，只测 resolveSetting）

- [ ] **Step 7.1 写失败测试 `src/main_test.go`**：

```go
package main

import (
	"os"
	"testing"
)

func TestResolveSetting(t *testing.T) {
	const env = "TAILCAT_SOCKS_TEST_RESOLVE"
	t.Cleanup(func() { os.Unsetenv(env) })

	// 什么都没设：默认值
	if got := resolveSetting(false, env, "default"); got != "default" {
		t.Errorf("no flag, no env: got %q, want default", got)
	}

	// env 覆盖默认
	os.Setenv(env, "fromenv")
	if got := resolveSetting(false, env, "default"); got != "fromenv" {
		t.Errorf("env should beat default, got %q", got)
	}

	// 显式 flag 压过 env——哪怕值恰好等于默认值
	if got := resolveSetting(true, env, "fromflag"); got != "fromflag" {
		t.Errorf("explicit flag should beat env, got %q", got)
	}
	if got := resolveSetting(true, env, "default"); got != "default" {
		t.Errorf("explicit flag at default value should still beat env, got %q", got)
	}

	// 显式空串 flag：--admin-addr="" 禁用 admin
	if got := resolveSetting(true, env, ""); got != "" {
		t.Errorf("explicit empty flag must stay empty, got %q", got)
	}

	// env 设成空串同样算"已设置"——ADMIN_ADDR="" 禁用
	os.Setenv(env, "")
	if got := resolveSetting(false, env, "default"); got != "" {
		t.Errorf(`ADMIN_ADDR="" must disable (got %q)`, got)
	}
}
```

- [ ] **Step 7.2 运行确认失败**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=https://goproxy.cn,direct go test ./...
  ```
  Expected: FAIL — `undefined: resolveSetting`。

- [ ] **Step 7.3 修改 `src/main.go`**，五处：

a) import 块加 `"net/http"` 与 `"time"`。

b) `main()` 的 flag 块与 `os.Exit` 整体替换为：

```go
	var (
		listen        = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (host:port; empty host binds all interfaces)")
		dnsFile       = flag.String("dns-file", "data/dns.txt", "domain->token mapping file")
		clientKeyFile = flag.String("client-key-file", "data/tailcat-client-key", "persistent tailcat client identity (JSON)")
		adminAddr     = flag.String("admin-addr", "127.0.0.1:8080", "admin page/API address (host:port; empty disables)")
		noWatch       = flag.Bool("no-watch", false, "disable dns.txt hot-reload")
	)
	flag.Parse()
	// flag.Visit reports only flags actually set on the command line, so an
	// explicit flag keeps priority even when its value equals the default.
	// Env fallbacks: TAILCAT_CLIENT_KEY_FILE, ADMIN_ADDR ("" disables).
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	os.Exit(run(*listen, *dnsFile,
		resolveSetting(explicit["client-key-file"], "TAILCAT_CLIENT_KEY_FILE", *clientKeyFile),
		resolveSetting(explicit["admin-addr"], "ADMIN_ADDR", *adminAddr),
		*noWatch))
```

c) `run` 签名替换为：

```go
func run(listen, dnsFile, clientKeyFile, adminAddr string, noWatch bool) int {
```

d) `if !noWatch {` 之前插入 admin 接线块（`registry`/`dnsMap`/`dnsFile` 均已在作用域内）：

```go
	// Admin page (spec §8): an empty addr disables it entirely; a failed
	// bind is fail-fast — a dead admin means the config/deploy is broken,
	// and silently running without the page is worse.
	var adminSrv *http.Server
	if adminAddr != "" {
		api := NewAdmin(registry, dnsMap, dnsFile)
		adminLn, err := net.Listen("tcp", adminAddr)
		if err != nil {
			slog.Error("cannot listen admin", "addr", adminAddr, "err", err)
			srv.Close()
			return 1
		}
		adminSrv = &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server error", "err", err)
			}
		}()
		slog.Info("admin listening", "url", "http://"+adminAddr+"/")
	}
```

e) 收尾三连 `cancel() / srv.Close() / dialer.Close()` 整体替换为（并在 `return code` 之前保持原位）：

```go
	cancel()    // stops the dns watcher
	srv.Close() // stop accepting; in-flight relays end on their own
	if adminSrv != nil {
		// Graceful: finish in-flight requests, hard-stop after 1s
		// (spec §10). context.Background: notifyCtx is already canceled.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutCancel()
		if err := adminSrv.Shutdown(shutCtx); err != nil {
			slog.Error("admin shutdown", "err", err)
		}
	}
	dialer.Close() // tailcat clients go last (spec §10 shutdown order)
	return code
```

f) `run` 之后新增函数：

```go
// resolveSetting implements the flag > env > default precedence used by
// every env-configurable knob. `explicit` comes from flag.Visit, so a flag
// set to its default value on the command line still wins; LookupEnv (not
// Getenv) makes an empty env value count as set — ADMIN_ADDR="" disables.
func resolveSetting(explicit bool, envName, flagVal string) string {
	if explicit {
		return flagVal
	}
	if v, ok := os.LookupEnv(envName); ok {
		return v
	}
	return flagVal
}
```

- [ ] **Step 7.4 全量测试 + race + vet**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks/src
  GOPROXY=https://goproxy.cn,direct go test ./... -race
  GOPROXY=https://goproxy.cn,direct go vet ./...
  ```
  Expected: 全部 PASS、vet 无输出。

- [ ] **Step 7.5 本地冒烟**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  GOPROXY=https://goproxy.cn,direct go build -o bin/tailcat-socks ./src
  ./bin/tailcat-socks --dns-file data/dns.txt.example --client-key-file /data/yaofeng/tmp/tc-smoke-key --admin-addr 127.0.0.1:18080 &
  sleep 1
  curl -fsS http://127.0.0.1:18080/api/connections | head -c 400; echo
  curl -fsS http://127.0.0.1:18080/ -o /dev/null -w 'index %{http_code}\n'
  ADMIN_ADDR=127.0.0.1:18081 ./bin/tailcat-socks --dns-file data/dns.txt.example --client-key-file /data/yaofeng/tmp/tc-smoke-key2 &
  sleep 1
  curl -fsS http://127.0.0.1:18081/api/dns -o /dev/null -w 'env %{http_code}\n'
  kill %1 %2
  ```
  Expected: connections JSON（`"active":[]`）、`index 200`、`env 200`。若本机没有可写的 /data/yaofeng/tmp，先 `mkdir -p`。（key 文件放在仓库外，避免污染 git status。）

- [ ] **Step 7.6 提交**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add src/main.go src/main_test.go
  git commit -m "feat(main): wire admin server with flag>env>default precedence

--admin-addr (env ADMIN_ADDR, empty disables) serves the embedded page
and JSON API; --client-key-file gains TAILCAT_CLIENT_KEY_FILE. Binding
admin fails fast with ERROR + exit 1; shutdown order becomes SOCKS
listener, in-flight relays, admin Shutdown(1s), tailcat clients.
resolveSetting keeps an explicitly-set flag winning even at its default
value, and treats an empty env value as set.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```

---

### Task 8: 部署面与文档（Dockerfile / compose / start.sh / .dockerignore / README）

库化后镜像里不再有第二个可执行文件：删 `go install tailcat` 层与 `COPY --from=build /go/bin/tailcat`、删 `UPSTREAM`/`TAILCAT_BIN` env 与 CMD flag；新增 `--admin-addr`（容器内默认 127.0.0.1:8080，不对宿主机暴露）与 client key 的持久卷。

**Files:**
- Rewrite: `docker/Dockerfile`、`docker/docker-compose.yml`、`README.md`
- Modify: `bin/start.sh`、`.dockerignore`

- [ ] **Step 8.1 整体替换 `docker/Dockerfile`**：

```dockerfile
# ---- build stage: compile the Go proxy (tailcat is a library dependency now) ----
FROM golang:1.27.1-bookworm AS build

# Dependency-heavy layers first: the big tailcat/tailscale module set downloads
# from go.sum and must not re-run when proxy sources change. GOPROXY is
# pre-approved for this network (proxy.golang.org unreachable). CGO off keeps
# the binary static so the slim runtime stage needs nothing else.
ENV GOPROXY=https://goproxy.cn,direct \
    CGO_ENABLED=0

WORKDIR /build
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/tailcat-socks .

# ---- runtime stage ----
# Alpine, not debian-slim: the binary is fully static (CGO off), so the
# 116MB glibc base buys nothing; alpine's sh keeps the env-expanding CMD intact.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates

COPY --from=build /out/tailcat-socks /usr/local/bin/tailcat-socks

WORKDIR /app
# dns.txt holds real tokens and is git-ignored; bake the example as a build
# fallback (compose mounts your real dns.txt over it at runtime).
COPY data/dns.txt.example /app/dns.txt

ENV LISTEN=0.0.0.0:1080 \
    DNS_FILE=/app/dns.txt \
    TAILCAT_CLIENT_KEY_FILE=/data/tailcat-client-key \
    ADMIN_ADDR=127.0.0.1:8080

# 1080 = SOCKS5 to clients. Admin stays container-internal (127.0.0.1) unless
# you deliberately expose it; see docker-compose.yml.
EXPOSE 1080
VOLUME ["/data"]

# exec so PID 1 forwards signals -> proxy runs its shutdown order
# (SOCKS listener, relays, admin Shutdown(1s), tailcat clients).
CMD ["sh", "-c", "exec tailcat-socks --listen \"$LISTEN\" --dns-file \"$DNS_FILE\" --client-key-file \"$TAILCAT_CLIENT_KEY_FILE\" --admin-addr \"$ADMIN_ADDR\""]
```

- [ ] **Step 8.2 整体替换 `docker/docker-compose.yml`**：

```yaml
services:
  tailcat-socks:
    build:
      context: ..                       # repo root so COPY src/ + data/dns.txt.example resolve
      dockerfile: docker/Dockerfile
    image: yaofeng928/tailcat-socks:latest
    container_name: tailcat-socks
    # Mount the CONTAINING DIRECTORY (not the file): editors replace files on
    # save, and a file-level bind-mount pins the old inode so hot-reload would
    # miss it. A directory mount tracks the rename, so the proxy sees edits.
    volumes:
      - ../:/app/config:ro
      - tailcat-client-key:/data        # persistent tailcat client identity (writable)
    ports:
      - "1080:1080"                     # SOCKS5 for clients: --socks5-hostname <host>:1080
      # - "8080:8080"                   # admin page from host: ALSO uncomment ADMIN_ADDR below
    environment:
      LISTEN: "0.0.0.0:1080"            # bind all ifaces inside the container
      DNS_FILE: "/app/config/data/dns.txt"  # live-mounted, hot-reloaded
      # Admin defaults to 127.0.0.1:8080 INSIDE the container (unreachable
      # from the host, on purpose: the page has NO auth). To open it:
      # uncomment BOTH this line and the ports mapping above.
      # ADMIN_ADDR: "0.0.0.0:8080"
    restart: unless-stopped
    # tailcat needs to reach DERP/STUN over the network; give it outbound access.

volumes:
  tailcat-client-key:
```

- [ ] **Step 8.3 修改 `bin/start.sh`**，三处：

a) 头部注释第 3–7 行的配置说明替换为：

```bash
# Start tailcat-socks in the background.
# Config via env (all optional):
#   LISTEN=127.0.0.1:1080  DNS_FILE=<root>/data/dns.txt  PROXY_ARGS="extra flags"
# The binary itself reads ADMIN_ADDR and TAILCAT_CLIENT_KEY_FILE (flag > env >
# default), so just export them; PROXY_ARGS can also pass --admin-addr.
# PROXY_ARGS is word-split and glob-expanded by the shell (intended for
# multiple flags); avoid spaces inside individual flag values.
```

b) 默认值块中的 `UPSTREAM`/`TAILCAT_BIN` 两行删除（只保留）：

```bash
LISTEN="${LISTEN:-127.0.0.1:1080}"
DNS_FILE="${DNS_FILE:-$ROOT/data/dns.txt}"
```

c) `ARGS=(...)` 行替换为（admin/key 不再透传——二进制自己按 flag>env>默认解析）：

```bash
ARGS=(--listen "$LISTEN" --dns-file "$DNS_FILE")
```

- [ ] **Step 8.4 修改 `.dockerignore`**：删除 `src/testdata/` 一行（目录已不存在），结果：

```
.git
run/
bin/
data/dns.txt
```

- [ ] **Step 8.5 整体替换 `README.md`**（注意：README 自身含多个 3-backtick 代码块，故本步骤外层用 4-backtick 围栏；落地时把围栏内内容原样写入文件）：

````markdown
# tailcat-socks

一个带**域名解析**的 SOCKS5 前置代理:把 `curl http://www.example.com/` 里的真实域名,按 `dns.txt` 里的映射改写成 [tailcat](https://github.com/tailscale/tailcat) 的 token(`tc...`),并在**同一进程内**用 tailcat Go 库经 WireGuard/DERP 隧道打到对应机器;**未命中映射的域名与 IP 字面量直接 TCP 拨号**。附带一个 Web 管理页(查看连接、在线编辑 dns.txt)。

你只需对一个固定的 SOCKS5 端口说人话域名,无需记 `tcXYZabc123...` 这种 token,也无需再单独运行 tailcat 可执行文件。

本仓库为 **Go 实现**(`src/`,单静态二进制;tailcat 与 fsnotify 是仅有的库依赖)。

## 它做什么

```
curl http://www.example.com:8888/health
   │  --socks5-hostname 127.0.0.1:1080
   ▼
本代理 (:1080)                          ← 你要用的端口
   ├─ 查 dns.txt: www.example.com → tcXYZabc... (改写主机名,大小写精确保留)
   ├─ 命中   → 进程内 tailcat 库 DialTCPPort → WireGuard/DERP → 那台机器:8888
   ├─ 未命中 → 直接 TCP 拨原 host:port(IP 字面量同样直连)
   └─ 每条连接计数(字节/速率),供管理页展示

管理页 (:8080,可改可关)
   ├─ 当前连接:活跃会话列表 + 汇总(速率由页面按 1s 轮询差值计算)
   └─ DNS 配置:编辑 token/domains/备注,保存即生效
```

关键设计:
- **token 只存在于 `dns.txt` 和代理内部**,从不进 URL。curl 会小写化 URL 主机名,但本设计里 curl 看到的是你给的小写域名,代理换成 `dns.txt` 里大小写混合的真实 token —— 恰好绕开 curl/浏览器小写化 token 的问题。
- **未命中直连**:dns.txt 没命中的域名和 IP 字面量由本进程直接拨号出去。旧的"未命中交给上游 tailcat socks(需 exit-node)"透传路径已随库化删除。
- **隧道 client 复用**:每个 token 一个 `tailcat.Client`(惰性创建 + 缓存);拨号失败后 client 保留(magicsock 端点缓存不丢)。隧道与直连拨号均带 15s 超时(DERP bootstrap + WireGuard 握手重传需要)。
- **客户端身份持久化**:首次启动生成 `data/tailcat-client-key`(JSON,0600)并复用;文件损坏时只 WARN 并用临时身份,绝不覆盖你的文件。

## dns.txt 格式

```
# 示例备注:生产 web 服务器
tcXYZabc123 www.example.com api.example.com
tcDEF456ghi foo.com bar.com
```

- 一行一个 token,后跟任意多个域名(空格/Tab 分隔);`#` 开头行为注释,空行忽略。
- **备注** = token 行紧邻上方的 `#` 行(多行合并为一条备注;与条目之间隔空行的注释是散注释,不算备注)。
- 一个 token 可对应多个域名;一个域名也可出现在多行(最后一条生效)。域名匹配大小写不敏感;token 原样保留大小写。
- **改存即热加载**,无需重启;在管理页保存则**立即生效**(不等防抖)。
- 页面保存会按条目重写整个文件:不属于任何条目的散注释(如示例文件头部说明)会被丢弃——**用页面管理 dns.txt 后,文件就归页面托管**。

## 依赖

- 构建只需要 Go 工具链(tailcat@v0.7.0 要求 **Go 1.27+**;Dockerfile 基于 `golang:1.27.1`)。**不再需要单独安装 tailcat 可执行文件**——它是库依赖。
- 本网络访问 proxy.golang.org 受限时用 `GOPROXY=https://goproxy.cn,direct`。

## 快速开始(裸机)

```bash
# 首次:从示例生成自己的映射文件,填入真实 token(此文件已被 git 忽略,勿提交)
cp data/dns.txt.example data/dns.txt && $EDITOR data/dns.txt

# 构建一次
(cd src && go build -o ../bin/tailcat-socks .)

# 前台运行:SOCKS5 127.0.0.1:1080,管理页 http://127.0.0.1:8080/
bin/tailcat-socks

# 用起来
curl --socks5-hostname 127.0.0.1:1080 http://www.example.com:8888/health
# 或给整个进程注入
export all_proxy=socks5h://127.0.0.1:1080
```

### 命令行参数

| 参数 | 默认 | 环境变量回退 | 说明 |
|---|---|---|---|
| `--listen` | `127.0.0.1:1080` | – | 面向客户端的 SOCKS5 监听地址 |
| `--dns-file` | `data/dns.txt` | – | 域名→token 映射文件 |
| `--client-key-file` | `data/tailcat-client-key` | `TAILCAT_CLIENT_KEY_FILE` | tailcat 客户端身份文件(JSON) |
| `--admin-addr` | `127.0.0.1:8080` | `ADMIN_ADDR` | 管理页/API 监听地址;**空串完全禁用** |
| `--no-watch` | 关 | – | 关闭 dns.txt 热加载 |

优先级:**命令行 > 环境变量 > 默认值**。env 只在对应 flag 未显式给出时生效;把 env 设为空串也算"已设置"(用于 `ADMIN_ADDR=""` 禁用页面)。

## Web 管理页

- **当前连接**:汇总条(活跃数、累计会话、上/下行合计、聚合速率)+ 每条连接:原始域名、目标(token 或直连 host)、端口、tunnel/direct 徽标、状态(handshake/open)、客户端地址、时长、上/下行字节与速率。速率由页面对相邻两次 1s 采样求差得到;已关闭的连接不保留历史。
- **DNS 配置**:表格编辑 token / domains(逗号或空格分隔)/ 备注;"保存并生效"经服务端校验 → 原子落盘 → 立即载入映射,失败会给出条目级原因且文件与映射都不动。
- 页面**无鉴权**,默认只绑 127.0.0.1,信任本机;要对宿主机/网络开放请自行加反代或防火墙(见 Docker 一节)。

## 启停脚本(bin/)

后台运行,pid 与 log 在 `run/tailcat-socks-go.{pid,log}`:

```bash
bin/start.sh          # 后台启动(缺二进制时自动 go build)
bin/stop.sh           # 停止
bin/restart.sh        # 重启
tail -f run/tailcat-socks-go.log
```

脚本只停止确属本代理的进程(校验 /proc pid 归属);start 后若进程秒退(如 dns 文件不存在、端口被占)会直接报错并以非零码退出,并打印日志尾部。

脚本自身认的环境变量:`LISTEN`、`DNS_FILE`、`PROXY_ARGS`(经 shell 分词,适合放多个额外 flag);`ADMIN_ADDR`、`TAILCAT_CLIENT_KEY_FILE` 由二进制直接读取。例:`LISTEN=127.0.0.1:1080 ADMIN_ADDR=127.0.0.1:8080 bin/start.sh`

## Docker

```bash
cd docker
docker compose up --build -d       # 构建并后台启动,暴露 1080
docker compose logs -f
docker compose down
```

- compose 把仓库根目录只读挂进容器 `/app/config`,`DNS_FILE=/app/config/data/dns.txt`。改宿主机 `data/dns.txt` 即热加载(挂目录而非单文件,规避编辑器 rename 换 inode 导致的热加载失效)。
- 容器内 `LISTEN=0.0.0.0:1080`;管理页默认 `127.0.0.1:8080`(**容器内**,宿主机访问不到)。要开放:同时取消 `ADMIN_ADDR: "0.0.0.0:8080"` 与 `8080:8080` 端口映射的注释——注意这等于把无鉴权页面暴露给宿主机网络。
- tailcat 客户端身份存在命名卷 `tailcat-client-key:/data`,容器重建不丢身份。
- 容器需能出网(DERP/STUN)。
- 镜像 runtime 为 `alpine:3.22`(全静态二进制不需要 glibc),**只含一个可执行文件**(tailcat 是库依赖);已发布为 `yaofeng928/tailcat-socks:latest`。
- 手动构建(等价于 compose):在仓库根目录 `docker build -f docker/Dockerfile -t tailcat-socks .`——context 必须是仓库根,不要写成 `-f docker/Dockerfile ..`(那会把父目录当 context)。

## dns.txt 热加载

代理监听 `dns.txt` 变化即重新载入(原子替换内存映射):用 fsnotify 文件事件,并在每次 Create/Write 后重新挂载 watch(规避原子保存换 inode)。读取失败时保留旧映射并打警告。改完文件存盘即生效,无需重启;`--no-watch` 关闭。管理页保存走的是"写完立即重载"路径,不受 50ms 防抖影响;与外部编辑器并发是 last-writer-wins。

## 目录结构

```
tailcat-socks/
├── src/                              # Go 版(idiomatic Go;单二进制)
│   ├── main.go                       # flag/env/信号/退出顺序
│   ├── dnsmap.go                     # dns.txt 条目解析 + 原子热加载
│   ├── dialer.go                     # 路由决策 + 进程内 tailcat 库 + 直连拨号
│   ├── stats.go                      # ConnRegistry 会话表 + 原子字节计数
│   ├── proxy.go                      # SOCKS5 服务端 + relay
│   ├── admin.go                      # 管理页 HTTP API(go:embed + 单页)
│   ├── static/index.html             # 管理页(编译期内嵌)
│   └── *_test.go                     # 单元 + 本地端到端(全离线)
├── bin/                              # 启停脚本 {start,stop,restart}.sh + 构建产物 tailcat-socks(gitignore)
├── docker/{Dockerfile,docker-compose.yml}
├── docs/superpowers/                 # Go 版设计文档与实现计划
├── data/
│   ├── dns.txt                       # 域名→token 映射(运行时热加载,gitignore)
│   ├── dns.txt.example               # 示例映射
│   └── tailcat-client-key            # tailcat 客户端身份(首启自动生成,gitignore)
└── run/                              # pid + log(脚本生成)
```

## 实现范围

- **支持**: SOCKS5 `CONNECT`;IPv4/IPv6/域名 ATYP;无认证握手;未命中(含 IP)直连;进程内 tailcat 隧道与 client 复用;连接统计与管理页;dns.txt 页内编辑 + 热加载;多线程并发。
- **不支持**(YAGNI): `UDP ASSOCIATE` / `BIND`(返回失败);用户名/密码认证;Web 鉴权;已关闭连接的历史/图表;exit-node 透传(由直连语义替代)。

## 测试

```bash
cd src && go test ./...             # 全离线:本地回环 + 注入 fake,无外网依赖
```

覆盖 dns.txt 条目解析/校验/原子保存、路由决策、注册表计数、SOCKS5 改写与 relay(含半关不 RST)、admin API 契约。真实隧道 e2e 依赖外网 DERP,手工清单:起服务 → 配一个可达 token → `curl --socks5-hostname` 打过去 → 管理页出现该连接且字节/速率增长 → 页内改映射立即生效。
````

- [ ] **Step 8.6 冒烟**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  GOPROXY=https://goproxy.cn,direct go build -o bin/tailcat-socks ./src
  bash -n bin/start.sh
  ./bin/tailcat-socks --listen 127.0.0.1:11081 --dns-file data/dns.txt.example \
      --client-key-file /data/yaofeng/tmp/tc-smoke-key4 --admin-addr "" &
  sleep 1
  curl -sS -m 2 -o /dev/null http://127.0.0.1:8080/api/connections \
      && echo "UNEXPECTED: admin running" || echo "admin disabled as expected"
  kill %1 2>/dev/null
  ```
  Expected: `admin disabled as expected`。（有 Docker daemon 且可出网时再加 `docker build -f docker/Dockerfile -t tailcat-socks-smoke .`，Expected: 构建成功；`docker compose -f docker/docker-compose.yml config` 校验 compose 语法。）

- [ ] **Step 8.7 提交**：

  ```bash
  cd /data/yaofeng/workspace/popeye/tailcat-socks
  git add docker/ bin/start.sh .dockerignore README.md
  git commit -m "deploy+docs: single static binary; admin addr; key volume

Dockerfile drops the tailcat install layer and child-binary COPY (the
library is compiled in), serves the page on container-internal
127.0.0.1:8080 by default, and persists the client key in a named
volume; compose documents how to expose admin deliberately. start.sh
forwards only listen/dns-file (ADMIN_ADDR and the key path are read by
the binary itself). README rewritten for library mode, direct dial and
the admin page.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
  ```
