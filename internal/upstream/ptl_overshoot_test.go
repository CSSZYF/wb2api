package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ptl_overshoot_test.go 11115「prompt is too long」的两个纯函数单测：
//   - ParsePromptTooLongOvershoot：从上游原文里抽出真实 token 数与上限值；
//   - DowngradeMaxTokens：按超限量与安全余量下调请求体的输出预算（max_tokens /
//     maxOutputTokens），只动这一个字段。
//
// 背景（实测，未 100% 证实）：客户端 max_tokens=128000、输入约 95 万 token 的请求被
// 上游回 11115「prompt is too long: 1083265 tokens > 1048576 maximum」；同一内容在
// 另一条 max_tokens=20000 的请求里被计为 955192。1083265-955192=128073 ≈ max_tokens
// (128000)，强烈提示上游把 max_tokens 也算进上下文上限检查。故按错误里的真实数字
// 下调 max_tokens 重试一次——这是**安全兜底**：算不出/不适用一律保持原行为。

// ptlUserSample 用户实测的 11115 原文（逐字）。
const ptlUserSample = `{"code":11115,"msg":"prompt is too long: 1083265 tokens > 1048576 maximum","requestId":"2b9f0a4e-1c3d-4e5f-8a7b-6c5d4e3f2a1b"}`

// TestParsePromptTooLongOvershoot 数字解析（含大小写/空格容错与拒绝形态）。
func TestParsePromptTooLongOvershoot(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		n     int64
		limit int64
		ok    bool
	}{
		{"用户实测原文", ptlUserSample, 1083265, 1048576, true},
		{"无 JSON 包裹", "prompt is too long: 1083265 tokens > 1048576 maximum", 1083265, 1048576, true},
		{"大写", "PROMPT IS TOO LONG: 1083265 TOKENS > 1048576 MAXIMUM", 1083265, 1048576, true},
		{"多空格", "prompt is too long:  1083265   tokens   >   1048576   maximum", 1083265, 1048576, true},
		{"前导文案", "request rejected: prompt is too long: 2000000 tokens > 1048576 maximum", 2000000, 1048576, true},
		{"等号（非超限）", "prompt is too long: 1048576 tokens > 1048576 maximum", 0, 0, false},
		{"反向（n < limit）", "prompt is too long: 100 tokens > 200 maximum", 0, 0, false},
		{"无数字", `{"code":11115,"msg":"prompt is too long","requestId":"r"}`, 0, 0, false},
		{"只有 n", "prompt is too long: 1083265 tokens", 0, 0, false},
		{"千分位（不解析）", "prompt is too long: 1,083,265 tokens > 1,048,576 maximum", 0, 0, false},
		{"非法数字（超 int64）", "prompt is too long: 99999999999999999999 tokens > 1048576 maximum", 0, 0, false},
		{"零", "prompt is too long: 0 tokens > 0 maximum", 0, 0, false},
		{"空串", "", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, limit, ok := ParsePromptTooLongOvershoot(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (n=%d limit=%d)", ok, c.ok, n, limit)
			}
			if !ok {
				return
			}
			if n != c.n || limit != c.limit {
				t.Errorf("n/limit=%d/%d want %d/%d", n, limit, c.n, c.limit)
			}
		})
	}
}

// TestDowngradeMaxTokensSnakeCase 用户实测形态：max_tokens=128000、超限 34689 →
// 新值 = 128000 - 34689 - max(512, 5%×128000=6400) = 86911。
// 只动 max_tokens 一个字段，其余字段（含 messages 内容与顺序）逐字节不变。
func TestDowngradeMaxTokensSnakeCase(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`)
	out, oldMax, newMax, ok := DowngradeMaxTokens(body, 34689)
	if !ok {
		t.Fatal("应给出下调方案")
	}
	if oldMax != 128000 {
		t.Errorf("oldMax=%d want 128000", oldMax)
	}
	// 34689 + 6400 = 41089；128000 - 41089 = 86911。
	const want = 86911
	if newMax != want {
		t.Errorf("newMax=%d want %d", newMax, want)
	}
	var got, orig map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("out not json: %v (%s)", err, out)
	}
	if err := json.Unmarshal(body, &orig); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(want) {
		t.Errorf("max_tokens=%v want %d", got["max_tokens"], want)
	}
	// 除 max_tokens 外**结构上**逐字段相同（键顺序由 marshal 决定，非本函数的契约）。
	orig["max_tokens"] = float64(want)
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("除 max_tokens 外必须完全一致:\n got=%v\nwant=%v", got, orig)
	}
	// 单键 JSON 的字节形态也锁一下（避免数字被写成 8.6911e+04 这类形态）。
	if s := string(out); !strings.Contains(s, `"max_tokens":86911`) {
		t.Errorf("max_tokens 必须写成整数字面量: %s", s)
	}
}

// TestDowngradeMaxTokensMarginRule 安全余量 = max(512, 5%)，下限 1024：
// 小数位场景取 5%、小值场景取 512、低于下限不重试。
func TestDowngradeMaxTokensMarginRule(t *testing.T) {
	// 小数位：5% 不足 512 → 用 512。max_tokens=5000、overshoot=1000 →
	// 5000-1000-512 = 3488。
	out, _, newMax, ok := DowngradeMaxTokens([]byte(`{"max_tokens":5000}`), 1000)
	if !ok || newMax != 3488 {
		t.Fatalf("newMax=%d ok=%v want 3488/true (out=%s)", newMax, ok, out)
	}
	// 大数位：5% 超过 512 → 用 5%。max_tokens=100000、overshoot=1000 →
	// 100000-1000-5000 = 94000。
	if _, _, got, ok := DowngradeMaxTokens([]byte(`{"max_tokens":100000}`), 1000); !ok || got != 94000 {
		t.Fatalf("newMax=%d ok=%v want 94000/true", got, ok)
	}
	// 低于下限（1024）：max_tokens=2000、overshoot=1500 → 2000-1500-512 = -12 → 不重试。
	if _, _, _, ok := DowngradeMaxTokens([]byte(`{"max_tokens":2000}`), 1500); ok {
		t.Error("新值低于下限必须不重试")
	}
	// 恰好等于下限：1024 放行（下限是闭区间）。
	if _, _, got, ok := DowngradeMaxTokens([]byte(`{"max_tokens":4000}`), 2464); !ok || got != 1024 {
		t.Fatalf("newMax=%d ok=%v want 1024/true", got, ok)
	}
	// overshoot<=0 → 不重试（上游没超限，别乱改请求）。
	if _, _, _, ok := DowngradeMaxTokens([]byte(`{"max_tokens":128000}`), 0); ok {
		t.Error("overshoot<=0 不重试")
	}
}

// TestDowngradeMaxTokensFieldForms 字段形态：两个键都支持（都改），都没有则不重试；
// 非正数/非数值（含 max_completion_tokens 别名，翻译发生在本函数之前）不重试；
// 畸形 JSON 不重试（绝不臆造请求体）。
func TestDowngradeMaxTokensFieldForms(t *testing.T) {
	// 只带 camelCase maxOutputTokens。
	out, _, newMax, ok := DowngradeMaxTokens([]byte(`{"maxOutputTokens":128000,"messages":[]}`), 10000)
	if !ok || newMax != 111600 { // 128000-10000-6400
		t.Fatalf("camelCase: newMax=%d ok=%v want 111600/true", newMax, ok)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["maxOutputTokens"] != float64(111600) {
		t.Errorf("maxOutputTokens=%v want 111600", got["maxOutputTokens"])
	}
	if _, has := got["max_tokens"]; has {
		t.Error("不得凭空新增 max_tokens 键")
	}
	// 两个键同时存在：都改成同一下调值（取更小者为基准）。
	out, _, newMax, ok = DowngradeMaxTokens([]byte(`{"max_tokens":128000,"maxOutputTokens":64000}`), 10000)
	if !ok {
		t.Fatal("两个键都在时应给出方案")
	}
	// 基准取更小者 64000：64000-10000-3200 = 50800。
	if newMax != 50800 {
		t.Errorf("newMax=%d want 50800", newMax)
	}
	var both map[string]any
	if err := json.Unmarshal(out, &both); err != nil {
		t.Fatal(err)
	}
	if both["max_tokens"] != float64(50800) || both["maxOutputTokens"] != float64(50800) {
		t.Errorf("两键应同时下调: %v", both)
	}
	// 没有输出预算字段 → 不重试。
	for _, body := range []string{
		`{"messages":[]}`,
		`{"max_completion_tokens":128000,"messages":[]}`, // 别名（出站管线已翻译成 max_tokens，此处不该兜底）
		`{"max_tokens":0,"messages":[]}`,
		`{"max_tokens":-5,"messages":[]}`,
		`{"max_tokens":"128000","messages":[]}`,
		`{"max_tokens":128000.5,"messages":[]}`,
		`not json`,
		``,
	} {
		if _, _, _, ok := DowngradeMaxTokens([]byte(body), 10000); ok {
			t.Errorf("body=%s 不该给出下调方案", body)
		}
	}
	// 失败时原样返回入参（调用方据此继续用原 body，不做任何改写）。
	in := []byte(`not json`)
	out, _, _, _ = DowngradeMaxTokens(in, 10000)
	if string(out) != string(in) {
		t.Errorf("失败时必须原样返回入参: %s", out)
	}
}
