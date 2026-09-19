package upstream

// reasoning_mirror_test.go reasoning 字段镜像写（issue #165 追评）+ sanitize 覆盖。
//
// 上游 sliver 5657229 在 backfillReasoningContent 的循环体里追加镜像写：部分账号/租户
// 校验 assistant 消息的 reasoning 字段 len(reasoning)>0（缺失/null/空串 400，空白串 200），
// 官方 CLI 本就给 assistant 挂上一轮 reasoning 文本（itemsToMessages 的
// applyPendingReasoning）——「每条 assistant 保证 reasoning 非空」是官方出站形态，
// 不是 hack。
//
// 规则（严格照上游）：
//   - reasoning 已是非空 string → 不覆盖；
//   - reasoning 缺失/null/空串且 reasoning_content 有来源文本 → 写入同一份文本（镜像）；
//   - 两者皆无/皆空 → 补单个空格 " "（上游 len>0 不 trim：空白串过闸、空串不过）。
//
// 关键约束（本文件的核心，也是本网关与上游的唯一分歧点）：镜像写发生在 sanitize
// **之前**（payload.go 的 backfillReasoningContent → sanitizeMessages 顺序），而
// sanitizeMessages 原先只净化 reasoning_content——照搬上游却不把 reasoning 纳入净化，
// 等于把指纹文本复制进一个不受净化的新字段，出站 body 里 reasoning 原样带指纹
// （11128 类误杀的新来源面）。故镜像写与 sanitize 覆盖必须同批落地，本文件同时锁死
// 两件事，且「sanitize 覆盖」用例是可失败的承重用例（只洗 rc 不洗 reasoning 时必红）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// assistantReasoningField 提取输出 messages 里各 assistant 消息的 reasoning 字段。
// 缺字段返回 "<absent>"，非 string 值返回 "<non-string>"，其余返回原值（含空串）。
// 返回切片与 messages 中 assistant 消息一一对应（与 assistantRC 同构）。
func assistantReasoningField(t *testing.T, out []byte) []string {
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
		if v, ok := msg["reasoning"]; !ok {
			got = append(got, "<absent>")
		} else if s, ok := v.(string); ok {
			got = append(got, s)
		} else {
			got = append(got, "<non-string>")
		}
	}
	return got
}

// TestBackfillReasoningFieldMirrors rc → reasoning 镜像写规则表：
// 已有非空不覆盖 / 缺失复制 / null 与空串归一 / 非 string 归一 / 皆无补单个空格。
// 同时断言 reasoning_content 的值不受镜像写影响（rc 语义本任务不动）。
func TestBackfillReasoningFieldMirrors(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantR  []string // 与 assistant 消息一一对应
		wantRC []string // 同上；"<absent>" 表示该字段应缺失
	}{
		{
			"rc 非空 → 镜像写入相同值",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"source text"}]}`,
			[]string{"source text"}, []string{"source text"},
		},
		{
			"reasoning 已有非空值 → 不覆盖",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":"kept","reasoning_content":"other"}]}`,
			[]string{"kept"}, []string{"other"},
		},
		{
			"rc 非空 + reasoning 为 null → 归一为 rc 值",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":null,"reasoning_content":"source text"}]}`,
			[]string{"source text"}, []string{"source text"},
		},
		{
			"rc 非空 + reasoning 为空串 → 覆盖为 rc 值",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":"","reasoning_content":"source text"}]}`,
			[]string{"source text"}, []string{"source text"},
		},
		{
			"rc 非空 + reasoning 非 string（数字）→ 归一为 rc 值",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":123,"reasoning_content":"source text"}]}`,
			[]string{"source text"}, []string{"source text"},
		},
		{
			"reasoning 非空且无 rc → rc 复制（既有语义）且 reasoning 不动",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":"thought"}]}`,
			[]string{"thought"}, []string{"thought"},
		},
		{
			"混合会话：有痕迹的镜像、无痕迹的补单个空格",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a1","reasoning":"t1"},
				{"role":"user","content":"u2"},
				{"role":"assistant","content":"a2"}]}`,
			[]string{"t1", " "}, []string{"t1", ""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			gotR := assistantReasoningField(t, out)
			gotRC := assistantRC(t, out)
			if len(gotR) != len(c.wantR) {
				t.Fatalf("assistant 消息数 = %d want %d (out=%s)", len(gotR), len(c.wantR), out)
			}
			for i := range c.wantR {
				if gotR[i] != c.wantR[i] {
					t.Errorf("assistant[%d].reasoning = %q want %q (out=%s)", i, gotR[i], c.wantR[i], out)
				}
				if gotRC[i] != c.wantRC[i] {
					t.Errorf("assistant[%d].reasoning_content = %q want %q（rc 语义不应被镜像写改动）(out=%s)",
						i, gotRC[i], c.wantRC[i], out)
				}
			}
		})
	}
}

// TestBackfillReasoningFieldPlaceholderIsSingleSpace 占位必须是**单个空格**而不是空串：
// 上游 len(reasoning)>0 不 trim——空白串过闸、空串 400。逐字节断言（长度 1 且是 U+0020），
// 不用 TrimSpace 之类的宽松比较，否则「占位被洗成空串」会被漏判。
func TestBackfillReasoningFieldPlaceholderIsSingleSpace(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a1","reasoning":"t1"},
		{"role":"assistant","content":"a2"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantReasoningField(t, out)
	if len(got) != 2 {
		t.Fatalf("assistant 消息数 = %d want 2 (out=%s)", len(got), out)
	}
	if got[1] != " " {
		t.Errorf("无痕迹 assistant 的 reasoning 占位 = %q (len=%d) want 单个空格 \" \" (out=%s)",
			got[1], len(got[1]), out)
	}
}

// TestSanitizeTextKeepsWhitespacePlaceholder 占位空格在 sanitizeText 下必须存活：
// 若净化层把它 TrimSpace 成空串，占位就失去「过 len>0 闸」的意义（空串 400）。
func TestSanitizeTextKeepsWhitespacePlaceholder(t *testing.T) {
	if out := sanitizeText(" "); out != " " {
		t.Errorf("占位空格被净化层改动: %q (len=%d) want \" \"", out, len(out))
	}
}

// TestBackfillReasoningFieldSurvivesSanitize 占位空格在完整管线（sanitize=true）后仍存在
// 且仍非空——端到端锁死「出站 reasoning 非空」这条上游校验语义。
func TestBackfillReasoningFieldSurvivesSanitize(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a1","reasoning":"t1"},
		{"role":"assistant","content":"a2"}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", true, false, nil, nil)
	got := assistantReasoningField(t, out)
	if len(got) != 2 {
		t.Fatalf("assistant 消息数 = %d want 2 (out=%s)", len(got), out)
	}
	for i, r := range got {
		if r == "" {
			t.Errorf("assistant[%d].reasoning 净化后为空串（会 400）(out=%s)", i, out)
		}
	}
	if got[1] != " " {
		t.Errorf("assistant[1].reasoning = %q want 单个空格（占位不得被净化层改动）(out=%s)", got[1], out)
	}
}

// TestSanitizeCoversReasoningField 本任务的承重用例：镜像写把 reasoning_content 的值
// 复制进 reasoning，而 sanitizeMessages 原先只净化 reasoning_content——若不把 reasoning
// 纳入净化，指纹文本就经这个新字段原样出站（11128 类误杀的新来源面）。
//
// 断言（结构性取值，不靠 marshalled 子串——Go 的 json.Marshal 对 map 按 key 排序，
// 子串匹配会因字段顺序变化而漏判）：
//   - reasoning 与 reasoning_content 都不得含原始指纹；
//   - 两者都必须出现改写后的形态（证明是「净化」而不是「删字段」）；
//   - 两者都非空，且值相同（同源同口径：镜像值 + 同一 sanitizeText）。
func TestSanitizeCoversReasoningField(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning_content":"` + ccIdentity + `"}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", true, false, nil, nil)
	asst := msgAt(t, out, 1)

	vals := map[string]string{}
	for _, key := range []string{"reasoning", "reasoning_content"} {
		v, ok := asst[key].(string)
		if !ok {
			t.Fatalf("%s 缺失或非 string: %#v (out=%s)", key, asst[key], out)
		}
		if strings.Contains(v, ccIdentity) {
			t.Errorf("%s 残留原始指纹——指纹经新字段出站（新通道）: %q (out=%s)", key, v, out)
		}
		if !strings.Contains(v, "CLI tool") {
			t.Errorf("%s 未见改写后形态（未走 sanitizeText）: %q (out=%s)", key, v, out)
		}
		if v == "" {
			t.Errorf("%s 被净化成空串（应保留改写后文本）(out=%s)", key, out)
		}
		vals[key] = v
	}
	if vals["reasoning"] != vals["reasoning_content"] {
		t.Errorf("两字段净化后应同值（镜像同源 + 同一 sanitizeText）: reasoning=%q reasoning_content=%q (out=%s)",
			vals["reasoning"], vals["reasoning_content"], out)
	}
}

// TestSanitizeCoversReasoningFieldWireBody 端到端（CN realm，走 ChatStream 全链路）：
// 上游实际收到的 body 里，两个字段都不得残留指纹。这是「指纹不因新字段出站」的最终锚点。
func TestSanitizeCoversReasoningFieldWireBody(t *testing.T) {
	body := marshalBody(t, "deepseek-v4-flash", []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "a1", "reasoning_content": ccIdentity},
		map[string]any{"role": "user", "content": "again"},
		map[string]any{"role": "assistant", "content": "a2"},
	})
	gotBody := wireBodyCN(t, body)

	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 数变了: %d (%s)", len(msgs), gotBody)
	}
	for _, idx := range []int{1, 3} {
		asst, _ := msgs[idx].(map[string]any)
		if asst == nil {
			t.Fatalf("messages[%d] 非对象: %v", idx, msgs[idx])
		}
		// reasoning 是上游 len>0 的校验位：无论有无痕迹，出站都必须存在且非空。
		r, ok := asst["reasoning"].(string)
		if !ok {
			t.Errorf("wire messages[%d].reasoning 缺失或非 string: %#v", idx, asst["reasoning"])
			continue
		}
		if r == "" {
			t.Errorf("wire messages[%d].reasoning 为空串——上游 len>0 校验会 400", idx)
		}
		if strings.Contains(r, ccIdentity) {
			t.Errorf("wire messages[%d].reasoning 残留指纹（新通道泄露）: %q", idx, r)
		}
		// reasoning_content 是既有字段（净化面早已覆盖）：有痕迹那条必须净化，
		// 无痕迹那条按既有语义可以是空串（见 backfill_test.go 的 wantVals）。
		rc, ok := asst["reasoning_content"].(string)
		if !ok {
			t.Errorf("wire messages[%d].reasoning_content 缺失或非 string: %#v", idx, asst["reasoning_content"])
			continue
		}
		if strings.Contains(rc, ccIdentity) {
			t.Errorf("wire messages[%d].reasoning_content 残留指纹: %q", idx, rc)
		}
		// 有痕迹那条（idx=1）：镜像值 = 净化后的指纹文本；无痕迹那条（idx=3）：占位单空格。
		if idx == 1 {
			if !strings.Contains(r, "CLI tool") {
				t.Errorf("wire messages[%d].reasoning 未净化（应见改写后形态）: %q", idx, r)
			}
			if rc == "" || r != rc {
				t.Errorf("wire messages[%d] 两字段应同值且非空: reasoning=%q reasoning_content=%q", idx, r, rc)
			}
		} else if r != " " {
			t.Errorf("wire messages[%d].reasoning = %q want 占位单个空格", idx, r)
		}
	}
	if strings.Contains(string(gotBody), ccIdentity) {
		t.Errorf("wire body 整体仍含指纹: %s", gotBody)
	}
}

// TestBackfillReasoningFieldNonDeepSeek 非 deepseek 模型零改动：reasoning 不补、不镜像
// （isDeepSeekModel 闸全域维持——11155 只在 deepseek thinking 形态出现）。
func TestBackfillReasoningFieldNonDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // "<absent>" 表示应保持缺失
	}{
		{"缺失不补", `{"model":"glm-5.2","messages":[
			{"role":"assistant","content":"a"}]}`, "<absent>"},
		{"已有原样保留", `{"model":"glm-5.2","messages":[
			{"role":"assistant","content":"a","reasoning":"kept"}]}`, "kept"},
		{"有 rc 痕迹也不镜像", `{"model":"glm-5.2","messages":[
			{"role":"assistant","content":"a","reasoning_content":"trace"}]}`, "<absent>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantReasoningField(t, out)
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("非 deepseek reasoning 应零改动: got %v want %q (out=%s)", got, c.want, out)
			}
		})
	}
}

// TestBackfillReasoningFieldAssistantOnly 只动 assistant：user 消息上的同名/同源字段
// 一律不碰（镜像写的作用域边界）。
func TestBackfillReasoningFieldAssistantOnly(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u","reasoning":"user-side"},
		{"role":"assistant","content":"a"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	user := msgAt(t, out, 0)
	if r, _ := user["reasoning"].(string); r != "user-side" {
		t.Errorf("user 消息的 reasoning 被改动: %q (out=%s)", r, out)
	}
	if _, ok := user["reasoning_content"]; ok {
		t.Errorf("user 消息不应被补 reasoning_content (out=%s)", out)
	}
	got := assistantReasoningField(t, out)
	if len(got) != 1 || got[0] != " " {
		t.Errorf("assistant 应被补占位空格: %v (out=%s)", got, out)
	}
}

// TestBackfillReasoningFieldSanitizeWashResidual 已知残余（记录而非修复）：当 rc 的值
// 是**纯指纹块**（如整段 header kv），sanitizeText 会整段删除并 TrimSpace → 空串。
// 镜像写发生在 sanitize 之前（补占位时 rc 还非空，故走的是镜像分支而非占位分支），
// 于是两字段净化后都是空串——对「校验 len(reasoning)>0」的租户，这条窄路径仍会 400。
//
// 为何不在此处修：本任务的契约是 reasoning 与 reasoning_content **完全同口径**净化
// （同源同值），单方面给 reasoning 加「净化后若空则再补占位」的补偿逻辑会打破这份对称、
// 引入上游没有的第二次改写；且该残余对 rc 是**既有行为**（改动前 rc 就被洗成空串），
// 不是本次引入的回归。故记录为已知残余，留给后续批次按需评估（例如 sanitize 收紧
// 空白处理时一并决定）。本用例锁死「残余存在且形态已知」，避免将来静默变化。
func TestBackfillReasoningFieldSanitizeWashResidual(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a","reasoning_content":"` + ccHeader + `"}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", true, false, nil, nil)
	asst := msgAt(t, out, 0)
	r, _ := asst["reasoning"].(string)
	rc, _ := asst["reasoning_content"].(string)
	// 残余形态：两字段同为净化后的值（此处为纯指纹块被整段删除后的空串）。
	if r != rc {
		t.Errorf("两字段净化后必须同值（同口径契约）: reasoning=%q reasoning_content=%q (out=%s)", r, rc, out)
	}
	// 关键不变量：无论残余如何，出站都不得残留原始指纹。
	if strings.Contains(r, "x-anthropic-billing-hdr") || strings.Contains(rc, "x-anthropic-billing-hdr") {
		t.Errorf("残余路径下仍残留指纹: reasoning=%q reasoning_content=%q (out=%s)", r, rc, out)
	}
	t.Logf("已知残余：纯指纹 rc 形态净化后 reasoning=%q (len=%d) reasoning_content=%q (len=%d)",
		r, len(r), rc, len(rc))
}
