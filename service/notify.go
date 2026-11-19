package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deepl-proxy/config"
)

// 通知事件类型。
//
// 这些字符串会出现在 webhook 请求体的 event 字段里，属于对外契约：
// 下游可能据此分流（比如只把 key_disabled 推给值班群），改名等于破坏兼容。
const (
	EventKeyDisabled    = "key_disabled"
	EventQuotaExhausted = "quota_exhausted"
	EventKeyTemporary   = "key_temporarily_disabled"
	EventNoKeys         = "no_keys"
)

// meowDefaultBase 是 MeoW 推送服务的默认入口。
// 使用 meow 格式时可以只填 nickname，不必重复写这个地址。
const meowDefaultBase = "https://api.chuckfang.com"

// notifyQueueSize 是待投递队列长度。
//
// 队列满时直接丢弃而不是阻塞调用方：通知永远不能拖慢翻译主链路，
// 丢一条告警远好过让一个翻译请求卡在 HTTP 投递上。
const notifyQueueSize = 32

// Notification 一条待投递的通知。
type Notification struct {
	Event     string
	Title     string
	Message   string
	Level     string // info / warning / error / critical
	Data      map[string]any
	DedupeKey string // 同类事件去重键，如 "key:3"、"global"
}

// Notifier 通用 webhook 通知器。
//
// 设计目标：不绑定任何具体推送服务。Format 决定请求形状，URL 决定地址：
//   - generic: POST JSON（通用 webhook / 自建接收端 / n8n 等）
//   - meow:    POST 到 MeoW 的 /{昵称}/{标题}，JSON 体 {title,msg}
//   - text:    POST text/plain（简单脚本、少数支持纯文本的推送服务）
//   - get:     GET URL 模板，支持 {event}/{level}/{title}/{msg}/{data} 占位符
//
// 投递在独立 goroutine 中进行，带超时、有限重试和按事件去重，
// 任何失败都只写日志，不影响翻译请求。
type Notifier struct {
	// cfg 用原子指针保存：webhook 配置可以在管理后台修改并立即生效，
	// 投递 goroutine 与 Notify 调用方都不会读到撕裂的半个配置。
	cfg    atomic.Pointer[config.WebhookConfig]
	client *http.Client
	ctx    context.Context

	queue chan Notification
	stop  chan struct{}
	wg    sync.WaitGroup
	// closeOnce 保证 stop 只关一次；关闭后 Notify 不再入队。
	closeOnce sync.Once

	mu       sync.Mutex
	lastSent map[string]time.Time
	closed   bool
}

// NewNotifier 创建通知器。cfg 未启用时返回的对象所有方法都是空操作，
// 调用方无需到处判空。
func NewNotifier(ctx context.Context, cfg config.WebhookConfig) *Notifier {
	n := &Notifier{
		ctx:      ctx,
		queue:    make(chan Notification, notifyQueueSize),
		stop:     make(chan struct{}),
		lastSent: make(map[string]time.Time),
	}
	normalized := cfg.Normalized()
	n.cfg.Store(&normalized)
	n.client = &http.Client{
		// 超时跟随单次投递的上下文（webhook.timeout_seconds 可在后台调整），
		// 这里不再固定 http.Client.Timeout。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if cfg.Enabled && !cfg.Configured() {
		log.Printf("webhook: enabled but not configured (format=%s); notifications will be skipped", cfg.Format)
	}
	// 始终启动投递 goroutine：配置可能稍后通过管理后台启用。
	// Add 必须在启动 goroutine 之前：Close() 可能在 goroutine
	// 真正执行到 Add 之前就调用了 Wait，那样会漏等这次投递。
	n.wg.Add(1)
	go n.run()
	return n
}

// CurrentConfig 返回当前生效的 webhook 配置副本。
func (n *Notifier) CurrentConfig() config.WebhookConfig {
	if n == nil {
		return config.WebhookConfig{}
	}
	if p := n.cfg.Load(); p != nil {
		return *p
	}
	return config.WebhookConfig{}
}

// Update 热更新配置。已排队但未投递的通知会使用新配置发送。
func (n *Notifier) Update(cfg config.WebhookConfig) {
	if n == nil {
		return
	}
	normalized := cfg.Normalized()
	n.cfg.Store(&normalized)
	if normalized.Enabled && !normalized.Configured() {
		log.Printf("webhook: enabled but not configured (format=%s); notifications will be skipped", normalized.Format)
	}
}

// Enabled 报告通知器是否会真正投递。
func (n *Notifier) Enabled() bool {
	return n != nil && n.CurrentConfig().Configured()
}

// NotifyTemporaryEnabled 报告临时熔断是否需要通知。
func (n *Notifier) NotifyTemporaryEnabled() bool {
	cfg := n.CurrentConfig()
	return n != nil && cfg.Enabled && cfg.NotifyTemporary
}

// Notify 把一个事件放进投递队列。
//
// 非阻塞：队列满、通知器关闭、或事件被配置关闭时直接返回。
func (n *Notifier) Notify(msg Notification) {
	if !n.Enabled() || !n.wants(msg.Event) {
		return
	}
	if msg.Level == "" {
		msg.Level = "warning"
	}
	if msg.DedupeKey == "" {
		msg.DedupeKey = msg.Event
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()
	if !n.allow(msg.DedupeKey) {
		return
	}
	select {
	case n.queue <- msg:
	default:
		log.Printf("webhook: queue full, dropped %s notification", msg.Event)
	}
}

// allow 按 DedupeKey 做节流，避免"没有可用 key"时每个请求推一条。
func (n *Notifier) allow(dedupeKey string) bool {
	window := time.Duration(n.CurrentConfig().MinIntervalSeconds) * time.Second
	if window <= 0 {
		return true
	}
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.lastSent[dedupeKey]; ok && now.Sub(last) < window {
		return false
	}
	n.lastSent[dedupeKey] = now
	return true
}

func (n *Notifier) wants(event string) bool {
	cfg := n.CurrentConfig()
	switch event {
	case EventKeyDisabled, EventQuotaExhausted:
		return cfg.NotifyKeyDisabled
	case EventKeyTemporary:
		return cfg.NotifyTemporary
	case EventNoKeys:
		return cfg.NotifyNoKeys
	default:
		return false
	}
}

// Close 停止投递并等待正在进行的这次投递结束。
//
// 关闭靠独立的 stop channel，而不是只依赖应用 ctx：
// 这样即使调用方没有先取消 ctx（测试、或启动失败提前关闭），
// 也不会让 Wait 永远等一个空队列上的阻塞 select。
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.mu.Unlock()
		close(n.stop)
	})
	n.wg.Wait()
}

func (n *Notifier) run() {
	defer n.wg.Done()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.stop:
			return
		case msg := <-n.queue:
			n.deliverWithRetry(n.ctx, n.CurrentConfig(), msg)
		}
	}
}

func (n *Notifier) deliverWithRetry(ctx context.Context, cfg config.WebhookConfig, msg Notification) error {
	const attempts = 3
	backoff := 300 * time.Millisecond
	var lastErr error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err, retryable := n.deliver(ctx, cfg, msg)
		if err == nil {
			if i > 0 {
				log.Printf("webhook: %s delivered after %d attempts", msg.Event, i+1)
			}
			return nil
		}
		lastErr = err
		if !retryable || i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.stop:
			return lastErr
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	log.Printf("webhook: deliver %s failed: %v", msg.Event, lastErr)
	return lastErr
}

// SendNow 同步投递一条通知，用于管理后台的“发送测试”。
//
// 与 Notify 的区别：不排队、不受事件开关与节流限制，错误直接返回给调用方，
// 便于把失败原因展示在页面上。
func (n *Notifier) SendNow(ctx context.Context, msg Notification) error {
	if n == nil {
		return fmt.Errorf("通知器未初始化")
	}
	return n.SendNowWithConfig(ctx, n.CurrentConfig(), msg)
}

// SendNowWithConfig 用指定配置同步投递一次（不排队、不受去重节流限制）。
//
// 管理后台"发送测试通知"用它：用户可以先用未保存的表单值试一次，
// 确认能收到再保存，避免保存前后行为不一致。
func (n *Notifier) SendNowWithConfig(ctx context.Context, cfg config.WebhookConfig, msg Notification) error {
	if n == nil {
		return fmt.Errorf("通知器未初始化")
	}
	cfg = cfg.Normalized()
	if !cfg.Enabled {
		return fmt.Errorf("webhook 未启用")
	}
	if !cfg.Configured() {
		return fmt.Errorf("webhook 缺少必填配置（%s）", cfg.Format)
	}
	if msg.Level == "" {
		msg.Level = "info"
	}
	if msg.DedupeKey == "" {
		msg.DedupeKey = "test"
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return n.deliverWithRetry(ctx, cfg, msg)
}

// deliver 投递一次，返回 (错误, 是否值得重试)。
func (n *Notifier) deliver(ctx context.Context, cfg config.WebhookConfig, msg Notification) (error, bool) {
	req, err := n.buildRequest(ctx, cfg, msg)
	if err != nil {
		return err, false
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err, true // 网络错误可重试
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil, false
	}
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return fmt.Errorf("webhook status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), retryable
}

func (n *Notifier) buildRequest(ctx context.Context, cfg config.WebhookConfig, msg Notification) (*http.Request, error) {
	title := n.fullTitle(cfg, msg.Title)
	switch cfg.Format {
	case "meow":
		return n.buildMeowRequest(ctx, cfg, title, msg)
	case "text":
		body := title + "\n\n" + msg.Message
		if len(msg.Data) > 0 {
			body += "\n\n" + renderData(msg.Data)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		n.setCommonHeaders(req, msg)
		return req, nil
	case "get":
		return n.buildGetRequest(ctx, cfg, title, msg)
	default: // generic
		payload := map[string]any{
			"service":   "deepl-proxy",
			"event":     msg.Event,
			"level":     msg.Level,
			"title":     title,
			"message":   msg.Message,
			"data":      msg.Data,
			"timestamp": time.Now().Format(time.RFC3339),
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		n.setCommonHeaders(req, msg)
		return req, nil
	}
}

// buildMeowRequest 构造 MeoW 兼容请求。
//
// MeoW 的路径是 /{昵称}/{标题}，正文为 JSON {title,msg}；
// 昵称含斜杠会被当成多段路径，因此这里显式拒绝而不是静默改写。
func (n *Notifier) buildMeowRequest(ctx context.Context, cfg config.WebhookConfig, title string, msg Notification) (*http.Request, error) {
	nickname := strings.TrimSpace(cfg.Nickname)
	if nickname == "" || strings.ContainsAny(nickname, "/?#") {
		return nil, fmt.Errorf("meow nickname %q is invalid", nickname)
	}
	base := strings.TrimSpace(cfg.URL)
	if base == "" {
		base = meowDefaultBase
	}
	target := strings.TrimRight(base, "/") + "/" + url.PathEscape(nickname)
	if title != "" {
		target += "/" + url.PathEscape(title)
	}
	payload := map[string]any{
		"title": title,
		"msg":   msg.Message,
		// 分类名最多 10 个字符（MeoW 限制），"DeepL代理" 为 7 个字符。
		"category": "DeepL代理",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	n.setCommonHeaders(req, msg)
	return req, nil
}

// buildGetRequest 用 URL 模板拼接 GET 请求。
//
// 模板里没有占位符时退化为追加查询参数，这样"只给一个地址"的服务
// （如 Bark 的 /{key}/{msg}）也能直接使用。
func (n *Notifier) buildGetRequest(ctx context.Context, cfg config.WebhookConfig, title string, msg Notification) (*http.Request, error) {
	raw := cfg.URL
	if !strings.Contains(raw, "{") {
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		raw += sep + "title=" + url.QueryEscape(title) + "&msg=" + url.QueryEscape(msg.Message) +
			"&event=" + url.QueryEscape(msg.Event) + "&level=" + url.QueryEscape(msg.Level)
	} else {
		// 占位符既可能落在路径里也可能落在 query 里，因此空格统一编码成
		// %20（QueryEscape 的 "+" 只在 query 中合法，放进路径会变成字面加号）。
		replacer := strings.NewReplacer(
			"{event}", escapeTemplate(msg.Event),
			"{level}", escapeTemplate(msg.Level),
			"{title}", escapeTemplate(title),
			"{msg}", escapeTemplate(msg.Message),
			"{data}", escapeTemplate(renderData(msg.Data)),
		)
		raw = replacer.Replace(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	n.setCommonHeaders(req, msg)
	return req, nil
}

// escapeTemplate 为 URL 模板占位符做编码。
func escapeTemplate(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func (n *Notifier) setCommonHeaders(req *http.Request, msg Notification) {
	req.Header.Set("User-Agent", "deepl-proxy/webhook")
	req.Header.Set("X-Webhook-Event", msg.Event)
}

func (n *Notifier) fullTitle(cfg config.WebhookConfig, title string) string {
	if title == "" {
		title = "通知"
	}
	if prefix := strings.TrimSpace(cfg.TitlePrefix); prefix != "" {
		return prefix + " · " + title
	}
	return title
}

// renderData 把附加数据渲染成稳定的 key=value 行，用于文本/GET 格式。
func renderData(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	// 固定顺序，保证同样的数据每次生成同样的文本（便于对比与去重）。
	sortStrings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", data[k])
	}
	return b.String()
}

func sortStrings(xs []string) {
	// 小切片插入排序，避免为一个通知引入 sort 包的反射路径。
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
