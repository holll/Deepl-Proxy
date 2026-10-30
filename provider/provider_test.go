package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadBodyRejectsOversizedResponse(t *testing.T) {
	size := maxUpstreamBodyBytes + 1024
	resp := &http.Response{
		Body:          io.NopCloser(bytes.NewReader(make([]byte, size))),
		ContentLength: int64(size),
	}

	body, err := readBody(resp)
	if err == nil {
		t.Fatalf("oversized body was accepted (%d bytes)", len(body))
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadBodyReadsSmallerResponse(t *testing.T) {
	want := []byte(`{"translations":[{"text":"你好"}]}`)
	resp := &http.Response{
		Body:          io.NopCloser(bytes.NewReader(want)),
		ContentLength: int64(len(want)),
	}

	got, err := readBody(resp)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// 恰好等于上限应当被接受（上限判断不能差一位）。
func TestReadBodyAcceptsExactlyMaxSize(t *testing.T) {
	want := bytes.Repeat([]byte("a"), maxUpstreamBodyBytes)
	resp := &http.Response{
		Body:          io.NopCloser(bytes.NewReader(want)),
		ContentLength: int64(len(want)),
	}

	got, err := readBody(resp)
	if err != nil {
		t.Fatalf("body of exactly max size was rejected: %v", err)
	}
	if len(got) != maxUpstreamBodyBytes {
		t.Fatalf("len = %d, want %d", len(got), maxUpstreamBodyBytes)
	}
}

// deeplx 只返回一条 translation，多文本必须在本地就报错，
// 绝不能把多条拼成一段发上去然后缓存一条结果。
func TestDeepLXRejectsMultiText(t *testing.T) {
	dx := &DeepLX{}
	_, err := dx.Translate(context.Background(), "http://127.0.0.1:1", "key", &TranslateRequest{
		Text:       []string{"a", "b"},
		TargetLang: "ZH",
	})
	if err == nil {
		t.Fatal("multi-text request was accepted by deeplx")
	}
	if err != errDeepLXMultiText {
		t.Fatalf("error = %v, want errDeepLXMultiText", err)
	}
}

// 上游错误响应里的 header 不能按引用带进缓存（否则 Set-Cookie 之类会被存库并回放）。
func TestDeepLXErrorDoesNotLeakUpstreamHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=secret")
		w.Header().Set("X-Upstream-Key-Name", "leaked-name")
		w.Header().Set("X-Custom-Noise", "noise")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
	}))
	defer srv.Close()

	dx := &DeepLX{}
	// 上游 5xx 会同时返回结果和错误：结果用于把上游响应透传给调用方。
	res, err := dx.Translate(context.Background(), srv.URL, "key", &TranslateRequest{
		Text:       []string{"a"},
		TargetLang: "ZH",
	})
	if err == nil {
		t.Fatal("upstream 500 should surface an error")
	}
	if res == nil {
		t.Fatalf("error path returned a nil result: %v", err)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if res.Headers.Get("Set-Cookie") != "" {
		t.Error("Set-Cookie leaked into the result headers")
	}
	if res.Headers.Get("X-Custom-Noise") != "" {
		t.Error("arbitrary upstream header leaked into the result headers")
	}
}

func TestDeepLXSuccessReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"data":"你好","source_lang":"EN"}`))
	}))
	defer srv.Close()

	dx := &DeepLX{}
	res, err := dx.Translate(context.Background(), srv.URL, "key", &TranslateRequest{
		Text:       []string{"hello"},
		TargetLang: "ZH",
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !bytes.Contains(res.Body, []byte("你好")) {
		t.Fatalf("body = %q", res.Body)
	}
}

// 客户端断开必须让上游请求立刻取消，而不是一直等到 15s 超时。
func TestDeepLCancelsUpstreamOnContextDone(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			<-released
		}
	}))
	defer srv.Close()
	defer close(released)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	dl := &DeepL{}
	start := time.Now()
	_, err := dl.Translate(ctx, srv.URL, "key", &TranslateRequest{
		Text:       []string{"hello"},
		TargetLang: "ZH",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("cancelled request returned no error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v; context was not propagated upstream", elapsed)
	}
}

func TestDetectProvider(t *testing.T) {
	if name := DetectProvider("deeplx").Name(); name != "deeplx" {
		t.Errorf("DetectProvider(deeplx).Name() = %q", name)
	}
	for _, p := range []string{"deepl", "", "unknown", "DeepL"} {
		if name := DetectProvider(p).Name(); name != "deepl" {
			t.Errorf("DetectProvider(%q).Name() = %q, want deepl", p, name)
		}
	}
}

func TestDetectSiteType(t *testing.T) {
	cases := map[string]string{
		"https://api.deepl.com":      "official",
		"https://api-free.deepl.com": "official",
		"https://example.com":        "deepl_pro",
		"":                           "deepl_pro",
	}
	for endpoint, want := range cases {
		if got := DetectSiteType(endpoint); got != want {
			t.Errorf("DetectSiteType(%q) = %q, want %q", endpoint, got, want)
		}
	}
}
