package upstream

import (
	"strings"
	"testing"
	"time"
)

// globalPromoBehavior 假上游：企业端点给裸目录（无 promo），/v3/config 带
// modelPromotions。**注意 v3/config 的模型集合刻意比企业端点少一个 id**，
// 以便同时覆盖「fill-only 能力补全」与「promo 覆盖」两条分支。
func globalPromoBehavior(path string) (int, string) {
	switch {
	case strings.HasSuffix(path, "/v2/enterprises/personal/models"):
		return 200, `{"code":0,"data":{"models":[
			{"id":"hy4-preview","name":"Hy4 Preview","maxInputTokens":200000,"maxOutputTokens":32768,"credits":"x0.29"},
			{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"},
			{"id":"hy3","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.00"}
		]}}`
	case strings.HasSuffix(path, "/v3/config"):
		return 200, `{"code":0,"data":{"models":[
			{"id":"hy4-preview","name":"Hy4 Preview","maxInputTokens":200000,"maxOutputTokens":32768,"credits":"x0.29"},
			{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"}
		],"modelPromotions":[
			{"enabled":true,"priority":100,"modelIds":["hy4-preview"],
			 "badge":{"label":"限时免费"},
			 "discount":{"discountedCredits":"0x","factor":0},
			 "hover":{"textZh":"限时免费至 09-30"},
			 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}},
			{"enabled":true,"priority":50,"modelIds":["glm-5.2"],
			 "badge":{"label":"错峰使用"},
			 "hover":{"textZh":"每日 23:00–07:50 优惠"},
			 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
		]}}`
	}
	return 500, "<html>500</html>"
}

// TestGlobalCatalogCarriesPromo 核心回归（任务 ②）：v3/config 返回带
// modelPromotions 时，global 目录（FetchGlobalModelInfos，服务 /v1/models 与
// /v1/stats 倍率快照）里的条目必须带上 promo_*。
//
// 实现前必红：applyGlobalV3Catalog 是 fill-only 逐字段搬运（ContextWindow/
// MaxTokens/Efforts/Name...），**完全没有 Promo 字段**——v3/config 明明已经把
// 优惠挂在返回的 ModelInfo 上（applyModelPromotions），合并这一步把它丢了。
func TestGlobalCatalogCarriesPromo(t *testing.T) {
	c, _ := globalTestClient(t, globalPromoBehavior)
	infos := c.FetchGlobalModelInfos(globalAuth())
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}

	hy, ok := byID["hy4-preview"]
	if !ok {
		t.Fatalf("缺 hy4-preview，实际 ids=%v", idsOf(infos))
	}
	if hy.PromoFactor == nil || *hy.PromoFactor != 0 {
		t.Errorf("hy4-preview promo_factor=%v want 0（限时免费）", hy.PromoFactor)
	}
	if hy.PromoCredits != "0x" || hy.PromoLabel != "限时免费" || hy.PromoNote != "限时免费至 09-30" {
		t.Errorf("hy4-preview promo 字段不全: credits=%q label=%q note=%q",
			hy.PromoCredits, hy.PromoLabel, hy.PromoNote)
	}
	// badge-only 条目（无 discount）：只挂标签，PromoFactor 保持 nil。
	glm := byID["glm-5.2"]
	if glm.PromoLabel != "错峰使用" || glm.PromoFactor != nil {
		t.Errorf("glm-5.2 label=%q factor=%v（badge-only 的 factor 必须 nil）", glm.PromoLabel, glm.PromoFactor)
	}
	// 未命中优惠的条目不得被凭空挂上 promo。
	if hy3 := byID["hy3"]; hy3.PromoLabel != "" || hy3.PromoFactor != nil {
		t.Errorf("hy3 未命中优惠却挂了 promo: %+v", hy3)
	}
}

// TestGlobalPromoNotPollutingCredits 口径分离（任务 ② 第 3 点）：
// Credits 是**牌价**（计费口径，落旁表 / 进 /v1/models 的 credits），Promo* 是
// **展示口径的生效价**。搬 promo 不得污染 Credits——两者混起来会让计费口径变成
// "限时免费"的假象（0x）。
func TestGlobalPromoNotPollutingCredits(t *testing.T) {
	c, _ := globalTestClient(t, globalPromoBehavior)
	_ = c.FetchGlobalModelInfos(globalAuth())

	snap := c.GlobalModelInfosSnapshot()
	if len(snap) == 0 {
		t.Fatal("快照为空：探测未落缓存")
	}
	byID := map[string]ModelInfo{}
	for _, mi := range snap {
		byID[mi.ID] = mi
	}
	// 快照是展示出口：牌价从旁表填回，promo 从缓存条目带出。
	hy := byID["hy4-preview"]
	if hy.Credits != "x0.29" {
		t.Errorf("快照 credits=%q want x0.29（牌价，旁表填回）", hy.Credits)
	}
	if hy.PromoCredits != "0x" {
		t.Errorf("快照 promo_credits=%q want 0x（生效价）", hy.PromoCredits)
	}
	if hy.Credits == hy.PromoCredits {
		t.Error("牌价与生效价必须分离（credits 是 x0.29、promo 是 0x），不得互相覆盖")
	}
	// 路由/列表口径（FetchGlobalModelInfos 返回值）恒无牌价——promo 的搬运不得
	// 顺手把 Credits 带进来（PLAN §3.D2）。
	infos := c.FetchGlobalModelInfos(globalAuth())
	for _, mi := range infos {
		if mi.Credits != "" {
			t.Errorf("FetchGlobalModelInfos 的 %s Credits=%q want 空（倍率不进路由口径）", mi.ID, mi.Credits)
		}
	}
}

// TestGlobalPromoFillOnlyKeepsEnterpriseSource 口径更正（2026-10-02）：
// 本函数对 promo 取 **fill-only**，而非此前的「无条件覆盖」。
//
// 为什么改：那条「覆盖」的前提是「promo 的唯一来源是 /v3/config，base 进本函数时
// promo 恒为零值」。该前提**已不成立**——企业端点现在也会下发 modelPromotions
// （global 的 /v2/enterprises/personal/models 实测 2 条），base 可能已带 promo。
// 若仍覆盖，v3 未给 promo 的模型会被**清零**，把企业端点刚挂上的生效价抹掉
// （实测症状：企业端点有 promo，面板却不显示）。
//
// 本用例锁定新语义：cap 无值 → base 的 promo 保留；能力字段照旧 fill-only。
//
// 时变语义（旧用例的关切）由**源头**保证：base 每轮由 probeGlobalModels 重建，
// parseGlobalModelInfos 现算 promo（过期条目不挂）→ 不存在"上一轮 promo 永久粘住"。
// 见 TestGlobalPromoFollowsCatalogRefresh（端到端两轮探测）。
func TestGlobalPromoFillOnlyKeepsEnterpriseSource(t *testing.T) {
	f := 0.5
	base := []ModelInfo{{
		ID: "glm-5.2", ContextWindow: 131072,
		PromoFactor: &f, PromoCredits: "0.50x", PromoLabel: "夜间折扣", PromoNote: "23:00–07:50",
	}}
	// 本轮 v3/config 只剩能力字段（v3 侧无该模型的 promo）。
	cap := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2", MaxTokens: 32768}}

	got := MergeCatalogOverlay(base, cap, nil)
	if got[0].PromoFactor == nil || got[0].PromoCredits != "0.50x" ||
		got[0].PromoLabel != "夜间折扣" || got[0].PromoNote != "23:00–07:50" {
		t.Errorf("v3 未给 promo 时不得抹掉 base 的来源（企业端点）: %+v", got[0])
	}
	// 能力字段仍是 fill-only：base 的窗口不被 cap 的零值抹掉，cap 的真值照常补进来。
	if got[0].ContextWindow != 131072 {
		t.Errorf("ContextWindow=%d want 131072（能力字段 fill-only，不得被零值覆盖）", got[0].ContextWindow)
	}
	if got[0].MaxTokens != 32768 {
		t.Errorf("MaxTokens=%d want 32768（cap 的真值应补入）", got[0].MaxTokens)
	}
}

// TestGlobalPromoV3OverridesEnterprise cap 有值时**覆盖** base：v3 是更权威的
// promo 源（含时段/优先级判定），同模型两边都给时 v3 胜出。
func TestGlobalPromoV3OverridesEnterprise(t *testing.T) {
	bf := 0.5
	base := []ModelInfo{{
		ID: "glm-5.2", PromoFactor: &bf, PromoCredits: "0.50x", PromoLabel: "企业端点标签",
	}}
	zf := 0.0
	cap := map[string]ModelInfo{"glm-5.2": {
		ID: "glm-5.2", PromoFactor: &zf, PromoCredits: "0x", PromoLabel: "限时免费",
	}}

	got := MergeCatalogOverlay(base, cap, nil)
	if got[0].PromoFactor == nil || *got[0].PromoFactor != 0 {
		t.Errorf("PromoFactor=%v want 0（v3 覆盖胜出）", got[0].PromoFactor)
	}
	if got[0].PromoLabel != "限时免费" || got[0].PromoCredits != "0x" {
		t.Errorf("label=%q credits=%q want 限时免费/0x", got[0].PromoLabel, got[0].PromoCredits)
	}
}

// TestGlobalPromoFollowsCatalogRefresh 端到端时变：同一个 Client 连续两轮探测，
// 第二轮的 promo 必须**替换**第一轮（而不是被缓存/粘住）——覆盖语义在真实调用链上的体现。
func TestGlobalPromoFollowsCatalogRefresh(t *testing.T) {
	round := 0
	c, _ := globalTestClient(t, func(path string) (int, string) {
		switch {
		case strings.HasSuffix(path, "/v2/enterprises/personal/models"):
			return 200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"}
			]}}`
		case strings.HasSuffix(path, "/v3/config"):
			if round == 0 {
				return 200, `{"code":0,"data":{"models":[
					{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"}
				],"modelPromotions":[
					{"enabled":true,"priority":100,"modelIds":["glm-5.2"],
					 "badge":{"label":"夜间折扣"},
					 "discount":{"discountedCredits":"0.50x","factor":0.5},
					 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
				]}}`
			}
			// 第二轮：折扣结束（modelPromotions 空）。
			return 200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"}
			]}}`
		}
		return 500, "<html>500</html>"
	})

	find := func() ModelInfo {
		for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
			if mi.ID == "glm-5.2" {
				return mi
			}
		}
		t.Fatal("目录缺 glm-5.2")
		return ModelInfo{}
	}

	if mi := find(); mi.PromoFactor == nil || *mi.PromoFactor != 0.5 || mi.PromoLabel != "夜间折扣" {
		t.Fatalf("第一轮应挂上夜间五折: %+v", mi)
	}
	// 强制缓存过期，触发第二轮探测。
	c.globalModels.Lock()
	c.globalModels.fetched = time.Now().Add(-2 * globalModelsTTL)
	c.globalModels.Unlock()
	round = 1

	if mi := find(); mi.PromoFactor != nil || mi.PromoLabel != "" {
		t.Errorf("折扣结束后不得残留上一轮 promo（时变字段必须随目录刷新清掉）: %+v", mi)
	}
}

// TestGlobalV3FallbackCarriesPromo v3 兜底路径（企业端点全挂 → v3CatalogInfos）：
// promo 必须一并带出（这条路径走 v3CatalogInfos 而非 applyGlobalV3Catalog，
// 但同样要核对——任务 ② 第 2 点）。
func TestGlobalV3FallbackCarriesPromo(t *testing.T) {
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v3/config") {
			return globalPromoBehavior(path)
		}
		return 500, "<html>500</html>"
	})
	infos := c.FetchGlobalModelInfos(globalAuth())
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	hy, ok := byID["hy4-preview"]
	if !ok {
		t.Fatalf("v3 兜底目录缺 hy4-preview，实际 ids=%v", idsOf(infos))
	}
	if hy.PromoFactor == nil || *hy.PromoFactor != 0 || hy.PromoCredits != "0x" || hy.PromoLabel != "限时免费" {
		t.Errorf("v3 兜底路径 promo 丢失: %+v", hy)
	}
	if hy.Credits != "" {
		t.Errorf("v3 兜底路径 Credits=%q want 空（倍率不进路由/列表口径）", hy.Credits)
	}
	// 快照出口同样能看到 promo（展示口径）。
	found := false
	for _, mi := range c.GlobalModelInfosSnapshot() {
		if mi.ID == "hy4-preview" {
			found = mi.PromoLabel == "限时免费" && mi.PromoFactor != nil && *mi.PromoFactor == 0
		}
	}
	if !found {
		t.Error("快照出口未带出 v3 兜底路径的 promo")
	}
}
