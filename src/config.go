package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// configTemplate 是 config.yaml 缺失时落盘的模板。密码给 changeme 只是为了让
// 首次启动能跑起来；启动时会 WARN 提示修改。两个用户同名密码是刻意的：
// 默认配置只是占位，真实部署必须改。
const configTemplate = `# tailcat-proxy 配置。此文件含明文密码，权限应为 0600。
listen:
  # 三个入口，空串表示不监听该入口。改完需重启。
  http-proxy: "0.0.0.0:7890"
  socks-proxy: "0.0.0.0:1080"
  web-console: "127.0.0.1:8080"

users:
  # tailcat: 只能访问 dns.txt 命中的域名（走隧道）
  tailcat: "changeme"
  # all: 可访问任意目标（命中走隧道，未命中直连）
  all: "changeme"

history:
  # 历史库固定落在 --data-path 目录下的 history.db。
  enabled: true
`

// listenDefaults 是三个入口的默认地址。它们同时出现在 configTemplate 里；
// 缺 listen 段的旧配置也回退到这里，不至于因为少一段就起不来。
var listenDefaults = Listen{
	HTTPProxy:  "0.0.0.0:7890",
	SOCKSProxy: "0.0.0.0:1080",
	WebConsole: "127.0.0.1:8080",
}

// Listen 是三个监听入口的地址。空串表示不监听该入口——比如只想要 SOCKS5，
// 就把 http-proxy 设成 ""。
type Listen struct {
	HTTPProxy  string `yaml:"http-proxy"`
	SOCKSProxy string `yaml:"socks-proxy"`
	WebConsole string `yaml:"web-console"`
}

// Config 是 <data-path>/config.yaml 的映射。新增配置项时同时更新
// configTemplate，否则新装用户拿不到默认值。
//
// users 直接在顶层：这个代理只有 SOCKS5 一种入口，"socks:" 包一层只是多一级
// 缩进而不带来任何区分度。
type Config struct {
	Listen  Listen            `yaml:"listen"`
	Users   map[string]string `yaml:"users"`
	History struct {
		// Enabled 只决定要不要记录历史。库的位置不在这里配：所有数据文件
		// 统一由 --data-path 决定（config.yaml、dns.txt、client key、history.db
		// 都放在那个目录），一个来源才不会出现"配置写相对路径、cwd 一换就
		// 建到别处"这类事故。
		Enabled bool `yaml:"enabled"`
	} `yaml:"history"`
}

// LoadConfig 读取并解析 path。文件不存在时写出一份模板并返回其解析结果，
// 让首次启动可用；解析失败或用户表为空则返回错误——认证是强制项，空用户
// 表等于谁都不能连，这种配置只能是笔误，不该被静默接受。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	generated := false
	if errors.Is(err, fs.ErrNotExist) {
		if werr := writeDefaultConfig(path); werr != nil {
			return nil, werr
		}
		slog.Warn("generated default config with placeholder passwords; edit it before exposing this proxy", "path", path)
		data = []byte(configTemplate)
		generated = true
	} else if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// KnownFields 让拼错的键直接报错；否则 history.enabld 之类会静默失效，
	// 用户看到的是"配置写了却没生效"，这种问题最难查。
	dec.KnownFields(true)
	if derr := dec.Decode(&cfg); derr != nil && !errors.Is(derr, io.EOF) {
		// 0 字节文件不算语法错误，而是"空配置"：让它落到下面的空用户表检查，
		// 用户拿到的才是"你得配用户"，而不是一句莫名其妙的 EOF。
		return nil, fmt.Errorf("parse config %s: %w", path, derr)
	}
	// listen 段缺失的字段（含整段缺失）回退到默认值；YAML 里显式写的 ""
	// 是"不监听"，必须与"没写"区分开——只给没出现的字段填默认值。
	if cfg.Listen.HTTPProxy == "" && !listenKeyPresent(data, "http-proxy") {
		cfg.Listen.HTTPProxy = listenDefaults.HTTPProxy
	}
	if cfg.Listen.SOCKSProxy == "" && !listenKeyPresent(data, "socks-proxy") {
		cfg.Listen.SOCKSProxy = listenDefaults.SOCKSProxy
	}
	if cfg.Listen.WebConsole == "" && !listenKeyPresent(data, "web-console") {
		cfg.Listen.WebConsole = listenDefaults.WebConsole
	}
	for _, l := range []struct{ name, addr string }{
		{"http-proxy", cfg.Listen.HTTPProxy},
		{"socks-proxy", cfg.Listen.SOCKSProxy},
		{"web-console", cfg.Listen.WebConsole},
	} {
		if l.addr == "" {
			continue // 空串 = 不监听
		}
		if err := validateListenAddr(l.addr); err != nil {
			return nil, fmt.Errorf("config %s: listen.%s: %w", path, l.name, err)
		}
	}
	if len(cfg.Users) == 0 {
		// 把期望形状直接写进错误里，省得用户去翻文档。
		return nil, fmt.Errorf("config %s: users is empty; at least one user is required (authentication is mandatory), e.g.\n  users:\n    tailcat: \"<password>\"", path)
	}
	for user, pw := range cfg.Users {
		// 只报用户名，绝不回显密码——这条错误会进日志和终端。
		if user == "" {
			return nil, fmt.Errorf("config %s: users has an empty username", path)
		}
		if strings.TrimSpace(pw) == "" {
			return nil, fmt.Errorf("config %s: users[%q] has an empty password", path, user)
		}
		// 用户名决定授权范围，所以拼错的名字（tailatc / Tailcat）不是"多了个
		// 普通账号"，而是静默放大权限或不生效。在这里就拒掉，别等到构造
		// Users 时才发现——那时配置已被接受过一次了。
		if _, merr := modeForUser(user); merr != nil {
			return nil, fmt.Errorf("config %s: %w", path, merr)
		}
	}
	// 到这里配置已经能用，才谈权限。只提示不改：既有文件的权限可能是运维
	// 有意为之，静默 chmod 会让"谁动的权限"变成无头案；生成路径才由我们
	// 自己钉死 0600。
	if !generated {
		if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm()&0o077 != 0 {
			slog.Warn("config holds plaintext passwords but is readable by group/others", "path", path, "mode", fmt.Sprintf("%04o", fi.Mode().Perm()))
		}
	}
	return &cfg, nil
}

// listenKeyPresent 报告 YAML 里 listen.<key> 是否被显式写过。用它区分"没写"
// 与"写成空串"：前者要补默认值，后者是"不监听"的明确意图。
func listenKeyPresent(data []byte, key string) bool {
	var probe struct {
		Listen map[string]any `yaml:"listen"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	_, ok := probe.Listen[key]
	return ok
}

// validateListenAddr 校验 host:port 形状与端口范围。空 host（":1080"）合法，
// 表示绑所有接口。
func validateListenAddr(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q: %w (want host:port)", addr, err)
	}
	_ = host // 空 host 合法
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%q: port %q is not a number", addr, portStr)
	}
	if p < 0 || p > 65535 {
		return fmt.Errorf("%q: port %d out of range", addr, p)
	}
	return nil
}

// writeDefaultConfig 以 O_EXCL 创建默认配置并钉死 0600。写入或关闭失败时删掉
// 半成品：一个截断的 config.yaml 会让之后每次启动都解析失败，而文件已存在，
// 再也不会重新生成，等于把用户永久锁在门外。
func writeDefaultConfig(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write default config %s: %w", path, err)
	}
	if _, err := f.WriteString(configTemplate); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write default config %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("write default config %s: %w", path, err)
	}
	// OpenFile 的 perm 会被 umask 过滤；Chmod 钉死 0600（文件里有明文密码）。
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("pin default config mode %s: %w", path, err)
	}
	return nil
}
