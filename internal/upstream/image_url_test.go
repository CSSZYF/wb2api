package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestHasBusinessCode 结构化业务码判定（吸收上游 d47219b）：上游信封在顶层
// / error / data / extError 之间来回变，且数字与字符串、紧凑与带空白形态并存——
// 遍历解码结构认「名为 code 的字段」，不靠文案猜测。只认**精确**值：
// 子串命中（如 requestId 里出现 14018）不算。
func TestHasBusinessCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"顶层数字", `{"code":14018,"msg":"x"}`, true},
		{"顶层数字带空白（JSON 空白容差）", `{"code": 14018 , "msg":"x"}`, true},
		{"顶层字符串", `{"code":"14018"}`, true},
		{"嵌套 error.data", `{"error":{"data":{"code":14018,"msg":"x"}}}`, true},
		{"嵌套 extError", `{"extError":{"code":14018}}`, true},
		{"数组内嵌套", `{"data":{"items":[{"code":"14018"}]}}`, true},
		{"其他码不命中", `{"code":14017,"msg":"x"}`, false},
		{"无 code 字段", `{"msg":"credits exhausted"}`, false},
		{"非 JSON", `not json`, false},
		{"空串", ``, false},
		// 子串不算：requestId 里含 14018 是噪声，不得据此判积分耗尽（否则会把
		// 限流号硬冷却到次日 04:00）。
		{"requestId 里含 14018 不算", `{"requestId":"req-14018","msg":"rate limit"}`, false},
		{"msg 里含 14018 不算", `{"msg":"error 14018 happened"}`, false},
		{"null code 不算", `{"code":null}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasBusinessCode(c.body, "14018"); got != c.want {
				t.Errorf("hasBusinessCode(%q)=%v want %v", c.body, got, c.want)
			}
		})
	}
}

// TestClassify14018HardCredit 429 + code 14018 → ErrHardCredit（吸收上游 d47219b，
// issue #175）：上游把积分耗尽也用 429 + 业务码表达。若不先判，会落通用 429 兜底
// 成软限流——全池冷却时的兜底选号会反复选中它白打请求（软冷却自愈不了真耗尽）。
//
// 只认结构化 code：无该码的 "credits exhausted" 文案仍是普通 429 软限流语义
// （fork-scan-absorb T-3 的既有裁定，不许被本条推翻）。
func TestClassify14018HardCredit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"429 顶层 code 14018", 429, `{"code":14018,"msg":"Credits exhausted"}`, ErrHardCredit},
		{"429 带空白 code", 429, `{"code": 14018, "msg":"credits exhausted"}`, ErrHardCredit},
		{"429 嵌套信封 code", 429, `{"error":{"data":{"code":14018,"msg":"credits exhausted"}}}`, ErrHardCredit},
		{"429 字符串 code", 429, `{"code":"14018"}`, ErrHardCredit},
		// 无 14018 的 429 仍是软限流（措辞跨计费/限流两界，状态码更权威）。
		{"429 无码保持软限流", 429, `{"code":1,"msg":"quota exceeded"}`, ErrSoftRate},
		{"429 credits exhausted 文案仍软限流", 429, `{"code":1,"msg":"credits exhausted"}`, ErrSoftRate},
		// 非 429 的 14018：hardMarkers 层已能凭文案兜住，这里确认不回归。
		{"403 code 14018 走 hardMarkers", 403, `{"code":14018,"msg":"额度不足"}`, ErrHardCredit},
		// 账号级故障码优先于 14018（14017 试用未激活：不是余额问题）。
		{"429 14017 仍账号级故障", 429, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestNormalizeImageURL OpenAI chat 多模态内容的 image_url 两种写法兼容
// （吸收上游 d47219b）：OpenAI 规范是对象 {"url":...}，但部分客户端（以及
// Responses → Chat 转换器）发字符串。上游只认对象形态，字符串直接 400 code=11101。
// 出站改写管线把字符串归一为对象，其余形态一律原样（不补默认值，让上游报真实错误）。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// TestNormalizeImageURLNonMultimodalUntouched 非多模态 body（字符串 content /
// 无 messages / content 非数组 / 非对象 part）零改动：归一不制造结构，也不 panic。
func TestNormalizeImageURLNonMultimodalUntouched(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"plain"}]}`,
		`{"model":"m"}`,
		`{"model":"m","messages":[]}`,
		`{"model":"m","messages":["weird"]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"t"},{"type":"image_url","image_url":"https://a/b.png"},{"type":"image_url","image_url":{"url":"https://a/c.png"}}]}]}`,
	}
	for _, body := range bodies {
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		// 出站 body 必须仍是合法 JSON，且强制 stream:true（管线既有语义不变）。
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v (%s)", err, out)
		}
		if obj["stream"] != true {
			t.Errorf("stream 强制语义被破坏: %s", out)
		}
	}
}

// TestIsInvalidImageDataFormatMarker 11135 家族补「invalid image_url content」文案
// （吸收上游 d47219b）：字符串形态的 image_url 归一后，剩下的 image_url 报错就是
// 真格式/数据问题，属请求级终态。同时确认 code 判定容忍 JSON 空白（`"code": 11135`）。
func TestIsInvalidImageDataFormatMarker(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":11135,"msg":"x"}`, true},
		{`{"code": 11135, "msg":"x"}`, true},
		{`{"code":"11135"}`, true},
		{`Parse message failed: invalid image_url content`, true},
		{`{"msg":"invalid image_url content"}`, true},
		{`{"extError":{"code":"invalid_image_data"}}`, true},
		{`Please start a new conversation, replace the image, and try again.`, true},
		// 噪声不命中。
		{`{"requestId":"11135","msg":"ok"}`, false},
		{`{"code":11133,"msg":"Invalid request parameters"}`, false},
		{`bad request`, false},
	}
	for _, c := range cases {
		if got := isInvalidImageData(c.body); got != c.want {
			t.Errorf("isInvalidImageData(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestClassifyImageURLContentError ErrInvalidImage 覆盖格式类文案：400 +
// "invalid image_url content" 是请求级终态（同 body 换号结果不变），
// 必须判在通用 ErrClient 兜底之前——否则每换一个号就喂一次连败，一张坏图把全池降权。
func TestClassifyImageURLContentError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{400, `Parse message failed: invalid image_url content`, ErrInvalidImage},
		{400, `{"code":11135,"msg":"invalid image_url content"}`, ErrInvalidImage},
		{400, `{"code": 11135, "msg":"invalid image_url content"}`, ErrInvalidImage},
		// 纯 code 形态（无文案）：codeMarker 的空白容差（`"code": 11135`）——字面量
		// marker 只能覆盖紧凑写法，本形态是 d47219b 明确要补的容差。
		{400, `{"code": 11135}`, ErrInvalidImage},
		{400, `{"code":"11135"}`, ErrInvalidImage},
		{400, `{"code":11135}`, ErrInvalidImage},
		// 429/5xx 语义更权威（限流/服务端故障），不被图片文案劫持。
		{429, `invalid image_url content`, ErrSoftRate},
		{500, `invalid image_url content`, ErrServer},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}
