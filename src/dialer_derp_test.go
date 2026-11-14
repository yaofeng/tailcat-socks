package main

import (
	"os"
	"testing"
)

// 本机打洞会产出一条 ~17 KB/s 的劣质 UDP 直连路径，magicsock 一旦
// "升级"过去就再也回不到 DERP，吞吐从 ~2 MB/s 跌到 ~17 KB/s。默认把
// TS_DEBUG_ALWAYS_USE_DERP 钉上，强制走 DERP；用户显式设成 0/false 时
// 尊重用户选择，不覆盖。
func TestPinDERPDefault(t *testing.T) {
	const env = "TS_DEBUG_ALWAYS_USE_DERP"

	t.Run("unset becomes forced", func(t *testing.T) {
		os.Unsetenv(env)
		t.Cleanup(func() { os.Unsetenv(env) })
		pinDERPDefault()
		if got := os.Getenv(env); got != "1" {
			t.Errorf("unset should pin to 1, got %q", got)
		}
	})

	t.Run("explicit 0 is respected", func(t *testing.T) {
		os.Setenv(env, "0")
		t.Cleanup(func() { os.Unsetenv(env) })
		pinDERPDefault()
		if got := os.Getenv(env); got != "0" {
			t.Errorf("explicit 0 must not be overwritten, got %q", got)
		}
	})

	t.Run("explicit empty string is respected", func(t *testing.T) {
		os.Setenv(env, "")
		t.Cleanup(func() { os.Unsetenv(env) })
		pinDERPDefault()
		if got := os.Getenv(env); got != "" {
			t.Errorf("explicit empty must not be overwritten, got %q", got)
		}
	})
}
