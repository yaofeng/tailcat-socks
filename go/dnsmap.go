package main

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// LoadDNSFile parses dns.txt into {domain: token}. Domains are stored
// lowercased; tokens keep their case. Blank lines and lines starting with
// '#' are ignored. A line with only a token (no domains) contributes
// nothing. Later lines override earlier ones for the same domain.
func LoadDNSFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mapping := make(map[string]string)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		token, domains := fields[0], fields[1:]
		for _, d := range domains {
			mapping[strings.ToLower(d)] = token
		}
	}
	return mapping, nil
}

// DNSMap holds the current domain->token mapping, swapped atomically on
// reload. The stored map must be treated as immutable after Store.
type DNSMap struct {
	v atomic.Pointer[map[string]string]
}

// Store atomically replaces the mapping.
func (m *DNSMap) Store(mapping map[string]string) {
	if mapping == nil {
		mapping = map[string]string{}
	}
	m.v.Store(&mapping)
}

// Load returns the current mapping (never nil).
func (m *DNSMap) Load() map[string]string {
	if p := m.v.Load(); p != nil {
		return *p
	}
	return map[string]string{}
}

// RewriteHost returns the token for host if mapped, else host unchanged.
func RewriteHost(host string, m map[string]string) string {
	if token, ok := m[strings.ToLower(host)]; ok {
		return token
	}
	return host
}

// WatchDNSFile polls path's mtime every interval; on change it reloads the
// mapping into m. Read errors keep the previous mapping, and a failed
// initial load is retried on every tick until it succeeds. It performs an
// initial load so the map is populated even if the caller started empty.
// Returns when stop is closed.
func WatchDNSFile(path string, m *DNSMap, interval time.Duration, stop <-chan struct{}) {
	// Stat before the initial load: a write landing between the two leaves
	// lastMod at the older mtime, so the loop's next tick sees the change
	// and reloads instead of silently swallowing the new content.
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}
	if first, err := LoadDNSFile(path); err == nil {
		m.Store(first)
	} else {
		lastMod = time.Time{} // retry the initial load on every tick (mtime != lastMod)
		slog.Warn("initial dns load failed; retrying", "path", path, "err", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().Equal(lastMod) {
			continue
		}
		newMap, err := LoadDNSFile(path)
		if err != nil {
			slog.Warn("dns reload failed; keeping previous map", "path", path, "err", err)
			continue
		}
		lastMod = fi.ModTime()
		m.Store(newMap)
		slog.Info("dns map reloaded", "path", path, "domains", len(newMap), "tokens", len(tokenSet(newMap)))
	}
}

// tokenSet returns the distinct tokens in a mapping.
func tokenSet(m map[string]string) map[string]bool {
	s := make(map[string]bool, len(m))
	for _, t := range m {
		s[t] = true
	}
	return s
}
