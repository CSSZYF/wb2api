// handler_reserve_interpolate_test.go 保留积分的余额插值在**真实请求路径**上的接线。
//
// pool.NoteConsumedCredits 的口径与不变量由 internal/pool/reserve_interpolate_test.go
// 逐条锁定；本文件只锁"接线"这一件事——一次成功的 chat 请求（流式 / 非流式）必须把
// 上游 usage.credit 喂到插值入口。为什么必须单独锁：
//   - 漏接线时，pool 侧的单测全绿而生产行为一字不变（余额仍只在余额刷新时下降），
//     正是本仓反复修的"改了实现却没接上"的失效模式；
//   - usage.credit **缺失**时不得调用（缺失 ≠ 0 成本）：否则免费请求也会被扣。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// creditsOf 读账号当前观测余额（本包测试专用；生产侧只经 /status 透出）。
func creditsOf(p *pool.Pool, uid string) int64 {
	st, ok := p.Status(uid)
	if !ok {
		return -1
	}
	return st.Credits
}

// sseWithCredit 在标准 SSE 成功帧序列的**末帧** usage 里带上 credit（真实上游形态：
// 扣费只在末帧 usage 出现）。credit<=0 时不带该字段，用于锁"缺失 ≠ 0 成本"。
//
// 注意：本网关的上游**总是**回 SSE（客户端要非流式时由 Aggregate 缓冲成整段响应），
// 故非流式用例同样用本 fixture（见 TestChatNonStreamAggregates 的既有形态）。
func sseWithCredit(credit float64) string {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	if credit > 0 {
		usage = `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"credit":` + formatFloat(credit) + `}`
	}
	return "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":" + usage + "}\n\n" +
		"data: [DONE]\n\n"
}

// formatFloat 浮点转 JSON 字面量（测试只用到 1/10/25/60 这类值）。
func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// TestChatStreamInterpolatesConsumedCredits 流式成功：末帧 usage.credit=60 → 账号观测
// 余额立刻从 100 降到 40（不等下一轮余额刷新）。
func TestChatStreamInterpolatesConsumedCredits(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseWithCredit(60), true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100, 9000) // 余额已知（creditsTotal>0 才插值）
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := creditsOf(p, "u1"); got != 40 {
		t.Fatalf("流式请求后余额=%d want 40（100-60，插值必须接在成功路径上）", got)
	}
}

// TestChatNonStreamInterpolatesConsumedCredits 非流式请求（客户端 stream=false）走
// Aggregate 缓冲后的**另一条** recordAttempt 调用点（obsFromResponse），同样必须插值
// ——两条路径各写一遍时极易漏一条。
func TestChatNonStreamInterpolatesConsumedCredits(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseWithCredit(25), true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100, 9000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := creditsOf(p, "u1"); got != 75 {
		t.Fatalf("非流式请求后余额=%d want 75（100-25）", got)
	}
}

// TestChatMissingCreditDoesNotInterpolate usage.credit **缺失** ≠ 0 成本：
// 缺失时不得扣减余额（扣了会让免费/无观测请求凭空消耗底线）。
func TestChatMissingCreditDoesNotInterpolate(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100, 9000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := creditsOf(p, "u1"); got != 100 {
		t.Fatalf("usage 无 credit 字段时余额不得变动，got %d want 100", got)
	}
}

// TestChatCreditInterpolationKeepsReserveGateHonest 端到端功能后果：一次请求把余额
// 从线上打到线下后，**紧接着的下一轮选号**就必须按保底换号（而不是继续用这个号）。
// 这条锁的是"插值真的能影响选号决策"，而不只是余额字段变了个数。
func TestChatCreditInterpolationKeepsReserveGateHonest(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithCredit(60), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100, 9000)
	p.SetReserveCredits(50)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	// 余额 40 ≤ 50：该号对贵模型必须立即出池（选号返回 nil → handler 走 503 文案）。
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
		t.Fatalf("插值触底后贵模型必须被保底拦下，got %s", got.UID)
	}
	// 免费模型照常（保底保的是"留余额给免费模型用"）。
	if got := p.PickExcludingForModel(nil, "hy3"); got == nil {
		t.Fatal("免费模型不得被拦")
	}
}
