package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"deepl-proxy/database"
	"deepl-proxy/provider"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

// newAdminKeysRouter 搭一个只挂 key 管理接口的路由，省去认证层。
func newAdminKeysRouter(t *testing.T) (*gin.Engine, *service.Keyring) {
	t.Helper()
	writeDB, readDB, err := database.Init(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writeDB.Close()
		_ = readDB.Close()
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	kr := service.NewKeyring(t.Context(), writeDB, readDB)
	t.Cleanup(kr.Close)
	r.GET("/keys", handleAdminKeys(kr))
	r.POST("/keys", handleAdminKeyCreate(kr))
	r.PUT("/keys/:id", handleAdminKeyUpdate(kr))
	r.POST("/keys/:id/test", handleAdminKeyTest(kr))
	return r, kr
}

func doJSON(t *testing.T, r http.Handler, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func listKeys(t *testing.T, r http.Handler) []map[string]any {
	t.Helper()
	w, out := doJSON(t, r, http.MethodGet, "/keys", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /keys = %d: %s", w.Code, w.Body.String())
	}
	raw, _ := out["keys"].([]any)
	keys := make([]map[string]any, len(raw))
	for i, v := range raw {
		keys[i], _ = v.(map[string]any)
	}
	return keys
}

func TestAdminKeyCreateDetectsEndpointFromSuffix(t *testing.T) {
	r, _ := newAdminKeysRouter(t)

	w, out := doJSON(t, r, http.MethodPost, "/keys", map[string]any{
		"name": "third-party", "auth_key": "secret-key:dp", "endpoint": "auto",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if got := out["endpoint"]; got != provider.EndpointDeepLPro {
		t.Fatalf("response endpoint = %v, want %s", got, provider.EndpointDeepLPro)
	}

	keys := listKeys(t, r)
	if len(keys) != 1 {
		t.Fatalf("keys = %d", len(keys))
	}
	k := keys[0]
	if k["endpoint"] != provider.EndpointDeepLPro {
		t.Fatalf("stored endpoint = %v", k["endpoint"])
	}
	if k["site_type"] != "deepl_pro" {
		t.Fatalf("site_type = %v", k["site_type"])
	}
	if k["auth_key_hint"] != "…:dp" {
		t.Fatalf("auth_key_hint = %v", k["auth_key_hint"])
	}
	if k["quota_period"] != service.QuotaPeriodAuto {
		t.Fatalf("quota_period = %v", k["quota_period"])
	}
	// auth_key 本身绝不能出现在响应里。
	if _, leaked := k["auth_key"]; leaked {
		t.Fatal("response leaks auth_key")
	}
}

func TestAdminKeyCreateFreeSuffixAndExplicitQuota(t *testing.T) {
	r, _ := newAdminKeysRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/keys", map[string]any{
		"name": "legacy-free", "auth_key": "abc:fx", "endpoint": "", "quota_period": "monthly",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	k := listKeys(t, r)[0]
	if k["endpoint"] != provider.EndpointDeepLFree || k["quota_period"] != service.QuotaPeriodMonthly {
		t.Fatalf("key = %+v", k)
	}
	if k["site_type"] != "deepl_free" {
		t.Fatalf("site_type = %v", k["site_type"])
	}
}

func TestAdminKeyCreateRejectsUnresolvableEndpoint(t *testing.T) {
	r, _ := newAdminKeysRouter(t)
	w, out := doJSON(t, r, http.MethodPost, "/keys", map[string]any{
		"name": "no-endpoint",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400", w.Code)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Fatalf("expected error message, got %v", out)
	}
}

func TestAdminKeyCreateRejectsBadQuotaPeriod(t *testing.T) {
	r, _ := newAdminKeysRouter(t)
	w, _ := doJSON(t, r, http.MethodPost, "/keys", map[string]any{
		"name": "x", "auth_key": "k:fx", "quota_period": "weekly",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400", w.Code)
	}
}

func TestAdminKeyUpdateReDetectsEndpointOnKeyChange(t *testing.T) {
	r, _ := newAdminKeysRouter(t)
	if w, _ := doJSON(t, r, http.MethodPost, "/keys", map[string]any{
		"name": "moving", "auth_key": "old:fx", "endpoint": "auto",
	}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	id := listKeys(t, r)[0]["id"]

	// 只换 auth_key、不给 endpoint：应跟着 :dp 后缀切到第三方站点。
	if w, _ := doJSON(t, r, http.MethodPut, fmt.Sprintf("/keys/%v", id), map[string]any{
		"auth_key": "new:dp",
	}); w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	k := listKeys(t, r)[0]
	if k["endpoint"] != provider.EndpointDeepLPro {
		t.Fatalf("endpoint = %v, want %s", k["endpoint"], provider.EndpointDeepLPro)
	}
}

// fakeTranslateUpstream 起一个最小的上游，只用于验证"测试 key"接口。
func fakeTranslateUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/translate" {
			http.NotFound(w, r)
			return
		}
		// DeepL 的认证头是 Authorization: DeepL-Auth-Key <key>。
		if r.Header.Get("Authorization") != "DeepL-Auth-Key good-key" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Wrong endpoint"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"EN","text":"ping"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminKeyTestEndpoint(t *testing.T) {
	r, _ := newAdminKeysRouter(t)
	upstream := fakeTranslateUpstream(t)

	for _, tc := range []struct {
		name     string
		authKey  string
		wantOK   bool
		wantCode int
		wantErr  string
	}{
		{"working key", "good-key", true, http.StatusOK, ""},
		{"wrong key", "bad-key", false, http.StatusForbidden, "permanent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, _ := doJSON(t, r, http.MethodPost, "/keys", map[string]any{
				"name": tc.name, "auth_key": tc.authKey, "endpoint": upstream.URL,
			}); w.Code != http.StatusCreated {
				t.Fatalf("create = %d: %s", w.Code, w.Body.String())
			}
			keys := listKeys(t, r)
			id := keys[len(keys)-1]["id"]

			w, out := doJSON(t, r, http.MethodPost, fmt.Sprintf("/keys/%v/test", id), nil)
			if w.Code != http.StatusOK {
				t.Fatalf("test = %d: %s", w.Code, w.Body.String())
			}
			if got := out["ok"]; got != tc.wantOK {
				t.Fatalf("ok = %v, want %v (body %s)", got, tc.wantOK, w.Body.String())
			}
			if got := int(out["status"].(float64)); got != tc.wantCode {
				t.Fatalf("status = %d, want %d", got, tc.wantCode)
			}
			if tc.wantErr != "" {
				if got := out["error_type"]; got != tc.wantErr {
					t.Fatalf("error_type = %v, want %s", got, tc.wantErr)
				}
			}
			// 手动测试不应改变 key 状态。
			if got := listKeys(t, r)[len(keys)-1]["status"]; got != "active" {
				t.Fatalf("status after test = %v, want active", got)
			}
		})
	}
}
