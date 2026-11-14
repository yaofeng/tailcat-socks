package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSetting(t *testing.T) {
	const env = "TAILCAT_SOCKS_TEST_RESOLVE"
	t.Cleanup(func() { os.Unsetenv(env) })

	// 什么都没设：默认值
	if got := resolveSetting(false, env, "default"); got != "default" {
		t.Errorf("no flag, no env: got %q, want default", got)
	}

	// env 覆盖默认
	os.Setenv(env, "fromenv")
	if got := resolveSetting(false, env, "default"); got != "fromenv" {
		t.Errorf("env should beat default, got %q", got)
	}

	// 显式 flag 压过 env——哪怕值恰好等于默认值
	if got := resolveSetting(true, env, "fromflag"); got != "fromflag" {
		t.Errorf("explicit flag should beat env, got %q", got)
	}
	if got := resolveSetting(true, env, "default"); got != "default" {
		t.Errorf("explicit flag at default value should still beat env, got %q", got)
	}

	// 显式空串 flag：--admin-addr="" 禁用 admin
	if got := resolveSetting(true, env, ""); got != "" {
		t.Errorf("explicit empty flag must stay empty, got %q", got)
	}

	// env 设成空串同样算"已设置"——ADMIN_ADDR="" 禁用
	os.Setenv(env, "")
	if got := resolveSetting(false, env, "default"); got != "" {
		t.Errorf(`ADMIN_ADDR="" must disable (got %q)`, got)
	}
}

// --data-path 把所有持久化文件收进一个目录，并且展开成绝对路径：相对路径
// 一旦漏到运行期，换个 cwd 启动就会一半文件读旧位置、一半写新位置。
func TestNewDataFilesExpandsToAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	rel, err := filepath.Rel(mustGetwd(t), dir)
	if err != nil {
		t.Skipf("temp dir not relative to cwd: %v", err)
	}
	files, err := newDataFiles(rel)
	if err != nil {
		t.Fatalf("newDataFiles: %v", err)
	}
	if !filepath.IsAbs(files.dir) {
		t.Errorf("dir = %q, want absolute", files.dir)
	}
	want := map[string]string{
		"dns":       filepath.Join(dir, "dns.txt"),
		"config":    filepath.Join(dir, "config.yaml"),
		"clientKey": filepath.Join(dir, "tailcat-client-key"),
		"history":   filepath.Join(dir, "history.db"),
	}
	got := map[string]string{
		"dns":       files.dns,
		"config":    files.config,
		"clientKey": files.clientKey,
		"history":   files.history,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestNewDataFilesUsesFixedNames(t *testing.T) {
	files, err := newDataFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{files.dns, files.config, files.clientKey, files.history} {
		if filepath.Dir(p) != files.dir {
			t.Errorf("%q should live directly in %q", p, files.dir)
		}
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
