package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Auth     AuthConfig     `yaml:"auth"`
	Database DatabaseConfig `yaml:"database"`
	Cache    CacheConfig    `yaml:"cache"`
	Webhook  WebhookConfig  `yaml:"webhook"`
}

// WebhookConfig 通用 webhook 通知配置。
//
// 刻意不绑定某一家推送服务：Format 决定请求形状，URL 决定投递地址。
// 这样同一个通知器既能对接通用 JSON webhook，也能对接 MeoW 这类
// "路径里带昵称"的服务，以及其他接受纯文本/GET 模板的服务。
type WebhookConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	// Format: generic（默认，POST JSON）/ meow（MeoW 兼容）/ text（POST 纯文本）/ get（GET URL 模板）
	Format string `yaml:"format"`
	// Nickname 仅 meow 格式使用：MeoW 的推送昵称。
	Nickname string `yaml:"nickname"`
	// TitlePrefix 通知标题前缀，便于与其他系统的推送区分。
	TitlePrefix string `yaml:"title_prefix"`
	// TimeoutSeconds 单次投递超时（秒）。
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// MinIntervalSeconds 同一事件的最短重发间隔（秒），用于防止刷屏。
	MinIntervalSeconds int `yaml:"min_interval_seconds"`
	// NotifyKeyDisabled 永久失效 / 配额耗尽的 key 是否通知。
	NotifyKeyDisabled bool `yaml:"notify_key_disabled"`
	// NotifyTemporary 临时熔断（429/5xx）是否通知，默认关闭以免噪声。
	NotifyTemporary bool `yaml:"notify_temporary"`
	// NotifyNoKeys 没有任何可用 key 时是否通知。
	NotifyNoKeys bool `yaml:"notify_no_keys"`
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

	// Source 记录 Path 是怎么定下来的，仅用于启动日志，不参与任何持久化。
	// 取值：命令行 / 环境变量 / 当前目录 / 程序目录 / 默认。
	Source string `yaml:"-"`
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

// Defaults 返回内置默认配置（不读文件、不读环境变量）。
func Defaults() *Config {
	return &Config{
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
		Webhook: WebhookConfig{
			Format:             "generic",
			TitlePrefix:        "DeepL Proxy",
			TimeoutSeconds:     8,
			MinIntervalSeconds: 600,
			NotifyKeyDisabled:  true,
			NotifyNoKeys:       true,
		},
	}
}

// Get 让 *Config 直接满足"每请求读取当前配置"的接口（见 handler.ConfigSource）。
func (c *Config) Get() *Config { return c }

// Flags 是命令行可覆盖的启动参数。
type Flags struct {
	Port   string
	DBPath string
}

// ParseArgs 解析命令行参数，支持 -port / --port、-db-path / --db-path。
//
// 帮助信息由 flags 自己打印；-h/--help 返回 flag.ErrHelp，调用方据此正常退出。
func ParseArgs(args []string) (Flags, error) {
	fs := flag.NewFlagSet("deepl-proxy", flag.ContinueOnError)
	var f Flags
	fs.StringVar(&f.Port, "port", "", "监听端口（优先于 PORT 环境变量与数据库中的设置）")
	fs.StringVar(&f.DBPath, "db-path", "", "SQLite 数据库路径（优先于 DB_PATH）；不传时依次在当前目录、程序所在目录查找")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "用法: deepl-proxy [选项]\n\n")
		fmt.Fprintf(out, "不传 -db-path 时，按「当前目录 → 程序所在目录」的顺序查找：\n")
		fmt.Fprintf(out, "  data/deepl-proxy.db 或 deepl-proxy.db\n")
		fmt.Fprintf(out, "都找不到时在 当前目录/data/deepl-proxy.db 新建。\n\n子命令:\n")
		fmt.Fprintf(out, "  reset-admin   不启动服务，直接重置管理员令牌（deepl-proxy reset-admin --help）\n\n选项:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return Flags{}, err
	}
	if fs.NArg() > 0 {
		return Flags{}, fmt.Errorf("无法识别的参数: %s", strings.Join(fs.Args(), " "))
	}
	return f, nil
}

// Load 返回启动基线配置（不解析命令行）。
//
// 注意：从 v2 起配置不再读 config.yaml —— 运行期设置（令牌、缓存、通知等）
// 都存在数据库的 app_settings 表并由管理后台维护；这里只保留启动前必须
// 确定的两项（监听端口、数据库路径）以及一次性迁移用的环境变量。
func Load() *Config {
	cfg, _ := LoadFrom(nil)
	return cfg
}

// LoadFrom 解析命令行并返回启动基线配置。
//
// 优先级：命令行 > 环境变量 > 内置默认值。数据库路径还会做一次"找库"：
// 显式给了就不用找；否则先看 DB_PATH，再看当前目录、程序所在目录。
func LoadFrom(args []string) (*Config, error) {
	flags, err := ParseArgs(args)
	if err != nil {
		return nil, err
	}

	cfg := Defaults()
	applyEnv(cfg)

	if flags.Port != "" {
		cfg.Server.Port = flags.Port
	}

	switch {
	case flags.DBPath != "":
		cfg.Database.Path = filepath.Clean(flags.DBPath)
		cfg.Database.Source = "命令行"
	case os.Getenv("DB_PATH") != "":
		cfg.Database.Source = "环境变量"
	default:
		cfg.Database.Path, cfg.Database.Source = DiscoverDBPath()
	}

	return cfg, nil
}

// DiscoverDBPath 在"当前目录 → 程序所在目录"里找已有的库文件。
//
// 每个目录依次尝试 data/deepl-proxy.db、deepl-proxy.db；都找不到时
// 返回 当前目录/data/deepl-proxy.db（首次启动会创建）。
func DiscoverDBPath() (string, string) {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "."
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	return ResolveDBPath("", cwd, exeDir)
}

// ResolveDBPath 是 DiscoverDBPath 的纯函数版本，方便测试。
func ResolveDBPath(explicit, cwd, exeDir string) (string, string) {
	if explicit != "" {
		return filepath.Clean(explicit), "命令行"
	}

	dirs := []struct {
		dir    string
		source string
	}{
		{cwd, "当前目录"},
	}
	if exeDir != "" && filepath.Clean(exeDir) != filepath.Clean(cwd) {
		dirs = append(dirs, struct {
			dir    string
			source string
		}{exeDir, "程序目录"})
	}

	for _, d := range dirs {
		for _, rel := range []string{filepath.Join("data", "deepl-proxy.db"), "deepl-proxy.db"} {
			candidate := filepath.Join(d.dir, rel)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, d.source
			}
		}
	}

	return filepath.Join(cwd, "data", "deepl-proxy.db"), "默认"
}

// applyEnv 应用启动类环境变量。DB 里的设置优先级更高，这里只是首次启动的基线。
func applyEnv(cfg *Config) {
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
	if v := os.Getenv("DB_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("WEBHOOK_URL"); v != "" {
		cfg.Webhook.URL = v
	}
	if v := os.Getenv("WEBHOOK_FORMAT"); v != "" {
		cfg.Webhook.Format = v
	}
	if v := os.Getenv("WEBHOOK_ENABLED"); v != "" {
		cfg.Webhook.Enabled = v == "1" || v == "true" || v == "TRUE" || v == "yes"
	}
	cfg.Webhook = cfg.Webhook.normalized()
}

// ImportLegacy 读取旧的 config.yaml（如果存在）。
//
// 只为一次性迁移保留：解析成功后由调用方写入数据库，之后该文件不再被读取。
// 返回的 error 仅表示"文件存在但解析失败"，文件不存在时返回 (nil, false, nil)。
func ImportLegacy(path string) (*Config, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, false, err
	}
	// 迁移时让环境变量覆盖文件值：环境变量通常由部署平台注入，更新更及时。
	applyEnv(cfg)
	return cfg, true, nil
}

// Normalized 返回补齐默认值并归一化枚举后的副本，供运行时热更新使用。
func (c WebhookConfig) Normalized() WebhookConfig { return c.normalized() }

// normalized 补齐零值默认值并归一化枚举值。
//
// 这样即使有人手工 new 一个 Config（测试、未来嵌入其他程序），
// 也不会因为 Format 为空而走到未定义分支。
func (c WebhookConfig) normalized() WebhookConfig {
	if strings.TrimSpace(c.Format) == "" {
		c.Format = "generic"
	}
	c.Format = strings.ToLower(strings.TrimSpace(c.Format))
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 8
	}
	if c.MinIntervalSeconds < 0 {
		c.MinIntervalSeconds = 0
	}
	return c
}

// Configured 报告通知器是否具备投递条件。
//
// meow 格式允许 URL 留空（默认 https://api.chuckfang.com），
// 但必须提供 nickname；其余格式必须给出完整 URL。
func (c WebhookConfig) Configured() bool {
	if !c.Enabled {
		return false
	}
	if c.Format == "meow" {
		return strings.TrimSpace(c.Nickname) != ""
	}
	return strings.TrimSpace(c.URL) != ""
}
