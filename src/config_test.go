package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigParsesUsersAndHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `socks:
  users:
    tailcat: "pw1"
    all: "pw2"
history:
  enabled: true
  path: "` + filepath.Join(dir, "h.db") + `"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Socks.Users["tailcat"]; got != "pw1" {
		t.Errorf("tailcat password = %q, want pw1", got)
	}
	if got := cfg.Socks.Users["all"]; got != "pw2" {
		t.Errorf("all password = %q, want pw2", got)
	}
	if !cfg.History.Enabled {
		t.Error("history.enabled should be true")
	}
	if cfg.History.Path != filepath.Join(dir, "h.db") {
		t.Errorf("history.path = %q", cfg.History.Path)
	}
}

// 文件不存在时生成默认配置并返回默认值——认证是强制项，不能因为缺文件就
// 变成"谁都能连"，也不能直接拒绝启动。
func TestLoadConfigCreatesDefaultWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on missing file: %v", err)
	}
	if cfg.Socks.Users["tailcat"] != "changeme" {
		t.Errorf("default tailcat password = %q, want changeme", cfg.Socks.Users["tailcat"])
	}
	if cfg.Socks.Users["all"] != "changeme" {
		t.Errorf("default all password = %q, want changeme", cfg.Socks.Users["all"])
	}
	// 文件必须真的落盘，否则用户的修改会在下次启动时被"重新生成"覆盖。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config was not written: %v", err)
	}
	// 再读一次必须得到同样的内容（round-trip）。
	again, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.Socks.Users["tailcat"] != "changeme" {
		t.Errorf("reloaded default = %q", again.Socks.Users["tailcat"])
	}
}

func TestLoadConfigRejectsEmptyUsers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("socks:\n  users: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("empty users must be an error: authentication is mandatory")
	}
}

func TestLoadConfigRejectsMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("socks: [not a map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("malformed YAML must be an error, never silently guessed")
	}
}
