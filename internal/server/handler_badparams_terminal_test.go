// handler_badparams_terminal_test.go 11101（bad_params）请求级化的端到端验收
// （吸收上游 PR #99）。
//
// 缺陷：ErrBadParams 被归「不罚账号但**仍轮转**」→ 坏请求按账号数放大：
//  1. 4 个并发坏请求 → 8 次上游请求（每个请求轮转 MaxRotate=2 次）；
//  2. 每次轮转前吃 rotateBackoff（500ms·2^i 封顶 8s）**占住 in-flight 名额**；
//  3. 末端落 503 → OpenAI 兼容客户端当瞬时故障**无限重试同一个坏 body**。
//
// 修法：与仓库为 11115 / 11135 确立的「请求级终态」哲学一致——零动作 + **终止轮转**
// + 400 透传原文。11133（model_param_invalid）**保留轮转**：它更可能是账号侧后端
// 差异（11102 的 (账号,模型) 负缓存证明同模型在不同账号上可用性不同），分野理由
// 见 upstream.Classify 与 applyErrorPolicy 的 ErrBadParams 分支注释。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// body11101User 用户实测形态的 11101 原文（含 requestId，透传时必须逐字保留）。
const body11101User = `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF",` +
	`"requestId":"7f3d2c1b-0000-4a4b-8c9d-1e2f3a4b5c6d"}`

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + 11101 → 请求级错误：
// 不罚账号（无冷却/无禁用/无熔断计数/无 errTotal/无连败），**且不轮转**——
// 同一 body 换号必然同样失败。端到端断言只打一次上游、直接回 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, body11101User, false
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
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal、无连败。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatBadParams400CarriesUpstreamBody 11101 的 400 响应必须包含上游原始
// 11101 信息（含 requestId，客户端据此排查），且不再出现空洞的 no_healthy_account。
func TestChatBadParams400CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11101User, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}
	if !strings.Contains(body, "7f3d2c1b-0000-4a4b-8c9d-1e2f3a4b5c6d") {
		t.Errorf("400 message should carry upstream requestId (逐字透传原文): %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
}

// TestChatModelParamInvalidStillRotates 11133（model_param_invalid）**仍轮转**
// （PR #99 也保留）：它可能是账号侧后端差异（11102 的 (账号,模型) 负缓存证明同模型
// 在不同账号上可用性不同），轮转仍有价值；但绝不喂连败（轮转 N 号 = N 次计数）。
//
// 与 11101 的分野：11101 发生在上游**解析请求体**阶段（还没走到模型路由），
// 「不同账号可能有不同模型权限」是 11102 的理由而不是 11101 的。
func TestChatModelParamInvalidStillRotates(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":11133,"msg":"Invalid request parameters"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (11133 应换号重试并成功)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次（11133 保留轮转）", calls)
	}
	// 11133 同样不喂连败（轮转 N 号 = N 次计数会把整池推向降权）。
	st, _ := p.Status("bad")
	if st.ConsecutiveFails != 0 || st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("11133 不得罚号: %+v", st)
	}
}

// body11133 用户实测形态的 11133 原文（extError=model_param_invalid）。
const body11133 = `{"code":11133,"msg":"Invalid request parameters","requestId":"r1",` +
	`"extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider"}}`

// TestChatModelParamInvalidSameFingerprintEarlyExit 两个不同账号回**同一** 11133
// 业务码 → 参数拒绝被证明是请求级终态（账号侧后端真有差异时应回不同结果）：
// 第三个账号不再打（不把必败请求放大 MaxRotate 倍），直接 400 透传原文，
// 且不再是 503「all accounts unavailable」。不罚任何账号。
func TestChatModelParamInvalidSameFingerprintEarlyExit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, body11133, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at-a3", ExpiresAt: 9999999999},
	)
	p.SetCredits("a1", 3000, 0)
	p.SetCredits("a2", 2000, 0)
	p.SetCredits("a3", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400 bad_params, not 503)", rec.Code, rec.Body)
	}
	total := 0
	for _, n := range calls {
		total += n
	}
	if total != 2 {
		t.Errorf("upstream calls=%d want 2（第二个号同指纹即终态，不再放大）", total)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11133") || !strings.Contains(body, "model_param_invalid") {
		t.Errorf("400 must passthrough upstream body: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") || strings.Contains(body, "all accounts unavailable") {
		t.Errorf("request-level 400 must not masquerade as pool exhaustion: %s", body)
	}
	for _, uid := range []string{"a1", "a2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
			t.Errorf("11133 must not penalize account %s: %+v", uid, st)
		}
	}
}

// TestChatModelParamInvalidTerminal400 单号池 11133：无同指纹可撞、轮转耗尽
// → 末端同样回 400 透传（paramsTerminal 分支），不再 503 误导为池子空了。
func TestChatModelParamInvalidTerminal400(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, body11133, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400 bad_params terminal, not 503)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "bad_params") || !strings.Contains(body, "11133") {
		t.Errorf("terminal must be bad_params + upstream body: %s", body)
	}
	if strings.Contains(body, "all accounts unavailable") {
		t.Errorf("11133 terminal must not say pool exhausted: %s", body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("11133 must not penalize account: %+v", st)
	}
}
