package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestStatsCreditsGlobalPrefersEnterprise 价格优先级的**展示侧**回归：
// /v1/stats 的 credits 必须反映企业端点（计费目录）的价，而不是 v3/config 的牌价。
//
// 实测（2026-10-02）：hy4-preview 企业端点 credits=x0.00（折后实扣口径，与该端点
// modelPromotions 的 discountedCredits "0x" 一致），v3-CLI credits=x0.29（牌价）。
// 重写前面板显示 x0.29（v3 覆盖），用户看到"限时免费却在扣费"。
//
// 数据源：GlobalModelInfosSnapshot 的倍率旁表——本用例走完整链路（探测 → 旁表 →
// /v1/stats enrich），而不是直接塞缓存，这样价格优先级一旦被改回 v3 覆盖，本用例即红。
func TestStatsCreditsGlobalPrefersEnterprise(t *testing.T) {
	withGlobalEnabled(t)

	const cliUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/enterprises/personal/models":
			// 计费目录：hy4-preview x0.00（折后）；**没有** deepseek-v4.1-flash-sg。
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00 credits"}
			],"agents":[{"name":"cli","models":["hy4-preview","hy3"]}]}}`))
		case "/v3/config":
			if r.Header.Get("User-Agent") == cliUA {
				// 客户端配置：同名模型是牌价 x0.29；另有一条企业端点没有的 id。
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
					{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"},
					{"id":"deepseek-v4.1-flash-sg","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03"}
				]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
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

	// 先预热目录（触发探测 → 倍率旁表落缓存）。
	byID := modelsByID(h.modelList())
	for _, id := range []string{"hy4-preview", "hy3", "deepseek-v4.1-flash-sg"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("预热失败：modelList 缺 %s，实际 ids=%v", id, keysOfAny(byID))
		}
	}
	now := time.Now()
	for _, m := range []string{"global:hy4-preview", "global:hy3", "global:deepseek-v4.1-flash-sg"} {
		rec.Add(now, "global", "g1", m, usage.Delta{
			PromptTokens: 1, HasPromptTokens: true, LatencyMs: 1, HasLatency: true,
		}, true)
	}

	req := httptest.NewRequest("GET", "/v1/stats", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	// 企业端点权威：hy4-preview 必须是 x0.00，不是 v3 的牌价 x0.29。
	if got := modelRow(t, body, "global:hy4-preview")["credits"]; got != "x0.00" {
		t.Errorf("hy4-preview credits=%v want x0.00（企业端点=计费目录，v3 的 x0.29 是牌价不得覆盖）", got)
	}
	// 企业端点带 " credits" 尾巴的形态必须归一（否则前端显示 "x0.00 credits"）。
	if got := modelRow(t, body, "global:hy3")["credits"]; got != "x0.00" {
		t.Errorf("hy3 credits=%v want x0.00（去 ' credits' 尾巴）", got)
	}
	// 企业端点没给的 id：v3 是唯一来源 → 必须透出（否则该模型倍率永远未知，
	// 保留积分的免费判定也拿不到依据）。
	if got := modelRow(t, body, "global:deepseek-v4.1-flash-sg")["credits"]; got != "x0.03" {
		t.Errorf("deepseek-v4.1-flash-sg credits=%v want x0.03（企业端点无此 id → 取 v3）", got)
	}
}
