package panel

import (
	"os"
	"strings"
	"testing"
)

// ptl_retry_frontend_test.go features.ptl_max_tokens_retry 的面板接线必须齐全
// （与 TestAppJSDegradeWiring / TestAppJSMaxRotateWiring 同因）：CFG_MAP 映射 +
// index.html 表单项。app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容
// ——少一处映射，用户在面板改了「上下文超限自动下调 max_tokens 重试」保存后后端收不到
// 该键，静默不生效。
func TestAppJSPTLRetryWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `ptl_max_tokens_retry: ['features', 'ptl_max_tokens_retry']`) {
		t.Error("app.js 缺 ptl_max_tokens_retry 的 CFG_MAP 映射（表单值无法读写 features.ptl_max_tokens_retry）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `name="ptl_max_tokens_retry"`) {
		t.Error(`index.html 缺配置表单项 name="ptl_max_tokens_retry"`)
	}
	// 开关项必须落在 features 段的既有 switch 区块里（与另外两个 features 布尔项同排布），
	// 便于运维在同一区域找到全部 features 开关。
	if !strings.Contains(string(html), `<input type="checkbox" name="ptl_max_tokens_retry">`) {
		t.Error("index.html 的 ptl_max_tokens_retry 必须是 checkbox（布尔开关）")
	}
}
