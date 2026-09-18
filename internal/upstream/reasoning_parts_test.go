package upstream

// reasoning_parts_test.go CN 域 reasoning content-part 转换（见 reasoning_parts.go）。
//
// 现场回归：ZCode 3.11.2 把思考内容作为 content 数组里的 {"type":"reasoning"} part 发出，
// CN 域（copilot.tencent.com）以 HTTP 400 code=11101
// "Parse message failed: unsupported content type at index 0: reasoning" 拒绝；
// global 域（www.workbuddy.ai）接受该格式。修法：CN 域把 part 提升为顶层
// reasoning_content 字符串字段（text 一字不动），global 域原样透传。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// msgsOf 解出输出 body 的 messages 数组（未解析出即 fatal）。
func msgsOf(t *testing.T, out []byte) []any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages 缺失 (out=%s)", out)
	}
	return msgs
}

// msgAt 取 messages[i] 并断言是对象。
func msgAt(t *testing.T, out []byte, i int) map[string]any {
	t.Helper()
	msgs := msgsOf(t, out)
	if i >= len(msgs) {
		t.Fatalf("messages 只有 %d 条，取不到 [%d] (out=%s)", len(msgs), i, out)
	}
	m, ok := msgs[i].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] 非对象: %v", i, msgs[i])
	}
	return m
}

// TestPromoteReasoningPartsCN 主用例：reasoning part 提升为顶层 reasoning_content，
// 数组里其余 part（text / image_url）原样保留数组形态、内容逐字不变。
//
// 文本里带前后空格与换行，锁死「不做 TrimSpace、不做任何文本改写」这条不变量。
func TestPromoteReasoningPartsCN(t *testing.T) {
	const (
		reason = "  [A]\nChen reports: 现场复现  "
		answer = "answer text kept verbatim\n"
	)
	body := `{"model":"glm-5.2","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"reasoning","text":"  [A]\nChen reports: 现场复现  "},
			{"type":"text","text":"answer text kept verbatim\n"},
			{"type":"image_url","image_url":{"url":"https://example/img.png"}}]}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", false, false, nil, nil)

	asst := msgAt(t, out, 1)
	if got, _ := asst["reasoning_content"].(string); got != reason {
		t.Errorf("顶层 reasoning_content = %q want %q (out=%s)", got, reason, out)
	}
	parts, ok := asst["content"].([]any)
	if !ok {
		t.Fatalf("content 应保留数组形态，got %#v (out=%s)", asst["content"], out)
	}
	if len(parts) != 2 {
		t.Fatalf("content 应剩 2 个 part（text+image），got %d: %v", len(parts), parts)
	}
	p0, _ := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != answer {
		t.Errorf("text part 被改动: %v", p0)
	}
	p1, _ := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Errorf("image part 被改动: %v", p1)
	}
	// 用户消息一字不动。
	user := msgAt(t, out, 0)
	if user["content"] != "hi" {
		t.Errorf("user 消息被改动: %v", user)
	}
}

// TestPromoteReasoningPartsOnlyPart content 数组只有 reasoning part → 删空后置 ""（string），
// 避免上游报 content 不能为空类错误；不是空数组、也不是 null。
func TestPromoteReasoningPartsOnlyPart(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":[{"type":"reasoning","text":"thought only"}]}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", false, false, nil, nil)
	asst := msgAt(t, out, 0)
	if got, ok := asst["content"].(string); !ok || got != "" {
		t.Errorf("content 应为空字符串，got %#v (out=%s)", asst["content"], out)
	}
	if got, _ := asst["reasoning_content"].(string); got != "thought only" {
		t.Errorf("reasoning_content = %q want %q (out=%s)", got, "thought only", out)
	}
}

// TestPromoteReasoningPartsMultiAndExisting 多个 reasoning part 按原顺序拼接；
// 已有非空 reasoning_content 时**追加在后**（两段文本均逐字保留，不插分隔符、不丢内容）。
func TestPromoteReasoningPartsMultiAndExisting(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","reasoning_content":"existing","content":[
			{"type":"reasoning","text":"p1"},
			{"type":"reasoning","text":"p2"},
			{"type":"text","text":"tail"}]}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", false, false, nil, nil)
	asst := msgAt(t, out, 0)
	if got, _ := asst["reasoning_content"].(string); got != "existingp1p2" {
		t.Errorf("reasoning_content = %q want %q (out=%s)", got, "existingp1p2", out)
	}
	if parts, ok := asst["content"].([]any); !ok || len(parts) != 1 {
		t.Fatalf("content 应只剩 1 个 text part，got %#v (out=%s)", asst["content"], out)
	}
}

// TestPromoteReasoningPartsBothRealmsConvert 两个域都转换（实测修正）：
// 最初假设 global 域接受 reasoning part、只 CN 域转换；2026-09-18 实测证伪——
// global 域同样 400 code=11101，转成顶层 reasoning_content 后两域均 200。
// 故本用例锁死「不分域」：global 与 CN 产出必须一致（都提升、都不残留 part）。
func TestPromoteReasoningPartsBothRealmsConvert(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":[
			{"type":"reasoning","text":"  [A]\nthought  "},
			{"type":"text","text":"answer"}]}]}`
	cn := PrepareBodyOptRealm([]byte(body), "cn", false, false, nil, nil)
	gl := PrepareBodyOptRealm([]byte(body), "global", false, false, nil, nil)
	if string(cn) != string(gl) {
		t.Errorf("两个域产出应逐字节相同\ncn =%s\ngl =%s", cn, gl)
	}
	asst := msgAt(t, gl, 0)
	rc, _ := asst["reasoning_content"].(string)
	if rc != "  [A]\nthought  " {
		t.Errorf("global 域 reasoning_content = %q want %q (out=%s)", rc, "  [A]\nthought  ", gl)
	}
	parts, ok := asst["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content 应只剩 1 个 text part，got %#v (out=%s)", asst["content"], gl)
	}
	p0, _ := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "answer" {
		t.Errorf("text part 被改动: %v", p0)
	}
}

// TestPromoteReasoningPartsNoReasoningZeroChange 无 reasoning part 的 body 零改动：
// CN 与 global 两条路径产出逐字节相同（含普通字符串 content、多模态 image part、
// 纯 text part 数组、缺 content 的消息、以及非 assistant 角色带 reasoning part 的情况）。
//
// 非 assistant 带 reasoning part 有意不转换：reasoning_content 是 assistant 侧字段，
// 把别的角色的 part 提到顶层属于语义错位（上游 role 白名单里也只有 assistant 认它）。
func TestPromoteReasoningPartsNoReasoningZeroChange(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"普通字符串 content",
			`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"answer"}]}`},
		{"多模态 image part 数组",
			`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example/i.png"}}]}]}`},
		{"纯 text part 数组（不扁平化）",
			`{"model":"glm-5.2","messages":[{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`},
		{"缺 content 的 assistant（工具调用轮）",
			`{"model":"glm-5.2","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`},
		{"非 assistant 角色带 reasoning part 不动",
			`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"reasoning","text":"not assistant"}]}]}`},
		{"content 空数组",
			`{"model":"glm-5.2","messages":[{"role":"assistant","content":[]}]}`},
		{"messages 缺失",
			`{"model":"glm-5.2"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cn := PrepareBodyOptRealm([]byte(c.body), "cn", false, false, nil, nil)
			gl := PrepareBodyOptRealm([]byte(c.body), "global", false, false, nil, nil)
			if string(cn) != string(gl) {
				t.Errorf("无 reasoning part 时 CN 与 global 产出应逐字节相同\ncn =%s\ngl =%s", cn, gl)
			}
		})
	}
}

// TestPromoteReasoningPartsBeforeBackfill 与 backfillReasoningContent 的先后关系：
// 转换必须在前，backfill 才看得见「历史里有思考痕迹」——提升出的 reasoning_content
// 触发 DeepSeek 多轮一致性，所有 assistant 消息补齐该字段。
//
// 两个域行为一致（转换不分域）：CN 与 global 都必须补齐，不得再有「数组 part 对
// backfill 不可见」的对照组（修复前的 global 行为已被实测证伪为 bug）。
func TestPromoteReasoningPartsBeforeBackfill(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":[{"type":"reasoning","text":"t1"},{"type":"text","text":"a1"}]},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2"}]}`

	for _, realm := range []string{"cn", "global"} {
		out := PrepareBodyOptRealm([]byte(body), realm, false, false, nil, nil)
		got := assistantRC(t, out)
		if len(got) != 2 {
			t.Fatalf("[%s] assistant 消息数 = %d want 2 (out=%s)", realm, len(got), out)
		}
		if got[0] != "t1" {
			t.Errorf("[%s] assistant[0].reasoning_content = %q want %q (out=%s)", realm, got[0], "t1", out)
		}
		if got[1] != "" {
			t.Errorf("[%s] backfill 应给无痕迹的 assistant 补空串，got %q (out=%s)", realm, got[1], out)
		}
	}
}

// TestPromoteReasoningPartsComposesWithSanitize 与 sanitize 的复合：提升出的
// reasoning_content 与数组里的 text part 都被指纹净化覆盖（提升不改变净化覆盖面）。
func TestPromoteReasoningPartsComposesWithSanitize(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":[
			{"type":"reasoning","text":"` + ccIdentity + `"},
			{"type":"text","text":"` + ccBranch + `"}]}]}`
	out := PrepareBodyOptRealm([]byte(body), "cn", true, false, nil, nil)
	asst := msgAt(t, out, 0)
	rc, _ := asst["reasoning_content"].(string)
	if rc == "" || strings.Contains(rc, ccIdentity) {
		t.Errorf("提升出的 reasoning_content 未被净化: %q (out=%s)", rc, out)
	}
	parts, _ := asst["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("content 应只剩 text part，got %#v", asst["content"])
	}
	p0, _ := parts[0].(map[string]any)
	if txt, _ := p0["text"].(string); strings.Contains(txt, ccBranch) {
		t.Errorf("text part 未被净化: %q", txt)
	}
}

// wireBodyCN 起假上游、走 ChatStream 全链路（CN realm），返回上游实际收到的 body。
func wireBodyCN(t *testing.T, body []byte) []byte {
	t.Helper()
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SetSanitizeFingerprints(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	return gotBody
}

// marshalBody 把 messages 直接 marshal 成请求体（真实换行等字符交给 JSON 编码处理，
// 避免手写 JSON 字符串时把 \n 写成裸换行导致整个 body 解析失败——解析失败时管线
// 按防御语义原样透传，会掩盖真正要测的转换行为）。
func marshalBody(t *testing.T, model string, messages []any) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{"model": model, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestChatStreamWireBodyCNReasoningPartPromoted 端到端（CN realm）：发往假上游的 wire body
// 里不得残留任何 "type":"reasoning" part，顶层必须有 reasoning_content 且与 part 原文逐字相同，
// text part 原样保留。这是现场 400 的直接回归。
func TestChatStreamWireBodyCNReasoningPartPromoted(t *testing.T) {
	const (
		reason = "[A]\nChen reports: 对话到一半 503"
		answer = "final answer"
	)
	body := marshalBody(t, "glm-5.2", []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "reasoning", "text": reason},
			map[string]any{"type": "text", "text": answer},
		}},
		map[string]any{"role": "user", "content": "again"},
	})
	gotBody := wireBodyCN(t, body)

	// 逐 part 结构断言（不靠子串）：Go 的 json.Marshal 对 map 按 key 排序输出，
	// 转换失效时 wire 上是 {"text":"…","type":"reasoning"}，子串 `"type":"reasoning"`
	// 恒不出现——只靠子串会把「没转换」误判成通过。
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	for i, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for j, pp := range parts {
			part, ok := pp.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := part["type"].(string); typ == "reasoning" {
				t.Errorf("wire body 残留 reasoning part messages[%d].content[%d]: %v", i, j, part)
			}
		}
	}
	// 现场 400 的原始形态（紧凑 JSON 里 type 在前）也一并锁死。
	if strings.Contains(string(gotBody), `"type":"reasoning"`) {
		t.Errorf("wire body 仍含 reasoning part: %s", gotBody)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages 数变了: %d (%s)", len(msgs), gotBody)
	}
	asst, _ := msgs[1].(map[string]any)
	if got, _ := asst["reasoning_content"].(string); got != reason {
		t.Errorf("wire reasoning_content = %q want %q", got, reason)
	}
	parts, ok := asst["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("wire content 应保留 1 个 text part，got %#v", asst["content"])
	}
	p0, _ := parts[0].(map[string]any)
	if p0["text"] != answer {
		t.Errorf("wire text part 被改动: %v", p0)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
}

// TestChatStreamWireBodyGlobalReasoningPartPromoted 端到端（global realm）：与 CN 同口径
// 必须提升——实测 global 上游同样 400 code=11101，原「global 原样透传」的假设已证伪。
// global 路径会先跑 ensureConsoleSystem（首条非 system 时前置兜底 system），
// 故这里显式带一条 system，assistant 稳定落在 index 1。
func TestChatStreamWireBodyGlobalReasoningPartPromoted(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SetSanitizeFingerprints(true)
	c.ChatBaseGlobal = ts.URL
	// CN base 指向不可达地址：global 账号若误走 CN 路径，本用例会以传输错误失败。
	c.ChatBaseCN = "https://cn.invalid"
	acct := &auth.Auth{AccessToken: "test-token", Domain: "www.workbuddy.ai", UID: "g1"}
	body := marshalBody(t, "glm-5.2", []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "reasoning", "text": "thought"},
			map[string]any{"type": "text", "text": "answer"},
		}},
	})
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数变了: %d (%s)", len(msgs), gotBody)
	}
	asst, _ := msgs[1].(map[string]any)
	if got, _ := asst["reasoning_content"].(string); got != "thought" {
		t.Errorf("global wire reasoning_content = %q want %q", got, "thought")
	}
	parts, ok := asst["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("global wire content 应保留 1 个 text part，got %#v", asst["content"])
	}
	p0, _ := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "answer" {
		t.Errorf("global wire text part 被改动: %v", p0)
	}
	if strings.Contains(string(gotBody), `"type":"reasoning"`) {
		t.Errorf("global wire body 仍含 reasoning part: %s", gotBody)
	}
}
