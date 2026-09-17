// growthdate.go 成长域「自然日」口径的统一入口：补签（heatmap 昨日判据 / 补签卡
// target_date）与连登（scheduler.makeupYesterday）必须用同一个 CST 昨日，
// 否则两处判据错位（查的是昨日、补的是今日）。
package upstream

import "time"

// cstShanghai 成长域自然日的固定时区（Asia/Shanghai，UTC+8）。
// 中国无夏令时，固定 +8 即可，不依赖容器 tzdata。
var cstShanghai = time.FixedZone("CST", 8*60*60)

// GrowthYesterdayDate 昨日的 CST 自然日（2006-01-02）。补签判据固定盯昨日：
// 连登断档只可能发生在「上一个自然日」（今日尚未结算）。
//
// 必须先 In(cstShanghai) 再 AddDate：AddDate 按**入参 Time 所在时区**做日历日减法，
// 容器时区含夏令时时，切换日的 23h/25h 会把瞬时点挪 1 小时、CST 日期错位一天
// （补签漏掉真实断档或补错日期）。先归一到 CST 再减日即与 scheduler.travelDay 同口径
// （CST 无夏令时，减一日恒为前一个 CST 自然日）。
//
// 参数 now 显式传入（不内部调 time.Now()）：两处调用方（upstream.HeatmapYesterdayMissed、
// scheduler.makeupYesterday）各自取一次时间即可，测试也能构造夏令时切换日的瞬时点。
func GrowthYesterdayDate(now time.Time) string {
	return now.In(cstShanghai).AddDate(0, 0, -1).Format("2006-01-02")
}
