package main

import (
	"os"
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
