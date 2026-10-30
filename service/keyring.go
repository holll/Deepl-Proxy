package service

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand/v2"
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
	writeDB    *sql.DB
	readDB     *sql.DB
	sampleRate float64

	// state 是读路径唯一的共享状态：加载即读快照，不加锁、不复制。
	state atomic.Pointer[keySnapshot]

	// writeMu 串行化 state 的写入。重载期间会一直持有，
	// 但读路径不碰它，所以慢查询不会阻塞任何翻译请求。
	writeMu sync.Mutex

	// reloadMu 只串行化"重载"这件事，避免 N 个并发请求同时打 DB。
	reloadMu sync.Mutex

	// round-robin counter
	counter atomic.Int64

	// usageSem 限制后台用量查询的并发度（用 channel 当信号量）。
	usageSem chan struct{}
}

func NewKeyring(writeDB, readDB *sql.DB) *Keyring {
	kr := &Keyring{
		writeDB:    writeDB,
		readDB:     readDB,
		sampleRate: 0.05,
		usageSem:   make(chan struct{}, maxConcurrentUsageFetch),
	}
	kr.state.Store(&keySnapshot{dirty: true})
	return kr
}

// DB returns the read connection (for read-only admin queries).
func (kr *Keyring) DB() *sql.DB { return kr.readDB }

// WriteDB returns the write connection (for admin write operations).
func (kr *Keyring) WriteDB() *sql.DB { return kr.writeDB }

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
func (kr *Keyring) GetActiveKeys() ([]models.DeeplKey, error) {
	now := time.Now().UnixMilli()
	if s := kr.state.Load(); s != nil && !s.dirty && now-s.loadedAt < keyCacheTTL {
		return s.keys, nil
	}

	// 只让一个 goroutine 真正去查库，其余排队后命中双检。
	kr.reloadMu.Lock()
	defer kr.reloadMu.Unlock()

	if s := kr.state.Load(); s != nil && !s.dirty && now-s.loadedAt < keyCacheTTL {
		return s.keys, nil
	}

	// 关键：慢查询在 writeMu 下进行，而读路径只看 atomic.Pointer，
	// 因此重载期间其余请求仍然可以拿到上一次的快照继续服务。
	kr.writeMu.Lock()
	defer kr.writeMu.Unlock()

	loaded, err := kr.loadFromDB(now)
	if err != nil {
		return nil, err
	}
	kr.state.Store(&keySnapshot{keys: loaded, loadedAt: time.Now().UnixMilli()})
	return loaded, nil
}

func (kr *Keyring) loadFromDB(now int64) ([]models.DeeplKey, error) {
	// 先恢复已过禁用期的 key
	if err := database.ReactivateExpiredKeys(kr.writeDB, now); err != nil {
		log.Printf("keyring: reactivate expired keys failed: %v", err)
	}

	// SQL 已过滤 status='active'，这里再筛一遍是防御性的双保险。
	keys, err := database.GetActiveKeys(kr.readDB)
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

func (kr *Keyring) AddKey(name, authKey, endpoint, prov string) (int64, error) {
	id, err := database.InsertKey(kr.writeDB, name, authKey, endpoint, prov)
	if err == nil {
		kr.invalidateKeyCache()
		kr.sampleUsageAsync(context.Background(), models.DeeplKey{
			ID: id, AuthKey: authKey, Endpoint: endpoint, Provider: prov,
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
		if err := database.UpdateUsageSnapshot(kr.writeDB, keyID, info.CharacterCount, info.CharacterLimit, now); err != nil {
			log.Printf("keyring: save usage for key %d failed: %v", keyID, err)
			return nil, err
		}
	}
	return info, nil
}

// sampleUsageAsync 以受限并发的方式后台刷新一次用量快照。
// 并发上限已满时直接放弃本次采样——用量是尽力而为，不能因此堆积 goroutine。
func (kr *Keyring) sampleUsageAsync(ctx context.Context, k models.DeeplKey) {
	select {
	case kr.usageSem <- struct{}{}:
	default:
		return
	}
	// 请求结束时它的 ctx 会被取消，但用量查询与请求生命周期无关，故用 WithoutCancel。
	base := context.WithoutCancel(ctx)
	go func() {
		defer func() { <-kr.usageSem }()
		fctx, cancel := context.WithTimeout(base, usageFetchTimeout)
		defer cancel()
		_, _ = kr.fetchAndSaveUsage(fctx, k.ID, k.AuthKey, k.Endpoint, k.Provider)
	}()
}

// RefreshAllUsage 查询所有 key（含未启用）的用量，用于启动时初始化。
func (kr *Keyring) RefreshAllUsage(ctx context.Context) {
	keys, err := database.GetAllKeysWithAuth(kr.readDB)
	if err != nil {
		log.Printf("keyring: refresh all usage failed: %v", err)
		return
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		_, _ = kr.fetchAndSaveUsage(ctx, k.ID, k.AuthKey, k.Endpoint, k.Provider)
	}
	log.Printf("keyring: refreshed usage for %d key(s)", len(keys))
}

func (kr *Keyring) UpdateKeyByID(id int64, name, authKey, endpoint, prov, status *string) error {
	err := database.UpdateKey(kr.writeDB, id, name, authKey, endpoint, prov, status)
	if err == nil {
		kr.invalidateKeyCache()
	}
	return err
}

func (kr *Keyring) DeleteKeyByID(id int64) error {
	err := database.DeleteKey(kr.writeDB, id)
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
		if result != nil {
			statusCode = result.StatusCode
			body = string(result.Body)
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

		kr.applyDisable(k, errType, statusCode, body, err)
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

func (kr *Keyring) applyDisable(k models.DeeplKey, errType string, statusCode int, body string, err error) {
	now := time.Now().UnixMilli()
	code := fmt.Sprintf("%d", statusCode)
	msg := body
	if err != nil {
		msg = err.Error()
	}

	switch errType {
	case "monthly":
		_ = database.DisableKeyMonthly(kr.writeDB, k.ID, code, msg, now)
	case "temporary":
		_ = database.DisableKeyTemporary(kr.writeDB, k.ID, code, msg, 5*60*1000, now)
	case "permanent":
		_ = database.DisableKeyPermanent(kr.writeDB, k.ID, code, msg, now)
	default:
		// network_error（超时等）：不禁用，仅记录错误信息
		return
	}

	kr.invalidateKeyCache()
}

func (kr *Keyring) shouldSampleUsage() bool {
	return rand.Float64() < kr.sampleRate
}

// RefreshUsage 手动刷新所有 active key 的用量
func (kr *Keyring) RefreshUsage(ctx context.Context) []UsageRefreshResult {
	keys, err := kr.GetActiveKeys()
	if err != nil {
		return []UsageRefreshResult{{KeyName: "", Ok: false, Error: err.Error()}}
	}

	var results []UsageRefreshResult
	for _, k := range keys {
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
				kr.applyDisable(k, errType, info.Status, info.Text, nil)
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
