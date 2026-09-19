package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 本文件覆盖流式中断兜底（移植上游 hub aaeefc4 的两处 streaming catch）：
//   - 非 EOF 读错误（空闲超时掐流 / 半截读 / 连接重置）不再裸返回：补 error 帧
//     + [DONE] 终结，并返回 errStreamAborted 供调用方收敛失败观测；
//   - 「恰好一个 [DONE]」不变量在三类收尾（正常/空流/中断）下均保持；
//   - 客户端已走（终结帧写失败）返回写错误而非哨兵，调用方据此不误标 502。

// abortFrame 一段有效数据帧（中断流用例的「已吐字节」部分）。
const abortFrame = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n"

// abortAfterReader 先原样吐 prefix，再返回固定读错误（模拟上游中途断流）。
type abortAfterReader struct {
	prefix string
	err    error
	done   bool
}

func (r *abortAfterReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		if r.prefix != "" {
			return copy(p, r.prefix), nil
		}
	}
	return 0, r.err
}

// doneCount 统计响应体里 "data: [DONE]" 出现次数（恰好一个不变量）。
func doneCount(body string) int { return strings.Count(body, "data: [DONE]") }

// errorFrames 收集响应体里的 error 帧 payload（非 JSON 行忽略）。
func errorFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.TrimPrefix(ln, "data: ") == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(ln[6:]), &obj) != nil {
			continue
		}
		if _, has := obj["error"]; has {
			out = append(out, obj)
		}
	}
	return out
}

// TestStreamMidwayAbortWritesErrorFrameAndDone 上游吐了一帧后断流：
// 已吐帧照常透传，末尾补 error 帧（code=upstream_aborted）+ 恰好一个 [DONE]，
// 并返回 IsStreamAbortedError 可识别的错误（handler 据此收敛 502 观测）。
func TestStreamMidwayAbortWritesErrorFrameAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, &abortAfterReader{prefix: abortFrame, err: io.ErrUnexpectedEOF})
	if !IsStreamAbortedError(err) {
		t.Fatalf("err=%v want IsStreamAbortedError (中途断流须可识别)", err)
	}
	if IsEmptyStreamError(err) {
		t.Errorf("中途断流不得判成空流（两者语义互斥）: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("已吐帧必须照常透传: %q", body)
	}
	if n := doneCount(body); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	efs := errorFrames(t, body)
	if len(efs) != 1 {
		t.Fatalf("error frames=%d want 1: %q", len(efs), body)
	}
	em, _ := efs[0]["error"].(map[string]any)
	if em == nil || em["message"] != "upstream stream aborted" || em["code"] != "upstream_aborted" {
		t.Errorf("abort frame payload wrong: %v", efs[0])
	}
	// 终结帧顺序：error 帧在 [DONE] 之前（客户端先知道失败再收尾）。
	if strings.Index(body, "upstream stream aborted") > strings.Index(body, "data: [DONE]") {
		t.Errorf("error frame must precede [DONE]: %q", body)
	}
}

// TestStreamAbortBeforeFirstFrameNotReportedAsEmptyStream 一帧未吐即断流：
// 读错误比「空流」更具体（空流留给上游正常 EOF 却 0 帧的形态），
// 报 aborted 而非 empty，且不叠加两帧 error（互斥）。
func TestStreamAbortBeforeFirstFrameNotReportedAsEmptyStream(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, &abortAfterReader{err: context.Canceled})
	if !IsStreamAbortedError(err) {
		t.Fatalf("err=%v want IsStreamAbortedError", err)
	}
	if IsEmptyStreamError(err) {
		t.Errorf("0 帧的中断流不得判成空流: %v", err)
	}
	body := rec.Body.String()
	if n := doneCount(body); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	if efs := errorFrames(t, body); len(efs) != 1 {
		t.Errorf("error frames=%d want 1 (中断优先于空流，不叠加): %q", len(efs), body)
	}
	if strings.Contains(body, "empty upstream stream") {
		t.Errorf("中断流不得再写空流帧: %q", body)
	}
}

// TestStreamAbortAfterDoneStaysClean 末行已是 [DONE] 且同时带读错误（半截读常见形态）：
// 上游已给终结标记 = 正常收尾，不按中断处理（无 error 帧、err 为 nil、恰好一个 [DONE]）。
func TestStreamAbortAfterDoneStaysClean(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, &abortAfterReader{prefix: abortFrame + "data: [DONE]\n\n", err: io.ErrUnexpectedEOF})
	if err != nil {
		t.Fatalf("err=%v want nil (已收 [DONE] 即正常收尾)", err)
	}
	body := rec.Body.String()
	if n := doneCount(body); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	if efs := errorFrames(t, body); len(efs) != 0 {
		t.Errorf("正常收尾不得有 error 帧: %q", body)
	}
}

// TestStreamAbortWriteFailureNotReportedAsUpstreamFault 客户端已走（终结帧写失败）：
// 返回的是**写错误**而不是 aborted 哨兵——调用方据此不把「人已走」误标 502。
func TestStreamAbortWriteFailureNotReportedAsUpstreamFault(t *testing.T) {
	clientGone := errors.New("client gone: broken pipe")
	w := &failAfterWriter{ok: 1, err: clientGone} // 第 1 次写（数据帧）成功，终结帧失败
	err := Stream(w, &abortAfterReader{prefix: abortFrame, err: io.ErrUnexpectedEOF})
	if err == nil {
		t.Fatal("want write error, got nil")
	}
	if IsStreamAbortedError(err) || IsEmptyStreamError(err) {
		t.Errorf("客户端已走必须返回写错误而非上游缺陷哨兵: %v", err)
	}
	if !errors.Is(err, clientGone) {
		t.Errorf("err=%v want wrap of %v", err, clientGone)
	}
}

// failAfterWriter 前 ok 次写成功，之后返回固定错误（模拟客户端断连后写失败）。
type failAfterWriter struct {
	mu   sync.Mutex
	hdr  http.Header
	ok   int
	err  error
	body strings.Builder
}

func (w *failAfterWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *failAfterWriter) WriteHeader(int) {}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ok <= 0 {
		return 0, w.err
	}
	w.ok--
	return w.body.Write(p)
}

// TestStreamIdleTimeoutAbortTerminalFrames 真实触发路径端到端：上游吐一帧后静默，
// IdleTimeout 掐流出站 ctx（idle.go）→ Stream 收到读错误 → 补 error 帧 + [DONE]。
// 这是生产里最常见的断流形态（默认 idle_timeout_seconds=300）。
func TestStreamIdleTimeoutAbortTerminalFrames(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, abortFrame); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-release // 静默：不再吐数据，等监控掐流或测试收尾
	}))
	defer func() { close(release); srv.Close() }()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 80 * time.Millisecond

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	serr := Stream(rec, rc)
	if !IsStreamAbortedError(serr) {
		t.Fatalf("err=%v want IsStreamAbortedError (idle 掐流须收敛为中断)", serr)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("idle 前已吐帧必须透传: %q", body)
	}
	if n := doneCount(body); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	if efs := errorFrames(t, body); len(efs) != 1 {
		t.Errorf("error frames=%d want 1: %q", len(efs), body)
	}
}

// TestStreamAbortTerminalFramesNoGatewayHint 中断兜底帧是网关本地形态（无上游原文），
// 即使 hintFn 对任意 payload 都返回非空串，也不得附加 gateway_hint（不编造）。
func TestStreamAbortTerminalFramesNoGatewayHint(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamHint(rec, &abortAfterReader{prefix: abortFrame, err: io.ErrUnexpectedEOF},
		func(string) string { return "some hint" })
	if !IsStreamAbortedError(err) {
		t.Fatalf("err=%v want IsStreamAbortedError", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "gateway_hint") {
		t.Errorf("本地兜底帧不得携带 gateway_hint: %q", body)
	}
	if n := doneCount(body); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
}

// TestStreamAbortErrorKeepsCause 返回错误保留底层读错误原文（日志排障用），
// 且可被 errors.Is 命中哨兵（handler 的判别口径）。
func TestStreamAbortErrorKeepsCause(t *testing.T) {
	cause := fmt.Errorf("read tcp 10.0.0.1:443: i/o timeout")
	err := Stream(httptest.NewRecorder(), &abortAfterReader{prefix: abortFrame, err: cause})
	if !errors.Is(err, errStreamAborted) {
		t.Fatalf("err=%v must wrap errStreamAborted", err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("底层原因须保留在错误里: %v", err)
	}
}
