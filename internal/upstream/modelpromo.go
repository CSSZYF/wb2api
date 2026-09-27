// modelpromo.go 限时优惠（/v3/config 的 data.modelPromotions）解析与生效判定。
//
// 上游口径（吸收上游 2b0eedd，2026-09-23 实测 7 条）：`credits` 是**牌价**（转正后
// 基准倍率），modelPromotions 给当前生效的折扣——WorkBuddy 客户端展示的是**生效价**。
// 面板倍率列据此显示「生效价 + 标签 + 划线牌价（悬停看时段）」。
//
// 拆到独立文件的理由：判定逻辑（时区/跨午夜窗口/日期范围/优先级）是纯函数，可脱离
// HTTP 单测；与 client.go 的目录解析分开后，改动面清晰。
package upstream

import (
	"strconv"
	"strings"
	"time"
)

// v3ModelPromotion /v3/config data.modelPromotions 单条优惠定义。
// discount 只在部分条目上存在：有 factor 的可算生效价；「错峰使用」类只有时段
// 文案（factor 藏在 hover 文本里，无机器可读值），仅透出标签与说明。
type v3ModelPromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"` // "23:00"
			End   string `json:"end"`   // "7:50"（可跨午夜）
		} `json:"daily"`
		Timezone   string `json:"timezone"`  // 实测恒 Asia/Shanghai
		ValidFrom  string `json:"validFrom"` // RFC3339，可缺省
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// promoZone 优惠时区：上游恒 Asia/Shanghai（UTC+8 无夏令时），用 FixedZone 免依赖
// 系统 tzdata（Windows 无 IANA 库时 LoadLocation 会失败）。
var promoZone = time.FixedZone("CST", 8*3600)

// promoClock 解析 "HH:MM" 为当日分钟数；坏值返回 (-1, false)。
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

// promoActive 评估优惠在 now 是否生效：enabled + validFrom/validUntil 内 + 落在
// 任一 daily 窗口（支持跨午夜，如 23:00→7:50）。schedule 为 nil 视为全天生效。
func promoActive(p *v3ModelPromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if sc := p.Schedule; sc != nil {
		if sc.ValidFrom != "" {
			from, err := time.Parse(time.RFC3339, sc.ValidFrom)
			if err == nil && now.Before(from) {
				return false
			}
		}
		if sc.ValidUntil != "" {
			until, err := time.Parse(time.RFC3339, sc.ValidUntil)
			if err == nil && !now.Before(until) {
				return false
			}
		}
		if len(sc.Daily) > 0 {
			cur := now.Hour()*60 + now.Minute()
			inWindow := false
			for _, w := range sc.Daily {
				st, ok1 := promoClock(w.Start)
				ed, ok2 := promoClock(w.End)
				if !ok1 || !ok2 {
					continue
				}
				if st <= ed {
					if cur >= st && cur < ed {
						inWindow = true
						break
					}
				} else if cur >= st || cur < ed { // 跨午夜（23:00→7:50）
					inWindow = true
					break
				}
			}
			if !inWindow {
				return false
			}
		}
	}
	return true
}

// applyModelPromotions 把当前生效的优惠挂到目录条目：同模型多条命中取 priority
// 最高（实测 glm-5.2 白天 badge-only(50) 与夜间五折(100) 靠 priority+daily 双轨
// 切换）。无 discount 对象的条目也挂标签/说明（错峰类），PromoFactor 留 nil。
func applyModelPromotions(out map[string]ModelInfo, promos []v3ModelPromotion) {
	if len(promos) == 0 || len(out) == 0 {
		return
	}
	now := time.Now().In(promoZone)
	type cand struct {
		prio int
		p    *v3ModelPromotion
	}
	best := map[string]cand{}
	for i := range promos {
		p := &promos[i]
		if !promoActive(p, now) {
			continue
		}
		for _, id := range p.ModelIDs {
			if _, ok := out[id]; !ok {
				continue // 目录外模型（如同名 global 变体）不挂
			}
			if b, seen := best[id]; !seen || p.Priority > b.prio {
				best[id] = cand{prio: p.Priority, p: p}
			}
		}
	}
	for id, c := range best {
		mi := out[id]
		if c.p.Badge != nil {
			mi.PromoLabel = c.p.Badge.Label
		}
		if c.p.Hover != nil {
			mi.PromoNote = c.p.Hover.TextZh
		}
		if c.p.Discount != nil {
			f := c.p.Discount.Factor
			mi.PromoFactor = &f
			mi.PromoCredits = c.p.Discount.DiscountedCredits
		}
		out[id] = mi
	}
}
