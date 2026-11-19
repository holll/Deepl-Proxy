package service

import (
	"testing"
	"time"
)

func TestResolveQuotaPeriod(t *testing.T) {
	million := int64(1_000_000)
	halfMillion := int64(500_000)
	for _, tc := range []struct {
		name  string
		ex    string
		limit *int64
		want  string
	}{
		{"explicit yearly wins", "yearly", &halfMillion, QuotaPeriodYearly},
		{"explicit monthly wins", "monthly", &million, QuotaPeriodMonthly},
		{"auto million limit", "auto", &million, QuotaPeriodYearly},
		{"auto legacy limit", "", &halfMillion, QuotaPeriodMonthly},
		{"auto no snapshot", "auto", nil, QuotaPeriodMonthly},
		{"unknown value falls back", "weekly", &million, QuotaPeriodYearly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveQuotaPeriod(tc.ex, tc.limit); got != tc.want {
				t.Fatalf("ResolveQuotaPeriod(%q,%v) = %q, want %q", tc.ex, tc.limit, got, tc.want)
			}
		})
	}
}

func TestQuotaResetAt(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name   string
		now    time.Time
		period string
		want   time.Time
	}{
		{
			name:   "monthly mid month",
			now:    time.Date(2026, 11, 18, 23, 15, 0, 0, cst),
			period: QuotaPeriodMonthly,
			want:   time.Date(2026, 12, 1, 0, 0, 0, 0, cst),
		},
		{
			name:   "monthly december rolls to next year",
			now:    time.Date(2026, 12, 31, 23, 59, 0, 0, cst),
			period: QuotaPeriodMonthly,
			want:   time.Date(2027, 1, 1, 0, 0, 0, 0, cst),
		},
		{
			name:   "yearly mid year",
			now:    time.Date(2026, 11, 18, 23, 15, 0, 0, cst),
			period: QuotaPeriodYearly,
			want:   time.Date(2027, 1, 1, 0, 0, 0, 0, cst),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := QuotaResetAt(tc.now, tc.period)
			if !got.Equal(tc.want) {
				t.Fatalf("QuotaResetAt = %v, want %v", got, tc.want)
			}
			if got.Location().String() != cst.String() {
				t.Fatalf("reset should stay in +08:00, got %v", got.Location())
			}
		})
	}

	// 传入 UTC 时刻也必须按北京时间算"下月 1 日"，
	// 否则每月最后 8 小时会把到期时间算到下下个月。
	utcNow := time.Date(2026, 11, 30, 20, 0, 0, 0, time.UTC) // 北京时间 12-01 04:00
	got := QuotaResetAt(utcNow, QuotaPeriodMonthly)
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, cst)
	if !got.Equal(want) {
		t.Fatalf("QuotaResetAt(UTC) = %v, want %v", got, want)
	}
}
