// rateexhaust_test.go 末端 429 + Retry-After 端到端（v1.9.19，对齐上游 da22a92）。
//
// 上游 hub 的行为：模型被限流（6004）且池内确实没有可用账号时回 429 + Retry-After
// （OpenAI 生态客户端对 429 的退避更规范），而不是 503——503 通常被当服务端故障，
// 会触发不恰当的客户端重试/熔断。
//
// 本 fork 的收窄实现（关键：只在**模型级冷却耗尽**这个窄分支改状态码）：
//   - 池内所有候选都因**该模型**的冷却而出局 → 429 + Retry-After（最早恢复时刻）；
//   - 池真空 / 全禁用 / 账号级冷却 / 任一候选干净 → **保持 503 不变**（反向断言）；
//   - gateway_hint 保留（429 用限流 hint），上游原文仍逐字透传。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChat429OnModelCooldownExhaustion 核心正向：上游对唯一号回 429/6004（带重置
// 墙钟）→ 该模型在池内全部候选上冷却 → 末端 429 + Retry-After（正整数秒），
// 且 gateway_hint 仍是限流 hint、message 逐字含上游原文。
func TestChat429OnModelCooldownExhaustion(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	raw := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` + ts + ` UTC+8 重置","requestId":"r"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, raw, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("模型级冷却耗尽应 429，code=%d body=%s", rec.Code, rec.Body)
	}
	// Retry-After：HTTP 标准头，正整数秒，且与解析出的重置墙钟一致（约 30 分钟）。
	ra := rec.Header().Get("Retry-After")
	n, err := strconv.Atoi(ra)
	if err != nil || n < 1 {
		t.Fatalf("Retry-After=%q want 正整数秒", ra)
	}
	if want := int(30 * time.Minute / time.Second); n < want-5 || n > want+5 {
		t.Errorf("Retry-After=%d want ~%d（该模型最早恢复时刻距现在的秒数）", n, want)
	}
	// gateway_hint 保留（与上游直接 429 的 ErrSoftRate hint 同文案）。
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "model_rate_limited" {
		t.Errorf("error.code=%q want model_rate_limited", e.Error.Code)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v want rate-limit hint（429 也必须带）", e.Error.GatewayHint)
	}
	// 上游原文仍逐字透传（透传纪律优先，message 不被本地文案覆盖）。
	if !strings.Contains(e.Error.Message, raw) {
		t.Errorf("message must carry verbatim upstream body:\n got %q\nwant contains %q", e.Error.Message, raw)
	}
}

// TestChat429RetryAfterTakesEarliestRecovery 多账号同模型限流：Retry-After 取
// **最早**恢复时刻（客户端按最短可等待时间退避即可，不必等最慢的号）。
func TestChat429RetryAfterTakesEarliestRecovery(t *testing.T) {
	soon := time.Now().Add(10 * time.Minute)
	late := time.Now().Add(50 * time.Minute)
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast", AccessToken: "at-fast", ExpiresAt: 9999999999},
	)
	p.SetCredits("slow", 2000, 0) // 先被选中（积分更高），失败后换到 fast 再失败
	p.SetCredits("fast", 1000, 0)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		at := late
		if authz == "Bearer at-fast" {
			at = soon
		}
		return 429, `{"code":6004,"msg":"将在 ` + at.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05") + ` UTC+8 重置"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("两号同模型限流应 429，code=%d body=%s", rec.Code, rec.Body)
	}
	n, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After 非整数: %q", rec.Header().Get("Retry-After"))
	}
	if want := int(10 * time.Minute / time.Second); n < want-5 || n > want+5 {
		t.Errorf("Retry-After=%d want ~%d（取最早恢复时刻，不是 50m 的慢号）", n, want)
	}
}

// TestChat503OnEmptyPoolNot429 关键反向断言：**池真空**（一个号都没有）→ 仍 503
// no_healthy_account，绝不变 429（没有账号可退避，回 429 是假信号）。
func TestChat503OnEmptyPoolNot429(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, nil)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("空池应 503（不是 429），code=%d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("503 不得带 Retry-After（那是 429 的契约）: %q", ra)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "no_healthy_account" {
		t.Errorf("error.code=%q want no_healthy_account", e.Error.Code)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "no healthy account available in pool; check /status or retry later" {
		t.Errorf("gateway_hint=%v want no-healthy hint", e.Error.GatewayHint)
	}
}

// TestChat503OnAllAccountsDisabledNot429 池内号全禁用/临时停用 → 仍 503：
// 「池子本身没有可用号」不是「模型被限流」（禁用号选号器一律不考虑，
// 判定里已显式排除，不得因此谎报 429）。
func TestChat503OnAllAccountsDisabledNot429(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Disable("u1", "test")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, nil)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("全禁用池应 503（不是 429），code=%d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("503 不得带 Retry-After: %q", ra)
	}
}

// TestChat503OnAccountLevelCoolingNot429 账号级冷却（非模型级）→ 仍 503：
// 账号级冷却不写 modelCooldowns，不是「模型被限流」，回 429 会让客户端退避一个
// 不存在的模型窗口。
func TestChat503OnAccountLevelCoolingNot429(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		// 非 6004 的普通限流：applyErrorPolicy 走账号级 CooldownSoftRate。
		return 429, `{"code":11140,"msg":"rate limiting requests"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("账号级冷却应 503（不是 429），code=%d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("503 不得带 Retry-After: %q", ra)
	}
}

// TestChat503OnModelBlockedNot429 11102「该后端无此模型」负缓存（与 6004 共用
// modelCooldowns 承载）→ 仍 503：模型不存在不是限流，退避多久都不会变好
// （TTL 6h 起、封顶 24h），回 429 + Retry-After 会让客户端白等数小时。
func TestChat503OnModelBlockedNot429(t *testing.T) {
	const raw = `{"code":11102,"msg":"model [glm-4.6v] service info not found"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, raw, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-4.6v","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("11102 应 503（不是 429），code=%d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("503 不得带 Retry-After: %q", ra)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	// 原文透传 + 既有 hint 不变（本批只动 429 窄分支）。
	if !strings.HasSuffix(e.Error.Message, raw) {
		t.Errorf("message must carry verbatim upstream body: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "no such model") {
		t.Errorf("gateway_hint=%v want model-blocked hint", e.Error.GatewayHint)
	}
}

// TestChat429NoUpstreamBodyKeepsReadableMessage 选号阶段就无候选（一次上游都没打）
// 却判定为模型级限流耗尽时，message 用可读文案（既有本地文案在 429 语境下会被读成
// 服务端故障），同时 hint/Retry-After 齐备。
//
// 构造：先让唯一号撞 6004 进入模型级冷却，再发第二个请求——此时池内该模型已无候选，
// 轮转第一次选号就失败（lastErr == nil），走"无上游原文"分支。
func TestChat429NoUpstreamBodyKeepsReadableMessage(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 第一个请求：撞 6004，写入模型级冷却。
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec1.Code != http.StatusTooManyRequests {
		t.Fatalf("首个请求应 429，code=%d body=%s", rec1.Code, rec1.Body)
	}
	before := calls

	// 第二个请求：该模型在池内已无候选，选号即失败（不打上游）。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("模型冷却耗尽应 429，code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls != before {
		t.Errorf("第二次请求不应打上游（池内该模型无候选），calls=%d want %d", calls, before)
	}
	n, err := strconv.Atoi(rec2.Header().Get("Retry-After"))
	if err != nil || n < 1 {
		t.Fatalf("Retry-After=%q want 正整数秒", rec2.Header().Get("Retry-After"))
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec2.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	// 无上游原文可透传 → 可读文案（不谎称"服务端故障"），code/hint 与 429 一致。
	if e.Error.Code != "model_rate_limited" {
		t.Errorf("error.code=%q want model_rate_limited", e.Error.Code)
	}
	if !strings.Contains(e.Error.Message, "rate limited") {
		t.Errorf("message 应说明是模型限流: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v want rate-limit hint", e.Error.GatewayHint)
	}
}

// TestChat429OtherModelStillServed 切模型即可用（模型级限流的既有豁免语义不变）：
// 同池同账号换一个模型请求应正常 200，不会被上一个模型的 429 波及。
func TestChat429OtherModelStillServed(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("首个请求应 429，code=%d", rec.Code)
	}
	// 换模型：上游仍回 429（fake 不分模型），但**选号**必须放行该账号
	// （模型级冷却只锁 glm-5.3），故仍会打一次上游 —— 断言账号被重新选中。
	var served bool
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		served = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hy3-x","messages":[]}`)))
	if !served {
		t.Fatalf("换模型后该账号应重新可选（模型级冷却只锁被限的模型），code=%d body=%s", rec2.Code, rec2.Body)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("换模型应 200，code=%d body=%s", rec2.Code, rec2.Body)
	}
}
