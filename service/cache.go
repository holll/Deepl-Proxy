package service

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/models"
)

type CacheService struct {
	writeDB *sql.DB
	readDB  *sql.DB
	cfg     *config.Config

	// ttl / cacheControl 在构造时算好：配置加载后不再变化，
	// 避免每个请求都做一次 Sprintf。
	ttl          time.Duration
	cacheControl string

	// legacyFallback 控制是否兼容读取 v1 键的旧行。
	// 默认开启（配置里不写就是开启），迁移完成后可以显式关掉。
	legacyFallback bool
}

func NewCacheService(writeDB, readDB *sql.DB, cfg *config.Config) *CacheService {
	cs := &CacheService{
		writeDB: writeDB,
		readDB:  readDB,
		cfg:     cfg,
		ttl:     cfg.Cache.TTLDuration(),
		// 默认开启：线上已有一批 v1 行，关掉等于上线即冷启动。
		legacyFallback: !cfg.Cache.LegacyKeyDisabled,
	}
	if cs.ttl > 0 {
		cs.cacheControl = "public, max-age=" + strconv.Itoa(int(cs.ttl.Seconds()))
	} else {
		cs.cacheControl = "no-cache"
	}
	return cs
}

type CachedHit struct {
	Body    []byte
	Headers http.Header
}

//
// ========================
// TTL
// ========================
//

func (cs *CacheService) TTL() time.Duration { return cs.ttl }

// CacheControlHeader 返回构造时预计算好的 Cache-Control 值，
// 不再每个响应都 Sprintf 一次。
func (cs *CacheService) CacheControlHeader() string { return cs.cacheControl }

//
// ========================
// Cache Key
// ========================
//

// cacheKeyVersion 变更编码规则时必须递增：旧键自然失效，不会与新键混淆，
// 旧行由后台清理任务按期回收，无需手工清表。
const cacheKeyVersion = "v2"

// ProviderClass 把 provider 归一到"响应体结构是否等价"的粒度。
//
// 这是缓存键里唯一新增的语义维度：provider 是轮询选出来的，
// deepl 与 deeplx 对同一请求产出的响应体结构并不相同
// （deeplx 恒返回 1 条 translation，且丢弃 7 个参数），
// 若不加区分就会把一方的响应回放给另一方。
//
// 只区分类别，不区分 key id / free-vs-pro / 具体 endpoint：
// 官方 free 与 pro 对同一请求的输出相同，把这些放进键只会碎片化缓存。
func ProviderClass(prov string) string {
	if strings.EqualFold(strings.TrimSpace(prov), "deeplx") {
		return "deeplx"
	}
	return "deepl"
}

// deeplxUnsupportedParams 是 deeplx provider 无法转发给上游的参数。
// 请求一旦带上这些参数，deeplx 就静默丢弃它们（返回"当作没传"的结果），
// 因此这类请求必须路由到官方 deepl，不能与 deepl 共用缓存键。
var deeplxUnsupportedParams = []string{
	"formality", "glossary_id", "context", "model_type",
	"split_sentences", "preserve_formatting", "outline_detection",
	"custom_instructions", "tag_handling_version", "show_billed_characters",
}

// CanServe 判断某 provider 能否忠实服务这次请求。
// deepl 全部支持；deeplx 只支持"单条 text + 只带它认识的参数"。
func CanServe(prov string, textCount int, extraParams map[string]string) bool {
	if ProviderClass(prov) != "deeplx" {
		return true
	}
	if textCount != 1 {
		// deeplx 把多条 text 用换行拼成一段再翻译，只会返回 1 条 translation，
		// 对 N 条 text 的请求结构上已经错了。
		return false
	}
	for _, f := range deeplxUnsupportedParams {
		if v, ok := extraParams[f]; ok && v != "" {
			return false
		}
	}
	return true
}

// appendField 以 "N:value" 的长度前缀写入一个字段。
// 字段序固定 + 每个字段都带长度前缀，因此不存在分隔符逃逸问题，
// ("ab","c") 与 ("a","bc") 这类拼接歧义在结构上不可能发生。
func appendField(b []byte, s string) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}

// appendList 编码字符串集合：丢弃空白项 → 排序 → 去重。
// 这三个列表在 DeepL 语义上都是集合，[a,b] 与 [b,a] 输出相同，
// 不归一就只会制造无谓的 miss。
func appendList(b []byte, xs []string) []byte {
	cp := make([]string, 0, len(xs))
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			cp = append(cp, x)
		}
	}
	sort.Strings(cp)
	cp = slices.Compact(cp)
	b = strconv.AppendInt(b, int64(len(cp)), 10)
	b = append(b, ':')
	for _, x := range cp {
		b = appendField(b, x)
	}
	return b
}

// BuildCacheKey 返回 64 位 hex 的 SHA-256。
//
// 与旧实现的三点差异：
//  1. 包含 providerClass —— 修复"deeplx 的响应被回放给官方 deepl 请求"的错误命中；
//  2. 长度前缀编码，不再把整段 text json.Marshal 一遍再哈希；
//  3. tag 列表排序去重，且不再带无语义价值的 "deepl:translate:" 前缀。
//
// text 逐字节原样参与编码、绝不归一：空白与 Unicode 形态都会改变上游输出，
// 对它做归一等于凭空制造错误命中。顺序有意义，保留数组序。
func BuildCacheKey(identity *models.CacheIdentity, providerClass string) string {
	buf := make([]byte, 0, 128+len(identity.Context))

	buf = appendField(buf, cacheKeyVersion)
	buf = appendField(buf, ProviderClass(providerClass))
	buf = appendField(buf, upperTrim(identity.TargetLang))
	buf = appendField(buf, upperTrim(identity.SourceLang))

	buf = strconv.AppendInt(buf, int64(len(identity.Text)), 10)
	buf = append(buf, ':')
	for _, t := range identity.Text {
		buf = appendField(buf, t)
	}

	buf = appendField(buf, strings.TrimSpace(identity.Formality))
	buf = appendField(buf, strings.TrimSpace(identity.GlossaryID))
	buf = appendField(buf, identity.Context)
	buf = appendField(buf, strings.TrimSpace(identity.ModelType))
	buf = appendField(buf, strings.TrimSpace(identity.SplitSentences))
	buf = appendField(buf, strings.TrimSpace(identity.PreserveFormatting))
	buf = appendField(buf, strings.TrimSpace(identity.TagHandling))
	buf = appendField(buf, strings.TrimSpace(identity.OutlineDetection))
	buf = appendField(buf, strings.TrimSpace(identity.CustomInstructions))
	buf = appendField(buf, strings.TrimSpace(identity.TagHandlingVersion))
	buf = appendField(buf, strings.TrimSpace(identity.ShowBilledCharacters))
	buf = appendList(buf, identity.NonSplittingTags)
	buf = appendList(buf, identity.SplittingTags)
	buf = appendList(buf, identity.IgnoreTags)

	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

func upperTrim(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

//
// ========================
// GET
// ========================
//

// Get 查询缓存。
//
// legacyKey 是 v1 键（由 BuildLegacyCacheKey 算出），wantTexts 是本次请求的
// text 条数，仅用于校验旧行是否可信 —— 旧键不含 provider 维度，
// 而 deeplx 产出的行恒定只有 1 条 translation：若把这样的行回放给
// 需要 N 条译文的请求，就是一次结构错误的响应。条数对不上就当作未命中。
func (cs *CacheService) Get(cacheKey, legacyKey string, wantTexts int) (*CachedHit, error) {
	// ttl<=0 表示禁用缓存：读取也必须关闭，否则旧的未过期行会被继续返回并续期，
	// 出现"配置说不缓存、实际却在缓存"的语义分裂。
	if cs.ttl <= 0 {
		return nil, nil
	}

	now := time.Now().UnixMilli()

	c, err := database.GetCacheByKey(cs.readDB, cacheKey, now)
	if err != nil {
		return nil, err
	}
	if c != nil {
		ttlMs := cs.ttl.Milliseconds()
		if c.ExpiresAt-now < ttlMs/2 {
			// 滑窗续期：CAS 到读到的那个 expires_at，并发命中只有一写成功，
			// 避免 N 次串行 UPDATE 挤占唯一写连接。
			if _, err := database.CompareAndRenewCache(cs.writeDB, cacheKey, c.ExpiresAt, now+ttlMs); err != nil {
				log.Printf("[cache] renew failed: %v", err)
			}
		}
		return &CachedHit{Body: c.Body, Headers: decodeCachedHeaders(c.HeadersJSON)}, nil
	}

	// 新键未命中，再按 v1 键查一次（迁移期内）。
	return cs.getLegacy(cacheKey, legacyKey, wantTexts, now)
}

// getLegacy 处理 v1 旧行的兼容读取与就地迁移。
func (cs *CacheService) getLegacy(cacheKey, legacyKey string, wantTexts int, now int64) (*CachedHit, error) {
	if !cs.legacyFallback || legacyKey == "" || legacyKey == cacheKey {
		return nil, nil
	}

	c, err := database.GetCacheByKey(cs.readDB, legacyKey, now)
	if err != nil || c == nil {
		return nil, err
	}

	// 旧键没有 provider 维度，无法从键本身判断这一行是哪个上游产出的。
	// 用响应体里的译文条数来判定：条数与本次请求一致才可信。
	// （deeplx 的行恒为 1 条，因此多 text 请求不会被它污染。）
	if wantTexts > 0 {
		if n, ok := countTranslations(c.Body); !ok || n != wantTexts {
			return nil, nil
		}
	}

	hit := &CachedHit{Body: c.Body, Headers: decodeCachedHeaders(c.HeadersJSON)}

	// 就地迁移：写一份到新键，之后同一请求直接走新键。
	// 旧行不删除、也不续期 —— 保留它是为了回滚时旧二进制仍能命中，
	// 它会按自己的 expires_at 被后台清理回收，不会长期双份占用。
	cs.Put(cacheKey, c.Body, hit.Headers)
	log.Printf("[cache] migrated legacy entry to key %s", cacheKey)

	return hit, nil
}

// countTranslations 从 DeepL 兼容响应体里数出译文条数。
// 解析失败或结构不符时返回 ok=false —— 这种行不参与兼容读取，
// 宁可少一次命中，也不回放来路不明的响应。
func countTranslations(body []byte) (int, bool) {
	var parsed struct {
		Translations []json.RawMessage `json:"translations"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	return len(parsed.Translations), true
}

func decodeCachedHeaders(raw string) http.Header {
	headers := make(http.Header)
	if raw == "" {
		return headers
	}
	var parsed map[string][]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return headers
	}
	// Add 会顺手做 MIME 规范化，旧行里大小写不一的键也能正确取出。
	for k, vs := range parsed {
		for _, v := range vs {
			headers.Add(k, v)
		}
	}
	return headers
}

//
// ========================
// PUT
// ========================
//

func (cs *CacheService) Put(cacheKey string, body []byte, upstreamHeaders http.Header) {
	if cs.ttl <= 0 {
		return
	}

	now := time.Now().UnixMilli()

	clean := sanitizeCacheHeaders(upstreamHeaders)
	clean.Set("Content-Type", "application/json; charset=utf-8")
	clean.Set("Cache-Control", cs.cacheControl)

	// http.Header 本身就是 map[string][]string，可直接序列化，
	// 不必再复制一份 map。
	headersJSON, _ := json.Marshal(clean)

	err := database.UpsertCache(cs.writeDB, &models.TranslateCache{
		CacheKey:    cacheKey,
		Body:        body,
		HeadersJSON: string(headersJSON),
		CreatedAt:   now,
		ExpiresAt:   now + cs.ttl.Milliseconds(),
	})
	if err != nil {
		log.Printf("[cache] put failed: %v", err)
	}
}

//
// ========================
// Cleanup
// ========================
//

func (cs *CacheService) CleanExpired() {
	if err := database.CleanExpiredCache(
		cs.writeDB,
		cs.cfg.Cache.CleanupBatchSize,
		cs.cfg.Cache.CleanupMaxRounds,
	); err != nil {
		log.Printf("[cache] cleanup failed: %v", err)
	}
}

//
// ========================
// Helpers
// ========================
//

func IsNoCacheRequest(r *http.Request) bool {
	v := strings.TrimSpace(r.Header.Get("x-no-cache"))
	return v == "1" || strings.EqualFold(v, "true")
}

// cacheHeaderBlocklist 是绝不写入缓存、也绝不在命中时回放的响应头。
// 它们都携带"这条响应由谁产生"的信息，缓存命中时已经不成立，
// 回放会给客户端一个与实际路由无关的假象。
var cacheHeaderBlocklist = map[string]bool{
	"x-upstream-key-name": true,
	"x-key-site-type":     true,
	"set-cookie":          true,
	"authorization":       true,
}

func sanitizeCacheHeaders(h http.Header) http.Header {
	clean := make(http.Header, len(h))
	for k, vs := range h {
		if cacheHeaderBlocklist[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			clean.Add(k, v)
		}
	}
	return clean
}
