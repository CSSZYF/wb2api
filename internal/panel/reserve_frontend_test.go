package panel

import (
	"os"
	"strings"
	"testing"
)

// TestAppJSReserveCreditsWiring pool.reserve_credits 的面板接线必须齐全：CFG_MAP 映射
// （否则表单值与后端对不上，输入框读不到也存不进去）+ index.html 的数字输入项存在。
//
// app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容——少一处映射，用户在
// 面板改了「保留积分」保存后后端收不到该键，静默不生效（与 TestAppJSMaxRotateWiring
// / TestAppJSDegradeWiring 同因：issue #17 那类"改了配置却不生效"的失效模式）。
func TestAppJSReserveCreditsWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	if !strings.Contains(s, `reserve_credits: ['pool', 'reserve_credits']`) {
		t.Error("app.js 缺 reserve_credits 的 CFG_MAP 映射（表单值无法读写 pool.reserve_credits）")
	}
	// CFG_MAP 的 pool 段必须仍在（防止误删整段把别的 pool 键一起带走）。
	body := jsFuncBody(s, "const CFG_MAP")
	if body == "" {
		t.Fatal("app.js 缺 CFG_MAP 定义")
	}
	for _, want := range []string{
		"['pool', 'reserve_credits']",
		"['pool', 'pick_mode']",
		"['pool', 'max_in_flight']",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("CFG_MAP 缺 pool 段映射：%s", want)
		}
	}

	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	if !strings.Contains(h, `name="reserve_credits"`) {
		t.Error("index.html 缺保留积分的数字输入项（name=reserve_credits）")
	}
	// 表单类型必须是 number（CFG_MAP 取值类型约定：type=number → number，否则按 string
	// 收——字符串 "50" 传给 int 字段会让后端 json.Unmarshal 报错，整个保存失败）。
	i := strings.Index(h, `name="reserve_credits"`)
	if i < 0 {
		t.Fatal("index.html 缺 reserve_credits 输入项")
	}
	seg := h[i:min(i+160, len(h))]
	if !strings.Contains(seg, `type="number"`) {
		t.Errorf("reserve_credits 输入项必须是 type=number（否则按字符串提交，后端解析失败）：%s", seg)
	}
	// placeholder 必须与后端默认值一致：面板显示 50、实际按别的数跑是最容易误判的漂移。
	if !strings.Contains(seg, `placeholder="50"`) {
		t.Errorf("reserve_credits 的 placeholder 必须与后端默认 50 一致：%s", seg)
	}
	// 提示文案要说明 0 = 关闭（否则用户不知道能不能关掉这个闸门）。
	if !strings.Contains(h, "0 = 关闭") {
		t.Error("index.html 缺「0 = 关闭」的说明（用户无从知道该闸门可否关掉）")
	}
}
