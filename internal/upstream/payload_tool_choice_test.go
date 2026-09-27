package upstream

// payload_tool_choice_test.go tool_choice 归一化的出站形态回归。
//
// 上游依据：hub 4db4e91（fix(tools): keep the tools declaration when
// tool_choice is "none"）。上游实测的故障模式——tool_choice="none" 时
// normalize_tool_choice() 把 tools/functions 整个删掉：
//   - 模型拿不到函数签名、又没有结构化工具通道，却仍被要求完成任务；
//   - 于是把「调用」降级成 DSML / 伪 JSON 文本塞进 content（tool_calls 为空、
//     finish_reason=stop），Agent 客户端解析不到调用只能再追问一轮；
//   - 模型每轮重复 "I'll do it"，上下文每轮 +2 条消息、token 线性膨胀——
//     usage.jsonl 记录 n_msgs 543→622、prompt_tokens 涨到 297k、
//     outcome=client_aborted gen_ms=128262 usage_missing=true（用户手动断开）。
//
// 修法：保留 tools 声明，只让 tool_choice 字段表达「本轮不许调用工具」。
//
// 我们此前逐字复刻了这个 bug（normalizeToolChoice 里 "none" → 删 tool_choice +
// 删 tools/functions，另有一个 suppress() 辅助函数），且 payload_test.go 里
// "none" 出现 0 次——零测试覆盖，所以它一直没被发现。
//
// 上游两条实测结论（本文件把它们钉死）：
//  1. 上游把 tool_choice 声明为 string，发对象形态会 400 code=11101——
//     所以 "none" 必须以字符串透传，不能改写成 {"type":"none"}；
//  2. 保留 tools 后上游仍可能返回 tool_calls（并不真正遵守 "none"），但这比
//     「删 tools → 模型输出不可解析文本 → Agent 原地死循环」严格更好。确实要
//     禁止调用时，客户端不传 tools 即可。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// tcTools 工具声明 fixture（Agent 客户端常见形态）。
var tcTools = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "run_shell",
		"description": "Run a shell command",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []any{"command"},
		},
	},
}

// tcToolsBytes 工具声明的规范形字节（键按 encoding/json 的字典序、无多余空白）。
// 出站管线把 body 解码为 map 再 Marshal，map 键会被字典序重排——只有用规范形
// fixture 构造输入，bytes.Contains(out, fixture) 这条「逐字节」断言才成立
// （它证明 tools 的值一个字节都没被动过，而不是「解码后相同」）。
func tcToolsBytes(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal([]any{tcTools})
	if err != nil {
		t.Fatalf("marshal tools fixture: %v", err)
	}
	return b
}

// tcBody 组装请求体：给定 tool_choice 的 JSON 字面量（nil 表示不带该字段）。
func tcBody(t *testing.T, toolChoiceJSON string, withFunctions bool) []byte {
	t.Helper()
	tools := string(tcToolsBytes(t))
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"do the thing"}]`
	if toolChoiceJSON != "" {
		body += `,"tool_choice":` + toolChoiceJSON
	}
	body += `,"tools":` + tools
	if withFunctions {
		body += `,"functions":` + tools
	}
	return []byte(body + `}`)
}

// TestToolChoiceNoneKeepsToolsDeclaration 核心回归：tool_choice="none"（含大小写/
// 空格变体与对象形态）必须**保留 tools/functions 且逐字节不变**，tool_choice 以
// 字符串 "none" 出站（对象形态会 11101，故不得写成 {"type":"none"}）。
func TestToolChoiceNoneKeepsToolsDeclaration(t *testing.T) {
	wantTools := tcToolsBytes(t)
	cases := []struct {
		name      string
		choice    string // tool_choice 的 JSON 字面量
		functions bool
	}{
		{"字符串 none", `"none"`, true},
		{"字符串 NONE", `"NONE"`, false},
		{"字符串前后空格", `" none "`, true},
		{"字符串 None 混合大小写", `"None"`, false},
		{"对象 type=none", `{"type":"none"}`, true},
		{"对象 type=NONE", `{"type":"NONE"}`, false},
		{"对象 type 前后空格", `{"type":" none "}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts(tcBody(t, c.choice, c.functions), false, nil)

			// 1) tools 值逐字节保留（修复前这里被整个删除）。
			if !bytes.Contains(out, wantTools) {
				t.Errorf("出站 tools 未逐字节保留（tool_choice=%s）:\n%s", c.choice, out)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}
			if _, ok := got["tools"]; !ok {
				t.Error("tools 被删除——这正是上游 4db4e91 修掉的 Agent 死循环成因")
			}

			// 2) tool_choice 必须是字符串 "none"（不是对象、不是被删）。
			tc, ok := got["tool_choice"].(string)
			if !ok {
				t.Fatalf("tool_choice 类型=%T（对象形态上游会 400 code=11101）: %v",
					got["tool_choice"], got["tool_choice"])
			}
			if tc != "none" {
				t.Errorf("tool_choice=%q want \"none\"", tc)
			}

			// 3) 旧字段 functions 同样保留（与 tools 同口径）。
			if c.functions {
				if !bytes.Contains(out, []byte(`"functions":`+string(wantTools))) {
					t.Errorf("functions 未逐字节保留（tool_choice=%s）:\n%s", c.choice, out)
				}
				if _, ok := got["functions"]; !ok {
					t.Error("functions 被删除")
				}
			}
		})
	}
}

// TestToolChoiceNoneToolsSurviveFullPipeline 经完整 prepareBody 管线（含脱敏 +
// 零宽两条净化支路）断言一次：tools 声明仍逐字节在出站 body 里。
// 同时确认净化真的执行了（否则是「管线提前 return」的假绿）。
func TestToolChoiceNoneToolsSurviveFullPipeline(t *testing.T) {
	wantTools := tcToolsBytes(t)
	body := `{"model":"deepseek-v4.1-flash","messages":[` +
		`{"role":"system","content":"` + ccIdentity + `"},` +
		`{"role":"user","content":"do the thing"}],` +
		`"tool_choice":"none","tools":` + string(wantTools) +
		`,"functions":` + string(wantTools) + `}`

	out := PrepareBodyOptRealmHistory([]byte(body), "cn", true, true, ReasoningHistoryFull, nil, nil)

	if !bytes.Contains(out, wantTools) {
		t.Errorf("完整管线后 tools 未逐字节保留:\n%s", out)
	}
	if !bytes.Contains(out, []byte(`"functions":`+string(wantTools))) {
		t.Errorf("完整管线后 functions 未逐字节保留:\n%s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	if got["tool_choice"] != "none" {
		t.Errorf("tool_choice=%v want \"none\"", got["tool_choice"])
	}
	// 反假绿：system 里的身份句确实被净化了 → 管线真的跑过。
	sys, _ := got["messages"].([]any)[0].(map[string]any)["content"].(string)
	if bytes.Contains([]byte(sys), []byte(ccIdentity)) {
		t.Errorf("净化未执行（system 仍含身份句）: %q", sys)
	}
}

// TestToolChoiceNonNoneSpellingsMatchBaseline 非 "none" 取值语义一字不动：
// 变体写法的出站 body 必须与「规范写法」的出站 body **逐字节相同**。
// 规范写法 = 修复前代码就会产出的形态（auto/required 原样透传、function 取裸名）。
// 这是既有测试的基线对比手法：同输入走两条入口，断言两者收敛到同一字节。
func TestToolChoiceNonNoneSpellingsMatchBaseline(t *testing.T) {
	cases := []struct {
		name     string
		variant  string
		baseline string
	}{
		{"auto 字符串", `"auto"`, `"auto"`},
		{"auto 对象", `{"type":"auto"}`, `"auto"`},
		{"AUTO 大写对象", `{"type":"AUTO"}`, `"auto"`},
		{"required 字符串", `"required"`, `"required"`},
		{"required 对象", `{"type":"required"}`, `"required"`},
		{"function 对象取裸名", `{"type":"function","function":{"name":"run_shell"}}`, `"run_shell"`},
		{"function 顶层 name 兜底", `{"type":"function","name":"run_shell"}`, `"run_shell"`},
		{"function 无名回落 auto", `{"type":"function"}`, `"auto"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				got := PrepareBodyOptWithEfforts(tcBody(t, c.variant, true), sanitize, nil)
				want := PrepareBodyOptWithEfforts(tcBody(t, c.baseline, true), sanitize, nil)
				if !bytes.Equal(got, want) {
					t.Errorf("sanitize=%v 变体出站与基线不逐字节一致\n got=%s\nwant=%s", sanitize, got, want)
				}
			}
			// 且 tools 逐字节保留（这些取值本来就不动 tools，一并钉住）。
			if out := PrepareBodyOptWithEfforts(tcBody(t, c.variant, true), false, nil); !bytes.Contains(out, tcToolsBytes(t)) {
				t.Errorf("tools 未逐字节保留:\n%s", out)
			}
		})
	}
}

// TestToolChoiceMissingOrUnknownLeavesToolsAlone 缺失 / 未知对象 / 非标量取值：
// 语义一字不动——不发明 tool_choice（缺失时）、畸形取值删除 tool_choice 字段、
// tools 一律原样保留（修复前 suppress() 只在 "none" 路径调用，这里锁死它没被扩大）。
func TestToolChoiceMissingOrUnknownLeavesToolsAlone(t *testing.T) {
	wantTools := tcToolsBytes(t)
	cases := []struct {
		name   string
		choice string // 空串 = 不带 tool_choice 字段
	}{
		{"缺失", ``},
		{"未知对象", `{"type":"bogus"}`},
		{"对象无 type", `{}`},
		{"数字", `123`},
		{"布尔", `true`},
		{"数组", `["none"]`},
		{"null", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts(tcBody(t, c.choice, true), false, nil)
			if !bytes.Contains(out, wantTools) {
				t.Errorf("tools 未逐字节保留:\n%s", out)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}
			if v, ok := got["tool_choice"]; ok {
				t.Errorf("tool_choice 应被删除/不发明，got %v (%T)", v, v)
			}
		})
	}
}

// TestToolChoiceNoneWireBodyKeepsTools 端到端：经真实 ChatStream 出站，假上游
// 收到的 wire body 里 tools 逐字节仍在、tool_choice 是字符串 "none"。
// 单测直调管线不足以覆盖 prepareBody 链路（参考 sanitize_test 的写法）。
func TestToolChoiceNoneWireBodyKeepsTools(t *testing.T) {
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

	rc, status, respBody, err := c.ChatStream(acct, tcBody(t, `"none"`, true), "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	if !bytes.Contains(gotBody, tcToolsBytes(t)) {
		t.Errorf("wire body 丢失 tools（Agent 死循环成因回归）:\n%s", gotBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v (body=%s)", err, gotBody)
	}
	if obj["tool_choice"] != "none" {
		t.Errorf("wire body tool_choice=%v want \"none\"", obj["tool_choice"])
	}
	if _, ok := obj["tools"]; !ok {
		t.Error("wire body 里 tools 被删除")
	}
	if _, ok := obj["functions"]; !ok {
		t.Error("wire body 里 functions 被删除")
	}
}
