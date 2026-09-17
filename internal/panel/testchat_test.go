package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newTestChatPanel 组装一个只含单账号的测试面板：上游 base 指向假服务器。
// 账号为 CN 域（chatBase → ChatBaseCN，chatPaths 只有 /v2/chat/completions 一条），
// 测试里只需处理这一个路径。
func newTestChatPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-11111111-2222-3333", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})
}

// testChatReq 发一次 test_chat 请求（带面板鉴权头），返回响应记录器。
func testChatReq(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/account/test_chat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// sseServer 起一个假上游：按 delta 分片回 SSE 流，末帧 [DONE]。
func sseServer(t *testing.T, status int, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":429,"msg":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, c := range chunks {
			frame, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "glm-5.2",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": c},
				}},
			})
			_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			if i == 0 {
				// 让流中间真的分片到达（Aggregate 必须跨帧拼接，不能只认第一帧）。
				w.(http.Flusher).Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// TestTestChatAggregatesStream 正常路径：SSE 分片被聚合成完整回复，
// 耗时字段存在、账号前缀回显、且出站 body 带上限 max_tokens。
func TestTestChatAggregatesStream(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 出站 body 也一并核对：面板发的就是网关 chat 路径的那一份。
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range []string{"你", "好", "，世界"} {
			frame, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "glm-5.2",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": c}}},
			})
			_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	rec := testChatReq(t, p, `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"你好"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK         bool   `json:"ok"`
		Status     int    `json:"status"`
		LatencyMs  int64  `json:"latency_ms"`
		Reply      string `json:"reply"`
		ReplyChars int    `json:"reply_chars"`
		Model      string `json:"model"`
		Account    string `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("ok=false body=%s", rec.Body.String())
	}
	if got.Status != 200 {
		t.Errorf("status=%d want 200", got.Status)
	}
	if got.Reply != "你好，世界" {
		t.Errorf("reply=%q want 你好，世界（SSE 分片必须跨帧拼接）", got.Reply)
	}
	if got.ReplyChars != 5 {
		t.Errorf("reply_chars=%d want 5", got.ReplyChars)
	}
	if got.Model != "glm-5.2" {
		t.Errorf("model=%q want glm-5.2", got.Model)
	}
	if got.Account == "" || !strings.HasPrefix("u-11111111-2222-3333", strings.TrimSuffix(got.Account, "…")) {
		t.Errorf("account=%q 应为 uid 前缀", got.Account)
	}
	if got.LatencyMs < 0 {
		t.Errorf("latency_ms=%d 不应为负", got.LatencyMs)
	}
	// 出站 body 必须带上限，否则超长输出会吃掉 60s 预算。
	if mt, ok := gotBody["max_tokens"].(float64); !ok || int(mt) != testChatMaxTokens {
		t.Errorf("出站 max_tokens=%v want %d", gotBody["max_tokens"], testChatMaxTokens)
	}
	if m, _ := gotBody["model"].(string); m != "glm-5.2" {
		t.Errorf("出站 model=%v want glm-5.2", gotBody["model"])
	}
	// stream 必须为 true：上游拒绝非流式，PrepareBody 会强制改写。
	if s, _ := gotBody["stream"].(bool); !s {
		t.Errorf("出站 stream=%v want true（上游拒绝非流式）", gotBody["stream"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("出站 messages=%v want 1 条", gotBody["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "你好" {
		t.Errorf("出站 messages[0]=%v want {user 你好}", m0)
	}
}

// TestTestChatTruncatesReply 回复超 200 字符时只回摘要，reply_chars 给出截断前长度。
func TestTestChatTruncatesReply(t *testing.T) {
	long := strings.Repeat("长", 500)
	srv := sseServer(t, 200, []string{long})
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	rec := testChatReq(t, p, `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"hi"}`)
	var got struct {
		Reply          string `json:"reply"`
		ReplyChars     int    `json:"reply_chars"`
		ReplyTruncated bool   `json:"reply_truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(strings.TrimSuffix(got.Reply, "…"))); n != testChatReplyLimit {
		t.Errorf("reply 截断后 %d 字符 want %d", n, testChatReplyLimit)
	}
	if got.ReplyChars != 500 {
		t.Errorf("reply_chars=%d want 500（截断前长度）", got.ReplyChars)
	}
	if !got.ReplyTruncated {
		t.Error("reply_truncated=false，500 字回复应标记为已截断")
	}
}

// TestTestChatAccountNotFound 账号不在池里：404 + 可读错误（不是 500/空回复）。
func TestTestChatAccountNotFound(t *testing.T) {
	srv := sseServer(t, 200, []string{"x"})
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	rec := testChatReq(t, p, `{"uid":"no-such-uid","model":"glm-5.2","message":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || !strings.Contains(got.Error, "账号不存在") {
		t.Errorf("ok=%v error=%q want 可读的账号不存在错误", got.OK, got.Error)
	}
}

// TestTestChatUpstreamErrorSurfaced 上游 4xx/5xx：ok=false + 上游 status 与原文透出，
// 且带 latency_ms（面板要能区分"秒拒"与"超时"）。
func TestTestChatUpstreamErrorSurfaced(t *testing.T) {
	srv := sseServer(t, http.StatusTooManyRequests, nil)
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	rec := testChatReq(t, p, `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"hi"}`)
	// 端点语义：测试已执行完毕 → HTTP 200，结论在 body。
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（上游错误不是本端点的 HTTP 错误）body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK        bool   `json:"ok"`
		Status    int    `json:"status"`
		Error     string `json:"error"`
		LatencyMs int64  `json:"latency_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.OK {
		t.Fatalf("ok=true 但上游 429 body=%s", rec.Body.String())
	}
	if got.Status != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429", got.Status)
	}
	if !strings.Contains(got.Error, "429") || !strings.Contains(got.Error, "rate limited") {
		t.Errorf("error=%q 应含上游状态码与原文", got.Error)
	}
	if got.LatencyMs < 0 {
		t.Errorf("latency_ms=%d 不应为负", got.LatencyMs)
	}
}

// TestTestChatParseFailure 上游 200 但流里没有有效数据事件：ok=false（不伪装成空回复成功）。
func TestTestChatParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n")) // 只有结束标记，无任何有效帧
	}))
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	rec := testChatReq(t, p, `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"hi"}`)
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || !strings.Contains(got.Error, "解析上游流失败") {
		t.Errorf("ok=%v error=%q want 解析失败", got.OK, got.Error)
	}
}

// TestTestChatValidation 参数校验：uid/model 必填、message 非空且不超 4000 字符。
// 这些是请求本身不合法 → 400（与"上游拒绝"的 200+ok=false 区分开）。
func TestTestChatValidation(t *testing.T) {
	srv := sseServer(t, 200, []string{"x"})
	defer srv.Close()
	p := newTestChatPanel(t, srv)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"uid 缺失", `{"model":"glm-5.2","message":"hi"}`, http.StatusBadRequest},
		{"uid 空白", `{"uid":"  ","model":"glm-5.2","message":"hi"}`, http.StatusBadRequest},
		{"model 缺失", `{"uid":"u-11111111-2222-3333","message":"hi"}`, http.StatusBadRequest},
		{"message 为空", `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":""}`, http.StatusBadRequest},
		{"message 全空白", `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"   "}`, http.StatusBadRequest},
		{"message 超长", `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"` + strings.Repeat("a", testChatMaxMessage+1) + `"}`, http.StatusBadRequest},
		{"body 非 JSON", `not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := testChatReq(t, p, c.body)
			if rec.Code != c.want {
				t.Fatalf("code=%d want %d body=%s", rec.Code, c.want, rec.Body.String())
			}
			var got struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.OK || got.Error == "" {
				t.Errorf("ok=%v error=%q want ok=false + 可读错误", got.OK, got.Error)
			}
		})
	}
}

// TestTestChatMessageLengthBoundary 4000 字符整（按字符不按字节）必须放行：
// 中文场景下按字节算会把 1333 字的上限砍成 1/3，是常见 off-by-bytes 回归点。
func TestTestChatMessageLengthBoundary(t *testing.T) {
	srv := sseServer(t, 200, []string{"ok"})
	defer srv.Close()
	p := newTestChatPanel(t, srv)

	msg := strings.Repeat("中", testChatMaxMessage) // 4000 字符 = 12000 字节
	body, _ := json.Marshal(map[string]any{
		"uid": "u-11111111-2222-3333", "model": "glm-5.2", "message": msg,
	})
	rec := testChatReq(t, p, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("4000 字符（12000 字节）应放行，code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestTestChatDoesNotTouchAccountState 诊断操作不得改账号状态：
// 上游 429 之后 success/err 计数、冷却、熔断全部原样（不调 applyErrorPolicy 的落点）。
func TestTestChatDoesNotTouchAccountState(t *testing.T) {
	srv := sseServer(t, http.StatusTooManyRequests, nil)
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	before, ok := p.cfg.Pool.Status("u-11111111-2222-3333")
	if !ok {
		t.Fatal("测试账号未入池")
	}
	rec := testChatReq(t, p, `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := p.cfg.Pool.Status("u-11111111-2222-3333")
	if after.SuccessCount != before.SuccessCount || after.ErrTotal != before.ErrTotal {
		t.Errorf("成功/错误计数被改写：before=%d/%d after=%d/%d",
			before.SuccessCount, before.ErrTotal, after.SuccessCount, after.ErrTotal)
	}
	if after.Cooling != before.Cooling || after.BreakerFails != before.BreakerFails {
		t.Errorf("冷却/熔断被改写：before cooling=%v fails=%d after cooling=%v fails=%d",
			before.Cooling, before.BreakerFails, after.Cooling, after.BreakerFails)
	}
	if after.InFlight != before.InFlight {
		t.Errorf("在途计数被改写：before=%d after=%d", before.InFlight, after.InFlight)
	}
}

// TestTestChatEmptyContentStillOk 思考型模型把 max_tokens 预算全花在思考上时，
// content 为空但链路是通的：ok=true + finish_reason/reasoning_chars 回显，
// 前端据此显示"思考占满预算"而不是含糊的"空回复"。
func TestTestChatEmptyContentStillOk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 只有 reasoning_content，没有 content；末帧 finish_reason=length。
		_, _ = w.Write([]byte("data: " + `{"id":"c1","model":"glm-5.2","choices":[{"index":0,` +
			`"delta":{"reasoning_content":"让我想想…"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: " + `{"id":"c1","model":"glm-5.2","choices":[{"index":0,` +
			`"delta":{},"finish_reason":"length"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	rec := testChatReq(t, p, `{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK             bool   `json:"ok"`
		Reply          string `json:"reply"`
		FinishReason   string `json:"finish_reason"`
		ReasoningChars int    `json:"reasoning_chars"`
		MaxTokens      int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("ok=false body=%s（链路通，只是没有 content）", rec.Body.String())
	}
	if got.Reply != "" {
		t.Errorf("reply=%q want 空（只有思考）", got.Reply)
	}
	if got.FinishReason != "length" {
		t.Errorf("finish_reason=%q want length（判读空回复的依据）", got.FinishReason)
	}
	if got.ReasoningChars != len([]rune("让我想想…")) {
		t.Errorf("reasoning_chars=%d want %d", got.ReasoningChars, len([]rune("让我想想…")))
	}
	if got.MaxTokens != testChatMaxTokens {
		t.Errorf("max_tokens=%d want %d（前端解释文案用）", got.MaxTokens, testChatMaxTokens)
	}
}

// TestTestChatCancelPropagates 取消传导：请求 ctx 一取消（浏览器关窗、或 60s 上限
// 到期），阻塞中的出站请求与聚合读必须立刻返回，handler 不得吊住。
// 用真实 ctx 取消验证这条路径，不必等满 60s。
func TestTestChatCancelPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // 挂住不吐数据，直到调用方取消
	}))
	defer srv.Close()

	p := newTestChatPanel(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/panel/api/account/test_chat",
		strings.NewReader(`{"uid":"u-11111111-2222-3333","model":"glm-5.2","message":"hi"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { p.ServeHTTP(rec, req); close(done) }()
	time.Sleep(150 * time.Millisecond) // 让出站请求真的发出并被挂住
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 handler 未返回：取消没有传导到上游（请求会吊到 60s 上限）")
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body=%q err=%v", rec.Body.String(), err)
	}
	if got.OK || got.Error == "" {
		t.Errorf("ok=%v error=%q want ok=false + 可读错误", got.OK, got.Error)
	}
}

// TestTestChatRequiresAuth 未带密钥必须 401（与其它 /panel/api/* 同口径）。
func TestTestChatRequiresAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	req := httptest.NewRequest("POST", "/panel/api/account/test_chat",
		strings.NewReader(`{"uid":"x","model":"y","message":"z"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}
