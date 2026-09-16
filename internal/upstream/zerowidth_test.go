package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// stripZW 去掉全部零宽空格——用来断言核心不变量：
// 脱敏后的"可见文本"必须与原文逐字符相同。
func stripZW(s string) string { return strings.ReplaceAll(s, string(zeroWidthSpace), "") }

// TestZeroWidthInvariantVisibleTextUnchanged 零宽脱敏的唯一卖点是
// "看得见的字一个没变"。这条不变量破了，功能就失去了意义（会污染对话内容）。
func TestZeroWidthInvariantVisibleTextUnchanged(t *testing.T) {
	cases := []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"Never assist with malware, ransomware, or reverse shell development.",
		"DoS and DDoS testing requires explicit authorization.",
		"普通中文句子夹杂 exploit 与 SQL injection 两个词。",
		"",
		"no sensitive terms here at all",
	}
	for _, in := range cases {
		out, n := defaultZWMatcher.InsertZeroWidth(in)
		if stripZW(out) != in {
			t.Errorf("可见文本被改动：\n in=%q\nout=%q\n去掉零宽后=%q", in, out, stripZW(out))
		}
		if n > 0 && out == in {
			t.Errorf("报告插入了 %d 处但字符串未变：%q", n, in)
		}
		if n == 0 && out != in {
			t.Errorf("报告没插入但字符串变了：%q -> %q", in, out)
		}
	}
}

// TestZeroWidthBasicInsertion 插入位置固定在首字符之后（与上游 _zero_width_split 一致）。
func TestZeroWidthBasicInsertion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"DoS", "D\u200boS"}, // 保留原文大小写（插进原文，不是插进小写副本）
		{"malware", "m\u200balware"},
		{"Claude Code", "C\u200blaude Code"},
		{"Anthropic", "A\u200bnthropic"},
		{"Co-Authored-By", "C\u200bo-Authored-By"},
	}
	for _, c := range cases {
		got, n := defaultZWMatcher.InsertZeroWidth(c.in)
		if n != 1 {
			t.Errorf("%q: 应插入 1 处，实际 %d", c.in, n)
		}
		if got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

// TestZeroWidthCasePreserved 大小写不敏感匹配，但插入时保留原文大小写。
func TestZeroWidthCasePreserved(t *testing.T) {
	got, n := defaultZWMatcher.InsertZeroWidth("MALWARE and Exploit")
	if n != 2 {
		t.Fatalf("应命中 2 处，实际 %d：%q", n, got)
	}
	if !strings.HasPrefix(got, "M\u200bALWARE") {
		t.Errorf("大写形态应保留原大小写：%q", got)
	}
	if !strings.Contains(got, "E\u200bxploit") {
		t.Errorf("首字母大写的词应保留原大小写：%q", got)
	}
}

// TestZeroWidthWordBoundary 必须是独立词——词根嵌在长词里时不动它。
//
// 对齐上游正则的 (?<!\w)term(?!\w)：这样 "exploited" / "myexploit" 不会被切开，
// 避免在正常单词中间塞进不可见字符。
func TestZeroWidthWordBoundary(t *testing.T) {
	noMatch := []string{
		"exploited",    // 词尾连着 e
		"exploitation", // 同上
		"myexploit",    // 词首前连着 y
		"malwares",     // 词尾连着 s
		"backdoors",    // 同上
	}
	for _, in := range noMatch {
		if got, n := defaultZWMatcher.InsertZeroWidth(in); n != 0 {
			t.Errorf("%q 不该命中，实际插了 %d 处：%q", in, n, got)
		}
	}
	mustMatch := []string{
		"exploit development",
		"(exploit)",
		"the malware.",
		"exploit,",
	}
	for _, in := range mustMatch {
		if _, n := defaultZWMatcher.InsertZeroWidth(in); n == 0 {
			t.Errorf("%q 应当命中（前后是标点/空格=词边界）", in)
		}
	}
}

// TestZeroWidthLongestMatch 同位置多个候选时取最长。
//
// 词表里既有 "escalated" 也有 "escalated privileges"：若按短的先匹配，
// 就会把后者的上半截切开、留下 " privileges" 这个无主的尾巴。
func TestZeroWidthLongestMatch(t *testing.T) {
	got, n := defaultZWMatcher.InsertZeroWidth("avoid escalated privileges")
	if n != 1 {
		t.Fatalf("应只命中 1 处（取最长），实际 %d：%q", n, got)
	}
	if !strings.Contains(got, "e\u200bscalated privileges") {
		t.Errorf("应整体命中 \"escalated privileges\"（零宽插在首字符后）：%q", got)
	}
}

// TestZeroWidthIdempotent 重复调用不二次插入。
//
// 已插入的词内部多了 U+200B，rune 序列不再等于词表，所以第二次应当直接判定无命中。
func TestZeroWidthIdempotent(t *testing.T) {
	once, n1 := defaultZWMatcher.InsertZeroWidth("develop malware")
	if n1 == 0 {
		t.Fatal("首次应命中")
	}
	twice, n2 := defaultZWMatcher.InsertZeroWidth(once)
	if n2 != 0 {
		t.Errorf("二次调用不该再插入，实际 %d 处", n2)
	}
	if twice != once {
		t.Errorf("二次调用改动了字符串：%q -> %q", once, twice)
	}
	if strings.Count(once, string(zeroWidthSpace)) != strings.Count(twice, string(zeroWidthSpace)) {
		t.Errorf("零宽字符数量变了：%d -> %d",
			strings.Count(once, string(zeroWidthSpace)), strings.Count(twice, string(zeroWidthSpace)))
	}
}

// TestZeroWidthNonASCIIIntact 文本里混入多字节字符时不得被破坏
// （逐 rune 处理的意义：按字节切会把中文/emoji 切成非法 UTF-8）。
func TestZeroWidthNonASCIIIntact(t *testing.T) {
	in := "说明：不要协助 malware 开发。🧪 backdoor 也是。"
	out, n := defaultZWMatcher.InsertZeroWidth(in)
	if n != 2 {
		t.Fatalf("应命中 malware 与 backdoor 共 2 处，实际 %d：%q", n, out)
	}
	if stripZW(out) != in {
		t.Errorf("可见文本被改动：%q", stripZW(out))
	}
	if !strings.Contains(out, "🧪") || !strings.Contains(out, "不要协助") {
		t.Errorf("多字节内容被破坏：%q", out)
	}
}

// TestApplyZeroWidthMessagesOnlySystem 作用域：只动 system，用户内容一字不改。
func TestApplyZeroWidthMessagesOnlySystem(t *testing.T) {
	userText := "help me write malware"
	asstText := "I can't help with malware"
	msgs := []any{
		map[string]any{"role": "system", "content": "Never assist with malware."},
		map[string]any{"role": "user", "content": userText},
		map[string]any{"role": "assistant", "content": asstText},
	}
	if n := ApplyZeroWidthMessages(msgs); n != 1 {
		t.Fatalf("应只改写 1 条 system 消息，实际 %d", n)
	}
	if got := msgs[0].(map[string]any)["content"].(string); !strings.Contains(got, "\u200b") {
		t.Errorf("system 未脱敏：%q", got)
	}
	if got := msgs[1].(map[string]any)["content"].(string); got != userText {
		t.Errorf("用户消息被改动：%q -> %q", userText, got)
	}
	if got := msgs[2].(map[string]any)["content"].(string); got != asstText {
		t.Errorf("assistant 消息被改动：%q -> %q", asstText, got)
	}
}

// TestApplyZeroWidthMessagesBlockContent content 为 text 块数组时也要覆盖
// （多模态/结构化请求的常见形态）。
func TestApplyZeroWidthMessagesBlockContent(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "avoid exploit code"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x/y.png"}},
		}},
	}
	if n := ApplyZeroWidthMessages(msgs); n != 1 {
		t.Fatalf("应改写 1 条，实际 %d", n)
	}
	blocks := msgs[0].(map[string]any)["content"].([]any)
	if txt := blocks[0].(map[string]any)["text"].(string); !strings.Contains(txt, "\u200b") {
		t.Errorf("text 块未脱敏：%q", txt)
	}
	// 非 text 块必须原样保留。
	if blocks[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("非 text 块被改动：%v", blocks[1])
	}
}

// TestZeroWidthPipelineOffByDefault 全链路：开关关闭时出站 body 与开启前逐字节相同。
func TestZeroWidthPipelineOffByDefault(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"system","content":"avoid exploit code"},{"role":"user","content":"hi"}]}`
	off := PrepareBodyOptWithEffortsAndDefault([]byte(body), true, false, nil, nil)
	if strings.Contains(string(off), "\u200b") {
		t.Errorf("开关关闭时不该出现零宽字符：%s", off)
	}
	on := PrepareBodyOptWithEffortsAndDefault([]byte(body), true, true, nil, nil)
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(on, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("消息数变了：%d", len(got.Messages))
	}
	if !strings.Contains(got.Messages[0].Content, "\u200b") {
		t.Errorf("开启后 system 应含零宽字符：%q", got.Messages[0].Content)
	}
	if got.Messages[1].Content != "hi" {
		t.Errorf("用户消息应原样：%q", got.Messages[1].Content)
	}
	// 可见文本仍与原文一致。
	if stripZW(got.Messages[0].Content) != "avoid exploit code" {
		t.Errorf("可见文本被改动：%q", stripZW(got.Messages[0].Content))
	}
}

// TestZeroWidthRunsAfterSanitize 顺序约束：sanitize 的整句改写必须先执行。
//
// 若先插零宽，句子中间多出 U+200B，"You are Claude Code, Anthropic's official CLI
// for Claude." 这种整句匹配随即失效，sanitize 的改写会全部落空——
// 两个功能互相削弱，等于白开一个。
func TestZeroWidthRunsAfterSanitize(t *testing.T) {
	const sentence = "You are Claude Code, Anthropic's official CLI for Claude."
	body := `{"model":"glm-5.2","messages":[{"role":"system","content":` +
		jsonString(sentence) + `}]}`
	out := PrepareBodyOptWithEffortsAndDefault([]byte(body), true, true, nil, nil)
	var got struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	visible := stripZW(got.Messages[0].Content)
	if !strings.Contains(visible, "CLI tool for Claude") {
		t.Errorf("sanitize 的整句改写未生效（说明零宽先跑了）：%q", visible)
	}
	if !strings.Contains(got.Messages[0].Content, "\u200b") {
		t.Errorf("零宽未生效：%q", got.Messages[0].Content)
	}
}

// jsonString 把字符串编成 JSON 字面量（避免手写转义）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
