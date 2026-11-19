package service

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"deepl-proxy/config"
)

// Webhook 运行时设置的键名。
//
// 这些键同时是 app_settings 表里的主键和 /admin/webhook 的字段名，
// 修改会破坏已保存的覆盖项，因此不要重命名。
const (
	WebhookSettingEnabled            = "enabled"
	WebhookSettingURL                = "url"
	WebhookSettingFormat             = "format"
	WebhookSettingNickname           = "nickname"
	WebhookSettingTitlePrefix        = "title_prefix"
	WebhookSettingTimeoutSeconds     = "timeout_seconds"
	WebhookSettingMinIntervalSeconds = "min_interval_seconds"
	WebhookSettingNotifyKeyDisabled  = "notify_key_disabled"
	WebhookSettingNotifyTemporary    = "notify_temporary"
	WebhookSettingNotifyNoKeys       = "notify_no_keys"
)

// WebhookSettingKeys 是全部可由管理后台覆盖的字段。
func WebhookSettingKeys() []string {
	return []string{
		WebhookSettingEnabled,
		WebhookSettingURL,
		WebhookSettingFormat,
		WebhookSettingNickname,
		WebhookSettingTitlePrefix,
		WebhookSettingTimeoutSeconds,
		WebhookSettingMinIntervalSeconds,
		WebhookSettingNotifyKeyDisabled,
		WebhookSettingNotifyTemporary,
		WebhookSettingNotifyNoKeys,
	}
}

// ResolveWebhookConfig 把 config.yaml / 环境变量得到的基线配置与数据库覆盖项合并。
//
// 覆盖规则是"逐字段、非空才覆盖"：空字符串表示该字段沿用基线，
// 这样用户在后台只改昵称时，不会把环境变量里注入的 URL 一并清掉。
func ResolveWebhookConfig(base config.WebhookConfig, overrides map[string]string) config.WebhookConfig {
	cfg := base
	get := func(k string) string {
		v, ok := overrides[k]
		if !ok {
			return ""
		}
		return strings.TrimSpace(v)
	}

	if v := get(WebhookSettingURL); v != "" {
		cfg.URL = v
	}
	if v := get(WebhookSettingFormat); v != "" {
		cfg.Format = strings.ToLower(v)
	}
	if v := get(WebhookSettingNickname); v != "" {
		cfg.Nickname = v
	}
	if v := get(WebhookSettingTitlePrefix); v != "" {
		cfg.TitlePrefix = v
	}
	if v := get(WebhookSettingEnabled); v != "" {
		cfg.Enabled = parseBool(v)
	}
	if v := get(WebhookSettingNotifyKeyDisabled); v != "" {
		cfg.NotifyKeyDisabled = parseBool(v)
	}
	if v := get(WebhookSettingNotifyTemporary); v != "" {
		cfg.NotifyTemporary = parseBool(v)
	}
	if v := get(WebhookSettingNotifyNoKeys); v != "" {
		cfg.NotifyNoKeys = parseBool(v)
	}
	if v := get(WebhookSettingTimeoutSeconds); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TimeoutSeconds = n
		}
	}
	if v := get(WebhookSettingMinIntervalSeconds); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MinIntervalSeconds = n
		}
	}
	return cfg.Normalized()
}

// ValidateWebhookConfig 校验合并后的配置。
//
// 允许"先填一半"：未启用时任何字段都可以留空；一旦启用，
// 就必须是该格式能真正发出去的完整配置，否则保存直接报错，
// 避免出现"保存成功但一条也发不出"的静默失败。
func ValidateWebhookConfig(cfg config.WebhookConfig) error {
	switch cfg.Format {
	case "generic", "meow", "text", "get":
	default:
		return fmt.Errorf("不支持的格式 %q（可选 generic / meow / text / get）", cfg.Format)
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 60 {
		return fmt.Errorf("超时秒数需在 1–60 之间")
	}
	if cfg.MinIntervalSeconds < 0 || cfg.MinIntervalSeconds > 86400 {
		return fmt.Errorf("最小通知间隔需在 0–86400 秒之间")
	}
	if len([]rune(cfg.TitlePrefix)) > 60 {
		return fmt.Errorf("标题前缀不能超过 60 个字符")
	}
	if cfg.URL != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("URL 必须是 http(s) 完整地址")
		}
	}
	if cfg.Nickname != "" && strings.ContainsAny(cfg.Nickname, "/?#") {
		return fmt.Errorf("昵称不能包含 / ? # 字符")
	}
	if !cfg.Enabled {
		return nil
	}
	if cfg.Format == "meow" {
		if strings.TrimSpace(cfg.Nickname) == "" {
			return fmt.Errorf("meow 格式需要填写昵称")
		}
		return nil
	}
	if strings.TrimSpace(cfg.URL) == "" {
		return fmt.Errorf("启用 webhook 需要填写 URL")
	}
	return nil
}

// WebhookEnvKeys 返回当前由环境变量提供（或覆盖）的字段，供后台提示"值来自环境变量"。
func WebhookEnvKeys() []string {
	out := make([]string, 0, 3)
	if strings.TrimSpace(os.Getenv("WEBHOOK_URL")) != "" {
		out = append(out, WebhookSettingURL)
	}
	if strings.TrimSpace(os.Getenv("WEBHOOK_FORMAT")) != "" {
		out = append(out, WebhookSettingFormat)
	}
	if strings.TrimSpace(os.Getenv("WEBHOOK_ENABLED")) != "" {
		out = append(out, WebhookSettingEnabled)
	}
	return out
}

// WebhookManager 管理 webhook 配置的读取、保存与热更新。
//
// 配置与其余运行期设置一起存在 app_settings 表（键名前缀 webhook.），
// 保存后立即对新的投递生效，不需要重启进程。
type WebhookManager struct {
	ctx      context.Context
	settings *Settings
	notifier *Notifier
}

// NewWebhookManager 基于当前设置创建通知器，并订阅后续配置变更。
func NewWebhookManager(ctx context.Context, settings *Settings) *WebhookManager {
	n := NewNotifier(ctx, settings.Get().Webhook)
	settings.AddListener(func(cfg *config.Config) { n.Update(cfg.Webhook) })
	return &WebhookManager{ctx: ctx, settings: settings, notifier: n}
}

// Notifier 返回当前通知器。
func (m *WebhookManager) Notifier() *Notifier { return m.notifier }

// Snapshot 返回生效配置与显式覆盖项。
func (m *WebhookManager) Snapshot() (config.WebhookConfig, map[string]string) {
	return m.settings.WebhookConfig()
}

// Update 保存补丁（nil 不改、空串清除覆盖）并热更新通知器。
func (m *WebhookManager) Update(ctx context.Context, patch map[string]*string) (config.WebhookConfig, error) {
	return m.settings.UpdateWebhook(ctx, patch)
}

// Test 立即发一条测试通知，错误返回给调用方展示。
func (m *WebhookManager) Test(ctx context.Context) error {
	return m.notifier.SendNow(ctx, testNotification())
}

// TestWithOverrides 用"当前配置 + 表单patch"发送测试通知。
//
// 这样用户可以先不保存就验证 url / 昵称是否写对。
func (m *WebhookManager) TestWithOverrides(ctx context.Context, overrides map[string]string) error {
	base, _ := m.settings.WebhookConfig()
	cfg := ResolveWebhookConfig(base, overrides)
	return m.notifier.SendNowWithConfig(ctx, cfg, testNotification())
}

func testNotification() Notification {
	return Notification{
		Event:   "test",
		Title:   "测试通知",
		Message: "这是一条来自 DeepL Proxy 管理后台的测试通知。",
		Level:   "info",
		Data: map[string]any{
			"source": "admin_console",
			"time":   time.Now().Format(time.RFC3339),
		},
	}
}

// Close 停止通知器。
func (m *WebhookManager) Close() {
	if m != nil && m.notifier != nil {
		m.notifier.Close()
	}
}
