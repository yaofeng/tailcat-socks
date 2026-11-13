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
