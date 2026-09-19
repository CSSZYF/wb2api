package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// statsFixture 造一个带用量记录的 handler（假上游计数用于断言"不触发上游"）。
type statsFixture struct {
	h      *Handler
	rec    *usage.Recorder
	upCall *atomic.Int64
}

func newStatsFixture(t *testing.T, apiKey string) *statsFixture {
	t.Helper()
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, sseOK, true
	})
	rec := usage.New("") // 纯内存：测试不落盘
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
		APIKey:   apiKey,
	})
	return &statsFixture{h: h, rec: rec, upCall: &calls}
}

// getStats 发一次 GET /v1/stats 并解析响应。
func (f *statsFixture) getStats(t *testing.T, path string, authz string) (int, map[string]any) {
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

// 空数据（全新实例）返回 200 + 完整结构，不 panic。
func TestStatsEndpointEmptyOK(t *testing.T) {
	f := newStatsFixture(t, "")
	code, body := f.getStats(t, "/v1/stats", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200", code)
	}
	if body["enabled"] != true {
		t.Errorf("enabled=%v want true", body["enabled"])
	}
	if _, ok := body["since"].(string); !ok {
		t.Errorf("since 必须是时间字符串，得到 %v", body["since"])
	}
	if _, ok := body["now"].(string); !ok {
		t.Errorf("now 必须是时间字符串，得到 %v", body["now"])
	}
	if _, ok := body["uptime_sec"].(float64); !ok {
		t.Errorf("uptime_sec 必须是数字，得到 %v", body["uptime_sec"])
	}
	models, ok := body["models"].([]any)
	if !ok {
		t.Fatalf("models 必须是数组，得到 %T（空数据也不得为 null）", body["models"])
	}
	if len(models) != 0 {
		t.Errorf("models=%d want 0", len(models))
	}
	total, ok := body["total"].(map[string]any)
	if !ok {
		t.Fatalf("total 必须是对象，得到 %T", body["total"])
	}
	if total["model"] != "total" || total["requests"] != float64(0) {
		t.Errorf("total=%v want model=total requests=0", total)
	}
}

// 上游 schema 的字段名逐字对齐（消费方按上游约定解析，改名即破坏兼容）。
func TestStatsEndpointSchemaFieldNames(t *testing.T) {
	f := newStatsFixture(t, "")
	// 造一条带全量观测的记录（含缓存三段与扣费）。
	f.rec.Add(time.Now(), "cn", "u1", "glm-5.2", usage.Delta{
		PromptTokens: 50, HasPromptTokens: true,
		CompletionTokens: 100, HasCompletion: true,
		TotalTokens: 150, HasTotal: true,
		LatencyMs: 5000, HasLatency: true,
		CacheHit: 800, CacheMiss: 200, HasCache: true,
		Credit: 0.02, HasCredit: true,
		Stream: true, TTFBMs: 1000, HasTTFB: true,
		GenerationMs: 4000, HasGenerationMs: true,
	}, true)

	code, body := f.getStats(t, "/v1/stats", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	// 顶层字段（上游 MetricsSnapshot 逐字）。
	for _, k := range []string{"enabled", "since", "now", "uptime_sec", "total", "models"} {
		if _, ok := body[k]; !ok {
			t.Errorf("顶层缺字段 %q", k)
		}
	}
	models := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models=%d want 1", len(models))
	}
	m := models[0].(map[string]any)
	// 元素字段（上游 ModelStatPayload 逐字）。
	for _, k := range []string{
		"model", "requests", "success", "failed", "streaming",
		"avg_ttfb_ms", "avg_latency_ms", "tokens_per_sec",
		"prompt_tokens", "completion_tokens", "total_tokens",
		"cache_hit_tokens", "cache_miss_tokens", "cache_write_tokens", "cache_hit_rate",
		"credit", "credit_per_req", "last_seen",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("models[] 缺字段 %q（必须与上游 schema 对齐）", k)
		}
	}
	// 数值口径抽查。
	if m["requests"] != float64(1) || m["success"] != float64(1) || m["streaming"] != float64(1) {
		t.Errorf("req/succ/stream=%v/%v/%v want 1/1/1", m["requests"], m["success"], m["streaming"])
	}
	if m["prompt_tokens"] != float64(50) || m["completion_tokens"] != float64(100) || m["total_tokens"] != float64(150) {
		t.Errorf("tokens=%v/%v/%v want 50/100/150", m["prompt_tokens"], m["completion_tokens"], m["total_tokens"])
	}
	if got := m["cache_hit_rate"].(float64); got < 0.7999 || got > 0.8001 {
		t.Errorf("cache_hit_rate=%v want 0.8", got)
	}
	if m["credit"] != 0.02 {
		t.Errorf("credit=%v want 0.02", m["credit"])
	}
	// total 与 models 同口径。
	tot := body["total"].(map[string]any)
	if tot["prompt_tokens"] != float64(50) || tot["cache_hit_tokens"] != float64(800) {
		t.Errorf("total=%v 与 models 不同口径", tot)
	}
}

// 鉴权与其余 /v1/* 同口径：无 key / 错 key → 401；对 key → 200；api_key 为空 → 放行。
func TestStatsEndpointAuth(t *testing.T) {
	f := newStatsFixture(t, "secret")
	for _, tc := range []struct {
		name  string
		authz string
		want  int
	}{
		{"无 key", "", http.StatusUnauthorized},
		{"错 key", "Bearer wrong", http.StatusUnauthorized},
		{"方案不对", "secret", http.StatusUnauthorized},
		{"对 key", "Bearer secret", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := f.getStats(t, "/v1/stats", tc.authz)
			if code != tc.want {
				t.Errorf("code=%d want %d", code, tc.want)
			}
		})
	}

	// api_key 为空：端点放行（与 /v1/models、/status 同一规则）。
	open := newStatsFixture(t, "")
	if code, _ := open.getStats(t, "/v1/stats", ""); code != http.StatusOK {
		t.Errorf("api_key 为空时放行，得到 code=%d", code)
	}
}

// 未装配 usage 记录器时仍 200 + enabled=false（不 404/501，消费方不必特判）。
func TestStatsEndpointNoRecorder(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	req := httptest.NewRequest("GET", "/v1/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（未装配记录器也不得 404/501）", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != false {
		t.Errorf("enabled=%v want false", body["enabled"])
	}
	if msg, _ := body["message"].(string); msg == "" {
		t.Error("enabled=false 时必须带 message 说明原因")
	}
	if _, ok := body["models"].([]any); !ok {
		t.Errorf("models 必须是数组（不得为 null），得到 %T", body["models"])
	}
}

// 聚合正确性：按模型分组，多模型互不串味，total 为各模型之和。
func TestStatsEndpointAggregatesByModel(t *testing.T) {
	f := newStatsFixture(t, "")
	now := time.Now()
	add := func(model string, pt, ct, hit, miss int64, ok bool) {
		f.rec.Add(now, "cn", "u1", model, usage.Delta{
			PromptTokens: pt, HasPromptTokens: ok,
			CompletionTokens: ct, HasCompletion: ok,
			TotalTokens: pt + ct, HasTotal: ok,
			LatencyMs: 1000, HasLatency: true,
			CacheHit: hit, CacheMiss: miss, HasCache: ok,
		}, ok)
	}
	add("glm-5.2", 100, 50, 800, 200, true)
	add("glm-5.2", 10, 5, 90, 10, true)
	add("claude-4.6", 7, 3, 0, 0, true)

	_, body := f.getStats(t, "/v1/stats", "")
	models := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models=%d want 2（按模型分组）", len(models))
	}
	byName := map[string]map[string]any{}
	for _, raw := range models {
		m := raw.(map[string]any)
		byName[m["model"].(string)] = m
	}
	g, ok := byName["glm-5.2"]
	if !ok {
		t.Fatalf("缺 glm-5.2 分组：%v", byName)
	}
	if g["requests"] != float64(2) || g["prompt_tokens"] != float64(110) || g["completion_tokens"] != float64(55) {
		t.Errorf("glm-5.2 聚合错误：%v", g)
	}
	if g["cache_hit_tokens"] != float64(890) || g["cache_miss_tokens"] != float64(210) {
		t.Errorf("glm-5.2 缓存聚合错误：%v", g)
	}
	c := byName["claude-4.6"]
	if c["requests"] != float64(1) || c["prompt_tokens"] != float64(7) {
		t.Errorf("claude-4.6 聚合错误：%v", c)
	}
	tot := body["total"].(map[string]any)
	if tot["requests"] != float64(3) || tot["prompt_tokens"] != float64(117) {
		t.Errorf("total=%v want requests=3 prompt=117", tot)
	}
}

// 只读缓存快照：打 /v1/stats 不得触发任何上游请求。
func TestStatsEndpointDoesNotCallUpstream(t *testing.T) {
	f := newStatsFixture(t, "")
	f.rec.Add(time.Now(), "cn", "u1", "glm-5.2", usage.Delta{
		PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true,
	}, true)

	before := f.upCall.Load()
	for i := 0; i < 5; i++ {
		if code, _ := f.getStats(t, "/v1/stats", ""); code != http.StatusOK {
			t.Fatalf("code=%d", code)
		}
	}
	if after := f.upCall.Load(); after != before {
		t.Errorf("上游请求数从 %d 变成 %d：统计端点必须只读本地快照，不得触发上游调用", before, after)
	}

	// 对照组：/v1/models 缓存冷时会拉上游（证明计数探针有效，上面的 0 增量不是假阴性）。
	if code := f.serve(t, "/v1/models"); code != http.StatusOK {
		t.Fatalf("models code=%d", code)
	}
	if f.upCall.Load() == before {
		t.Error("对照失效：/v1/models 冷缓存应触发上游拉取（否则上游计数探针测不出东西）")
	}
}

func (f *statsFixture) serve(t *testing.T, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code
}

// 端到端采集：真实走一次流式 chat 请求，观测必须落进 /v1/stats。
func TestStatsEndpointCollectsFromStreamChat(t *testing.T) {
	var calls atomic.Int64
	const sseUsage = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":100,\"total_tokens\":150,\"prompt_cache_hit_tokens\":800,\"prompt_cache_miss_tokens\":200,\"credit\":0.02}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, sseUsage, true
	})
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
	})
	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", w.Code, w.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("上游调用=%d want 1", calls.Load())
	}

	// 统计端点读同一份记录：token 三段与缓存三段必须来自末帧 usage。
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest("GET", "/v1/stats", nil))
	if srec.Code != http.StatusOK {
		t.Fatalf("stats code=%d", srec.Code)
	}
	var snap struct {
		Models []struct {
			Model            string  `json:"model"`
			Requests         int64   `json:"requests"`
			Success          int64   `json:"success"`
			Streaming        int64   `json:"streaming"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			TotalTokens      int64   `json:"total_tokens"`
			CacheHitTokens   int64   `json:"cache_hit_tokens"`
			CacheMissTokens  int64   `json:"cache_miss_tokens"`
			CacheHitRate     float64 `json:"cache_hit_rate"`
			Credit           float64 `json:"credit"`
			AvgTTFBMS        float64 `json:"avg_ttfb_ms"`
		} `json:"models"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Models) != 1 {
		t.Fatalf("models=%d want 1: %s", len(snap.Models), srec.Body)
	}
	m := snap.Models[0]
	if m.Model != "glm-5.2" {
		t.Errorf("model=%q want glm-5.2", m.Model)
	}
	if m.Requests != 1 || m.Success != 1 || m.Streaming != 1 {
		t.Errorf("req/succ/stream=%d/%d/%d want 1/1/1", m.Requests, m.Success, m.Streaming)
	}
	if m.PromptTokens != 50 || m.CompletionTokens != 100 || m.TotalTokens != 150 {
		t.Errorf("tokens=%d/%d/%d want 50/100/150", m.PromptTokens, m.CompletionTokens, m.TotalTokens)
	}
	if m.CacheHitTokens != 800 || m.CacheMissTokens != 200 {
		t.Errorf("cache=%d/%d want 800/200（末帧 usage 的缓存三段必须采到）", m.CacheHitTokens, m.CacheMissTokens)
	}
	if m.CacheHitRate < 0.7999 || m.CacheHitRate > 0.8001 {
		t.Errorf("hit_rate=%.4f want 0.8", m.CacheHitRate)
	}
	if m.Credit != 0.02 {
		t.Errorf("credit=%v want 0.02（末帧 usage.credit 必须采到）", m.Credit)
	}
}

// 端到端采集：非流式路径的缓存三段与扣费同样落进统计。
//
// 注意出站恒为 SSE（网关强制 stream:true，见 README「流式行为细节」），故假上游
// 返回 SSE 帧；客户端侧不带 stream 字段 → 走 Aggregate 聚合路径（非流式响应）。
func TestStatsEndpointCollectsFromSyncChat(t *testing.T) {
	const sseUsage = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":20,\"total_tokens\":50,\"prompt_cache_hit_tokens\":400,\"prompt_cache_miss_tokens\":100,\"credit\":0.05}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseUsage, true })
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", w.Code, w.Body)
	}
	s := rec.Stats(0)
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.CacheHitTokens != 400 || m.CacheMissTokens != 100 {
		t.Errorf("cache=%d/%d want 400/100（非流式也要采缓存三段）", m.CacheHitTokens, m.CacheMissTokens)
	}
	if m.Credit < 0.0499 || m.Credit > 0.0501 {
		t.Errorf("credit=%.4f want 0.05", m.Credit)
	}
	if m.Streaming != 0 {
		t.Errorf("streaming=%d want 0", m.Streaming)
	}
	// 非流式没有首帧：不得产出首字观测（否则均值被 0 拉低）。
	if m.AvgTTFBMS != 0 {
		t.Errorf("avg_ttfb=%.2f want 0（非流式无首帧）", m.AvgTTFBMS)
	}
	if m.PromptTokens != 30 || m.CompletionTokens != 20 {
		t.Errorf("tokens=%d/%d want 30/20", m.PromptTokens, m.CompletionTokens)
	}
}

// 失败尝试也计入请求数（重试放大靠这一列才看得出来），且不伪造 token 观测。
func TestStatsEndpointCountsFailedAttempts(t *testing.T) {
	var calls atomic.Int64
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 429, `{"code":429,"msg":"rate limited"}`, false
	})
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if w.Code == http.StatusOK {
		t.Fatalf("上游 429 不该回 200：code=%d", w.Code)
	}
	s := rec.Stats(0)
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.Requests == 0 {
		t.Fatal("失败尝试必须计入请求数（否则重试放大在统计里看不见）")
	}
	if m.Failed != m.Requests || m.Success != 0 {
		t.Errorf("succ/fail=%d/%d want 0/%d", m.Success, m.Failed, m.Requests)
	}
	if m.PromptTokens != 0 || m.CompletionTokens != 0 || m.CacheHitTokens != 0 {
		t.Errorf("失败尝试无 usage：不得伪造 token/缓存观测，得到 %+v", m)
	}
}

// hours 参数：非法值不报错（按缺省全量），合法值生效。
func TestStatsEndpointHoursParam(t *testing.T) {
	f := newStatsFixture(t, "")
	now := time.Now()
	// 5 小时前的桶（hours=1 窗口外）+ 当前桶（窗口内）。
	f.rec.Add(now.Add(-5*time.Hour), "cn", "u1", "old", usage.Delta{
		PromptTokens: 999, HasPromptTokens: true, LatencyMs: 1, HasLatency: true,
	}, true)
	f.rec.Add(now, "cn", "u1", "new", usage.Delta{
		PromptTokens: 7, HasPromptTokens: true, LatencyMs: 1, HasLatency: true,
	}, true)

	for _, tc := range []struct {
		name     string
		path     string
		wantMods int
	}{
		{"缺省全量", "/v1/stats", 2},
		{"hours=1", "/v1/stats?hours=1", 1},
		{"非法值按缺省", "/v1/stats?hours=abc", 2},
		{"零按缺省", "/v1/stats?hours=0", 2},
		{"负数按缺省", "/v1/stats?hours=-5", 2},
		{"超上限被钳制", "/v1/stats?hours=999999999", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := f.getStats(t, tc.path, "")
			if code != http.StatusOK {
				t.Fatalf("code=%d want 200", code)
			}
			models := body["models"].([]any)
			if len(models) != tc.wantMods {
				t.Errorf("models=%d want %d", len(models), tc.wantMods)
			}
		})
	}
}

// 统计端点不落盘：反复打它不产生 usage.json（只读语义）。
func TestStatsEndpointDoesNotFlush(t *testing.T) {
	path := t.TempDir() + "/usage.json"
	rec := usage.New(path)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Usage:    rec,
	})
	for i := 0; i < 3; i++ {
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/stats", nil))
		if rec2.Code != http.StatusOK {
			t.Fatalf("code=%d", rec2.Code)
		}
	}
	// 只读端点不得触发 Save（面板的 usageSave 才是落盘入口）。
	if _, err := os.Stat(path); err == nil {
		t.Error("GET /v1/stats 不应写盘（统计端点是只读快照）")
	}
}
