package service

import (
	"testing"

	"deepl-proxy/models"
)

// 这些基准值不是"跑一遍代码把输出抄下来"，而是用
//
//	git show HEAD:service/cache.go   （旧实现：sha256(json.Marshal(identity))）
//	git show HEAD:models/models.go   （旧结构体与 json 标签）
//
// 独立编译出的旧代码对同一批输入算出来的。因此它们真正约束的是
// "新实现能否逐字节复刻历史键"，而不是"两个同样的结构体是否相等"。
// 任何字段顺序、json 标签、字段集合、前缀的改动都会让这些断言失败。
func TestBuildLegacyCacheKeyMatchesV1Golden(t *testing.T) {
	cases := []struct {
		name     string
		identity models.CacheIdentity
		want     string
	}{
		{
			name:     "single text minimal",
			identity: models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"},
			want:     "deepl:translate:a8cc92d1d16a6fca60342211bfc2dd645bf50d3b83d95bbd4486f4c99405e6ec",
		},
		{
			name: "two texts with tags and formality",
			identity: models.CacheIdentity{
				Text:             []string{"hello", "world"},
				TargetLang:       "ZH",
				SourceLang:       "EN",
				Formality:        "more",
				NonSplittingTags: []string{"b", "a"},
			},
			want: "deepl:translate:0234b5db5bfdaddae413bee07e876897156b17b4168ce3d88783b2bacfc3b39e",
		},
		{
			name: "every v1 field populated",
			identity: models.CacheIdentity{
				Text:               []string{"a b", "c"},
				TargetLang:         "EN-US",
				SourceLang:         "JA",
				Formality:          "less",
				GlossaryID:         "g1",
				Context:            "ctx",
				ModelType:          "prefer_quality_optimized",
				SplitSentences:     "1",
				PreserveFormatting: "1",
				TagHandling:        "html",
				OutlineDetection:   "0",
				SplittingTags:      []string{"p"},
				IgnoreTags:         []string{"span"},
			},
			want: "deepl:translate:06e479dbfd3112a921152e9965b34df0af82ae2f1c26ee07a42241e01465c593",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildLegacyCacheKey(&tc.identity)
			if got != tc.want {
				t.Fatalf("legacy key mismatch:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestBuildLegacyCacheKeyNilIdentity(t *testing.T) {
	if got := BuildLegacyCacheKey(nil); got != "" {
		t.Fatalf("nil identity should yield empty key, got %q", got)
	}
}

// v1 结构体里没有这三个字段，因此它们不参与旧键计算。
// 这是刻意的：旧行产生时这些参数根本没被转发过。
func TestBuildLegacyCacheKeyIgnoresV2OnlyFields(t *testing.T) {
	base := models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"}
	want := BuildLegacyCacheKey(&base)

	withExtras := base
	withExtras.CustomInstructions = "keep the tone casual"
	withExtras.TagHandlingVersion = "v2"
	withExtras.ShowBilledCharacters = "1"

	if got := BuildLegacyCacheKey(&withExtras); got != want {
		t.Fatalf("v2-only fields must not enter the v1 key:\n base %s\n with %s", want, got)
	}
}

// 旧键对新键一定是不同的值（前缀 + 编码方式都不同），
// 因此两者不可能互相命中，兼容读取不会造成新旧混淆。
func TestLegacyKeyNeverEqualsV2Key(t *testing.T) {
	identity := models.CacheIdentity{Text: []string{"hello"}, TargetLang: "ZH"}
	v2 := BuildCacheKey(&identity, "deepl")
	v1 := BuildLegacyCacheKey(&identity)

	if v1 == v2 {
		t.Fatalf("v1 and v2 keys must differ, both = %s", v1)
	}
	if len(v1) != len(legacyCacheKeyPrefix)+64 {
		t.Fatalf("unexpected legacy key shape: %q", v1)
	}
	if len(v2) != 64 {
		t.Fatalf("v2 key should be 64 hex chars, got %d: %q", len(v2), v2)
	}
}
