package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatModelBlockedNegativeCacheEndToEnd 端到端：上游 404 + 11102「该后端无此模型」
// → 该 (账号, 模型) 进负缓存避让（modelCooldowns 台账可见），账号级**不**冷却
// （切模型即可用），轮转到 good 号后请求成功。
func TestChatModelBlockedNegativeCacheEndToEnd(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 404, `{"code":11102,"msg":"model [glm-5.2] service info not found"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Errorf("11102 是模型级负缓存，账号级不应冷却: %+v", st)
	}
	if st.Disabled {
		t.Errorf("11102 不得禁用账号: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.2" {
		t.Fatalf("want single model ledger row glm-5.2: %+v", st.RateLimitedModels)
	}
	if !strings.HasPrefix(st.RateLimitedModels[0].Reason, "11102") {
		t.Errorf("reason=%q 应以 11102 开头（供 BlockModelClear 前缀识别）", st.RateLimitedModels[0].Reason)
	}
	// 选号避开：同模型请求不再选 bad；切模型仍可选中 bad（豁免保持）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "good" {
		t.Fatalf("同模型应跳过 bad（负缓存避让），got %+v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "bad" {
		t.Fatalf("切模型应豁免 bad，got %+v", got)
	}
}

// TestChatModelBlockedClearOnSuccess 成功清命：账号带 11102 负缓存条目 + 处于短软冷却
// （半开探测窗口）时，全冷却兜底仍会选中它；该模型请求一旦成功 → 条目立即清除
// （不必等 TTL），账号回到可选。
func TestChatModelBlockedClearOnSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.BlockModelBackoff("u1", "glm-5.2", upstream.ModelBlockReason)
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
		t.Fatalf("预置负缓存后该模型应无健康候选，got %+v", got)
	}
	// 短软冷却（非 CoolHard）让账号进入「全冷却兜底」可选中集合：模拟负缓存半开探测
	// ——模型条目仍在，但账号被兜底路径选中并实测成功。
	p.Cooldown("u1", pool.CoolSoft, 2*time.Second, "half-open probe")
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 成功路径 BlockModelClear：11102 条目被清（选号恢复可选）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "u1" {
		t.Fatalf("成功后 11102 条目应清除（该模型恢复可选），got %+v", got)
	}
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("成功后模型级台账应为空，got %+v", st.RateLimitedModels)
	}
}

// TestApplyErrorPolicyModelBlockedNoAccountCooldown handler 层：ErrModelBlocked 只写
// 模型级负缓存，不产生账号级冷却、不喂熔断、不禁用。
func TestApplyErrorPolicyModelBlockedNoAccountCooldown(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})
	h.applyErrorPolicy("u1", upstream.ErrModelBlocked, `{"code":11102}`, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.BreakerFails != 0 {
		t.Fatalf("ErrModelBlocked 不得冷却/禁用/喂熔断: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.2" {
		t.Fatalf("应写入模型级负缓存台账: %+v", st.RateLimitedModels)
	}
}
