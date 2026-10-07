package upstream

import (
	"strings"
	"unicode"
)

// DisplayBody 把上游错误 body 压成单行、可直接拼进客户端 error.message 的文本。
//
// 透传纪律不变：业务 JSON body 逐字原样返回——上游原文（code/extError/
// displayMsg/requestId）是最有价值的排障信息，禁止固定词覆盖。
// 唯一的例外是 HTML 错误页（WAF 拦截页/网关错误页）：整页数 KB 标记原样拼进
// error.message 会灌满客户端日志且完全不可读（实测腾讯云 WAF Block Page
// 随 503 原文下发）。此时提取 <title> 与 WAF Request UUID（用户可拿 UUID
// 向 WAF 提交误杀反馈），返回单行摘要代替整页。
func DisplayBody(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return raw
	}
	lower := strings.ToLower(s)
	html := strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") ||
		strings.Contains(lower, "<title>")
	if !html {
		return raw // 非 HTML：原文全量透传
	}
	title := ""
	if i := strings.Index(lower, "<title>"); i >= 0 {
		if j := strings.Index(lower[i+len("<title>"):], "</title>"); j >= 0 {
			title = strings.TrimSpace(s[i+len("<title>") : i+len("<title>")+j])
		}
	}
	// WAF 拦截页把请求 UUID 放在 <span id="uuid">…</span>（提交误杀反馈的凭据）。
	uuid := ""
	if i := strings.Index(lower, `id="uuid"`); i >= 0 {
		if j := strings.Index(s[i:], ">"); j >= 0 {
			rest := s[i+j+1:]
			if k := strings.Index(rest, "<"); k >= 0 {
				uuid = strings.TrimSpace(rest[:k])
			}
		}
	}
	if uuid == "" {
		// 兜底：简化拦截页把 UUID 写成 "Request UUID: xxx" 纯文本（无 span）。
		if i := strings.Index(lower, "request uuid:"); i >= 0 {
			rest := strings.TrimSpace(s[i+len("request uuid:"):])
			end := len(rest)
			for j, r := range rest {
				if r == '<' || unicode.IsSpace(r) {
					end = j
					break
				}
			}
			uuid = rest[:end]
		}
	}
	switch {
	case title != "" && uuid != "":
		return "HTML error page: " + title + " (WAF request uuid: " + uuid + ")"
	case title != "":
		return "HTML error page: " + title
	case uuid != "":
		return "HTML error page (WAF request uuid: " + uuid + ")"
	default:
		return "HTML error page"
	}
}
