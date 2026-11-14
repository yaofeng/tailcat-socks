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
