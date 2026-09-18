package panel

// cooldownprobe_panel_test.go 后台冷却探活的面板接线：
//   - CFG_MAP 两键 + index.html 表单项（否则面板上是个只显示不保存的装饰控件）；
//   - 手动触发端点 POST /panel/api/cooldown_probe/run（前端按钮 → 端点路径 → 回执渲染）。
//
// 与 TestAppJSTestChatWiring 同因：app.js/index.html 是 go:embed 静态资源，Go 编译器
// 不校验其内容——少一处映射或端点路径写错，用户点按钮只会拿到 404 而所有 Go 测试仍全绿。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestAppJSCooldownProbeWiring 配置项两键的面板接线必须齐全（CFG_MAP + 表单项），
// 且分钟数是 type="number"（collectConfig 按 el.type 决定是否 Number() 转换，
// 写成文本框会以 string 提交 → 后端 json 解码 int 失败 → 整个保存请求 400）。
func TestAppJSCooldownProbeWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, want := range []string{
		`cooldown_probe_enabled: ['schedule', 'cooldown_probe_enabled']`,
		`cooldown_probe_minutes: ['schedule', 'cooldown_probe_minutes']`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js 缺 CFG_MAP 映射：%s", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`name="cooldown_probe_enabled"`,
		`name="cooldown_probe_minutes" type="number"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 缺配置表单项 %s", want)
		}
	}
}

// TestAppJSCooldownProbeButtonWiring 手动触发按钮的接线必须齐全：
// 按钮 DOM（btnCooldownProbe）→ 端点路径（cooldown_probe/run）→ 回执渲染字段。
// 三者任一被误删，面板上就是一个点了没反应的按钮（或静默丢结果）。
func TestAppJSCooldownProbeButtonWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		`'btnCooldownProbe'`,        // 按钮
		`'cooldown_probe/run'`,      // 端点（api() 会补 /panel/api/ 前缀）
		`res.probed`, `res.cleared`, // 回执字段（后端 CooldownProbeResult 的 JSON tag）
		`res.accounts`, `res.skipped`,
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺少冷却探活接线：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="btnCooldownProbe"`) {
		t.Error(`index.html 缺按钮 DOM：id="btnCooldownProbe"`)
	}
}

// TestCooldownProbeRunEndpointRegistered 端点注册必须与前端调用同路径（改了一边
// 忘另一边 = 404）。未注入 Scheduler 时返回 501（不是 404，证明路由已注册）。
func TestCooldownProbeRunEndpointRegistered(t *testing.T) {
	pn := newTestPanel() // 无 Scheduler
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/cooldown_probe/run", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	pn.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("panel.go 未注册 POST /panel/api/cooldown_probe/run")
	}
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("code=%d want 501（无 Scheduler 时应报 not available 而非 404）", rec.Code)
	}
}

// TestCooldownProbeRunEndpointProbesAndClears 手动触发端点端到端：上游已提前恢复
// （200 + 合法 SSE）时，点一下按钮即清掉到期的模型级冷却，回执带 result 汇总。
//
// 同时钉住"同步返回"的契约：响应体里直接有结果（不像 checkin_all 那样异步 + 看日志），
// 用户点完就知道结论。
func TestCooldownProbeRunEndpointProbesAndClears(t *testing.T) {
	// calls 用 atomic：写侧是 httptest 的 handler goroutine，读侧是本测试 goroutine。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-p", "object": "chat.completion.chunk", "model": "glm-5.3",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "2"}}},
		})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})

	// 一条已到期的模型级冷却（过去的重置墙钟 → until = now+1ms）。
	p.CooldownSoftForModel("u1", 0, time.Now().Add(-time.Hour), "glm-5.3", "6004 model rate limit")
	deadline := time.Now().Add(2 * time.Second)
	for len(p.CooldownProbeTargets(time.Now())) != 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(p.CooldownProbeTargets(time.Now())); n != 1 {
		t.Fatalf("前置条件失败：应有 1 个到期目标，got %d", n)
	}

	req := httptest.NewRequest("POST", "/panel/api/cooldown_probe/run", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Probed   int  `json:"probed"`
			Cleared  int  `json:"cleared"`
			Accounts int  `json:"accounts"`
			Skipped  bool `json:"skipped"`
		} `json:"result"`
		Accounts []any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	if !out.OK {
		t.Errorf("ok=false body=%s", rec.Body.String())
	}
	if out.Result.Probed != 1 || out.Result.Cleared != 1 {
		t.Errorf("result=%+v want probed=1 cleared=1", out.Result)
	}
	if out.Result.Skipped {
		t.Error("skipped=true want false（没有并发巡检）")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("上游调用数=%d want 1", n)
	}
	if len(out.Accounts) != 1 {
		t.Errorf("响应应回带最新账号列表（供面板免二次拉取），got %d 项", len(out.Accounts))
	}
	// 解冻已生效：该账号对 glm-5.3 恢复可选。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Errorf("探活成功后账号应可被选中，got %+v", got)
	}
}
