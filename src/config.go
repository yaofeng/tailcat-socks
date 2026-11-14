package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

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
		Enabled bool   `yaml:"enabled"`
		Path    string `yaml:"path"`
	} `yaml:"history"`
}

// LoadConfig 读取并解析 path。文件不存在时写出一份模板并返回其解析结果，
// 让首次启动可用；解析失败或用户表为空则返回错误——认证是强制项，空用户
// 表等于谁都不能连，这种配置只能是笔误，不该被静默接受。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if werr := os.WriteFile(path, []byte(configTemplate), 0o600); werr != nil {
			return nil, fmt.Errorf("write default config %s: %w", path, werr)
		}
		slogWarnDefaultConfig(path)
		data = []byte(configTemplate)
	} else if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cfg.Socks.Users) == 0 {
		return nil, fmt.Errorf("config %s: socks.users is empty; at least one user is required (authentication is mandatory)", path)
	}
	return &cfg, nil
}

func slogWarnDefaultConfig(path string) {
	slog.Warn("generated default config with placeholder passwords; edit it before exposing this proxy", "path", path)
}
