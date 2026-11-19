package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/service"
)

// TestAdminSetupFirstRun 验证全新实例没有令牌时的首次初始化流程。
func TestAdminSetupFirstRun(t *testing.T) {
	h := newHarnessWith(t, 1, func(cfg *config.Config) {
		cfg.Auth.AdminToken = ""
		cfg.Auth.GatewayToken = ""
		cfg.Auth.AdminCookieSecret = ""
	})

	status, _, body := h.do(http.MethodGet, "/admin/session", "", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("session status = %d", status)
	}
	var session struct {
		Authenticated bool `json:"authenticated"`
		SetupRequired bool `json:"setup_required"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatal(err)
	}
	if session.Authenticated || !session.SetupRequired {
		t.Fatalf("session = %+v", session)
	}

	// 未初始化时登录必须被拒绝，而不是拿空令牌蒙混过关。
	status, _, _ = h.do(http.MethodPost, "/admin/login", "application/json",
		[]byte(`{"admin_token":""}`), nil)
	if status != http.StatusConflict {
		t.Fatalf("login before setup status = %d", status)
	}

	status, _, body = h.do(http.MethodPost, "/admin/setup", "application/json",
		[]byte(`{"admin_token":"setup-token-123","gateway_token":"gw-setup-token"}`), nil)
	if status != http.StatusOK {
		t.Fatalf("setup status = %d body %s", status, body)
	}

	// 初始化后立即可以访问管理接口。
	status, _, body = h.do(http.MethodGet, "/admin/keys", "", nil,
		map[string]string{"Authorization": "Bearer setup-token-123"})
	if status != http.StatusOK {
		t.Fatalf("keys after setup status = %d body %s", status, body)
	}

	// 重复初始化会被拒绝。
	status, _, _ = h.do(http.MethodPost, "/admin/setup", "application/json",
		[]byte(`{"admin_token":"other-token-123"}`), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("second setup status = %d", status)
	}
}

func TestAdminRotateSecretSignsOutCurrentBrowser(t *testing.T) {
	h := newHarness(t, 1)
	login := func() map[string]string {
		t.Helper()
		status, hdr, body := h.do(http.MethodPost, "/admin/login", "application/json", mustJSON(t, map[string]string{"admin_token": testAdminToken}), nil)
		assertStatus(t, status, http.StatusOK, body)
		cookie, _, _ := strings.Cut(hdr.Get("Set-Cookie"), ";")
		if cookie == "" {
			t.Fatal("login did not issue a session cookie")
		}
		return map[string]string{"Cookie": cookie}
	}
	assertSession := func(headers map[string]string, want bool) {
		t.Helper()
		status, hdr, body := h.do(http.MethodGet, "/admin/session", "", nil, headers)
		assertStatus(t, status, http.StatusOK, body)
		var result map[string]bool
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if got, ok := result["authenticated"]; !ok || got != want || hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("session = %s, Cache-Control = %q, want authenticated = %v and no-store", body, hdr.Get("Cache-Control"), want)
		}
	}

	oldCookie := login()
	assertSession(oldCookie, true)
	status, hdr, body := h.do(http.MethodPost, "/admin/settings/rotate-secret", "", nil, oldCookie)
	assertStatus(t, status, http.StatusOK, body)
	cookieName, _, _ := strings.Cut(oldCookie["Cookie"], "=")
	cookies := (&http.Response{Header: hdr}).Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || cookies[0].Value != "" || cookies[0].MaxAge >= 0 || cookies[0].Path != "/" {
		t.Fatalf("rotation must expire the existing cookie, got %q", hdr.Values("Set-Cookie"))
	}
	assertSession(nil, false)
	assertSession(oldCookie, false)
	status, _, body = h.do(http.MethodGet, "/admin/keys", "", nil, oldCookie)
	assertStatus(t, status, http.StatusUnauthorized, body)
	assertSession(login(), true)
}

// TestAdminSettingsLiveReload 验证设置保存后，令牌类修改立即生效（无需重启）。
func TestAdminSettingsLiveReload(t *testing.T) {
	h := newHarness(t, 1)

	status, body := h.admin(http.MethodGet, "/admin/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("get settings status = %d body %s", status, body)
	}

	// 轮换网关令牌：旧令牌立刻失效，新令牌立刻可用。
	status, body = h.admin(http.MethodPut, "/admin/settings", map[string]any{
		"gateway_token": "gw-rotated-token",
	})
	if status != http.StatusOK {
		t.Fatalf("put settings status = %d body %s", status, body)
	}

	h.addKey("rotated", "deepl")
	payload := map[string]any{"text": []string{"hi"}, "target_lang": "ZH"}
	status, _, _ = h.do(http.MethodPost, "/v2/translate", "application/json", mustJSON(t, payload),
		map[string]string{"Authorization": "Bearer gw-rotated-token"})
	if status != http.StatusOK {
		t.Fatalf("translate with rotated token status = %d", status)
	}
	status, _, _ = h.do(http.MethodPost, "/v2/translate", "application/json", mustJSON(t, payload),
		map[string]string{"Authorization": "Bearer " + testGatewayToken})
	if status != http.StatusUnauthorized {
		t.Fatalf("old gateway token should be rejected, got %d", status)
	}

	// 轮换管理员令牌：旧令牌失效、新令牌可用。
	status, body = h.admin(http.MethodPut, "/admin/settings", map[string]any{
		"admin_token": "rotated-admin-token",
	})
	if status != http.StatusOK {
		t.Fatalf("rotate admin token status = %d body %s", status, body)
	}
	status, _, _ = h.do(http.MethodGet, "/admin/keys", "", nil,
		map[string]string{"Authorization": "Bearer rotated-admin-token"})
	if status != http.StatusOK {
		t.Fatalf("new admin token status = %d", status)
	}
	status, _, _ = h.do(http.MethodGet, "/admin/keys", "", nil,
		map[string]string{"Authorization": "Bearer " + testAdminToken})
	if status != http.StatusUnauthorized {
		t.Fatalf("old admin token should be rejected, got %d", status)
	}
}

// TestAdminSettingsValidation 验证非法设置被拒绝且不影响已有配置。
func TestAdminSettingsValidation(t *testing.T) {
	h := newHarness(t, 1)

	cases := []struct {
		name   string
		patch  map[string]any
		expect int
	}{
		{"bad session ttl", map[string]any{"session_ttl": "5"}, http.StatusBadRequest},
		{"empty admin token", map[string]any{"admin_token": ""}, http.StatusBadRequest},
		{"ok ttl", map[string]any{"session_ttl": "7200"}, http.StatusOK},
	}
	for _, tc := range cases {
		status, body := h.admin(http.MethodPut, "/admin/settings", tc.patch)
		if status != tc.expect {
			t.Fatalf("%s: status = %d body %s", tc.name, status, body)
		}
	}
}

func TestAdminSettingsOmitsServiceFields(t *testing.T) {
	h := newHarness(t, 1)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var payload any
		if method == http.MethodPut {
			payload = map[string]any{"server_port": "9090", "allow_origin": "https://example.com", "database_path": "/tmp/ignored.db"}
		}
		status, body := h.admin(method, "/admin/settings", payload)
		if status != http.StatusOK {
			t.Fatalf("%s settings status = %d body %s", method, status, body)
		}
		var resp struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"server_port", "allow_origin", "database_path"} {
			if _, ok := resp.Settings[field]; ok {
				t.Errorf("%s settings still exposes %s", method, field)
			}
		}
		var restart []string
		if err := json.Unmarshal(resp.Settings["restart_required_for"], &restart); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(restart, "server_port") {
			t.Errorf("%s settings still lists server_port for restart", method)
		}
	}
	stored, err := database.GetAppSettings(t.Context(), h.writeDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{service.SettingServerPort, service.SettingAllowOrigin, "database.path"} {
		if _, ok := stored[key]; ok {
			t.Errorf("removed field was persisted as %s", key)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAdminSettingsWebhookNested 验证旧的短会话密钥不阻止保存 Webhook。
func TestAdminSettingsWebhookNested(t *testing.T) {
	h := newHarnessWith(t, 1, func(cfg *config.Config) {
		cfg.Auth.AdminCookieSecret = "legacy-short"
	})

	status, body := h.admin(http.MethodPut, "/admin/settings", map[string]any{
		"webhook": map[string]any{
			"enabled":      "true",
			"format":       "meow",
			"nickname":     "meow-test-user",
			"url":          "",
			"title_prefix": "DeepL Proxy",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("save webhook status = %d body %s", status, body)
	}

	status, body = h.admin(http.MethodGet, "/admin/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("get settings status = %d", status)
	}
	var resp struct {
		Settings struct {
			Webhook struct {
				Enabled  bool   `json:"enabled"`
				Format   string `json:"format"`
				Nickname string `json:"nickname"`
			} `json:"webhook"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Settings.Webhook.Enabled || resp.Settings.Webhook.Format != "meow" || resp.Settings.Webhook.Nickname != "meow-test-user" {
		t.Fatalf("webhook = %+v", resp.Settings.Webhook)
	}

	status, body = h.admin(http.MethodPut, "/admin/webhook", map[string]any{"nickname": "updated"})
	if status != http.StatusOK {
		t.Fatalf("save via dedicated webhook route status = %d body %s", status, body)
	}

	for _, tc := range []struct {
		path  string
		patch map[string]any
	}{
		{"/admin/settings", map[string]any{"webhook": map[string]any{"format": "telegram"}}},
		{"/admin/webhook", map[string]any{"format": "telegram"}},
	} {
		status, body = h.admin(http.MethodPut, tc.path, tc.patch)
		if status != http.StatusBadRequest || strings.Contains(string(body), "会话签名密钥") {
			t.Errorf("invalid webhook format via %s: status = %d body %s", tc.path, status, body)
		}
	}

	status, body = h.admin(http.MethodPut, "/admin/settings", map[string]any{"session_ttl": "7200"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "会话签名密钥长度不能少于 16") {
		t.Errorf("auth validation was bypassed: status = %d body %s", status, body)
	}
}
