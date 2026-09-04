package main

import (
	"log"
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

// DNSMap holds the domain->token mapping, hot-swappable atomically.
type DNSMap struct {
	v atomic.Value // map[string]string
}

// Store atomically replaces the mapping.
func (m *DNSMap) Store(mapping map[string]string) { m.v.Store(mapping) }

// Load returns the current mapping (never nil).
func (m *DNSMap) Load() map[string]string {
	if v := m.v.Load(); v != nil {
		return v.(map[string]string)
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
// mapping into m. Read errors keep the previous mapping. It performs an
// initial load so the map is populated even if the caller started empty.
// Returns when stop is closed.
func WatchDNSFile(path string, m *DNSMap, interval time.Duration, stop <-chan struct{}) {
	if first, err := LoadDNSFile(path); err == nil {
		m.Store(first)
	} else {
		log.Printf("[tailcat-dns-proxy] initial load of %s failed: %v", path, err)
	}
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
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
			log.Printf("[tailcat-dns-proxy] reload failed (%v); keeping previous map", err)
			continue
		}
		lastMod = fi.ModTime()
		m.Store(newMap)
		log.Printf("[tailcat-dns-proxy] reloaded %s: %d domain(s) -> %d token(s)",
			path, len(newMap), len(tokenSet(newMap)))
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
