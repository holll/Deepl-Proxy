package service

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"deepl-proxy/database"
	"deepl-proxy/models"
	"deepl-proxy/provider"
)

func TestTemporaryCooldown(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status int
		value  string
		want   time.Duration
	}{
		{"seconds", 429, " 45 ", 45 * time.Second},
		{"date", 429, now.Add(12 * time.Minute).Format(http.TimeFormat), 12 * time.Minute},
		{"past date", 429, now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"zero seconds", 429, "0", 0},
		{"missing", 429, "", 5 * time.Minute},
		{"invalid", 429, "later", 5 * time.Minute},
		{"negative", 429, "-1", 5 * time.Minute},
		{"overflow", 429, "9223372037", 5 * time.Minute},
		{"service unavailable", 503, "15", 5 * time.Minute},
		{"other status", 529, "15", 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := temporaryCooldown(tc.status, tc.value, now); got != tc.want {
				t.Fatalf("temporaryCooldown = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUpstream429SetsKeyCooldown(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		usage          bool
	}{
		{"DeepL translation", "deepl", false},
		{"DeepLX translation", "deeplx", false},
		{"DeepL usage", "deepl", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cooldown = 45 * time.Second
			ctx := t.Context()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(http.StatusTooManyRequests)
				// The explicit status must win even if the body contains a quota keyword.
				_, _ = w.Write([]byte("quota exceeded"))
			}))
			defer server.Close()
			w, r, err := database.Init(ctx, filepath.Join(t.TempDir(), "keys.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			defer r.Close()
			kr := NewKeyring(ctx, w, r)
			defer kr.Close()
			id, err := database.InsertKey(ctx, w, "test-key", "fake-secret", server.URL, tc.provider, "auto")
			if err != nil {
				t.Fatal(err)
			}
			key, err := database.GetKeyByID(ctx, r, id)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if tc.usage {
				results := kr.RefreshUsage(ctx)
				if len(results) != 1 || results[0].Status != http.StatusTooManyRequests {
					t.Fatalf("RefreshUsage = %+v, want one 429 result", results)
				}
			} else {
				_, _, failure, err := kr.Translate(ctx, Selection{Keys: []models.DeeplKey{*key}}, &provider.TranslateRequest{Text: []string{"hello"}, TargetLang: "DE"})
				if err == nil || failure == nil || failure.Status != http.StatusTooManyRequests {
					t.Fatalf("failure = %+v, err = %v, want 429", failure, err)
				}
			}
			end := time.Now()
			key, err = database.GetKeyByID(ctx, r, id)
			if err != nil {
				t.Fatal(err)
			}
			if key.Status != "disabled" || key.DisableType == nil || *key.DisableType != "temporary" || key.DisabledUntil == nil {
				t.Fatalf("status = %s, disable type = %v, disabled until = %v; want temporary cooldown", key.Status, key.DisableType, key.DisabledUntil)
			}
			if got := *key.DisabledUntil; got < start.Add(cooldown).UnixMilli() || got > end.Add(cooldown).UnixMilli() {
				t.Fatalf("disabled_until = %d, want now + %s", got, cooldown)
			}
		})
	}
}
