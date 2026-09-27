package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// handler_turn_id_test.go conversationRequestID 统一轮级（吸收上游 03ce06d 的 #170 项，
// 上游原始提交 b9ac0d3）。
//
// 背景：X-Conversation-Request-ID 是上游后台的**对话轮级聚合主键**——一次 user send
// 内的所有 tool call / 换号重试 / 降级重发共用一个 ID，后台据此聚成一条记录；换
// user 消息即换键。官方桌面 CLI 的实证语义（上游审计 §2 解包证据）：TraceStartHook
// 在每次 USER_PROMPT_SUBMIT 时清空重生成 conversationRequestId——同轮内复用、跨轮必换。
//
// 本仓此前对**带会话键**的客户端（conversationId / metadata.conversation_id /
// prompt_cache_key / 内容派生 d- 键）走 RequestIDForKey(sessKey) 会话级聚合：同一
// 会话的全部轮共用一个 ID（跨轮同值）。该行为继承自 merge-base（上游 #170 之前的
// 形态），本仓从未跟进 b9ac0d3——后果是上游用量明细把整段会话的几十轮并成一条，
// 轮级信息（每轮各自的 token/延迟/命中）在后台聚合里丢失。
//
// 本仓保留的差异：turnKey 空态（无 user 消息 / 无可签名内容）仍回落会话级
// RequestIDForKey(sessKey)——残留空态聚合好于请求级随机。

// outboundConvReqIDs 发若干次 chat 请求，返回每次出站携带的
// X-Conversation-Request-ID（按出站顺序）。断言轮级语义必须看**出站头**，
// 而不是 handler 内部变量——透传/兜底分支都在该头上收敛。
func outboundConvReqIDs(t *testing.T, bodies ...string) []string {
	t.Helper()
	var ids []string
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			ids = append(ids, r.Header.Get("X-Conversation-Request-ID"))
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	for i, body := range bodies {
		if code := postChat(t, h, body); code != 200 {
			t.Fatalf("call %d: code=%d", i, code)
		}
	}
	if len(ids) != len(bodies) {
		t.Fatalf("出站次数=%d want %d", len(ids), len(bodies))
	}
	return ids
}

// TestTurnIDSessionKeyTurnScoped #170 RED 锚（R1）：带会话键客户端同会话**两轮**
// （末条 user 不同）→ 出站聚合 ID 必须不同（轮级语义）。旧行为
// RequestIDForKey(sessKey) 会话级跨轮同值 → 本用例必红。
//
// 这是本文件的核心契约：上游后台按该头聚合请求，会话级 ID 会把整段会话并成一条，
// 每轮的 token/延迟明细全部丢失。
func TestTurnIDSessionKeyTurnScoped(t *testing.T) {
	ids := outboundConvReqIDs(t,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[{"role":"user","content":"第一问"}]}`,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[`+
			`{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},`+
			`{"role":"user","content":"第二问"}]}`,
	)
	if ids[0] == "" || ids[1] == "" {
		t.Fatalf("带会话键请求也必须发聚合 ID: %v", ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("同会话跨轮应换聚合 ID（轮级，对齐官方 CLI）: a=%q b=%q", ids[0], ids[1])
	}
}

// TestTurnIDStableWithinTurn R2：轮内 tool-call 多步（追加 assistant/tool，末条 user
// 不变）→ 同 ID。这是 issue #35 的轮内聚合核心语义，统一轮级后必须保留（否则一次
// user send 的多个 tool call 又被拆成多条）。
func TestTurnIDStableWithinTurn(t *testing.T) {
	ids := outboundConvReqIDs(t,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[{"role":"user","content":"跑一下"}]}`,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[`+
			`{"role":"user","content":"跑一下"},`+
			`{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"pwsh"}}]},`+
			`{"role":"tool","tool_call_id":"c1","content":"结果"}]}`,
	)
	if ids[0] == "" {
		t.Fatal("聚合 ID 缺失")
	}
	if ids[0] != ids[1] {
		t.Errorf("轮内追加消息不得改变聚合键: step1=%q step2=%q", ids[0], ids[1])
	}
}

// TestTurnIDCrossSessionSameTurnText R4：不同会话同轮文本 → 不同 ID（会话段入复合键，
// 防跨会话同文本互撞——没有会话段时两个会话的「同样的问题」会共用聚合键）。
func TestTurnIDCrossSessionSameTurnText(t *testing.T) {
	ids := outboundConvReqIDs(t,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-a"},"messages":[{"role":"user","content":"同样的问题"}]}`,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-b"},"messages":[{"role":"user","content":"同样的问题"}]}`,
	)
	if ids[0] == "" || ids[1] == "" {
		t.Fatalf("聚合 ID 缺失: %v", ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("不同会话同轮文本不应共用聚合 ID（会话段入键防撞）: %q", ids[0])
	}
}

// TestTurnIDEmptyTurnKeyFallsBackToSession R5：turnKey 空态（无 user 消息/无可签名
// 内容）+ 会话键非空 → 回落**会话级**兜底（同 sessKey 恒同值、纯派生非随机）。
// 残留空态下会话级聚合好于请求级随机（每条上游记录各一个 ID = 完全碎片化）。
func TestTurnIDEmptyTurnKeyFallsBackToSession(t *testing.T) {
	ids := outboundConvReqIDs(t,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-e"},"messages":[{"role":"assistant","content":"续"}]}`,
		`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-e"},"messages":[{"role":"assistant","content":"又续"}]}`,
	)
	if ids[0] == "" || ids[1] == "" {
		t.Fatalf("聚合 ID 缺失: %v", ids)
	}
	if ids[0] != ids[1] {
		t.Errorf("turnKey 空态应回落会话级兜底（同 sessKey 同值）: a=%q b=%q", ids[0], ids[1])
	}
}

// TestTurnIDNoSessionKeyStaysTurnLevel R6：无会话键客户端走既有纯轮级键——跨轮换键、
// 同轮同键。#170 只改「带会话键」那一支，这类客户端的语义必须零漂移。
//
// 隔离手法：首条 user 消息无可签名内容（空串）→ 本仓第 4 位内容派生键（d- 前缀）
// 恒空（deriveKey 要求首条 user 可签名），而 TurnKey 取**末条** user 消息 → 非空，
// 于是恰好落到 sessKey=="" && turnKey!="" 的纯轮级分支（带 user_id 的抑制形态由
// TestChatUserIDKeepsTurnLevelAggregation 覆盖，此处走另一条入口）。
func TestTurnIDNoSessionKeyStaysTurnLevel(t *testing.T) {
	ids := outboundConvReqIDs(t,
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":""},{"role":"assistant","content":"起手"},{"role":"user","content":"第一问"}]}`,
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":""},{"role":"assistant","content":"起手"},{"role":"user","content":"第一问"}]}`,
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":""},{"role":"assistant","content":"起手"},{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`,
	)
	if ids[0] == "" || ids[1] == "" || ids[2] == "" {
		t.Fatalf("聚合 ID 缺失: %v", ids)
	}
	if ids[0] != ids[1] {
		t.Errorf("同轮同 body 应复用聚合 ID（纯轮级）: %q vs %q", ids[0], ids[1])
	}
	if ids[2] == ids[0] {
		t.Errorf("无会话键客户端跨轮应换聚合 ID: first=%q next=%q", ids[0], ids[2])
	}
}

// TestTurnIDInboundHeaderPassthrough 入站 X-Conversation-Request-ID 透传优先于任何
// 派生（客户端自己声明的轮级 ID 是权威值）：原样出站，不被网关改写。
func TestTurnIDInboundHeaderPassthrough(t *testing.T) {
	const inbound = "0123456789abcdef0123456789abcdef"
	var got string
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Get("X-Conversation-Request-ID")
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-x"},"messages":[{"role":"user","content":"问"}]}`))
	req.Header.Set("X-Conversation-Request-ID", inbound)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if got != inbound {
		t.Errorf("入站轮级 ID 应原样透传: got=%q want=%q", got, inbound)
	}
}
