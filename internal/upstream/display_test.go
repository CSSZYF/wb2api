package upstream

import (
	"strings"
	"testing"
)

// TestDisplayBody 展示态 body：业务 JSON 逐字透传，HTML 错误页压成单行
// 摘要（title + WAF Request UUID），绝不让整页 HTML 流进客户端
// error.message / 面板日志行。
func TestDisplayBody(t *testing.T) {
	json11133 := `{"code":11133,"msg":"Invalid request parameters","requestId":"r1"}`
	wafPage := `<!DOCTYPE html><html lang="en"><head><title>WAF Block Page</title></head>` +
		`<body><p class="uuid-wrapper"> Request UUID:<span id="uuid">e233bd16f1767dd06037e2851bbaabc5-d0bbfc9154fb0694b1087ed05346cd3f</span></p></body></html>`

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"json passthrough", json11133, json11133},
		{"empty", "", ""},
		{"plain text", "rate limit exceeded", "rate limit exceeded"},
		{"waf span uuid", wafPage,
			"HTML error page: WAF Block Page (WAF request uuid: e233bd16f1767dd06037e2851bbaabc5-d0bbfc9154fb0694b1087ed05346cd3f)"},
		{"waf plain uuid", `<html><head><title>WAF Block Page</title></head><body>Tencent Cloud WAF Access blocked Request UUID: test-waf-id</body></html>`,
			"HTML error page: WAF Block Page (WAF request uuid: test-waf-id)"},
		{"title only", `<html><head><title>Access Denied</title></head><body>blocked</body></html>`,
			"HTML error page: Access Denied"},
		{"no title", `<html><body>Forbidden</body></html>`, "HTML error page"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DisplayBody(c.in); got != c.want {
				t.Errorf("DisplayBody=%q want %q", got, c.want)
			}
		})
	}
	// 关键性质：任何 HTML 入参的输出都不允许再含 '<'——单行、可安全拼接。
	if got := DisplayBody(wafPage); strings.ContainsAny(got, "<>") {
		t.Errorf("HTML page must never leak markup into error.message: %q", got)
	}
}
