package upstream

import (
	"net/http"
	"strings"
	"testing"
)

// global_envelope_test.go 吸收上游 c3cc888（global 目录多信封解析兜底）+
// b498416（ModelTrialBanner 试用模型）+ 9dce68a（/v3/config 双 UA 并发取并集）。
//
// 背景：国际站的目录端点曾在多种 envelope 之间切换，只认单一形态会把登录成功的
// 账号误判成「无模型」（表现为 /v1/models 空列表 + 面板「拉取模型」报错）；
// /v3/config 对不同 UA 下发的模型集合也不同（IDE 14 条 / CLI 22 条，各有独有模型），
// 且「N 天免费试用」模型只放在 data.productFeaturesConfig.ModelTrialBanner 里、
// 不在 data.models 中。

// TestParseGlobalModelInfosTolerantEnvelopes 多信封容错（吸收上游 c3cc888）：
// code 字段可选（缺失 = 放行）、payload 取 data（非空非 null）否则整包、
// 数组定位支持 payload 直出与 models/items/list/data/result 递归下钻。
func TestParseGlobalModelInfosTolerantEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"标准 data.models", `{"code":0,"data":{"models":[{"id":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"无 code 字段（直出目录）", `{"data":{"models":[{"id":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"code 为字符串 0", `{"code":"0","data":{"models":[{"id":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"data 为 null → 取整包", `{"models":[{"id":"glm-5.2"}]}`, []string{"glm-5.2"}},
		{"顶层 models", `{"models":[{"id":"glm-5.2"},{"id":"hy3"}]}`, []string{"glm-5.2", "hy3"}},
		{"data.items", `{"code":0,"data":{"items":[{"id":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"data.list", `{"code":0,"data":{"list":[{"id":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"data.data.models 递归", `{"code":0,"data":{"data":{"models":[{"id":"glm-5.2"}]}}}`, []string{"glm-5.2"}},
		{"data.result", `{"code":0,"data":{"result":[{"id":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"payload 直出数组", `{"code":0,"data":[{"id":"glm-5.2"}]}`, []string{"glm-5.2"}},
		{"窄表字符串数组", `{"code":0,"data":["glm-5.2","hy3"]}`, []string{"glm-5.2", "hy3"}},
		{"顶层直出数组", `[{"id":"glm-5.2"}]`, []string{"glm-5.2"}},
		// 宽松键兜底（上游换键名时不至于整域空列表）：id → modelId → model → name。
		{"modelId 回退", `{"code":0,"data":{"models":[{"modelId":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"model 回退", `{"code":0,"data":{"models":[{"model":"glm-5.2"}]}}`, []string{"glm-5.2"}},
		{"name 回退", `{"code":0,"data":{"models":[{"name":"glm-5.2"}]}}`, []string{"glm-5.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			infos, err := parseGlobalModelInfos([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse: %v (raw=%s)", err, tc.raw)
			}
			got := idsOf(infos)
			if len(got) != len(tc.want) {
				t.Fatalf("ids=%v want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("ids=%v want %v", got, tc.want)
					break
				}
			}
		})
	}
}

// TestParseGlobalModelInfosLooseFallbackKeepsMetadata 宽松兜底路径仍要保留能认出的
// 元数据（上游换键名时只是键名不同，能力字段往往还在）：contextWindow/maxTokens
// 别名键、reasoning 新老双键、supportsImages；credits 照常解析（旁表来源）。
func TestParseGlobalModelInfosLooseFallbackKeepsMetadata(t *testing.T) {
	raw := `{"code":0,"data":{"items":[{"modelId":"loose-1","contextWindow":200000,"maxTokens":32000,
		"supportsReasoning":true,"supportsImages":true,"credits":"x0.5",
		"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high"]}},
		{"modelId":"loose-2","contextWindow":128000,"reasoning":{"effort":"medium"}}]}}`
	infos, err := parseGlobalModelInfos([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	m := byID["loose-1"]
	if m.ContextWindow != 200000 || m.MaxTokens != 32000 {
		t.Errorf("宽松路径窗口=%d/%d want 200000/32000", m.ContextWindow, m.MaxTokens)
	}
	if !m.SupportsReasoning || !m.SupportsImages || m.DefaultEffort != "high" || len(m.Efforts) != 2 {
		t.Errorf("宽松路径能力字段丢失：%+v", m)
	}
	if m.Credits != "x0.5" {
		t.Errorf("宽松路径 credits=%q want x0.5（旁表来源）", m.Credits)
	}
	if got := byID["loose-2"].DefaultEffort; got != "medium" {
		t.Errorf("宽松路径老键 effort=%q want medium", got)
	}
}

// TestParseGlobalModelInfosRejectsBadEnvelopeKept 宽容化不得把「真错误」也放行：
// code≠0 仍拒（业务失败不是空目录）、完全解析不出条目仍拒（调用方回落静态）。
func TestParseGlobalModelInfosRejectsBadEnvelopeKept(t *testing.T) {
	for _, raw := range []string{
		`{"code":500,"data":{"models":[{"id":"x"}]}}`,
		`{"code":"500","data":{"models":[{"id":"x"}]}}`,
		`<html>500 Internal Server Error</html>`,
		`{"code":0,"data":{"models":[]}}`,
		`{"code":0,"data":{"models":[{"disabled":true}]}}`,
		`{"code":0,"data":{"nothing":"here"}}`,
	} {
		if _, err := parseGlobalModelInfos([]byte(raw)); err == nil {
			t.Errorf("应返回错误（调用方据此回落静态）: %s", raw)
		}
	}
}

// TestFetchGlobalModelInfosV3AsFallbackWhenEnterpriseFails 企业端点全失败但
// /v3/config 可用 → 用 v3 目录兜底（吸收上游 9dce68a 的「v3 也是目录来源」语义）。
//
// 为什么值得补：企业端点挂了不等于"上游没模型"——v3/config 是同一批账号可用的
// 另一条目录来源，此时回落静态名单会让客户端在**上游其实可用**的情况下少看到
// deepseek 系列等模型（静态名单里虽有，但没有能力元数据；更关键的是 v3 独有的
// 试用模型/新模型会整批消失）。两路都失败才回落静态 + 负缓存。
func TestFetchGlobalModelInfosV3AsFallbackWhenEnterpriseFails(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/v3/config":
				return jsonResp(200, `{"code":0,"data":{"models":[
					{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":384000}]}}`), nil
			default:
				return jsonResp(500, "<html>500 Internal Server Error</html>"), nil
			}
		})},
		ChatBaseCN: "https://cn.example", BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"

	infos := c.FetchGlobalModelInfos(globalAuth())
	var got *ModelInfo
	for i := range infos {
		if infos[i].ID == "deepseek-v4.1-flash" {
			got = &infos[i]
		}
	}
	if got == nil {
		t.Fatalf("v3 兜底应把 deepseek-v4.1-flash 带进目录，实际=%v", idsOf(infos))
	}
	if got.ContextWindow != 1000000 || got.MaxTokens != 384000 {
		t.Errorf("v3 兜底条目应带能力元数据：%d/%d want 1000000/384000", got.ContextWindow, got.MaxTokens)
	}
}

// TestFetchGlobalModelInfosV3FallbackFailureStillNegativeCaches 两路全失败仍走
// 原有降级链：静态名单 + 5min 负缓存（零上游调用），不得因新增 v3 路而反复重试。
func TestFetchGlobalModelInfosV3FallbackFailureStillNegativeCaches(t *testing.T) {
	c, calls := globalTestClient(t, func(string) (int, string) {
		return 500, "<html>500</html>"
	})
	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) != len(GlobalModelNames) {
		t.Fatalf("两路全失败应回落静态名单：%d want %d", len(infos), len(GlobalModelNames))
	}
	first := calls.Load()
	_ = c.FetchGlobalModelInfos(globalAuth())
	if calls.Load() != first {
		t.Errorf("负缓存期内不应再打上游：calls %d → %d", first, calls.Load())
	}
}

// v3ConfigTestClient 构造带 /v3/config 路由的 fake 客户端（global realm）。
// v3 响应按 User-Agent 区分，用于验证双 UA 并集；enterprise 端点固定返回 entModels。
//
// UA 记录用 pathRecorder 同族的并发安全容器：双 UA 探测是**并发两路**，
// 裸 append 在 -race 下构成数据竞争（与 global_models_test.go 的 callCounter 同因）。
func v3ConfigTestClient(t *testing.T, entModels string, v3ByUA map[string]string) (*Client, *pathRecorder) {
	t.Helper()
	uas := new(pathRecorder)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/v3/config":
				ua := r.Header.Get("User-Agent")
				uas.add(ua)
				if body, ok := v3ByUA[ua]; ok {
					return jsonResp(200, body), nil
				}
				return jsonResp(400, `{"code":12403,"msg":"bad ua"}`), nil
			case "/v2/enterprises/personal/models":
				return jsonResp(200, entModels), nil
			default:
				return jsonResp(500, "<html>500</html>"), nil
			}
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"
	return c, uas
}

// TestFetchGlobalModelInfosTrialBanner 试用横幅模型补进目录（吸收上游 b498416）：
// 上游把「N 天免费试用」模型只放在 data.productFeaturesConfig.ModelTrialBanner
// （实测形态 modelId=hy4-preview-f、targetModelId=hy4-preview），data.models 里没有——
// 纯目录解析会漏，客户端选不到（该模型实际可调用）。
//
// 元数据口径：能力字段从 targetModelId 的既有条目继承（同族模型能力一致）；
// credits 与 tags 清空（它们描述的是「转正后」的计费与营销信息，用在免费试用版上
// 会误导下游展示）；firstUseTimeKey / trialDays 属账号级状态，不透出。
func TestFetchGlobalModelInfosTrialBanner(t *testing.T) {
	c, _ := v3ConfigTestClient(t,
		`{"code":0,"data":{"models":[{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"}]}}`,
		map[string]string{
			codeBuddyIDEUA: `{"code":0,"data":{"models":[{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"}],
				"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
					{"firstUseTimeKey":"hy4.first_user_time","modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14}]}}}}`,
			codeBuddyCLIUA: `{"code":0,"data":{"models":[{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":64000}]}}`,
		})

	infos := c.FetchGlobalModelInfos(globalAuth())
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	m, ok := byID["hy4-preview-f"]
	if !ok {
		t.Fatalf("试用模型 hy4-preview-f 应补进目录，实际 ids=%v", idsOf(infos))
	}
	if m.ContextWindow != 1000000 || m.MaxTokens != 64000 {
		t.Errorf("试用模型应从 targetModelId 继承能力：窗口/输出=%d/%d want 1000000/64000",
			m.ContextWindow, m.MaxTokens)
	}
	if m.Credits != "" {
		t.Errorf("试用模型 credits=%q want 空（转正后的计费信息不适用于试用版）", m.Credits)
	}
	// 目录里已有的 id 不重复补（同 id 去重）。
	n := 0
	for _, mi := range infos {
		if mi.ID == "hy4-preview-f" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("hy4-preview-f 出现 %d 次 want 1（去重）", n)
	}
}

// TestFetchGlobalModelInfosDualUACapabilityFill /v3/config 双 UA 并发取并集
// （吸收上游 9dce68a）：该端点对不同 UA 下发不同集合（IDE 14 条 / CLI 22 条，各有
// 独有模型与字段）。本仓的 /v3/config 是**能力覆盖源**（目录本身来自企业端点 ∪
// 静态名单），故并集的作用是：任一 UA 认得的模型都能拿到窗口 / 档位。
func TestFetchGlobalModelInfosDualUACapabilityFill(t *testing.T) {
	// 企业端点只给裸 id（无窗口）；IDE UA 不给该模型，CLI UA 给全字段。
	c, uas := v3ConfigTestClient(t,
		`{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash-sg"}]}}`,
		map[string]string{
			codeBuddyIDEUA: `{"code":0,"data":{"models":[{"id":"other-model","maxInputTokens":1000}]}}`,
			codeBuddyCLIUA: `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash-sg","maxInputTokens":1000000,"maxOutputTokens":384000,
				"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}}]}}`,
		})

	infos := c.FetchGlobalModelInfos(globalAuth())
	var got *ModelInfo
	for i := range infos {
		if infos[i].ID == "deepseek-v4.1-flash-sg" {
			got = &infos[i]
		}
	}
	if got == nil {
		t.Fatalf("目录应含 deepseek-v4.1-flash-sg，实际=%v", idsOf(infos))
	}
	if got.ContextWindow != 1000000 || got.MaxTokens != 384000 {
		t.Errorf("CLI UA 独有的能力字段应被采纳：%d/%d want 1000000/384000", got.ContextWindow, got.MaxTokens)
	}
	if got.DefaultEffort != "high" || len(got.Efforts) != 3 {
		t.Errorf("CLI UA 独有的档位应被采纳：effort=%q efforts=%v", got.DefaultEffort, got.Efforts)
	}
	// 两路都探测过（并发并集，不是单路）。
	var sawIDE, sawCLI bool
	for _, ua := range uas.all() {
		switch ua {
		case codeBuddyIDEUA:
			sawIDE = true
		case codeBuddyCLIUA:
			sawCLI = true
		}
	}
	if !sawIDE || !sawCLI {
		t.Errorf("应并发探测 IDE 与 CLI 两路 UA，实际=%v", uas.all())
	}
}

// TestFetchGlobalModelInfosV3FailureKeepsCatalog /v3/config 两路全失败（或只有一路
// 成功）不得影响目录本身：能力字段缺失退化为零值（由 context_catalog 知识表补），
// 目录照常返回、不报错（fail-soft，与探测失败回落静态名单的哲学一致）。
func TestFetchGlobalModelInfosV3FailureKeepsCatalog(t *testing.T) {
	c, _ := v3ConfigTestClient(t,
		`{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":200000}]}}`,
		map[string]string{}) // 两路 v3 全 400
	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) == 0 {
		t.Fatal("v3/config 全失败时目录仍应返回（企业端点成功）")
	}
	for _, mi := range infos {
		if mi.ID == "glm-5.2" && mi.ContextWindow != 200000 {
			t.Errorf("企业端点的窗口应保留：%d want 200000", mi.ContextWindow)
		}
	}
}

// TestV3ConfigModelMapUADefault 空 UA 等价 codeBuddyIDEUA（兼容既有调用点语义，
// 与上游 9dce68a 的「空串等价 IDE UA」一致）。
func TestV3ConfigModelMapUADefault(t *testing.T) {
	c, uas := v3ConfigTestClient(t, `{"code":0,"data":{"models":[{"id":"x"}]}}`,
		map[string]string{codeBuddyIDEUA: `{"code":0,"data":{"models":[{"id":"glm-5.2"}]}}`})
	if _, err := c.fetchV3ConfigModelMap(globalAuth(), ""); err != nil {
		t.Fatalf("fetchV3ConfigModelMap: %v", err)
	}
	if got := uas.all(); len(got) != 1 || got[0] != codeBuddyIDEUA {
		t.Errorf("空 UA 应回落 codeBuddyIDEUA，实际=%v", got)
	}
}

// TestGlobalProbeDoesNotHitV3WhenGateOff 逃生门（GlobalEnabled=false）关闭时，
// 新增的 v3 能力补全步骤也不得打上游（零调用契约不变）。
func TestGlobalProbeDoesNotHitV3WhenGateOff(t *testing.T) {
	c, calls := globalTestClient(t, func(string) (int, string) {
		t.Error("逃生门关闭时不应打上游")
		return 500, ""
	})
	c.GlobalEnabled = false
	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) != len(GlobalModelNames) {
		t.Fatalf("逃生门兜底条目数=%d want %d", len(infos), len(GlobalModelNames))
	}
	if calls.Load() != 0 {
		t.Errorf("上游调用=%d want 0", calls.Load())
	}
}

// TestGlobalProbeV3RequestsUseV3PathAndRealmBase v3 补全必须走 realm 切分后的 base
// （global 账号 → global base），不得串到 CN base。
//
// hosts 用 pathRecorder 记录（并发安全）：v3 能力探测是并发两路，裸 append 在
// -race 下构成数据竞争。
func TestGlobalProbeV3RequestsUseV3PathAndRealmBase(t *testing.T) {
	var rec pathRecorder
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			rec.add(r.URL.Host + r.URL.Path)
			if r.URL.Path == "/v2/enterprises/personal/models" {
				return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}]}}`), nil
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}]}}`), nil
		})},
		ChatBaseCN: "https://cn.example", BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"
	_ = c.FetchGlobalModelInfos(globalAuth())
	hosts := rec.all()
	for _, h := range hosts {
		if strings.HasPrefix(h, "cn.example") {
			t.Errorf("global 探测不得打 CN base：%v", hosts)
		}
	}
	if len(hosts) == 0 {
		t.Fatal("应有上游调用")
	}
}
