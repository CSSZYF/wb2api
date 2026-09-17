package upstream

import (
	"testing"
	"time"
)

// TestGrowthYesterdayDateCST 同区时区（即时区为 CST）下的基本口径。
func TestGrowthYesterdayDateCST(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, cst)
	if got := GrowthYesterdayDate(now); got != "2026-09-15" {
		t.Errorf("GrowthYesterdayDate=%q want 2026-09-15", got)
	}
	// CST 日界前后：00:00 与 23:59 都是同一天的「昨日」。
	if got := GrowthYesterdayDate(time.Date(2026, 9, 16, 0, 0, 0, 0, cst)); got != "2026-09-15" {
		t.Errorf("CST 00:00 → %q want 2026-09-15", got)
	}
	if got := GrowthYesterdayDate(time.Date(2026, 9, 16, 23, 59, 59, 0, cst)); got != "2026-09-15" {
		t.Errorf("CST 23:59 → %q want 2026-09-15", got)
	}
}

// TestGrowthYesterdayDateDSTZone 容器时区含夏令时（如 America/New_York）时，
// 昨日 CST 自然日必须仍按 CST 日界计算。
//
// 缺陷：原实现先 AddDate(0,0,-1)（按**入参 Time 的时区**做日历日减法）再转 CST，
// 而注释声称与 scheduler.travelDay 同口径（travelDay 是「先转 CST 再取日」）。
// 在夏令时切换日，AddDate 保持墙钟时刻跨 23h/25h 的一天会把瞬时点挪 1 小时，
// CST 日期随之错位一天——补签（makeupYesterday）会漏掉真实断档或补错日期。
//
// 春令时切换日（ET 2026-03-08）：01:00 CST 落在这个 ±1h 带内。
// 修复前 buggy=2026-03-08（把「已过去的那天」算成今天）→ RED。
func TestGrowthYesterdayDateDSTZone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata 不可用: %v", err)
	}
	// 2026-03-08T15:30:00Z = ET 10:30（切换后）= CST 当日 23:30。
	now := time.Date(2026, 3, 8, 15, 30, 0, 0, time.UTC).In(loc)
	if got := GrowthYesterdayDate(now); got != "2026-03-07" {
		t.Errorf("GrowthYesterdayDate=%q want 2026-03-07（昨日 CST；DST 切换日不得错位）", got)
	}
	// 秋令时切换日（ET 2026-11-01，25h 天）：同样不得错位。
	// 2026-11-02T04:30:00Z = ET 2026-11-01 23:30（EST，切换后）= CST 2026-11-02 12:30。
	now2 := time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC).In(loc)
	if got := GrowthYesterdayDate(now2); got != "2026-11-01" {
		t.Errorf("GrowthYesterdayDate=%q want 2026-11-01（秋令时切换日不得错位）", got)
	}
}

// TestGrowthYesterdayDateEqualsTravelDayMinusOne 与 scheduler.travelDay 同口径：
// 「昨日 CST」= 「CST 视角今天 - 1 天」，两处判据（heatmap 昨日 / 补签 target_date）
// 必须落在同一自然日。
func TestGrowthYesterdayDateEqualsTravelDayMinusOne(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	for _, utcHour := range []int{0, 5, 12, 15, 16, 23} {
		now := time.Date(2026, 9, 11, utcHour, 0, 0, 0, time.UTC)
		today := now.In(cst).Format("2006-01-02")
		want := now.In(cst).AddDate(0, 0, -1).Format("2006-01-02")
		if got := GrowthYesterdayDate(now); got != want {
			t.Errorf("utcHour=%d: yesterday=%s want %s (today=%s)", utcHour, got, want, today)
		}
	}
}
