# SOCKS5 认证授权 + SQLite 历史记录 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 tailcat-socks 加上 SOCKS5 用户名/密码认证与按用户的目标授权，并把每次连接按（日期, 客户端 IP, 域名, 端口）聚合写入 SQLite，管理页新增「历史连接」标签查看。

**Architecture:** 新增 `config.go`（YAML 配置加载 + 默认文件生成）、`auth.go`（用户表与常量时间密码校验）、`history.go`（SQLite 打开、建表、upsert、按日查询）。`proxy.go` 的 `serveConn` 从「无认证」改为 RFC1929 子协商，路由后按用户名做授权（`tailcat` 只许隧道目标，`all` 不限）。`admin.go` 加 `GET /api/history`，`static/index.html` 加第三个标签。历史写入挂在 `serveConn` 的 defer，连接结束写一次。

**Tech Stack:** Go 1.27.1、`gopkg.in/yaml.v3`（已缓存）、`modernc.org/sqlite`（纯 Go，保持 CGO_ENABLED=0）、`net/http` httptest、`database/sql`。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `src/config.go`（新建） | `Config` 类型、`LoadConfig`、`configTemplate`、默认文件生成 |
| `src/config_test.go`（新建） | 配置解析/默认文件/错误路径 |
| `src/auth.go`（新建） | `Users` 类型、`Authenticate`（常量时间比较）、`Allows`（按目标授权） |
| `src/auth_test.go`（新建） | 认证与授权矩阵 |
| `src/history.go`（新建） | `History` 类型：`OpenHistory`、`Record`、`Query(date)`、`Close` |
| `src/history_test.go`（新建） | 建表/upsert 累加/按日过滤/倒序/失败计数 |
| `src/proxy.go`（修改） | RFC1929 子协商、授权检查、`socksReplyDenied`、defer 里 `Record` |
| `src/proxy_test.go`（修改） | 认证成功/失败、无 0x02、授权矩阵、失败不建 Session |
| `src/admin.go`（修改） | `history` 字段、`GET /api/history`、`historyJSON` DTO |
| `src/admin_test.go`（修改） | `/api/history` 契约 |
| `src/main.go`（修改） | `--config` flag、加载配置、构造 Users/History、传入 Server 与 Admin、退出时 Close |
| `src/static/index.html`（修改） | 第三个标签「历史连接」+ 日期筛选 + 表格 |
| `.version`（修改） | 0.9.0 → 0.10.0 |
| `.gitignore` / `.dockerignore`（修改） | 忽略 `data/config.yaml`、`data/history.db*` |
| `data/config.yaml.example`（新建） | 示例配置 |
| `README.md`（修改） | 配置、认证、历史三节 |

---

### Task 1: 配置加载（config.go）

**Files:**
- Create: `src/config.go`
- Create: `src/config_test.go`
- Modify: `src/go.mod`、`src/go.sum`（新增 `gopkg.in/yaml.v3`）

- [ ] **Step 1.1 拉依赖**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src
GOPROXY=https://goproxy.cn,direct go get gopkg.in/yaml.v3@v3.0.1
GOPROXY=https://goproxy.cn,direct go mod tidy
```

Expected: `go.mod` 的 require 块出现 `gopkg.in/yaml.v3 v3.0.1`；`go build ./...` 无输出。

- [ ] **Step 1.2 写失败测试 `src/config_test.go`**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigParsesUsersAndHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `socks:
  users:
    tailcat: "pw1"
    all: "pw2"
history:
  enabled: true
  path: "` + filepath.Join(dir, "h.db") + `"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Socks.Users["tailcat"]; got != "pw1" {
		t.Errorf("tailcat password = %q, want pw1", got)
	}
	if got := cfg.Socks.Users["all"]; got != "pw2" {
		t.Errorf("all password = %q, want pw2", got)
	}
	if !cfg.History.Enabled {
		t.Error("history.enabled should be true")
	}
	if cfg.History.Path != filepath.Join(dir, "h.db") {
		t.Errorf("history.path = %q", cfg.History.Path)
	}
}

// 文件不存在时生成默认配置并返回默认值——认证是强制项，不能因为缺文件就
// 变成"谁都能连"，也不能直接拒绝启动。
func TestLoadConfigCreatesDefaultWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on missing file: %v", err)
	}
	if cfg.Socks.Users["tailcat"] != "changeme" {
		t.Errorf("default tailcat password = %q, want changeme", cfg.Socks.Users["tailcat"])
	}
	if cfg.Socks.Users["all"] != "changeme" {
		t.Errorf("default all password = %q, want changeme", cfg.Socks.Users["all"])
	}
	// 文件必须真的落盘，否则用户的修改会在下次启动时被"重新生成"覆盖。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config was not written: %v", err)
	}
	// 再读一次必须得到同样的内容（round-trip）。
	again, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.Socks.Users["tailcat"] != "changeme" {
		t.Errorf("reloaded default = %q", again.Socks.Users["tailcat"])
	}
}

func TestLoadConfigRejectsEmptyUsers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("socks:\n  users: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("empty users must be an error: authentication is mandatory")
	}
}

func TestLoadConfigRejectsMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("socks: [not a map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("malformed YAML must be an error, never silently guessed")
	}
}
```

- [ ] **Step 1.3 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestLoadConfig'
```

Expected: FAIL — `undefined: LoadConfig`。

- [ ] **Step 1.4 实现 `src/config.go`**

```go
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// configTemplate 是 config.yaml 缺失时落盘的模板。密码给 changeme 只是为了让
// 首次启动能跑起来；启动时会 WARN 提示修改。两个用户同名密码是刻意的：
// 默认配置只是占位，真实部署必须改。
const configTemplate = `# tailcat-socks 配置。此文件含明文密码，权限应为 0600。
socks:
  users:
    # tailcat: 只能访问 dns.txt 命中的域名（走隧道）
    tailcat: "changeme"
    # all: 可访问任意目标（命中走隧道，未命中直连）
    all: "changeme"

history:
  enabled: true
  path: "data/history.db"
`

// Config 是 data/config.yaml 的映射。目前只有 SOCKS 用户与历史记录两组；
// 新增配置项时同时更新 configTemplate，否则新装用户拿不到默认值。
type Config struct {
	Socks struct {
		Users map[string]string `yaml:"users"`
	} `yaml:"socks"`
	History struct {
		Enabled bool   `yaml:"enabled"`
		Path    string `yaml:"path"`
	} `yaml:"history"`
}

// LoadConfig 读取并解析 path。文件不存在时写出一份模板并返回其解析结果，
// 让首次启动可用；解析失败或用户表为空则返回错误——认证是强制项，空用户
// 表等于谁都不能连，这种配置只能是笔误，不该被静默接受。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if werr := os.WriteFile(path, []byte(configTemplate), 0o600); werr != nil {
			return nil, fmt.Errorf("write default config %s: %w", path, werr)
		}
		slogWarnDefaultConfig(path)
		data = []byte(configTemplate)
	} else if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cfg.Socks.Users) == 0 {
		return nil, fmt.Errorf("config %s: socks.users is empty; at least one user is required (authentication is mandatory)", path)
	}
	return &cfg, nil
}
```

`config.go` 还需要 `slogWarnDefaultConfig`。为避免与 `config_test.go` 的断言顺序纠缠，直接在同文件定义：

```go
func slogWarnDefaultConfig(path string) {
	slog.Warn("generated default config with placeholder passwords; edit it before exposing this proxy", "path", path)
}
```

并在 import 中加入 `"log/slog"`。

- [ ] **Step 1.5 运行确认通过**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestLoadConfig' -v
```

Expected: 4 个测试全部 PASS。

- [ ] **Step 1.6 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/config.go src/config_test.go src/go.mod src/go.sum
git commit -m "feat(config): load data/config.yaml, generating defaults on first run

socks.users is mandatory — an empty table means nobody can connect, so it
is rejected rather than silently accepted. A missing file is written with
placeholder passwords (changeme) and a WARN, so first start works and the
user is told to change them.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: 认证与授权（auth.go）

**Files:**
- Create: `src/auth.go`
- Create: `src/auth_test.go`

- [ ] **Step 2.1 写失败测试 `src/auth_test.go`**

```go
package main

import "testing"

func testUsers() Users {
	return Users{
		"tailcat": {Password: "pw1", Mode: ModeMatchedOnly},
		"all":     {Password: "pw2", Mode: ModeAny},
	}
}

func TestAuthenticate(t *testing.T) {
	u := testUsers()
	tests := []struct {
		name, user, pass string
		want             bool
	}{
		{"tailcat ok", "tailcat", "pw1", true},
		{"all ok", "all", "pw2", true},
		{"wrong password", "tailcat", "nope", false},
		{"unknown user", "ghost", "pw1", false},
		{"empty password", "tailcat", "", false},
		{"empty user", "", "pw1", false},
		// 密码是区分大小写的：这是 HTTP 代理凭据，不是用户名。
		{"case sensitive password", "tailcat", "PW1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := u.Authenticate(tt.user, tt.pass)
			if ok != tt.want {
				t.Fatalf("Authenticate(%q,%q) ok = %v, want %v", tt.user, tt.pass, ok, tt.want)
			}
			if ok && got.Mode == "" {
				t.Error("authenticated user must carry a Mode")
			}
		})
	}
}

// 一个用户的密码不能登进另一个用户——这是最容易被表实现写错的地方。
func TestAuthenticateDoesNotCrossUsers(t *testing.T) {
	u := testUsers()
	if _, ok := u.Authenticate("all", "pw1"); ok {
		t.Error("all must not accept tailcat's password")
	}
	if _, ok := u.Authenticate("tailcat", "pw2"); ok {
		t.Error("tailcat must not accept all's password")
	}
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name  string
		mode  Mode
		via   string
		want  bool
	}{
		{"matched-only allows tunnel", ModeMatchedOnly, ViaTunnel, true},
		{"matched-only rejects direct", ModeMatchedOnly, ViaDirect, false},
		{"any allows tunnel", ModeAny, ViaTunnel, true},
		{"any allows direct", ModeAny, ViaDirect, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.Allows(tt.via); got != tt.want {
				t.Errorf("%s.Allows(%s) = %v, want %v", tt.mode, tt.via, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2.2 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestAuthenticate|TestAllows'
```

Expected: FAIL — `undefined: Users`、`undefined: ModeMatchedOnly` 等。

- [ ] **Step 2.3 实现 `src/auth.go`**

```go
package main

import "crypto/subtle"

// Mode 是一个用户的授权范围。用户只能在自己模式允许的目标上建立连接。
type Mode string

const (
	// ModeMatchedOnly：只允许 dns.txt 命中的目标（即走隧道的那些）。
	ModeMatchedOnly Mode = "matched-only"
	// ModeAny：任何目标——命中走隧道，未命中直连。
	ModeAny Mode = "any"
)

// User 是 config.yaml 里的一个 SOCKS5 用户。
type User struct {
	Password string
	Mode     Mode
}

// Users 是用户名到用户的映射。构建后只读，可安全并发使用。
type Users map[string]User

// modeForUser 把配置里的用户名翻译成授权模式。用户名是策略的一部分而不是
// 自由标签：tailcat 意味着"只走命中隧道"，其余名字按 all 处理——这样配置
// 里写错用户名时不会意外放大权限，而是退化成最宽但显式的 any。
//
// 注意：这里刻意不按配置里出现的顺序猜。只有 "tailcat" 收窄，其他一律 any。
func modeForUser(name string) Mode {
	if name == "tailcat" {
		return ModeMatchedOnly
	}
	return ModeAny
}

// Authenticate 校验 user/password，成功时返回该用户与其模式。密码用常量
// 时间比较，避免通过响应时间逐字节猜测——代理端口可能暴露在网络上。
// 用户不存在时也与错误密码走同样多的比较工作（用一个哑用户占位）。
func (u Users) Authenticate(user, password string) (User, bool) {
	dummy := User{Password: "\x00invalid\x00", Mode: ModeAny}
	got, ok := u[user]
	if !ok {
		got = dummy
	}
	// 无论用户是否存在都执行一次比较，保持时序一致。
	match := subtle.ConstantTimeCompare([]byte(got.Password), []byte(password)) == 1
	if !ok || !match {
		return User{}, false
	}
	return got, true
}

// Allows 报告该模式是否允许走 via 这条通道。授权在路由之后判断，所以这里
// 看的是路由结果（ViaTunnel / ViaDirect），而不是再做一次 DNS 查询。
func (m Mode) Allows(via string) bool {
	switch m {
	case ModeMatchedOnly:
		return via == ViaTunnel
	case ModeAny:
		return true
	}
	return false
}
```

- [ ] **Step 2.4 运行确认通过**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestAuthenticate|TestAllows' -v
```

Expected: 全部 PASS。

- [ ] **Step 2.5 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/auth.go src/auth_test.go
git commit -m "feat(auth): constant-time SOCKS5 credentials with per-user reach

tailcat is matched-only (tunnel targets); every other name is 'any'.
Authorization keys off the routing result (tunnel vs direct) rather than
re-querying dns.txt, so the check cannot disagree with the dial that
follows. Password comparison is constant-time and does equal work for
unknown users.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: SQLite 历史存储（history.go）

**Files:**
- Create: `src/history.go`
- Create: `src/history_test.go`
- Modify: `src/go.mod`、`src/go.sum`

- [ ] **Step 3.1 拉依赖**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src
GOPROXY=https://goproxy.cn,direct go get modernc.org/sqlite@v1.34.5
GOPROXY=https://goproxy.cn,direct go mod tidy
GOPROXY=https://goproxy.cn,direct go build ./...
```

Expected: `go.mod` 出现 `modernc.org/sqlite v1.34.5`；`go build ./...` 无输出。若该版本在 Go 1.27.1 下编译失败，改用 `go get modernc.org/sqlite@latest` 并把版本号记进提交信息。

- [ ] **Step 3.2 写失败测试 `src/history_test.go`**

```go
package main

import (
	"path/filepath"
	"testing"
)

func testHistory(t *testing.T) *History {
	t.Helper()
	h, err := OpenHistory(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistory: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestHistoryAggregatesSameKey(t *testing.T) {
	h := testHistory(t)
	day := "2026-11-14"

	h.Record(day, "172.16.2.233", "a.example.com", 443, false, 100, 200)
	h.Record(day, "172.16.2.233", "a.example.com", 443, false, 1, 2)
	h.Record(day, "172.16.2.233", "a.example.com", 443, true, 0, 0)

	entries, totals, err := h.Query(day)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("same (date,ip,host,port) must collapse to one row, got %d", len(entries))
	}
	e := entries[0]
	if e.Count != 3 {
		t.Errorf("count = %d, want 3 (failures also count as an attempt)", e.Count)
	}
	if e.Failures != 1 {
		t.Errorf("failures = %d, want 1", e.Failures)
	}
	if e.BytesUp != 101 || e.BytesDown != 202 {
		t.Errorf("bytes = %d/%d, want 101/202", e.BytesUp, e.BytesDown)
	}
	if totals.Count != 3 || totals.BytesUp != 101 || totals.BytesDown != 202 {
		t.Errorf("totals = %+v", totals)
	}
}

func TestHistorySeparatesDatesAndClients(t *testing.T) {
	h := testHistory(t)
	h.Record("2026-11-13", "1.1.1.1", "a.example.com", 443, false, 10, 10)
	h.Record("2026-11-14", "1.1.1.1", "a.example.com", 443, false, 20, 20)
	h.Record("2026-11-14", "2.2.2.2", "a.example.com", 443, false, 30, 30)

	entries, _, err := h.Query("2026-11-14")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 rows for 2026-11-14, got %d", len(entries))
	}
	// 按累计流量倒序：2.2.2.2(60) 应排在 1.1.1.1(40) 之前。
	if entries[0].ClientIP != "2.2.2.2" {
		t.Errorf("first row = %s, want 2.2.2.2 (highest total bytes)", entries[0].ClientIP)
	}
}

func TestHistoryOrdersByTotalBytesDesc(t *testing.T) {
	h := testHistory(t)
	day := "2026-11-14"
	h.Record(day, "small", "a.example.com", 1, false, 1, 1)
	h.Record(day, "big", "a.example.com", 1, false, 1000, 1000)
	h.Record(day, "mid", "a.example.com", 1, false, 100, 100)

	entries, _, err := h.Query(day)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"big", "mid", "small"}
	for i, w := range want {
		if entries[i].ClientIP != w {
			t.Errorf("row %d = %s, want %s", i, entries[i].ClientIP, w)
		}
	}
}

func TestHistoryEmptyDay(t *testing.T) {
	h := testHistory(t)
	entries, totals, err := h.Query("2020-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || totals.Count != 0 {
		t.Errorf("empty day should be empty, got %d rows, totals %+v", len(entries), totals)
	}
}

// 幂等：同一个库重复打开不应炸，也不应重建表。
func TestOpenHistoryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h1, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	h1.Record("2026-11-14", "1.1.1.1", "a.example.com", 443, false, 5, 5)
	h1.Close()

	h2, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	entries, _, err := h2.Query("2026-11-14")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].BytesUp != 5 {
		t.Errorf("data did not survive reopen: %+v", entries)
	}
}

// Record 的 day 参数由调用方给，这里验证它不被本地时区二次转换。
func TestHistoryKeepsGivenDateVerbatim(t *testing.T) {
	h := testHistory(t)
	h.Record("1999-12-31", "1.1.1.1", "a.example.com", 443, false, 1, 1)
	if _, _, err := h.Query("1999-12-31"); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 3.3 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestHistory|TestOpenHistory'
```

Expected: FAIL — `undefined: OpenHistory`、`undefined: History`。

- [ ] **Step 3.4 实现 `src/history.go`**

```go
package main

import (
	"database/sql"
	"fmt"
	"log/slog"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册 "sqlite" 方言
)

// historySchema 建表 + 索引，全部 IF NOT EXISTS，重复打开是幂等的。
//
// 主键是自增 id（页面用它引用行），聚合靠 UNIQUE(date, client_ip, host, port)
// 定位——同一个（日期, IP, 域名, 端口）在一行里累加。date 用 DATE 亲和性存
// 'YYYY-MM-DD' 字符串：SQLite 没有真正的日期类型，DATE 让 schema 语义清晰，
// 比较与 ORDER BY 仍按字典序（对 ISO 日期等价于时间序）。
const historySchema = `
CREATE TABLE IF NOT EXISTS history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  date        DATE    NOT NULL,
  client_ip   TEXT    NOT NULL,
  host        TEXT    NOT NULL,
  port        INTEGER NOT NULL,
  count       INTEGER NOT NULL DEFAULT 0,
  failures    INTEGER NOT NULL DEFAULT 0,
  bytes_up    INTEGER NOT NULL DEFAULT 0,
  bytes_down  INTEGER NOT NULL DEFAULT 0,
  UNIQUE (date, client_ip, host, port)
);
CREATE INDEX IF NOT EXISTS idx_history_date_bytes
  ON history(date, (bytes_up + bytes_down) DESC);
`

const historyUpsert = `
INSERT INTO history (date, client_ip, host, port, count, failures, bytes_up, bytes_down)
VALUES (?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(date, client_ip, host, port) DO UPDATE SET
  count      = count + 1,
  failures   = failures + excluded.failures,
  bytes_up   = bytes_up + excluded.bytes_up,
  bytes_down = bytes_down + excluded.bytes_down;
`

// HistoryEntry 是查询结果的一行。TotalBytes 在 SQL 里算好，页面直接排序展示。
type HistoryEntry struct {
	ID         int64  `json:"id"`
	ClientIP   string `json:"clientIp"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Count      int64  `json:"count"`
	Failures   int64  `json:"failures"`
	BytesUp    int64  `json:"bytesUp"`
	BytesDown  int64  `json:"bytesDown"`
	TotalBytes int64  `json:"totalBytes"`
}

type HistoryTotals struct {
	Entries    int   `json:"entries"`
	Count      int64 `json:"count"`
	Failures   int64 `json:"failures"`
	BytesUp    int64 `json:"bytesUp"`
	BytesDown  int64 `json:"bytesDown"`
	TotalBytes int64 `json:"totalBytes"`
}

// History 是历史库的句柄。db 为 nil 表示历史功能未启用（config 关掉或打开
// 失败），此时 Record 与 Query 都是安全的空操作——旁路功能不该让主路径出错。
type History struct {
	db *sql.DB
}

// OpenHistory 打开（必要时创建）path 处的 SQLite 库并建表。WAL 模式让读写
// 并发不互相阻塞；busy_timeout 让偶发的写锁竞争自己退避而不是立刻报错。
func OpenHistory(path string) (*History, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open history db %s: %w", path, err)
	}
	// SQLite 写入串行化；限制成单连接可避免 SQLITE_BUSY，且本场景写入频率
	// 只有"每个连接结束时一次"。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(historySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create history schema: %w", err)
	}
	return &History{db: db}, nil
}

// Record 把一次已结束的连接并入当日该（IP, 域名, 端口）的累计行。
// failed 为 true 时 failures 加一；字节数按本次会话累加。
// 写失败只 WARN：历史是旁路，不能影响代理。
func (h *History) Record(day, clientIP, host string, port int, failed bool, bytesUp, bytesDown uint64) {
	if h == nil || h.db == nil {
		return
	}
	fail := 0
	if failed {
		fail = 1
	}
	if _, err := h.db.Exec(historyUpsert, day, clientIP, host, port, fail, bytesUp, bytesDown); err != nil {
		slog.Warn("history write failed", "day", day, "host", host, "port", port, "err", err)
	}
}

// Query 返回某一天的全部聚合行与合计，按累计流量倒序（大流量在前）。
func (h *History) Query(day string) ([]HistoryEntry, HistoryTotals, error) {
	var entries []HistoryEntry
	var totals HistoryTotals
	if h == nil || h.db == nil {
		return entries, totals, nil
	}
	rows, err := h.db.Query(`
SELECT id, client_ip, host, port, count, failures, bytes_up, bytes_down,
       bytes_up + bytes_down AS total
FROM history
WHERE date = ?
ORDER BY total DESC, id ASC`, day)
	if err != nil {
		return nil, totals, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	totals.Entries = 0
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.ID, &e.ClientIP, &e.Host, &e.Port, &e.Count,
			&e.Failures, &e.BytesUp, &e.BytesDown, &e.TotalBytes); err != nil {
			return nil, totals, fmt.Errorf("scan history row: %w", err)
		}
		entries = append(entries, e)
		totals.Entries++
		totals.Count += e.Count
		totals.Failures += e.Failures
		totals.BytesUp += e.BytesUp
		totals.BytesDown += e.BytesDown
		totals.TotalBytes += e.TotalBytes
	}
	if err := rows.Err(); err != nil {
		return nil, totals, fmt.Errorf("iterate history rows: %w", err)
	}
	return entries, totals, nil
}

// Close 关掉底层连接池。nil 接收者安全，方便 defer 无条件调用。
func (h *History) Close() error {
	if h == nil || h.db == nil {
		return nil
	}
	return h.db.Close()
}
```

- [ ] **Step 3.5 运行确认通过**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestHistory|TestOpenHistory' -v
```

Expected: 6 个测试全部 PASS。

- [ ] **Step 3.6 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/history.go src/history_test.go src/go.mod src/go.sum
git commit -m "feat(history): sqlite store aggregating per day, client, host, port

Autoincrement id for page references; aggregation keyed on
UNIQUE(date, client_ip, host, port) so a repeated target folds into one
row. modernc.org/sqlite keeps CGO_ENABLED=0 and the static build intact.
A nil History is a safe no-op so the admin API and the proxy hot path
both degrade gracefully when history is disabled or the db fails to open.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: proxy.go 接入认证、授权与历史

**Files:**
- Modify: `src/proxy.go`
- Modify: `src/proxy_test.go`

- [ ] **Step 4.1 写失败测试（追加到 `src/proxy_test.go`）**

```go
// socksAuthConnect 完成 greeting（申请 0x02）+ RFC1929 子协商 + CONNECT。
// 返回连接与 CONNECT 的 reply code，调用方据此断言授权结果。
func socksAuthConnect(t *testing.T, proxyAddr, user, pass, host string, port uint16) (net.Conn, byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte{0x05, 0x01, 0x02})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		t.Fatalf("greeting reply: %v", err)
	}
	if rep[0] != 0x05 || rep[1] != 0x02 {
		c.Close()
		t.Fatalf("server must select 0x02, got %v", rep)
	}
	sub := []byte{0x01, byte(len(user))}
	sub = append(sub, user...)
	sub = append(sub, byte(len(pass)))
	sub = append(sub, pass...)
	c.Write(sub)
	auth := make([]byte, 2)
	if _, err := io.ReadFull(c, auth); err != nil {
		c.Close()
		t.Fatalf("auth reply: %v", err)
	}
	if auth[1] != 0x00 {
		c.Close()
		return nil, 0xFF // 认证失败，调用方只看 code
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	c.Write(req)
	head := make([]byte, 10)
	if _, err := io.ReadFull(c, head); err != nil {
		c.Close()
		t.Fatalf("connect reply: %v", err)
	}
	return c, head[1]
}

func authUsers() Users {
	return Users{
		"tailcat": {Password: "pw1", Mode: ModeMatchedOnly},
		"all":     {Password: "pw2", Mode: ModeAny},
	}
}

func TestSocks5RequiresUsernamePasswordMethod(t *testing.T) {
	srv := startProxy(t, map[string]string{"a.example.com": "tcAAA"}, dialerToEcho(t))
	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00}) // 只提供 no-auth
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatal(err)
	}
	if rep[0] != 0x05 || rep[1] != 0xFF {
		t.Errorf("no 0x02 offered must be refused with 0xFF, got %v", rep)
	}
}

func TestSocks5RejectsWrongPassword(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxyWithUsers(t, map[string]string{"a.example.com": "tcAAA"}, fd, authUsers())
	c, code := socksAuthConnect(t, srv.ActualAddr().String(), "tailcat", "wrong", "a.example.com", 443)
	if c != nil {
		c.Close()
	}
	if code != 0xFF {
		t.Errorf("wrong password must fail auth, got code %#x", code)
	}
	select {
	case tg := <-fd.targets:
		t.Fatalf("failed auth must never dial, but dialer saw %+v", tg)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSocks5MatchedOnlyUserRejectedOnUnmatchedHost(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxyWithUsers(t, map[string]string{"a.example.com": "tcAAA"}, fd, authUsers())
	c, code := socksAuthConnect(t, srv.ActualAddr().String(), "tailcat", "pw1", "other.com", 443)
	if c != nil {
		c.Close()
	}
	if code != 0x02 {
		t.Errorf("tailcat on unmatched host must be refused with 0x02, got %#x", code)
	}
	select {
	case tg := <-fd.targets:
		t.Fatalf("denied target must never dial, but dialer saw %+v", tg)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSocks5MatchedOnlyUserAllowedOnMatchedHost(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxyWithUsers(t, map[string]string{"a.example.com": "tcAAA"}, fd, authUsers())
	c, code := socksAuthConnect(t, srv.ActualAddr().String(), "tailcat", "pw1", "a.example.com", 443)
	if c == nil {
		t.Fatalf("matched host should connect, got code %#x", code)
	}
	defer c.Close()
	if code != 0x00 {
		t.Errorf("reply code = %#x, want 0x00", code)
	}
	if tg := nextTarget(t, fd); tg.Via != ViaTunnel {
		t.Errorf("via = %s, want tunnel", tg.Via)
	}
}

func TestSocks5AnyUserAllowedOnUnmatchedHost(t *testing.T) {
	fd := dialerToEcho(t)
	srv := startProxyWithUsers(t, map[string]string{"a.example.com": "tcAAA"}, fd, authUsers())
	c, code := socksAuthConnect(t, srv.ActualAddr().String(), "all", "pw2", "other.com", 443)
	if c == nil {
		t.Fatalf("any user should reach unmatched hosts, got code %#x", code)
	}
	defer c.Close()
	if code != 0x00 {
		t.Errorf("reply code = %#x, want 0x00", code)
	}
	if tg := nextTarget(t, fd); tg.Via != ViaDirect {
		t.Errorf("via = %s, want direct", tg.Via)
	}
}

// 授权被拒的连接不能出现在 registry 里：它从来没真正建立。
func TestSocks5DeniedConnectionIsNotRegistered(t *testing.T) {
	fd := dialerToEcho(t)
	reg := NewConnRegistry()
	srv, err := NewServerWithUsers(&DNSMap{}, fd, reg, "127.0.0.1:0", authUsers(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.DNSMap.Store(map[string]string{"a.example.com": "tcAAA"})
	defer srv.Close()
	go srv.Serve()

	c, _ := socksAuthConnect(t, srv.ActualAddr().String(), "tailcat", "pw1", "other.com", 443)
	if c != nil {
		c.Close()
	}
	time.Sleep(150 * time.Millisecond)
	if snaps, _ := reg.Snapshot(); len(snaps) != 0 {
		t.Errorf("denied connection must not be registered, got %+v", snaps)
	}
}
```

这两个 helper 也要追加：

```go
func startProxyWithUsers(t *testing.T, mapping map[string]string, d Dialer, users Users) *Server {
	t.Helper()
	dm := &DNSMap{}
	dm.Store(mapping)
	srv, err := NewServerWithUsers(dm, d, NewConnRegistry(), "127.0.0.1:0", users, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve()
	return srv
}
```

- [ ] **Step 4.2 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestSocks5Requires|TestSocks5RejectsWrong|TestSocks5MatchedOnly|TestSocks5AnyUser|TestSocks5Denied'
```

Expected: FAIL — 编译错误 `undefined: NewServerWithUsers`、`undefined: authUsers`（新测试引用了尚未实现的符号）。

- [ ] **Step 4.3 修改 `src/proxy.go`**

a) 在 `socksOKReply`/`socksFailReply` 旁新增拒绝回执：

```go
var (
	socksOKReply   = []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	socksFailReply = []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	// 0x02 = connection not allowed by ruleset（RFC 1928）：目标是策略拒绝的，
	// 不是网络不通。客户端据此区分"我没权限"和"对面挂了"。
	socksDeniedReply = []byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
)
```

b) `Server` 增加两个字段：

```go
type Server struct {
	DNSMap   *DNSMap
	Dialer   Dialer
	Registry *ConnRegistry
	// Users 是 SOCKS5 认证用户表。认证是强制的，不能为 nil。
	Users Users
	// History 可为 nil（历史关闭或打开失败），此时不记录。
	History *History
	ln       net.Listener
	relayIdle time.Duration
	handshakeTimeout time.Duration
}
```

c) 新增带认证的构造器。`NewServer` 委托给它，用一个"空用户名空密码、权限不限"的
测试用户表，这样不需要认证的老测试只需升级握手字节即可继续跑：

```go
// NewServerWithUsers 是带认证的构造器：生产路径走这个。
func NewServerWithUsers(dnsMap *DNSMap, d Dialer, reg *ConnRegistry, listen string, users Users, hist *History) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &Server{DNSMap: dnsMap, Dialer: d, Registry: reg, Users: users, History: hist,
		ln: ln, relayIdle: relayIdleTimeout, handshakeTimeout: handshakeTimeout}, nil
}

// NewServer 是 NewServerWithUsers 的无认证便捷形式，只服务于不需要认证的
// 单元测试（凭据为空字符串即通过）。生产路径必须走 NewServerWithUsers 并
// 提供从 config.yaml 构造的真实用户表——认证是强制的。
func NewServer(dnsMap *DNSMap, d Dialer, reg *ConnRegistry, listen string) (*Server, error) {
	return NewServerWithUsers(dnsMap, d, reg, listen, noAuthUsers(), nil)
}
```

在 `auth.go` 追加：

```go
// noAuthUsers 是 NewServer 使用的测试用户表：用户名与密码都为空、权限不限。
// 它只降低测试的握手负担，不是生产路径——生产必须从 config.yaml 构造用户表。
func noAuthUsers() Users {
	return Users{"": {Password: "", Mode: ModeAny}}
}
```

同时把 `proxy_test.go` 里现有的 `socksConnect` 升级为走 RFC1929（空凭据）：

```go
// socksConnect dials the proxy and completes greeting + RFC1929 + CONNECT
// using empty credentials (the table NewServer installs).
func socksConnect(t *testing.T, proxyAddr, host string, port uint16) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte{0x05, 0x01, 0x02})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		t.Fatalf("greeting reply: %v", err)
	}
	if rep[0] != 0x05 || rep[1] != 0x02 {
		c.Close()
		t.Fatalf("greeting refused: %v", rep)
	}
	c.Write([]byte{0x01, 0x00, 0x00}) // ver, ulen=0, plen=0
	auth := make([]byte, 2)
	if _, err := io.ReadFull(c, auth); err != nil {
		c.Close()
		t.Fatalf("auth reply: %v", err)
	}
	if auth[1] != 0x00 {
		c.Close()
		t.Fatalf("auth failed: %v", auth)
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
```

`TestSocks5ProxyNoAcceptableMethod` 需要改写：现在"可接受的方法"是 0x02，所以该测试要发 `{0x05,0x01,0x00}` 并期待 `0xFF`——这与新增的 `TestSocks5RequiresUsernamePasswordMethod` 重复，**删除旧测试**，保留新的。

f) `serveConn` 的 greeting 改为只接受 0x02，并加 RFC1929 子协商与授权：

```go
func (s *Server) serveConn(client net.Conn) {
	client.SetDeadline(time.Now().Add(s.handshakeTimeout))

	// --- greeting：只接受 USERNAME/PASSWORD（0x02） ---
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
	userPass := false
	for _, m := range methods {
		if m == 0x02 {
			userPass = true
			break
		}
	}
	if !userPass {
		_, _ = client.Write([]byte{0x05, 0xFF})
		return
	}
	if _, err := client.Write([]byte{0x05, 0x02}); err != nil {
		return
	}

	// --- RFC 1929 子协商 ---
	username, ok := s.readUserPassAuth(client)
	if !ok {
		return
	}
	user, authed := s.Users.Authenticate(username.user, username.pass)
	if !authed {
		_, _ = client.Write([]byte{0x01, 0x01}) // auth failure
		slog.Warn("socks auth failed", "user", username.user, "client", client.RemoteAddr().String())
		return
	}
	if _, err := client.Write([]byte{0x01, 0x00}); err != nil {
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
	// 授权在路由之后判断：用的是实际要走的通道，不会与后面的拨号分歧。
	// 被拒的连接从未建立，因此不注册 Session、不记历史。
	if !user.Mode.Allows(target.Via) {
		_, _ = client.Write(socksDeniedReply)
		slog.Warn("socks target denied by user policy", "user", username.user,
			"mode", user.Mode, "host", host, "port", port, "via", target.Via)
		return
	}

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
	client.SetDeadline(time.Now().Add(s.handshakeTimeout))
	if err != nil {
		slog.Warn("dial failed", "via", target.Via, "host", host, "port", target.Port, "err", err)
		socksFail(client)
		return
	}
	if _, err := client.Write(socksOKReply); err != nil {
		up.Close()
		return
	}
	s.Registry.MarkOpen(sess)

	client.SetDeadline(time.Time{})
	up.SetDeadline(time.Time{})
	relay(client, up, s.relayIdle, sess)
}
```

g) 新增两个小函数（放在 `readATYPAddr` 附近）：

```go
// userPassAuth 是一次 RFC 1929 子协商的结果。
type userPassAuth struct {
	user, pass string
}

// readUserPassAuth 读 RFC 1929 的 [ver=1][ulen][uname][plen][pass]。
// ver 必须是 1；ulen/plen 为 0 时仍合法（空用户名/空密码），由用户表决定
// 是否放行。任何读取失败或版本不符都返回 ok=false，调用方直接断开。
func (s *Server) readUserPassAuth(r io.Reader) (userPassAuth, bool) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return userPassAuth{}, false
	}
	if head[0] != 0x01 {
		return userPassAuth{}, false
	}
	uname := make([]byte, head[1])
	if _, err := io.ReadFull(r, uname); err != nil {
		return userPassAuth{}, false
	}
	var pl [1]byte
	if _, err := io.ReadFull(r, pl[:]); err != nil {
		return userPassAuth{}, false
	}
	pass := make([]byte, pl[0])
	if _, err := io.ReadFull(r, pass); err != nil {
		return userPassAuth{}, false
	}
	return userPassAuth{user: string(uname), pass: string(pass)}, true
}

// clientIP 从 RemoteAddr 取出纯 IP。历史按 IP 聚合而不是 IP:port——客户端
// 每次新建连接都会换源端口，带端口会让"当日累计次数"永远是 1。
func clientIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
```

- [ ] **Step 4.4 更新受影响的老测试**

`TestSocks5ProxyNoAcceptableMethod` 删除（由 `TestSocks5RequiresUsernamePasswordMethod` 取代）。运行全量测试确认无回归：

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... 2>&1 | tail -20
```

Expected: 全部 PASS。若有老测试因握手变化失败，说明该测试直接手写了 greeting 字节——把它换成 `socksConnect`。

- [ ] **Step 4.5 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/proxy.go src/proxy_test.go src/auth.go
git commit -m "feat(proxy): SOCKS5 user/pass auth, per-user reach, history recording

Greeting now offers only USERNAME/PASSWORD; clients without RFC1929 get
0xFF. Authorization runs after routing so it keys off the channel the
dial will actually use: a matched-only user hitting an unmatched host is
refused with 0x02 and never dials. Denied connections create no Session
and leave no history row. Every session that does register is folded into
the sqlite aggregate when it ends, with failures derived from !Opened.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: admin API 与历史页面

**Files:**
- Modify: `src/admin.go`
- Modify: `src/admin_test.go`
- Modify: `src/static/index.html`

- [ ] **Step 5.1 写失败测试（追加到 `src/admin_test.go`）**

```go
type clientHistoryDoc struct {
	Enabled bool            `json:"enabled"`
	Date    string          `json:"date"`
	Totals  HistoryTotals   `json:"totals"`
	Entries []HistoryEntry  `json:"entries"`
}

func TestHandleHistoryDefaultsToToday(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	h, err := OpenHistory(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	admin.history = h
	today := time.Now().Format("2006-01-02")
	h.Record(today, "1.1.1.1", "a.example.com", 443, false, 10, 20)

	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var doc clientHistoryDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Enabled || doc.Date != today {
		t.Errorf("enabled=%v date=%q, want true/%q", doc.Enabled, doc.Date, today)
	}
	if len(doc.Entries) != 1 || doc.Entries[0].TotalBytes != 30 {
		t.Errorf("entries = %+v", doc.Entries)
	}
}

func TestHandleHistoryRejectsBadDate(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history?date=11-14-2026", nil))
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleHistoryDisabled(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t) // history 保持 nil
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history?date=2026-11-14", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc clientHistoryDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Enabled {
		t.Error("nil history must report enabled=false")
	}
	if len(doc.Entries) != 0 {
		t.Errorf("disabled history must return no entries, got %+v", doc.Entries)
	}
}
```

- [ ] **Step 5.2 运行确认失败**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestHandleHistory'
```

Expected: FAIL — `admin.history` 未定义、`/api/history` 404。

- [ ] **Step 5.3 修改 `src/admin.go`**

a) `Admin` 增加 history 字段：

```go
type Admin struct {
	registry *ConnRegistry
	dnsMap   *DNSMap
	dnsPath  string
	history  *History // nil = 历史未启用
}
```

b) 构造器增加一个设置方法（保持 `NewAdmin` 签名不变，避免改一堆测试）：

```go
// SetHistory 挂上历史库。nil 表示历史未启用，API 会如实报告 enabled=false。
func (a *Admin) SetHistory(h *History) { a.history = h }
```

c) `Handler()` 增加路由：

```go
mux.HandleFunc("GET /api/history", a.handleHistory)
```

d) 追加 handler 与 DTO：

```go
type historyDoc struct {
	Enabled bool           `json:"enabled"`
	Date    string         `json:"date"`
	Totals  HistoryTotals  `json:"totals"`
	Entries []HistoryEntry `json:"entries"`
}

// handleHistory 返回某一天的聚合历史。date 缺省为今天；格式必须是
// YYYY-MM-DD，否则 400——不做"猜你意思"的宽容解析。
func (a *Admin) handleHistory(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("date")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", day); err != nil {
		httpError(w, http.StatusBadRequest, "date 需为 YYYY-MM-DD")
		return
	}
	doc := historyDoc{Enabled: a.history != nil, Date: day, Entries: []HistoryEntry{}}
	if a.history != nil {
		entries, totals, err := a.history.Query(day)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "读取历史失败: "+err.Error())
			return
		}
		if entries != nil {
			doc.Entries = entries
		}
		doc.Totals = totals
	}
	writeJSON(w, http.StatusOK, doc)
}
```

`admin.go` 的 import 需加 `"time"`。

- [ ] **Step 5.4 运行确认通过**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -run 'TestHandleHistory' -v
```

Expected: 3 个测试 PASS。

- [ ] **Step 5.5 修改 `src/static/index.html`**

a) 标签栏加第三个按钮（`<nav class="tabs">` 内，DNS 配置之前）：

```html
    <button id="tab-conn" class="active">当前连接</button>
    <button id="tab-history">历史连接</button>
    <button id="tab-dns">DNS 配置</button>
```

b) 在 `<section id="view-conn">` 之后插入历史视图：

```html
  <section id="view-history" hidden>
    <div class="bar" style="margin-top:0;margin-bottom:12px">
      <label for="history-date" class="muted">日期</label>
      <input type="date" id="history-date" class="txt" style="width:180px">
    </div>
    <div class="cards">
      <div class="card"><div class="k">条目数</div><div class="v" id="hist-entries">–</div></div>
      <div class="card"><div class="k">总次数</div><div class="v" id="hist-count">–</div></div>
      <div class="card"><div class="k">失败次数</div><div class="v" id="hist-failures">–</div></div>
      <div class="card"><div class="k">上行</div><div class="v" id="hist-up">–</div></div>
      <div class="card"><div class="k">下行</div><div class="v" id="hist-down">–</div></div>
      <div class="card"><div class="k">总流量</div><div class="v" id="hist-total">–</div></div>
    </div>
    <table>
      <thead><tr>
        <th>客户端 IP</th><th>域名</th><th>端口</th><th>次数</th><th>失败</th>
        <th>↑ 字节</th><th>↓ 字节</th><th>总流量</th>
      </tr></thead>
      <tbody id="hist-body"><tr><td colspan="8" class="muted">加载中…</td></tr></tbody>
    </table>
    <div id="hist-msg" class="msg"></div>
  </section>
```

c) JS：`showTab` 增加分支：

```js
function showTab(name) {
  $("tab-conn").classList.toggle("active", name === "tab-conn");
  $("tab-history").classList.toggle("active", name === "tab-history");
  $("tab-dns").classList.toggle("active", name === "tab-dns");
  $("view-conn").hidden = name !== "tab-conn";
  $("view-history").hidden = name !== "tab-history";
  $("view-dns").hidden = name !== "tab-dns";
}
$("tab-conn").onclick = () => showTab("tab-conn");
$("tab-history").onclick = () => { showTab("tab-history"); loadHistory(); };
$("tab-dns").onclick = () => { showTab("tab-dns"); loadDns(); };
```

d) 追加历史加载逻辑：

```js
// ---- 历史连接：单日查询，不轮询（历史只在连接结束时变化） ----
function todayStr() {
  const d = new Date();
  const p = (n) => String(n).padStart(2, "0");
  return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate());
}

async function loadHistory() {
  const dateEl = $("history-date");
  if (!dateEl.value) dateEl.value = todayStr();
  const body = $("hist-body");
  const msg = $("hist-msg");
  msg.textContent = "";
  try {
    const r = await fetch("/api/history?date=" + encodeURIComponent(dateEl.value), {cache: "no-store"});
    const doc = await r.json().catch(() => ({}));
    if (!r.ok) { msg.className = "msg err"; msg.textContent = "读取失败: " + (doc.error || ("HTTP " + r.status)); return; }
    if (!doc.enabled) {
      body.innerHTML = '<tr><td colspan="8" class="muted">历史记录未启用（config.yaml: history.enabled）</td></tr>';
      for (const id of ["hist-entries", "hist-count", "hist-failures", "hist-up", "hist-down", "hist-total"]) $(id).textContent = "–";
      return;
    }
    const t = doc.totals || {};
    $("hist-entries").textContent = t.entries ?? 0;
    $("hist-count").textContent = t.count ?? 0;
    $("hist-failures").textContent = t.failures ?? 0;
    $("hist-up").textContent = fmtBytes(t.bytesUp || 0);
    $("hist-down").textContent = fmtBytes(t.bytesDown || 0);
    $("hist-total").textContent = fmtBytes(t.totalBytes || 0);
    const rows = (doc.entries || []).map((e) => {
      const failCell = e.failures > 0
        ? '<td style="color:#ff7b72">' + e.failures + "</td>"
        : "<td>0</td>";
      return "<tr><td class=\"muted\">" + esc(e.clientIp) +
        "</td><td>" + esc(e.host) +
        "</td><td>" + e.port +
        "</td><td>" + e.count +
        "</td>" + failCell +
        "</td><td>" + fmtBytes(e.bytesUp) +
        "</td><td>" + fmtBytes(e.bytesDown) +
        "</td><td>" + fmtBytes(e.totalBytes) + "</td></tr>";
    });
    body.innerHTML = rows.length ? rows.join("") : '<tr><td colspan="8" class="muted">该日暂无记录</td></tr>';
  } catch (e) {
    msg.className = "msg err";
    msg.textContent = "读取失败: " + e.message;
  }
}

$("history-date").value = todayStr();
$("history-date").onchange = loadHistory;
```

- [ ] **Step 5.6 全量测试 + 冒烟**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go test ./... -race 2>&1 | tail -5
GOPROXY=off go vet ./... && gofmt -l .
```

Expected: 测试 PASS，vet/gofmt 无输出。

- [ ] **Step 5.7 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/admin.go src/admin_test.go src/static/index.html
git commit -m "feat(admin): history tab with single-day filter, sorted by traffic

GET /api/history takes an optional date (default today) and rejects
anything that is not YYYY-MM-DD rather than guessing. A nil History
reports enabled=false so the page can say why the tab is empty. Rows
arrive sorted by total bytes descending, which is what the page shows —
no client-side re-sorting.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: main.go 接线、版本号与文档

**Files:**
- Modify: `src/main.go`
- Modify: `.version`
- Modify: `.gitignore`、`.dockerignore`
- Create: `data/config.yaml.example`
- Modify: `README.md`
- Modify: `docker/docker-compose.yml`（注释）

- [ ] **Step 6.1 修改 `src/main.go`**

a) flag 增加 `--config`：

```go
		configFile    = flag.String("config", "data/config.yaml", "config file (created with defaults if missing)")
```

b) `os.Exit(run(...))` 传入 config 路径（放在 listen/dnsFile 之后）：

```go
	os.Exit(run(*listen, *dnsFile, *configFile,
		resolveSetting(explicit["client-key-file"], "TAILCAT_CLIENT_KEY_FILE", *clientKeyFile),
		resolveSetting(explicit["admin-addr"], "ADMIN_ADDR", *adminAddr),
		*noWatch))
```

c) `run` 签名：

```go
func run(listen, dnsFile, configFile, clientKeyFile, adminAddr string, noWatch bool) int {
```

d) 在 `LoadDNSFile` 之后、`NewServer` 之前插入配置与历史初始化：

```go
	cfg, err := LoadConfig(configFile)
	if err != nil {
		slog.Error("cannot load config", "path", configFile, "err", err)
		return 1
	}
	users := make(Users, len(cfg.Socks.Users))
	for name, pw := range cfg.Socks.Users {
		users[name] = User{Password: pw, Mode: modeForUser(name)}
	}

	// 历史是旁路功能：打不开就降级为不可用并继续服务，绝不让代理因此拒绝
	// 启动（spec §5.5）。
	var history *History
	if cfg.History.Enabled {
		history, err = OpenHistory(cfg.History.Path)
		if err != nil {
			slog.Warn("history disabled: cannot open database", "path", cfg.History.Path, "err", err)
			history = nil
		}
	}
	if history != nil {
		defer history.Close()
	}
```

e) `NewServer` 改为 `NewServerWithUsers`：

```go
	srv, err := NewServerWithUsers(dnsMap, dialer, registry, listen, users, history)
```

f) admin 构造后挂上历史：

```go
		api := NewAdmin(registry, dnsMap, dnsFile)
		api.SetHistory(history)
```

- [ ] **Step 6.2 编译与测试**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/src && GOPROXY=off go build ./... && GOPROXY=off go test ./... 2>&1 | tail -5
```

Expected: 无输出（build），测试 PASS。若 `main_test.go` 的 `resolveSetting` 测试受影响则无需改（签名未变）。

- [ ] **Step 6.3 冒烟：认证 + 历史**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
rm -rf /tmp/tc-smoke && mkdir -p /tmp/tc-smoke
cat > /tmp/tc-smoke/config.yaml <<'EOF'
socks:
  users:
    tailcat: "pw1"
    all: "pw2"
history:
  enabled: true
  path: "/tmp/tc-smoke/history.db"
EOF
GOPROXY=https://goproxy.cn,direct go build -o /tmp/tc-smoke/tailcat-socks ./src
/tmp/tc-smoke/tailcat-socks --listen 127.0.0.1:11095 --dns-file data/dns.txt.example \
  --config /tmp/tc-smoke/config.yaml --client-key-file /tmp/tc-smoke/key \
  --admin-addr 127.0.0.1:18095 &
sleep 2
echo '--- 无认证应被拒 ---'
curl -sS -m 5 --socks5-hostname 127.0.0.1:11095 http://example.com/ -o /dev/null; echo "rc=$?"
echo '--- 正确凭据（直连 example.com 走 all 用户）---'
curl -sS -m 15 --proxy-user all:pw2 --socks5-hostname 127.0.0.1:11095 http://example.com/ -o /dev/null -w 'http=%{http_code}\n'
echo '--- 错误密码 ---'
curl -sS -m 5 --proxy-user all:wrong --socks5-hostname 127.0.0.1:11095 http://example.com/ -o /dev/null; echo "rc=$?"
echo '--- 历史 API ---'
curl -sS "http://127.0.0.1:18095/api/history" | head -c 600; echo
kill %1 2>/dev/null
```

Expected：无认证 curl 失败（rc 非 0）；正确凭据 `http=200`；错误密码失败；历史 API 返回该日条目（至少 all 用户那次直连）。

- [ ] **Step 6.4 版本号与忽略文件**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
printf '0.10.0\n' > .version
grep -q '^data/config.yaml$' .gitignore || printf 'data/config.yaml\ndata/history.db\ndata/history.db-wal\ndata/history.db-shm\n' >> .gitignore
grep -q '^data/config.yaml$' .dockerignore || printf 'data/config.yaml\ndata/history.db\n' >> .dockerignore
cat .version; echo '--- .gitignore ---'; cat .gitignore; echo '--- .dockerignore ---'; cat .dockerignore
```

Expected：`.version` 为 `0.10.0`；两个 ignore 文件各含新行且无重复。

- [ ] **Step 6.5 新建 `data/config.yaml.example`**

```yaml
# tailcat-socks 配置示例。复制为 data/config.yaml 并修改密码。
# 真实配置文件含明文密码，已被 git 忽略，权限请保持 0600。
socks:
  users:
    # tailcat: 只能访问 dns.txt 命中的域名（走隧道）
    tailcat: "changeme"
    # all: 可访问任意目标（命中走隧道，未命中直连）
    all: "changeme"

history:
  # 关掉后不打开数据库，管理页的"历史连接"标签会显示未启用。
  enabled: true
  path: "data/history.db"
```

- [ ] **Step 6.6 更新 `README.md`**

在「命令行参数」表格中新增 `--config` 行，并在其下插入三节：

```markdown
| `--config` | `data/config.yaml` | – | 配置文件；不存在时自动生成默认模板 |
```

```markdown
## 配置（data/config.yaml）

首次启动时若文件不存在会自动生成一份模板并打 WARN。模板里的密码是占位的
`changeme`，**暴露到网络前务必修改**。文件含明文密码，权限应为 0600，已被
git 忽略。

```yaml
socks:
  users:
    tailcat: "changeme"      # 只能访问 dns.txt 命中的域名（走隧道）
    all: "changeme"          # 可访问任意目标

history:
  enabled: true
  path: "data/history.db"
```

配置在启动时读取一次，改完需重启。

## SOCKS5 认证

代理**强制**要求用户名/密码认证（RFC 1929）——不再接受 `0x00` 无认证方式，
客户端必须同时提供用户名和密码。

| 用户名 | 权限 |
|---|---|
| `tailcat` | **只能**访问 `dns.txt` 命中的域名（走隧道）；未命中的目标返回 SOCKS5 `0x02`（connection not allowed by ruleset） |
| 其他任意名字（按 `all` 处理） | 可访问任意目标：命中走隧道，未命中直连 |

```bash
# curl：用 --proxy-user 带上凭据
curl --socks5-hostname 127.0.0.1:1080 --proxy-user tailcat:密码 http://a.example.com/
curl --socks5-hostname 127.0.0.1:1080 --proxy-user all:密码 http://example.com/
```

认证失败（`0x01 0x01`）与权限拒绝（`0x02`）都不会写入历史。

## 历史连接

每次连接结束后，按（日期, 客户端 IP, 域名, 端口）聚合一行写入
`data/history.db`（SQLite，WAL 模式）：当日累计次数、失败次数、上下行字节。
失败指拨号失败或未进入转发阶段。

管理页的「历史连接」标签可按日期查看，列表按累计流量倒序。认证失败与权限
拒绝不计入历史——它们从未建立连接。

`history.enabled: false` 或数据库打不开时，历史功能降级为不可用，代理继续
正常服务（启动日志有 WARN）。
```

- [ ] **Step 6.7 `docker/docker-compose.yml` 补注释**

在 `volumes:` 的 `/app/config:ro` 行下方加一行注释：

```yaml
      - ../:/app/config:ro             # dns.txt + config.yaml (read-only)
```

- [ ] **Step 6.8 提交**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add src/main.go .version .gitignore .dockerignore data/config.yaml.example README.md docker/docker-compose.yml
git commit -m "feat: wire config and history into main; bump to 0.10.0

Authentication is mandatory, so the config is loaded before the listener
binds and an empty user table aborts startup. A history database that
fails to open only WARNs and leaves the feature disabled — it is a
side channel and must not take the proxy down. .version is the single
source of the version number and moves to 0.10.0.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 自查

**1. Spec 覆盖**

| Spec 章节 | 对应任务 |
|---|---|
| §3 配置（YAML、默认生成、空 users 报错） | Task 1 |
| §4.1 RFC1929 认证 | Task 4 |
| §4.2 按用户授权 + 0x02 | Task 2（策略）、Task 4（接线） |
| §4.3 拒绝回执 | Task 4 |
| §5.1–5.2 依赖与表结构 | Task 3 |
| §5.3 upsert 与失败语义 | Task 3、Task 4 |
| §5.4 WAL/busy_timeout | Task 3 |
| §5.5 打开失败降级 | Task 6 |
| §6.1–6.2 标签与页面 | Task 5 |
| §6.3 API 契约 | Task 5 |
| §7 退出顺序（Close） | Task 6 |
| §8 测试计划 | 各任务的测试步骤 |
| §9 部署面与版本号 | Task 6 |

**2. 占位符扫描**：无 TBD/TODO；每个代码步骤都有完整代码。

**3. 类型一致性**：`Users`/`User`/`Mode`（Task 2）在 Task 4、Task 6 一致；`History`/`HistoryEntry`/`HistoryTotals`（Task 3）在 Task 5 的 DTO 中一致；`NewServerWithUsers` 签名在 Task 4 定义、Task 6 调用，参数顺序一致；`modeForUser` 在 Task 2 定义、Task 6 调用。
