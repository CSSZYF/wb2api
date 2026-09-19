package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// handler 级端到端：粘性键补全（prompt_cache_key 来源 + 内容派生兜底的图片盲区
// 修复 + user_id 抑制）必须**经 /v1/chat/completions 全链路**成立——键在
// session.ExtractKey 里算，handler 只按 sessKey 走既有粘性路径，故本组用例
// 钉住的是"提取结果 → 绑定写入"的接线不回归。

// stickySessionFor 构建一个粘性路由 + 记录镜像的 Store，池含两个可用账号。
func stickySessionFor(t *testing.T, st *bindStore) *session.Router {
	t.Helper()
	return session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"a1", "a2"} },
	})
}

// newStickyHandler 构建「两个健康账号 + 粘性路由」的 handler。
func newStickyHandler(t *testing.T, st *bindStore) *Handler {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	return NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      stickySessionFor(t, st),
		SoftCooldown: time.Minute,
	})
}

// postChat 发一次非流式 chat 请求并返回响应码。
func postChat(t *testing.T, h *Handler, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec.Code
}

// TestChatStickyPromptCacheKeySource prompt_cache_key 来源端到端：OpenAI 兼容客户端
// （无 conversation_id / metadata）把会话 ID 放在 prompt_cache_key 里 → 粘性绑定
// 以该值为键建立（此前 ExtractKey 恒空、粘性永不参与、逐请求换号）。
func TestChatStickyPromptCacheKeySource(t *testing.T) {
	st := newBindStore()
	h := newStickyHandler(t, st)
	body := `{"model":"glm-5.2","prompt_cache_key":"pi-ai-sess-7","messages":[{"role":"user","content":"你好"}]}`
	if code := postChat(t, h, body); code != 200 {
		t.Fatalf("code=%d", code)
	}
	uid, ok := st.lastUID("pi-ai-sess-7")
	if !ok || uid == "" {
		t.Fatalf("prompt_cache_key 应作为粘性键建立绑定, binds=%v", st.binds)
	}
	// 同键第二次请求 → 仍绑同一账号（粘性命中而非重新分配）。
	if code := postChat(t, h, body); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if uid2, _ := st.lastUID("pi-ai-sess-7"); uid2 != uid {
		t.Errorf("同 prompt_cache_key 应粘同一账号: %q vs %q", uid2, uid)
	}
}

// TestChatStickyImageOnlyFirstTurn 首图会话粘性端到端（G1 加重形态）：首条 user
// 纯图片（无文本）此前派生不出键 → 该类会话完全无粘性；现派生 d- 键并建立绑定。
func TestChatStickyImageOnlyFirstTurn(t *testing.T) {
	st := newBindStore()
	h := newStickyHandler(t, st)
	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`
	if code := postChat(t, h, body); code != 200 {
		t.Fatalf("code=%d", code)
	}
	var key string
	for k := range st.binds {
		key = k
	}
	if key == "" {
		t.Fatalf("纯图片首轮应建立粘性绑定（G1 修复）, binds=%v", st.binds)
	}
	if !strings.HasPrefix(key, "d-") {
		t.Errorf("纯图片会话键应走内容派生（d- 前缀）: %q", key)
	}
	// 会话推进（历史追加、首条 user 不变）→ 同一键 → 同账号。
	uid, _ := st.lastUID(key)
	next := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]},` +
		`{"role":"assistant","content":"答"},{"role":"user","content":"继续"}]}`
	if code := postChat(t, h, next); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if uid2, ok := st.lastUID(key); !ok || uid2 != uid {
		t.Errorf("纯图会话推进应复用同一绑定: key=%q uid=%q->%q binds=%v", key, uid, uid2, st.binds)
	}
}

// TestChatStickySuppressedByUserID user_id 抑制端到端（P1-anti-monopoly）：
// 带 user_id 的请求**不得**建立任何粘性绑定——否则只发 user_id 的客户端会借内容
// 派生重新获得粘性，一个 user 的并行对话被钉到同一账号（契约回归）。
func TestChatStickySuppressedByUserID(t *testing.T) {
	st := newBindStore()
	h := newStickyHandler(t, st)
	bodies := []string{
		`{"model":"glm-5.2","metadata":{"user_id":"u-42"},"messages":[{"role":"user","content":"帮我写代码"}]}`,
		`{"model":"glm-5.2","user_id":"u-42","messages":[{"role":"user","content":"帮我写代码"}]}`,
		// 纯图片 + user_id：两条兜底路径都不得给键。
		`{"model":"glm-5.2","metadata":{"user_id":"u-42"},"messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`,
	}
	for _, body := range bodies {
		if code := postChat(t, h, body); code != 200 {
			t.Fatalf("code=%d", code)
		}
		if len(st.binds) != 0 {
			t.Fatalf("带 user_id 的请求不应建立粘性绑定（P1-anti-monopoly）, binds=%v", st.binds)
		}
	}
}

// TestChatStickyExplicitConversationIDUnchanged 向后兼容端到端：显式 conversation_id
// 路径的绑定键与语义完全不变（改动只新增来源与兜底，不动既有四键）。
func TestChatStickyExplicitConversationIDUnchanged(t *testing.T) {
	st := newBindStore()
	h := newStickyHandler(t, st)
	body := `{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`
	if code := postChat(t, h, body); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if _, ok := st.lastUID("conv-1"); !ok {
		t.Fatalf("显式 conversation_id 应照常建立绑定, binds=%v", st.binds)
	}
	// 显式键优先于 prompt_cache_key（同请求两者都有时）。
	if code := postChat(t, h, `{"model":"glm-5.2","prompt_cache_key":"pk","messages":[],"metadata":{"conversation_id":"conv-2"}}`); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if _, ok := st.lastUID("conv-2"); !ok {
		t.Errorf("conversation_id 应优先于 prompt_cache_key, binds=%v", st.binds)
	}
	if _, ok := st.lastUID("pk"); ok {
		t.Errorf("有 conversation_id 时不应以 prompt_cache_key 建键, binds=%v", st.binds)
	}
}

// TestChatImageOnlyTurnAggregation 纯图片轮的**聚合**半边（G1）：末条 user 纯图片
// 此前 TurnKey 返回空串 → 出站 X-Conversation-Request-ID 退化成请求级随机
// （同轮内换号/重试各发一个 ID，上游用量明细碎片化）。
//
// 注意本仓键链与上游的**结构差异**（实测确认，非猜测）：上游无内容派生键，
// 无会话标识的请求走轮级兜底（TurnKey → TurnRequestID，跨轮换 ID）；本仓有第 4 位
// 内容派生键（d- 前缀），这类请求走**会话级**（RequestIDForKey(d-键)，跨轮恒同 ID，
// 粒度更粗但聚合更紧——同一对话的全部轮并成一条）。本用例钉的是"非空且稳定"：
//   - 同 body 两次出站 → 同 ID（原为各自随机的两个 ID）；
//   - 会话推进（历史追加、首条 user 不变）→ 仍同 ID（会话级语义）；
//   - 换图（新会话）→ 换 ID。
func TestChatImageOnlyTurnAggregation(t *testing.T) {
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
	// 不注入 Session：本用例只看聚合头（与粘性开关解耦）。
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	imgBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`
	for i := 0; i < 2; i++ {
		if code := postChat(t, h, imgBody); code != 200 {
			t.Fatalf("call %d: code=%d", i, code)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("出站次数=%d want 2", len(ids))
	}
	if ids[0] == "" {
		t.Fatal("纯图请求应派生聚合 ID（G1：原为空串 → 请求级随机碎片化）")
	}
	if ids[0] != ids[1] {
		t.Errorf("纯图同 body 两次出站应同聚合 ID: %q vs %q", ids[0], ids[1])
	}
	if len(ids[0]) != 32 {
		t.Errorf("聚合 ID 应为 32 hex（可作 B3 TraceId）: %q", ids[0])
	}
	// 会话推进：本仓走内容派生（会话级）→ 跨轮仍同 ID（比上游轮级粒度更粗）。
	next := `{"model":"glm-5.2","stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]},` +
		`{"role":"assistant","content":"答"},{"role":"user","content":"继续"}]}`
	if code := postChat(t, h, next); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if len(ids) != 3 || ids[2] != ids[0] {
		t.Errorf("纯图会话推进应复用同一（会话级）聚合 ID: first=%q next=%q", ids[0], ids[2])
	}
	// 换图 = 新会话 → 换 ID。
	otherImg := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/dog.png"}}]}]}`
	if code := postChat(t, h, otherImg); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if len(ids) != 4 || ids[3] == "" || ids[3] == ids[0] {
		t.Errorf("换图（新会话）应换聚合 ID: first=%q other=%q", ids[0], ids[3])
	}
}

// TestChatUserIDKeepsTurnLevelAggregation user_id 抑制的**聚合侧**影响：带 user_id 的
// 请求不派生内容键（P1-anti-monopoly），故这类请求回落**轮级**兜底（TurnKey →
// TurnRequestID）——跨轮换 ID（对齐官方 queueRequestId 形态），而非会话级。
// 本用例同时验证抑制没有把聚合链一并打断（仍有非空、稳定的 ID）。
func TestChatUserIDKeepsTurnLevelAggregation(t *testing.T) {
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

	body := `{"model":"glm-5.2","stream":true,"metadata":{"user_id":"u1"},"messages":[` +
		`{"role":"user","content":"第一问"}]}`
	for i := 0; i < 2; i++ {
		if code := postChat(t, h, body); code != 200 {
			t.Fatalf("call %d: code=%d", i, code)
		}
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("同轮内（末条 user 不变）应复用同一聚合 ID: %v", ids)
	}
	// 下一轮（末条 user 变化）→ 轮级换 ID。
	next := `{"model":"glm-5.2","stream":true,"metadata":{"user_id":"u1"},"messages":[` +
		`{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},` +
		`{"role":"user","content":"第二问"}]}`
	if code := postChat(t, h, next); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if len(ids) != 3 || ids[2] == "" || ids[2] == ids[0] {
		t.Errorf("带 user_id 的请求应回落轮级聚合（跨轮换 ID）: first=%q next=%q", ids[0], ids[2])
	}
}
