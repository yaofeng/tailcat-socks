package main

import (
	"os"
	"path/filepath"
	"sync"
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

// 空路径必须报错：拼接式 DSN 会把空路径变成一个名叫 "?_pragma=..." 的垃圾
// 文件，而且 WAL 和 busy_timeout 都不生效——静默建错库比报错糟得多。
func TestOpenHistoryRejectsEmptyPath(t *testing.T) {
	if _, err := OpenHistory(""); err == nil {
		t.Fatal("empty history path must be an error")
	}
}

// SQLite 不会自动创建中间目录，而默认配置就是 data/history.db。不建目录时
// 失败会表现为 "unable to open database file: out of memory (14)"，
// 那句 out of memory 是 SQLITE_CANTOPEN 的误导性措辞。
func TestOpenHistoryCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deep", "h.db")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory should create the parent dir: %v", err)
	}
	defer h.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file was not created: %v", err)
	}
}

// 表里存客户端 IP 与域名，属于敏感元数据；仓库对 config.yaml 和 client key
// 都钉了 0600，这里保持一致。
func TestOpenHistoryPinsMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("db mode = %o, want 600", got)
	}
}

// 路径里的 ? 是 DSN 的分隔符：字符串拼接会把它之后的内容当成参数，库被
// 建到截断后的路径上。用 file: URI 转义后必须落在原路径。
func TestOpenHistoryHandlesQuestionMarkInPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weird?name.db")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory with ? in path: %v", err)
	}
	defer h.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db was not created at the literal path: %v", err)
	}
}

// 并发写入是常态（每条连接结束时写一次）。这条测试守住 WAL + 单连接池的
// 组合：将来有人改坏连接池设置，断言会立刻炸。
func TestHistoryConcurrentRecord(t *testing.T) {
	h := testHistory(t)
	const (
		goroutines = 50
		perRoutine = 4
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				h.Record("2026-11-14", "1.1.1.1", "a.example.com", 443, false, 1, 1)
			}
		}()
	}
	wg.Wait()

	entries, totals, err := h.Query("2026-11-14")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want a single folded row, got %d", len(entries))
	}
	want := int64(goroutines * perRoutine)
	if entries[0].Count != want {
		t.Errorf("count = %d, want %d", entries[0].Count, want)
	}
	if entries[0].BytesUp != want || entries[0].BytesDown != want {
		t.Errorf("bytes = %d/%d, want %d/%d", entries[0].BytesUp, entries[0].BytesDown, want, want)
	}
	if totals.Count != want {
		t.Errorf("totals.count = %d, want %d", totals.Count, want)
	}
}

// Close 之后再写不得 panic：Record 只 WARN，Query 返回错误或空结果。
// 关停时在途连接的 defer 可能正好撞上历史库关闭。
func TestHistoryAfterClose(t *testing.T) {
	h, err := OpenHistory(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h.Record("2026-11-14", "1.1.1.1", "a.example.com", 443, false, 1, 1) // 不得 panic
	_, _, _ = h.Query("2026-11-14")                                      // 不关心结果，只要不 panic
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
	// TotalBytes 由 SQL 计算，是页面排序的依据，必须与两侧之和一致。
	if e.TotalBytes != 303 {
		t.Errorf("row TotalBytes = %d, want 303", e.TotalBytes)
	}
	if totals.Count != 3 || totals.BytesUp != 101 || totals.BytesDown != 202 {
		t.Errorf("totals = %+v", totals)
	}
	if totals.TotalBytes != 303 || totals.Entries != 1 {
		t.Errorf("totals TotalBytes/Entries = %d/%d, want 303/1", totals.TotalBytes, totals.Entries)
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

// 日期以调用方给的文本原样落库：直接用 SQL 读回裸值，避免 Record 与 Query
// 用同样方式"错得一致"让测试蒙混过关。
func TestHistoryStoresDateAsGiven(t *testing.T) {
	h := testHistory(t)
	h.Record("1999-12-31", "1.1.1.1", "a.example.com", 443, false, 1, 1)

	var raw string
	// CAST 成 TEXT：date 列声明为 DATE，驱动会把该列的文本解析成 time.Time，
	// 直接 Scan 进 string 会得到 RFC3339 形式。
	if err := h.db.QueryRow(`SELECT CAST(date AS TEXT) FROM history`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "1999-12-31" {
		t.Errorf("stored date = %q, want the exact string 1999-12-31", raw)
	}
}
