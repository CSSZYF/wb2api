package upstream

// catalog_rewrite_test.go 模型目录合并链路的重写回归（任务 A/B）。
//
// 实测依据（2026-10-02，真实 global 账号直连上游，原始 dump 存于
// D:/wb2tmp/upstream/*.json，字节级核对过）：
//
//   - /v2/enterprises/personal/models（计费目录）：18 条，**完全不含 deepseek 系列**
//     （bytes.count(b"deepseek")==0）；hy4-preview/hy3 的 credits 是 x0.00（折扣后口径，
//     与 modelPromotions 的 discountedCredits "0x" 一致）；
//   - /v3/config（CLI UA）：22 条，含 deepseek-v4.1-flash(x0.00)、
//     deepseek-v4.1-flash-sg(x0.03)、glm-5.3-flash(x0.06)、kimi-k2.8-preview(x0.77)、
//     gpt-6-astra(x6.67) —— 这 5 条**企业端点一条都没有**；
//   - /v3/config（IDE UA）：13 条，含 auto-chat / enhance-1.0 / o4-mini /
//     hunyuan-image-alpha(text-to-image) / nes-* / completion-1.0 / codewise-jump，
//     以及 ModelTrialBanner 的 hy4-preview-f（**data.models 里没有它**）。
//
// 三条结构性缺陷（本文件逐条锁死）：
//
//  1. 面板路径（FetchModelsDiag → mergeModelCapabilities）**没有追加步**——它只对基底
//     已有的 id 做 fill-only 覆盖，v3 独有 id 完全不进结果。于是 CLI 路独有的 5 条
//     （deepseek-v4.1-flash-sg / glm-5.3-flash / kimi-k2.8-preview / gpt-6-astra /
//     hy4-preview-f）在面板上**一条都看不到**，而 /v1/models 路径（applyGlobalV3Catalog）
//     看得到——同一份目录两种投影，面板比客户端少 5 个可调用模型。
//  2. 双 UA 并集（mergeV3CapabilityMaps）取「primary(IDE) 优先，只补 secondary 没有的
//     id」——而 hy4-preview-f 在 IDE 路是**空壳**（IDE 的 data.models 里没有
//     hy4-preview，试用横幅继承不到任何能力），CLI 路才继承到 hy4-preview 的能力。
//     primary 优先把空壳留下来，metadata 全零。
//  3. credits 的覆盖方向反了：mergeModelCapabilities 用 v3 覆盖企业端点，于是
//     hy4-preview 显示 v3 的牌价 x0.29 而不是企业端点的实扣 x0.00。

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"strings"
	"testing"
)

// catalogFixture 真实上游形态的假上游：按 UA 分路，内容与实测 dump 同构。
// 每个字段都能在 D:/wb2tmp/upstream 的原始 dump 里找到出处。
type catalogFixture struct {
	enterprise string
	v3IDE      string
	v3CLI      string
}

// realShapedCatalog 与实测 dump 同构的最小复刻（保留所有影响结论的字段）。
func realShapedCatalog() catalogFixture {
	return catalogFixture{
		// 计费目录：18 条里抽 6 条（含两个 x0.00 与 agents[cli] 白名单）。
		// **故意一条 deepseek 都没有**——这是实测事实，也是 5 条丢失的前提。
		enterprise: `{"code":0,"data":{
			"models":[
				{"id":"default-model","name":"Auto","maxInputTokens":176000,"maxOutputTokens":24000,"credits":"x0.79 credits","isDefault":true},
				{"id":"fast-model","name":"Fast","maxInputTokens":200000,"maxOutputTokens":32000,"credits":"x0.34 credits"},
				{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":48000,"credits":"x0.79"},
				{"id":"kimi-k2.6","name":"Kimi-K2.6","maxInputTokens":256000,"maxOutputTokens":32000,"credits":"x0.52"}
			],
			"agents":[{"name":"cli","models":["default-model","fast-model","hy4-preview","hy3","glm-5.3","kimi-k2.6"]}],
			"modelPromotions":[]
		}}`,
		// IDE 路：13 条里抽关键几条 + 试用横幅（hy4-preview-f 只在这里）。
		v3IDE: `{"code":0,"data":{
			"models":[
				{"id":"default-model","name":"Auto","maxInputTokens":176000,"maxOutputTokens":24000},
				{"id":"auto-chat","name":"Auto","maxInputTokens":168000,"maxOutputTokens":32000,"credits":"x0.57"},
				{"id":"enhance-1.0","name":"Enhance-1.0","maxOutputTokens":32000},
				{"id":"o4-mini","name":"GPT-4o-Mini","maxInputTokens":104000,"maxOutputTokens":24000,"tags":["badge:企业版:#3B82F6"]},
				{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},
				{"id":"nes-1.1","name":"nes-1.1","maxOutputTokens":8192},
				{"id":"completion-1.0","name":"completion-1.0","maxOutputTokens":256},
				{"id":"codewise-jump","name":"codewise-jump","maxOutputTokens":256}
			],
			"modelPromotions":[],
			"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
				{"modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14}
			]}}
		}}`,
		// CLI 路：22 条里抽关键几条——**5 条丢失模型全在这一路**。
		v3CLI: `{"code":0,"data":{
			"models":[
				{"id":"default-model","name":"Auto","maxInputTokens":176000,"maxOutputTokens":24000},
				{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"},
				{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":48000,"credits":"x0.79"},
				{"id":"kimi-k2.6","name":"Kimi-K2.6","maxInputTokens":256000,"maxOutputTokens":32000,"credits":"x0.52"},
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.00","supportsImages":true,"supportsReasoning":true,"reasoning":{"effort":"high"}},
				{"id":"deepseek-v4.1-flash-sg","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03","supportsImages":true,"supportsReasoning":true,"reasoning":{"effort":"high"}},
				{"id":"glm-5.3-flash","name":"GLM-5.3-Flash","maxInputTokens":1000000,"maxOutputTokens":32000,"credits":"x0.06","supportsImages":true,"supportsReasoning":true,"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}},
				{"id":"kimi-k2.8-preview","name":"Kimi-K2.8-Preview","maxInputTokens":1000000,"maxOutputTokens":32000,"credits":"x0.77","supportsImages":true,"supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}},
				{"id":"gpt-6-astra","name":"GPT-6-Astra","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x6.67","supportsImages":true,"supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","medium","high","xhigh","max"]}}
			],
			"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
				{"modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14}
			]}}
		}}`,
	}
}

// catalogSortedIDs 目录 id 列表（升序，失败信息里用）。
func catalogSortedIDs(m map[string]ModelInfo) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// catalogTestClient 假上游按路径 + UA 分路（与真实上游同形态）。
func catalogTestClient(t *testing.T, f catalogFixture) *Client {
	t.Helper()
	c, _ := globalTestClient(t, func(string) (int, string) { return 500, "unused" })
	c.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v2/enterprises/personal/models":
			return jsonResp(200, f.enterprise), nil
		case "/v3/config":
			if r.Header.Get("User-Agent") == codeBuddyCLIUA {
				return jsonResp(200, f.v3CLI), nil
			}
			return jsonResp(200, f.v3IDE), nil
		}
		return jsonResp(404, `{}`), nil
	})}
	return c
}

// lostV3OnlyIDs 实测「面板看不见、但真实可调用」的 5 条（任务 A 的核心回归清单）。
var lostV3OnlyIDs = []string{
	"deepseek-v4.1-flash-sg", // 新加坡版，v3-CLI x0.03
	"glm-5.3-flash",          // v3-CLI x0.06
	"kimi-k2.8-preview",      // v3-CLI x0.77
	"gpt-6-astra",            // v3-CLI x6.67
	"hy4-preview-f",          // 试用横幅（data.models 里没有，只在 ModelTrialBanner）
}

// TestCatalogKeepsV3OnlyModels /v1/models 路径（FetchGlobalModelInfos）必须含 5 条。
// 这条今天就是绿的（applyGlobalV3Catalog 有追加步）——它的价值是防止重写把追加步删掉。
func TestCatalogKeepsV3OnlyModels(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	byID := map[string]ModelInfo{}
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		byID[mi.ID] = mi
	}
	for _, id := range lostV3OnlyIDs {
		if _, ok := byID[id]; !ok {
			t.Errorf("/v1/models 目录缺 %s，实际 ids=%v", id, catalogSortedIDs(byID))
		}
	}
}

// TestCatalogPanelPathKeepsV3OnlyModels 面板路径（FetchModelsDiag）同样必须含 5 条。
//
// **实现前必红**：mergeModelCapabilities 只对基底已有 id 做 fill-only 覆盖，
// v3 独有 id 完全不进 out——5 条全部缺失。这是用户实测「面板 14 条 / Fetch 28 条」
// 差距的主因（另一半是隐藏别名）。
func TestCatalogPanelPathKeepsV3OnlyModels(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	infos, _, err := c.FetchModelsDiag(globalAuth())
	if err != nil {
		t.Fatalf("FetchModelsDiag: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	for _, id := range lostV3OnlyIDs {
		mi, ok := byID[id]
		if !ok {
			t.Errorf("面板路径缺 %s（v3 独有 id 未追加），实际 ids=%v", id, catalogSortedIDs(byID))
			continue
		}
		// 追加的条目必须带 v3 的能力元数据（不是裸 id）。
		if mi.MaxTokens == 0 {
			t.Errorf("%s 追加后缺 max_output_tokens（能力未搬运）: %+v", id, mi)
		}
	}
	// 两条路径都必须覆盖「上游目录 ∪ v3 追加」——同一份上游数据，两种投影不许漂移。
	// （/v1/models 额外还会按静态名单补 id：那是探测失败时的兜底名单，见
	// mergeGlobalModelInfos，不属于"上游数据"，故不在此比对。）
	panel := map[string]bool{}
	for _, mi := range infos {
		panel[mi.ID] = true
	}
	for _, id := range []string{"default-model", "fast-model", "hy4-preview", "hy3", "glm-5.3", "kimi-k2.6"} {
		if !panel[id] {
			t.Errorf("面板路径缺企业端点条目 %s", id)
		}
	}
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		if isStaticFallbackName(mi.ID) {
			continue // 静态兜底名单（GlobalModelNames）不是上游数据，/v1/models 独有
		}
		if !panel[mi.ID] {
			t.Errorf("%s 在 /v1/models 有、面板没有（两种投影漂移）", mi.ID)
		}
	}
}

// isStaticFallbackName 该 id 是否来自静态兜底名单（GlobalModelNames）。
func isStaticFallbackName(id string) bool {
	for _, n := range GlobalModelNames {
		if n == id {
			return true
		}
	}
	return false
}

// TestCatalogTrialBannerInheritsCapabilities hy4-preview-f 只在 ModelTrialBanner 里，
// 能力必须继承 targetModelId(hy4-preview)。双 UA 并集若取「primary(IDE) 优先」，
// 留下的是 IDE 路的**空壳**（IDE 的 data.models 没有 hy4-preview）→ metadata 全零。
func TestCatalogTrialBannerInheritsCapabilities(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	byID := map[string]ModelInfo{}
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		byID[mi.ID] = mi
	}
	hy, ok := byID["hy4-preview-f"]
	if !ok {
		t.Fatalf("缺 hy4-preview-f，实际 ids=%v", catalogSortedIDs(byID))
	}
	if hy.ContextWindow != 1000000 || hy.MaxTokens != 64000 {
		t.Errorf("hy4-preview-f 能力未从 targetModelId 继承：ctx=%d maxOut=%d want 1000000/64000",
			hy.ContextWindow, hy.MaxTokens)
	}
	// 试用版的 Credits 显式清空（描述的是转正后的计费，用在试用版上会误导）。
	if hy.Credits != "" {
		t.Errorf("hy4-preview-f Credits=%q want 空（试用版不继承牌价）", hy.Credits)
	}
}

// TestCatalogCreditsPrecedenceEnterpriseWins 价格优先级（任务 B）：
// **企业端点是计费目录，同名时它的 credits 权威**；v3 只在企业端点没给该 id 时补缺。
//
// 实测：hy4-preview 企业端点 x0.00（= promo 的 discountedCredits "0x"，即实扣口径），
// v3-CLI x0.29（牌价）——现状被 v3 覆盖，面板显示 x0.29。
func TestCatalogCreditsPrecedenceEnterpriseWins(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	snap := map[string]string{}
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		snap[mi.ID] = mi.Credits
	}
	_ = snap

	// 快照出口才带倍率（Fetch 的返回值恒空，PLAN §3.D2）。
	byID := map[string]ModelInfo{}
	for _, mi := range c.GlobalModelInfosSnapshot() {
		byID[mi.ID] = mi
	}
	if got := byID["hy4-preview"].Credits; got != "x0.00" {
		t.Errorf("hy4-preview credits=%q want x0.00（企业端点=计费目录，权威；v3 的 x0.29 是牌价，不得覆盖）", got)
	}
	if got := byID["hy3"].Credits; got != "x0.00" {
		t.Errorf("hy3 credits=%q want x0.00", got)
	}
	// 企业端点**没给**的 id：v3 是唯一来源 → 采用（否则 5 条全是未知倍率）。
	want := map[string]string{
		"deepseek-v4.1-flash":    "x0.00",
		"deepseek-v4.1-flash-sg": "x0.03",
		"glm-5.3-flash":          "x0.06",
		"kimi-k2.8-preview":      "x0.77",
		"gpt-6-astra":            "x6.67",
	}
	for id, w := range want {
		if got := byID[id].Credits; got != w {
			t.Errorf("%s credits=%q want %q（企业端点无此 id → 取 v3）", id, got, w)
		}
	}
	// 企业端点带 " credits" 尾巴的形态必须归一（面板曾原样显示 "x0.79 credits"）。
	if got := byID["default-model"].Credits; got != "x0.79" {
		t.Errorf("default-model credits=%q want x0.79（去 ' credits' 尾巴）", got)
	}
}

// TestCatalogPromoExpiredNotAttached 促销只在生效期内挂（既有语义不回归）。
// 实测 dump 里 hy3/hy4-preview 的两条 promo 都已过期（validUntil 09-30 / 09-08），
// 今天是 2026-10-02 → 不得挂 promo_*，也不得把 credits 改写成折扣价。
func TestCatalogPromoExpiredNotAttached(t *testing.T) {
	f := realShapedCatalog()
	f.enterprise = `{"code":0,"data":{"models":[
		{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"},
		{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.00"}
	],"agents":[{"name":"cli","models":["hy3","hy4-preview"]}],
	"modelPromotions":[
		{"enabled":true,"priority":200,"modelIds":["hy3"],"badge":{"label":"Free now"},
		 "discount":{"discountedCredits":"0x","factor":0},
		 "hover":{"textZh":"Aug 6 – Sep 30"},
		 "schedule":{"timezone":"Asia/Shanghai","validFrom":"2026-07-06T00:00:00+08:00","validUntil":"2026-09-30T00:00:00+08:00"}},
		{"enabled":true,"priority":200,"modelIds":["hy4-preview"],"badge":{"label":"Free now"},
		 "discount":{"discountedCredits":"0x","factor":0},
		 "hover":{"textZh":"Aug 28 – Sep 10"},
		 "schedule":{"timezone":"Asia/Shanghai","validFrom":"2026-07-06T00:00:00+08:00","validUntil":"2026-09-08T00:00:00+08:00"}}
	]}}`
	c := catalogTestClient(t, f)
	byID := map[string]ModelInfo{}
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		byID[mi.ID] = mi
	}
	for _, id := range []string{"hy3", "hy4-preview"} {
		mi := byID[id]
		if mi.PromoFactor != nil || mi.PromoLabel != "" {
			t.Errorf("%s 的 promo 已过期（validUntil 早于今天），不得挂载: %+v", id, mi)
		}
	}
}

// TestCatalogPromoActiveAttached 生效期内的 promo 必须挂上（正例，防"全不挂"式修复）。
func TestCatalogPromoActiveAttached(t *testing.T) {
	f := realShapedCatalog()
	f.enterprise = `{"code":0,"data":{"models":[
		{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"}
	],"agents":[{"name":"cli","models":["hy3"]}],
	"modelPromotions":[
		{"enabled":true,"priority":200,"modelIds":["hy3"],"badge":{"label":"Free now"},
		 "discount":{"discountedCredits":"0x","factor":0},
		 "hover":{"textZh":"Free daily use"},
		 "schedule":{"timezone":"Asia/Shanghai","validFrom":"2020-01-01T00:00:00+08:00","validUntil":"2099-01-01T00:00:00+08:00"}}
	]}}`
	c := catalogTestClient(t, f)
	// Fetch 触发探测并落缓存；倍率只在快照出口透出（PLAN §3.D2）。
	byID := map[string]ModelInfo{}
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		byID[mi.ID] = mi
	}
	snap := map[string]ModelInfo{}
	for _, mi := range c.GlobalModelInfosSnapshot() {
		snap[mi.ID] = mi
	}
	mi := byID["hy3"]
	if mi.PromoFactor == nil || *mi.PromoFactor != 0 {
		t.Fatalf("hy3 promo_factor=%v want 0（生效期内必须挂）", mi.PromoFactor)
	}
	if mi.PromoCredits != "0x" || mi.PromoLabel != "Free now" || mi.PromoNote != "Free daily use" {
		t.Errorf("hy3 promo 字段不全: %+v", mi)
	}
	// promo 不得污染牌价（两个口径分离）。
	if snap["hy3"].Credits != "x0.00" {
		t.Errorf("hy3 牌价 credits=%q 被 promo 污染", snap["hy3"].Credits)
	}
}

// TestNonChatModelBoundary nonChatModel 边界（不回归 + 修漏）：
//   - 真图片/嵌入/补全模型仍被过滤（text-to-image、image-to-image、nes-/completion-/
//     codewise- 前缀、maxOutputTokens≤256）；
//   - 带营销 tags（badge:…）或能力 tags（craft）的对话模型**不得**误伤。
func TestNonChatModelBoundary(t *testing.T) {
	cases := []struct {
		id     string
		maxOut int64
		tags   []string
		want   bool
	}{
		// 真非对话模型：过滤。
		{"hunyuan-image-alpha", 0, []string{"text-to-image"}, true},
		{"hunyuan-image-alpha-edit", 0, []string{"image-to-image"}, true}, // 实测 CN v3 下发
		{"nes-1.1", 8192, nil, true},
		{"nes-1.2", 8192, nil, true},
		{"nes-gf", 256, nil, true},
		{"completion-1.0", 256, nil, true},
		{"codewise-jump", 256, nil, true},
		{"codewise-default-model-v2", 32000, nil, true},
		{"tiny-output", 256, nil, true},
		// 对话模型：保留（tags 里的营销/能力标注不得当成图片模型）。
		{"hy4-preview", 64000, nil, false},
		{"hy3", 64000, []string{"craft"}, false},
		{"balanced-model", 32000, []string{"craft"}, false},
		{"o4-mini", 24000, []string{"badge:企业版:#3B82F6"}, false},
		{"glm-5.3-flash", 32000, nil, false},
		{"kimi-k2.8-preview", 32000, nil, false},
		{"deepseek-v4.1-flash-sg", 128000, nil, false},
		{"gpt-6-astra", 128000, nil, false},
		// maxOutputTokens 缺失（0）不是"tiny 输出"：未知 ≠ 非对话。
		{"enhance-1.0", 0, nil, false},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.maxOut, c.tags); got != c.want {
			t.Errorf("nonChatModel(%q, %d, %v)=%v want %v", c.id, c.maxOut, c.tags, got, c.want)
		}
	}
}

// TestCatalogImageEditFilteredFromList image-to-image 模型（实测 CN 下发
// hunyuan-image-alpha-edit）不得进目录——选了报错，与 text-to-image 同类。
func TestCatalogImageEditFilteredFromList(t *testing.T) {
	f := realShapedCatalog()
	f.v3IDE = strings.Replace(f.v3IDE,
		`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]}`,
		`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},
		 {"id":"hunyuan-image-alpha-edit","name":"Hunyuan Image Alpha Edit","tags":["image-to-image"]}`,
		1)
	c := catalogTestClient(t, f)
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		if mi.ID == "hunyuan-image-alpha-edit" {
			t.Fatal("image-to-image 模型不应进目录")
		}
	}
}

// TestCreditsNormalizedDropTrailingWord 上游部分条目把 credits 发成 "x0.79 credits"
// （实测企业端点 5/18 条如此，面板原样显示成 "x0.79 credits"）——必须归一。
func TestCreditsNormalizedDropTrailingWord(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	_ = c.FetchGlobalModelInfos(globalAuth()) // 触发探测落缓存
	byID := map[string]ModelInfo{}
	for _, mi := range c.GlobalModelInfosSnapshot() {
		byID[mi.ID] = mi
	}
	for _, id := range []string{"default-model", "fast-model"} {
		got := byID[id].Credits
		if got == "" || strings.Contains(got, "credits") {
			t.Errorf("%s credits=%q 未归一（不得带 'credits' 尾巴）", id, got)
		}
	}
	// 归一后必须能被 ParseMultiplier 解析（保留积分的免费判定依赖它）。
	for _, id := range []string{"default-model", "fast-model", "hy3"} {
		if _, ok := ParseMultiplier(byID[id].Credits); !ok {
			t.Errorf("%s credits=%q 归一后仍不可解析", id, byID[id].Credits)
		}
	}
}

// TestCatalogHiddenAliasAutoChat auto-chat 是 IDE 侧的自动选档别名（name "Auto"，
// 无固定模型身份，disabledMultimodal=true）——与 default-model 同类，默认隐藏；
// 想展示可在 config 里把 models.hidden_models 设为 []。
func TestCatalogHiddenAliasAutoChat(t *testing.T) {
	found := false
	for _, id := range DefaultHiddenModels {
		if id == "auto-chat" {
			found = true
		}
	}
	if !found {
		t.Errorf("auto-chat 应进 DefaultHiddenModels（自动选档别名），实际 %v", DefaultHiddenModels)
	}
	// o4-mini / enhance-1.0 是真实模型（IDE agents 分别绑定 web-fetch / enhance prompt），
	// 不得隐藏。
	h := ResolveHiddenModels(nil)
	for _, id := range []string{"o4-mini", "enhance-1.0"} {
		if h.Has(id) {
			t.Errorf("%s 是真实可调用模型，不应默认隐藏", id)
		}
	}
}

// TestCatalogAppendRespectsAgentsWhitelist 追加步不得推翻 agents 白名单。
//
// 实测 CN：企业端点 models[] 有 31 条，但 agents[name=cli].models 只列 17 条——
// 白名单**刻意排除**了 deepseek-v4-flash / glm-4.6 / glm-4.7 / kimi-k2.5 /
// minimax-m2.5 / hunyuan-chat 等 11 条（这些是别的客户端形态/内测的模型，不在
// cli 会话里）。若追加步拿"过滤后的 base"当"已知 id"判据，v3 里那些同名条目会被
// 当成"v3 独有"重新追加——白名单被悄悄推翻，CN 面板凭空多出 11 个模型。
//
// 本用例构造该形态：白名单挡掉 A/B，v3 同时给了 A/B（同名）与 C（真独有）。
// 期望：只追加 C，A/B 仍被白名单挡住。
func TestCatalogAppendRespectsAgentsWhitelist(t *testing.T) {
	const body = `{"code":0,"data":{
		"models":[
			{"id":"keep","name":"Keep","maxInputTokens":1000,"maxOutputTokens":500},
			{"id":"blocked-a","name":"Blocked A","maxInputTokens":1000,"maxOutputTokens":500},
			{"id":"blocked-b","name":"Blocked B","maxInputTokens":1000,"maxOutputTokens":500}
		],
		"agents":[{"name":"cli","models":["keep"]}]
	}}`
	c, _ := globalTestClient(t, func(path string) (int, string) {
		switch path {
		case "/console/enterprises/personal/models":
			return 200, body
		case "/v3/config":
			// v3 同时给同名（blocked-a/b）与真独有（v3-only）。
			return 200, `{"code":0,"data":{"models":[
				{"id":"keep","maxInputTokens":1000,"maxOutputTokens":500},
				{"id":"blocked-a","maxInputTokens":2000,"maxOutputTokens":900},
				{"id":"blocked-b","maxInputTokens":2000,"maxOutputTokens":900},
				{"id":"v3-only","maxInputTokens":3000,"maxOutputTokens":700}
			]}}`
		}
		return 500, "boom"
	})
	cnAuth := &auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "tok"}
	infos, diag, err := c.FetchModelsDiag(cnAuth)
	if err != nil {
		t.Fatalf("FetchModelsDiag(cn): %v", err)
	}
	ids := map[string]bool{}
	for _, mi := range infos {
		ids[mi.ID] = true
	}
	if !ids["keep"] {
		t.Errorf("白名单内的 keep 应保留: %v", ids)
	}
	for _, id := range []string{"blocked-a", "blocked-b"} {
		if ids[id] {
			t.Errorf("%s 被 agents 白名单挡掉，追加步不得把它放回来: %v", id, ids)
		}
	}
	if !ids["v3-only"] {
		t.Errorf("真独有 id v3-only 应被追加: %v", ids)
	}
	// 诊断里同样只该有真独有 id。
	if len(diag.V3OnlyIDs) != 1 || diag.V3OnlyIDs[0] != "v3-only" {
		t.Errorf("diag.v3_only_ids=%v want [v3-only]", diag.V3OnlyIDs)
	}
}

// TestCatalogDiagReportsV3OnlyIDs 诊断必须摊出「企业端点没有、靠 v3 追加」的 id。
//
// 这是排查"某模型怎么不在面板里"的关键一环：重写前的 diag 只记 Dropped（上游给了但
// 被筛掉），回答不了"只有 v3 给了、追加步有没有生效"——用户实测的那 5 条正落在
// 后一类里，而当时 diag 显示 dropped=[] 让人误以为一切正常。
func TestCatalogDiagReportsV3OnlyIDs(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	_, diag, err := c.FetchModelsDiag(globalAuth())
	if err != nil {
		t.Fatalf("FetchModelsDiag: %v", err)
	}
	got := map[string]bool{}
	for _, id := range diag.V3OnlyIDs {
		got[id] = true
	}
	for _, id := range lostV3OnlyIDs {
		if !got[id] {
			t.Errorf("diag.v3_only_ids 缺 %s（实际 %v）", id, diag.V3OnlyIDs)
		}
	}
	// 企业端点已给的 id 不得混进 v3_only_ids（否则诊断失去意义）。
	for _, id := range []string{"hy4-preview", "hy3", "glm-5.3", "kimi-k2.6"} {
		if got[id] {
			t.Errorf("%s 是企业端点给的，不应出现在 v3_only_ids: %v", id, diag.V3OnlyIDs)
		}
	}
	// 升序（面板直接 join 展示，顺序稳定才好比对两次刷新）。
	for i := 1; i < len(diag.V3OnlyIDs); i++ {
		if diag.V3OnlyIDs[i-1] > diag.V3OnlyIDs[i] {
			t.Errorf("v3_only_ids 未升序: %v", diag.V3OnlyIDs)
			break
		}
	}
}

// TestCatalogJSONShapeStable 面板前端按这些键渲染：能力字段是数字/布尔/数组。
func TestCatalogJSONShapeStable(t *testing.T) {
	c := catalogTestClient(t, realShapedCatalog())
	var found bool
	for _, mi := range c.FetchGlobalModelInfos(globalAuth()) {
		if mi.ID != "glm-5.3-flash" {
			continue
		}
		found = true
		b, err := json.Marshal(mi)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m["ContextWindow"] != float64(1000000) || m["MaxTokens"] != float64(32000) {
			t.Errorf("glm-5.3-flash 能力字段: %v", m)
		}
		if m["DefaultEffort"] != "high" || m["CanDisableThinking"] != true {
			t.Errorf("glm-5.3-flash 档位字段: %v", m)
		}
	}
	if !found {
		t.Fatal("缺 glm-5.3-flash")
	}
}
