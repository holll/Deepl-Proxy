package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/provider"
)

// newQuotaTestRing 建一个只有一条 key 的 keyring，并在需要时挂上通知器。
func newQuotaTestRing(t *testing.T, endpoint string, limit int64, quotaPeriod string) (*Keyring, context.Context) {
	t.Helper()
	writeDB, readDB, err := database.Init(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writeDB.Close()
		_ = readDB.Close()
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	kr := NewKeyring(ctx, writeDB, readDB)
	t.Cleanup(kr.Close)

	id, err := kr.AddKey(ctx, "quota-key", "auth-key", endpoint, "deepl", quotaPeriod)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateUsageSnapshot(ctx, writeDB, id, nil, &limit, 0); err != nil {
		t.Fatal(err)
	}
	return kr, ctx
}

// captureNotifications 起一个 webhook 接收端，把收到的通知转成 channel。
func captureNotifications(t *testing.T) (*Notifier, <-chan map[string]any) {
	t.Helper()
	events := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			events <- payload
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	n := NewNotifier(t.Context(), config.WebhookConfig{
		Enabled:           true,
		URL:               srv.URL,
		Format:            "generic",
		TimeoutSeconds:    2,
		NotifyKeyDisabled: true,
		NotifyNoKeys:      true,
	})
	t.Cleanup(n.Close)
	return n, events
}

func waitEvent(t *testing.T, events <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no webhook notification delivered")
		return nil
	}
}

func TestQuotaExhaustedDisablesUntilYearlyReset(t *testing.T) {
	// 直接驱动 applyDisable：456 被判定为 monthly（官方站配额）后的落库行为。
	// 上游站点类型由 ClassifyError 决定，这里只验证"用哪个周期算重置时刻"。
	kr, ctx := newQuotaTestRing(t, "https://api.deepl.com", 1_000_000, QuotaPeriodAuto)
	notifier, events := captureNotifications(t)
	kr.SetNotifier(notifier)

	keys, err := kr.GetActiveKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, err = %v", len(keys), err)
	}
	kr.applyDisable(ctx, keys[0], "monthly", 456, `{"message":"Quota exceeded"}`, "", nil)

	key, err := database.GetKeyByID(ctx, kr.DB(), keys[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if key.Status != "disabled" || key.DisableType == nil || *key.DisableType != "quota" {
		t.Fatalf("status=%s disable_type=%v", key.Status, key.DisableType)
	}
	wantReset := QuotaResetAt(time.Now(), QuotaPeriodYearly).UnixMilli()
	if key.DisabledUntil == nil || *key.DisabledUntil != wantReset {
		t.Fatalf("disabled_until = %v, want yearly reset %d", key.DisabledUntil, wantReset)
	}

	event := waitEvent(t, events)
	if event["event"] != EventQuotaExhausted {
		t.Fatalf("event = %v", event["event"])
	}
	data, _ := event["data"].(map[string]any)
	if data["quota_period"] != QuotaPeriodYearly {
		t.Fatalf("quota_period = %v, want yearly", data["quota_period"])
	}
}

func TestQuotaExhaustedUsesMonthlyForLegacyLimit(t *testing.T) {
	// 旧免费 key：50 万/月，只能停到下月 1 日。
	kr, ctx := newQuotaTestRing(t, "https://api.deepl.com", 500_000, QuotaPeriodAuto)
	keys, err := kr.GetActiveKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, err = %v", len(keys), err)
	}
	kr.applyDisable(ctx, keys[0], "monthly", 456, `{"message":"Quota exceeded"}`, "", nil)

	key, err := database.GetKeyByID(ctx, kr.DB(), keys[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wantReset := QuotaResetAt(time.Now(), QuotaPeriodMonthly).UnixMilli()
	if key.DisabledUntil == nil || *key.DisabledUntil != wantReset {
		t.Fatalf("disabled_until = %v, want monthly reset %d", key.DisabledUntil, wantReset)
	}
}

func TestPermanentFailureNotifiesKeyDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Wrong endpoint"}`))
	}))
	defer upstream.Close()

	kr, ctx := newQuotaTestRing(t, upstream.URL, 1_000_000, QuotaPeriodAuto)
	notifier, events := captureNotifications(t)
	kr.SetNotifier(notifier)

	keys, _ := kr.GetActiveKeys(ctx)
	sel, _ := kr.Select(keys, 1, nil)
	if _, _, _, err := kr.Translate(ctx, sel, &provider.TranslateRequest{
		Text: []string{"hello"}, TargetLang: "ZH", FormParams: "text=hello&target_lang=ZH",
	}); err == nil {
		t.Fatal("expected translate error")
	}

	key, err := database.GetKeyByID(ctx, kr.DB(), keys[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if key.Status != "dead" || key.DisableType == nil || *key.DisableType != "permanent" {
		t.Fatalf("status=%s disable_type=%v", key.Status, key.DisableType)
	}
	event := waitEvent(t, events)
	if event["event"] != EventKeyDisabled {
		t.Fatalf("event = %v, want %s", event["event"], EventKeyDisabled)
	}
}

func TestNotifyNoKeysDeduplicated(t *testing.T) {
	kr, ctx := newQuotaTestRing(t, "https://api.deepl.com", 500_000, QuotaPeriodAuto)
	var mu sync.Mutex
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := NewNotifier(ctx, config.WebhookConfig{
		Enabled:            true,
		URL:                srv.URL,
		Format:             "generic",
		TimeoutSeconds:     2,
		MinIntervalSeconds: 600,
		NotifyNoKeys:       true,
	})
	t.Cleanup(n.Close)
	kr.SetNotifier(n)

	kr.NotifyNoKeys("没有可用 key")
	kr.NotifyNoKeys("没有可用 key")
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	got := count
	mu.Unlock()
	if got != 1 {
		t.Fatalf("no_keys deliveries = %d, want 1 (deduped)", got)
	}
}
