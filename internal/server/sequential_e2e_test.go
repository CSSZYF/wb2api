// sequential_e2e_test.go 顺序填充式选号在 handler 轮转循环里的端到端行为
// （需求 3/4/5 的联合验收）：把 pool 顺序模式与 429 不退避放在一起跑，
// 证明用户要的完整链路成立。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// seqPool 顺序模式池（含 realm 中性账号），供本文件用例使用。
func seqPool(uids ...string) *pool.Pool {
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 0)
	}
	p.SetOrder(uids)
	p.SetPickMode(pool.PickSequential)
	return p
}

// TestSequentialRotatesToNextAccountOn429 需求 3/4/5 联合：
// 顺序第一的号回 429（无恢复时间 → 有界退避冷却），handler 必须**立即**切到
// 顺序第二个号（不退避），第二个号成功 → 客户端拿到 200。
// 断言：请求总耗时 < 一次退避基数（证 429 未退避）+ 首个号被冷却 + 成功号是第二个。
func TestSequentialRotatesToNextAccountOn429(t *testing.T) {
	withRotateBackoff(t, 300*time.Millisecond)
	calls := []string{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls = append(calls, authz)
		if authz == "Bearer at-a" {
			// 无重置文案的 429 → ErrSoftRate + 有界退避冷却（需求 3 的「用完」形态之一）。
			return 429, `{"code":6004,"msg":"rate limited, please retry later"}`, false
		}
		return 200, sseOK, true
	})
	p := seqPool("a", "b")
	h := NewHandler(Config{Pool: p, Upstream: up})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("a 429 后应切到 b 并成功，code=%d body=%s", rec.Code, rec.Body)
	}
	if len(calls) != 2 || calls[0] != "Bearer at-a" || calls[1] != "Bearer at-b" {
		t.Fatalf("调用序列=%v want [at-a at-b]（顺序第一→第二，不回头）", calls)
	}
	if elapsed >= 300*time.Millisecond {
		t.Errorf("429 切换不得退避：elapsed=%v ≥ base 300ms", elapsed)
	}
	// a 被冷却（"用完"语义）：下一次选号必须直接落到 b。
	st, _ := p.Status("a")
	if !st.Cooling {
		t.Errorf("429 的号应进入冷却（until 在未来）, status=%+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Errorf("a 冷却后应首选 b, got %v", got)
	}
}

// TestSequentialSecondAccountUsedWhenFirstInFlightFull 需求 2 的端到端形态：
// 顺序第一的号在途占满（并发上限 1）→ handler 选号落到第二个号。
func TestSequentialSecondAccountUsedWhenFirstInFlightFull(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := seqPool("a", "b")
	p.SetMaxInFlight(1)
	if !p.Acquire("a") { // 模拟一个在途请求占住 a
		t.Fatal("warm up a")
	}
	defer p.Release("a")
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// b 是本次请求实际使用的号（成功号会被记为 sticky，但这里直接看在途计数更硬：
	// a 在途恒为 1，说明 handler 没有在 a 上再占名额）。
	stA, _ := p.Status("a")
	if stA.InFlight != 1 {
		t.Errorf("a 在途=%d want 1（handler 不得在满额号上再占名额）", stA.InFlight)
	}
	stB, _ := p.Status("b")
	if stB.InFlight != 0 {
		t.Errorf("b 在途=%d want 0（请求结束应释放）", stB.InFlight)
	}
}

// TestSequentialPrefersFirstAfterInFlightReturns 需求 2 的「回归」端到端形态：
// a 满时用 b；a 释放后，下一个请求回到 a（顺序靠前优先，不是轮转）。
func TestSequentialPrefersFirstAfterInFlightReturns(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := seqPool("a", "b")
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})
	send := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	_ = send()
	// 用选号器直接观察（handler 内的选号不可从响应体反推，且会话粘性会干扰）：
	if !p.Acquire("a") {
		t.Fatal("warm up a")
	}
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("a 满 → want b, got %v", got)
	}
	p.Release("a")
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("a 释放后 → want a（回归首选）, got %v", got)
	}
}
