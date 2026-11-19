package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"deepl-proxy/config"
)

// newTestNotifier 构造一个连到 test server 的通知器，并保证测试结束时清理。
func newTestNotifier(t *testing.T, mutate func(*config.WebhookConfig)) (*Notifier, <-chan *http.Request, <-chan []byte) {
	t.Helper()
	reqs := make(chan *http.Request, 8)
	bodies := make(chan []byte, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqs <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config.WebhookConfig{
		Enabled:            true,
		URL:                srv.URL,
		Format:             "generic",
		TitlePrefix:        "DeepL Proxy",
		TimeoutSeconds:     2,
		MinIntervalSeconds: 0,
		NotifyKeyDisabled:  true,
		NotifyNoKeys:       true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n := NewNotifier(ctx, cfg)
	t.Cleanup(n.Close)
	return n, reqs, bodies
}

func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
		var zero T
		return zero
	}
}

func TestNotifierGenericPayload(t *testing.T) {
	n, reqs, bodies := newTestNotifier(t, nil)
	n.Notify(Notification{
		Event:     EventKeyDisabled,
		Title:     "Key 失效",
		Message:   "key prod-main 返回 401",
		Level:     "error",
		Data:      map[string]any{"key_id": 7, "status": 401},
		DedupeKey: "key:7",
	})

	req := waitFor(t, reqs)
	body := waitFor(t, bodies)

	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if ev := req.Header.Get("X-Webhook-Event"); ev != EventKeyDisabled {
		t.Fatalf("X-Webhook-Event = %q", ev)
	}

	var payload struct {
		Service string         `json:"service"`
		Event   string         `json:"event"`
		Level   string         `json:"level"`
		Title   string         `json:"title"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if payload.Service != "deepl-proxy" || payload.Event != EventKeyDisabled || payload.Level != "error" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.Title != "DeepL Proxy · Key 失效" {
		t.Fatalf("title = %q", payload.Title)
	}
	if payload.Message != "key prod-main 返回 401" {
		t.Fatalf("message = %q", payload.Message)
	}
	if payload.Data["key_id"] != float64(7) {
		t.Fatalf("data = %+v", payload.Data)
	}
}

func TestNotifierMeowFormat(t *testing.T) {
	n, reqs, bodies := newTestNotifier(t, func(c *config.WebhookConfig) {
		c.Format = "meow"
		c.Nickname = "meow-test-user"
	})
	n.Notify(Notification{Event: EventNoKeys, Title: "无可用 Key", Message: "所有 Key 均不可用", Level: "critical"})

	req := waitFor(t, reqs)
	body := waitFor(t, bodies)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s", req.Method)
	}
	// r.URL.Path 是解码后的路径；PathEscape 已在构造时处理过转义。
	if want := "/meow-test-user/DeepL Proxy · 无可用 Key"; req.URL.Path != want {
		t.Fatalf("path = %q", req.URL.Path)
	}
	var payload struct {
		Title    string `json:"title"`
		Msg      string `json:"msg"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Msg != "所有 Key 均不可用" || payload.Title != "DeepL Proxy · 无可用 Key" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Category != "DeepL代理" {
		t.Fatalf("category = %q", payload.Category)
	}
}

func TestNotifierGetTemplate(t *testing.T) {
	// 用 channel 传递，避免测试 goroutine 与 handler 之间的数据竞争。
	type captured struct{ path, query string }
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- captured{path: r.URL.Path, query: r.URL.RawQuery}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.WebhookConfig{
		Enabled:            true,
		URL:                srv.URL + "/push/{event}/{title}?msg={msg}",
		Format:             "get",
		TimeoutSeconds:     2,
		MinIntervalSeconds: 0,
		NotifyNoKeys:       true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNotifier(ctx, cfg)
	defer n.Close()
	n.Notify(Notification{Event: EventNoKeys, Title: "无可用 Key", Message: "没有可用 key", Level: "warning"})

	var c captured
	select {
	case c = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no GET request received")
	}
	// 这个用例没有配置 TitlePrefix，所以标题不加前缀。
	if want := "/push/no_keys/无可用 Key"; c.path != want {
		t.Fatalf("path = %q, want %q", c.path, want)
	}
	// 空格统一编码成 %20，比较前先解码。
	decoded, err := url.QueryUnescape(c.query)
	if err != nil {
		t.Fatalf("unescape query: %v", err)
	}
	if !strings.Contains(decoded, "msg=没有可用 key") {
		t.Fatalf("query = %q (decoded %q)", c.query, decoded)
	}
}

func TestNotifierDeduplicatesWithinWindow(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.WebhookConfig{
		Enabled:            true,
		URL:                srv.URL,
		Format:             "generic",
		TimeoutSeconds:     2,
		MinIntervalSeconds: 3600,
		NotifyNoKeys:       true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNotifier(ctx, cfg)
	defer n.Close()

	n.Notify(Notification{Event: EventNoKeys, DedupeKey: "global", Message: "第一次"})
	n.Notify(Notification{Event: EventNoKeys, DedupeKey: "global", Message: "第二次"})
	// 不同去重键不应被抑制。
	n.Notify(Notification{Event: EventNoKeys, DedupeKey: "key:1", Message: "第三条"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && count.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := count.Load(); got != 2 {
		t.Fatalf("deliveries = %d, want 2 (global deduped, key:1 delivered)", got)
	}
}

func TestNotifierRetriesServerErrors(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.WebhookConfig{
		Enabled:            true,
		URL:                srv.URL,
		Format:             "generic",
		TimeoutSeconds:     2,
		MinIntervalSeconds: 0,
		NotifyNoKeys:       true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNotifier(ctx, cfg)
	defer n.Close()
	n.Notify(Notification{Event: EventNoKeys, Message: "重试我"})

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && count.Load() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := count.Load(); got < 2 {
		t.Fatalf("attempts = %d, want retry after 500", got)
	}
}

func TestNotifierRespectsEventConfig(t *testing.T) {
	n, reqs, _ := newTestNotifier(t, func(c *config.WebhookConfig) {
		c.NotifyTemporary = false
	})
	n.Notify(Notification{Event: EventKeyTemporary, Message: "临时熔断不应通知"})
	n.Notify(Notification{Event: EventNoKeys, Message: "无可用 key 应该通知"})

	req := waitFor(t, reqs)
	if got := req.Header.Get("X-Webhook-Event"); got != EventNoKeys {
		t.Fatalf("delivered event = %q, want only no_keys", got)
	}
	select {
	case r := <-reqs:
		t.Fatalf("unexpected second delivery: %s", r.Header.Get("X-Webhook-Event"))
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNotifierDisabledIsNoop(t *testing.T) {
	cfg := config.WebhookConfig{Enabled: false, URL: "http://127.0.0.1:1/hook"}
	n := NewNotifier(context.Background(), cfg)
	defer n.Close()
	if n.Enabled() {
		t.Fatal("disabled notifier must report Enabled()=false")
	}
	// 不应 panic、不应阻塞。
	n.Notify(Notification{Event: EventNoKeys, Message: "x"})
}
