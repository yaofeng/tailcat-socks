package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// LoadDNSFile parses dns.txt into {domain: token}. Domains are stored
// lowercased; tokens keep their case. Blank lines and lines starting with
// '#' are ignored. A line with only a token (no domains) contributes
// nothing. Later lines override earlier ones for the same domain. The
// mapping is derived by `LoadDNSEntries` + `EntriesToMap`.
func LoadDNSFile(path string) (map[string]string, error) {
	entries, err := LoadDNSEntries(path)
	if err != nil {
		return nil, err
	}
	return EntriesToMap(entries), nil
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

// reload parses path and swaps it into m, keeping the old map on error. The
// error is returned so PUT /api/dns can report "saved but reload failed";
// the fsnotify path ignores it (it only logs), as before.
func reload(path string, m *DNSMap) error {
	newMap, err := LoadDNSFile(path)
	if err != nil {
		slog.Warn("reload failed; keeping previous map", "path", path, "err", err)
		return err
	}
	m.Store(newMap)
	slog.Info("reloaded", "path", path, "domains", len(newMap), "tokens", len(tokenSet(newMap)))
	return nil
}

// tokenSet returns the distinct tokens in a mapping.
func tokenSet(m map[string]string) map[string]bool {
	s := make(map[string]bool, len(m))
	for _, t := range m {
		s[t] = true
	}
	return s
}

// Entry is one dns.txt record: a token, its domains, and an optional single-line
// note persisted as "# ..." comment lines directly above the token line.
type Entry struct {
	Token   string   `json:"token"`
	Domains []string `json:"domains"`
	Note    string   `json:"note"`
}

// LoadDNSEntries parses dns.txt into entries. Consecutive "#..." lines are held
// as a pending note group and attach to the next token line (joined by single
// spaces); a blank line drops the pending group so file-header comments do not
// become the first entry's note. Token lines keep strings.Fields semantics: a
// trailing '#' is just another field, and the UI validator is what forbids '#'
// inside domains. Files without comments load as all-empty notes (old format).
func LoadDNSEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	var note []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			note = nil
		case strings.HasPrefix(line, "#"):
			// 空注释体（"#"/"#   "）跳过：append 空串会让 join 后带上前导
			// 空格，破坏 round-trip。
			if body := strings.TrimSpace(strings.TrimLeft(line, "#")); body != "" {
				note = append(note, body)
			}
		default:
			fields := strings.Fields(line)
			entries = append(entries, Entry{
				Token:   fields[0],
				Domains: fields[1:],
				Note:    strings.Join(note, " "),
			})
			note = nil
		}
	}
	return entries, nil
}

// EntriesToMap derives the domain->token mapping from entries: domains are
// lowercased, tokens keep their case, later entries win.
func EntriesToMap(entries []Entry) map[string]string {
	mapping := make(map[string]string)
	for _, e := range entries {
		for _, d := range e.Domains {
			mapping[strings.ToLower(d)] = e.Token
		}
	}
	return mapping
}

// ValidateEntries rejects anything SaveDNSEntries could not round-trip through
// LoadDNSEntries verbatim. Messages are user-facing (shown on the admin page),
// so they are Chinese and name the offending entry (1-based). A missing "tc"
// prefix only WARNs (spec §7): real token files use it, but nothing about
// parsing breaks without it, so it is never an error.
func ValidateEntries(entries []Entry) error {
	for i, e := range entries {
		n := i + 1
		if e.Token == "" {
			return fmt.Errorf("条目%d: token 不能为空", n)
		}
		if strings.HasPrefix(e.Token, "#") {
			return fmt.Errorf("条目%d: token 不能以 # 开头", n)
		}
		if strings.ContainsAny(e.Token, " \t\r\n") {
			return fmt.Errorf("条目%d: 包含空白", n)
		}
		if !strings.HasPrefix(e.Token, "tc") {
			slog.Warn("token lacks the tc prefix (tailcat addresses normally start with it)", "entry", n, "token", e.Token)
		}
		if len(e.Domains) == 0 {
			return fmt.Errorf("条目%d: 至少需要一个域名", n)
		}
		for _, d := range e.Domains {
			if d == "" {
				return fmt.Errorf("条目%d: 域名不能为空", n)
			}
			if strings.ContainsAny(d, "# \t") {
				return fmt.Errorf("条目%d: 域名 %q 不能包含 # 或空白", n, d)
			}
		}
		if strings.ContainsAny(e.Note, "\r\n") {
			return fmt.Errorf("条目%d: 备注必须是单行", n)
		}
	}
	return nil
}

// SaveDNSEntries validates and atomically rewrites path: one optional
// "# note" line per entry, then "token dom1 dom2..." — so any comment not
// attached to an entry (file headers, stray notes) is dropped on save.
// temp file + rename keeps the fsnotify watcher and any concurrent reader on
// whole-file transitions; the temp is always removed.
func SaveDNSEntries(path string, entries []Entry) error {
	if err := ValidateEntries(entries); err != nil {
		return err
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeded
	var sb strings.Builder
	for _, e := range entries {
		if e.Note != "" {
			sb.WriteString("# " + e.Note + "\n")
		}
		sb.WriteString(strings.Join(append([]string{e.Token}, e.Domains...), " ") + "\n")
	}
	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
