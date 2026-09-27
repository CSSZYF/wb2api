// stats_credits_test.go /v1/stats 倍率透出（issue #176 对齐 sliver 5009a1f + 1dfe750）。
//
// 纪律（sliver 原话，逐条守住）：
//   - **缺失 ≠ 免费**：目录未下发 / 缓存冷 / 查不到条目 → credits 字段**整体省略**
//     （json:"...,omitempty"），绝不输出 "x0.00"、也不输出空串。把未知倍率显示成 0
//     会让运维以为该模型免费，据此做容量/成本决策。
//   - 只读：合入路径只读本地目录缓存，**绝不触发上游探测**（/v1/stats 被面板高频
//     轮询，任何上游调用都会变成对上游的额外压力）。
//   - 与 Credit / CreditPerReq 无关：那两个是**真实扣费观测**（来自上游 usage），
//     倍率是目录里的**牌价**。两者语义不同，本文件不得改动前者的取值口径。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// statsCreditsFixture 造一个能同时驱动 CN / global 目录缓存的 handler。
type statsCreditsFixture struct {
	h      *Handler
	rec    *usage.Recorder
	upCall *atomic.Int64 // CN 假上游调用计数（newStatsCreditsFixture）
	probe  *atomic.Int64 // global 目录探测计数（newGlobalStatsFixture）
}

// getStats 发一次 GET /v1/stats 并解析响应（与 statsFixture.getStats 同款，
// 本 fixture 另起一份是为了同时持有 CN 调用计数与 global 探测计数）。
func (f *statsCreditsFixture) getStats(t *testing.T, path, authz string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body)
	}
	return rec.Code, body
}

// serve 发一次请求只取状态码（对照组用）。
func (f *statsCreditsFixture) serve(t *testing.T, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code
}

func newStatsCreditsFixture(t *testing.T, apiKey string) *statsCreditsFixture {
	t.Helper()
	var calls atomic.Int64
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","credits":"x0.10"}]}}`, false
	})
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
		APIKey:   apiKey,
	})
	return &statsCreditsFixture{h: h, rec: rec, upCall: &calls, probe: new(atomic.Int64)}
}

// globalCatalogBody global 目录探测的假上游响应（对象形态，带倍率）。
const globalCatalogBody = `{"code":0,"data":{"models":[
	{"id":"hy3","name":"Hy3","maxInputTokens":192000,"credits":"x0.05"},
	{"id":"gpt-5.4","name":"GPT 5.4","maxInputTokens":262144,"credits":"x1.20"},
	{"id":"no-credits-model","name":"No Credits","maxInputTokens":128000}
]}}`

// newGlobalStatsFixture 造一个含 global 账号 + global 假上游的 handler。
// 探测计数经 probe 字段暴露：预热走一次真实探测，之后所有断言都必须零新增探测。
func newGlobalStatsFixture(t *testing.T) *statsCreditsFixture {
	t.Helper()
	withGlobalEnabled(t)
	var probe atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/enterprises/personal/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		probe.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(globalCatalogBody))
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	rec := usage.New("")
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{
			UID: "g1", AccessToken: "at-gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999,
		}),
		Upstream:         up,
		Usage:            rec,
		GlobalEnabled:    true,
		StripRealmPrefix: true,
		RealmPrecedence:  "global",
		HiddenModels:     upstream.ResolveHiddenModels(nil),
		PinnedModels:     upstream.ResolvePinnedModels(nil),
	})
	return &statsCreditsFixture{h: h, rec: rec, probe: &probe}
}

// warmGlobalCatalog 经一次 modelList 触发真实探测并落 Client 缓存
// （仓库既有手法，见 models_catalog_test.go：不引入测试专用生产 API）。
func (f *statsCreditsFixture) warmGlobalCatalog(t *testing.T) {
	t.Helper()
	before := f.probe.Load()
	byID := modelsByID(f.h.modelList())
	if _, ok := byID["hy3"]; !ok {
		t.Fatalf("预热失败：modelList 缺 hy3，实际 ids=%v", keysOfAny(byID))
	}
	if f.probe.Load() == before {
		t.Fatal("预热失败：modelList 未触发 global 探测（后续零探测断言失去意义）")
	}
}

// modelRow 取 /v1/stats 响应里某个模型的原始 JSON 行（map 形态，供"键存在性"断言）。
func modelRow(t *testing.T, body map[string]any, model string) map[string]any {
	t.Helper()
	models, _ := body["models"].([]any)
	for _, raw := range models {
		m, _ := raw.(map[string]any)
		if m["model"] == model {
			return m
		}
	}
	t.Fatalf("响应缺模型 %q：%v", model, models)
	return nil
}

// rawModelRow 取某个模型行**序列化后**的 JSON 文本（omitempty 的键存在性只能这样断言：
// map 里 "credits":"" 与键缺席看起来都是 m["credits"]==nil）。
func rawModelRow(t *testing.T, body map[string]any, model string) string {
	t.Helper()
	raw, err := json.Marshal(modelRow(t, body, model))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// 一、CN 侧合入
// ---------------------------------------------------------------------------

// TestStatsCreditsCNMerged 目录命中即透出倍率原文；剥前缀后能对上目录条目
// （cn:deepseek-v4.1-flash → deepseek-v4.1-flash）。
func TestStatsCreditsCNMerged(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	seedModelsCache([]upstream.ModelInfo{
		{ID: "glm-5.2", Credits: "x0.79"},
		{ID: "deepseek-v4.1-flash", Credits: "x0.03"},
		{ID: "no-credits-model"}, // 目录给了条目但没给倍率 → 不得输出
	})

	f := newStatsCreditsFixture(t, "")
	now := time.Now()
	for _, m := range []string{"cn:glm-5.2", "glm-5.2", "cn:deepseek-v4.1-flash", "cn:no-credits-model", "cn:not-in-catalog"} {
		f.rec.Add(now, "cn", "u1", m, usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)
	}

	code, body := f.getStats(t, "/v1/stats", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	// 前缀形态与裸名形态都按剥前缀后的裸名查目录。
	if got := modelRow(t, body, "cn:glm-5.2")["credits"]; got != "x0.79" {
		t.Errorf("cn:glm-5.2 credits=%v want x0.79（剥前缀后命中目录）", got)
	}
	if got := modelRow(t, body, "glm-5.2")["credits"]; got != "x0.79" {
		t.Errorf("裸名 glm-5.2 credits=%v want x0.79（裸名同样查目录）", got)
	}
	if got := modelRow(t, body, "cn:deepseek-v4.1-flash")["credits"]; got != "x0.03" {
		t.Errorf("cn:deepseek-v4.1-flash credits=%v want x0.03", got)
	}
	// 目录有条目但倍率为空 → 整体省略（缺失 ≠ 免费）。
	if raw := rawModelRow(t, body, "cn:no-credits-model"); strings.Contains(raw, `"credits"`) {
		t.Errorf("目录条目无倍率时不得输出 credits 键，得到 %s", raw)
	}
	// 目录里没有的模型 → 整体省略。
	if raw := rawModelRow(t, body, "cn:not-in-catalog"); strings.Contains(raw, `"credits"`) {
		t.Errorf("目录外模型不得输出 credits 键，得到 %s", raw)
	}
	// total 行是跨模型聚合，倍率无意义 → 整体省略。
	tot, _ := body["total"].(map[string]any)
	if raw, _ := json.Marshal(tot); strings.Contains(string(raw), `"credits"`) {
		t.Errorf("total 行不得输出 credits 键（跨倍率聚合无意义），得到 %s", raw)
	}
}

// TestStatsCreditsCNSnapshotIsReadOnly 冷缓存下 CN 行整体省略，且**零上游调用**
// （只读快照，绝不为了统计端点去探测目录）。
func TestStatsCreditsCNSnapshotIsReadOnly(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	f := newStatsCreditsFixture(t, "")
	f.rec.Add(time.Now(), "cn", "u1", "cn:glm-5.2", usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)

	before := f.upCall.Load()
	for i := 0; i < 3; i++ {
		code, body := f.getStats(t, "/v1/stats", "")
		if code != http.StatusOK {
			t.Fatalf("code=%d", code)
		}
		if raw := rawModelRow(t, body, "cn:glm-5.2"); strings.Contains(raw, `"credits"`) {
			t.Errorf("冷缓存下不得输出 credits 键（缺失≠免费），得到 %s", raw)
		}
	}
	if after := f.upCall.Load(); after != before {
		t.Errorf("上游请求数 %d → %d：/v1/stats 合入倍率必须只读本地缓存", before, after)
	}

	// 对照组：/v1/models 冷缓存会拉上游（证明计数探针有效，上面的 0 增量不是假阴性）。
	if code := f.serve(t, "/v1/models"); code != http.StatusOK {
		t.Fatalf("models code=%d", code)
	}
	if f.upCall.Load() == before {
		t.Error("对照失效：/v1/models 冷缓存应触发上游拉取（否则计数探针测不出东西）")
	}
}

// TestStatsCreditsCNExpiredCacheOmitted 缓存**过期**（超 TTL）同样视为冷：
// 省略字段，且不得因过期去刷新（只读快照的口径是"TTL 内才算数"）。
func TestStatsCreditsCNExpiredCacheOmitted(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	seedModelsCache([]upstream.ModelInfo{{ID: "glm-5.2", Credits: "x0.79"}})
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Now().Add(-2 * dynamicModelsTTL) // 过期
	dynamicModelsCache.Unlock()

	f := newStatsCreditsFixture(t, "")
	f.rec.Add(time.Now(), "cn", "u1", "cn:glm-5.2", usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)

	before := f.upCall.Load()
	_, body := f.getStats(t, "/v1/stats", "")
	if raw := rawModelRow(t, body, "cn:glm-5.2"); strings.Contains(raw, `"credits"`) {
		t.Errorf("缓存过期视同冷，不得输出 credits 键，得到 %s", raw)
	}
	if after := f.upCall.Load(); after != before {
		t.Errorf("上游请求数 %d → %d：过期缓存不得触发刷新", before, after)
	}
}

// ---------------------------------------------------------------------------
// 二、global 侧合入（只读快照，不触发探测）
// ---------------------------------------------------------------------------

// TestStatsCreditsGlobalMerged global 目录命中 → global: 行透出倍率；
// 目录未给倍率的条目 / 目录外模型 → 整体省略。
func TestStatsCreditsGlobalMerged(t *testing.T) {
	f := newGlobalStatsFixture(t)
	f.warmGlobalCatalog(t)

	now := time.Now()
	for _, m := range []string{"global:hy3", "global:no-credits-model", "global:not-in-catalog"} {
		f.rec.Add(now, "global", "g1", m, usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)
	}

	before := f.probe.Load()
	code, body := f.getStats(t, "/v1/stats", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got := modelRow(t, body, "global:hy3")["credits"]; got != "x0.05" {
		t.Errorf("global:hy3 credits=%v want x0.05（global 目录命中）", got)
	}
	if raw := rawModelRow(t, body, "global:no-credits-model"); strings.Contains(raw, `"credits"`) {
		t.Errorf("global 目录条目无倍率时不得输出 credits 键（缺失≠免费），得到 %s", raw)
	}
	if raw := rawModelRow(t, body, "global:not-in-catalog"); strings.Contains(raw, `"credits"`) {
		t.Errorf("global 目录外模型不得输出 credits 键，得到 %s", raw)
	}
	if after := f.probe.Load(); after != before {
		t.Errorf("global 探测请求数 %d → %d：倍率合入必须只读快照", before, after)
	}
}

// TestStatsCreditsGlobalColdCacheNoProbe global 缓存冷 → 省略且**绝不触发探测**
// （这是 1dfe750 的核心契约：GlobalModelInfosSnapshot 是只读读取口）。
func TestStatsCreditsGlobalColdCacheNoProbe(t *testing.T) {
	f := newGlobalStatsFixture(t)
	f.rec.Add(time.Now(), "global", "g1", "global:hy3", usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)

	before := f.probe.Load()
	for i := 0; i < 3; i++ {
		code, body := f.getStats(t, "/v1/stats", "")
		if code != http.StatusOK {
			t.Fatalf("code=%d", code)
		}
		if raw := rawModelRow(t, body, "global:hy3"); strings.Contains(raw, `"credits"`) {
			t.Errorf("global 冷缓存下不得输出 credits 键（缺失≠免费），得到 %s", raw)
		}
	}
	if after := f.probe.Load(); after != before {
		t.Errorf("global 探测请求数 %d → %d：冷缓存不得触发探测", before, after)
	}
}

// TestStatsCreditsGlobalExpiredCacheOmitted 缓存过期（TTL 外）视同冷的口径由
// internal/upstream 的 TestGlobalModelInfosSnapshotTTL 在包内直接验证（那里能改
// fetched 时间戳）；本层只锁"快照 nil → 字段整体省略"的投影行为。
func TestStatsCreditsGlobalExpiredCacheOmitted(t *testing.T) {
	f := newGlobalStatsFixture(t)
	f.warmGlobalCatalog(t)
	// 让快照返回 nil：等价于过期（fetchGlobalModelsCache 的唯一 TTL 判据就是
	// fetched 时间；包内用例已覆盖时间推进路径）。
	f.h.cfg.Upstream = nil

	f.rec.Add(time.Now(), "global", "g1", "global:hy3", usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)

	_, body := f.getStats(t, "/v1/stats", "")
	if raw := rawModelRow(t, body, "global:hy3"); strings.Contains(raw, `"credits"`) {
		t.Errorf("global 快照不可用时不得输出 credits 键，得到 %s", raw)
	}
}

// ---------------------------------------------------------------------------
// 三、键归一与边界
// ---------------------------------------------------------------------------

// TestStatsCreditsKeyNormalization 键归一：裸名按 router 归属域查表；未知前缀 /
// 统计占位名（"-"）一律查不到 → 省略。
func TestStatsCreditsKeyNormalization(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	seedModelsCache([]upstream.ModelInfo{{ID: "glm-5.2", Credits: "x0.79"}})

	f := newStatsCreditsFixture(t, "")
	now := time.Now()
	// 未知前缀 "weird:" 不是 realm 前缀 → 整体当裸名，目录里没有该条目 → 省略。
	// 空模型名由 usage 记录器归一为 "(unknown)"（见 usage.Add），一并纳入。
	for _, m := range []string{"-", "weird:glm-5.2", "total", "", "(unknown)"} {
		f.rec.Add(now, "cn", "u1", m, usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)
	}
	f.rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)

	_, body := f.getStats(t, "/v1/stats", "")
	if got := modelRow(t, body, "glm-5.2")["credits"]; got != "x0.79" {
		t.Errorf("裸名 glm-5.2 credits=%v want x0.79", got)
	}
	for _, m := range []string{"-", "weird:glm-5.2", "total", "(unknown)"} {
		row := modelRow(t, body, m)
		if raw, _ := json.Marshal(row); strings.Contains(string(raw), `"credits"`) {
			t.Errorf("模型键 %q 不得输出 credits 键，得到 %s", m, raw)
		}
	}
}

// TestStatsCreditsDoesNotTouchCreditObservations 倍率合入不得改动真实扣费观测的
// 既有语义（Credit / CreditPerReq 来自上游 usage，与目录牌价是两回事）。
func TestStatsCreditsDoesNotTouchCreditObservations(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	seedModelsCache([]upstream.ModelInfo{{ID: "glm-5.2", Credits: "x0.79"}})

	f := newStatsCreditsFixture(t, "")
	now := time.Now()
	f.rec.Add(now, "cn", "u1", "cn:glm-5.2", usage.Delta{
		PromptTokens: 10, HasPromptTokens: true, LatencyMs: 100, HasLatency: true,
		Credit: 0.06, HasCredit: true,
	}, true)
	f.rec.Add(now, "cn", "u1", "cn:glm-5.2", usage.Delta{
		PromptTokens: 10, HasPromptTokens: true, LatencyMs: 100, HasLatency: true,
		Credit: 0.02, HasCredit: true,
	}, true)

	_, body := f.getStats(t, "/v1/stats", "")
	row := modelRow(t, body, "cn:glm-5.2")
	if row["credit"] != float64(0.08) {
		t.Errorf("credit=%v want 0.08（真实扣费求和，不受倍率影响）", row["credit"])
	}
	if row["credit_per_req"] != float64(0.04) {
		t.Errorf("credit_per_req=%v want 0.04（扣费/请求数）", row["credit_per_req"])
	}
	// 倍率是目录原文，与扣费观测并存且互不换算。
	if row["credits"] != "x0.79" {
		t.Errorf("credits=%v want x0.79（目录原文，非扣费换算）", row["credits"])
	}
}

// TestStatsCreditsNotAssembledUpstream /v1/stats 不装配 Upstream（裸 handler / 测试）
// 时不得 panic：CN 侧照常合入，global 侧整体省略。
func TestStatsCreditsNotAssembledUpstream(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	seedModelsCache([]upstream.ModelInfo{{ID: "glm-5.2", Credits: "x0.79"}})

	rec := usage.New("")
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Usage: rec})
	rec.Add(time.Now(), "cn", "u1", "cn:glm-5.2", usage.Delta{PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true}, true)

	req := httptest.NewRequest("GET", "/v1/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（未装配 Upstream 也不得 500）", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := modelRow(t, body, "cn:glm-5.2")["credits"]; got != "x0.79" {
		t.Errorf("credits=%v want x0.79（CN 侧不依赖 Upstream 实例）", got)
	}
}
