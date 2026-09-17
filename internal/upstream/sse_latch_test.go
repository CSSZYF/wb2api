package upstream

import (
	"strings"
	"testing"
)

// 本文件覆盖 SSE 聚合的 latch 与合并语义（吸收上游 94bc325/f2e51ab/6701631/11b75d4/
// 5c2db2f 五连 + hub 占位过滤）：
//   - appendContent 是「已取到正文」的唯一写入点：空串不占 latch 名额（issue #142）；
//   - 非 delta message 回退分支与 delta 路径同口径（不重复追加、同构合并字段）；
//   - 缺 index 的 tool_call 按 id 优先/lastIdx 兜底分派，不再一律归 0；
//   - name+arguments 双空的 tool_call 占位被剔除，无真实调用时 finish_reason 降级 stop。

// wantAggContent 断言聚合结果 message.content 等于 want。
func wantAggContent(t *testing.T, raw string, want string) {
	t.Helper()
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg, _ := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("message missing: %#v", resp["choices"])
	}
	if got, _ := msg["content"].(string); got != want {
		t.Errorf("content=%q want %q", got, want)
	}
}

// TestAggregateNonDeltaMessageContentNotDuplicated（6701631）上游把**完整消息**放在
// choices[].message（非 delta）且每帧都带时，正文必须只采一次——此前回退分支只
// WriteString 不置 latch，!gotAnyContent 守卫永不成立，N 帧正文被拼成 N 遍。
func TestAggregateNonDeltaMessageContentNotDuplicated(t *testing.T) {
	wantAggContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":3}}\n\n"+
			"data: [DONE]\n\n",
		"abc")
}

// TestAggregateEmptyDeltaFrameDoesNotLatch（94bc325 S1b）OpenAI 风格 role-only 首帧
// （delta.content=""）不占 latch 名额：后续整条 message 的真正文照常采入。此前空串
// 也置位 latch，真正文被 !gotAnyContent 守卫静默拒绝，输出 ""。
func TestAggregateEmptyDeltaFrameDoesNotLatch(t *testing.T) {
	wantAggContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"真正文\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"真正文")
}

// TestAggregateEmptyMessageFrameDoesNotLatch（94bc325 S1a）整条空 message 帧
// （message.content=""）同样不占 latch 名额：帧 2 的真正文照常采入。
func TestAggregateEmptyMessageFrameDoesNotLatch(t *testing.T) {
	wantAggContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"Hello")
}

// TestAggregateDeltaPriorityOverMessage 空 message 帧不吞 latch 后，delta 优先语义不变
// （#134 守卫保持）：delta 先取到非空正文，之后的整条 message 不再追加。
func TestAggregateDeltaPriorityOverMessage(t *testing.T) {
	wantAggContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"delta\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"message\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"delta")
}

// TestAggregateDeltaStreamInterleavedEmptyContent 空串 delta 帧不追加任何字节，
// 也不影响前后真正文的拼接（appendContent 空串短路）。
func TestAggregateDeltaStreamInterleavedEmptyContent(t *testing.T) {
	wantAggContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"He\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"Hello")
}

// TestAggregateEmptyFrameFieldsStillMerged（94bc325 S4）空 content 帧的其它字段照常合并：
// role / reasoning_content / tool_calls 不因 content 空而丢。
func TestAggregateEmptyFrameFieldsStillMerged(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"id\":\"tc1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got, _ := msg["content"].(string); got != "done" {
		t.Errorf("content=%q want %q", got, "done")
	}
	if got, _ := msg["reasoning_content"].(string); got != "think" {
		t.Errorf("reasoning_content=%q want %q（空 content 帧的 reasoning 不应丢）", got, "think")
	}
	// 生产代码聚合 tool_calls 为 []map[string]any（sse.go calls 构造），按此类型断言。
	tcs, _ := msg["tool_calls"].([]map[string]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len=%d want 1（空 content 帧的 tool_calls 不应丢）: %#v", len(tcs), msg["tool_calls"])
	}
	fn := tcs[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name=%v want get_weather", fn["name"])
	}
}

// TestAggregateNonDeltaMessageMergesToolCalls（11b75d4）非 delta 整条 message 的
// tool_calls / reasoning_content / role 必须与 delta 分支同构合并——此前回退分支
// 只合并 content，这三项全丢。
func TestAggregateNonDeltaMessageMergesToolCalls(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"查一下\",\"reasoning_content\":\"need weather\",\"tool_calls\":[{\"id\":\"call_m1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("role=%v want assistant", msg["role"])
	}
	if msg["content"] != "查一下" {
		t.Errorf("content=%q want 查一下", msg["content"])
	}
	if msg["reasoning_content"] != "need weather" {
		t.Errorf("reasoning_content=%q want need weather（message 分支同构合并）", msg["reasoning_content"])
	}
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v want 1 call（message 分支同构合并）", msg["tool_calls"])
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if fn == nil || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("merged call=%#v", calls[0])
	}
}

// TestAggregateToolCallsNoIndexNotCollapsed（5c2db2f）上游省略 tool_call 的 index
// （规范要求但部分上游省略）时，不同调用不得被合并进同一槽——此前缺 index 一律归 0，
// 多调用被合并、arguments 串联污染、name 互相覆盖。
func TestAggregateToolCallsNoIndexNotCollapsed(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"f2","arguments":"{\"b\":2}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("two distinct tool_calls must not collapse: %#v", msg["tool_calls"])
	}
	// 两个 call 各自独立：arguments 不串联、name 不被覆盖。
	argSets := map[string]bool{}
	names := map[string]bool{}
	for _, c := range calls {
		fn := c["function"].(map[string]any)
		argSets[fn["arguments"].(string)] = true
		names[fn["name"].(string)] = true
	}
	if !argSets[`{"a":1}`] || !argSets[`{"b":2}`] {
		t.Errorf("arguments must stay separate, got %v", argSets)
	}
	if !names["f1"] || !names["f2"] {
		t.Errorf("names must stay separate, got %v", names)
	}
}

// TestAggregateToolCallsMixedIndexAbsent 混合形态：合规的 index-0 call 与缺 index 的
// 新 call 共存——缺 index 的补位不得覆盖既有 index-0 槽（nextToolIndex 递增跳过已占槽）。
func TestAggregateToolCallsMixedIndexAbsent(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"f2","arguments":"{\"b\":2}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("mixed index forms must produce 2 calls: %#v", msg["tool_calls"])
	}
	seen := map[int]bool{}
	for _, c := range calls {
		seen[c["index"].(int)] = true
	}
	if len(seen) != 2 || !seen[0] {
		t.Errorf("indexes=%v want two distinct incl. 0", seen)
	}
}

// TestAggregateToolCallsIdCarriedAcrossFrames 缺 index 但跨帧都带同 id 的延续形态：
// call_a 的碎片必须归位到同一调用（idIndex 命中），不因跨帧拆成两个调用。
func TestAggregateToolCallsIdCarriedAcrossFrames(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"a\":"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":"1}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("same-id fragments must merge into one call: %#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Errorf("arguments=%v want {\"a\":1}", fn["arguments"])
	}
}

// TestAggregatePlaceholderToolCallDowngradesFinish（hub 241be21 语义，防客户端死等）：
// name 与 arguments 双空的 tool_call 占位剔除；全部滤掉且 finish_reason=="tool_calls"
// 时降级为 "stop"——否则严格客户端收到「无 tool_calls 的 tool_calls 结束」会无限挂起。
// 只双空才丢：单空（有 name 无参 / 有参无 name）保留，交客户端 schema 判定。
func TestAggregatePlaceholderToolCallDowngradesFinish(t *testing.T) {
	// A) 纯占位（name/arguments 双空）+ finish_reason=tool_calls → 剔除并降级 stop。
	placeholder := `data: {"id":"x1","choices":[{"index":0,"delta":{"content":"Hello!"}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(placeholder))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("placeholder: finish_reason=%v want stop（降级防死等）", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Errorf("placeholder: tool_calls should be dropped: %#v", msg["tool_calls"])
	}

	// B) 无参工具（有 name、arguments 空）→ 保留，finish_reason 不变。
	nameless := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ok","type":"function","function":{"name":"list_dir","arguments":""}}]},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err = Aggregate(strings.NewReader(nameless))
	if err != nil {
		t.Fatal(err)
	}
	choice = resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("nameless: finish_reason=%v want tool_calls（单空不丢）", choice["finish_reason"])
	}
	msg = choice["message"].(map[string]any)
	if calls, _ := msg["tool_calls"].([]map[string]any); len(calls) != 1 {
		t.Errorf("nameless: single-empty call must be kept: %#v", msg["tool_calls"])
	}

	// C) 有参无 name（内容仍在）→ 保留（只双空才丢）。
	anonName := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_y","type":"function","function":{"name":"","arguments":"{\"k\":\"v\"}"}}]},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err = Aggregate(strings.NewReader(anonName))
	if err != nil {
		t.Fatal(err)
	}
	msg = resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if calls, _ := msg["tool_calls"].([]map[string]any); len(calls) != 1 {
		t.Errorf("args-only call must be kept: %#v", msg["tool_calls"])
	}
}
