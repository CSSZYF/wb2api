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
