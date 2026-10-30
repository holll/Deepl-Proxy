package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"deepl-proxy/config"

	"github.com/gin-gonic/gin"
)

func TestSanitizeRequestID(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"empty":            {"", ""},
		"normal":           {"abc-123", "abc-123"},
		"single char":      {"x", "x"},
		"shorter than 8":   {"abc", "abc"},
		"exactly 64":       {strings.Repeat("a", 64), strings.Repeat("a", 64)},
		"longer than 64":   {strings.Repeat("a", 100), strings.Repeat("a", 64)},
		"strips controls":  {"ab\x00\x01\x1fcd", "abcd"},
		"strips newline":   {"ab\ncd", "abcd"},
		"strips 0x7f":      {"ab\x7fcd", "abcd"},
		"strips non-ascii": {"中文id", "id"},
	}
	for name, tc := range cases {
		if got := sanitizeRequestID(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeRequestID(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// 旧实现是 reqID[:8]：任何不足 8 字节的 X-Request-ID 都会 panic。
func TestShortIDNeverPanics(t *testing.T) {
	for _, in := range []string{"", "a", "ab", "abcdefg", "abcdefgh", "abcdefghi", strings.Repeat("z", 500)} {
		got := shortID(in)
		if len(got) > 8 {
			t.Errorf("shortID(%q) = %q (len %d), want <= 8", in, got, len(got))
		}
		if in != "" && !strings.HasPrefix(in, got) {
			t.Errorf("shortID(%q) = %q, want a prefix of the input", in, got)
		}
	}
}

func newTestRouter(mw gin.HandlerFunc, cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw)
	r.POST("/v2/translate", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func TestRequestLoggerEchoesSanitizedID(t *testing.T) {
	r := newTestRouter(RequestLogger(), &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/v2/translate", nil)
	req.Header.Set("X-Request-ID", "x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a 1-byte request id must not panic)", w.Code)
	}
	if got := w.Header().Get("X-Request-ID"); got != "x" {
		t.Fatalf("echoed X-Request-ID = %q, want %q", got, "x")
	}
}

func TestRequestLoggerGeneratesIDWhenAbsent(t *testing.T) {
	r := newTestRouter(RequestLogger(), &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/v2/translate", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("X-Request-ID was not generated")
	}
}

func TestRequestLoggerTruncatesLongID(t *testing.T) {
	r := newTestRouter(RequestLogger(), &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/v2/translate", nil)
	req.Header.Set("X-Request-ID", strings.Repeat("A", 500))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-ID"); len(got) != maxRequestIDLen {
		t.Fatalf("echoed id length = %d, want %d", len(got), maxRequestIDLen)
	}
}

func TestGatewayAuthMiddleware(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{GatewayToken: "gw"}}

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"bearer", "Bearer gw", http.StatusOK},
		{"deepl compat header", "DeepL-Auth-Key gw", http.StatusOK},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"missing", "", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		r := newTestRouter(GatewayAuthMiddleware(cfg), cfg)
		req := httptest.NewRequest(http.MethodPost, "/v2/translate", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

func TestGatewayAuthRejectsUnconfiguredToken(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{GatewayToken: ""}}
	r := newTestRouter(GatewayAuthMiddleware(cfg), cfg)

	req := httptest.NewRequest(http.MethodPost, "/v2/translate", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when GATEWAY_TOKEN is unset", w.Code)
	}
}
