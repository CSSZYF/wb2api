package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本文件覆盖流式中断兜底的**服务端观测**（移植上游 hub aaeefc4）：
//   - 上游中途断流 → 客户端收到 error 帧 + 恰好一个 [DONE]，日志状态收敛 502
//     （此前既不写终结帧也不收敛状态：客户端不知道流结束了，运维看到假 200）；
//   - 客户端主动断连 → **不**标 502（人已走，502 观测没有意义），两道闸门分别覆盖；
//   - 不回归：正常流仍恰好一个 [DONE]，空流仍 502。

// abortingUpstreamBody 先吐 prefix，再返回固定读错误（模拟上游中途断流）。
type abortingUpstreamBody struct {
	prefix string
	err    error
	done   bool
}

func (b *abortingUpstreamBody) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		if b.prefix != "" {
			return copy(p, b.prefix), nil
		}
	}
	return 0, b.err
}

func (b *abortingUpstreamBody) Close() error { return nil }

// newAbortingUpstream 假上游：200 + text/event-stream，body 吐 prefix 后读失败。
func newAbortingUpstream(prefix string, readErr error) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &abortingUpstreamBody{prefix: prefix, err: readErr},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// const abortPartialFrame 中断流用例里「已吐字节」的部分（半截流：有内容、无 [DONE]）。
const abortPartialFrame = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"半截\"}}]}\n\n"

// chatStreamReq 构造一条流式 chat 请求（ctx 为 nil 时用默认背景 ctx）。
func chatStreamReq(ctx context.Context) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	return req
}

// TestChatStreamMidwayAbortLogs502 RED：上游中途断流（读错误）时客户端必须收到
// error 帧 + 恰好一个 [DONE] 终结，日志状态收敛 502——此前读错误直接退出：既不写
// error 帧也不写 [DONE]，客户端拿着半截流不知道结束了，运维只看到假 200。
func TestChatStreamMidwayAbortLogs502(t *testing.T) {
	withChatLog(t)
	up := newAbortingUpstream(abortPartialFrame, errors.New("read tcp: connection reset by peer"))
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, chatStreamReq(nil))
	})
	if rec.Code != 200 {
		t.Fatalf("wire code=%d want 200 (SSE headers already sent)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "半截") {
		t.Errorf("已吐帧必须透传给客户端: %q", body)
	}
	if !strings.Contains(body, "upstream stream aborted") {
		t.Errorf("client should receive abort error frame: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want exactly 1: %q", n, body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("log row status must be 502 (midway abort is upstream failure), got:\n%s", out)
	}
	if strings.Contains(out, "| 200 |") {
		t.Errorf("log row must NOT be a fake 200, got:\n%s", out)
	}
}

// TestChatStreamMidwayAbortClientDisconnectNot502 客户端主动断连（入站 ctx 已取消）：
// 出站请求随之取消、底流 Read 返回 context.Canceled——形态与「上游静默被掐流」同形，
// 但这是「人已走」不是上游缺陷，**不得**标 502 假失败。
func TestChatStreamMidwayAbortClientDisconnectNot502(t *testing.T) {
	withChatLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已走
	up := newAbortingUpstream(abortPartialFrame, context.Canceled)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, chatStreamReq(ctx))
	})
	if !strings.Contains(out, "| stream |") {
		t.Fatalf("log row missing (request did not reach the streaming path?):\n%s", out)
	}
	if strings.Contains(out, "| 502 |") {
		t.Errorf("client disconnect must NOT be logged as 502 (人已走，观测无意义), got:\n%s", out)
	}
	// 终结帧照常写出（本地写出与客户端是否还在读无关；写失败才走下面那道闸门）。
	if n := strings.Count(rec.Body.String(), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want exactly 1: %q", n, rec.Body.String())
	}
}

// failAfterFlusher 包装 ResponseWriter：前 ok 次写成功，之后写失败（模拟客户端断连后
// 内核缓冲也写不进去）。实现 Flusher 以满足 SSE 的 flush 路径。
type failAfterFlusher struct {
	rec *httptest.ResponseRecorder
	mu  sync.Mutex
	ok  int
	err error
}

func (w *failAfterFlusher) Header() http.Header { return w.rec.Header() }

func (w *failAfterFlusher) WriteHeader(code int) { w.rec.WriteHeader(code) }

func (w *failAfterFlusher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ok <= 0 {
		return 0, w.err
	}
	w.ok--
	return w.rec.Write(p)
}

func (w *failAfterFlusher) Flush() {}

// TestChatStreamAbortWriteFailureNot502 客户端断连的另一道闸门：终结帧写失败时
// Stream 返回的是**写错误**而不是上游缺陷哨兵 → 不标 502（「只认上游缺陷、不认
// 人已走」的口径）。
func TestChatStreamAbortWriteFailureNot502(t *testing.T) {
	withChatLog(t)
	up := newAbortingUpstream(abortPartialFrame, io.ErrUnexpectedEOF)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	// 前 2 次写（数据帧 + 可能的 flush 序）成功，终结帧写失败。
	w := &failAfterFlusher{rec: httptest.NewRecorder(), ok: 1, err: errors.New("client gone: broken pipe")}
	out := captureStdout(t, func() {
		h.ServeHTTP(w, chatStreamReq(nil))
	})
	if !strings.Contains(out, "| stream |") {
		t.Fatalf("log row missing (request did not reach the streaming path?):\n%s", out)
	}
	if strings.Contains(out, "| 502 |") {
		t.Errorf("terminal-frame write failure (client gone) must NOT be logged as 502, got:\n%s", out)
	}
	// 已吐帧照常写出（写失败发生在终结帧之后，已到字节不回收）。
	if !strings.Contains(w.rec.Body.String(), "半截") {
		t.Errorf("frames written before the failure must stay: %q", w.rec.Body.String())
	}
}

// TestChatStreamNormalStillOneDone 不回归：正常流仍恰好一个 [DONE]、无 error 帧、
// 日志状态 200。
func TestChatStreamNormalStillOneDone(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, chatStreamReq(nil))
	})
	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want exactly 1: %q", n, body)
	}
	if strings.Contains(body, "upstream stream aborted") || strings.Contains(body, `"error"`) {
		t.Errorf("normal stream must have no error frame: %q", body)
	}
	if !strings.Contains(out, "| 200 |") {
		t.Errorf("normal stream must log 200, got:\n%s", out)
	}
}

// TestChatStreamEmptyStill502 不回归：空流（0 帧 + 正常 EOF）仍写 error 帧 +
// [DONE] 并收敛 502（与中断流互斥：空流分支只留给正常 EOF 的 0 帧形态）。
func TestChatStreamEmptyStill502(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, "", true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, chatStreamReq(nil))
	})
	body := rec.Body.String()
	if !strings.Contains(body, "empty upstream stream") {
		t.Errorf("empty stream frame missing: %q", body)
	}
	if strings.Contains(body, "upstream stream aborted") {
		t.Errorf("empty stream must not carry the abort frame: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want exactly 1: %q", n, body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("empty stream must still log 502, got:\n%s", out)
	}
}

// TestChatStreamIdleCutoffEndToEnd 端到端（假上游 httptest + 真实出站传输层 +
// IdleTimeout 掐流）：上游吐一帧后静默，空闲监控 cancel 出站 ctx（idle.go）→
// 客户端收到 error 帧 + 恰好一个 [DONE]，日志状态收敛 502。
// 这是生产最常见的断流触发路径（默认 idle_timeout_seconds=300）。
func TestChatStreamIdleCutoffEndToEnd(t *testing.T) {
	withChatLog(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, abortPartialFrame); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-release // 静默：等空闲监控掐流
	}))
	defer func() { close(release); srv.Close() }()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.IdleTimeout = 80 * time.Millisecond

	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, chatStreamReq(nil))
	})
	if rec.Code != 200 {
		t.Fatalf("wire code=%d want 200 (headers already sent)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "半截") {
		t.Errorf("idle 前已吐帧必须透传: %q", body)
	}
	if !strings.Contains(body, "upstream stream aborted") {
		t.Errorf("idle 掐流后客户端须收到 error 帧: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want exactly 1: %q", n, body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("idle 掐流必须收敛 502 观测（不再假 200）, got:\n%s", out)
	}
}
