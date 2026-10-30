package service

import (
	"encoding/hex"
	"testing"

	"deepl-proxy/models"
)

func baseIdentity() *models.CacheIdentity {
	return &models.CacheIdentity{
		Text:       []string{"hello"},
		TargetLang: "ZH",
		SourceLang: "EN",
	}
}

func TestProviderClass(t *testing.T) {
	cases := map[string]string{
		"deepl":           "deepl",
		"deeplx":          "deeplx",
		"DeepLX":          "deeplx",
		"  deeplx  ":      "deeplx",
		"DEEPL":           "deepl",
		"":                "deepl",
		"something-else":  "deepl",
		"deepl_pro":       "deepl",
		"deeplx-unstable": "deepl",
	}
	for in, want := range cases {
		if got := ProviderClass(in); got != want {
			t.Errorf("ProviderClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildCacheKeyIsDeterministicHex(t *testing.T) {
	a := BuildCacheKey(baseIdentity(), "deepl")
	b := BuildCacheKey(baseIdentity(), "deepl")
	if a != b {
		t.Fatalf("key is not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("key is not valid hex: %v", err)
	}
}

// 这是本次修复的核心：同一个内容在 deepl 与 deeplx 下必须是两个键。
// 否则 deeplx 返回的"单条 translation"会被回放给官方 deepl 的请求。
func TestBuildCacheKeySeparatesProviderClass(t *testing.T) {
	if BuildCacheKey(baseIdentity(), "deepl") == BuildCacheKey(baseIdentity(), "deeplx") {
		t.Fatal("deepl and deeplx share a cache key")
	}
}

// 长度前缀编码：("ab","c") 与 ("a","bc") 的拼接结果都是 "abc"，
// 但长度前缀让它们得到不同的键。
func TestBuildCacheKeyTextBoundaryIsUnambiguous(t *testing.T) {
	a := baseIdentity()
	a.Text = []string{"ab", "c"}
	b := baseIdentity()
	b.Text = []string{"a", "bc"}
	if BuildCacheKey(a, "deepl") == BuildCacheKey(b, "deepl") {
		t.Fatal("text list boundary is ambiguous: [ab c] and [a bc] collide")
	}

	// 数量不同也不能碰撞
	c := baseIdentity()
	c.Text = []string{"abc"}
	if BuildCacheKey(a, "deepl") == BuildCacheKey(c, "deepl") {
		t.Fatal("text count is not part of the key")
	}
}

// 相邻字段边界同样不能靠分隔符猜测。
func TestBuildCacheKeyFieldBoundaryIsUnambiguous(t *testing.T) {
	a := baseIdentity()
	a.GlossaryID = "x"
	a.Context = "y"

	b := baseIdentity()
	b.GlossaryID = "xy"
	b.Context = ""

	if BuildCacheKey(a, "deepl") == BuildCacheKey(b, "deepl") {
		t.Fatal("adjacent fields are ambiguous: glossary=x/context=y collides with glossary=xy/context=")
	}

	// 值为空与值形似"长度前缀"都要区分得开
	c := baseIdentity()
	c.Context = "1:a"
	if BuildCacheKey(a, "deepl") == BuildCacheKey(c, "deepl") {
		t.Fatal("field value that looks like a length prefix collides")
	}
}

// tag 列表是集合语义：顺序、重复、空串都不应影响键。
func TestBuildCacheKeyTagListsAreSetSemantics(t *testing.T) {
	ordered := baseIdentity()
	ordered.NonSplittingTags = []string{"a", "b"}

	reordered := baseIdentity()
	reordered.NonSplittingTags = []string{"b", "a"}

	dup := baseIdentity()
	dup.NonSplittingTags = []string{"a", "b", "a", "b"}

	withEmpty := baseIdentity()
	withEmpty.NonSplittingTags = []string{"", "a", "b", "  "}

	want := BuildCacheKey(ordered, "deepl")
	for name, got := range map[string]string{
		"reordered":  BuildCacheKey(reordered, "deepl"),
		"duplicated": BuildCacheKey(dup, "deepl"),
		"with empty": BuildCacheKey(withEmpty, "deepl"),
	} {
		if got != want {
			t.Errorf("%s tag list produced a different key", name)
		}
	}

	// 但内容真正不同必须换键
	different := baseIdentity()
	different.NonSplittingTags = []string{"a"}
	if BuildCacheKey(different, "deepl") == want {
		t.Error("different tag sets share a key")
	}
}

func TestBuildCacheKeyNormalizesLanguageCase(t *testing.T) {
	lower := baseIdentity()
	lower.TargetLang = "zh"
	upper := baseIdentity()
	upper.TargetLang = " ZH "

	if BuildCacheKey(lower, "deepl") != BuildCacheKey(upper, "deepl") {
		t.Fatal("language should be case/space insensitive")
	}
}

// 文本必须逐字节参与键：任何归一化都会把不同原文折叠成同一缓存。
func TestBuildCacheKeyDoesNotNormalizeText(t *testing.T) {
	a := baseIdentity()
	a.Text = []string{"hello world"}
	b := baseIdentity()
	b.Text = []string{"hello  world"} // 多一个空格
	c := baseIdentity()
	c.Text = []string{" hello world"} // 多一个前导空格

	ka := BuildCacheKey(a, "deepl")
	if ka == BuildCacheKey(b, "deepl") {
		t.Error("whitespace was normalized away")
	}
	if ka == BuildCacheKey(c, "deepl") {
		t.Error("leading whitespace was trimmed away")
	}
}

// 每个会被转发给上游的参数都必须改变缓存键。
func TestBuildCacheKeyCoversEveryForwardedParam(t *testing.T) {
	base := BuildCacheKey(baseIdentity(), "deepl")

	tweak := map[string]func(*models.CacheIdentity){
		"target_lang":          func(i *models.CacheIdentity) { i.TargetLang = "JA" },
		"source_lang":          func(i *models.CacheIdentity) { i.SourceLang = "DE" },
		"text":                 func(i *models.CacheIdentity) { i.Text = []string{"other"} },
		"formality":            func(i *models.CacheIdentity) { i.Formality = "more" },
		"glossary_id":          func(i *models.CacheIdentity) { i.GlossaryID = "g1" },
		"context":              func(i *models.CacheIdentity) { i.Context = "ctx" },
		"model_type":           func(i *models.CacheIdentity) { i.ModelType = "prefer_quality_optimized" },
		"split_sentences":      func(i *models.CacheIdentity) { i.SplitSentences = "0" },
		"preserve_formatting":  func(i *models.CacheIdentity) { i.PreserveFormatting = "1" },
		"tag_handling":         func(i *models.CacheIdentity) { i.TagHandling = "xml" },
		"outline_detection":    func(i *models.CacheIdentity) { i.OutlineDetection = "0" },
		"non_splitting_tags":   func(i *models.CacheIdentity) { i.NonSplittingTags = []string{"br"} },
		"splitting_tags":       func(i *models.CacheIdentity) { i.SplittingTags = []string{"p"} },
		"ignore_tags":          func(i *models.CacheIdentity) { i.IgnoreTags = []string{"code"} },
		"custom_instructions":  func(i *models.CacheIdentity) { i.CustomInstructions = "be formal" },
		"tag_handling_version": func(i *models.CacheIdentity) { i.TagHandlingVersion = "v2" },
		"show_billed_characters": func(i *models.CacheIdentity) {
			i.ShowBilledCharacters = "1"
		},
	}

	for name, apply := range tweak {
		id := baseIdentity()
		apply(id)
		if got := BuildCacheKey(id, "deepl"); got == base {
			t.Errorf("param %q is forwarded upstream but missing from the cache key", name)
		}
	}
}

func TestCanServe(t *testing.T) {
	params := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}

	if !CanServe("deepl", 3, params("formality", "more")) {
		t.Error("deepl should serve any request")
	}
	if !CanServe("", 5, nil) {
		t.Error("empty provider defaults to deepl")
	}

	if CanServe("deeplx", 2, nil) {
		t.Error("deeplx must refuse multi-text requests")
	}
	if CanServe("deeplx", 1, params("formality", "more")) {
		t.Error("deeplx must refuse formality")
	}
	if CanServe("deeplx", 1, params("custom_instructions", "be formal")) {
		t.Error("deeplx must refuse custom_instructions")
	}
	// 空值等于没传，仍可服务
	if !CanServe("deeplx", 1, params("formality", "")) {
		t.Error("empty param should not disqualify deeplx")
	}
	if !CanServe("deeplx", 1, nil) {
		t.Error("deeplx should serve a plain single-text request")
	}
}
