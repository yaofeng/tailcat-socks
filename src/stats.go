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
	ID     uint64
	Client string // 客户端 socket 的对端地址
	Host   string // 客户端请求的原始域名
	Target string // token（tunnel）或 host（direct）
	Port   int
	Via    string // ViaTunnel / ViaDirect
	// Opened 只许经 ConnRegistry.MarkOpen 写：与 Snapshot 的读同受 registry
	// 锁保护，禁止任何直接赋值（会竞态 Snapshot）。
	Opened  bool
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
