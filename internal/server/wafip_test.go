package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// withWafIPWindow 临时收紧 IP 级判定窗（默认 60s；测试用短窗快速验证过期解除）。
func withWafIPWindow(t *testing.T, d time.Duration) {
	t.Helper()
	old := wafIPWindow
	wafIPWindow = d
	t.Cleanup(func() { wafIPWindow = old })
}

// TestWafIPGateTriggersOnTwoUIDs 短窗内两个不同 UID 命中 WAF 403 → 激活；
// 单号反复命中永不触发（判定口径是「不同号数」）。
func TestWafIPGateTriggersOnTwoUIDs(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	var g wafIPGate
	if g.noteWaf("u1") {
		t.Fatal("single uid must not activate IP-level gate")
	}
	for i := 0; i < 5; i++ {
		if g.noteWaf("u1") {
			t.Fatal("same uid repeated hits must never activate (only distinct uids counted)")
		}
	}
	if !g.noteWaf("u2") {
		t.Fatal("second distinct uid within window must activate")
	}
	if !g.active() {
		t.Fatal("gate must be active after activation")
	}
	// 激活期内新命中仍返回 true（不续期）。
	if !g.noteWaf("u3") {
		t.Fatal("hit during active window must report active")
	}
}

// TestWafIPGateWindowExpires 窗口过期自然解除（激活不续期）。
func TestWafIPGateWindowExpires(t *testing.T) {
	withWafIPWindow(t, 40*time.Millisecond)
	var g wafIPGate
	if g.noteWaf("u1") {
		t.Fatal("first distinct uid must not activate")
	}
	if !g.noteWaf("u2") {
		t.Fatal("activation should happen on second distinct uid")
	}
	if !g.active() {
		t.Fatal("must be active right after trigger")
	}
	time.Sleep(60 * time.Millisecond)
	if g.active() {
		t.Fatal("gate must expire after window (no renewal)")
	}
	// 过期后需全新命中重新判定（旧账已清）。
	if g.noteWaf("u3") {
		t.Fatal("post-expiry single hit must not re-activate")
	}
}

// TestWafIPGateOldHitsPruned 窗外的旧命中被剪枝：u1 命中后等到窗外，
// u2 单独命中不得激活（旧账不计入）。
func TestWafIPGateOldHitsPruned(t *testing.T) {
	withWafIPWindow(t, 40*time.Millisecond)
	var g wafIPGate
	g.noteWaf("u1")
	time.Sleep(60 * time.Millisecond)
	if g.noteWaf("u2") {
		t.Fatal("stale hit outside window must be pruned (no activation)")
	}
}

// TestChatWafIPFailFastStopsRotation 端到端：两账号池，第一轮 u1 命中 WAF 403
// （IP 门未激活，轮转继续），第二轮 u2 命中 → 门激活，轮转立即终止（不再打第三个号）。
// 断言上游调用数 = 2（MaxRotate 默认 3），末端 code=waf_ip_blocked。
func TestChatWafIPFailFastStopsRotation(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 403, "", false // 所有号都被 WAF 拦（IP 级）
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 3000, 0)
	p.SetCredits("u2", 2000, 0)
	p.SetCredits("u3", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}
	// 阈值命中那次请求即止损：只打 2 个号（放大倍数=1），不打第三个。
	if calls != 2 {
		t.Errorf("calls=%d want 2 (fail-fast stops rotation at threshold)", calls)
	}
	if !strings.Contains(rec.Body.String(), "waf_ip_blocked") {
		t.Errorf("terminal error must carry waf_ip_blocked code: %s", rec.Body)
	}
	// 账号级软冷却照常记账（IP 级状态只改「是否继续轮转」，协同不叠加）。
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if !st.Cooling {
			t.Errorf("%s must still be soft-cooled (account-level accounting unaffected): %+v", uid, st)
		}
	}
}

// TestChatWafSingleUIDNoFailFast 单账号池行为不劣化：唯一号 WAF 403 时 IP 门永不
// 激活（只有一个 UID），轮转照常进行（MaxRotate 次重试同一号），末端文案为通用
// no_healthy_account（不是 IP 级措辞）。
func TestChatWafSingleUIDNoFailFast(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 403, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	// 单号池：账号级冷却生效，第二轮起选号会跳过冷却号（全冷却兜底才可能再选中），
	// 关键断言是「IP 门未激活」→ 文案不是 waf_ip_blocked。
	if strings.Contains(rec.Body.String(), "waf_ip_blocked") {
		t.Errorf("single-uid pool must not report IP-level block: %s", rec.Body)
	}
	if calls == 0 {
		t.Error("must have attempted upstream at least once")
	}
}

// TestChatWafIPFailFastPassesThroughUpstreamBody 透传语义回归：IP 级 fail-fast
// 终止路径有上游原文时原文优先（本地文案只作前缀说明）。
func TestChatWafIPFailFastPassesThroughUpstreamBody(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "Forbidden: request blocked by WAF", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 2000, 0)
	p.SetCredits("u2", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "waf_ip_blocked") {
		t.Fatalf("want waf_ip_blocked code: %s", body)
	}
	if !strings.Contains(body, "Forbidden: request blocked by WAF") {
		t.Errorf("upstream 原文 must be preserved on fail-fast path: %s", body)
	}
}

// TestWafIPGateActiveNoRenewal 激活期内不续期：激活后多次命中不延长 until。
func TestWafIPGateActiveNoRenewal(t *testing.T) {
	withWafIPWindow(t, 50*time.Millisecond)
	var g wafIPGate
	g.noteWaf("u1")
	g.noteWaf("u2") // 激活
	g.mu.Lock()
	until := g.until
	g.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	g.noteWaf("u3") // 激活期内命中：不续期
	g.mu.Lock()
	until2 := g.until
	g.mu.Unlock()
	if !until.Equal(until2) {
		t.Fatalf("active window must not be renewed: %v -> %v", until, until2)
	}
}

// TestChatWafIPGateDoesNotAffectOtherErrors IP 门只对 ErrWafBlock 生效：
// 5xx 等错误即使池内多号失败也不触发 fail-fast（走正常轮转 + 熔断计数）。
func TestChatWafIPGateDoesNotAffectOtherErrors(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 500, `{"code":1,"msg":"internal error"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if calls != 3 {
		t.Errorf("5xx must rotate through all accounts (no waf fail-fast), calls=%d", calls)
	}
	if h.wafIP.active() {
		t.Error("IP gate must not activate on non-WAF errors")
	}
}

// TestWafIPGateConcurrent 并发安全（-race 下有效）：多 goroutine 同时喂 WAF 命中，
// 门状态不崩、最终激活。
func TestWafIPGateConcurrent(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	var g wafIPGate
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for k := 0; k < 20; k++ {
				g.noteWaf(string(rune('a' + n)))
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if !g.active() {
		t.Fatal("concurrent distinct uids must activate the gate")
	}
}
