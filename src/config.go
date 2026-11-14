package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// configTemplate 是 config.yaml 缺失时落盘的模板。密码给 changeme 只是为了让
// 首次启动能跑起来；启动时会 WARN 提示修改。两个用户同名密码是刻意的：
// 默认配置只是占位，真实部署必须改。
const configTemplate = `# tailcat-socks 配置。此文件含明文密码，权限应为 0600。
socks:
  users:
    # tailcat: 只能访问 dns.txt 命中的域名（走隧道）
    tailcat: "changeme"
    # all: 可访问任意目标（命中走隧道，未命中直连）
    all: "changeme"

history:
  enabled: true
  path: "data/history.db"
`

// Config 是 data/config.yaml 的映射。目前只有 SOCKS 用户与历史记录两组；
// 新增配置项时同时更新 configTemplate，否则新装用户拿不到默认值。
type Config struct {
	Socks struct {
		Users map[string]string `yaml:"users"`
	} `yaml:"socks"`
	History struct {
		Enabled bool `yaml:"enabled"`
		// Path 是历史库路径，相对路径按进程工作目录解析，而不是按配置文件
		// 所在目录——和 --dns-file / --client-key-file 保持同一个约定，不能
		// 让配置项各有一套解析规则。容器部署请写绝对路径，并落在挂载卷里，
		// 否则 db 会跟着容器一起消失。
		Path string `yaml:"path"`
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
	if len(cfg.Socks.Users) == 0 {
		// 把期望形状直接写进错误里，省得用户去翻文档。
		return nil, fmt.Errorf("config %s: socks.users is empty; at least one user is required (authentication is mandatory), e.g.\n  socks:\n    users:\n      tailcat: \"<password>\"", path)
	}
	for user, pw := range cfg.Socks.Users {
		// 只报用户名，绝不回显密码——这条错误会进日志和终端。
		if user == "" {
			return nil, fmt.Errorf("config %s: socks.users[%q] has an empty username", path, user)
		}
		if strings.TrimSpace(pw) == "" {
			return nil, fmt.Errorf("config %s: socks.users[%q] has an empty password", path, user)
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
