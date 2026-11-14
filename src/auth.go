package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
)

// Mode 是一个用户的授权范围。用户只能在自己模式允许的目标上建立连接。
type Mode string

const (
	// ModeMatchedOnly：只允许 dns.txt 命中的目标（即走隧道的那些）。
	ModeMatchedOnly Mode = "matched-only"
	// ModeAny：任何目标——命中走隧道，未命中直连。
	ModeAny Mode = "any"
)

// User 是 config.yaml 里的一个 SOCKS5 用户。password 不导出：User 整个值
// 很可能被塞进日志（slog.Debug("authed", "user", user)），导出字段会把明文
// 密码一起打出去。
type User struct {
	password string
	Mode     Mode
}

// Users 是用户名到用户的映射。构建后只读，可安全并发使用。
type Users map[string]User

// dummyUser 用于用户不存在时仍执行一次等价比较。它没有真实用途，password
// 取值本身也不重要——比较的是摘要，不是明文。
var dummyUser = User{password: "invalid", Mode: ModeAny}

// modeForUser 把配置里的用户名翻译成授权模式。只认两个名字：tailcat 收窄到
// 只走命中隧道，all 不限。其余名字一律报错而不是退化成 ModeAny——授权边界
// 上的拼写错误（tailatc / Tailcat）会静默放大成完全直连权限，和 config.go
// 用 KnownFields 拒绝拼错的键是同一个立场：配置写错了就得说。
func modeForUser(name string) (Mode, error) {
	switch name {
	case "tailcat":
		return ModeMatchedOnly, nil
	case "all":
		return ModeAny, nil
	}
	return "", fmt.Errorf("unknown socks user %q: only \"tailcat\" (matched-only) and \"all\" (unrestricted) are supported", name)
}

// Authenticate 校验 user/password，成功时返回该用户与其模式。密码用常量
// 时间比较，避免通过响应时间逐字节猜测——代理端口可能暴露在网络上。
//
// 无论用户是否存在都执行一次等价比较，且比较的是定长摘要，所以"用户不存在"
// 与"密码错误"在时序上不可区分。
//
// 空用户名或空密码由 LoadConfig 拒绝，不在这里兜底：认证是强制项，空凭据
// 等于谁都能连。不要因为这里没检查，就把配置层那道 guard 简化掉。
func (u Users) Authenticate(user, password string) (User, bool) {
	got, ok := u[user]
	if !ok {
		got = dummyUser
	}
	// 比较定长摘要而不是明文：ConstantTimeCompare 在长度不等时提前返回，
	// 直接比较明文会把"密码长度"泄漏成时序差异，也就能区分"用户不存在"
	// 与"密码错误"。摘要长度恒定，这条路径才是真正等价的。
	gotSum := sha256.Sum256([]byte(got.password))
	inSum := sha256.Sum256([]byte(password))
	match := subtle.ConstantTimeCompare(gotSum[:], inSum[:]) == 1
	if !ok || !match {
		return User{}, false
	}
	return got, true
}

// Allows 报告该模式是否允许走 via 这条通道。授权在路由之后判断，所以这里
// 看的是路由结果（ViaTunnel / ViaDirect），而不是再做一次 DNS 查询。
func (m Mode) Allows(via string) bool {
	switch m {
	case ModeMatchedOnly:
		return via == ViaTunnel
	case ModeAny:
		// 只认两条已知通道；未知的 via 说明路由出了问题，不该被当成放行。
		return via == ViaTunnel || via == ViaDirect
	}
	return false
}
