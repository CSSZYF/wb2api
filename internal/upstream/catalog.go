// catalog.go 模型目录的**单一合并入口**（2026-10-02 重写，替代原先分散在
// global_models.go / client.go 的两份合并实现）。
//
// ── 为什么必须收成一份 ──────────────────────────────────────────────────
// 重写前有两条各自演化的合并链路，服务同一份目录的两个投影：
//
//	① /v1/models 与路由：probeGlobalModels(企业端点) → mergeGlobalModelInfos
//	   → applyGlobalV3Catalog(能力 fill-only + 追加 v3 独有 id)
//	② 面板「模型与档位」：fetchModelsOnce(企业端点) → mergeModelCapabilities
//	   (能力覆盖 + promo，**没有追加步**)
//
// 两条链路对「能力字段怎么合」「独有 id 要不要追加」给出不同答案，症状就是
// **同一份上游目录在面板比在客户端少**：实测 global（2026-10-02，真实 dump 回放）
// 面板最终可见 **14 条**、/v1/models **28 条**，差集里 8 条是 v3 独有 id（面板路径缺
// 追加步，一条都进不来）。修好后两条路径都产出 27 条、面板可见 21 条
// （27 = 企业端点 18 + v3 追加 9 + pinned 兜底 1；hidden 5 + auto-chat 按名单隐藏）。
// 这不是某个过滤条件写错，而是"同一件事写两遍"的必然漂移——故本次不再各修一处，
// 而是删掉两份实现、只留本文件的 MergeCatalogOverlay。
//
// ── 合并语义（三条，逐条给出依据）────────────────────────────────────────
//
//  1. **能力字段：overlay 非零值覆盖**（base 已有真值也覆盖；overlay 的零值不动 base）。
//     依据：能力字段描述"这个模型能干什么"，而 /v3/config 是**官方客户端真正读的那份
//     配置**（客户端按它决定 reasoning 档位与窗口提示）。CN 侧两端点实测有实质差异
//     （deepseek-v4.1-pro 的 maxOutputTokens 企业端点 128000 / v3-IDE 393216；
//     glm-5.2 64000 / 131072），重写前**面板路径**一直是 v3 覆盖（既有用例
//     TestFetchModelsOverlaysV3ConfigCapabilities 锁着），故重写保持该方向——统一到
//     一条链路时不改变既有可观测行为。global 侧两端点在 17 个同名模型上能力字段
//     零差异，这个选择在 global 上不可观测。
//     （/v1/models 路径重写前是 fill-only，与面板不同——正是本重写要消灭的漂移之一。
//     选 override 而非 fill-only 的判据：v3 是客户端消费的配置源，且 override 只在
//     overlay **有值**时生效，不会把 base 的真值抹成零。）
//
//  2. **credits：base 优先，overlay 只补缺**（方向与能力字段相反，理由见下）。
//     依据（2026-10-02 实测，字节级核对）：
//     - 企业端点 hy4-preview / hy3 的 credits 是 **x0.00**，而同端点 modelPromotions
//     给这两条的 discountedCredits 是 "0x"、factor 0 —— 即**企业端点的 credits
//     已经是折后口径**（把限时优惠烤进了字段）；
//     - v3-CLI 给同名模型的 credits 是 **x0.29 / x0.00**（牌价，未打折）。
//     若让 v3 覆盖（重写前的行为），面板就把"已打折的实扣价 x0.00"换成"牌价
//     x0.29"——用户看到的正是这个（面板 x0.29 vs 真实免费）。
//     最强的单一证据在 CN：hy4-preview-f 的 v3-CLI 是 x0.00（试用实扣）、v3-IDE 是
//     x0.29（牌价）——同一模型同一时刻两个值，低的那个与生效中的 promo 一致。
//     故：**企业端点（计费目录）的 credits 权威；它没给这个 id 时才用 v3 的**
//     （v3 是 v3 独有模型唯一的价格来源，否则 5 条新模型全是未知倍率）。
//
//  3. **独有 id 追加**（overlay 有、base 没有，且不是非对话模型）。
//     依据：这些 id 在官方客户端里**实际可调用**（deepseek-v4.1-flash-sg 等 5 条
//     在 v3-CLI 的 data.models 里、hy4-preview-f 在 ModelTrialBanner 里），
//     不追加则客户端选不到。过滤只挡真正的非对话模型（见 nonChatModel）。
//
// promo（Promo*）与 credits 相反、与能力字段同向：overlay 有值就覆盖——v3 是含
// 时段/优先级判定的更权威 promo 源，企业端点补 v3 没给的那部分（该语义由
// TestGlobalPromoV3OverridesEnterprise / FillOnlyKeepsEnterpriseSource 锁定）。
package upstream

import (
	"sort"
	"strings"
)

// MergeCatalogOverlay 把 overlay（v3/config 能力表）并入 base（企业端点目录）。
//
// upstreamIDs 是**上游企业端点目录列过的全部 id**（未经 agents 白名单 / nonChatModel
// 过滤的原始集合），决定"独有 id"的判据——只有上游目录**压根没有**的 id 才追加。
// 传 nil 等价于"base 就是完整上游目录"（global 探测路径即如此：它不过 agents 白名单）。
//
// 为什么必须把这两个集合分开：面板路径的 base 是 `agents[name=cli].models` 白名单的
// 投影，而白名单**刻意排除**了一批企业端点列出的模型（实测 CN：deepseek-v4-flash /
// glm-4.6 / kimi-k2.5 等 11 条在 models[] 里但不在 cli 名单里）。若拿 base 当"已知"
// 判据，v3 里那些同名条目会被当成"v3 独有"重新追加回来——白名单的语义被悄悄推翻，
// CN 面板会凭空多出 11 个本不该列的模型。判据必须是**上游目录**，不是 base。
//
// 语义见文件头三条。返回新切片（base 的元素按需就地更新）。overlay 为空时原样返回
// base（调用方零成本）。
//
// 追加顺序：按 id 升序（overlay 是 map，迭代序随机——不排序会让面板列表每次刷新
// 抖动）。
func MergeCatalogOverlay(base []ModelInfo, overlay map[string]ModelInfo, upstreamIDs map[string]bool) []ModelInfo {
	if len(overlay) == 0 {
		return base
	}
	if upstreamIDs == nil {
		upstreamIDs = make(map[string]bool, len(base))
		for i := range base {
			upstreamIDs[base[i].ID] = true
		}
	}
	seen := make(map[string]bool, len(base))
	for i := range base {
		id := base[i].ID
		seen[id] = true
		ov, ok := overlay[id]
		if !ok {
			continue
		}
		overrideCapabilities(&base[i], ov)
		// credits：**base 优先**（企业端点=计费目录，且已是折后口径）。见文件头 §2。
		// overlay 只在 base 没给时补缺（v3 独有模型的价格来源）。
		if base[i].Credits == "" {
			base[i].Credits = ov.Credits
		}
		// promo：fill-only，overlay 有值才覆盖（v3 更权威，见文件头末段）。
		if ov.PromoFactor != nil {
			base[i].PromoFactor = ov.PromoFactor
		}
		if ov.PromoCredits != "" {
			base[i].PromoCredits = ov.PromoCredits
		}
		if ov.PromoLabel != "" {
			base[i].PromoLabel = ov.PromoLabel
		}
		if ov.PromoNote != "" {
			base[i].PromoNote = ov.PromoNote
		}
	}

	extra := make([]string, 0, len(overlay))
	for id, mi := range overlay {
		if id == "" || seen[id] || upstreamIDs[id] {
			continue // 上游目录已知（哪怕被白名单挡在 base 之外）→ 不是"独有 id"
		}
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}
		extra = append(extra, id)
	}
	if len(extra) == 0 {
		return base
	}
	sort.Strings(extra)
	for _, id := range extra {
		base = append(base, overlay[id])
	}
	return base
}

// ModelIDs 返回条目 id 集合（供 MergeCatalogOverlay 的 upstreamIDs 参数使用）。
func ModelIDs(infos []ModelInfo) map[string]bool {
	out := make(map[string]bool, len(infos))
	for i := range infos {
		if infos[i].ID != "" {
			out[infos[i].ID] = true
		}
	}
	return out
}

// StringSet 把字符串切片转成集合（供 MergeCatalogOverlay 的 upstreamIDs 参数使用）。
func StringSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// overrideCapabilities 用 src 的**非零值**覆盖 dst 的能力字段（src 零值不动 dst）。
//
// 与 fillCapabilities 的区别只在方向：本函数是"有值即覆盖"，那个是"只在 dst 缺时填"。
// 两个都要，因为两条合并链的语义本就不同（见 MergeCatalogOverlay 文件头 §1 与
// mergeV3CapabilityMaps）——把它们并成一个函数会让某一侧的语义被悄悄改掉。
func overrideCapabilities(dst *ModelInfo, src ModelInfo) {
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if src.MaxTokens > 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if src.MaxAllowedSize > 0 {
		dst.MaxAllowedSize = src.MaxAllowedSize
	}
	if len(src.Efforts) > 0 {
		dst.Efforts = src.Efforts
	}
	if src.DefaultEffort != "" {
		dst.DefaultEffort = src.DefaultEffort
	}
	if src.CanDisableThinking {
		dst.CanDisableThinking = true
	}
	if src.SupportsReasoning {
		dst.SupportsReasoning = true
	}
	if src.Description != "" {
		dst.Description = src.Description
	}
	if src.Vendor != "" {
		dst.Vendor = src.Vendor
	}
	if src.IsDefault {
		dst.IsDefault = src.IsDefault
	}
	if src.SupportsToolCall {
		dst.SupportsToolCall = src.SupportsToolCall
	}
	if src.SupportsImages {
		dst.SupportsImages = true
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if len(src.Tags) > 0 {
		dst.Tags = src.Tags
	}
}

// fillCapabilities 把 src 的**非零值**能力字段填进 dst（dst 已有真值不动）。
// 供双 UA 并集（mergeV3CapabilityMaps）使用：primary（IDE 路）字段更全，secondary
// （CLI 路）只补 primary 的零值——IDE 路可能是空壳（见 mergeV3CapabilityMaps 注释）。
//
// 不含 credits / promo：那两个有各自的优先级语义（见 MergeCatalogOverlay）。
func fillCapabilities(dst *ModelInfo, src ModelInfo) {
	if dst.ContextWindow == 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if dst.MaxTokens == 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if dst.MaxAllowedSize == 0 {
		dst.MaxAllowedSize = src.MaxAllowedSize
	}
	if len(dst.Efforts) == 0 {
		dst.Efforts = src.Efforts
	}
	if dst.DefaultEffort == "" {
		dst.DefaultEffort = src.DefaultEffort
	}
	if !dst.CanDisableThinking {
		dst.CanDisableThinking = src.CanDisableThinking
	}
	if !dst.SupportsReasoning {
		dst.SupportsReasoning = src.SupportsReasoning
	}
	if dst.Description == "" {
		dst.Description = src.Description
	}
	if dst.Vendor == "" {
		dst.Vendor = src.Vendor
	}
	if !dst.IsDefault {
		dst.IsDefault = src.IsDefault
	}
	if !dst.SupportsToolCall {
		dst.SupportsToolCall = src.SupportsToolCall
	}
	if !dst.SupportsImages {
		dst.SupportsImages = src.SupportsImages
	}
	if dst.Name == "" {
		dst.Name = src.Name
	}
	if len(dst.Tags) == 0 {
		dst.Tags = src.Tags
	}
}

// normalizeCredits 归一上游 credits 原文：剥掉尾部的 " credits" 词。//
// 实测形态（2026-10-02，企业端点）：同一个响应里两种写法混用——
// "x0.00" / "x0.79 credits"（CN 侧 23/31 条带尾巴，global 侧 5/18 条带尾巴）。
// 不归一的后果有三处：面板倍率列原样显示 "x0.79 credits"；/v1/stats 的 credits
// 字段同款；**保留积分的免费判定**（ParseMultiplier 解析失败 → 该模型既不免费也
// 不参与阈值比较，静默漏判）。
//
// 只剥这一个词（上游 schema 的固定后缀），不做通用清洗：倍率原文是展示字段，
// 改动面越小越好。空串原样返回（缺失 ≠ 免费，调用方整体省略该字段）。
func normalizeCredits(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, " "); i > 0 {
		if strings.EqualFold(strings.TrimSpace(s[i+1:]), "credits") {
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}
