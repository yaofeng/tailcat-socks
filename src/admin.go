package main

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Admin 承载管理页的 HTTP API（spec §8）。页面路由 "/" 在 Task 6 注册，
// 接线在 Task 7；本类型只持有 registry/dnsMap/dnsPath 三个句柄，读取
// 全部走锁内快照或原子加载，不需要自己的锁。
type Admin struct {
	registry *ConnRegistry
	dnsMap   *DNSMap
	dnsPath  string
	// history 为 nil 表示历史功能未启用（配置关掉或数据库打不开），
	// /api/history 会如实返回 enabled=false。
	history *History
}

func NewAdmin(registry *ConnRegistry, dnsMap *DNSMap, dnsPath string) *Admin {
	return &Admin{registry: registry, dnsMap: dnsMap, dnsPath: dnsPath}
}

// SetHistory 挂上历史库。单独一个 setter 而不是改 NewAdmin 签名：NewAdmin
// 已被多处测试与调用方使用，加参数会让每个不需要历史的场景都传 nil。
func (a *Admin) SetHistory(h *History) { a.history = h }

//go:embed static/index.html
var staticIndexHTML string // 编译期打进二进制；单个嵌入模式允许直接嵌进 string

// Handler 返回 admin mux，用 Go 1.22 的方法+路径模式：未注册路径 404，
// 同路径异方法由 mux 直接给 405。
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /api/connections", a.handleConnections)
	mux.HandleFunc("GET /api/history", a.handleHistory)
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

// historyDoc 是 /api/history 的响应体。Entries 永远是非 nil 切片（即使为空），
// 页面才能无条件 for-of；disabled 时也给出空数组而不是 null。
type historyDoc struct {
	Enabled bool           `json:"enabled"`
	Date    string         `json:"date"`
	Totals  HistoryTotals  `json:"totals"`
	Entries []HistoryEntry `json:"entries"`
}

// handleHistory 返回某一天的聚合历史。date 缺省为今天；格式必须是
// YYYY-MM-DD，否则 400——不做"猜你意思"的宽容解析，因为把 11-14-2026
// 当成别的日子只会让人以为数据丢了。
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

func (a *Admin) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// go:embed 在编译期保证内容存在；写失败只可能是客户端已断开。
	_, _ = w.Write([]byte(staticIndexHTML))
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
	// 请求体上限 1 MiB：dns.txt 是人规模配置，且 admin 与 SOCKS 代理同进程——
	// spec §11 允许 ADMIN_ADDR=0.0.0.0，不设上限的大 JSON 能打死整个进程。
	// 超限在 Decode 处以错误呈现，走 400，不碰文件与映射。
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
