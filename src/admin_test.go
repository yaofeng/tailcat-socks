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

// clientConnectionsDoc is the test-side decode mirror of the spec §8
// document. It is named apart from admin.go's connectionsDoc because both
// files are package main and two package-scope types cannot share a name.
type clientConnectionsDoc struct {
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
	var doc clientConnectionsDoc
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

func TestHandlePutDNSBodyTooLarge(t *testing.T) {
	admin, _, dnsMap, path := newAdminFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// ~2.3 MB，超过 1 MiB 上限：MaxBytesReader 在 Decode 流读到上限时报错。
	// 断言要求错误串直指上限——若日后删掉 cap 行，本测试必须失败（届时
	// 400 只会来自末尾 `},]` 的语法错误），而不是搭上空转的便车。
	body := "[" + strings.Repeat(`{"token":"tcAAA111","domains":["a.example.com"],"note":""},`, 40000) + "]"
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/api/dns", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (1 MiB body cap)", rec.Code)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || !strings.Contains(e.Error, "request body too large") {
		t.Fatalf("body = %s, want error citing request body too large", rec.Body)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("oversized PUT must not touch dns.txt:\n%s", after)
	}
	if got := len(dnsMap.Load()); got != 3 {
		t.Errorf("dnsMap changed despite 400: %d domains, want 3", got)
	}
}

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

// clientHistoryDoc 是 /api/history 的测试侧解码镜像。与 admin.go 的
// historyDoc 分开命名：同在 package main，两个包级类型不能重名。
type clientHistoryDoc struct {
	Enabled bool           `json:"enabled"`
	Date    string         `json:"date"`
	Totals  HistoryTotals  `json:"totals"`
	Entries []HistoryEntry `json:"entries"`
}

func TestHandleHistoryDefaultsToToday(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	h, err := OpenHistory(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	admin.SetHistory(h)

	today := time.Now().Format("2006-01-02")
	h.Record(today, "1.1.1.1", "a.example.com", 443, false, 10, 20)

	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var doc clientHistoryDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Enabled {
		t.Error("enabled should be true when history is attached")
	}
	if doc.Date != today {
		t.Errorf("date = %q, want %q (default should be today)", doc.Date, today)
	}
	if len(doc.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(doc.Entries))
	}
	if got := doc.Entries[0].TotalBytes; got != 30 {
		t.Errorf("entry totalBytes = %d, want 30", got)
	}
	if doc.Totals.TotalBytes != 30 || doc.Totals.Count != 1 {
		t.Errorf("totals = %+v", doc.Totals)
	}
}

// 日期格式不做宽容解析：11-14-2026 这种必须 400，而不是被当成别的日子。
func TestHandleHistoryRejectsBadDate(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history?date=11-14-2026", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "YYYY-MM-DD") {
		t.Errorf("error should explain the expected format, got %s", rec.Body)
	}
}

// 历史未启用（nil）时仍要 200 + enabled:false + 空数组，页面据此显示"未启用"，
// 而不是当成接口坏了。
func TestHandleHistoryDisabled(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history?date=2026-11-14", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc clientHistoryDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Enabled {
		t.Error("nil history must report enabled=false")
	}
	if doc.Entries == nil {
		t.Error("entries must be [] not null, so the page can iterate unconditionally")
	}
	if len(doc.Entries) != 0 {
		t.Errorf("disabled history must return no entries, got %+v", doc.Entries)
	}
}

// 指定日期只返回该日的数据，且按累计流量倒序（服务端排序，页面不再排）。
func TestHandleHistoryFiltersByDateAndOrders(t *testing.T) {
	admin, _, _, _ := newAdminFixture(t)
	h, err := OpenHistory(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	admin.SetHistory(h)

	h.Record("2026-11-13", "9.9.9.9", "old.example.com", 443, false, 999999, 999999)
	h.Record("2026-11-14", "small", "a.example.com", 443, false, 1, 1)
	h.Record("2026-11-14", "big", "a.example.com", 443, false, 5000, 5000)

	rec := httptest.NewRecorder()
	admin.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/history?date=2026-11-14", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc clientHistoryDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 2 {
		t.Fatalf("want only the 2 rows from 2026-11-14, got %d: %+v", len(doc.Entries), doc.Entries)
	}
	if doc.Entries[0].ClientIP != "big" {
		t.Errorf("first row = %q, want big (highest traffic first)", doc.Entries[0].ClientIP)
	}
}
