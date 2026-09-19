package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// assistantRC 提取输出 messages 里各 assistant 消息的 reasoning_content（缺字段返回 ""+false）。
// 返回的切片与 messages 中 assistant 消息一一对应（非 assistant 消息跳过）。
func assistantRC(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	var got []string
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if v, ok := msg["reasoning_content"].(string); ok {
			got = append(got, v)
		} else {
			got = append(got, "<absent>")
		}
	}
	return got
}

// TestBackfillReasoningContentDeepSeek DeepSeek 多轮一致性：门控 thinkingEnabled||hasTrace
// 亮起时（本组请求经 injectThinking 注入 enabled），所有 assistant 消息必须带
// reasoning_content（string）。对齐官方 requiresReasoningContentOnAssistantMessages。
func TestBackfillReasoningContentDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		// 只断言「assistant 消息数」与「每条是否有 reasoning_content 字段」，
		// 值是 string 即可（复制或空串，由具体用例断言）。
		wantCount int
		wantVals  []string // 与 assistant 消息一一对应；空串表示任意 string
	}{
		{"assistant 带 reasoning 无 reasoning_content → 复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a","reasoning":"thought text"}]}`,
			1, []string{"thought text"}},
		{"assistant 带 reasoning_content 原样保留",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"already there"}]}`,
			1, []string{"already there"}},
		{"混合会话全量补上：无 reasoning 的 assistant 补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a1","reasoning":"t1"},
				{"role":"user","content":"u2"},
				{"role":"assistant","content":"a2"}]}`,
			2, []string{"t1", ""}},
		// 新契约（门控 thinkingEnabled||hasTrace，issue #165）：injectThinking 注入
		// enabled 后零痕迹 assistant 也补空串——期望从 <absent> 改为存在 string。
		{"assistant reasoning 为空串 → 视为零痕迹但 enabled 下补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":""}]}`,
			1, []string{""}},
		{"多 assistant 都带 reasoning 全部复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning":"r1"},
				{"role":"assistant","content":"a2","reasoning":"r2"}]}`,
			2, []string{"r1", "r2"}},
		// 官方 "string"!=typeof 语义：非 string reasoning 无从复制，落补 "" 分支。
		{"assistant reasoning 非 string 值（数字）→ 补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":123}]}`,
			1, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != c.wantCount {
				t.Fatalf("assistant 消息数 = %d want %d (out=%s)", len(got), c.wantCount, out)
			}
			for i, want := range c.wantVals {
				if want == "" {
					continue // 任意 string（补空串/复制都接受，但必须存在）
				}
				if got[i] != want {
					t.Errorf("assistant[%d].reasoning_content = %q want %q (out=%s)", i, got[i], want, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNoTrace 零痕迹 + thinking 非 enabled → 零改动：
// 两个半边（thinkingEnabled / hasTrace）都不亮，不白白给 assistant 消息加字段。
//
// 官方 ReasoningContentBackfillRule 门控 = thinkingEnabled || hasTrace；本用例是
// issue #165 修复的**反向断言**——修复前该形态一律不补（对），但修复后必须仍不补，
// 否则「开思考才补」被放宽成「无条件补」，会给出站 body 平白加字段。
func TestBackfillReasoningContentNoTrace(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled + 纯 text assistant 不动",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"plain answer"}]}`},
		{"无 assistant 消息不动",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
				{"role":"user","content":"u"}]}`},
		{"messages 缺失不动",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			for _, rc := range assistantRC(t, out) {
				if rc != "<absent>" {
					t.Errorf("thinkingEnabled/hasTrace 均不亮却被加 reasoning_content=%q (out=%s)", rc, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNonDeepSeek 非 deepseek 模型零改动：
// reasoning 字段保持原样，不新增 reasoning_content（isDeepSeekModel 闸不动）。
func TestBackfillReasoningContentNonDeepSeek(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("非 deepseek 不应 backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillReasoningContentBothFields 同时带 reasoning 与 reasoning_content：
// 以 reasoning_content 为准（不覆盖），reasoning 字段保留（兼容）——对齐客户端 matches 规则。
func TestBackfillReasoningContentBothFields(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a","reasoning":"t","reasoning_content":"existing"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "existing" {
		t.Errorf("reasoning_content 应以已有值为准: got %v (out=%s)", got, out)
	}
	// 同时确认 reasoning 字段仍原样保留。
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if r, ok := first["reasoning"].(string); !ok || r != "t" {
		t.Errorf("reasoning 字段被改动: %v (out=%s)", first, out)
	}
}

// TestBackfillComposesWithInjectThinking backfill 与 injectThinking 组合语义：
// 显式 disabled 时 reasoning_effort 被删，但 backfill 的 hasTrace 半边照常生效
// （多轮一致性不因关思维链而丢）；disabled + 零痕迹则零改动（thinkingEnabled 半边不亮）。
func TestBackfillComposesWithInjectThinking(t *testing.T) {
	// disabled + 有痕迹 → 照补（复制 reasoning，官方 hasTrace 半边，双方一致）。
	body := `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "thought" {
		t.Errorf("disabled 时 backfill 的 hasTrace 半边仍应生效: got %v (out=%s)", got, out)
	}
	if typ, present := getThinkingType(t, out); !present || typ != "disabled" {
		t.Errorf("thinking.type 应保留 disabled, got %q present=%v", typ, present)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s 应被删除（disabled 时）", k)
		}
	}

	// disabled + 零痕迹 → 零改动（门控两个半边都不亮）。
	body = `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"plain"}]}`
	out = PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("disabled+零痕迹 不应 backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillZeroTraceThinkingEnabled issue #165 复现锚：零痕迹多轮 deepseek，
// 经 injectThinking 注入 enabled 后 thinkingEnabled 半边亮 → 每条 assistant 保证
// reasoning_content 是 string（此处无 reasoning 可复制，全补空串）。
//
// 这正是 issue #165 的原始病灶：第三方客户端不回传推理（历史里零痕迹），
// 修复前只移植了 hasTrace 半边 → 门控永不亮 → 永远不补，与官方行为相悖。
func TestBackfillZeroTraceThinkingEnabled(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2"},
		{"role":"user","content":"u3"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	// 前置：出站确实是注入后的 enabled 形态（thinkingEnabled 判定读注入后请求体）。
	if typ, present := getThinkingType(t, out); !present || typ != "enabled" {
		t.Fatalf("thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}
	got := assistantRC(t, out)
	if len(got) != 2 {
		t.Fatalf("assistant 消息数 = %d want 2 (out=%s)", len(got), out)
	}
	for i, rc := range got {
		if rc != "" { // 既有 string（含空串）即满足；<absent>（键不存在）不满足
			t.Errorf("零痕迹 enabled 形态下 assistant[%d].reasoning_content = %q want 存在且为 \"\" (out=%s)", i, rc, out)
		}
	}
}

// TestBackfillNullAndNonStringNormalized issue #165 null/非 string 归一化：
// 官方跳过条件是 "string"!=typeof reasoning_content 才动手——null/数字会被旧代码
// 的「键存在即跳过」当「已有」漏补，新契约归一化为 ""。
// 注意 hasTrace 半边：reasoning_content 键存在本身即痕迹，门控必然亮。
func TestBackfillNullAndNonStringNormalized(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"reasoning_content:null 归一化为空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":null}]}`},
		{"reasoning_content:数字 归一化为空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":123}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != 1 {
				t.Fatalf("assistant 消息数 = %d want 1 (out=%s)", len(got), out)
			}
			if got[0] != "" {
				t.Errorf("reasoning_content 应归一化为 \"\", got %q (out=%s)", got[0], out)
			}
			// 出站字节级确认：wire 上不得再有 null（键存在即跳过会让 null 原样出站）。
			if strings.Contains(string(out), `"reasoning_content":null`) {
				t.Errorf("wire body 仍含 reasoning_content:null (out=%s)", out)
			}
		})
	}
}

// TestBackfillNullNonStringComposesWithThinkingEnabled 归一化与 thinkingEnabled 半边
// 的交叉形态：thinking 显式 enabled + reasoning_content 非 string（null/数字）时，
// 归一化必须落在**同一** assistant 上，而不是只对「有 reasoning 可复制」的消息生效。
func TestBackfillNullNonStringComposesWithThinkingEnabled(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a1","reasoning_content":null},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2","reasoning":"r2"},
		{"role":"assistant","content":"a3","reasoning_content":123}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	want := []string{"", "r2", ""}
	if len(got) != len(want) {
		t.Fatalf("assistant 消息数 = %d want %d (out=%s)", len(got), len(want), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("assistant[%d].reasoning_content = %q want %q (out=%s)", i, got[i], want[i], out)
		}
	}
}

// TestBackfillHasTraceScopeDeviation 固定 hasTrace 半边**有意的口径偏差**：
// 我们扫全部消息（任一消息带非空 reasoning 或 reasoning_content 键即算痕迹），
// 官方只扫 assistant 消息。差异只落在 disabled 分支（enabled 下官方本就补，
// 两口径同结果），且方向是「更保守地补齐」——非 assistant 携带痕迹时仍补齐
// assistant 侧字段，不会漏补。此用例把该偏差钉住，防止被当成 bug 顺手「修正」
// 而改变 disabled 分支的既有行为。
func TestBackfillHasTraceScopeDeviation(t *testing.T) {
	// user 消息带 reasoning_content + thinking disabled → hasTrace 亮 → assistant 补齐。
	body := `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"user","content":"u","reasoning_content":"user side trace"},
		{"role":"assistant","content":"a"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "" {
		t.Errorf("非 assistant 痕迹应触发 hasTrace 半边（口径偏差，保守补齐）: got %v (out=%s)", got, out)
	}
}

// TestBackfillThinkingEnabledViaInjection 完整链路（注入 → 门控 → 补）：
// 请求不带任何 thinking/reasoning 字段，经 injectThinking 注入 enabled 后
// backfill 必须读到注入结果并生效（管线顺序 injectThinking 先于 backfill）。
func TestBackfillThinkingEnabledViaInjection(t *testing.T) {
	// 反向对照：thinking 显式 disabled 且零痕迹 → 同一 body 结构下零改动。
	bodyEnabled := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a"}]}`
	bodyDisabled := `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"assistant","content":"a"}]}`

	out := PrepareBodyOptWithEfforts([]byte(bodyEnabled), false, nil)
	if typ, present := getThinkingType(t, out); !present || typ != "enabled" {
		t.Fatalf("注入后 thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}
	if got := assistantRC(t, out); len(got) != 1 || got[0] != "" {
		t.Errorf("注入 enabled 后 backfill 未生效: got %v (out=%s)", got, out)
	}

	out = PrepareBodyOptWithEfforts([]byte(bodyDisabled), false, nil)
	if typ, present := getThinkingType(t, out); !present || typ != "disabled" {
		t.Fatalf("disabled 形态 thinking.type=%q present=%v (out=%s)", typ, present, out)
	}
	if got := assistantRC(t, out); len(got) != 1 || got[0] != "<absent>" {
		t.Errorf("disabled 零痕迹不应 backfill: got %v (out=%s)", got, out)
	}
}

// TestBackfillWireBodyZeroTraceThinkingEnabled 端到端（参考 sanitize_test 的
// TestChatStreamWireBodySanitized）：零痕迹多轮 deepseek 经 ChatStream 出站，
// 假上游收到的 wire body 里每条 assistant 都带 reasoning_content（string，空串）。
func TestBackfillWireBodyZeroTraceThinkingEnabled(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"user","content":"u1"},` +
		`{"role":"assistant","content":"a1"},` +
		`{"role":"user","content":"u2"},` +
		`{"role":"assistant","content":"a2"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 wire body：thinking 已注入 enabled，每条 assistant 都补上 reasoning_content。
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v (body=%s)", err, gotBody)
	}
	th, _ := obj["thinking"].(map[string]any)
	if typ, _ := th["type"].(string); typ != "enabled" {
		t.Errorf("wire body thinking.type = %q want enabled (body=%s)", typ, gotBody)
	}
	got := assistantRC(t, gotBody)
	if len(got) != 2 {
		t.Fatalf("wire assistant 消息数 = %d want 2 (body=%s)", len(got), gotBody)
	}
	for i, v := range got {
		if v != "" {
			t.Errorf("wire assistant[%d].reasoning_content = %q want 存在且为 \"\" (body=%s)", i, v, gotBody)
		}
	}
}

// TestBackfillWireBodyNonDeepSeekZeroTrace 端到端反向断言：非 deepseek 模型零痕迹
// 请求出站，wire body 不得新增 reasoning_content（isDeepSeekModel 闸不变）。
func TestBackfillWireBodyNonDeepSeekZeroTrace(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"user","content":"u1"},` +
		`{"role":"assistant","content":"a1"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	if strings.Contains(string(gotBody), "reasoning_content") {
		t.Errorf("非 deepseek wire body 不应出现 reasoning_content: %s", gotBody)
	}
}
