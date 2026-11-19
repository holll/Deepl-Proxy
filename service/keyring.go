package service

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deepl-proxy/database"
	"deepl-proxy/models"
	"deepl-proxy/provider"
)

// keyCacheTTL 内存 key 缓存最大存活时间，确保临时禁用到期后能被发现。
const keyCacheTTL int64 = 30 * 1000 // 30 秒

// usageFetchTimeout 单次用量查询的独立超时。用量是尽力而为的快照，
// 不能因为一次慢请求把 goroutine 挂死。
const usageFetchTimeout = 20 * time.Second

// maxConcurrentUsageFetch 限制同时在跑的用量查询数量。
const maxConcurrentUsageFetch = 4

// keySnapshot 是 key 列表的不可变快照（copy-on-write）。
// 读路径通过 atomic.Pointer 拿到它，完全无锁、也不复制切片；
// 调用方必须把它当作只读，不得修改其中的元素或切片。
type keySnapshot struct {
	keys     []models.DeeplKey
	loadedAt int64
	dirty    bool
}

// Keyring 管理上游 key 的轮询、熔断和用量快照。
type Keyring struct {
	lifecycle  context.Context
	writeDB    *sql.DB
	readDB     *sql.DB
	sampleRate float64

	// state 是读路径唯一的共享状态：加载即读快照，不加锁、不复制。
	state atomic.Pointer[keySnapshot]

	// writeMu 串行化 state 的写入。重载期间会一直持有，
	// 新鲜快照读路径不碰它；过期读需等待有预算的重载。
	writeMu sync.Mutex

	// reloadMu 只串行化"重载"这件事，避免 N 个并发请求同时打 DB。
	reloadMu chan struct{}

	// round-robin counter
	counter atomic.Int64

	// notifier 由 main 在启动时注入；读多写一次，用 atomic.Pointer 免锁。
	notifier atomic.Pointer[Notifier]

	// usageSem 限制后台用量查询的并发度（用 channel 当信号量）。
	usageSem chan struct{}
	usageMu  sync.Mutex
	usageWG  sync.WaitGroup
	closed   bool
}

func NewKeyring(ctx context.Context, writeDB, readDB *sql.DB) *Keyring {
	kr := &Keyring{
		lifecycle:  ctx,
		reloadMu:   make(chan struct{}, 1),
		writeDB:    writeDB,
		readDB:     readDB,
		sampleRate: 0.05,
		usageSem:   make(chan struct{}, maxConcurrentUsageFetch),
	}
	kr.state.Store(&keySnapshot{dirty: true})
	return kr
}

// Context returns the application lifecycle, never a single request context.
func (kr *Keyring) Context() context.Context { return kr.lifecycle }

// DB returns the read connection (for read-only admin queries).
func (kr *Keyring) DB() *sql.DB { return kr.readDB }

// WriteDB returns the write connection (for admin write operations).
func (kr *Keyring) WriteDB() *sql.DB { return kr.writeDB }

// SetNotifier 注入 webhook 通知器。必须在开始处理请求前调用一次。
func (kr *Keyring) SetNotifier(n *Notifier) { kr.notifier.Store(n) }

// NotifyNoKeys 在没有任何可用 key 时发出告警。
//
// reason 会原样出现在通知正文里，调用方应给出用户能看懂的原因
// （例如"所有 key 均已熔断"或"没有能处理该请求的 key 类别"）。
// 去重键是全局的：一次故障只推一条，恢复后再次故障会重新推送。
func (kr *Keyring) NotifyNoKeys(reason string) {
	n := kr.notifier.Load()
	if n == nil {
		return
	}
	n.Notify(Notification{
		Event:     EventNoKeys,
		Title:     "没有可用 Key",
		Message:   reason,
		Level:     "critical",
		DedupeKey: "no_keys:global",
		Data: map[string]any{
			"reason": reason,
		},
	})
}

// notifyKeyEvent 把一次熔断转成 webhook 通知。所有失败都只影响日志。
func (kr *Keyring) notifyKeyEvent(event string, k models.DeeplKey, level, title, detail string, data map[string]any) {
	n := kr.notifier.Load()
	if n == nil {
		return
	}
	msg := fmt.Sprintf("Key「%s」%s", k.Name, detail)
	n.Notify(Notification{
		Event:     event,
		Title:     title,
		Message:   msg,
		Level:     level,
		DedupeKey: fmt.Sprintf("key:%d", k.ID),
		Data:      data,
	})
}

// invalidateKeyCache marks the in-memory key list as stale.
//
// 必须持 writeMu：重载期间 writeMu 一直被持有，这样"置脏"就不会插进
// 一次重载的 DB 读窗口里被随后那次 Store 覆盖掉（否则刚熔断的 key
// 还能继续被使用最多 30 秒）。
func (kr *Keyring) invalidateKeyCache() {
	kr.writeMu.Lock()
	defer kr.writeMu.Unlock()

	old := kr.state.Load()
	if old == nil || old.dirty {
		return
	}
	kr.state.Store(&keySnapshot{keys: old.keys, loadedAt: old.loadedAt, dirty: true})
}

// GetActiveKeys returns active keys from memory cache, reloading from DB when dirty or cache expired.
//
// 返回的切片是共享只读快照：调用方只读不写，因此零拷贝、零分配。
func (kr *Keyring) GetActiveKeys(ctx context.Context) ([]models.DeeplKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if s := kr.state.Load(); s != nil && !s.dirty && now-s.loadedAt < keyCacheTTL {
		return s.keys, nil
	}

	// Only stale reads allocate a timer; fresh snapshots stay allocation-free.
	ctx, cancel := context.WithTimeout(ctx, database.OperationTimeout)
	defer cancel()
	// 只让一个 goroutine 真正去查库，其余排队后命中双检。
	select {
	case kr.reloadMu <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-kr.reloadMu }()

	if s := kr.state.Load(); s != nil && !s.dirty && now-s.loadedAt < keyCacheTTL {
		return s.keys, nil
	}

	// 关键：慢查询在 writeMu 下进行，而读路径只看 atomic.Pointer，
	// 过期/置脏的调用仍需等待重载；等待与 SQL 共享同一操作预算。
	kr.writeMu.Lock()
	defer kr.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	loaded, err := kr.loadFromDB(ctx, now)
	if err != nil {
		return nil, err
	}
	kr.state.Store(&keySnapshot{keys: loaded, loadedAt: time.Now().UnixMilli()})
	return loaded, nil
}

// RunKeyRecovery 恢复已到期的 Key，即使没有翻译请求也会运行。
func (kr *Keyring) RunKeyRecovery(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		recovered, err := database.ReactivateExpiredKeys(ctx, kr.writeDB, time.Now().UnixMilli())
		if err != nil && ctx.Err() == nil {
			log.Printf("keyring: background key recovery failed: %v", err)
		}
		if recovered > 0 {
			kr.invalidateKeyCache()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (kr *Keyring) loadFromDB(ctx context.Context, now int64) ([]models.DeeplKey, error) {
	// 翻译请求刷新快照时也可立即恢复，不必等下一次后台检查。
	if _, err := database.ReactivateExpiredKeys(ctx, kr.writeDB, now); err != nil {
		log.Printf("keyring: reactivate expired keys failed: %v", err)
	}

	// SQL 已过滤 status='active'，这里再筛一遍是防御性的双保险。
	keys, err := database.GetActiveKeys(ctx, kr.readDB)
	if err != nil {
		return nil, err
	}
	active := keys[:0]
	for _, k := range keys {
		if k.Status == "active" {
			active = append(active, k)
		}
	}
	return active, nil
}

// --- Key CRUD wrappers (invalidate cache on write) ---

func (kr *Keyring) AddKey(ctx context.Context, name, authKey, endpoint, prov, quotaPeriod string) (int64, error) {
	id, err := database.InsertKey(ctx, kr.writeDB, name, authKey, endpoint, prov, quotaPeriod)
	if err == nil {
		kr.invalidateKeyCache()
		kr.sampleUsageAsync(ctx, models.DeeplKey{
			ID: id, AuthKey: authKey, Endpoint: endpoint, Provider: prov, QuotaPeriod: quotaPeriod,
		})
	}
	return id, err
}

// fetchAndSaveUsage 查询指定 key 的用量并保存到数据库，返回用量信息。
//
// 注意：这里刻意不调用 invalidateKeyCache。用量快照只写
// character_count / character_limit / last_checked_at，而 GetActiveKeys
// 只按 status='active' 过滤，这几个字段既不参与过滤也不进入缓存键。
// 旧实现在每次采样成功后都置脏，配合 5% 采样率会让内存缓存形同虚设，
// 并让每个请求都掉进"锁内查库"的慢路径。
func (kr *Keyring) fetchAndSaveUsage(ctx context.Context, keyID int64, authKey, endpoint, prov string) (*provider.UsageInfo, error) {
	p := provider.DetectProvider(prov)
	if !p.SupportsUsage() {
		return nil, nil
	}
	info, err := p.FetchUsage(ctx, endpoint, authKey)
	if err != nil {
		log.Printf("keyring: fetch usage for key %d failed: %v", keyID, err)
		return nil, err
	}
	if info.Ok && (info.CharacterCount != nil || info.CharacterLimit != nil) {
		now := time.Now().UnixMilli()
		if err := database.UpdateUsageSnapshot(ctx, kr.writeDB, keyID, info.CharacterCount, info.CharacterLimit, now); err != nil {
			log.Printf("keyring: save usage for key %d failed: %v", keyID, err)
			return nil, err
		}
	}
	return info, nil
}

// sampleUsageAsync 以受限并发的方式后台刷新一次用量快照。
// 并发上限已满时直接放弃本次采样——用量是尽力而为，不能因此堆积 goroutine。
func (kr *Keyring) sampleUsageAsync(ctx context.Context, k models.DeeplKey) {
	kr.usageMu.Lock()
	defer kr.usageMu.Unlock()
	if kr.closed || kr.lifecycle.Err() != nil || ctx.Err() != nil {
		return
	}
	select {
	case kr.usageSem <- struct{}{}:
	default:
		return
	}
	// 采样与单次请求无关，但必须继承应用关闭，并有独立有限预算。
	base := kr.lifecycle
	kr.usageWG.Add(1)
	go func() {
		defer kr.usageWG.Done()
		defer func() { <-kr.usageSem }()
		fctx, cancel := context.WithTimeout(base, usageFetchTimeout)
		defer cancel()
		_, _ = kr.fetchAndSaveUsage(fctx, k.ID, k.AuthKey, k.Endpoint, k.Provider)
	}()
}

// Close prevents new usage samples and waits for already bounded workers.
// The application lifecycle must be canceled before calling Close during shutdown.
func (kr *Keyring) Close() {
	kr.usageMu.Lock()
	kr.closed = true
	kr.usageMu.Unlock()
	kr.usageWG.Wait()
}

// RefreshAllUsage 查询所有 key（含未启用）的用量，用于启动时初始化。
func (kr *Keyring) RefreshAllUsage(ctx context.Context) {
	keys, err := database.GetAllKeysWithAuth(ctx, kr.readDB)
	if err != nil {
		log.Printf("keyring: refresh all usage failed: %v", err)
		return
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		fctx, cancel := context.WithTimeout(ctx, usageFetchTimeout)
		_, _ = kr.fetchAndSaveUsage(fctx, k.ID, k.AuthKey, k.Endpoint, k.Provider)
		cancel()
	}
	log.Printf("keyring: refreshed usage for %d key(s)", len(keys))
}

func (kr *Keyring) UpdateKeyByID(ctx context.Context, id int64, name, authKey, endpoint, prov, quotaPeriod, status *string) error {
	err := database.UpdateKey(ctx, kr.writeDB, id, name, authKey, endpoint, prov, quotaPeriod, status)
	if err == nil {
		kr.invalidateKeyCache()
	}
	return err
}

func (kr *Keyring) DeleteKeyByID(ctx context.Context, id int64) error {
	err := database.DeleteKey(ctx, kr.writeDB, id)
	if err == nil {
		kr.invalidateKeyCache()
	}
	return err
}

// Selection 描述一次轮询选择的结果。
// Keys 只包含同一 provider 类别的 key，ProviderClass 与之一一对应，
// 因此"用哪个类别建缓存键"和"实际由哪个类别服务"始终一致。
type Selection struct {
	ProviderClass string
	Keys          []models.DeeplKey
	Start         int
}

// Select 在可服务的 key 里挑一轮，并在 provider 类别之间轮询。
//
// 为什么要按类别分组：deepl 与 deeplx 的响应体结构不同，缓存键必须区分；
// 若在同一次轮询里跨类别轮换，就会出现"键说 deepl、实际 deeplx"的错配。
// 按类别轮询还能让两个类别各自维护自己的缓存空间，不会互相打掉命中率。
func (kr *Keyring) Select(all []models.DeeplKey, textCount int, extraParams map[string]string) (Selection, bool) {
	var groups [][]models.DeeplKey
	var classes []string
	classIndex := make(map[string]int, 2)

	for _, k := range all {
		if !CanServe(k.Provider, textCount, extraParams) {
			continue
		}
		c := ProviderClass(k.Provider)
		i, ok := classIndex[c]
		if !ok {
			i = len(groups)
			classIndex[c] = i
			classes = append(classes, c)
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], k)
	}

	if len(groups) == 0 {
		return Selection{}, false
	}

	sel := int(kr.counter.Add(1) - 1)
	gi := sel % len(groups)
	g := groups[gi]
	return Selection{
		ProviderClass: classes[gi],
		Keys:          g,
		Start:         (sel / len(groups)) % len(g),
	}, true
}

// Translate 按 Selection 给定的起点依次尝试 key，失败时自动熔断。
//
// ctx 会一路传到 http.NewRequestWithContext：客户端断开时立刻停止，
// 不再让后续 key 继续跑满 15s 超时、白白消耗付费字符数。
func (kr *Keyring) Translate(ctx context.Context, sel Selection, req *provider.TranslateRequest) (*provider.TranslateResult, *models.DeeplKey, *FailureInfo, error) {
	n := len(sel.Keys)
	if n == 0 {
		return nil, nil, nil, fmt.Errorf("no keys")
	}

	var lastFailure *FailureInfo

	for i := range n {
		k := sel.Keys[(sel.Start+i)%n]
		prov := provider.DetectProvider(k.Provider)
		result, err := prov.Translate(ctx, k.Endpoint, k.AuthKey, req)

		if err == nil && result.StatusCode < 500 {
			if prov.SupportsUsage() && kr.shouldSampleUsage() {
				kr.sampleUsageAsync(ctx, k)
			}
			key := k
			return result, &key, nil, nil
		}

		// 客户端已断开：后续 key 不必再试。
		if err := ctx.Err(); err != nil {
			return nil, nil, lastFailure, err
		}

		errType := "network_error"
		statusCode := 0
		body := ""
		retryAfter := ""
		if result != nil {
			statusCode = result.StatusCode
			body = string(result.Body)
			if statusCode == http.StatusTooManyRequests {
				retryAfter = result.Headers.Get("Retry-After")
			}
		}

		// provider 命中非成功状态码（456/429/5xx/401/403）时会同时返回 result 和 error，
		// 只有连连接都没建立（result == nil）才是真正的 network_error。
		// 若仅以 err == nil 判断，会把这些状态码全部误判为 network_error 而永不熔断。
		if result != nil {
			siteType := "deeplx"
			if k.Provider == "deepl" || k.Provider == "" {
				siteType = provider.DetectSiteType(k.Endpoint)
			}
			errType = provider.ClassifyError(statusCode, body, siteType)
		}

		// 打印上游错误详情
		if result != nil {
			log.Printf("keyring: key %s upstream %d [%s]: %s", k.Name, statusCode, errType, truncateStr(body, 200))
		} else {
			log.Printf("keyring: key %s network error: %v", k.Name, err)
		}

		kr.applyDisable(ctx, k, errType, statusCode, body, retryAfter, err)
		lastFailure = &FailureInfo{
			KeyName: k.Name,
			Status:  statusCode,
			Body:    truncateStr(body, 160),
			Err:     err,
		}
	}

	msg := "All keys unavailable"
	if lastFailure != nil {
		msg = fmt.Sprintf("All keys unavailable, last failure: %s", lastFailure.KeyName)
	}
	return nil, nil, lastFailure, fmt.Errorf("%s", msg)
}

// FailureInfo 记录最后一次上游失败的信息
type FailureInfo struct {
	KeyName string
	Status  int
	Body    string
	Err     error
}

const defaultTemporaryCooldown = 5 * time.Minute

func temporaryCooldown(statusCode int, retryAfter string, now time.Time) time.Duration {
	if statusCode != http.StatusTooManyRequests {
		return defaultTemporaryCooldown
	}
	value := strings.TrimSpace(retryAfter)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil && seconds <= uint64((1<<63-1)/time.Second) {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return max(0, retryAt.Sub(now))
	}
	return defaultTemporaryCooldown
}

func (kr *Keyring) applyDisable(ctx context.Context, k models.DeeplKey, errType string, statusCode int, body, retryAfter string, err error) {
	nowTime := time.Now().Truncate(time.Millisecond)
	now := nowTime.UnixMilli()
	code := fmt.Sprintf("%d", statusCode)
	msg := body
	if err != nil {
		msg = err.Error()
	}

	switch errType {
	case "monthly":
		// 配额用尽：停到本周期结束。周期由 key 的配额类型决定——
		// 新免费 key 是每年 100 万，旧 key 是每月 50 万，不能混用。
		period := ResolveQuotaPeriod(k.QuotaPeriod, k.CharacterLimit)
		until := QuotaResetAt(time.Now(), period)
		if err := database.DisableKeyQuota(ctx, kr.writeDB, k.ID, until.UnixMilli(), code, msg, now); err != nil {
			log.Printf("keyring: disable key %d for quota failed: %v", k.ID, err)
		}
		kr.notifyKeyEvent(EventQuotaExhausted, k, "error", "Key 配额用尽",
			fmt.Sprintf("配额已用尽（%s），已停用到 %s。", quotaPeriodLabel(period), until.Format("2006-01-02 15:04")),
			map[string]any{
				"key_id":           k.ID,
				"endpoint":         k.Endpoint,
				"provider":         k.Provider,
				"quota_period":     period,
				"status":           statusCode,
				"disabled_until":   until.Format(time.RFC3339),
				"upstream_message": truncateStr(msg, 300),
				"character_count":  k.CharacterCount,
				"character_limit":  k.CharacterLimit,
			})
	case "temporary":
		cooldown := temporaryCooldown(statusCode, retryAfter, nowTime)
		if err := database.DisableKeyTemporary(ctx, kr.writeDB, k.ID, code, msg, cooldown.Milliseconds(), now); err != nil {
			log.Printf("keyring: temporarily disable key %d failed: %v", k.ID, err)
			return
		}
		if n := kr.notifier.Load(); n != nil && n.NotifyTemporaryEnabled() {
			kr.notifyKeyEvent(EventKeyTemporary, k, "warning", "Key 临时停用",
				fmt.Sprintf("上游返回 %d，已临时停用到 %s。", statusCode, nowTime.Add(cooldown).Format(time.RFC3339)),
				map[string]any{
					"key_id":           k.ID,
					"endpoint":         k.Endpoint,
					"provider":         k.Provider,
					"status":           statusCode,
					"upstream_message": truncateStr(msg, 300),
				})
		}
	case "permanent":
		_ = database.DisableKeyPermanent(ctx, kr.writeDB, k.ID, code, msg, now)
		kr.notifyKeyEvent(EventKeyDisabled, k, "critical", "Key 已失效",
			fmt.Sprintf("上游返回 %d，该 key 已永久停用，请更换。", statusCode),
			map[string]any{
				"key_id":           k.ID,
				"endpoint":         k.Endpoint,
				"provider":         k.Provider,
				"status":           statusCode,
				"upstream_message": truncateStr(msg, 300),
			})
	default:
		// network_error（超时等）：不禁用，仅记录错误信息
		return
	}

	kr.invalidateKeyCache()
}

// KeyTestResult 一次性手动测试的结果（只读检查，不改数据库）。
type KeyTestResult struct {
	Ok        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	SiteType  string `json:"site_type"`
	ErrorType string `json:"error_type,omitempty"`
	Message   string `json:"message,omitempty"`
}

// TestKey 用一条最小请求真实调用上游，验证 key 是否可用。
//
// 刻意不写数据库：手动测试是"先看看能不能用"，是否熔断应该由真实
// 翻译流量决定，否则运维点一次测试就把 key 禁掉会很意外。
func (kr *Keyring) TestKey(ctx context.Context, id int64) (KeyTestResult, error) {
	key, err := database.GetKeyByID(ctx, kr.readDB, id)
	if err != nil {
		return KeyTestResult{}, err
	}
	res := KeyTestResult{Provider: key.Provider, Endpoint: key.Endpoint}
	if key.Provider == "" || key.Provider == "deepl" {
		res.SiteType = provider.DetectSiteType(key.Endpoint)
	} else {
		res.SiteType = "deeplx"
	}

	prov := provider.DetectProvider(key.Provider)
	// deepl 走 form-urlencoded（只读 FormParams），deeplx 读 Text，
	// 两者都填上，测试接口才对两类 provider 都成立。
	form := url.Values{}
	form.Set("text", "ping")
	form.Set("target_lang", "ZH")
	start := time.Now()
	out, err := prov.Translate(ctx, key.Endpoint, key.AuthKey, &provider.TranslateRequest{
		Text:       []string{"ping"},
		TargetLang: "ZH",
		FormParams: form.Encode(),
	})
	res.LatencyMS = time.Since(start).Milliseconds()
	if out != nil {
		res.Status = out.StatusCode
	}

	switch {
	case err == nil && out != nil && out.StatusCode < 500:
		res.Ok = true
	case out != nil:
		res.ErrorType = provider.ClassifyError(out.StatusCode, string(out.Body), res.SiteType)
		res.Message = truncateStr(string(out.Body), 300)
	default:
		res.ErrorType = "network_error"
		if err != nil {
			res.Message = err.Error()
		}
	}
	return res, nil
}

// quotaPeriodLabel 把内部周期标识转成通知里可读的中文。
func quotaPeriodLabel(period string) string {
	switch period {
	case QuotaPeriodYearly:
		return "每年配额"
	case QuotaPeriodMonthly:
		return "每月配额"
	default:
		return "配额"
	}
}

func (kr *Keyring) shouldSampleUsage() bool {
	return rand.Float64() < kr.sampleRate
}

// RefreshUsage 手动刷新所有 active key 的用量
func (kr *Keyring) RefreshUsage(ctx context.Context) []UsageRefreshResult {
	keys, err := kr.GetActiveKeys(ctx)
	if err != nil {
		return []UsageRefreshResult{{KeyName: "", Ok: false, Error: err.Error()}}
	}

	var results []UsageRefreshResult
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		if !provider.DetectProvider(k.Provider).SupportsUsage() {
			results = append(results, UsageRefreshResult{
				KeyName: k.Name,
				Ok:      false,
				Skipped: "provider does not support usage",
			})
			continue
		}
		info, err := kr.fetchAndSaveUsage(ctx, k.ID, k.AuthKey, k.Endpoint, k.Provider)
		if err != nil {
			results = append(results, UsageRefreshResult{
				KeyName: k.Name,
				Ok:      false,
				Error:   err.Error(),
			})
			continue
		}
		// 用量接口失败（403/401/456 等）说明 key 已不可用，按错误类型熔断，
		// 避免前端仍显示 active。
		if info != nil && !info.Ok {
			siteType := "deeplx"
			if k.Provider == "deepl" || k.Provider == "" {
				siteType = provider.DetectSiteType(k.Endpoint)
			}
			errType := provider.ClassifyError(info.Status, info.Text, siteType)
			if errType != "none" {
				kr.applyDisable(ctx, k, errType, info.Status, info.Text, info.RetryAfter, nil)
			}
		}
		data := map[string]any{}
		if info.CharacterCount != nil {
			data["character_count"] = *info.CharacterCount
		}
		if info.CharacterLimit != nil {
			data["character_limit"] = *info.CharacterLimit
		}
		results = append(results, UsageRefreshResult{
			KeyName: k.Name,
			Ok:      info.Ok,
			Status:  info.Status,
			Data:    data,
		})
	}
	return results
}

// UsageRefreshResult 用量刷新结果
type UsageRefreshResult struct {
	KeyName string         `json:"key"`
	Ok      bool           `json:"ok"`
	Skipped string         `json:"skipped,omitempty"`
	Status  int            `json:"status,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func truncateStr(s string, max int) string {
	return s[:min(len(s), max)]
}
