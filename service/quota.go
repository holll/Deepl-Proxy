package service

import (
	"strings"
	"time"
)

// 配额周期枚举。
const (
	QuotaPeriodAuto    = "auto"
	QuotaPeriodMonthly = "monthly"
	QuotaPeriodYearly  = "yearly"
)

// yearlyQuotaThreshold 是"年度配额"的判定阈值。
//
// DeepL 旧免费 key 的 character_limit 是 500000（每月），
// 新免费 key 是 1000000（每年）。用 100 万做阈值而不是精确等于，
// 是为了兼容以后可能出现的 120 万等新档位：只要上限达到百万级，
// 就按年重置处理，不会把年度 key 错误地每月重置一次。
const yearlyQuotaThreshold int64 = 1_000_000

// beijingOffset 固定 +08:00。中国不使用夏令时，用固定偏移比加载
// tzdata 更可靠（容器里常常没有 zoneinfo），也和既有月度重置逻辑一致。
const beijingOffset = 8 * time.Hour

// ResolveQuotaPeriod 决定一个 key 的配额重置周期。
//
// 显式配置优先（后台可手工把某个 key 固定为 monthly/yearly），
// 否则按 usage 接口返回的 character_limit 推断：
//   - 没有用量快照 → monthly（旧套餐更常见，且月重置不会让 key 停用更久）
//   - 上限 ≥ 100 万 → yearly
//   - 其余 → monthly
func ResolveQuotaPeriod(explicit string, characterLimit *int64) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case QuotaPeriodMonthly, QuotaPeriodYearly:
		return strings.ToLower(strings.TrimSpace(explicit))
	}
	if characterLimit != nil && *characterLimit >= yearlyQuotaThreshold {
		return QuotaPeriodYearly
	}
	return QuotaPeriodMonthly
}

// QuotaResetAt 返回配额重置时刻（北京时间零点）。
//
// 月度 = 下月 1 日 00:00，年度 = 次年 1 月 1 日 00:00。
// 熔断到期后 keyring 的 ReactivateExpiredKeys 会自动把 key 恢复为 active，
// 因此这里只需要给出"到期"这个绝对时刻。
func QuotaResetAt(now time.Time, period string) time.Time {
	bj := now.In(time.FixedZone("CST", int(beijingOffset/time.Second)))
	if strings.EqualFold(period, QuotaPeriodYearly) {
		return time.Date(bj.Year()+1, 1, 1, 0, 0, 0, 0, bj.Location())
	}
	return time.Date(bj.Year(), bj.Month()+1, 1, 0, 0, 0, 0, bj.Location())
}
