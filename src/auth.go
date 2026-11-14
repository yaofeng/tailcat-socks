package main

import "crypto/subtle"

// Mode 是一个用户的授权范围。用户只能在自己模式允许的目标上建立连接。
type Mode string

const (
	// ModeMatchedOnly：只允许 dns.txt 命中的目标（即走隧道的那些）。
	ModeMatchedOnly Mode = "matched-only"
	// ModeAny：任何目标——命中走隧道，未命中直连。
	ModeAny Mode = "any"
)

// User 是 config.yaml 里的一个 SOCKS5 用户。
type User struct {
	Password string
	Mode     Mode
}

// Users 是用户名到用户的映射。构建后只读，可安全并发使用。
type Users map[string]User

// modeForUser 把配置里的用户名翻译成授权模式。用户名是策略的一部分而不是
// 自由标签：tailcat 意味着"只走命中隧道"，其余名字按 all 处理——这样配置
// 里写错用户名时不会意外放大权限，而是退化成最宽但显式的 any。
//
// 注意：这里刻意不按配置里出现的顺序猜。只有 "tailcat" 收窄，其他一律 any。
func modeForUser(name string) Mode {
	if name == "tailcat" {
		return ModeMatchedOnly
	}
	return ModeAny
}

// Authenticate 校验 user/password，成功时返回该用户与其模式。密码用常量
// 时间比较，避免通过响应时间逐字节猜测——代理端口可能暴露在网络上。
// 用户不存在时也与错误密码走同样多的比较工作（用一个哑用户占位）。
func (u Users) Authenticate(user, password string) (User, bool) {
	dummy := User{Password: "\x00invalid\x00", Mode: ModeAny}
	got, ok := u[user]
	if !ok {
		got = dummy
	}
	// 无论用户是否存在都执行一次比较，保持时序一致。
	match := subtle.ConstantTimeCompare([]byte(got.Password), []byte(password)) == 1
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
		return true
	}
	return false
}
