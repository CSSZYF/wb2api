// realm_fallback_test.go 跨域回落（realm fallback，issue #199c）的 handler 级端到端。
//
// 复现现场（Chen，本地 v1.9.11 混合池 1 global + 1 cn、realm_precedence=global）：
//
//	裸名 deepseek-v4.1-flash → 503（只试了 global 号，429/6004 后无候选；CN 号从未被尝试）
//	cn:deepseek-v4.1-flash   → 200（CN 号完全可用）
//
// 期望：裸名（网关默认倾向 global）在本域全部不可用时回落 CN 成功；显式前缀是用户
// 强指定，**不**回落（换域可能违反其意图）。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// realmCall 记录一次上游调用的账号与目标 host（host 用于确认域路由正确）。
type realmCall struct {
	auth string
	host string
}

// newRealmUpstream 构造 realm 感知的 fake upstream：按 Authorization 决定行为，
// 并记录每次调用的账号与 host。GlobalEnabled=true + ChatBaseGlobal 非空，使 global
// 账号真的走 global base（house 与 CN 分离，可断言跨域路由）。
func newRealmUpstream(t *testing.T, behavior func(authz string) (int, string, bool)) (*upstream.Client, *[]realmCall) {
	t.Helper()
	var mu sync.Mutex
	calls := &[]realmCall{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			mu.Lock()
			*calls = append(*calls, realmCall{auth: authz, host: r.URL.Host})
			mu.Unlock()
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:     "https://fake-cn.example",
		BillingBaseCN:  "https://fake-cn.example",
		ChatBaseGlobal: "https://fake-global.example",
		GlobalEnabled:  true,
	}
	return up, calls
}

// modelRateLimit429 上游 429 + code 6004 模型级限流的真实形态（英文文案带重置墙钟）。
// 带重置时间 → CooldownSoftForModel 只写 modelCooldowns[model]、账号级仍 healthy，
// 于是"本域无候选"只体现在 healthyForModel 之外——正是需要回落兜住的形态。
func modelRateLimit429() (int, string, bool) {
	return 429, `{"code":6004,"msg":"model usage reached the limit, your usage will reset at 2099-01-01 00:00:00 UTC+8, alternatively, you can switch to the other models to continue using it."}`, false
}

// realmMixedPool 混合池：1 个 global 号 + 1 个 cn 号（Chen 报告的确切现场）。
func realmMixedPool(t *testing.T) *pool.Pool {
	t.Helper()
	withGlobalEnabled(t)
	p := testPoolWith(
		&auth.Auth{UID: "bebb8506", Domain: "www.workbuddy.ai", AccessToken: "at-global", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c28ceb89", Domain: "www.codebuddy.cn", AccessToken: "at-cn", ExpiresAt: 9999999999},
	)
	return p
}

// TestChatRealmFallbackBareNameGlobalToCN Chen 场景的正面用例：裸名归属 global，
// global 号 429/6004 后第二轮选号在本域（global）已无候选 → 回落 cn 成功，最终 200。
func TestChatRealmFallbackBareNameGlobalToCN(t *testing.T) {
	calls := map[string]int{}
	up, seq := newRealmUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-global" {
			return modelRateLimit429()
		}
		return 200, sseOK, true
	})
	p := realmMixedPool(t)
	// global 号积分更高 → 第一轮确定性地先选它（模拟"优先走国际"）。
	p.SetCredits("bebb8506", 5000, 0)

	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		GlobalEnabled: true, RealmPrecedence: "global",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))

	if rec.Code != 200 {
		t.Fatalf("裸名 deepseek-v4.1-flash：code=%d body=%s（期望回落 CN 后 200）", rec.Code, rec.Body)
	}
	if calls["Bearer at-global"] != 1 || calls["Bearer at-cn"] != 1 {
		t.Fatalf("上游调用序列应 global 先试 1 次、回落 cn 1 次，实际 calls=%v", calls)
	}
	got := *seq
	if len(got) != 2 || got[0].auth != "Bearer at-global" || got[1].auth != "Bearer at-cn" {
		t.Fatalf("调用顺序应 [global, cn]，实际 %+v", got)
	}
	// 跨域路由正确：global 号打 global base，cn 号打 cn base（不是把 cn 号送去国际站）。
	if got[0].host != "fake-global.example" || got[1].host != "fake-cn.example" {
		t.Fatalf("跨域 host 错配：%+v", got)
	}
	// global 号被 6004 模型级限流（账号级仍健康），cn 号无冷却且成功。
	stG, _ := p.Status("bebb8506")
	if len(stG.RateLimitedModels) != 1 || stG.RateLimitedModels[0].Model != "deepseek-v4.1-flash" {
		t.Errorf("global 号应记 deepseek-v4.1-flash 模型级限流台账：%+v", stG.RateLimitedModels)
	}
	stC, _ := p.Status("c28ceb89")
	if stC.Cooling || len(stC.RateLimitedModels) != 0 {
		t.Errorf("cn 号不应被波及：%+v", stC)
	}
	stC2, _ := p.Status("c28ceb89")
	if stC2.SuccessCount == 0 {
		t.Errorf("cn 号应有成功计数（回落确实调了它）")
	}
}

// TestChatRealmFallbackExplicitPrefixDoesNotFallback 显式 "global:" 前缀是用户强指定：
// global 号限流后**不**回落 CN，直接 503（cn 号零调用）——换域会违背用户意图。
func TestChatRealmFallbackExplicitPrefixDoesNotFallback(t *testing.T) {
	calls := map[string]int{}
	up, _ := newRealmUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-global" {
			return modelRateLimit429()
		}
		return 200, sseOK, true
	})
	p := realmMixedPool(t)
	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		GlobalEnabled: true, RealmPrecedence: "global",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("显式 global 前缀且 global 全不可用应 503，code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 {
		t.Fatalf("显式前缀不得跨域回落，cn 号却被调了 %d 次", calls["Bearer at-cn"])
	}
	if calls["Bearer at-global"] != 1 {
		t.Fatalf("global 号应只被尝试 1 次（轮转内无更多本域候选），calls=%v", calls)
	}
}

// TestChatRealmFallbackBothRealmsLimited503 两域都对同模型 429/6004 → 503，
// 且不会无限回落（每个账号最多试一次，共 2 次上游调用）。
func TestChatRealmFallbackBothRealmsLimited503(t *testing.T) {
	calls := map[string]int{}
	up, _ := newRealmUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return modelRateLimit429()
	})
	p := realmMixedPool(t)
	p.SetCredits("bebb8506", 5000, 0)

	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		GlobalEnabled: true, RealmPrecedence: "global",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("两域全限流应 503，code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-global"] != 1 || calls["Bearer at-cn"] != 1 {
		t.Fatalf("两域各试一次即止（不得无限回落），calls=%v", calls)
	}
}

// TestChatRealmFallbackPrecedenceCN 反向 precedence：裸名归属 cn，cn 号限流后回落
// global（软优先是双向的，不是只往 CN 落）。
func TestChatRealmFallbackPrecedenceCN(t *testing.T) {
	calls := map[string]int{}
	up, seq := newRealmUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-cn" {
			return modelRateLimit429()
		}
		return 200, sseOK, true
	})
	p := realmMixedPool(t)
	p.SetCredits("c28ceb89", 5000, 0) // cn 号积分更高 → 第一轮先选它

	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		GlobalEnabled: true, RealmPrecedence: "cn",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))

	if rec.Code != 200 {
		t.Fatalf("裸名（归属 cn）在 cn 限流后应回落 global 得 200，code=%d body=%s", rec.Code, rec.Body)
	}
	got := *seq
	if len(got) != 2 || got[0].auth != "Bearer at-cn" || got[1].auth != "Bearer at-global" {
		t.Fatalf("调用顺序应 [cn, global]，实际 %+v", got)
	}
}

// TestChatRealmFallbackSingleRealmUnchanged 单域部署零回归：池内只有 global 号且
// 它对该模型限流 → 仍 503（没有另一域可回落），不会凭空变出候选。
func TestChatRealmFallbackSingleRealmUnchanged(t *testing.T) {
	withGlobalEnabled(t)
	up, _ := newRealmUpstream(t, func(authz string) (int, string, bool) {
		return modelRateLimit429()
	})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-global", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		GlobalEnabled: true, RealmPrecedence: "global",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("单域限流应 503，code=%d body=%s", rec.Code, rec.Body)
	}
}
