package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"deepl-proxy/database"
	"deepl-proxy/models"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

func TestCacheListIgnoresSearch(t *testing.T) {
	writeDB, readDB, err := database.Init(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writeDB.Close()
		_ = readDB.Close()
	})
	for i, key := range []string{"needle-key", "other-key", "newest-key"} {
		if err := database.UpsertCache(context.Background(), writeDB, &models.TranslateCache{
			CacheKey: key, Body: []byte(`{"translations":[{"text":"needle body"}]}`),
			HeadersJSON: "{}", CreatedAt: int64(i + 1), ExpiresAt: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	kr := service.NewKeyring(t.Context(), writeDB, readDB)
	t.Cleanup(kr.Close)
	r.GET("/admin/cache", handleCacheList(kr))
	r.DELETE("/admin/cache", handleCacheDelete(kr))

	for _, tc := range []struct {
		name, query          string
		limit, offset, count int
		first                string
	}{
		{"normal page", "?limit=1&offset=1", 1, 1, 1, "other-key"},
		{"key search ignored", "?search=needle-key&limit=1&offset=1", 1, 1, 1, "other-key"},
		{"body search ignored", "?search=needle%20body&limit=1&offset=1", 1, 1, 1, "other-key"},
		{"no match ignored", "?search=does-not-exist&limit=1&offset=1", 1, 1, 1, "other-key"},
		{"wildcards ignored", "?search=%25_&limit=1&offset=1", 1, 1, 1, "other-key"},
		{"empty search ignored", "?search=&limit=1&offset=1", 1, 1, 1, "other-key"},
		{"repeated search ignored", "?search=needle-key&search=missing&limit=1&offset=1", 1, 1, 1, "other-key"},
		{"default page", "", 50, 0, 3, "newest-key"},
		{"invalid pagination", "?limit=501&offset=-1&search=missing", 50, 0, 3, "newest-key"},
		{"past end", "?limit=1&offset=3&search=missing", 1, 3, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/cache"+tc.query, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var got struct {
				Entries []struct {
					CacheKey string `json:"cache_key"`
					Preview  string `json:"preview"`
					Expired  bool   `json:"expired"`
				} `json:"entries"`
				Total  int `json:"total"`
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Total != 3 || got.Limit != tc.limit || got.Offset != tc.offset || len(got.Entries) != tc.count || got.Entries == nil {
				t.Fatalf("unexpected pagination: %+v", got)
			}
			if tc.count > 0 && (got.Entries[0].CacheKey != tc.first || got.Entries[0].Preview != "needle body" || !got.Entries[0].Expired) {
				t.Fatalf("unexpected entry: %+v", got.Entries[0])
			}
		})
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/admin/cache?key=other-key", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", w.Code, w.Body.String())
	}
	entries, total, err := database.ListCacheEntries(context.Background(), readDB, 50, 0)
	if err != nil || total != 2 || len(entries) != 2 {
		t.Fatalf("list after delete = %v, %d, %v", entries, total, err)
	}
}
