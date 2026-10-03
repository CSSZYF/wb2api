// sse_error_frame_observer_test.go 上游 error 帧观察者（WithErrorFrameObserver）
// 的行为钉桩（吸收上游 PR #93）。
//
// 契约：观察者只在**带 error 键**的帧上触发一次、拿到原始 payload；透传字节不变
// （error-passthrough 语义：code/message/requestId 照常到达客户端）；正常数据帧
// 零回调；三参调用（无选项）与既有 Stream/StreamHint 行为逐字节一致。
package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamHintErrorFrameObserver 上游 error 帧必须旁路通知观察者（账号处置挂载点），
// 且透传字节不变——error 帧原文（code/msg/requestId）照常到达客户端。
// 正常数据帧不得触发观察者。
func TestStreamHintErrorFrameObserver(t *testing.T) {
	const errPayload = `{"error":{"code":6004,"message":"模型限流，将在 2026-09-27 01:00:00 重置"}}`
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: " + errPayload + "\n\n" +
		"data: [DONE]\n\n"

	var observed []string
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithErrorFrameObserver(func(payload string) {
		observed = append(observed, payload)
	})); err != nil {
		t.Fatal(err)
	}

	if len(observed) != 1 || observed[0] != errPayload {
		t.Fatalf("观察者回调 = %v want [%s]", observed, errPayload)
	}
	// 透传字节不变：error 帧原文必须仍在响应里（error-passthrough 语义）。
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("error 帧原文未透传: %s", rec.Body.String())
	}

	// 正常流（无 error 帧）：观察者零回调。
	var normalObserved int
	raw2 := "data: {\"id\":\"x2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec2 := httptest.NewRecorder()
	if err := StreamHint(rec2, strings.NewReader(raw2), nil, WithErrorFrameObserver(func(payload string) {
		normalObserved++
	})); err != nil {
		t.Fatal(err)
	}
	if normalObserved != 0 {
		t.Fatalf("正常流触发了观察者 %d 次，want 0", normalObserved)
	}
}

// TestStreamHintOptionsBackwardCompatible 三参调用（无选项）必须与加选项时逐字节
// 一致——variadic 只增加旁路观测，绝不改变透传。同时锁住空流/中断流的本地兜底帧
// **不**触发观察者（网关本地故障形态没有上游原文，不编造）。
func TestStreamHintOptionsBackwardCompatible(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"

	recPlain := httptest.NewRecorder()
	if err := StreamHint(recPlain, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	recOpts := httptest.NewRecorder()
	if err := StreamHint(recOpts, strings.NewReader(raw), nil, WithErrorFrameObserver(func(string) {})); err != nil {
		t.Fatal(err)
	}
	if recPlain.Body.String() != recOpts.Body.String() {
		t.Fatalf("加选项改变了透传字节:\n plain=%q\n  opts=%q", recPlain.Body.String(), recOpts.Body.String())
	}

	// 空流：本地兜底 error 帧（"empty upstream stream"）不得触发观察者。
	var localObserved int
	recEmpty := httptest.NewRecorder()
	if err := StreamHint(recEmpty, strings.NewReader(""), nil, WithErrorFrameObserver(func(string) {
		localObserved++
	})); err == nil {
		t.Fatal("空流应返回 errEmptyStream")
	}
	if localObserved != 0 {
		t.Fatalf("网关本地兜底帧触发了观察者 %d 次（不编造上游原文）", localObserved)
	}
}

// TestStreamErrorFrameObserverHintCoexists 观察者与 gateway_hint 共存：两者都在
// error 帧上工作，且互不干扰（hint 只加 error.gateway_hint 字段，观察者拿到的是
// **原始** payload——不含 hint 字段）。
func TestStreamErrorFrameObserverHintCoexists(t *testing.T) {
	const errPayload = `{"error":{"code":6004,"message":"rate limited"}}`
	raw := "data: " + errPayload + "\n\n" + "data: [DONE]\n\n"

	var observed string
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(raw), func(payload string) string {
		return "rate limited by upstream; retry after reset"
	}, WithErrorFrameObserver(func(payload string) { observed = payload }))
	if err != nil {
		t.Fatal(err)
	}
	if observed != errPayload {
		t.Errorf("观察者应拿到原始 payload（不含 hint）: %q", observed)
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("hint 应仍被附加: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Errorf("error 原文应仍被透传: %s", rec.Body.String())
	}
}
