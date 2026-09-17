package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestAppJSMaxRotateWiring server.max_rotate 的面板接线必须齐全：CFG_MAP 映射
// （否则表单值与后端对不上，输入框读不到也存不进去）+ 数字输入项存在。
// app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容——少一处映射，
// 用户改了「单请求最多换号次数」保存后后端收不到该键，静默不生效（与
// TestAppJSTestChatWiring 同因，把接线完整性前移）。
func TestAppJSMaxRotateWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `max_rotate: ['server', 'max_rotate']`) {
		t.Error("app.js 缺 max_rotate 的 CFG_MAP 映射（表单值无法读写 server.max_rotate）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name="max_rotate"`, `name="max_body_mb"`} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 缺配置表单项 %s", want)
		}
	}
}

// TestAppJSTestChatWiring 单账号对话测试的前端接线必须齐全：
// 行内按钮（data-a="testchat"）→ 端点路径 → 弹窗 DOM。三者任一被误删，
// 面板上就是一个点了没反应的按钮，而所有 Go 测试仍会全绿（与 TestAppJSSyntax 同因）。
func TestAppJSTestChatWiring(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, must := range []string{
		`data-a="testchat"`,               // 账号行按钮
		`'account/test_chat'`,             // 后端端点（api() 会补 /panel/api/ 前缀）
		`openTestChat(`,                   // 行点击分发
		`'tcModel'`, `'tcMsg'`, `'tcOut'`, // 弹窗三要素：模型下拉/输入/结果区
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺少对话测试接线：%s", must)
		}
	}
	// 端点注册必须与前端调用同路径（改了一边忘另一边 = 404）。
	pn := newTestPanel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/account/test_chat", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	pn.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Error("panel.go 未注册 POST /panel/api/account/test_chat")
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestFrontendVoucherModalWiring 券码弹窗的四处接线必须同时存在：
//
//	index.html：vcVeil 弹窗骨架（vcBody/vcNote/btnVcClose/btnVcRefresh）+ .vc-* 样式
//	            + 开学季区块的「查询券码」按钮
//	app.js：copyText（clipboard→execCommand 降级）、vcCard 渲染、loadSchoolVouchers、
//	        四个 DOM 绑定
//
// 为什么需要：app.js/index.html 是 go:embed 的静态资源，Go 编译器与 Go 测试都不
// 校验其内容——少一个 id 或一个绑定，点击按钮就是无反应的静默失败（页面不白屏，
// 测试也全绿）。此用例把"接线完整性"前移。
func TestFrontendVoucherModalWiring(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	for _, want := range []string{
		`id="vcVeil"`, `id="vcBody"`, `id="vcNote"`,
		`id="btnVcClose"`, `id="btnVcRefresh"`, `id="btnSchoolVouchers"`,
		`.vc-acct {`, `.vc .ft code {`, `.vc.expired {`, // 票券式卡片样式
		`class="hint">官方返回的最大输出仅为参考`, // 模型能力标题 hint
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %s（券码弹窗/标题 hint 接线不完整）", want)
		}
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()
	for _, want := range []string{
		"function copyText(", "execCommand('copy')", // 远程 http 面板降级路径
		"function vcCard(", "async function loadSchoolVouchers(",
		"$('btnSchoolVouchers').onclick", "$('btnVcClose').onclick", "$('btnVcRefresh').onclick",
		"qrMatrix(", "qrSVG(", "button[data-qr]", // 二维码（内嵌编码器，无外链依赖）
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %s（券码查询接线不完整）", want)
		}
	}
}

// TestFrontendVoucherModalNoExternalRefs 券码弹窗不得引入外部资源：
// CSP 只允许 self（script-src 'self' / connect-src 'self' / img-src 'self' data:），
// 任何外链 QR 服务或 CDN 都会被浏览器拦掉（且面板是离线单文件部署）。
func TestFrontendVoucherModalNoExternalRefs(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()

	for _, bad := range []string{"api.qrserver", "chart.googleapis", "cdn.jsdelivr", "unpkg.com", "https://cdn"} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js 引用了外部资源 %q（CSP script-src 'self' 会拦截）", bad)
		}
	}
}
