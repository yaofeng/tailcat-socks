package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册 "sqlite" 方言
)

// historySchema 建表 + 索引，全部 IF NOT EXISTS，重复打开是幂等的。
//
// 主键是自增 id（页面用它引用行），聚合靠 UNIQUE(date, client_ip, host, port)
// 定位——同一个（日期, IP, 域名, 端口）在一行里累加。
//
// 关于 date 的 DATE 类型：SQLite 没有真正的日期类型，DATE 只给出 NUMERIC
// 亲和性；存进去的是 'YYYY-MM-DD' 文本，按字典序比较对 ISO-8601 恰好等价于
// 时间序，所以 WHERE date = ? 与 ORDER BY 都正确。两个注意点：
//   - 驱动会把声明为 DATE 的列里的文本在 SELECT 时解析成 time.Time，将来
//     若在 SELECT 里带上 date 列，Scan 目标必须是 time.Time 而不是 string。
//   - 形如 "20261114" 的纯数字串会绑成 INTEGER，就不再是文本比较了；写入
//     方（Record 的 day 参数）必须保持带连字符的格式。
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

// historyDSN 把文件路径编成 SQLite 的 file: URI。
//
// 不能直接把参数拼在路径后面：驱动在第一个 '?' 处切分文件名与查询串，于是
// 空路径会变成一个名叫 "?_pragma=..." 的垃圾文件、含 '?' 的路径会被截断，
// 两种情况都静默成功（库建在了别处，WAL 也没开）。用 file: URI 并对路径做
// 转义，'?' 会变成 %3F 回到文件名里。
//
// 注意不能用 url.URL{Scheme:"file"}.String()：相对路径会编成 file://data/x，
// 驱动把 data 当成主机名，直接打不开。
func historyDSN(path string) string {
	u := &url.URL{Path: path}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	// 侧表数据，WAL 下 NORMAL 已足够安全；每条连接结束一次的写入没必要
	// 为每次提交付 FULL 模式的 fsync。
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + u.EscapedPath() + "?" + q.Encode()
}

// OpenHistory 打开（必要时创建）path 处的 SQLite 库并建表。
func OpenHistory(path string) (*History, error) {
	if path == "" {
		return nil, fmt.Errorf("history path is empty; it is derived from --data-path, so this means the data directory was not resolved")
	}
	// SQLite 不会创建中间目录，而默认配置就是 data/history.db。不建目录时
	// 失败会报成 "unable to open database file: out of memory (14)"——那句
	// out of memory 是 SQLITE_CANTOPEN 的误导性措辞，运维会被带偏。
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create history dir %s: %w", dir, err)
		}
	}
	// 先以 0600 建立文件：表里存客户端 IP 与域名，属于敏感元数据，与
	// config.yaml / client key 的处理保持一致（驱动自己建的话是 0644）。
	// OpenFile 的 perm 仍会被 umask 过滤，所以再 Chmod 钉死。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create history db %s: %w", path, err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn("cannot pin history db mode", "path", path, "err", err)
	}

	db, err := sql.Open("sqlite", historyDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open history db %s: %w", path, err)
	}
	// 保留单连接：写入本来就串行，单连接让写路径简单，代价是一次慢 Query
	// 会把后面的 Record 排在自己后面（可接受：Query 只在管理页切标签时跑）。
	// 真正消除 SQLITE_BUSY 的是 WAL + busy_timeout，不是这个设置。
	//
	// 注意这是经典的 MaxOpenConns(1) 陷阱：将来若有人持着 Rows 未关闭又发起
	// 另一个语句，会死锁。Query 目前自包含，不构成问题。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(historySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create history schema in %s: %w", path, err)
	}
	return &History{db: db}, nil
}

// Record 把一次已结束的连接并入当日该（IP, 域名, 端口）的累计行。
// failed 为 true 时 failures 加一；字节数按本次会话累加。
// 写失败只 WARN：历史是旁路，不能影响代理。
//
// 参数是 uint64 而列是 INTEGER（int64）：超过 2^63-1 的值会被驱动在绑定时
// 直接拒绝（不是回绕成负数），于是那一条记录连同 WARN 一起丢掉。真出现
// 这种量级（9.2 EB 的单次会话）时丢行是良性的失败方式。
func (h *History) Record(day, clientIP, host string, port int, failed bool, bytesUp, bytesDown uint64) {
	if h == nil || h.db == nil {
		return
	}
	fail := 0
	if failed {
		fail = 1
	}
	if _, err := h.db.Exec(historyUpsert, day, clientIP, host, port, fail, bytesUp, bytesDown); err != nil {
		slog.Warn("history write failed", "day", day, "client", clientIP, "host", host, "port", port, "err", err)
	}
}

// Query 返回某一天的全部聚合行与合计，按累计流量倒序（大流量在前）。
//
// 出错时返回零值 totals：半途失败时把已累加的合计交给调用方，会让人看到
// 一个和（nil 的）条目列表对不上的数字。
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
		return nil, HistoryTotals{}, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.ID, &e.ClientIP, &e.Host, &e.Port, &e.Count,
			&e.Failures, &e.BytesUp, &e.BytesDown, &e.TotalBytes); err != nil {
			return nil, HistoryTotals{}, fmt.Errorf("scan history row: %w", err)
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
		return nil, HistoryTotals{}, fmt.Errorf("iterate history rows: %w", err)
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
