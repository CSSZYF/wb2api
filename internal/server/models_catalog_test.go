package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// resetDynamicModelsCache 清 CN 动态模型缓存（避免跨测试串味：缓存是包级变量）。
func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// modelsByID 把 modelList 结果按裸 id 建索引（忽略域前缀协议）。
func modelsByID(list []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		if i := strings.LastIndex(id, ":"); i >= 0 {
			id = id[i+1:] // 去掉 cn:/global: 前缀，测试按裸名断言
		}
		out[id] = m
	}
	return out
}

// TestModelListStaticCNUsesKnowledgeTable CN 动态探测失败 → 静态兜底表，窗口 / 输出上限
// 由 upstream.context_catalog 知识表按 id 补真值（不再是全表 131072 假兜底）。
func TestModelListStaticCNUsesKnowledgeTable(t *testing.T) {
	resetDynamicModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 上游 500：动态分支失败 → 静态兜底。
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 500, `boom`, false })
	h := NewHandler(Config{Pool: p, Upstream: up, StripRealmPrefix: true})

	byID := modelsByID(h.modelList())
	if len(byID) < len(staticCNModelIDs) {
		t.Fatalf("静态兜底条目数=%d want >=%d", len(byID), len(staticCNModelIDs))
	}
	// 每个静态 id 都必须在知识表命中（否则会缺 context_length）。
	for _, id := range staticCNModelIDs {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("静态表模型 %s 缺失", id)
		}
		ctx, has := m["context_length"]
		if !has {
			t.Errorf("%s: 静态兜底缺 context_length（知识表未覆盖？）", id)
			continue
		}
		if n, _ := ctx.(int64); n <= 0 {
			t.Errorf("%s: context_length=%v want >0", id, ctx)
		}
		if ctx == int64(131072) {
			t.Errorf("%s: context_length 仍是 131072 假兜底（应取知识表真值）", id)
		}
	}
	// 抽两条核对真值。
	if got := byID["glm-5.2"]["context_length"]; got != int64(1000000) {
		t.Errorf("glm-5.2 context_length=%v want 1000000（知识表）", got)
	}
	if got := byID["deepseek-v4-pro"]["context_length"]; got != int64(1000000) {
		t.Errorf("deepseek-v4-pro context_length=%v want 1000000（知识表）", got)
	}
	if got := byID["kimi-k2.7"]["context_length"]; got != int64(256000) {
		t.Errorf("kimi-k2.7 context_length=%v want 256000（知识表）", got)
	}
}

// TestModelListCNZeroContextUsesKnowledgeTable CN 动态模型 maxInputTokens 为零值 →
// 走知识表补真值；知识表未收录的 id → 省略字段（不编造 131072）。
func TestModelListCNZeroContextUsesKnowledgeTable(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"glm-5.2","maxInputTokens":0,"maxOutputTokens":0},
			{"id":"totally-unknown-model","maxInputTokens":0,"maxOutputTokens":0},
			{"id":"dyn-real","maxInputTokens":262144,"maxOutputTokens":32768}
		],"agents":[{"name":"cli","models":["glm-5.2","totally-unknown-model","dyn-real"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, StripRealmPrefix: true})

	byID := modelsByID(h.modelList())
	// 知识表命中：glm-5.2 远端零值 → 1M（绝不再是 131072）。
	if got := byID["glm-5.2"]["context_length"]; got != int64(1000000) {
		t.Errorf("glm-5.2 零值 context_length=%v want 1000000（知识表）", got)
	}
	if got := byID["glm-5.2"]["max_output_tokens"]; got != int64(131072) {
		t.Errorf("glm-5.2 零值 max_output_tokens=%v want 131072（知识表）", got)
	}
	// 知识表未收录 + 远端零值 → 两个字段都省略（不编造）。
	unknown := byID["totally-unknown-model"]
	if _, has := unknown["context_length"]; has {
		t.Errorf("未知模型不应输出 context_length（不编造），got %v", unknown["context_length"])
	}
	if _, has := unknown["max_output_tokens"]; has {
		t.Errorf("未知模型不应输出 max_output_tokens，got %v", unknown["max_output_tokens"])
	}
	// 远端真值权威（不因知识表被改写）。
	if got := byID["dyn-real"]["context_length"]; got != int64(262144) {
		t.Errorf("dyn-real context_length=%v want 262144（远端权威）", got)
	}
	if got := byID["dyn-real"]["max_output_tokens"]; got != int64(32768) {
		t.Errorf("dyn-real max_output_tokens=%v want 32768（远端权威）", got)
	}
}

// TestModelListGlobalMetadataFromProbe global 探测成功 → 条目带完整元数据
// （窗口 / 输出上限 / 档位 / 能力旗标），字段集合与 CN 分支一致（共用 modelEntry）。
func TestModelListGlobalMetadataFromProbe(t *testing.T) {
	withGlobalEnabled(t)
	resetDynamicModelsCache()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/enterprises/personal/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// global 对象形态：与 CN 目录同构（maxInputTokens/maxOutputTokens/reasoning.*）。
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
			{"id":"gpt-5.4","name":"GPT 5.4","maxInputTokens":262144,"maxOutputTokens":65536,
			 "maxAllowedSize":262144,"supportsReasoning":true,"supportsImages":true,
			 "reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high"]}},
			{"id":"zero-meta","maxInputTokens":0,"maxOutputTokens":0},
			{"id":"disabled-model","maxInputTokens":100000,"disabled":true}
		]}}`))
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	h := NewHandler(Config{
		Pool: p, Upstream: up,
		GlobalEnabled: true, StripRealmPrefix: true, RealmPrecedence: "global",
		HiddenModels: upstream.ResolveHiddenModels(nil),
		PinnedModels: upstream.ResolvePinnedModels(nil),
	})
	byID := modelsByID(h.modelList())

	m, ok := byID["gpt-5.4"]
	if !ok {
		t.Fatalf("缺 gpt-5.4，实际 ids=%v", keysOfAny(byID))
	}
	// 元数据字段断言：探测值全部透出（与 CN 分支同字段集）。
	if got := m["context_length"]; got != int64(262144) {
		t.Errorf("gpt-5.4 context_length=%v want 262144（探测权威）", got)
	}
	if got := m["max_output_tokens"]; got != int64(65536) {
		t.Errorf("gpt-5.4 max_output_tokens=%v want 65536", got)
	}
	if got := m["max_allowed_size"]; got != int64(262144) {
		t.Errorf("gpt-5.4 max_allowed_size=%v want 262144", got)
	}
	if m["supports_reasoning"] != true || m["can_disable_thinking"] != true {
		t.Errorf("gpt-5.4 思考旗标=%v/%v want true/true", m["supports_reasoning"], m["can_disable_thinking"])
	}
	if m["supports_images"] != true {
		t.Errorf("gpt-5.4 supports_images=%v want true", m["supports_images"])
	}
	if m["default_effort"] != "high" {
		t.Errorf("gpt-5.4 default_effort=%v want high", m["default_effort"])
	}
	if eff, _ := m["supported_efforts"].([]string); len(eff) != 2 || eff[0] != "low" {
		t.Errorf("gpt-5.4 supported_efforts=%v want [low high]", m["supported_efforts"])
	}
	// credits 不进 global 路径（PLAN §3.D2）：即便探测端点返回也忽略。
	if _, has := m["credits"]; has {
		t.Errorf("global 条目不应有 credits，got %v", m["credits"])
	}
	// 零元数据条目：知识表未收录 → 省略 context_length（不编造）。
	if z, ok := byID["zero-meta"]; ok {
		if _, has := z["context_length"]; has {
			t.Errorf("zero-meta 不应输出 context_length（未知即省略），got %v", z["context_length"])
		}
	} else {
		t.Error("缺 zero-meta（探测独有条目应保留）")
	}
	// disabled 剔除。
	if _, has := byID["disabled-model"]; has {
		t.Error("disabled 模型不应出现在列表里")
	}
	// 静态独有条目（探测未返回）仍按 id 补齐：deepseek-v4.1-flash 由写死条目兜底显示，
	// 满足「免费模型能显示」——窗口取写死快照 1000000。
	ds, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("缺 deepseek-v4.1-flash（静态独有条目未补齐），实际 ids=%v", keysOfAny(byID))
	}
	if got := ds["context_length"]; got != int64(1000000) {
		t.Errorf("deepseek-v4.1-flash context_length=%v want 1000000（写死条目快照）", got)
	}
}

// TestModelListGlobalFallbackToStaticNoMetadata global 探测失败（5min 负缓存）→ 回落
// 静态名单：条目仍在（id 齐全），窗口由知识表补真值，不编造、不空列表。
func TestModelListGlobalFallbackToStaticNoMetadata(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "<html>500 Internal Server Error</html>", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	h := NewHandler(Config{
		Pool: p, Upstream: up,
		GlobalEnabled: true, StripRealmPrefix: true, RealmPrecedence: "global",
		HiddenModels: upstream.ResolveHiddenModels(nil),
		PinnedModels: upstream.ResolvePinnedModels(nil),
	})
	byID := modelsByID(h.modelList())
	if len(byID) == 0 {
		t.Fatal("探测失败时不应返回空列表（静态名单兜底）")
	}
	// 静态名单里的真实模型（非路由别名）都应在，且窗口来自知识表。
	for _, id := range []string{"glm-5.2", "gpt-5.4", "kimi-k2.6", "hy3"} {
		m, ok := byID[id]
		if !ok {
			t.Errorf("探测失败时静态名单模型 %s 缺失，实际 ids=%v", id, keysOfAny(byID))
			continue
		}
		if m["context_length"] == nil {
			t.Errorf("%s: 静态兜底缺 context_length（知识表未覆盖）", id)
		}
	}
	if got := byID["glm-5.2"]["context_length"]; got != int64(1000000) {
		t.Errorf("glm-5.2 context_length=%v want 1000000（知识表）", got)
	}
	// 路由别名默认隐藏。
	if _, has := byID["default-model"]; has {
		t.Error("default-model 是路由别名，默认应被 hidden_models 隐藏")
	}
	// 负缓存：第二次调用零上游请求。
	before := hits
	_ = h.modelList()
	if hits != before {
		t.Errorf("负缓存期内不应再打上游：hits %d → %d", before, hits)
	}
}

// TestModelEntryOmitsUnknownContext modelEntry 未知模型（远端零值 + 知识表未收录）
// 省略 context_length / max_output_tokens；远端有值时原样透出。
func TestModelEntryOmitsUnknownContext(t *testing.T) {
	e := modelEntry("", upstream.ModelInfo{ID: "brand-new"})
	if _, has := e["context_length"]; has {
		t.Errorf("未知模型不应有 context_length，got %v", e["context_length"])
	}
	if _, has := e["max_output_tokens"]; has {
		t.Errorf("未知模型不应有 max_output_tokens，got %v", e["max_output_tokens"])
	}
	if e["id"] != "brand-new" || e["object"] != "model" || e["owned_by"] != "workbuddy" {
		t.Errorf("基础字段=%v", e)
	}
	e = modelEntry("global:", upstream.ModelInfo{ID: "glm-5.2", ContextWindow: 500, MaxTokens: 100})
	if e["id"] != "global:glm-5.2" {
		t.Errorf("id=%v want global:glm-5.2", e["id"])
	}
	if e["context_length"] != int64(500) || e["max_output_tokens"] != int64(100) {
		t.Errorf("远端值应权威透出：%v/%v", e["context_length"], e["max_output_tokens"])
	}
}

// TestStaticCNModelIDsCoveredByCatalog 静态 CN 表每个 id 都在知识表命中
// （静态分支无上游动态值，知识表是唯一真值来源；漏一个该模型就缺 context_length）。
func TestStaticCNModelIDsCoveredByCatalog(t *testing.T) {
	for _, id := range staticCNModelIDs {
		if _, ok := upstream.ContextWindowListing(id, 0); !ok {
			t.Errorf("静态 CN 模型 %s 不在知识表内（探测失败时会缺 context_length）", id)
		}
	}
}

func keysOfAny(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
