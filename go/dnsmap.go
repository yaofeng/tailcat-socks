package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
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

// WatchDNSFile hot-reloads the mapping into m until ctx is done. The fsnotify
// watch is armed before the initial load so no change can slip between the
// two, and re-armed on every create/write event because atomic saves
// (write+rename) replace the watched inode. A short debounce coalesces the
// event bursts editors emit per save. A failed reload keeps the previous map.
func WatchDNSFile(ctx context.Context, path string, m *DNSMap) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("create watcher", "err", err)
		return
	}
	defer watcher.Close()
	rearm := func() {
		if err := watcher.Add(path); err != nil {
			// Gone right now (remove/rename); a later Create re-arms us.
			slog.Debug("watch re-arm skipped", "path", path, "err", err)
		}
	}
	rearm()
	if first, err := LoadDNSFile(path); err != nil {
		slog.Warn("initial load failed", "path", path, "err", err)
	} else {
		m.Store(first)
	}

	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
				rearm() // atomic saves swap the inode; watch the new file
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(50*time.Millisecond, func() { reload(path, m) })
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("watch error", "path", path, "err", err)
		}
	}
}

// reload parses path and swaps it into m, keeping the old map on error.
func reload(path string, m *DNSMap) {
	newMap, err := LoadDNSFile(path)
	if err != nil {
		slog.Warn("reload failed; keeping previous map", "path", path, "err", err)
		return
	}
	m.Store(newMap)
	slog.Info("reloaded", "path", path, "domains", len(newMap), "tokens", len(tokenSet(newMap)))
}

// tokenSet returns the distinct tokens in a mapping.
func tokenSet(m map[string]string) map[string]bool {
	s := make(map[string]bool, len(m))
	for _, t := range m {
		s[t] = true
	}
	return s
}
