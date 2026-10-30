package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/handler"
	"deepl-proxy/models"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

const (
	testGatewayToken = "gw-token"
	testAdminToken   = "admin-token"
)

// fakeUpstream 模拟 DeepL 官方接口与 DeepLX 接口。
//
// /v2/translate 按请求里的 text 数量返回同样多条 translations，
// 这样"deeplx 只返回一条"的错配就能被测试发现。
func fakeUpstream(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	hits := new(atomic.Int64)
	delayNs := new(atomic.Int64)
	mux := http.NewServeMux()

	mux.HandleFunc("/v2/translate", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 可控延迟：用来把并发请求压在 leader 的调用里，验证合并行为。
		if d := time.Duration(delayNs.Load()); d > 0 {
			time.Sleep(d)
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		texts := r.PostForm["text"]
		formality := r.PostForm.Get("formality") != ""
		translations := make([]map[string]any, 0, len(texts))
		for i := range texts {
			translations = append(translations, map[string]any{
				"detected_source_language": "EN",
				"text":                     fmt.Sprintf("deepl[%d/%d/formality=%t]", i, len(texts), formality),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"translations": translations})
	})

	mux.HandleFunc("/translate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":        200,
			"data":        "deeplx:" + req.Text,
			"source_lang": "EN",
			"method":      "fake",
		})
	})

	mux.HandleFunc("/v2/usage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"character_count":10,"character_limit":100}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hits, delayNs
}

type harness struct {
	t        *testing.T
	srv      *httptest.Server
	upstream *httptest.Server
	writeDB  *sql.DB
	// upstreamHits 记录假上游 /v2/translate 被真实调用的次数。
	upstreamHits *atomic.Int64
	// upstreamDelay 控制假上游的响应耗时（纳秒）。
	upstreamDelay *atomic.Int64
	// usageMu 让"用量查询"这类会走后台采样的路径不干扰断言。
	mu sync.Mutex
}

func newHarness(t *testing.T, ttlDays int) *harness {
	t.Helper()
	return newHarnessWith(t, ttlDays, nil)
}

// newHarnessWith 允许测试在构造前改写配置（例如关掉 v1 兼容读取）。
func newHarnessWith(t *testing.T, ttlDays int, mutate func(*config.Config)) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	upstream, upstreamHits, upstreamDelay := fakeUpstream(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	writeDB, readDB := database.Init(dbPath)
	t.Cleanup(func() {
		_ = writeDB.Close()
		_ = readDB.Close()
	})

	cfg := &config.Config{
		Server:   config.ServerConfig{Port: "0", AllowOrigin: "*"},
		Auth:     config.AuthConfig{AdminToken: testAdminToken, GatewayToken: testGatewayToken, AdminCookieSecret: "0123456789abcdef0123456789abcdef", SessionTTL: 3600},
		Database: config.DatabaseConfig{Path: dbPath},
		Cache:    config.CacheConfig{TTLDays: ttlDays, CleanupInterval: 3600, CleanupBatchSize: 500, CleanupMaxRounds: 20},
	}
	if mutate != nil {
		mutate(cfg)
	}

	kr := service.NewKeyring(writeDB, readDB)
	cs := service.NewCacheService(writeDB, readDB, cfg)

	r := gin.New()
	if err := r.SetTrustedProxies([]string{"127.0.0.1"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.Use(gin.Recovery())
	handler.SetupRoutes(r, kr, cs, cfg)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &harness{
		t:             t,
		srv:           srv,
		upstream:      upstream,
		writeDB:       writeDB,
		upstreamHits:  upstreamHits,
		upstreamDelay: upstreamDelay,
	}
}

func (h *harness) do(method, path, contentType string, body []byte, hdr map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, out
}

func (h *harness) translateJSON(payload any, extra map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("marshal payload: %v", err)
	}
	hdr := map[string]string{"Authorization": "Bearer " + testGatewayToken}
	for k, v := range extra {
		hdr[k] = v
	}
	return h.do(http.MethodPost, "/v2/translate", "application/json", body, hdr)
}

func (h *harness) translateForm(form string) (int, http.Header, []byte) {
	h.t.Helper()
	return h.do(http.MethodPost, "/v2/translate", "application/x-www-form-urlencoded", []byte(form),
		map[string]string{"Authorization": "Bearer " + testGatewayToken})
}

func (h *harness) admin(method, path string, payload any) (int, []byte) {
	h.t.Helper()
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			h.t.Fatalf("marshal admin payload: %v", err)
		}
	}
	status, _, out := h.do(method, path, "application/json", body,
		map[string]string{"Authorization": "Bearer " + testAdminToken})
	return status, out
}

func (h *harness) addKey(name, prov string) int64 {
	h.t.Helper()
	status, body := h.admin(http.MethodPost, "/admin/keys", map[string]any{
		"name":     name,
		"auth_key": "key-" + name,
		"endpoint": h.upstream.URL,
		"provider": prov,
	})
	if status != http.StatusCreated {
		h.t.Fatalf("create key: status %d body %s", status, body)
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		h.t.Fatalf("decode create key: %v (%s)", err, body)
	}
	return resp.ID
}

type adminKey struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (h *harness) listKeys() []adminKey {
	h.t.Helper()
	status, body := h.admin(http.MethodGet, "/admin/keys", nil)
	if status != http.StatusOK {
		h.t.Fatalf("list keys: status %d body %s", status, body)
	}
	var resp struct {
		Keys []adminKey `json:"keys"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		h.t.Fatalf("decode list keys: %v (%s)", err, body)
	}
	return resp.Keys
}

func (h *harness) deleteAllKeys() {
	h.t.Helper()
	for _, k := range h.listKeys() {
		if status, body := h.admin(http.MethodDelete, fmt.Sprintf("/admin/keys/%d", k.ID), nil); status != http.StatusOK {
			h.t.Fatalf("delete key %d: status %d body %s", k.ID, status, body)
		}
	}
}

func translationTexts(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode translations: %v (%s)", err, body)
	}
	out := make([]string, 0, len(resp.Translations))
	for _, tr := range resp.Translations {
		out = append(out, tr.Text)
	}
	return out
}

func assertStatus(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, body)
	}
}

func assertCacheStatus(t *testing.T, hdr http.Header, want string) {
	t.Helper()
	if got := hdr.Get("X-Cache-Status"); got != want {
		t.Fatalf("X-Cache-Status = %q, want %q", got, want)
	}
}

// --- 缓存键与 provider 路由 ---

func TestDeepLXSingleTextRoundTrip(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dx", "deeplx")

	status, hdr, body := h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH"}, nil)
	assertStatus(t, status, http.StatusOK, body)
	if texts := translationTexts(t, body); len(texts) != 1 || texts[0] != "deeplx:hello" {
		t.Fatalf("deeplx result = %q, want [deeplx:hello]", texts)
	}
	assertCacheStatus(t, hdr, "MISS")

	status, hdr, body = h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH"}, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "HIT")
	if texts := translationTexts(t, body); len(texts) != 1 || texts[0] != "deeplx:hello" {
		t.Fatalf("cached deeplx result = %q, want [deeplx:hello]", texts)
	}
}

func TestDeepLXRejectsMultiTextWith503(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dx", "deeplx")

	status, _, body := h.translateJSON(map[string]any{"text": []string{"a", "b"}, "target_lang": "ZH"}, nil)
	assertStatus(t, status, http.StatusServiceUnavailable, body)
	if !strings.Contains(string(body), "deeplx") {
		t.Fatalf("503 body should explain the deeplx limitation: %s", body)
	}
}

// 这是缓存键缺 provider 维度时的真实故障：
// deeplx 缓存的"单条 translation"会被官方 deepl 的请求原样重放。
func TestProviderSwitchDoesNotReplayCache(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dx", "deeplx")

	status, _, body := h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH"}, nil)
	assertStatus(t, status, http.StatusOK, body)
	if texts := translationTexts(t, body); len(texts) != 1 || texts[0] != "deeplx:hello" {
		t.Fatalf("seed result = %q, want [deeplx:hello]", texts)
	}

	h.deleteAllKeys()
	h.addKey("dl", "deepl")

	status, hdr, body := h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH"}, nil)
	assertStatus(t, status, http.StatusOK, body)
	texts := translationTexts(t, body)
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "deepl[") {
		t.Fatalf("provider switch replayed the deeplx body: %q", texts)
	}
	assertCacheStatus(t, hdr, "MISS")
}

func TestMultiTextWithOfficialProvider(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	payload := map[string]any{"text": []string{"a", "b", "c"}, "target_lang": "ZH"}
	status, hdr, body := h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	if texts := translationTexts(t, body); len(texts) != 3 {
		t.Fatalf("got %d translations, want 3: %q", len(texts), texts)
	}
	assertCacheStatus(t, hdr, "MISS")

	status, hdr, body = h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "HIT")
	if texts := translationTexts(t, body); len(texts) != 3 {
		t.Fatalf("cached hit lost translations: %q", texts)
	}
}

// --- 参数是否既转发又进键 ---

func TestFormalityIsForwardedAndKeyed(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	status, hdr, body := h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH", "formality": "more"}, nil)
	assertStatus(t, status, http.StatusOK, body)
	if texts := translationTexts(t, body); len(texts) != 1 || !strings.Contains(texts[0], "formality=true") {
		t.Fatalf("formality was not forwarded upstream: %q", texts)
	}
	assertCacheStatus(t, hdr, "MISS")

	// 同一请求命中
	_, hdr, _ = h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH", "formality": "more"}, nil)
	assertCacheStatus(t, hdr, "HIT")

	// 不带 formality 的请求必须是另一个键
	_, hdr, body = h.translateJSON(map[string]any{"text": "hello", "target_lang": "ZH"}, nil)
	assertCacheStatus(t, hdr, "MISS")
	if texts := translationTexts(t, body); !strings.Contains(texts[0], "formality=false") {
		t.Fatalf("formality leaked between cache entries: %q", texts)
	}
}

// 这三个参数此前被白名单静默丢弃：既不转发、也不进键。
func TestPreviouslyDroppedParamsAreForwardedAndKeyed(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	payload := map[string]any{
		"text":                "hello",
		"target_lang":         "ZH",
		"custom_instructions": "use a formal tone",
	}
	status, hdr, body := h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "MISS")

	// 重复请求命中
	_, hdr, _ = h.translateJSON(payload, nil)
	assertCacheStatus(t, hdr, "HIT")

	// 参数不同的请求不能共用这条缓存
	_, hdr, _ = h.translateJSON(map[string]any{
		"text":                "hello",
		"target_lang":         "ZH",
		"custom_instructions": "use a casual tone",
	}, nil)
	assertCacheStatus(t, hdr, "MISS")
}

// --- 请求形态 ---

func TestNoCacheRequestBypassesCache(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	payload := map[string]any{"text": "hello", "target_lang": "ZH"}
	for i := 0; i < 3; i++ {
		status, hdr, body := h.translateJSON(payload, map[string]string{"x-no-cache": "1"})
		assertStatus(t, status, http.StatusOK, body)
		assertCacheStatus(t, hdr, "MISS")
	}
}

func TestFormEncodedRequest(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	status, _, body := h.translateForm("text=form1&target_lang=zh&source_lang=en")
	assertStatus(t, status, http.StatusOK, body)
	if texts := translationTexts(t, body); len(texts) != 1 || !strings.HasPrefix(texts[0], "deepl[") {
		t.Fatalf("form request not translated: %q", texts)
	}
}

// 旧实现在这里 reqID[:8] 直接 panic（X-Request-ID 由客户端控制）。
func TestRequestIDHeaderRobustness(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	payload := map[string]any{"text": "hello", "target_lang": "ZH"}
	for name, id := range map[string]string{
		"one char": "x",
		"empty":    "",
		"too long": strings.Repeat("A", 500),
		"3 bytes":  "abc",
	} {
		status, hdr, body := h.translateJSON(payload, map[string]string{"X-Request-ID": id})
		if status != http.StatusOK {
			t.Errorf("%s: status = %d (body %s)", name, status, body)
			continue
		}
		if got := hdr.Get("X-Request-ID"); len(got) > 64 {
			t.Errorf("%s: echoed X-Request-ID is %d bytes, want <= 64", name, len(got))
		}
	}
}

// --- 配置语义 ---

// ttl_days=0 表示不缓存：读路径也必须关闭。
// 只测"新写入不生效"还不够——真正的坑是库里已经有的旧行会不会被继续返回并续期，
// 所以这里直接手写一条未过期的缓存行来验证读路径。
func TestZeroTTLDisablesCacheEntirely(t *testing.T) {
	// seed 用与 handler 完全一致的字段拼出缓存键，先验证拼得对，再验证被忽略。
	seed := func(h *harness) {
		t.Helper()
		key := service.BuildCacheKey(&models.CacheIdentity{
			Text:       []string{"hello"},
			TargetLang: "ZH",
		}, "deepl")
		now := time.Now().UnixMilli()
		if err := database.UpsertCache(h.writeDB, &models.TranslateCache{
			CacheKey:    key,
			Body:        []byte(`{"translations":[{"text":"seeded"}]}`),
			HeadersJSON: "{}",
			CreatedAt:   now,
			ExpiresAt:   now + 3_600_000,
		}); err != nil {
			t.Fatalf("seed cache row: %v", err)
		}
	}

	payload := map[string]any{"text": "hello", "target_lang": "ZH"}

	t.Run("ttl>0 serves the seeded row", func(t *testing.T) {
		h := newHarness(t, 1)
		h.addKey("dl", "deepl")
		seed(h)

		status, hdr, body := h.translateJSON(payload, nil)
		assertStatus(t, status, http.StatusOK, body)
		assertCacheStatus(t, hdr, "HIT")
		if texts := translationTexts(t, body); len(texts) != 1 || texts[0] != "seeded" {
			t.Fatalf("control subtests did not hit the seeded row: %q", texts)
		}
	})

	t.Run("ttl=0 ignores the seeded row", func(t *testing.T) {
		h := newHarness(t, 0)
		h.addKey("dl", "deepl")
		seed(h)

		status, hdr, body := h.translateJSON(payload, nil)
		assertStatus(t, status, http.StatusOK, body)
		assertCacheStatus(t, hdr, "MISS")
		if texts := translationTexts(t, body); len(texts) == 1 && texts[0] == "seeded" {
			t.Fatal("ttl_days=0 still served a cached row")
		}
	})
}

// --- 旁路接口 ---

func TestGatewayAuthRejectsMissingToken(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	body, _ := json.Marshal(map[string]any{"text": "hello", "target_lang": "ZH"})
	status, _, resp := h.do(http.MethodPost, "/v2/translate", "application/json", body, nil)
	assertStatus(t, status, http.StatusUnauthorized, resp)
}

func TestUsageCompat(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	status, _, body := h.do(http.MethodGet, "/v2/usage", "", nil,
		map[string]string{"Authorization": "Bearer " + testGatewayToken})
	assertStatus(t, status, http.StatusOK, body)

	var resp struct {
		CharacterLimit int64 `json:"character_limit"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode usage: %v (%s)", err, body)
	}
	if resp.CharacterLimit != 100 {
		t.Fatalf("character_limit = %d, want 100", resp.CharacterLimit)
	}
}

func TestAdminCacheListPreview(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	status, _, body := h.translateJSON(map[string]any{"text": "hello world", "target_lang": "ZH"}, nil)
	assertStatus(t, status, http.StatusOK, body)

	status, body = h.admin(http.MethodGet, "/admin/cache?limit=50", nil)
	assertStatus(t, status, http.StatusOK, body)

	var resp struct {
		Total   int `json:"total"`
		Entries []struct {
			Preview string `json:"preview"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode cache list: %v (%s)", err, body)
	}
	if resp.Total == 0 || len(resp.Entries) == 0 {
		t.Fatalf("cache list is empty: %s", body)
	}
	if resp.Entries[0].Preview == "" {
		t.Fatal("preview is empty")
	}
}

// --- v1 旧缓存键兼容 ---

// seedLegacyRow 用旧版算法算出缓存键并写入一行，
// 模拟生产环境里升级前就已存在的那一万条缓存。
func seedLegacyRow(t *testing.T, h *harness, identity *models.CacheIdentity, body string) string {
	t.Helper()
	key := service.BuildLegacyCacheKey(identity)
	if key == "" {
		t.Fatal("legacy key is empty")
	}
	now := time.Now().UnixMilli()
	if err := database.UpsertCache(h.writeDB, &models.TranslateCache{
		CacheKey:    key,
		Body:        []byte(body),
		HeadersJSON: `{"Content-Type":["application/json"]}`,
		CreatedAt:   now,
		ExpiresAt:   now + 3_600_000,
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	return key
}

// countCacheRows 统计某键在表里的行数，用来验证迁移是否落库。
func countCacheRows(t *testing.T, h *harness, key string) int {
	t.Helper()
	var n int
	if err := h.writeDB.QueryRow(
		`SELECT COUNT(*) FROM deepl_translation_cache WHERE cache_key=?`, key,
	).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// 升级后必须能直接命中旧行 —— 否则上线瞬间缓存归零，全量请求打上游。
func TestLegacyCacheRowIsServedAndMigrated(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	identity := &models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"}
	legacyKey := seedLegacyRow(t, h, identity, `{"translations":[{"detected_source_language":"EN","text":"legacy-hit"}]}`)

	payload := map[string]any{"text": "hello", "target_lang": "zh"}
	status, hdr, body := h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "HIT")

	texts := translationTexts(t, body)
	if len(texts) != 1 || texts[0] != "legacy-hit" {
		t.Fatalf("upstream was called instead of serving the legacy row: %q", texts)
	}

	// 命中后应当把这一行就地迁移到新键，后续请求走新键。
	newKey := service.BuildCacheKey(identity, "deepl")
	if n := countCacheRows(t, h, newKey); n != 1 {
		t.Fatalf("legacy row was not migrated to the v2 key, rows=%d", n)
	}
	// 旧行保留（回滚时旧二进制仍要能命中），只是不再续期。
	if n := countCacheRows(t, h, legacyKey); n != 1 {
		t.Fatalf("legacy row should be kept for rollback, rows=%d", n)
	}

	// 第二次请求来自 v2 键，内容仍是同一份译文。
	status, hdr, body = h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "HIT")
	if texts := translationTexts(t, body); len(texts) != 1 || texts[0] != "legacy-hit" {
		t.Fatalf("second request lost the migrated entry: %q", texts)
	}
}

// 旧键没有 provider 维度：deeplx 产出的行恒定只有 1 条译文。
// 把这样的行回放给需要 3 条译文的请求，就是一次结构错误的响应。
func TestLegacyRowIgnoredWhenTranslationCountMismatches(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	identity := &models.CacheIdentity{Text: []string{"a", "b", "c"}, TargetLang: "ZH"}
	// 只塞 1 条译文，模拟"这一行其实是 deeplx 产出的"。
	seedLegacyRow(t, h, identity, `{"translations":[{"text":"only-one"}]}`)

	status, hdr, body := h.translateJSON(map[string]any{
		"text":        []string{"a", "b", "c"},
		"target_lang": "zh",
	}, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "MISS")

	if texts := translationTexts(t, body); len(texts) != 3 {
		t.Fatalf("expected 3 fresh translations, got %d: %q", len(texts), texts)
	}
}

// v1 时代这三个参数是被静默丢弃的，旧行等价于"没带这些参数"。
// 现在它们会被真实转发，因此必须跳过兼容读取，否则会把忽略指令的旧译文
// 当成带指令的结果返回。
func TestLegacyReadSkippedForV2OnlyParams(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	identity := &models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"}
	seedLegacyRow(t, h, identity, `{"translations":[{"text":"legacy-hit"}]}`)

	status, hdr, body := h.translateJSON(map[string]any{
		"text":                "hello",
		"target_lang":         "zh",
		"custom_instructions": "keep the tone casual",
	}, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "MISS")

	if texts := translationTexts(t, body); len(texts) == 1 && texts[0] == "legacy-hit" {
		t.Fatal("a row created while custom_instructions was silently dropped was replayed")
	}
}

// 迁移完成后要能一键关掉兼容读取（省掉每次未命中的额外查询）。
func TestLegacyReadCanBeDisabled(t *testing.T) {
	h := newHarnessWith(t, 1, func(cfg *config.Config) {
		cfg.Cache.LegacyKeyDisabled = true
	})
	h.addKey("dl", "deepl")

	seedLegacyRow(t, h, &models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"},
		`{"translations":[{"text":"legacy-hit"}]}`)

	status, hdr, body := h.translateJSON(map[string]any{"text": "hello", "target_lang": "zh"}, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "MISS")
	if texts := translationTexts(t, body); len(texts) == 1 && texts[0] == "legacy-hit" {
		t.Fatal("legacy_key_disabled=true still served a v1 row")
	}
}

// 兼容读取不能反过来污染新键：新键命中时不应触发任何旧键查询。
func TestV2HitDoesNotTouchLegacyPath(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")

	payload := map[string]any{"text": "hello", "target_lang": "zh"}
	status, hdr, body := h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "MISS")

	status, hdr, _ = h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "HIT")

	// 只有 v2 那一行，没有凭空多出 v1 行。
	newKey := service.BuildCacheKey(&models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"}, "deepl")
	if n := countCacheRows(t, h, newKey); n != 1 {
		t.Fatalf("expected exactly 1 v2 row, got %d", n)
	}
	legacyKey := service.BuildLegacyCacheKey(&models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"})
	if n := countCacheRows(t, h, legacyKey); n != 0 {
		t.Fatalf("v2 hit must not create legacy rows, got %d", n)
	}
}

// --- 缓存击穿保护 ---

// 同一份冷文本被 N 个请求同时打进来时，只应产生一次上游调用。
func TestConcurrentMissesShareOneUpstreamCall(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")
	h.upstreamDelay.Store(int64(150 * time.Millisecond))

	payload := map[string]any{"text": "cold text", "target_lang": "zh"}
	const n = 10

	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	hdrs := make([]http.Header, n)
	statuses := make([]int, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start // 尽量让这些请求同时进入 handler
			statuses[i], hdrs[i], bodies[i] = h.translateJSON(payload, nil)
		})
	}
	close(start)
	wg.Wait()

	if got := h.upstreamHits.Load(); got != 1 {
		t.Fatalf("stampede not collapsed: upstream called %d times, want 1", got)
	}
	// 关键断言：这 N 个请求**都**是缓存未命中，却只产生了一次上游调用。
	// 若没有合并，串行执行会让后面的请求变成 HIT —— 那也算"只调用一次上游"，
	// 所以必须同时断言"N 个 MISS"，否则这个用例测不出任何东西。
	for i := range n {
		assertStatus(t, statuses[i], http.StatusOK, bodies[i])
		assertCacheStatus(t, hdrs[i], "MISS")
		if string(bodies[i]) != string(bodies[0]) {
			t.Fatalf("request %d got a different body: %s vs %s", i, bodies[i], bodies[0])
		}
	}

	// 合并后结果已入库，后续请求直接命中。
	h.upstreamDelay.Store(0)
	status, hdr, body := h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "HIT")
	if got := h.upstreamHits.Load(); got != 1 {
		t.Fatalf("follow-up request hit upstream again, total %d", got)
	}
}

// x-no-cache 的请求本来就有意绕过缓存，不应该合并、也不应该写缓存。
func TestNoCacheRequestsAreNotCoalesced(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")
	h.upstreamDelay.Store(int64(100 * time.Millisecond))

	payload := map[string]any{"text": "bypass", "target_lang": "zh"}
	const n = 3

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			status, _, body := h.translateJSON(payload, map[string]string{"x-no-cache": "1"})
			assertStatus(t, status, http.StatusOK, body)
		})
	}
	close(start)
	wg.Wait()

	if got := h.upstreamHits.Load(); got != n {
		t.Fatalf("x-no-cache requests must not coalesce: upstream called %d times, want %d", got, n)
	}

	// 它们也没有写缓存。
	h.upstreamDelay.Store(0)
	status, hdr, body := h.translateJSON(payload, nil)
	assertStatus(t, status, http.StatusOK, body)
	assertCacheStatus(t, hdr, "MISS")
}

// 合并只发生在"同一份请求"上：不同文本不能互相复用结果。
func TestDifferentTextsAreNotCoalesced(t *testing.T) {
	h := newHarness(t, 1)
	h.addKey("dl", "deepl")
	h.upstreamDelay.Store(int64(100 * time.Millisecond))

	texts := []string{"alpha", "beta", "gamma"}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, txt := range texts {
		wg.Go(func() {
			<-start
			status, _, body := h.translateJSON(map[string]any{"text": txt, "target_lang": "zh"}, nil)
			assertStatus(t, status, http.StatusOK, body)
			if got := translationTexts(t, body); len(got) != 1 || !strings.Contains(got[0], "deepl[0/1") {
				t.Fatalf("unexpected body for %q: %q", txt, got)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := h.upstreamHits.Load(); got != int64(len(texts)) {
		t.Fatalf("distinct texts must each call upstream: got %d, want %d", got, len(texts))
	}
}
