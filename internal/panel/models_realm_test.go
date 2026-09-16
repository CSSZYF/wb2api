package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsGlobalRealmUsesV2Path 复现并锁死「拉取模型 500」。
//
// 线上症状：只登国际版账号时点面板「模型与档位」→ 502 + body
// `fetch models: models api status 500: <html>…500 Internal Server Error…</html>`。
// 根因：panel.models 原先用 Pool.Pick() 取号 + FetchModels 写死 /console 家族路径；
// 国际站的 /console 家族返回 500 网关错误页，而 global 需要 /v2 家族。
//
// 修复后：global 域必须走 /v2 家族，且绝不能再碰 /console。
func TestPanelModelsGlobalRealmUsesV2Path(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var consoleHits, v2Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/enterprises/personal/models":
			v2Hits++
			w.Header().Set("Content-Type", "application/json")
			// 国际站形态：带完整能力字段、不带 agents 名单（走"过滤后全表"兜底）。
			// 混入路由别名 default-model 与带冗余厂商前缀的 qwen 名称验展示治理；
			// **故意不返回 deepseek-v4.1-flash**——它靠写死条目兜底出现。
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"default-model","name":"Auto","maxInputTokens":180224,"maxOutputTokens":24576,
				 "credits":"x0.79","reasoning":{"defaultEffort":"auto"}},
				{"id":"gpt-5.4","name":"GPT 5.4","maxInputTokens":262144,"maxOutputTokens":65536,
				 "maxAllowedSize":262144,"credits":"x1.00","supportsReasoning":true,
				 "reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}},
				{"id":"qwen-3-max","name":"Qwen: Qwen 3 Max",
				 "maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.11"},
				{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,
				 "credits":"x0.79","reasoning":{"defaultEffort":"auto","supportedEfforts":["auto"]}},
				{"id":"completion-x","maxOutputTokens":128}
			]}}`))
		case "/console/enterprises/personal/models":
			consoleHits++
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<html><head><title>500 Internal Server Error</title></head></html>`))
		default:
			w.WriteHeader(http.StatusNotFound) // /v3/config 覆盖失败→静默保留目录字段
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})

	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	pn := New(Config{
		Version: "test", APIKey: "test-key", Pool: p, Upstream: up,
		HiddenModels: upstream.ResolveHiddenModels(nil), // 与 main 注入方式一致
		PinnedModels: upstream.ResolvePinnedModels(nil), // 同上
	})
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（非 200 说明又打到了 /console 家族）", rec.Code, rec.Body.String())
	}
	if consoleHits != 0 {
		t.Fatalf("global 域不应请求 /console 家族，实际命中 %d 次", consoleHits)
	}
	if v2Hits == 0 {
		t.Fatal("未请求 /v2 家族：global 模型目录探测路径不对")
	}

	var got struct {
		Realm  string `json:"realm"`
		Models []struct {
			ID               string   `json:"id"`
			Name             string   `json:"name"`
			Credits          string   `json:"credits"`
			DefaultEffort    string   `json:"default_effort"`
			SupportedEfforts []string `json:"supported_efforts"`
			ContextLength    int64    `json:"context_length"`
			MaxOutputTokens  int64    `json:"max_output_tokens"`
		} `json:"models"`
		Diag struct {
			Path        string   `json:"path"`
			CliAgentIDs []string `json:"cli_agent_ids"`
			AllModelIDs []string `json:"all_model_ids"`
			Dropped     []string `json:"dropped"`
			Hidden      []string `json:"hidden"`
			Pinned      []struct {
				ID string `json:"id"`
			} `json:"pinned"`
			CountUpstream int `json:"count_upstream"`
			CountShown    int `json:"count_shown"`
		} `json:"diag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Realm != "global" {
		t.Fatalf("realm=%q want global", got.Realm)
	}
	if len(got.Models) == 0 {
		t.Fatal("模型列表为空")
	}

	// 五列都得有值——这正是"面板模型与档位整片空白"的回归点：
	// 只要 global 走了只取模型名的探测路径，倍率/默认档/思考档/上下文/最大输出就全空。
	byID := make(map[string]int, len(got.Models))
	for i, m := range got.Models {
		byID[m.ID] = i
	}
	if _, ok := byID["completion-x"]; ok {
		t.Fatal("completion- 前缀的非对话模型不应出现在列表里")
	}
	// 路由别名不是真实模型：面板与 /v1/models 都不该列（同名单位于 hidden_models）。
	if _, ok := byID["default-model"]; ok {
		t.Fatal("default-model 是上游路由别名，默认应被 hidden_models 隐藏")
	}
	// 名称里的冗余厂商标注要剥掉："Qwen: Qwen 3 Max" → "Qwen 3 Max"
	qIdx, ok := byID["qwen-3-max"]
	if !ok {
		t.Fatalf("缺 qwen-3-max，实际 ids=%v", got.Models)
	}
	if n := got.Models[qIdx].Name; n != "Qwen 3 Max" {
		t.Errorf("qwen 展示名=%q want %q（不应带冗余厂商标注）", n, "Qwen 3 Max")
	}

	// 写死条目：上游**没返回** deepseek-v4.1-flash，它仍必须出现在面板里，且带写死的能力快照。
	dsIdx, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("缺 deepseek-v4.1-flash（写死条目未生效），实际 ids=%v", got.Models)
	}
	ds := got.Models[dsIdx]
	if ds.Name != "Deepseek-V4.1-Flash" {
		t.Errorf("deepseek name=%q want Deepseek-V4.1-Flash", ds.Name)
	}
	if ds.Credits != "x0.03" {
		t.Errorf("deepseek credits=%q want x0.03", ds.Credits)
	}
	if ds.DefaultEffort != "high" {
		t.Errorf("deepseek default_effort=%q want high", ds.DefaultEffort)
	}
	if len(ds.SupportedEfforts) != 0 {
		t.Errorf("deepseek supported_efforts=%v want 空（固定档）", ds.SupportedEfforts)
	}
	if ds.ContextLength != 1000000 {
		t.Errorf("deepseek context_length=%d want 1000000（面板 1000K）", ds.ContextLength)
	}
	if ds.MaxOutputTokens != 128000 {
		t.Errorf("deepseek max_output_tokens=%d want 128000（面板 128K）", ds.MaxOutputTokens)
	}
	i, ok := byID["gpt-5.4"]
	if !ok {
		t.Fatalf("缺 gpt-5.4，实际 ids=%v", got.Models)
	}
	m := got.Models[i]
	if m.Credits != "x1.00" {
		t.Errorf("积分倍率 credits=%q want x1.00", m.Credits)
	}
	if m.DefaultEffort != "high" {
		t.Errorf("默认档 default_effort=%q want high", m.DefaultEffort)
	}
	if len(m.SupportedEfforts) != 3 {
		t.Errorf("思考档 supported_efforts=%v want [low high max]", m.SupportedEfforts)
	}
	if m.ContextLength != 262144 {
		t.Errorf("上下文 context_length=%d want 262144", m.ContextLength)
	}
	if m.MaxOutputTokens != 65536 {
		t.Errorf("最大输出 max_output_tokens=%d want 65536", m.MaxOutputTokens)
	}

	// 诊断块：回答"上游给了什么、哪一步筛掉了谁"——排查"某模型怎么不在列表里"的唯一依据。
	if got.Diag.Path != "/v2/enterprises/personal/models" {
		t.Errorf("diag.path=%q want /v2/enterprises/personal/models", got.Diag.Path)
	}
	if len(got.Diag.CliAgentIDs) != 0 {
		t.Errorf("diag.cli_agent_ids=%v want 空（上游未给 agents）", got.Diag.CliAgentIDs)
	}
	// count_upstream 只记上游给的（写死条目不算上游）：completion-x 被 nonChatModel 滤掉 → 4。
	if got.Diag.CountUpstream != 4 {
		t.Errorf("diag.count_upstream=%d want 4（completion-x 已被 nonChatModel 滤掉）", got.Diag.CountUpstream)
	}
	// 展示 = 上游 4 + 写死 1 − 隐藏别名 1 = 4
	if got.Diag.CountShown != 4 {
		t.Errorf("diag.count_shown=%d want 4", got.Diag.CountShown)
	}
	if len(got.Diag.Pinned) != len(upstream.DefaultPinnedModels) {
		t.Errorf("diag.pinned=%d 项 want %d", len(got.Diag.Pinned), len(upstream.DefaultPinnedModels))
	}
	if len(got.Diag.AllModelIDs) != 4 {
		t.Errorf("diag.all_model_ids=%v want 4 项（completion-x 应被 nonChatModel 滤掉）", got.Diag.AllModelIDs)
	}
	for _, id := range got.Diag.AllModelIDs {
		if id == "completion-x" {
			t.Error("completion- 前缀模型不应进入 all_model_ids")
		}
	}
	if len(got.Diag.Hidden) != len(upstream.DefaultHiddenModels) {
		t.Errorf("diag.hidden=%v want 默认 %v", got.Diag.Hidden, upstream.DefaultHiddenModels)
	}
}

// TestPanelModelsNoAccountForRealm 域内无可用账号：503 且提示指明是哪个域，
// 不再笼统说"请先添加账号"（用户看到的是"没账号"却明明有另一域的账号）。
func TestPanelModelsNoAccountForRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "tok"})

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/panel/api/models?realm=global", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
}
