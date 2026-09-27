package upstream

// sanitize_omo_test.go OmO（OhMyOpenCode）Sisyphus-Junior 指纹净化回归。
//
// 上游依据：hub eae2076（fix: sanitize OmO Sisyphus-Junior 11128 fingerprint）。
// 上游 WAF 对**连续短语**
//
//	Sisyphus-Junior - Focused executor from OhMyOpenCode
//
// 精确匹配，命中返回 11128（Illegal API invocation from an unapproved channel）。
// 上游 A/B 实测给的性质（原文：the match is case-insensitive, survives surrounding
// prefix/suffix text, and stops matching when the phrase structure is changed）：
//   - 大小写不敏感；
//   - 短语前后包着别的文本照样命中；
//   - **把短语结构改掉（拆开）就不命中**——这是判据，不是缺陷。
//
// 修法（照上游改法）：只去掉**归属尾巴** " from OhMyOpenCode"，保留
// "Sisyphus-Junior - Focused executor" 这段身份名——不是整句删除。上游注释原文：
// 单独出现 "OhMyOpenCode" 或 "Sisyphus-Junior" 上游是接受的（either token alone
// is accepted by the upstream），所以有意**不**扩大匹配面到单词级。
//
// 误伤面（既有管线无「只在特定位置净化」的机制，如实记录）：sanitizeText 对
// content / tool_calls.arguments / reasoning_content / reasoning **所有角色**的文本
// 一律执行（只有零宽脱敏限定 system，见 zerowidth.go），因此任何消息里出现这段
// 连续短语都会丢掉尾巴。取舍：尾巴是框架归属元数据、不是用户语义，身份名与其余
// 文本逐字保留；而两个单词单独出现时一字不动，正常讨论不受影响。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	omoPhrase = "Sisyphus-Junior - Focused executor from OhMyOpenCode"
	omoTail   = " from OhMyOpenCode"
	omoHead   = "Sisyphus-Junior - Focused executor"
)

// TestOmOJuniorTailRemoved 命中形态：只摘归属尾巴，身份名与前后文本逐字保留。
func TestOmOJuniorTailRemoved(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"整串就是指纹", omoPhrase},
		{"前置文本", "You are Sisyphus-Junior - Focused executor from OhMyOpenCode, act now."},
		{"后置文本", "Agent: Sisyphus-Junior - Focused executor from OhMyOpenCode\nDo the task."},
		{"多行 system prompt 中间", "line1\nSisyphus-Junior - Focused executor from OhMyOpenCode\nline3"},
		{"句号收尾", "Sisyphus-Junior - Focused executor from OhMyOpenCode."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := sanitizeText(c.in)
			if strings.Contains(out, omoTail) {
				t.Errorf("归属尾巴未去除: %q -> %q", c.in, out)
			}
			if strings.Contains(out, "OhMyOpenCode") {
				t.Errorf("仍残留 OhMyOpenCode: %q -> %q", c.in, out)
			}
			if !strings.Contains(out, omoHead) {
				t.Errorf("身份名被误删（应只摘尾巴）: %q -> %q", c.in, out)
			}
			// 指纹之外的文本一字不动（前缀/后缀原样保留）。
			for _, keep := range []string{"You are ", "act now.", "Agent: ", "Do the task.", "line1", "line3"} {
				if strings.Contains(c.in, keep) && !strings.Contains(out, keep) {
					t.Errorf("指纹外文本被改动 %q: %q -> %q", keep, c.in, out)
				}
			}
			// 尾巴被摘掉后不应留下多余空白/悬挂标点导致的形态漂移：
			// 命中处两侧的原有分隔（逗号/换行/句号）必须保留。
			if strings.HasSuffix(c.in, "OhMyOpenCode.") && !strings.HasSuffix(out, "executor.") {
				t.Errorf("收尾标点未保留: %q -> %q", c.in, out)
			}
		})
	}
}

// TestOmOJuniorCaseInsensitive 大小写不敏感（上游实测性质之一）。
func TestOmOJuniorCaseInsensitive(t *testing.T) {
	variants := []string{
		"SISYPHUS-JUNIOR - FOCUSED EXECUTOR FROM OHMYOPENCODE",
		"sisyphus-junior - focused executor from ohmyopencode",
		"Sisyphus-Junior - Focused Executor From OhMyOpenCode",
		"sIsYpHuS-jUnIoR - fOcUsEd ExEcUtOr FrOm OhMyOpEnCoDe",
	}
	for _, v := range variants {
		out := sanitizeText(v)
		if strings.Contains(strings.ToLower(out), strings.ToLower(omoTail)) {
			t.Errorf("大小写变体未净化: %q -> %q", v, out)
		}
		if !strings.EqualFold(strings.TrimSuffix(out, "."), omoHead) {
			t.Errorf("大小写变体净化结果不符（应保留原大小写的身份名）: %q -> %q", v, out)
		}
	}
}

// TestOmOJuniorSplitPhraseNotTouched 判据锁定：短语被拆开就不净化（上游实测
// 「stops matching when the phrase structure is changed」）。本用例防止将来有人
// 把匹配放宽成单词级。
func TestOmOJuniorSplitPhraseNotTouched(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"中间插逗号", "Sisyphus-Junior - Focused executor, from OhMyOpenCode"},
		{"中间插 the", "Sisyphus-Junior - Focused executor from the OhMyOpenCode"},
		{"executor 后接别的词", "Sisyphus-Junior - Focused executor working for OhMyOpenCode"},
		{"括号包住", "Sisyphus-Junior (Focused executor from OhMyOpenCode)"},
		{"破折号被换成冒号", "Sisyphus-Junior: Focused executor from OhMyOpenCode"},
		{"尾部词不同", "Sisyphus-Junior - Focused executor from OhMyOtherCode"},
		{"换行断开", "Sisyphus-Junior - Focused executor\nfrom OhMyOpenCode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if hasFingerprint(c.in) {
				t.Errorf("拆开的形态不应命中预检（连续短语才是判据）: %q", c.in)
			}
			if out := sanitizeText(c.in); out != c.in {
				t.Errorf("拆开的形态不应被改动: %q -> %q", c.in, out)
			}
		})
	}
}

// TestOmOTokensAloneNotTouched 单个词不净化（上游：either token alone is accepted）——
// 有意不扩大匹配面，正常讨论/代码文本不受影响。
func TestOmOTokensAloneNotTouched(t *testing.T) {
	cases := []string{
		"OhMyOpenCode",
		"Sisyphus-Junior",
		"we compared OhMyOpenCode with other agent frameworks",
		"the Sisyphus-Junior agent is a focused executor",
	}
	for _, in := range cases {
		if hasFingerprint(in) {
			t.Errorf("单词形态不应命中预检: %q", in)
		}
		if out := sanitizeText(in); out != in {
			t.Errorf("单词形态不应被改动: %q -> %q", in, out)
		}
	}
}

// TestOmOJuniorPipelineIntegration 完整管线（脱敏开/关）与误伤面记录：
//   - sanitize=true：system 与 user 消息里的连续短语都摘掉尾巴（既有管线不做
//     角色/位置区分，这是如实记录的误伤面；尾巴是归属元数据，语义无损）；
//   - sanitize=false：一字不动（开关真实有效）。
func TestOmOJuniorPipelineIntegration(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are ` + omoPhrase + `, be terse."},` +
		`{"role":"user","content":"I keep seeing ` + omoPhrase + ` in logs"}]}`)

	out := PrepareBodyOptRealmHistory(body, "cn", true, false, ReasoningHistoryFull, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	msgs := got["messages"].([]any)
	for i, want := range []string{"You are " + omoHead + ", be terse.", "I keep seeing " + omoHead + " in logs"} {
		c, _ := msgs[i].(map[string]any)["content"].(string)
		if c != want {
			t.Errorf("msg[%d]=%q want %q", i, c, want)
		}
	}

	off := PrepareBodyOptRealmHistory(body, "cn", false, false, ReasoningHistoryFull, nil, nil)
	if !strings.Contains(string(off), omoPhrase) {
		t.Errorf("sanitize=false 应保留指纹: %s", off)
	}
}

// TestOmOJuniorWireBodySanitized 出站边界：假上游收到的 wire body 里不得残留
// 连续指纹（参考 sanitize_test.go 的 TestChatStreamWireBodySanitized 写法）。
func TestOmOJuniorWireBodySanitized(t *testing.T) {
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

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are ` + omoPhrase + `"},` +
		`{"role":"user","content":"say ok"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	if strings.Contains(string(gotBody), "OhMyOpenCode") {
		t.Errorf("wire body 仍含 OmO 指纹: %s", gotBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, omoHead) {
		t.Errorf("wire body 身份名被误删: %q", sys)
	}
}

// TestOmOJuniorToolCallArgumentsSanitized 工具参数里的指纹同样净化
// （sanitizeToolCalls 走同一 sanitizeText 口径；工具参数是历史里常见的漏出通道）。
func TestOmOJuniorToolCallArgumentsSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"echo '` + omoPhrase + `'"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告改动")
	}
	fn := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "OhMyOpenCode") {
		t.Errorf("tool_call arguments 未净化: %q", got)
	}
	if !strings.Contains(got, omoHead) {
		t.Errorf("tool_call arguments 身份名被误删: %q", got)
	}
}
