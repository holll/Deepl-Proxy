package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"deepl-proxy/config"
	"deepl-proxy/database"
)

func newSettingsForTest(t *testing.T, mutate func(*config.Config)) (*Settings, func()) {
	t.Helper()
	dir := t.TempDir()
	writeDB, readDB, err := database.Init(context.Background(), filepath.Join(dir, "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	base := config.Defaults()
	base.Database.Path = filepath.Join(dir, "settings.db")
	if mutate != nil {
		mutate(base)
	}
	st, err := NewSettings(context.Background(), writeDB, base)
	if err != nil {
		t.Fatal(err)
	}
	return st, func() {
		_ = writeDB.Close()
		_ = readDB.Close()
	}
}

func TestSettingsSetupFlow(t *testing.T) {
	st, cleanup := newSettingsForTest(t, nil)
	defer cleanup()

	if !st.SetupRequired() {
		t.Fatal("全新数据库应要求初始化")
	}
	if _, err := st.Setup(context.Background(), "short", "", ""); err == nil {
		t.Fatal("过短的管理员令牌应被拒绝")
	}
	cfg, err := st.Setup(context.Background(), "setup-token-123", "gw-token-123", "")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if st.SetupRequired() {
		t.Fatal("初始化后不应再要求 setup")
	}
	if cfg.Auth.AdminCookieSecret == "" {
		t.Fatal("应自动生成会话签名密钥")
	}
	// 重复初始化必须失败，避免覆盖已有令牌。
	if _, err := st.Setup(context.Background(), "another-token-123", "", ""); err == nil {
		t.Fatal("重复初始化应被拒绝")
	}
}

func TestSettingsUpdatePersistsAndValidates(t *testing.T) {
	st, cleanup := newSettingsForTest(t, nil)
	defer cleanup()
	ctx := context.Background()
	if _, err := st.Setup(ctx, "setup-token-123", "", ""); err != nil {
		t.Fatal(err)
	}

	ttl := "7200"
	if _, err := st.Update(ctx, map[string]*string{SettingSessionTTL: &ttl}); err != nil {
		t.Fatal(err)
	}
	if got := st.Get().Auth.SessionTTL; got != 7200 {
		t.Fatalf("session ttl = %d", got)
	}

	// 端口非法：拒绝且不落库。
	bad := "70000"
	if _, err := st.Update(ctx, map[string]*string{SettingServerPort: &bad}); err == nil {
		t.Fatal("非法端口应被拒绝")
	}
	if got := st.Get().Server.Port; got == bad {
		t.Fatal("校验失败不应改变生效配置")
	}

	// 清空覆盖：回落到基线默认值。
	empty := ""
	if _, err := st.Update(ctx, map[string]*string{SettingSessionTTL: &empty}); err != nil {
		t.Fatal(err)
	}
	if got := st.Get().Auth.SessionTTL; got != config.Defaults().Auth.SessionTTL {
		t.Fatalf("清除覆盖后应回落默认值，得到 %d", got)
	}
}

func TestSettingsCannotClearAdminToken(t *testing.T) {
	st, cleanup := newSettingsForTest(t, nil)
	defer cleanup()
	if _, err := st.Setup(context.Background(), "setup-token-123", "", ""); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if _, err := st.Update(context.Background(), map[string]*string{SettingAdminToken: &empty}); err == nil {
		t.Fatal("清空管理员令牌应被拒绝")
	}
}

func TestSettingsListenerNotifiesOnChange(t *testing.T) {
	st, cleanup := newSettingsForTest(t, nil)
	defer cleanup()
	if _, err := st.Setup(context.Background(), "setup-token-123", "", ""); err != nil {
		t.Fatal(err)
	}
	var calls int
	st.AddListener(func(*config.Config) { calls++ })
	v := "18080"
	if _, err := st.Update(context.Background(), map[string]*string{SettingServerPort: &v}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("listener calls = %d", calls)
	}
}

func TestWebhookManagerSendsAfterSettingsUpdate(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		got <- payload
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st, cleanup := newSettingsForTest(t, nil)
	defer cleanup()
	if _, err := st.Setup(context.Background(), "setup-token-123", "", ""); err != nil {
		t.Fatal(err)
	}
	wm := NewWebhookManager(context.Background(), st)
	defer wm.Close()

	// 先启用：通过设置层保存，通知器应立刻使用新配置。
	enabled, url, format := "true", srv.URL, "generic"
	minInterval := "0"
	if _, err := st.UpdateWebhook(context.Background(), map[string]*string{
		WebhookSettingEnabled:            &enabled,
		WebhookSettingURL:                &url,
		WebhookSettingFormat:             &format,
		WebhookSettingMinIntervalSeconds: &minInterval,
	}); err != nil {
		t.Fatal(err)
	}

	if err := wm.Test(context.Background()); err != nil {
		t.Fatalf("test send: %v", err)
	}
	payload := <-got
	if payload["event"] != "test" || payload["service"] != "deepl-proxy" {
		t.Fatalf("payload = %+v", payload)
	}

	// 保存非法格式应被拒绝，且不影响已生效配置。
	badFmt := "telegram"
	if _, err := st.UpdateWebhook(context.Background(), map[string]*string{WebhookSettingFormat: &badFmt}); err == nil {
		t.Fatal("非法格式应被拒绝")
	}
}

func TestResetAdminToken(t *testing.T) {
	st, cleanup := newSettingsForTest(t, nil)
	defer cleanup()
	ctx := context.Background()

	if _, err := st.Setup(ctx, "old-admin-token", "old-gateway-token", "*"); err != nil {
		t.Fatal(err)
	}
	oldSecret := st.Get().Auth.AdminCookieSecret
	if oldSecret == "" {
		t.Fatal("初始化后应当有会话密钥")
	}

	if _, err := st.ResetAdminToken(ctx, "short"); err == nil {
		t.Fatal("少于 8 位应被拒绝")
	}

	cfg, err := st.ResetAdminToken(ctx, "  new-admin-token  ")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.AdminToken != "new-admin-token" {
		t.Fatalf("token = %q", cfg.Auth.AdminToken)
	}
	if cfg.Auth.AdminCookieSecret == oldSecret {
		t.Fatal("会话密钥必须轮换")
	}
	// 网关令牌不应被重置流程改动。
	if cfg.Auth.GatewayToken != "old-gateway-token" {
		t.Fatalf("gateway = %q", cfg.Auth.GatewayToken)
	}

	// 重新从库里读一遍，确认真的落盘了。
	st2, err := NewSettings(ctx, st.db, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Get().Auth.AdminToken != "new-admin-token" {
		t.Fatalf("persisted token = %q", st2.Get().Auth.AdminToken)
	}
	if st2.SetupRequired() {
		t.Fatal("重置后不应处于待初始化状态")
	}
}

func TestGenerateAdminToken(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		token, err := GenerateAdminToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) < 8 {
			t.Fatalf("token too short: %q", token)
		}
		if seen[token] {
			t.Fatalf("重复令牌: %q", token)
		}
		seen[token] = true
	}
}
