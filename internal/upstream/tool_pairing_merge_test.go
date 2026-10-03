// tool_pairing_merge_test.go 出站工具配对「合并步」的行为钉桩（吸收上游 PR #93 的
// mergeAdjacentToolCalls）。断言口径来自 2026-09-20 的线上实机定位：上游对
// 「声明 tool_calls 的 assistant 之后必须紧跟它自己的结果」有硬校验，违反即
// 400 code=11148（tool_call_sequence_broken）并顶死整条会话（换号无效）。
//
// 我们的缺口证据：本仓 grep mergeAdjacentToolCalls 零命中，payload.go 的出站管线
// 只有「repack → cleanup」两步——部分 OpenAI 兼容 agent 客户端把同一批并行工具调用
// 拆成多条紧邻的 assistant 消息回放时，两条相邻的 assistant.tool_calls 原样出站，
// deepseek 系模型判 11148，客户端重放同一历史 → 换号无效、整条会话报废。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// mergeMsgs 解析测试用的 messages JSON 数组。
func mergeMsgs(t *testing.T, s string) []any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("parse messages: %v", err)
	}
	return v
}

// mergeSummary 把消息序列压成可读摘要：assistant 带 tool_calls 记 "assistant(id1,id2)"，
// tool 记 "tool(id)"，其余记 "role(-)"。用于逐条比对「合并后长什么样」。
func mergeSummary(messages []any) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, "<non-object>")
			continue
		}
		role, _ := msg["role"].(string)
		if role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			out = append(out, "tool("+id+")")
			continue
		}
		ids := []string{}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				if tc, ok := tci.(map[string]any); ok {
					id, _ := tc["id"].(string)
					ids = append(ids, id)
				}
			}
		}
		if len(ids) > 0 {
			out = append(out, role+"("+strings.Join(ids, ",")+")")
			continue
		}
		out = append(out, role+"(-)")
	}
	return out
}

func assertMergeSummary(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("消息数不符:\n got %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("消息[%d] = %s want %s\n got %v\nwant %v", i, got[i], want[i], got, want)
		}
	}
}

// TestMergeAdjacentToolCalls 背靠背的两条 assistant.tool_calls 必须合成一条。
//
// 这是部分 OpenAI 兼容 agent 客户端回放并行工具调用的报文形状（同一批调用
// 拆成多条独立 assistant 消息），上游 deepseek 系模型对它判 11148。
func TestMergeAdjacentToolCalls(t *testing.T) {
	const (
		aC00 = `{"role":"assistant","content":null,"tool_calls":[{"id":"c00","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		aC01 = `{"role":"assistant","content":null,"tool_calls":[{"id":"c01","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		tC00 = `{"role":"tool","tool_call_id":"c00","content":"r0"}`
		tC01 = `{"role":"tool","tool_call_id":"c01","content":"r1"}`
	)

	t.Run("并行两条合成一条（修复目标形态）", func(t *testing.T) {
		// 输入即线上复现 11148 的报文：assistant(c00) 之后紧跟 assistant(c01)。
		in := mergeMsgs(t, "["+aC00+","+aC01+","+tC00+","+tC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望发生合并（两条相邻 assistant.tool_calls 未合并 → 上游 11148）")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(c00,c01)", "tool(c00)", "tool(c01)"})
		// 拼接顺序必须是声明顺序（= 结果的 wire 顺序），不得反转。
		tcs := out[0].(map[string]any)["tool_calls"].([]any)
		if id, _ := tcs[0].(map[string]any)["id"].(string); id != "c00" {
			t.Errorf("合并后首个 tool_call = %q want c00", id)
		}
	})

	t.Run("三条连续全并", func(t *testing.T) {
		aC02 := `{"role":"assistant","content":null,"tool_calls":[{"id":"c02","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		in := mergeMsgs(t, "["+aC00+","+aC01+","+aC02+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望发生合并")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(c00,c01,c02)"})
	})

	t.Run("已是规范形态原样返回且零改动", func(t *testing.T) {
		merged := `{"role":"assistant","content":null,"tool_calls":[{"id":"c00"},{"id":"c01"}]}`
		in := mergeMsgs(t, "["+merged+","+tC00+","+tC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，规范形态不应被改动（上层据此判 no-op）")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(c00,c01)", "tool(c00)", "tool(c01)"})
	})

	t.Run("中间隔着消息不合并（非背靠背）", func(t *testing.T) {
		in := mergeMsgs(t, "["+aC00+`,{"role":"user","content":"x"},`+aC01+","+tC00+","+tC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，非相邻不应合并（凭猜测合并会改变语义）")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(c00)", "user(-)", "assistant(c01)", "tool(c00)", "tool(c01)"})
	})

	t.Run("后一条 content 非空不合并", func(t *testing.T) {
		in := mergeMsgs(t, "["+aC00+`,{"role":"assistant","content":"text","tool_calls":[{"id":"c01"}]}`+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，content 非空不应合并（无法无损拼接）")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(c00)", "assistant(c01)"})
	})

	t.Run("前一条无 tool_calls 不合并", func(t *testing.T) {
		in := mergeMsgs(t, `[{"role":"assistant","content":"hi"},`+aC00+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，前一条无 tool_calls 不应合并")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(-)", "assistant(c00)"})
	})

	t.Run("reasoning_content 逐条保留（换行拼接）", func(t *testing.T) {
		in := mergeMsgs(t, `[{"role":"assistant","content":null,"reasoning_content":"r1","tool_calls":[{"id":"c00"}]},{"role":"assistant","content":null,"reasoning_content":"r2","tool_calls":[{"id":"c01"}]}]`)
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望合并")
		}
		rc, _ := out[0].(map[string]any)["reasoning_content"].(string)
		if rc != "r1\nr2" {
			t.Errorf("reasoning_content = %q want %q（deepseek 多轮要求回填思维链，丢弃会换一个错误）", rc, "r1\nr2")
		}
	})

	t.Run("首条即 assistant.tool_calls 不越界", func(t *testing.T) {
		out, changed := mergeAdjacentToolCalls(mergeMsgs(t, "["+aC00+"]"))
		if changed {
			t.Error("单条消息不应改动")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"assistant(c00)"})
	})

	t.Run("非对象元素原样保留且不阻断后续合并", func(t *testing.T) {
		in := mergeMsgs(t, `["str",`+aC00+","+aC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望 aC00/aC01 合并")
		}
		assertMergeSummary(t, mergeSummary(out), []string{"<non-object>", "assistant(c00,c01)"})
	})
}

// TestMergeAdjacentToolCallsEmptyArrayContent 空数组 content 也必须触发合并：
// 有客户端把「没有正文」发成 `content: []`（而不是 null），此前按「非字符串一律
// 非空」处理 → 背靠背的 assistant(tool_calls) 不合并 → 上游 deepseek 系判 11148。
func TestMergeAdjacentToolCallsEmptyArrayContent(t *testing.T) {
	in := mergeMsgs(t, `[
		{"role":"assistant","content":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":[],"tool_calls":[{"id":"c2","type":"function","function":{"name":"B","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"r1"},
		{"role":"tool","tool_call_id":"c2","content":"r2"}
	]`)
	out, changed := mergeAdjacentToolCalls(in)
	if !changed {
		t.Fatal("content:[] 的两条 assistant(tool_calls) 未合并（11148 残留形态）")
	}
	if len(out) != 3 {
		t.Fatalf("合并后长度 = %d, want 3", len(out))
	}
	tcs, _ := out[0].(map[string]any)["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("首条 tool_calls = %d, want 2", len(tcs))
	}

	// 合并只以后一条（被并入方）的 content 是否为空为判据：并入不覆盖前一条的
	// content，所以前一条是**非空数组**时同样应当合并，且内容必须原样保留。
	in2 := mergeMsgs(t, `[
		{"role":"assistant","content":[{"type":"text","text":"hi"}],"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":[],"tool_calls":[{"id":"c2","type":"function","function":{"name":"B","arguments":"{}"}}]}
	]`)
	out2, changed2 := mergeAdjacentToolCalls(in2)
	if !changed2 {
		t.Fatal("后一条 content 为空数组时应合并")
	}
	first, _ := out2[0].(map[string]any)
	if c, ok := first["content"].([]any); !ok || len(c) != 1 {
		t.Fatalf("合并后前一条 content 丢失: %#v", first["content"])
	}
	if tcs, _ := first["tool_calls"].([]any); len(tcs) != 2 {
		t.Fatalf("合并后 tool_calls = %d, want 2", len(tcs))
	}
}

// TestEmptyContent 空判定必须认四种形态：缺失/nil、空串、空数组算空；非空数组与
// 非空字符串不算空（宁可漏合并，也不丢内容）。
func TestEmptyContent(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, true},
		{"空串", "", true},
		{"空数组", []any{}, true},
		{"非空串", "text", false},
		{"非空数组", []any{map[string]any{"type": "text", "text": "hi"}}, false},
		{"数字（畸形）", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := emptyContent(c.v); got != c.want {
				t.Errorf("emptyContent(%#v) = %v want %v", c.v, got, c.want)
			}
		})
	}
}

// TestFoldTextIntoPrevToolCall 反向形态：前一条是带 tool_calls 但没正文的 assistant，
// 本条是纯正文 assistant（正文排在工具调用之后）——折进前一条，合成
// assistant(正文 + tool_calls)，让「工具调用紧跟自己的结果」在两种拆分顺序下都成立。
func TestFoldTextIntoPrevToolCall(t *testing.T) {
	in := mergeMsgs(t, `[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":"我来解释一下"},
		{"role":"tool","tool_call_id":"c1","content":"r1"}
	]`)
	out, changed := mergeAdjacentToolCalls(in)
	if !changed {
		t.Fatal("正文未折进前一条工具调用")
	}
	if len(out) != 2 {
		t.Fatalf("合并后长度 = %d, want 2", len(out))
	}
	first, _ := out[0].(map[string]any)
	if first["content"] != "我来解释一下" {
		t.Fatalf("content = %#v, want 正文", first["content"])
	}
	if tcs, _ := first["tool_calls"].([]any); len(tcs) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(tcs))
	}
	if role, _ := out[1].(map[string]any)["role"].(string); role != "tool" {
		t.Fatalf("第二条应为 tool，实际 %q（上游 11148 校验的关键）", role)
	}

	// 反向守卫：前一条没有 tool_calls 时不得折叠（那是两条独立 assistant 轮次）。
	in2 := mergeMsgs(t, `[
		{"role":"assistant","content":"第一轮"},
		{"role":"assistant","content":"第二轮"}
	]`)
	if _, changed2 := mergeAdjacentToolCalls(in2); changed2 {
		t.Fatal("前一条无 tool_calls 时不该折叠")
	}

	// 反向守卫：正文是数组（可能含多模态块）时不折叠，交给 repack 原样处理。
	in3 := mergeMsgs(t, `[
		{"role":"assistant","content":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":[{"type":"text","text":"hi"}]}
	]`)
	if _, changed3 := mergeAdjacentToolCalls(in3); changed3 {
		t.Fatal("数组正文不该被折叠（会丢结构）")
	}
}

// TestPrepareBodyMergesSplitParallelToolCalls 全链路：出站管线必须把「拆开的并行调用」
// 归一到上游认可的形态（merge 步在 repack 之前——先合并，repack 才看得到完整一批调用）。
func TestPrepareBodyMergesSplitParallelToolCalls(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"跑两个命令"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_00_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_01_b","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_00_a","content":"/opt"},
		{"role":"tool","tool_call_id":"call_01_b","content":"a b c"}
	]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal 出站 body: %v", err)
	}
	messages, _ := obj["messages"].([]any)
	assertMergeSummary(t, mergeSummary(messages), []string{
		"user(-)", "assistant(call_00_a,call_01_b)", "tool(call_00_a)", "tool(call_01_b)",
	})
	// 逐条复核：不得出现「带 tool_calls 的 assistant 紧跟另一条带 tool_calls 的 assistant」。
	prevHadCalls := false
	for i, m := range messages {
		msg, _ := m.(map[string]any)
		hasCalls := false
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasCalls = true
		}
		if prevHadCalls && hasCalls {
			t.Fatalf("消息[%d] 仍是两条相邻的 assistant.tool_calls（上游判 11148）: %v", i, mergeSummary(messages))
		}
		prevHadCalls = hasCalls
	}
}
