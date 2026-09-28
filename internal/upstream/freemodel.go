// freemodel.go 从模型目录推导「免费 / 低价模型」集合（保留积分判定用）。
//
// 用途：pool.reserve_credits（保留积分）在余额触底时只放行免费/低价模型。池层不认识
// 模型目录（架构约束，见 internal/pool/pick.go 的模型感知形态），故由本包把目录里的
// **生效倍率**折算成 id 集合，再经 pool.SetFreeModels 注入。
//
// 牌价 vs 生效价（与 modelpromo.go 同口径，勿混）：
//   - `Credits` 是**牌价**（转正后基准倍率，如 "x0.29"）；
//   - `PromoFactor` 是当前生效折扣（0 = 限时免费）。
//
// 保留积分要回答的是"这个模型**此刻**要花多少积分"，所以用**生效倍率**
// （有 promo 用 promo，否则用牌价）——限时免费的活动模型确实不花钱，不该被底线拦住；
// 反过来，牌价 x0.00 的模型即便无 promo 也是免费的。
//
// 只读、无副作用：输入是目录快照（server 侧的两个缓存出口），不触发任何上游请求。
package upstream

import (
	"strconv"
	"strings"
)

// FreeModelMultiplierThreshold 判定「免费 / 低价」的生效倍率阈值：**≤ 阈值即视为免费**。
//
// 取 0 的取舍：0 的语义明确（上游用 x0.00 表示免费，modelPromotions 的 factor=0 表示
// 限时免费），不会把付费模型误判成免费。代价是「很便宜但要花钱」的模型（如
// deepseek-v4.1-flash 的牌价 x0.03）不在此集合内——它们由池侧的**兜底白名单**
// （pool.defaultFreeModels，用户点名的 hy4-preview / hy3 / deepseek-v4.1-flash）覆盖。
// 若日后要把阈值放宽到某个小数（如 0.05 把 4.1-flash 的 x0.03 一并算进来），改这一个
// 常量即可：所有调用点（FreeModelSet）都读它。
const FreeModelMultiplierThreshold = 0.0

// ParseMultiplier 解析倍率原文为浮点。上游下发形态不统一，实测有：
// "x0.79" / "x1" / "x0.00"（牌价，前缀 x）与 "0x" / "0.50x"（折扣后价，后缀 x）。
// 两侧的 x 都剥掉再解析；无法解析（空串、纯 "x"、非数字）→ ok=false
// （**未知 ≠ 免费**：解析不出的条目绝不入免费集合）。
func ParseMultiplier(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "x")
	t = strings.TrimPrefix(t, "X")
	t = strings.TrimSuffix(t, "x")
	t = strings.TrimSuffix(t, "X")
	t = strings.TrimSpace(t)
	if t == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// EffectiveMultiplier 返回条目的**生效倍率**（当前真实计费口径）与是否可知：
//   - PromoFactor 非 nil（该模型有 machine-readable 折扣）→ 用它（0 = 限时免费）；
//   - 否则解析牌价 Credits（"x0.79" → 0.79）；
//   - 两者都无 → ok=false（未知，调用方按"非免费"处理）。
func (mi ModelInfo) EffectiveMultiplier() (float64, bool) {
	if mi.PromoFactor != nil {
		return *mi.PromoFactor, true
	}
	return ParseMultiplier(mi.Credits)
}

// IsFreeModel 报告目录里该模型是否「生效倍率 ≤ FreeModelMultiplierThreshold」。
//
// 逐条扫描而不先建集合：调用点在**选号热路径**上（每次选号一次），而集合版
// （FreeModelSet）会为每次调用分配一个 map——热路径上只查一个 id 时那是纯浪费。
// 目录条目数是个位数到数十，线性扫描与 map 查询同阶。
//
// 空 id / 目录为空 / 倍率未知 → false（缺失 ≠ 免费，与 /v1/stats 的倍率口径同纪律）。
func IsFreeModel(infos []ModelInfo, model string) bool {
	if model == "" {
		return false
	}
	for _, mi := range infos {
		if mi.ID != model {
			continue
		}
		f, ok := mi.EffectiveMultiplier()
		return ok && f <= FreeModelMultiplierThreshold
	}
	return false
}

// FreeModelSet 从目录条目推导「生效倍率 ≤ FreeModelMultiplierThreshold」的模型 id 集合
// （返回非 nil 空 map，调用方可直接 range/union）。供需要整体名单的调用方使用
// （热路径单点查询请用 IsFreeModel）。
//
// 空 id 跳过；倍率未知的条目不入集合（缺失 ≠ 免费，与 /v1/stats 的倍率口径同纪律）。
func FreeModelSet(infos []ModelInfo) map[string]bool {
	out := make(map[string]bool, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		f, ok := mi.EffectiveMultiplier()
		if !ok || f > FreeModelMultiplierThreshold {
			continue
		}
		out[mi.ID] = true
	}
	return out
}
