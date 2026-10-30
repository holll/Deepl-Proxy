package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Auth     AuthConfig     `yaml:"auth"`
	Database DatabaseConfig `yaml:"database"`
	Cache    CacheConfig    `yaml:"cache"`
}

type ServerConfig struct {
	Port        string `yaml:"port"`
	AllowOrigin string `yaml:"allow_origin"`
}

type AuthConfig struct {
	AdminToken        string `yaml:"admin_token"`
	GatewayToken      string `yaml:"gateway_token"`
	AdminCookieSecret string `yaml:"admin_cookie_secret"`
	SessionTTL        int    `yaml:"session_ttl"` // 秒
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type CacheConfig struct {
	// TTL 改为"天"，避免歧义
	TTLDays int `yaml:"ttl_days"`

	CleanupInterval  int `yaml:"cleanup_interval"`   // 秒
	CleanupBatchSize int `yaml:"cleanup_batch_size"` // 条数
	CleanupMaxRounds int `yaml:"cleanup_max_rounds"`

	// LegacyKeyDisabled 关闭对 v1 旧缓存键的兼容读取。
	//
	// 故意用"禁用"而非"启用"语义：配置文件里缺省该项时布尔零值是 false，
	// 也就是兼容读取默认开启。若写成 LegacyKeyEnabled，老配置文件（没有这一行）
	// 会被解析成 false，升级瞬间缓存全部失效 —— 这正是要避免的事故。
	//
	// v1 旧行全部过期后（最长一个 TTL）可以设为 true 省掉这次额外查询。
	LegacyKeyDisabled bool `yaml:"legacy_key_disabled"`
}

// TTLDuration 转换：统一提供一个方法
func (c *CacheConfig) TTLDuration() time.Duration {
	return time.Duration(c.TTLDays) * 24 * time.Hour
}

func Load() *Config {
	cfg := &Config{
		Server: ServerConfig{
			Port:        "8080",
			AllowOrigin: "*",
		},
		Auth: AuthConfig{
			SessionTTL: 43200, // 12小时（秒）
		},
		Database: DatabaseConfig{
			Path: "./data/deepl-proxy.db",
		},
		Cache: CacheConfig{
			TTLDays:          1,   // 1天
			CleanupInterval:  600, // 10分钟
			CleanupBatchSize: 500,
			CleanupMaxRounds: 20,
		},
	}

	// 读取配置文件
	if data, err := os.ReadFile("config.yaml"); err == nil {
		_ = yaml.Unmarshal(data, cfg)
	}

	// 环境变量覆盖
	if v := os.Getenv("GATEWAY_TOKEN"); v != "" {
		cfg.Auth.GatewayToken = v
	}
	if v := os.Getenv("ADMIN_TOKEN"); v != "" {
		cfg.Auth.AdminToken = v
	}
	if v := os.Getenv("ADMIN_COOKIE_SECRET"); v != "" {
		cfg.Auth.AdminCookieSecret = v
	}
	if v := os.Getenv("ALLOW_ORIGIN"); v != "" {
		cfg.Server.AllowOrigin = v
	}
	if v := os.Getenv("PORT"); v != "" {
		cfg.Server.Port = v
	}

	return cfg
}
