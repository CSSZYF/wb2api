package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestApplyErrorPolicyErrClientFeedsConsecutiveFails ErrClient（default 分支，
// 未知 4xx）喂连败计数（issue #114）：单次不罚（无冷却/熔断），达阈降权出池。
// 修复前该形态只换号不罚，坏号留在池内被反复选中——现在有连败兜底。
func TestApplyErrorPolicyErrClientFeedsConsecutiveFails(t *testing.T) {
	p := pool.New("")
	p.SetDegrade(3, 10*time.Minute, 2*time.Hour) // 阈 3 便于测试
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})

	// 前阈-1 次：只计数，不冷却不熔断。
	for n := 0; n < 2; n++ {
		ue := &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: `{"code":1,"msg":"unknown business error"}`}
		h.applyErrorPolicy("u1", upstream.ErrClient, `{"code":1,"msg":"unknown business error"}`, "", ue)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.BreakerFails != 0 {
		t.Fatalf("ErrClient 前阈-1 次不应有任何惩罚: %+v", st)
	}
	if st.ConsecutiveFails != 2 {
		t.Fatalf("ErrClient 应喂连败计数, consecutive_fails=%d want 2", st.ConsecutiveFails)
	}
	// 达阈第 3 次：降权（Cooling 呈 degrade 形态），仍不熔断不禁用。
	ue := &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: `x`}
	h.applyErrorPolicy("u1", upstream.ErrClient, `x`, "", ue)
	st, _ = p.Status("u1")
	if !st.Cooling || st.Reason != "consecutive failures" || st.CoolKind != "degrade" {
		t.Fatalf("达阈应触发降权: %+v", st)
	}
	if st.Disabled || st.BreakerFails != 0 {
		t.Fatalf("连败降权不叠加禁用/熔断: %+v", st)
	}
}

// TestApplyErrorPolicyErrNoneNotFed ErrNone 走 default 分支（防御路径）不喂连败。
func TestApplyErrorPolicyErrNoneNotFed(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrNone, "", "", nil)
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("ErrNone 不应喂连败, got %d", st.ConsecutiveFails)
	}
}

// TestApplyErrorPolicyClassifiedErrorsNotFed 带权威分类的错误不喂连败（不重复计罚）：
// ErrServer（熔断）与 ErrSoftRate（冷却）各走各自惩罚，consecutive_fails 恒 0。
func TestApplyErrorPolicyClassifiedErrorsNotFed(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})

	h.applyErrorPolicy("u1", upstream.ErrServer, "boom", "", &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"})
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, "rate limit", "", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "rate limit"})
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("权威分类错误不应喂连败（惩罚已存在，不重复计罚）, got %d", st.ConsecutiveFails)
	}
	if st.BreakerFails != 1 {
		t.Fatalf("ErrServer 应喂熔断, fails=%d", st.BreakerFails)
	}
}

// TestApplyErrorPolicyWafAndNotFoundNotFed 其余带权威分类的错误同样不喂连败：
// WAF 403（软冷却）/404（短冷却）/402（硬冷却）/11102（负缓存）都不该重复计罚。
func TestApplyErrorPolicyWafAndNotFoundNotFed(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})

	cases := []struct {
		kind upstream.ErrKind
		body string
	}{
		{upstream.ErrWafBlock, "waf"},
		{upstream.ErrNotFound, "not found"},
		{upstream.ErrHardCredit, "余额不足"},
		{upstream.ErrModelBlocked, `{"code":11102,"msg":"service info not found"}`},
		{upstream.ErrContentBlocked, "content blocked"},
		{upstream.ErrBadParams, "Unmarshal chat params failed"},
		{upstream.ErrPromptTooLong, "prompt is too long"},
	}
	for _, c := range cases {
		h.applyErrorPolicy("u1", c.kind, c.body, "glm-5.2", &upstream.Error{Kind: c.kind, Status: 400, Msg: c.body})
	}
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("带权威分类的错误均不应喂连败, got %d", st.ConsecutiveFails)
	}
}

// TestChatTransportErrorFeedsConsecutiveFailures 传输层失败（连不上上游）喂连败
// （issue #114 端到端）：N 连败后该号出池（AvailableUIDs 不再含它），仍不熔断。
// fake transport 返回 error（非 *upstream.Error）→ handler 网络抖动分支。
func TestChatTransportErrorFeedsConsecutiveFailures(t *testing.T) {
	var calls int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("dial tcp: connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := pool.New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour) // 阈 2：两轮请求即降权
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	for round := 0; round < 2; round++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 503 {
			t.Fatalf("round %d: transport error 应回 503, got %d", round, rec.Code)
		}
	}
	if calls == 0 {
		t.Fatal("precondition: fake transport 应被调用")
	}
	// 两轮（每轮 1 次传输失败）后：连败=2 已达阈 → 降权出池。
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("连败达阈后账号应出池, still available: %v", uids)
	}
	st, _ := p.Status("u1")
	if st.CoolKind != "degrade" || st.Reason != "consecutive failures" {
		t.Fatalf("降权态应呈 degrade: %+v", st)
	}
	if st.BreakerFails != 0 {
		t.Fatalf("传输层错误不喂熔断（既有语义不回归）, fails=%d", st.BreakerFails)
	}
}

// TestChatErrClientDegradedNotPicked ErrClient 连败出池端到端：降权号不被
// normal 选号选中（换其他号继续服务），请求仍成功。
//
// 确定性：两号池下加权抽签可能先选中健康号，降权号吃不到 ErrClient。故分两段——
// 先只放 bad 并让它吃满阈值（单号池轮转 1 次即 503），再放 good 验证请求落到 good。
func TestChatErrClientDegradedNotPicked(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			// 未知业务 4xx：ErrClient（default 只换号不罚的形态）。
			return 400, `{"code":60001,"msg":"unknown business error"}`, false
		}
		return 200, sseOK, true
	})
	p := pool.New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour) // 阈 2
	p.Add(&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999})
	p.SetCredits("bad", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 预喂 1 次，再让真实请求吃最后一次 ErrClient（单号池 → 请求以 503 收尾）。
	p.NoteFailures("bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 503 {
		t.Fatalf("单号池全失败应回 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("bad")
	if st.CoolKind != "degrade" || st.Reason != "consecutive failures" {
		t.Fatalf("bad 应已被降权: %+v", st)
	}
	if st.BreakerFails != 0 || st.Disabled {
		t.Fatalf("ErrClient 连败不应叠加熔断/禁用: %+v", st)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("降权号不应在可用列表: %v", uids)
	}

	// 放入健康号：后续请求应落到 good 并成功（降权号不被 normal 选号选中）。
	p.Add(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	p.SetCredits("good", 1000, 1000)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != 200 {
		t.Fatalf("有健康号时请求应成功, got %d body=%s", rec2.Code, rec2.Body.String())
	}
	if st, _ := p.Status("good"); st.SuccessCount == 0 {
		t.Error("请求应落到健康号 good")
	}
	if st, _ := p.Status("bad"); st.SuccessCount != 0 {
		t.Errorf("降权号 bad 不应被选中, success=%d", st.SuccessCount)
	}
}

// TestChatDegradedAccountBackInPoolAfterSuccess 降权号经兜底被选中并成功后立即回池
// （成功即回池，issue #114 端到端）：降权截止被清、可用列表恢复。
func TestChatDegradedAccountBackInPoolAfterSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := pool.New("")
	p.SetDegrade(1, time.Hour, 2*time.Hour) // 阈 1：一次失败即降权
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 1000)
	p.NoteFailures("u1") // 降权
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("precondition: 应已出池, got %v", uids)
	}
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 单号池全冷却 → Pick 走兜底选中降权号 → 请求成功 → NoteSuccess 清降权。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("兜底选中后请求应成功, got %d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Cooling || !st.DegradeUntil.IsZero() || st.ConsecutiveFails != 0 {
		t.Fatalf("成功后应清降权回池: %+v", st)
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "u1" {
		t.Fatalf("成功后账号应回到可用列表, got %v", uids)
	}
}

// TestChatDegradeThresholdHugeNoEffect 反向断言（端到端）：阈值设成极大 →
// 连续 ErrClient 失败不触发降权，账号始终留在池内（证明配置真的被用上）。
func TestChatDegradeThresholdHugeNoEffect(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":60001,"msg":"unknown business error"}`, false
	})
	p := pool.New("")
	p.SetDegrade(1_000_000, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	for round := 0; round < 5; round++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 503 {
			t.Fatalf("round %d: 全失败应回 503, got %d", round, rec.Code)
		}
	}
	st, _ := p.Status("u1")
	if st.Cooling || !st.DegradeUntil.IsZero() {
		t.Fatalf("阈值极大时不应降权: %+v", st)
	}
	if st.ConsecutiveFails == 0 {
		t.Fatal("计数应如实累计（证明喂入路径通）")
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 {
		t.Fatalf("未达阈账号应留在池内, got %v", uids)
	}
}
