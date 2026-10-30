package models

// DeeplKey 上游 key 记录
type DeeplKey struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	AuthKey          string  `json:"-"`
	Endpoint         string  `json:"endpoint"`
	Provider         string  `json:"provider"`
	Status           string  `json:"status"`
	DisableType      *string `json:"disable_type,omitempty"`
	DisabledUntil    *int64  `json:"disabled_until,omitempty"`
	LastErrorCode    *string `json:"last_error_code,omitempty"`
	LastErrorMessage *string `json:"last_error_message,omitempty"`
	LastUsedAt       *int64  `json:"last_used_at,omitempty"`
	LastCheckedAt    *int64  `json:"last_checked_at,omitempty"`
	CharacterCount   *int64  `json:"character_count,omitempty"`
	CharacterLimit   *int64  `json:"character_limit,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	UpdatedAt        int64   `json:"updated_at"`

	// ISO helpers
	DisabledUntilISO *string `json:"disabled_until_iso,omitempty"`
	LastUsedAtISO    *string `json:"last_used_at_iso,omitempty"`
	LastCheckedAtISO *string `json:"last_checked_at_iso,omitempty"`
	CreatedAtISO     *string `json:"created_at_iso,omitempty"`
	UpdatedAtISO     *string `json:"updated_at_iso,omitempty"`
}

// TranslateCache 翻译缓存。
// Body 用 []byte 而不是 string：命中路径上直接交给 c.Data 写出，
// 避免 string→[]byte 的整份拷贝（string 无法与 []byte 共享底层数组）。
// 写库时由 database.UpsertCache 转回 TEXT，保证管理端 LIKE 查询仍然可用。
type TranslateCache struct {
	CacheKey    string `json:"cache_key"`
	Body        []byte `json:"-"`
	HeadersJSON string `json:"headers_json"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

// CacheIdentity 构建缓存键的归一化参数。
// 取值必须与真正转发给上游的参数一一对应：多一个字段降低命中率，
// 少一个字段就会把不同的上游请求折叠成同一条缓存（错误命中）。
// 编码方式见 service.BuildCacheKey —— 长度前缀 + 固定字段序，不依赖 JSON。
type CacheIdentity struct {
	Text               []string
	TargetLang         string
	SourceLang         string
	Formality          string
	GlossaryID         string
	Context            string
	ModelType          string
	SplitSentences     string
	PreserveFormatting string
	TagHandling        string
	OutlineDetection   string
	// 这三个参数同样会被转发给上游，因此必须进键，
	// 否则带不同指令的请求会共用同一条译文。
	CustomInstructions   string
	TagHandlingVersion   string
	ShowBilledCharacters string
	NonSplittingTags     []string
	SplittingTags        []string
	IgnoreTags           []string
}
