package upstream

import (
	"strings"
	"testing"
)

// TestIsTruncatedArguments 单元：空串（无参工具）合法；非空不可解析=截断；可解析=完整。
func TestIsTruncatedArguments(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},                 // 无参工具空分片
		{"   ", false},              // 纯空白
		{"{}", false},               // 合法空对象
		{`{"a":1}`, false},          // 合法对象
		{"null", false},             // 能解析（类型错误交给 schema，非截断）
		{"[1,2]", false},            // 能解析（数组非对象，非截断）
		{`{"command": "ls -`, true}, // 半截 JSON
		{`{"a":`, true},             // 半截键
		{`\deveco-code-rust\crates\deveco`, true}, // 非 JSON 文本
	}
	for _, c := range cases {
		if got := isTruncatedArguments(c.raw); got != c.want {
			t.Errorf("isTruncatedArguments(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}

// TestDropTruncatedToolCalls 过滤残缺调用，完整调用正例零改动（顺序保持）。
func TestDropTruncatedToolCalls(t *testing.T) {
	calls := []map[string]any{
		{"id": "ok", "function": map[string]any{"name": "read", "arguments": `{"file_path":"a.go"}`}},
		{"id": "bad", "function": map[string]any{"name": "read", "arguments": `{"file_path": "`}},
		{"id": "empty", "function": map[string]any{"name": "list_dir", "arguments": ""}},
		{"id": "nofn"},
	}
	out := dropTruncatedToolCalls(calls)
	if len(out) != 3 {
		t.Fatalf("kept=%d want 3, out=%#v", len(out), out)
	}
	if out[0]["id"] != "ok" || out[1]["id"] != "empty" || out[2]["id"] != "nofn" {
		t.Fatalf("order/kept wrong: %#v", out)
	}
}

// TestAggregateTruncatedToolCalls finish_reason==length + 残缺 arguments →
// 不把脏参数交给客户端（残缺调用被剔除，完整调用保留）。
func TestAggregateTruncatedToolCalls(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_bad","type":"function","function":{"name":"read","arguments":"{\"file_path\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason=%v want length", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("truncated tool_calls should NOT be handed to client: %#v", msg["tool_calls"])
	}
}

// TestAggregateEOFDropsTruncatedToolCalls 第二个截断来源：上游连接中断（EOF 收尾、
// 未发 data: [DONE]）——残缺 arguments 同样必须丢弃，否则客户端解析非法 JSON 卡死。
func TestAggregateEOFDropsTruncatedToolCalls(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_bad","type":"function","function":{"name":"exec","arguments":"{\"cmd\":\"ls"}}]}}]}

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("EOF-truncated tool_calls should NOT be handed to client: %#v", msg["tool_calls"])
	}
}

// TestAggregateDoneKeepsCompleteToolCall 不误伤锚：正常 [DONE] 收尾且参数完整 →
// 原样保留（EOF 过滤条件不得破坏正例）。
func TestAggregateDoneKeepsCompleteToolCall(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"a.go\"}"},"index":0}]}}]}

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
		t.Fatalf("[DONE] with complete args must be preserved: %#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"file_path":"a.go"}` {
		t.Fatalf("complete arguments mutated: %v", fn["arguments"])
	}
}

// TestAggregateToolCallsLengthComplete 正例零改动：finish_reason==length 但参数完整
// （以及无参数空串）→ 原样保留。
func TestAggregateToolCallsLengthComplete(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"a.go\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("complete tool_calls under length must be preserved: %#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"file_path":"a.go"}` {
		t.Fatalf("complete arguments mutated: %v", fn["arguments"])
	}
}

// TestAggregateTruncatedDropLogged 偏离上游（本仓新增）：过滤非空时必须留 WARN 日志，
// 不静默丢——排障需要看到「上游流被截断、丢了几个调用」。
func TestAggregateTruncatedDropLogged(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_bad","type":"function","function":{"name":"read","arguments":"{\"file_path\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	out := captureUpstreamLog(t, func() {
		if _, err := Aggregate(strings.NewReader(raw)); err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
	})
	if !strings.Contains(out, "drop 1 truncated tool_call") {
		t.Errorf("truncation drop must be logged (not silent), got:\n%s", out)
	}
	if !strings.Contains(out, "finish=length") {
		t.Errorf("log must carry finish reason for triage, got:\n%s", out)
	}
}

// TestAggregateToolCallsFinishNoCallsDowngrades 上游声明 finish_reason="tool_calls"
// 却压根没发任何调用 → 降级 "stop"（客户端收到「无 tool_calls 的 tool_calls 结束」
// 会无限挂起等工具调用）。
func TestAggregateToolCallsFinishNoCallsDowngrades(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"content":"hi"}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop (no calls at all)", choice["finish_reason"])
	}
}

// TestAggregateLengthWithoutCallsKeepsFinish 无 tool_calls 时截断过滤不误改
// finish_reason（length 是 max_tokens 中止的真实信号，与工具无关）。
func TestAggregateLengthWithoutCallsKeepsFinish(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"content":"hi"}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Errorf("finish_reason=%v want length", choice["finish_reason"])
	}
}

// TestAggregateTruncationBeforePlaceholderOrder 三步顺序协调：先截断过滤 → 后占位
// 过滤 → 最后判空降级。本用例中 finish_reason=="tool_calls" 且 [DONE] 正常收尾，
// 截断检测按上游语义不触发（只认 length/EOF）——残缺调用保留，而双空占位仍被
// 第 2 步剔除，最终剩 1 个调用，finish_reason 保持 tool_calls。
func TestAggregateTruncationBeforePlaceholderOrder(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"p","function":{"name":"","arguments":""}}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"bad","function":{"name":"r","arguments":"{\"a\":"}}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]map[string]any)
	if len(calls) != 1 || calls[0]["id"] != "bad" {
		t.Fatalf("placeholder dropped / truncated kept under tool_calls+DONE: %#v", msg["tool_calls"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls (one call remains)", choice["finish_reason"])
	}
}
