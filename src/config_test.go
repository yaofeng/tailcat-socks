package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("default config was not written: %v", err)
	}
	// 文件里有明文密码：umask 会过滤 OpenFile 的 perm，必须 Chmod 钉死 0600。
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("generated config mode = %o, want 0600", got)
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

// 空用户表、空用户名、空白密码都必须拒绝，并且错误要指出具体是哪一项：
// 只说"配置非法"用户根本不知道改哪里。
func TestLoadConfigRejectsEmptyUsers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty map", "socks:\n  users: {}\n", "socks.users is empty"},
		{"zero byte", "", "socks.users is empty"},
		{"users absent", "history:\n  enabled: true\n", "socks.users is empty"},
		{"empty password", "socks:\n  users:\n    tailcat: \"\"\n", `socks.users["tailcat"] has an empty password`},
		{"blank password", "socks:\n  users:\n    tailcat: \"   \"\n", `socks.users["tailcat"] has an empty password`},
		{"empty username", "socks:\n  users:\n    \"\": hunter2\n", `socks.users[""] has an empty username`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("config must be rejected: authentication is mandatory")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			// 出错信息会进日志和终端，绝不能把密码本身打出来。
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error leaks the password: %q", err)
			}
		})
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

// 拼错的键必须报错：KnownFields 关掉时 history.enabld 会静默关掉历史记录，
// 这种"配置看起来生效了其实没有"最难查。
func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "socks:\n  users:\n    tailcat: hunter2\nhistory:\n  enabld: true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("unknown field must be an error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %q, want it to mention parse config", err)
	}
}

// 既有文件权限过松时只告警、不改权限：文件可能是运维故意留下的，静默 chmod
// 会让人查不到是谁动的。生成路径才需要自己钉死 0600。
func TestLoadConfigWarnsOnLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "socks:\n  users:\n    tailcat: hunter2\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !strings.Contains(buf.String(), "group/others") {
		t.Errorf("expected a permission warning, got logs: %q", buf.String())
	}
	// 告警不等于改权限。
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want untouched 0644", got)
	}
}

// 用户名决定授权范围，拼错的名字不能静默通过：tailatc 既不是 tailcat（收窄）
// 也不该被当成"又一个普通账号"。只认 tailcat 与 all 两个名字。
func TestLoadConfigRejectsUnknownUsernames(t *testing.T) {
	cases := []struct{ name, body string }{
		{"typo", "socks:\n  users:\n    tailatc: pw\n"},
		{"wrong case", "socks:\n  users:\n    Tailcat: pw\n"},
		{"arbitrary name", "socks:\n  users:\n    bob: pw\n"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("%s must be rejected", tt.name)
			}
			if !strings.Contains(err.Error(), "unknown socks user") {
				t.Errorf("error should name the unknown user, got %v", err)
			}
		})
	}
}

// 两个合法用户名必须仍能加载。
func TestLoadConfigAcceptsBothKnownUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "socks:\n  users:\n    tailcat: pw1\n    all: pw2\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("both known users must load: %v", err)
	}
	if len(cfg.Socks.Users) != 2 {
		t.Errorf("users = %v, want tailcat and all", cfg.Socks.Users)
	}
}
