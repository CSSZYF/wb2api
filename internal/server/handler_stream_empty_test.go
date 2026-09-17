package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatStreamEmptyUpstreamLogged502 RED：上游 200 + 空流（0 有效帧）时，Stream
// 写 error 帧 + [DONE] 兜底（HTTP 头已发出，wire 仍 200），但日志状态必须收敛为
// 502 观测——此前 `_ =` 吞错把失败流记成 200 假成功（运维排障被误导）。
func TestChatStreamEmptyUpstreamLogged502(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "", true // 空 body
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})
	if rec.Code != 200 {
		t.Fatalf("wire code=%d want 200 (headers already sent before empty-stream detected)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty upstream stream") {
		t.Errorf("client should receive empty-stream error frame: %s", rec.Body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("log row status must be 502 (empty stream is upstream failure), got:\n%s", out)
	}
}

// TestChatStreamNoUsageShowsDashTok 流式末帧无 usage 时 tok 列显示 "-"：chatStat.toks
// 初值 -1 是「未观测到 usage」哨兵，不得被 stats.Tokens() 的零值 0 覆盖成「测得 0 token」
// （与非流式走 completionTokens 返回 -1 的口径一致）。
func TestChatStreamNoUsageShowsDashTok(t *testing.T) {
	withChatLog(t)
	const sseNoUsage = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseNoUsage, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	if !strings.Contains(out, "tok=-") {
		t.Errorf("missing usage must keep the -1 sentinel (tok=-), got:\n%s", out)
	}
	if strings.Contains(out, "tok=0 ") {
		t.Errorf("missing usage must NOT be reported as tok=0 (fabricated observation), got:\n%s", out)
	}
}
