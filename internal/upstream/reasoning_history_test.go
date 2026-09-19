package upstream

// reasoning_history_test.go features.reasoning_history（出站历史推理文本裁剪）三档语义。
//
// 上游计费实测（2026-09-19，真实请求打 deepseek-v4.1-flash / global 域）：
//
//	出站 assistant 消息                                  上游 prompt_tokens
//	带 tool_calls，无推理字段                                346
//	带 tool_calls + reasoning_content(40000 字符)          9234
//	带 tool_calls + reasoning(40000 字符)                  9234
//	带 tool_calls + 两字段各 40000 字符                     9234（不翻倍，只算一份）
//	带 tool_calls + 两字段都是单个空格 " "                    346（零成本）
//	不带 tool_calls + reasoning_content(40000 字符)           42（完全不计）
//
// 结论：上游只对「带 tool_calls 的 assistant 消息」计算推理文本 token，两字段只算一份、
// 空白串零成本；而真实客户端（ZCode 3.11.2）给每条 assistant 挂完整推理文本，长会话里
// 推理约占**总 token 的一半**（887 条消息实测 376 条带推理、合计 178 万字符）。
//
// 本文件锁死三件事（缺一即回归）：
//  1. 三档语义（full 零改动 / last 只留最后一条 / blank 全置占位），两个字段同步；
//  2. 占位串口径（字段保留且非空，单个 U+0020；与 thinking.go 206b 镜像写同口径）；
//  3. 裁剪步在 sanitizeMessages **之后**（净化成空串的 rc 被补成 " "，顺带修
//     sanitize.go 记录的「rc 净化后为空 → 租户 400」残余）。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 测试用 body（多行只为可读，JSON 语义与单行等价）。

// rhBodyDeepSeekBoth deepseek + 两条 assistant 各带 reasoning_content 与 reasoning 原文。
const rhBodyDeepSeekBoth = `{"model":"deepseek-v4-flash","messages":[
	{"role":"user","content":"u1"},
	{"role":"assistant","content":"a1","reasoning_content":"rc1","reasoning":"r1"},
	{"role":"user","content":"u2"},
	{"role":"assistant","content":"a2","reasoning_content":"rc2","reasoning":"r2"}]}`

// rhBodyGlmRCOnly glm（非 deepseek，无 backfill）+ 两条 assistant 只带 reasoning_content。
const rhBodyGlmRCOnly = `{"model":"glm-5.2","messages":[
	{"role":"assistant","content":"a1","reasoning_content":"rc1"},
	{"role":"user","content":"u"},
	{"role":"assistant","content":"a2","reasoning_content":"rc2"}]}`

// TestReasoningHistoryTrimModes 三档取值 × 典型 body 的裁剪结果（表驱动）。
func TestReasoningHistoryTrimModes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		sanitize bool
		mode     string
		wantRC   []string // 与 assistant 消息一一对应；"<absent>" = 字段缺失
		wantR    []string // 同上
	}{
		{
			"full：deepseek 两字段原文零改动",
			rhBodyDeepSeekBoth, false, ReasoningHistoryFull,
			[]string{"rc1", "rc2"}, []string{"r1", "r2"},
		},
		{
			"last：只留最后一条原文，前面置单个空格（两字段同步）",
			rhBodyDeepSeekBoth, false, ReasoningHistoryLast,
			[]string{" ", "rc2"}, []string{" ", "r2"},
		},
		{
			"blank：全部置单个空格（两字段同步）",
			rhBodyDeepSeekBoth, false, ReasoningHistoryBlank,
			[]string{" ", " "}, []string{" ", " "},
		},
		{
			"last：非 deepseek 同样裁剪（新步不分模型）",
			rhBodyGlmRCOnly, false, ReasoningHistoryLast,
			[]string{" ", "rc2"}, []string{"<absent>", "<absent>"},
		},
		{
			"blank：非 deepseek 同样裁剪，且不凭空创建 reasoning 字段",
			rhBodyGlmRCOnly, false, ReasoningHistoryBlank,
			[]string{" ", " "}, []string{"<absent>", "<absent>"},
		},
		{
			"full：非 deepseek 零改动",
			rhBodyGlmRCOnly, false, ReasoningHistoryFull,
			[]string{"rc1", "rc2"}, []string{"<absent>", "<absent>"},
		},
		{
			// keeper 判定：rc 为空串但 reasoning 有原文 → 该条仍算「带推理文本」，
			// 保留其原文；同一条里空的那个字段补占位（保留原文优先，空则占位）。
			"last：keeper 的空字段补占位、非空字段保原文",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning_content":"","reasoning":"r1"}]}`,
			false, ReasoningHistoryLast,
			[]string{" "}, []string{"r1"},
		},
		{
			"blank：空串字段补占位",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning_content":"","reasoning":"r1"}]}`,
			false, ReasoningHistoryBlank,
			[]string{" "}, []string{" "},
		},
		{
			// 非 deepseek 下 reasoning 空串不会被 backfill 镜像成 rc 的值，
			// 正好用来验证「keeper 保留非空原文、空字段补占位」的裁剪口径本身。
			"last：keeper 的 reasoning 空串补占位、rc 保原文",
			`{"model":"glm-5.2","messages":[
				{"role":"assistant","content":"a1","reasoning_content":"rc1","reasoning":""}]}`,
			false, ReasoningHistoryLast,
			[]string{"rc1"}, []string{" "},
		},
		{
			// null 同样算「字段存在但为空」：裁剪档下必须写成非空占位
			// （租户校验 len>0，缺失/null/空串 400、空白串 200）。
			"blank：null 字段补占位",
			`{"model":"glm-5.2","messages":[
				{"role":"assistant","content":"a1","reasoning_content":null}]}`,
			false, ReasoningHistoryBlank,
			[]string{" "}, []string{"<absent>"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptRealmHistory([]byte(c.body), "cn", c.sanitize, false, c.mode, nil, nil)
			if got := assistantRC(t, out); !equalStrings(got, c.wantRC) {
				t.Errorf("assistant.reasoning_content = %q want %q (out=%s)", got, c.wantRC, out)
			}
			if got := assistantReasoningField(t, out); !equalStrings(got, c.wantR) {
				t.Errorf("assistant.reasoning = %q want %q (out=%s)", got, c.wantR, out)
			}
		})
	}
}

// TestReasoningHistoryLastKeepsLastOnly last 档「最后一条」判定：多条 assistant 带推理时
// 只有最后一条保留原文，前面的都变占位；末尾无推理字段的 assistant 不参与判定、
// 也不被凭空加字段（keeper 仍是更早那条带文本的）。
func TestReasoningHistoryLastKeepsLastOnly(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a1","reasoning_content":"t1"},
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a2","reasoning_content":"t2"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a3"}]}`
	out := PrepareBodyOptRealmHistory([]byte(body), "cn", false, false, ReasoningHistoryLast, nil, nil)
	want := []string{" ", "t2", "<absent>"}
	if got := assistantRC(t, out); !equalStrings(got, want) {
		t.Errorf("last 档 assistant.reasoning_content = %q want %q (out=%s)", got, want, out)
	}
	// 三档里只有一条原文存活：raw 字节上确认前两条的原文已被清掉。
	s := string(out)
	if strings.Contains(s, `"t1"`) {
		t.Errorf("last 档不该保留更早那条的推理原文: %s", out)
	}
	if !strings.Contains(s, `"reasoning_content":"t2"`) {
		t.Errorf("last 档必须逐字保留最后一条的推理原文: %s", out)
	}
}

// TestReasoningHistoryRunsAfterSanitize 顺序约束：裁剪步必须在 sanitizeMessages 之后。
//
// sanitize 会把纯指纹块的 rc 整段删除成空串（sanitize.go 记录的「已知残余」：
// rc 净化后为空 → 租户 len>0 校验 400）。裁剪步在其后即可把空串补成 " "，顺带修掉
// 这条路径；若裁剪先于 sanitize，补好的占位没有净化需求（无害），但被清空的 rc
// 永远停在空串、无人补位（有害）。
//
// 本用例的最后一个子用例是**可失败承重用例**：它同时锁死「keeper 判定基于净化后文本」
// ——若先裁剪后净化，keeper 会选中第二条（裁剪时它还是纯指纹原文），净化再把它清成空串，
// 结果是既丢了第一条的原文、又留下空串（最坏组合）。
func TestReasoningHistoryRunsAfterSanitize(t *testing.T) {
	// ccHeader 是 sanitize_test.go 里的纯指纹块（整段删除 → 空串）。
	fpBody := `{"model":"glm-5.2","messages":[{"role":"assistant","content":"a1","reasoning_content":` +
		jsonString(ccHeader) + `}]}`
	fpRC := func(out []byte) []string { return assistantRC(t, out) }

	// full 档：既有残余原样保留（本功能不改变 full 档任何行为，残余仍在）。
	out := PrepareBodyOptRealmHistory([]byte(fpBody), "cn", true, false, ReasoningHistoryFull, nil, nil)
	if got := fpRC(out); !equalStrings(got, []string{""}) {
		t.Errorf("full 档不该改动净化后的空串（既有残余）: %q (out=%s)", got, out)
	}
	// last 档：唯一一条 assistant 的 rc 净化后为空 → 没有 keeper → 补占位。
	out = PrepareBodyOptRealmHistory([]byte(fpBody), "cn", true, false, ReasoningHistoryLast, nil, nil)
	if got := fpRC(out); !equalStrings(got, []string{" "}) {
		t.Errorf("last 档应把净化后的空串补成占位: %q (out=%s)", got, out)
	}
	// blank 档：同上。
	out = PrepareBodyOptRealmHistory([]byte(fpBody), "cn", true, false, ReasoningHistoryBlank, nil, nil)
	if got := fpRC(out); !equalStrings(got, []string{" "}) {
		t.Errorf("blank 档应把净化后的空串补成占位: %q (out=%s)", got, out)
	}

	// 承重用例：第一条有真原文、第二条是纯指纹块（净化后为空）。
	mixed := `{"model":"glm-5.2","messages":[` +
		`{"role":"assistant","content":"a1","reasoning_content":"real text"},` +
		`{"role":"assistant","content":"a2","reasoning_content":` + jsonString(ccHeader) + `}]}`
	out = PrepareBodyOptRealmHistory([]byte(mixed), "cn", true, false, ReasoningHistoryLast, nil, nil)
	if got := fpRC(out); !equalStrings(got, []string{"real text", " "}) {
		t.Errorf("keeper 判定须基于净化后文本（保留真原文、空串补占位）: %q (out=%s)", got, out)
	}
}

// TestReasoningHistoryPlaceholderIsSingleSpace 占位口径：被裁剪的推理字段必须**存在**、
// 是 string、非空（len>0）——上游对这两个字段做 len>0 校验（不 trim），空串/缺失 400、
// 空白串 200；被裁掉的那些一律是单个 U+0020（与 206b 镜像写同口径，不自创占位）。
func TestReasoningHistoryPlaceholderIsSingleSpace(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a1","reasoning_content":"text"},
		{"role":"assistant","content":"a2","reasoning_content":""},
		{"role":"assistant","content":"a3","reasoning_content":null}]}`
	// last：第一条是 keeper（唯一带非空文本者），另两条（空串 / null）补占位。
	// blank：三条全部补占位。
	want := map[string][]string{
		ReasoningHistoryLast:  {"text", " ", " "},
		ReasoningHistoryBlank: {" ", " ", " "},
	}
	for mode, wantRC := range want {
		out := PrepareBodyOptRealmHistory([]byte(body), "cn", false, false, mode, nil, nil)
		if got := assistantRC(t, out); !equalStrings(got, wantRC) {
			t.Errorf("mode=%s assistant.reasoning_content = %q want %q (out=%s)", mode, got, wantRC, out)
		}
		// 结构断言：每个存在的字段都是非空 string（空串/null 一律已被补位）。
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v (out=%s)", err, out)
		}
		msgs, _ := obj["messages"].([]any)
		for i, mm := range msgs {
			msg, _ := mm.(map[string]any)
			for _, key := range []string{"reasoning_content", "reasoning"} {
				v, ok := msg[key]
				if !ok {
					continue
				}
				s, isStr := v.(string)
				if !isStr {
					t.Errorf("mode=%s msg[%d].%s 应为 string，实际 %T", mode, i, key, v)
					continue
				}
				if len(s) == 0 {
					t.Errorf("mode=%s msg[%d].%s 不得为空串（上游 len>0 校验）", mode, i, key)
				}
			}
		}
		// 字节断言：出站 JSON 里出现的是 " "，不是 ""。
		if strings.Contains(string(out), `"reasoning_content":""`) {
			t.Errorf("mode=%s 出站不该出现空串 rc: %s", mode, out)
		}
		if !strings.Contains(string(out), `"reasoning_content":" "`) {
			t.Errorf("mode=%s 出站应出现单空格占位: %s", mode, out)
		}
	}
}

// TestReasoningHistoryNonAssistantUntouched 只动 assistant：user / tool / system 消息
// （即使携带同名推理字段）一字不动，也不被凭空改写。
func TestReasoningHistoryNonAssistantUntouched(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"s","reasoning_content":"sys-rc"},
		{"role":"user","content":"u","reasoning_content":"u-rc","reasoning":"u-r"},
		{"role":"tool","content":"t","reasoning_content":"t-rc"},
		{"role":"assistant","content":"a1","reasoning_content":"a1-rc"},
		{"role":"assistant","content":"a2","reasoning_content":"a2-rc"}]}`
	want := map[string][]string{
		// last：前四条一字不动；assistant 只留最后一条（a2）。
		ReasoningHistoryLast: {
			"system|sys-rc/<absent>",
			"user|u-rc/u-r",
			"tool|t-rc/<absent>",
			"assistant| /<absent>",
			"assistant|a2-rc/<absent>",
		},
		// blank：assistant 两条都置占位，非 assistant 三条仍一字不动。
		ReasoningHistoryBlank: {
			"system|sys-rc/<absent>",
			"user|u-rc/u-r",
			"tool|t-rc/<absent>",
			"assistant| /<absent>",
			"assistant| /<absent>",
		},
	}
	for mode, wantFields := range want {
		out := PrepareBodyOptRealmHistory([]byte(body), "cn", false, false, mode, nil, nil)
		if got := roleReasoningField(t, out); !equalStrings(got, wantFields) {
			t.Errorf("mode=%s 消息字段 = %q want %q (out=%s)", mode, got, wantFields, out)
		}
	}
}

// TestReasoningHistoryIdempotent 幂等：已是占位的字段不重复改写（裁剪档下二次经过管线
// 产出不变），保证「裁剪过的历史再被裁剪」不会产生第二形态。
func TestReasoningHistoryIdempotent(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a1","reasoning_content":"rc1"},
		{"role":"assistant","content":"a2","reasoning_content":"rc2"}]}`
	for _, mode := range []string{ReasoningHistoryLast, ReasoningHistoryBlank} {
		first := PrepareBodyOptRealmHistory([]byte(body), "cn", false, false, mode, nil, nil)
		second := PrepareBodyOptRealmHistory(first, "cn", false, false, mode, nil, nil)
		if string(first) != string(second) {
			t.Errorf("mode=%s 二次裁剪产出不同:\nfirst =%s\nsecond=%s", mode, first, second)
		}
	}
}

// TestReasoningHistoryWireBodyExact 端到端字节断言：完整出站 JSON 与期望串逐字节相等
// （不是只看结构取值）——锁死「两字段被替换、其余字段一字不动」。
func TestReasoningHistoryWireBodyExact(t *testing.T) {
	const body = `{"model":"glm-5.2","messages":[` +
		`{"role":"assistant","content":"a1","reasoning_content":"rc1"},` +
		`{"role":"assistant","content":"a2","reasoning_content":"rc2"}]}`
	cases := []struct {
		mode string
		want string
	}{
		{
			ReasoningHistoryBlank,
			`{"messages":[{"content":"a1","reasoning_content":" ","role":"assistant"},` +
				`{"content":"a2","reasoning_content":" ","role":"assistant"}],` +
				`"model":"glm-5.2","stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			ReasoningHistoryLast,
			`{"messages":[{"content":"a1","reasoning_content":" ","role":"assistant"},` +
				`{"content":"a2","reasoning_content":"rc2","role":"assistant"}],` +
				`"model":"glm-5.2","stream":true,"stream_options":{"include_usage":true}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			out := PrepareBodyOptRealmHistory([]byte(body), "cn", false, false, c.mode, nil, nil)
			if string(out) != c.want {
				t.Errorf("wire body 不匹配\n got=%s\nwant=%s", out, c.want)
			}
		})
	}
}

// TestReasoningHistoryFullAndInvalidByteIdentical 零回归锚：full 档与非法值档
// （空串/未知/大小写与空白变体归一后仍是 full）的出站 body 必须与既有入口
// PrepareBodyOptRealm 逐字节相同——覆盖多种 body（deepseek 两字段、非 deepseek、
// reasoning part 数组、指纹句、tool_calls、无 messages、坏 JSON）。
func TestReasoningHistoryFullAndInvalidByteIdentical(t *testing.T) {
	bodies := []string{
		rhBodyDeepSeekBoth,
		rhBodyGlmRCOnly,
		`{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":[` +
			`{"type":"reasoning","text":"thinking..."},{"type":"text","text":"answer"}]}]}`,
		`{"model":"glm-5.2","messages":[{"role":"system","content":` + jsonString(ccIdentity) + `},` +
			`{"role":"user","content":"hi"}]}`,
		`{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":null,` +
			`"reasoning_content":"rc","reasoning":"r","tool_calls":[{"id":"c1","type":"function",` +
			`"function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`,
		`{"model":"glm-5.2","messages":[]}`,
		`{"model":"glm-5.2"}`,
		`not json at all`,
		``,
	}
	// 非法/归一后仍为 full 的取值：出站必须与基线逐字节一致。
	modes := []string{
		ReasoningHistoryFull,
		"",
		"FULL",
		"  full  ",
		"bogus",
		"lastt",
		"true",
		"1",
		"none",
		"complete",
	}
	for _, sanitize := range []bool{true, false} {
		for bi, b := range bodies {
			baseline := PrepareBodyOptRealm([]byte(b), "cn", sanitize, false, nil, nil)
			for _, mode := range modes {
				out := PrepareBodyOptRealmHistory([]byte(b), "cn", sanitize, false, mode, nil, nil)
				if string(out) != string(baseline) {
					t.Errorf("sanitize=%v body#%d mode=%q 出站与基线不逐字节一致\n got=%s\nwant=%s",
						sanitize, bi, mode, out, baseline)
				}
			}
		}
	}
}

// TestReasoningHistoryWireBodyViaChatStream 端到端（参考 backfill_test 的
// TestBackfillWireBodyZeroTraceThinkingEnabled）：经真实 ChatStream 出站，
// 假上游收到的 wire body 里 assistant 推理字段已被裁剪成单个空格；full 档则与
// 未开启本功能的客户端逐字节相同。
func TestReasoningHistoryWireBodyViaChatStream(t *testing.T) {
	body := []byte(rhBodyDeepSeekBoth)
	capture := func(t *testing.T, mode string) []byte {
		t.Helper()
		var gotBody []byte
		ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
		})
		defer ts.Close()
		c := New()
		c.ChatBaseCN = ts.URL
		if mode != "" {
			c.SetReasoningHistory(mode)
		}
		acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}
		rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		if status >= 400 {
			t.Fatalf("upstream status %d: %s", status, respBody)
		}
		if len(gotBody) == 0 {
			t.Fatal("假上游没收到 body")
		}
		return gotBody
	}

	// blank 档：wire body 里两条 assistant 的两字段都是单个空格。
	blank := capture(t, ReasoningHistoryBlank)
	if got := assistantRC(t, blank); !equalStrings(got, []string{" ", " "}) {
		t.Errorf("wire assistant.reasoning_content = %q want [\" \" \" \"] (body=%s)", got, blank)
	}
	if got := assistantReasoningField(t, blank); !equalStrings(got, []string{" ", " "}) {
		t.Errorf("wire assistant.reasoning = %q want [\" \" \" \"] (body=%s)", got, blank)
	}

	// last 档：只留最后一条。
	last := capture(t, ReasoningHistoryLast)
	if got := assistantRC(t, last); !equalStrings(got, []string{" ", "rc2"}) {
		t.Errorf("wire assistant.reasoning_content = %q want [\" \" rc2] (body=%s)", got, last)
	}

	// full 档（含未设置档位的客户端）：与显式 full 逐字节相同，且原文完整。
	fullExplicit := capture(t, ReasoningHistoryFull)
	fullDefault := capture(t, "")
	if string(fullExplicit) != string(fullDefault) {
		t.Errorf("full 档与未设置档位的出站应逐字节相同\n got=%s\nwant=%s", fullExplicit, fullDefault)
	}
	if got := assistantRC(t, fullExplicit); !equalStrings(got, []string{"rc1", "rc2"}) {
		t.Errorf("full 档不该改动推理原文: %q (body=%s)", got, fullExplicit)
	}
}

// TestReasoningHistoryClientMode 档位在 Client 上的读写语义：New() 默认 full；
// setter 归一化（大小写/空白），非法值回落 full——Client 内不存未归一化字符串。
func TestReasoningHistoryClientMode(t *testing.T) {
	c := New()
	if got := c.ReasoningHistoryMode(); got != ReasoningHistoryFull {
		t.Errorf("New() 默认档位 = %q want %q", got, ReasoningHistoryFull)
	}
	c.SetReasoningHistory(ReasoningHistoryBlank)
	if got := c.ReasoningHistoryMode(); got != ReasoningHistoryBlank {
		t.Errorf("SetReasoningHistory(blank) 后 = %q want blank", got)
	}
	c.SetReasoningHistory("  LAST ")
	if got := c.ReasoningHistoryMode(); got != ReasoningHistoryLast {
		t.Errorf("大小写/空白应归一化：= %q want last", got)
	}
	for _, bad := range []string{"", "bogus", "off"} {
		c.SetReasoningHistory(bad)
		if got := c.ReasoningHistoryMode(); got != ReasoningHistoryFull {
			t.Errorf("非法档位 %q 应回落 full，实际 %q", bad, got)
		}
	}
}

// TestReasoningHistoryHotSwapRace 面板热改档位 vs 请求路径读的数据竞争回归（-race）。
// 写法同 sanitize_race_test.go（时间窗，确保写读真并发）。
func TestReasoningHistoryHotSwapRace(t *testing.T) {
	c := New()
	body := []byte(rhBodyDeepSeekBoth)
	modes := []string{ReasoningHistoryFull, ReasoningHistoryLast, ReasoningHistoryBlank}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.SetReasoningHistory(modes[i%len(modes)])
			i++
		}
	}()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if out := c.prepareBody(body, "cn", "u1", "c1"); len(out) == 0 {
					t.Error("prepareBody 返回空 body")
					return
				}
			}
		}()
	}
	time.Sleep(320 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// equalStrings 逐元素比较（nil 与空切片视为相等）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// roleReasoningField 提取 messages 里**每条**消息（不筛角色）的 role 与两个推理字段，
// 用于断言非 assistant 消息零改动。缺失字段记 "<absent>"，非 string 记 "<non-string>"。
func roleReasoningField(t *testing.T, out []byte) []string {
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
		got = append(got, role+"|"+rawField(msg, "reasoning_content")+"/"+rawField(msg, "reasoning"))
	}
	return got
}

func rawField(msg map[string]any, key string) string {
	v, ok := msg[key]
	if !ok {
		return "<absent>"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "<non-string>"
}
