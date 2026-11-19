package provider

import "strings"

// 已知站点的固定 endpoint。
//
// 这三个地址是 DeepL 生态里最常见的入口：官方付费站、官方免费站、
// 以及第三方 deepl-pro 兼容站。把它们集中在这里，既让"按 key 后缀
// 自动识别"有唯一事实来源，也方便后台展示时反查站点名称。
const (
	EndpointDeepLOfficial = "https://api.deepl.com"
	EndpointDeepLFree     = "https://api-free.deepl.com"
	EndpointDeepLPro      = "https://api.deepl-pro.com"
)

// 已知的 auth key 后缀。
//
// DeepL 官方免费 key 以 ":fx" 结尾，这是官方文档里的固定格式；
// 第三方 deepl-pro 站点的 key 以 ":dp" 结尾，用于和官方 key 区分。
// 官方付费 key 没有后缀。
const (
	AuthKeySuffixFree     = ":fx"
	AuthKeySuffixDeepLPro = ":dp"

	authKeySuffixFreeLower = ":fx"
	authKeySuffixProLower  = ":dp"
)

// AuthKeySuffix 返回 key 的后缀标识（":fx" / ":dp" / 空字符串）。
//
// 只认完整后缀，不做模糊匹配：key 中间出现的 "dp" 不能当成站点标识，
// 否则一个普通 key 会被误判到第三方站点，请求全部打错上游。
func AuthKeySuffix(authKey string) string {
	k := strings.TrimSpace(authKey)
	lower := strings.ToLower(k)
	switch {
	case strings.HasSuffix(lower, authKeySuffixFreeLower):
		return AuthKeySuffixFree
	case strings.HasSuffix(lower, authKeySuffixProLower):
		return AuthKeySuffixDeepLPro
	default:
		return ""
	}
}

// EndpointFromAuthKey 根据 auth key 后缀推断上游站点。
//
// ok=false 表示 key 为空或没有可识别后缀，调用方应保留用户填写的
// endpoint（自动识别只做"没填/选自动"时的兜底，不覆盖显式配置）。
func EndpointFromAuthKey(authKey string) (endpoint, providerName string, ok bool) {
	if strings.TrimSpace(authKey) == "" {
		return "", "", false
	}
	switch AuthKeySuffix(authKey) {
	case AuthKeySuffixDeepLPro:
		return EndpointDeepLPro, "deepl", true
	case AuthKeySuffixFree:
		return EndpointDeepLFree, "deepl", true
	default:
		return EndpointDeepLOfficial, "deepl", true
	}
}

// IsAutoEndpoint 判断前端传来的 endpoint 是否表示"自动识别"。
//
// 兼容三种写法：空串、"auto"、"detect"。管理端表单用 auto，
// 手工调用 API 的旧脚本通常传空串。
func IsAutoEndpoint(endpoint string) bool {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "", "auto", "detect":
		return true
	default:
		return false
	}
}
