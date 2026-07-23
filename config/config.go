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
