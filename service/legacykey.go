package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"deepl-proxy/models"
)

//
// ========================
// v1 缓存键（兼容读取）
// ========================
//

// 背景：v2 键改了三处（加入 provider 维度、长度前缀编码、去掉前缀字符串），
// 线上已有一批 v1 行，若直接切换会导致这些行全部失联 —— 缓存瞬间归零，
// 所有请求重新打上游，既烧钱又放大上游压力。
//
// 因此保留一份 v1 键的只读复刻：新键未命中时按 v1 键再查一次，
// 命中就把它迁移写入新键。v1 行不做续期，按自己的 expires_at 自然过期，
// 不需要删表、也不会造成长期双份存储。

// legacyCacheKeyPrefix 是 v1 键的固定前缀，历史实现里硬编码为 "deepl:translate:"。
// 它不承载语义（provider 并未体现在其中），只是为了与旧行逐字节对齐。
const legacyCacheKeyPrefix = "deepl:translate:"

// legacyCacheIdentity 逐字节复刻 v1 的键输入结构。
//
// 字段顺序、json 标签、字段集合三者都必须与旧版本完全一致：
// v1 键 = "deepl:translate:" + hex(sha256(json.Marshal(CacheIdentity)))。
// json.Marshal 按结构体声明顺序输出、omitempty 决定字段是否出现，
// 所以哪怕只是给某个字段改个标签名或挪个位置，历史行也会整体失联。
//
// 不要"顺手现代化"这个结构体（比如给字段加 omitzero、改成 any、
// 或复用 models.CacheIdentity）——它的唯一职责就是复刻历史字节。
type legacyCacheIdentity struct {
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

// BuildLegacyCacheKey 计算 v1 键，仅用于兼容读取。
//
// 注意它只取 v1 存在的 14 个字段：custom_instructions /
// tag_handling_version / show_billed_characters 是 v2 才纳入键的，
// 旧行里没有它们的信息，因此不能参与 v1 键计算。
//
// 返回值在 identity 为空时是 ""，调用方应把空串视为"没有旧键"。
func BuildLegacyCacheKey(identity *models.CacheIdentity) string {
	if identity == nil {
		return ""
	}

	legacy := legacyCacheIdentity{
		Text:               identity.Text,
		TargetLang:         identity.TargetLang,
		SourceLang:         identity.SourceLang,
		Formality:          identity.Formality,
		GlossaryID:         identity.GlossaryID,
		Context:            identity.Context,
		ModelType:          identity.ModelType,
		SplitSentences:     identity.SplitSentences,
		PreserveFormatting: identity.PreserveFormatting,
		TagHandling:        identity.TagHandling,
		OutlineDetection:   identity.OutlineDetection,
		NonSplittingTags:   identity.NonSplittingTags,
		SplittingTags:      identity.SplittingTags,
		IgnoreTags:         identity.IgnoreTags,
	}

	payload, err := json.Marshal(&legacy)
	if err != nil {
		// []string 与 string 不可能序列化失败，这里只是防御。
		return ""
	}

	sum := sha256.Sum256(payload)
	return legacyCacheKeyPrefix + hex.EncodeToString(sum[:])
}
