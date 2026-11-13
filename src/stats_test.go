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
