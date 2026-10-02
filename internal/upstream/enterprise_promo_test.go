package upstream

import (
	"strings"
	"testing"
	"time"
)

// enterprisePromoBehavior 假上游：**企业端点自己下发 modelPromotions**，
// /v3/config 返回空数组。复现 2026-10-02 实测到的 global 真实形态
// （global 的 /v2/enterprises/personal/models 是唯一会下发 promo 的端点）。
//
// 这条路径此前被结构性丢弃：fetchModelsOnce 的匿名信封结构缺 ModelPromotions
// 字段 → json.Unmarshal 直接无视 → 表现为「上游续期了限时免费，面板却一直不显示」。
func enterprisePromoBehavior(path string) (int, string) {
	switch {
	case strings.HasSuffix(path, "/v2/enterprises/personal/models"):
		return 200, `{"code":0,"data":{
			"models":[
				{"id":"hy3","name":"Hy3","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.00"},
				{"id":"hy4-preview","name":"Hy4 Preview","maxInputTokens":200000,"maxOutputTokens":32768,"credits":"x0.29"},
				{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.79"}
			],
			"modelPromotions":[
				{"enabled":true,"priority":100,"modelIds":["hy3"],
				 "badge":{"label":"限时免费"},
				 "discount":{"discountedCredits":"0x","factor":0},
				 "hover":{"textZh":"限时免费至 10-31"},
				 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
			]}}`
	case strings.HasSuffix(path, "/v3/config"):
		// v3 不带 promo（空数组）—— 与 global 实测一致。
		return 200, `{"code":0,"data":{"models":[
			{"id":"hy3","name":"Hy3","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.00"}
		],"modelPromotions":[]}}`
	}
	return 500, "<html>500</html>"
}

// TestEnterpriseEndpointPromoReachesCatalog 核心回归：**企业端点下发的 promo
// 必须能进目录**（global 实测形态）。
//
// 此前 fetchModelsOnce 的信封结构缺 ModelPromotions 字段，上游下发的 promo
// 被 json.Unmarshal 结构性丢弃 —— 且旧注释还断言「企业端点不下发」，
// 让这个问题看起来像「上游没给」。
func TestEnterpriseEndpointPromoReachesCatalog(t *testing.T) {
	c, srv := globalTestClient(t, enterprisePromoBehavior)
	_ = srv

	infos := c.FetchGlobalModelInfos(globalAuth())
	byID := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		byID[mi.ID] = mi
	}

	hy3, ok := byID["hy3"]
	if !ok {
		t.Fatalf("目录缺 hy3: %v", idsOf(infos))
	}
	if hy3.PromoFactor == nil {
		t.Fatalf("企业端点的 promo 未进目录：hy3.PromoFactor=nil（" +
			"信封结构缺 ModelPromotions 字段时，json.Unmarshal 会静默丢弃）")
	}
	if *hy3.PromoFactor != 0 {
		t.Errorf("hy3.PromoFactor=%v want 0（限时免费）", *hy3.PromoFactor)
	}
	if hy3.PromoCredits != "0x" {
		t.Errorf("hy3.PromoCredits=%q want \"0x\"", hy3.PromoCredits)
	}
	if hy3.PromoLabel != "限时免费" {
		t.Errorf("hy3.PromoLabel=%q want \"限时免费\"", hy3.PromoLabel)
	}
	if hy3.PromoNote != "限时免费至 10-31" {
		t.Errorf("hy3.PromoNote=%q want \"限时免费至 10-31\"", hy3.PromoNote)
	}

	// 未命中 promo 的模型：字段保持零值（不编造）。
	if glm, ok := byID["glm-5.2"]; ok && glm.PromoFactor != nil {
		t.Errorf("glm-5.2 未命中 promo，PromoFactor 应为 nil，got %v", *glm.PromoFactor)
	}
}

// TestEnterprisePromoDoesNotPolluteCredits 口径分离：promo 是**展示口径**，
// 不得污染 Credits（计费口径的牌价）。
func TestEnterprisePromoDoesNotPolluteCredits(t *testing.T) {
	c, _ := globalTestClient(t, enterprisePromoBehavior)
	infos := c.FetchGlobalModelInfos(globalAuth())
	for _, mi := range infos {
		if mi.ID != "hy3" {
			continue
		}
		// 牌价仍是企业端点给的 x0.00（不是 promo 的 0x）。
		if mi.Credits == "0x" {
			t.Errorf("Credits 被 promo 污染：%q（牌价与生效价是两个口径）", mi.Credits)
		}
		return
	}
	t.Fatal("目录缺 hy3")
}

// TestV3PromoWinsOverEnterprisePromo 优先级：同模型两边都给 promo 时，
// **v3 覆盖胜出**（企业端点的 promo 先挂基底，v3 再覆盖）。
func TestV3PromoWinsOverEnterprisePromo(t *testing.T) {
	behavior := func(path string) (int, string) {
		switch {
		case strings.HasSuffix(path, "/v2/enterprises/personal/models"):
			return 200, `{"code":0,"data":{
				"models":[{"id":"hy3","name":"Hy3","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.00"}],
				"modelPromotions":[
					{"enabled":true,"priority":10,"modelIds":["hy3"],
					 "badge":{"label":"企业端点标签"},
					 "discount":{"discountedCredits":"0.5x","factor":0.5},
					 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
				]}}`
		case strings.HasSuffix(path, "/v3/config"):
			return 200, `{"code":0,"data":{"models":[
				{"id":"hy3","name":"Hy3","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.00"}
			],"modelPromotions":[
				{"enabled":true,"priority":100,"modelIds":["hy3"],
				 "badge":{"label":"v3 标签"},
				 "discount":{"discountedCredits":"0x","factor":0},
				 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
			]}}`
		}
		return 500, "<html>500</html>"
	}
	c, _ := globalTestClient(t, behavior)
	infos := c.FetchGlobalModelInfos(globalAuth())
	for _, mi := range infos {
		if mi.ID != "hy3" {
			continue
		}
		if mi.PromoLabel != "v3 标签" {
			t.Errorf("PromoLabel=%q want \"v3 标签\"（v3 是更高优先级的 promo 来源）", mi.PromoLabel)
		}
		if mi.PromoFactor == nil || *mi.PromoFactor != 0 {
			t.Errorf("PromoFactor=%v want 0（v3 覆盖胜出）", mi.PromoFactor)
		}
		return
	}
	t.Fatal("目录缺 hy3")
}

// TestEnterprisePromoRespectsActiveWindow 过期 promo 不挂（沿用既有 promoActive
// 判定，不因换来源而放宽）。
func TestEnterprisePromoRespectsActiveWindow(t *testing.T) {
	expired := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	behavior := func(path string) (int, string) {
		switch {
		case strings.HasSuffix(path, "/v2/enterprises/personal/models"):
			return 200, `{"code":0,"data":{
				"models":[{"id":"hy3","name":"Hy3","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.00"}],
				"modelPromotions":[
					{"enabled":true,"priority":100,"modelIds":["hy3"],
					 "badge":{"label":"限时免费"},
					 "discount":{"discountedCredits":"0x","factor":0},
					 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],
					             "timezone":"Asia/Shanghai","validUntil":"` + expired + `"}}
				]}}`
		case strings.HasSuffix(path, "/v3/config"):
			return 200, `{"code":0,"data":{"models":[],"modelPromotions":[]}}`
		}
		return 500, "<html>500</html>"
	}
	c, _ := globalTestClient(t, behavior)
	infos := c.FetchGlobalModelInfos(globalAuth())
	for _, mi := range infos {
		if mi.ID == "hy3" && mi.PromoFactor != nil {
			t.Errorf("已过期 promo（validUntil=%s）不得挂到目录：PromoFactor=%v", expired, *mi.PromoFactor)
		}
	}
}
