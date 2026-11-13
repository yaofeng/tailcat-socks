package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dns.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDNSFileBasic(t *testing.T) {
	path := writeTemp(t, `
# comment line
tcXYZabc123   www.example.com  api.example.com
tcDEF456ghi   foo.com

tcGGG         bar.com	baz.com
`)
	m, err := LoadDNSFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"www.example.com": "tcXYZabc123",
		"api.example.com": "tcXYZabc123",
		"foo.com":         "tcDEF456ghi",
		"bar.com":         "tcGGG",
		"baz.com":         "tcGGG",
	}
	for d, tok := range want {
		if m[d] != tok {
			t.Errorf("m[%q] = %q, want %q", d, m[d], tok)
		}
	}
	if _, ok := m["tcxyzabc123"]; ok {
		t.Error("token line itself must not become a domain key")
	}
}

func TestLoadDNSFileMissingDomains(t *testing.T) {
	m, err := LoadDNSFile(writeTemp(t, "tcONLYONE\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("token-only line should contribute nothing, got %v", m)
	}
}

func TestLoadDNSFileLaterOverrides(t *testing.T) {
	m, err := LoadDNSFile(writeTemp(t, "tcA dup.com\ntcB dup.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["dup.com"] != "tcB" {
		t.Errorf("later line should win, got %q", m["dup.com"])
	}
}

func TestLoadDNSFileMissingFile(t *testing.T) {
	if _, err := LoadDNSFile(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestRewriteHost(t *testing.T) {
	m := map[string]string{"www.example.com": "tcXYZ"}
	if got := RewriteHost("www.example.com", m); got != "tcXYZ" {
		t.Errorf("exact match: got %q", got)
	}
	if got := RewriteHost("WWW.Example.COM", m); got != "tcXYZ" {
		t.Errorf("case-insensitive match: got %q", got)
	}
	if got := RewriteHost("other.com", m); got != "other.com" {
		t.Errorf("no match should return host unchanged, got %q", got)
	}
}

func TestWatchDNSFileReloads(t *testing.T) {
	path := writeTemp(t, "tcA alpha.com\n")
	m := &DNSMap{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchDNSFile(ctx, path, m)

	// initial load
	waitFor(t, 3*time.Second, func() bool { return m.Load()["alpha.com"] == "tcA" })

	// fsnotify fires on the content write itself; no mtime bump needed
	if err := os.WriteFile(path, []byte("tcB beta.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return m.Load()["beta.com"] == "tcB" })
	if _, ok := m.Load()["alpha.com"]; ok {
		t.Error("removed domain should be gone after reload")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestWatchDNSFileKeepsPreviousMapOnReadError(t *testing.T) {
	path := writeTemp(t, "tcA keep.com\n")
	m := &DNSMap{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchDNSFile(ctx, path, m)
	waitFor(t, 3*time.Second, func() bool { return m.Load()["keep.com"] == "tcA" })

	// Make reads fail while events keep coming: replace the file with a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// A few ticks later the old mapping must still be served.
	time.Sleep(300 * time.Millisecond)
	if m.Load()["keep.com"] != "tcA" {
		t.Fatal("read error must keep previous mapping")
	}
}

func TestLoadDNSEntriesGroupsNotes(t *testing.T) {
	path := writeTemp(t, `# 头部说明第一行
# 头部说明第二行

# 生产 web 服务器
# 机房 A
tcXYZabc123 www.example.com api.example.com
tcDEF456ghi foo.com # 行尾井号不是注释
`)
	entries, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	e0 := entries[0]
	if e0.Token != "tcXYZabc123" || strings.Join(e0.Domains, ",") != "www.example.com,api.example.com" {
		t.Errorf("entry0 = %+v", e0)
	}
	if e0.Note != "生产 web 服务器 机房 A" {
		t.Errorf("entry0.Note = %q, want %q", e0.Note, "生产 web 服务器 机房 A")
	}
	e1 := entries[1]
	if e1.Note != "" {
		t.Errorf("entry1.Note = %q, want empty", e1.Note)
	}
	// 行尾 # 只是普通字段（Fields 语义），由校验层在保存时拒绝，而不是解析层。
	// 该行 Fields 出 4 段：token + foo.com + "#" + 行尾井号不是注释，故 Domains 长度为 3。
	if len(e1.Domains) != 3 || e1.Domains[1] != "#" {
		t.Errorf("entry1.Domains = %v", e1.Domains)
	}
}

func TestLoadDNSEntriesHeaderWithoutBlankLine(t *testing.T) {
	// 无空行分隔时，头部注释会并入第一条目的 note —— 这正是保存示例文件
	// 时要求空行分隔的原因。
	path := writeTemp(t, "# 就是备注\ntcA a.com\n")
	entries, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Note != "就是备注" {
		t.Errorf("note = %q", entries[0].Note)
	}
}

func TestEntriesToMap(t *testing.T) {
	m := EntriesToMap([]Entry{
		{Token: "tcA", Domains: []string{"A.com"}},
		{Token: "tcB", Domains: []string{"a.com", "b.com"}},
	})
	if m["a.com"] != "tcB" || m["b.com"] != "tcB" {
		t.Errorf("later entry must win, domains must lowercase: %v", m)
	}
}

func TestValidateEntries(t *testing.T) {
	ok := []Entry{{Token: "tcA", Domains: []string{"a.com"}, Note: "备注"}}
	if err := ValidateEntries(ok); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	// spec §7: a missing "tc" prefix is warn-only, never an error.
	if err := ValidateEntries([]Entry{{Token: "XYZabc", Domains: []string{"a.com"}}}); err != nil {
		t.Fatalf("tc-prefix must warn only, got: %v", err)
	}
	bad := [][]Entry{
		{{Token: "", Domains: []string{"a.com"}}},
		{{Token: "#x", Domains: []string{"a.com"}}},
		{{Token: "a b", Domains: []string{"x.com"}}},
		{{Token: "tcA", Domains: nil}},
		{{Token: "tcA", Domains: []string{"a#b"}}},
		{{Token: "tcA", Domains: []string{"a b"}}},
		{{Token: "tcA", Domains: []string{"a.com"}, Note: "两\n行"}},
	}
	for i, b := range bad {
		if err := ValidateEntries(b); err == nil {
			t.Errorf("bad case %d accepted", i)
		} else if !strings.HasPrefix(err.Error(), "条目1") {
			t.Errorf("bad case %d: message %q should name the entry", i, err)
		}
	}
}

func TestLoadDNSEntriesSkipsEmptyCommentBodies(t *testing.T) {
	path := writeTemp(t, "# a\n#\n#  \n# b\ntcA a.com\n")
	entries, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Note != "a b" {
		t.Fatalf("empty comment bodies leaked into note: %+v", entries)
	}
}

func TestSaveDNSEntriesRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns.txt")
	// 旧格式 + 头部散注释：保存后应只剩 entries 派生的行。
	if err := os.WriteFile(path, []byte("# 将被丢弃的头部\n\ntcA a.com\n\ntcB b.com c.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := []Entry{
		{Token: "tcA", Domains: []string{"a.com"}, Note: ""},
		{Token: "tcB", Domains: []string{"b.com", "c.com"}, Note: "双域名 站点"},
	}
	if err := SaveDNSEntries(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadDNSEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Note != "双域名 站点" {
		t.Fatalf("roundtrip lost data: %+v", out)
	}
	if got := EntriesToMap(out); got["c.com"] != "tcB" {
		t.Errorf("map after roundtrip: %v", got)
	}
	// 目录里不留临时文件。
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("save left extra files: %v", ents)
	}
}

func TestSaveDNSEntriesRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns.txt")
	if err := os.WriteFile(path, []byte("tcA a.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveDNSEntries(path, []Entry{{Token: "tcB", Domains: []string{"bad#dom"}}}); err == nil {
		t.Fatal("invalid entries must be rejected")
	}
	kept, _ := os.ReadFile(path)
	if string(kept) != "tcA a.com\n" {
		t.Errorf("rejected save must not touch the file, got %q", kept)
	}
}
