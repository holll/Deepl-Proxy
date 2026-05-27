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

// TranslateCache 翻译缓存
type TranslateCache struct {
	CacheKey    string `json:"cache_key"`
	Body        string `json:"body"`
	HeadersJSON string `json:"headers_json"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

// CacheIdentity 构建缓存键的归一化参数
type CacheIdentity struct {
	Text               []string `json:"text"`
	TargetLang         string   `json:"target_lang"`
	SourceLang         string   `json:"source_lang,omitempty"`
	Formality          string   `json:"formality,omitempty"`
	GlossaryID         string   `json:"glossary_id,omitempty"`
	Context            string   `json:"context,omitempty"`
	ModelType          string   `json:"model_type,omitempty"`
	SplitSentences     string   `json:"split_sentences,omitempty"`
	PreserveFormatting string   `json:"preserve_formatting,omitempty"`
	TagHandling        string   `json:"tag_handling,omitempty"`
	OutlineDetection   string   `json:"outline_detection,omitempty"`
	NonSplittingTags   []string `json:"non_splitting_tags,omitempty"`
	SplittingTags      []string `json:"splitting_tags,omitempty"`
	IgnoreTags         []string `json:"ignore_tags,omitempty"`
}
