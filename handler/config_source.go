package handler

import "deepl-proxy/config"

// ConfigSource 提供"当前生效配置"。
//
// *config.Config 自己实现 Get（返回自身），因此测试里可以直接传静态配置；
// 运行时传 *service.Settings，每次请求都读到最新快照，
// 这样后台改完令牌/允许来源后无需重启即可生效。
type ConfigSource interface {
	Get() *config.Config
}
