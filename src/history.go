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
