package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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

// TestAppJSDegradeWiring 连败降权三键的面板接线必须齐全（issue #114）：CFG_MAP 映射
// （否则表单值与后端对不上，输入框读不到也存不进去）+ 数字/文本输入项存在 + 账号表
// 状态列能显示降权态（cool_kind=degrade → 「连败降权」标签）。
// app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容——少一处映射，用户改了
// 「连败降权阈值」保存后后端收不到该键，静默不生效（与 TestAppJSMaxRotateWiring 同因）。
func TestAppJSDegradeWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		`degrade_threshold: ['pool', 'degrade_threshold']`,
		`degrade_cooldown: ['pool', 'degrade_cooldown']`,
		`degrade_cooldown_max: ['pool', 'degrade_cooldown_max']`,
		// 状态列渲染：后端在纯降权态下发 cool_kind=degrade，前端必须翻译成人话，
		// 否则用户看到的是「限流冷却」（误导——降权不是限流）。
		`'degrade'`,
		`连败降权`,
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺少连败降权接线：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name="degrade_threshold"`,
		`name="degrade_cooldown"`,
		`name="degrade_cooldown_max"`,
	} {
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
		// 成功解冻回执的渲染：后端在真清了该模型冷却时回传该字段，前端漏渲染则
		// 用户不知道"测一下"顺带摘掉了冷却标记（协议两侧必须同名，见 testchat.go）。
		`model_cooldown_cleared`,
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

// TestAppJSAccountsModelLimitWiring 账号表必须展示模型级冷却台账（issue #199d）：
// 6004 模型级限流 / 11102 该后端无此模型时，账号本身仍算「可用」并能服务其他模型，
// 于是面板状态列显示「可用」而该模型请求 503——用户只能翻日志才知道是谁被限。
// app.js 的 renderAccounts 必须引用 rate_limited_models，且渲染函数与状态列的接线齐全。
//
// 为什么需要：app.js 是 go:embed 的静态资源，Go 编译器不检查其内容——删掉一行插值，
// 面板就回到「完全看不出谁被模型级冷却」，而所有 Go 测试仍会全绿（同 TestAppJSSyntax）。
func TestAppJSAccountsModelLimitWiring(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, must := range []string{
		"rate_limited_models", // 后端字段被引用（此前 0 命中 = 问题根因）
		"function modelLimitTag(",
		"function modelResetText(",
		"modelLimitTag(s.rate_limited_models", // 状态列插入点
		"reset_at", "until",                   // 恢复时刻双来源（reset_at 优先，零值退回 until）
		"11102", // reason 前缀区分「该后端无此模型」与 6004 限流
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺少模型级冷却接线：%s", must)
		}
	}
	// reset_at 的 Go 零值仍是 "0001-01-01T00:00:00Z"（time.Time 是结构体，omitempty
	// 不生效），JS 必须按前缀判零，否则界面上会出现「0001-01-01 恢复」这类垃圾时刻。
	// 断言收在 modelResetText 函数体内——ago() 早有同样的 "0001-" 判零，全文件搜索
	// 无法区分，删掉本函数里的判零也会照样通过。
	body := jsFuncBody(s, "function modelResetText(")
	if body == "" {
		t.Fatal("app.js 缺少函数 modelResetText")
	}
	if !strings.Contains(body, "0001-") {
		t.Error(`modelResetText 未处理 time.Time 零值 reset_at（应含 "0001-" 前缀判零，同 ago()）`)
	}
	if !strings.Contains(body, "reset_at") || !strings.Contains(body, "until") {
		t.Error("modelResetText 未同时读 reset_at 与 until（零值回退链）")
	}
}

// TestAppJSConnLayerWiring 连接层四项（h2 开关 / TLS 握手 / 拨号 / 空闲池）的
// 面板接线必须齐全：CFG_MAP 四键 + index.html 表单项。app.js/index.html 是
// go:embed 静态资源，Go 编译器不校验其内容——少一处映射，用户在面板改了
// 「禁用 HTTP/2」或三个超时保存后后端收不到该键，静默不生效（用户抱怨的正是
// "面板看不到 TLS 配置"，见任务书）。同 TestAppJSMaxRotateWiring 的动因。
//
// h2 的默认语义额外锁一层：checkbox 未勾选 = false = **启用 h2**（不能写成
// "启用 HTTP/2" 的勾选语义——那会让缺省态变成禁用）。
func TestAppJSConnLayerWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		`disable_http2: ['upstream', 'disable_http2']`,
		`tls_handshake_timeout_seconds: ['upstream', 'tls_handshake_timeout_seconds']`,
		`dial_timeout_seconds: ['upstream', 'dial_timeout_seconds']`,
		`idle_conn_timeout_seconds: ['upstream', 'idle_conn_timeout_seconds']`,
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺连接层 CFG_MAP 映射：%s", must)
		}
	}

	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`name="disable_http2"`, `type="checkbox" name="disable_http2"`, // 复选框（缺省不勾 = 启用 h2）
		`name="tls_handshake_timeout_seconds"`,
		`name="dial_timeout_seconds"`,
		`name="idle_conn_timeout_seconds"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 缺连接层表单项 %s", want)
		}
	}
}

// jsFuncBody 返回 src 中名为 sig（形如 "function foo("）的函数体文本，用于把源码断言
// 收窄到单个函数——否则全文件搜索会被别处的同名片段放行。朴素花括号配对（本仓 app.js
// 无模板字符串嵌套花括号的写法，够用）；找不到函数名返回空串。
func jsFuncBody(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	j := strings.Index(src[i:], "{")
	if j < 0 {
		return ""
	}
	depth, start := 0, i+j
	for k := start; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : k+1]
			}
		}
	}
	return ""
}

// jsFuncFull 返回 src 中名为 sig 的函数的**完整文本**（含签名），用于在 node 里
// 实跑纯函数（jsFuncBody 只给花括号体，拼起来不是合法语句）。找不到返回空串。
func jsFuncFull(src, sig string) string {
	body := jsFuncBody(src, sig)
	if body == "" {
		return ""
	}
	i := strings.Index(src, sig)
	j := strings.Index(src[i:], body)
	if j < 0 {
		return ""
	}
	return src[i : i+j+len(body)]
}

// TestPanelOverviewExposesModelLimitLedger 面板 overview 必须把模型级冷却台账透出给前端
// （app.js 读的就是这些键）：6004 条目带 reset_at（上游权威恢复墙钟），11102 条目
// reset_at 为零值而 until 为退避 TTL——前端正是据此退回 until 显示恢复时间。
// 无模型级冷却的账号不得出现该字段（零回归：不凭空多出标签）。
func TestPanelOverviewExposesModelLimitLedger(t *testing.T) {
	type ledgerRow struct {
		Model   string `json:"model"`
		Until   string `json:"until"`
		ResetAt string `json:"reset_at"`
		Reason  string `json:"reason"`
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	resetAt := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.3", "6004 model rate limit")
	p.BlockModelBackoff("u1", "hy3-preview", "11102 model not available") // ResetAt 零值

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Accounts []struct {
			UID               string      `json:"uid"`
			RateLimitedModels []ledgerRow `json:"rate_limited_models"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("overview JSON 解析失败：%v", err)
	}
	byUID := map[string][]ledgerRow{}
	for _, a := range got.Accounts {
		byUID[a.UID] = a.RateLimitedModels
	}
	// u2 无模型级冷却 → 字段缺席（omitempty）；u1 有两条（6004 + 11102）。
	if len(byUID["u2"]) != 0 {
		t.Errorf("无模型级冷却的账号不应带台账：%+v", byUID["u2"])
	}
	if len(byUID["u1"]) != 2 {
		t.Fatalf("u1 台账=%+v want 2 行（6004 + 11102）", byUID["u1"])
	}
	rows := map[string]ledgerRow{}
	for _, r := range byUID["u1"] {
		rows[r.Model] = r
		if r.Until == "" || strings.HasPrefix(r.Until, "0001-") {
			t.Errorf("%s: until 必须可用（前端零值判据）：%+v", r.Model, r)
		}
	}
	// 6004：reset_at 是上游权威恢复时刻（未被 soft_rate_max 截断），前端优先用它。
	if r, ok := rows["glm-5.3"]; !ok {
		t.Error("台账缺 6004 条目 glm-5.3")
	} else if r.ResetAt == "" || strings.HasPrefix(r.ResetAt, "0001-") {
		t.Errorf("6004 条目应带 reset_at 权威恢复时刻：%+v", r)
	}
	// 11102：无重置文案 → reset_at 零值（Go 会序列化成 "0001-01-01T00:00:00Z"，
	// omitempty 对 time.Time 结构体不生效），前端须退回 until；reason 前缀可判别。
	if r, ok := rows["hy3-preview"]; !ok {
		t.Error("台账缺 11102 条目 hy3-preview")
	} else {
		if !strings.HasPrefix(r.ResetAt, "0001-") {
			t.Errorf("11102 条目 reset_at 应为零值（前端靠它走 until 回退）：%+v", r)
		}
		if !strings.HasPrefix(r.Reason, "11102") {
			t.Errorf("11102 条目 reason 前缀应可判别：%+v", r)
		}
	}
}

// TestPanelOverviewExposesCreditsExpiring 缺陷 B 的端到端回归：overview 的 accounts[]
// 必须带 credits_expiring（面板积分列直接读它），且三态在 JSON 上可区分——
// ① 有快过期 → 键带数值；② 无快过期但总额已知 → 键缺席 + credits_total 在场；
// ③ 旧 state/未知 → 两者都缺席。② 与 ③ 不分会让前端把「没有」渲染成「0 分快过期」。
func TestPanelOverviewExposesCreditsExpiring(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}) // ① 有快过期
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999}) // ② 总额已知、无快过期
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at", ExpiresAt: 9999999999}) // ③ 旧 state
	p.SetCreditsDetailed("u1", 1000, 2000, 300)
	p.SetCreditsDetailed("u2", 500, 800, 0)
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		ExpiringSoonSec int64 `json:"expiring_soon_sec"`
		Accounts        []struct {
			UID             string `json:"uid"`
			CreditsTotal    int64  `json:"credits_total"`
			CreditsExpiring *int64 `json:"credits_expiring"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("overview JSON 解析失败：%v", err)
	}
	// 窗口随 overview 下发（前端提示文案据此说清依据，不写死 168h）。
	if got.ExpiringSoonSec != int64((7 * 24 * time.Hour).Seconds()) {
		t.Errorf("overview.expiring_soon_sec=%d want %d（pool.expiring_soon 解析值）",
			got.ExpiringSoonSec, int64((7 * 24 * time.Hour).Seconds()))
	}
	byUID := map[string]struct {
		CreditsTotal    int64  `json:"credits_total"`
		CreditsExpiring *int64 `json:"credits_expiring"`
	}{}
	for _, a := range got.Accounts {
		byUID[a.UID] = struct {
			CreditsTotal    int64  `json:"credits_total"`
			CreditsExpiring *int64 `json:"credits_expiring"`
		}{a.CreditsTotal, a.CreditsExpiring}
	}
	if v := byUID["u1"].CreditsExpiring; v == nil || *v != 300 {
		t.Errorf("① u1 credits_expiring=%v want 300（字段缺失即缺陷 B）", v)
	}
	if v := byUID["u2"].CreditsExpiring; v != nil {
		t.Errorf("② u2 无快过期时不应出现 credits_expiring：%v", *v)
	}
	if byUID["u2"].CreditsTotal != 800 {
		t.Errorf("② u2 必须有 credits_total（前端据此判定「窗口内没有」而非「未知」）：%d", byUID["u2"].CreditsTotal)
	}
	if v := byUID["u3"].CreditsExpiring; v != nil {
		t.Errorf("③ u3（旧 state）不应有 credits_expiring：%v", *v)
	}
	if byUID["u3"].CreditsTotal != 0 {
		t.Errorf("③ u3（旧 state）不应有 credits_total：%d", byUID["u3"].CreditsTotal)
	}
}

// TestAppJSExpiringThreeStates 前端三态判定逻辑（纯 JS，用 node 实跑函数体）：
// ① 有值 → 显示数值；② 字段缺席/0 且总额已知 → **不**显示成「快过期 0」；
// ③ 旧 state/未知 → 与 ② 同处理（不显示数值），文案说明依据。
//
// 为什么在 Go 里跑 node：app.js 是 go:embed 静态资源（Go 编译器不校验其内容），
// 本仓没有 JS 测试框架（frontend_test.go 的既有做法是 node --check 语法校验）。
// 这里把 expiringWindowText/expiringNote 的函数体抽出来，在 node 里以真实入参调用，
// 断言三态可区分且窗口文案来自参数（不写死 168h）；无 node 环境时跳过。
func TestAppJSExpiringThreeStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	winFn := jsFuncFull(src, "function expiringWindowText(")
	noteFn := jsFuncFull(src, "function expiringNote(")
	if winFn == "" || noteFn == "" {
		t.Fatal("app.js 缺 expiringWindowText/expiringNote（快过期三态判定的纯函数）")
	}
	script := winFn + "\n" + noteFn + `
const win7 = expiringWindowText(604800);   // 168h
const win3 = expiringWindowText(259200);   // 72h
const out = {
  win: { w7: win7, w3: win3, w1h: expiringWindowText(3600), wOff: expiringWindowText(0) },
  notes: {
    some: expiringNote({ credits_expiring: 120, credits_total: 8406 }, win7),
    some3: expiringNote({ credits_expiring: 120, credits_total: 8406 }, win3),
    none: expiringNote({ credits_total: 8406 }, win7),   // omitempty：0 时字段缺席
    zero: expiringNote({ credits_expiring: 0, credits_total: 8406 }, win7),
    old: expiringNote({ credits: 500 }, win7),           // 旧 state：无总额
    off: expiringNote({ credits_total: 8406 }, ''),      // 窗口未知/为 0：分桶未启用
  },
};
console.log(JSON.stringify(out));
`
	fp := filepath.Join(t.TempDir(), "expiring_check.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, out)
	}
	type noteCase struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		Tip  string `json:"tip"`
	}
	var got struct {
		Win   map[string]string   `json:"win"`
		Notes map[string]noteCase `json:"notes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, out)
	}
	// 窗口文案由秒数算出（不写死 168h/7 天）：同一函数在不同窗口给出不同文案。
	if got.Win["w7"] != "7 天" || got.Win["w3"] != "3 天" {
		t.Errorf("expiringWindowText 天数换算错误：%v", got.Win)
	}
	if got.Win["w1h"] != "1 小时" || got.Win["wOff"] != "" {
		t.Errorf("expiringWindowText 小时/禁用窗口回落错误：%v", got.Win)
	}
	// ① 有值 → 明确显示数值 + 文案带窗口依据
	if s := got.Notes["some"]; s.Kind != "some" || !strings.Contains(s.Text, "120") || !strings.Contains(s.Tip, "7 天") {
		t.Errorf("① 有快过期应显示数值并带窗口依据：%+v", s)
	}
	// ② 无值（字段缺席或 0）→ 不显示成「快过期 0」，文案说清是窗口内没有
	for _, k := range []string{"none", "zero"} {
		s := got.Notes[k]
		if s.Kind != "none" {
			t.Errorf("② %s 应判为 none（无快过期但总额已知）：%+v", k, s)
		}
		if s.Text != "" {
			t.Errorf("② %s 不得渲染任何数值标记（避免「0 分快过期」误导）：%+v", k, s)
		}
		if !strings.Contains(s.Tip, "7 天") {
			t.Errorf("② %s 的提示必须说明依据窗口：%+v", k, s)
		}
	}
	// ③ 旧 state/未知 → 与 ② 同处理（不显示数值），但 kind 可区分
	if s := got.Notes["old"]; s.Kind != "unknown" || s.Text != "" {
		t.Errorf("③ 旧 state 应判为 unknown 且不显示数值：%+v", s)
	}
	if got.Notes["none"].Kind == got.Notes["old"].Kind {
		t.Error("②「无快过期」与 ③「未知」必须可区分（否则前端无法说明依据）")
	}
	// ④ 窗口未知/为 0（分桶未启用）→ 不得谎称「没有快过期」，与 ② 也必须可区分。
	if s := got.Notes["off"]; s.Kind != "off" || s.Text != "" {
		t.Errorf("④ 分桶未启用应判为 off 且不显示数值：%+v", s)
	}
	if got.Notes["off"].Kind == got.Notes["none"].Kind {
		t.Error("④「分桶未启用」与 ②「窗口内没有」必须可区分（否则会谎报没有快过期积分）")
	}
	// 窗口参数化：3 天窗口下文案必须是 3 天（证明不是硬编码 168h/7 天）。
	if !strings.Contains(got.Notes["some3"].Tip, "3 天") {
		t.Errorf("窗口文案必须来自配置参数：%+v", got.Notes["some3"])
	}
}

// TestAppJSCreditsExpiringWiring 前端接线完整性：overview 的 expiring_soon_sec 必须被
// 读、积分列必须渲染快过期标记、手动刷新 toast 必须带上快过期信息（本 issue 的
// 直接诉求是「到期积分不显示」，只修后端不接线等于没修）。
func TestAppJSCreditsExpiringWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, want := range []string{
		"expiring_soon_sec",            // 窗口来源（后端按 pool.expiring_soon 解析后下发）
		"credits_expiring",             // 三态判据字段
		"function expiringNote(",       // 三态判定
		"function expiringWindowText(", // 窗口文案
		"function expiringToast(",      // toast 后缀
		"function expiringOf(",         // 账号行用的三态结果（窗口取自 overview）
		"expiringOf(s)",                // renderAccounts 接线
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js 缺快过期积分接线：%q", want)
		}
	}
	body := jsFuncBody(s, "function renderAccounts(")
	if body == "" {
		t.Fatal("app.js 缺 renderAccounts")
	}
	if !strings.Contains(body, "expiringOf(") {
		t.Error("renderAccounts 未渲染快过期标记（积分列）")
	}
	// toast 文案（签到 / 余额刷新两处）必须带快过期信息。
	for _, sig := range []string{"accounts/' + encodeURIComponent(u) + '/checkin'", "accounts/' + encodeURIComponent(u) + '/balance'"} {
		i := strings.Index(s, sig)
		if i < 0 {
			t.Fatalf("app.js 缺 %s 调用", sig)
		}
		tail := s[i:]
		if end := strings.Index(tail, "} else"); end > 0 {
			tail = tail[:end]
		}
		if !strings.Contains(tail, "expiringToast(") {
			t.Errorf("%s 的 toast 未带快过期信息：%s", sig, tail)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), ".cred .exp") {
		t.Error("index.html 缺 .cred .exp 样式（快过期标记沿用积分列的既有样式体系）")
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

// TestAppJSMachineIDHeadersWiring upstream.machine_id_headers 的面板接线必须齐全：
// CFG_MAP 映射（缺则表单读不到也存不进）+ index.html 的 checkbox。
// app.js / index.html 是 go:embed 静态资源，Go 编译器不校验其内容——少一处映射，
// 用户取消勾选保存后后端收不到该键，静默仍带设备指纹（与 TestAppJSMaxRotateWiring 同因）。
func TestAppJSMachineIDHeadersWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `machine_id_headers: ['upstream', 'machine_id_headers']`) {
		t.Error("app.js 缺 machine_id_headers 的 CFG_MAP 映射（表单值无法读写 upstream.machine_id_headers）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	// checkbox 必须是 checkbox 类型（CFG_MAP 按 el.type==='checkbox' 走 bool 分支）。
	body := string(html)
	i := strings.Index(body, `name="machine_id_headers"`)
	if i < 0 {
		t.Fatal(`index.html 缺配置表单项 name="machine_id_headers"`)
	}
	// 往前找本 input 标签（同一标签内 type 在前）。
	start := strings.LastIndex(body[:i], "<input")
	if start < 0 || !strings.Contains(body[start:i], `type="checkbox"`) {
		t.Error(`index.html 的 machine_id_headers 必须是 <input type="checkbox">（否则 collectConfig 不走 bool 分支）`)
	}
}

// TestAppJSUsageChartTimeAxis 用量图必须按**真实时间戳**定位数据点，而不是按序号等距：
//
// 后端时序里既有 1 小时的间隔，也有 6~8 小时的断档（没请求的时段不产生桶），按序号
// 等距排布会把 8 小时画得和 1 小时一样宽，「什么时候用的」完全失真；同时
// preserveAspectRatio="none" 会把 760 宽的 viewBox 横向拉伸到容器宽度，柱与文字一起
// 变形。这两点都是**只影响观感、不影响任何 Go 代码**的静默回归——app.js 是 go:embed
// 静态资源，Go 编译器不校验其内容（同 TestAppJSSyntax）。此用例把绘图口径前移。
func TestAppJSUsageChartTimeAxis(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)

	// 时间戳解析辅助函数必须存在（hour "2006-01-02T15" / day "2006-01-02" 两种长度）。
	if !strings.Contains(s, "function parsePointTime(") {
		t.Fatal("app.js 缺少 parsePointTime（时间戳解析辅助函数）")
	}
	body := jsFuncBody(s, "function parsePointTime(")
	if body == "" {
		t.Fatal("app.js parsePointTime 函数体解析失败")
	}
	// day 串必须补 "T00:00:00"：ES 规范里「纯日期」按 UTC 解析，而后端分片键是本地
	// 时区——不补的话东八区整条轴平移 8 小时（图表整体错位却毫无报错）。
	if !strings.Contains(body, "T00:00:00") {
		t.Error(`parsePointTime 未补 "T00:00:00"（纯日期串会按 UTC 解析，与后端本地时区口径错位）`)
	}
	if !strings.Contains(body, "getTime()") {
		t.Error("parsePointTime 未取 getTime() 毫秒时间戳")
	}

	chart := jsFuncBody(s, "function renderUsageChart(")
	if chart == "" {
		t.Fatal("app.js 缺少函数 renderUsageChart")
	}
	// 真实时间轴：x 由时间戳算出，而不是 i * step 之类按序号等距。
	for _, must := range []string{
		"xOf",                                 // 时间戳 → x 的映射函数
		"(t - t0) / span",                     // 真实比例映射
		"degenerate",                          // 单点/同刻的退化保护（不除零）
		`preserveAspectRatio="xMidYMid meet"`, // 固定比例，不再横向拉伸
	} {
		if !strings.Contains(chart, must) {
			t.Errorf("renderUsageChart 缺真实时间轴要素：%q", must)
		}
	}
	// 反向断言收在 svg 开标签的**实际拼接**上：注释里提到过 "none" 这个写法（说明为何
	// 弃用），按子串全局搜索会被注释放行，所以匹配带 role 属性的完整标签片段。
	if strings.Contains(chart, `preserveAspectRatio="none" role="img"`) {
		t.Error(`renderUsageChart 仍用 preserveAspectRatio="none"（横向拉伸会压扁柱与文字）`)
	}
	// 按序号等距的老写法（x = PL + i * step）不得回归。
	if strings.Contains(chart, "i * step") {
		t.Error("renderUsageChart 仍按序号等距排布（x = PL + i * step），时间轴不真实")
	}
}

// TestAppJSUsageWindowWiring 用量视图的「生效窗口 + 粒度单选」接线必须齐全。
//
// 背景是用户实测的两个现象：
//
//	A. 切换范围时顶部六个汇总数字与「按账号」「按模型」两张表纹丝不动。根因在
//	   后端（窗口只作用于时序），但前端**没有任何地方显示实际生效的窗口**，用户
//	   因此无法区分「窗口没生效」与「这段流量本来就一样多」。
//	B. 柱状图粒度与选择相反（选 30 天横轴是小时标签、选 24 小时横轴是日标签）。
//	   根因也在后端（小时点与日点混排），前端则靠 `p.scope === 'day'` 逐点猜粒度，
//	   把混排直接画了出来。
//
// 修复后前端只做两件事：把后端给的 granularity 直接用于标签格式与柱宽，把后端给
// 的 hours（**实际生效值**，非法入参已回退）显示在 usNote 里。此用例把这两条接线
// 前移——app.js 是 go:embed 静态资源，Go 编译器不校验其内容（同
// TestAppJSUsageChartTimeAxis）。
func TestAppJSUsageWindowWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)

	if !strings.Contains(s, "function usWindowText(") {
		t.Fatal("app.js 缺少 usWindowText（窗口文案）——usNote 无法显示实际生效的窗口")
	}

	ru := jsFuncBody(s, "function renderUsage(")
	if ru == "" {
		t.Fatal("app.js 缺少函数 renderUsage")
	}
	// ① usNote 的窗口文案由响应里的 hours 驱动，不写死、也不由前端再猜下拉框的值
	//   （下拉框只反映「用户选了什么」，响应里的 hours 才是「实际生效了什么」）。
	if !strings.Contains(ru, "usWindowText(") || !strings.Contains(ru, "d.hours") {
		t.Error("renderUsage 未把响应里的 d.hours 经 usWindowText 写进 usNote" +
			"（用户无法区分「窗口没生效」与「这段流量本来就一样多」）")
	}
	// ② 粒度由后端的 granularity 字段单选，前端不逐点猜。
	if !strings.Contains(ru, "renderUsageChart(d.series || [], d.granularity") {
		t.Error("renderUsage 未把 d.granularity 传给 renderUsageChart（前端只能自己猜粒度）")
	}

	chart := jsFuncBody(s, "function renderUsageChart(")
	if chart == "" {
		t.Fatal("app.js 缺少函数 renderUsageChart")
	}
	if !strings.Contains(chart, "gran === 'day'") {
		t.Error("renderUsageChart 未按 granularity 决定横轴标签格式（小时 HH:00 / 日 M-D）")
	}
	// 反向断言：逐点按 scope 判粒度的写法不得回归——后端一旦返回混合粒度，那写法
	// 会把 "22:00" 与 "9-17" 两种标签画到同一条轴上（现象 B）。
	for _, banned := range []string{"p.scope === 'day'", "p.scope !== 'day'"} {
		if strings.Contains(chart, banned) {
			t.Errorf("renderUsageChart 仍逐点用 %s 猜粒度：混排时两种标签会同轴", banned)
		}
	}
}

// TestAppJSUsageWindowTextMapping 窗口文案的换算（纯 JS，用 node 实跑函数体）：
// 24/72/168/720 必须分别写成「近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天」，与下拉框
// 选项逐字一致；窗口未知（0/undefined，如旧后端不带 hours）时返回空串，由调用方
// 省略前缀，而不是显示「近 0 小时」。无 node 环境时跳过。
func TestAppJSUsageWindowTextMapping(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := jsFuncFull(string(js), "function usWindowText(")
	if fn == "" {
		t.Fatal("app.js 缺少 usWindowText")
	}
	script := fn + `
const out = {
  h1: usWindowText(1),
  h24: usWindowText(24),
  h72: usWindowText(72),
  h168: usWindowText(168),
  h720: usWindowText(720),
  h1440: usWindowText(1440),
  off: usWindowText(0),
  undef: usWindowText(undefined),
};
console.log(JSON.stringify(out));
`
	fp := filepath.Join(t.TempDir(), "us_window_check.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, out)
	}
	// 与 index.html 下拉框的选项文案逐字对齐，用户切了之后看到的字就是它。
	for k, want := range map[string]string{
		"h1":    "近 1 小时",
		"h24":   "近 24 小时",
		"h72":   "近 3 天",
		"h168":  "近 7 天",
		"h720":  "近 30 天",
		"h1440": "近 60 天",
	} {
		if got[k] != want {
			t.Errorf("usWindowText → %q, want %q", got[k], want)
		}
	}
	if got["off"] != "" || got["undef"] != "" {
		t.Errorf("窗口未知时文案必须为空（省略前缀），得到 %q / %q", got["off"], got["undef"])
	}
}

// TestAppJSAuthWatchWiring schedule.auth_watch_* 的面板接线必须齐全：CFG_MAP 映射
// （否则表单值与后端对不上）+ 开关与数字输入项存在。
//
// 为什么需要：auth_watch_* 是「手工上传凭证文件免重启生效」的唯一开关，接线缺失时
// 面板上是个只显示不保存的装饰控件——用户关掉它以为省了 IO，实际照旧每 30 秒扫描；
// 反之改了间隔也不生效。app.js/index.html 是 go:embed 静态资源，Go 编译器不校验
// 其内容（与 TestAppJSMaxRotateWiring 同因，把接线完整性前移）。
func TestAppJSAuthWatchWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, want := range []string{
		`auth_watch_enabled: ['schedule', 'auth_watch_enabled']`,
		`auth_watch_seconds: ['schedule', 'auth_watch_seconds']`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js 缺 CFG_MAP 映射：%s", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name="auth_watch_enabled"`, `name="auth_watch_seconds"`} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 缺配置表单项 %s", want)
		}
	}
	// 秒数输入必须是 number 类型：collectConfig 按 el.type 决定是否 Number() 转换，
	// 写成文本框会以 string 提交（后端 json 解码 int 失败 → 整个保存请求 400）。
	body := jsFuncBody(s, "const CFG_MAP")
	if body == "" {
		t.Fatal("app.js 缺 CFG_MAP 定义")
	}
	if !strings.Contains(string(html), `name="auth_watch_seconds" type="number"`) {
		t.Error(`auth_watch_seconds 必须是 type="number"（否则提交字符串导致保存失败）`)
	}
}

// TestAppJSReadTimeoutWiring server.read_timeout_seconds 的面板接线必须齐全：
// CFG_MAP 映射（否则表单值与后端对不上，输入框读不到也存不进去）+ 数字输入项存在。
//
// 为什么需要：本键是 v1.9.13 生产事故（慢链路上传大上下文被 60s 读超时掐断、用户
// 对话被拦腰截断）的修复开关——接线缺失时面板上是个只显示不保存的装饰控件，用户
// 以为调大了就完事，实际仍按旧值跑（与 TestAppJSMaxRotateWiring 同因，把接线完整性
// 前移；app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容）。
func TestAppJSReadTimeoutWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	if !strings.Contains(s, `read_timeout_seconds: ['server', 'read_timeout_seconds']`) {
		t.Error("app.js 缺 read_timeout_seconds 的 CFG_MAP 映射（表单值无法读写 server.read_timeout_seconds）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	if !strings.Contains(h, `name="read_timeout_seconds"`) {
		t.Error(`index.html 缺配置表单项 name="read_timeout_seconds"`)
	}
	// 必须是 number 类型：collectConfig 按 el.type 决定是否 Number() 转换，
	// 写成文本框会以 string 提交（后端 json 解码 int 失败 → 整个保存请求 400）。
	if !strings.Contains(h, `name="read_timeout_seconds" type="number"`) {
		t.Error(`read_timeout_seconds 必须是 type="number"（否则提交字符串导致保存失败）`)
	}
	// CFG_MAP 的 server 段必须仍在（防止误删整段把别的 server 键一起带走）。
	body := jsFuncBody(s, "const CFG_MAP")
	if body == "" {
		t.Fatal("app.js 缺 CFG_MAP 定义")
	}
	for _, want := range []string{
		`max_body_mb: ['server', 'max_body_mb']`,
		`read_timeout_seconds: ['server', 'read_timeout_seconds']`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("CFG_MAP 缺 server 段映射：%s", want)
		}
	}
}

// TestAppJSManualDisableWiring 临时停用/恢复的前端接线必须齐全（上游 a20d06f 吸收）：
// 后端字段 manual_disabled/manual_reason → 状态列标签 → 行内「停用/恢复」按钮 → 端点路径。
// 四者任一被误删，面板上就是「后端摘了号但界面上看不出来」或「点了没反应的按钮」，
// 而所有 Go 测试仍会全绿（app.js/index.html 是 go:embed 静态资源，Go 编译器不检查内容）。
//
// 重点锁死「两种不可选分开呈现」：disabled（永久禁用）与 manual_disabled（临时停用）
// 是独立两位、可叠加，面板必须同时显示（合并成一个标签会让运维分不清该点解冻还是恢复）。
func TestAppJSManualDisableWiring(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, must := range []string{
		"manual_disabled",                     // 后端字段被引用
		"manual_reason",                       // 停用原因（与惩罚 reason 分开两行）
		`data-a="suspend"`, `data-a="resume"`, // 行内按钮
		`'/suspend'`, `'/resume'`, // 端点路径（api() 会补 /accounts/{uid} 前缀）
		"临时停用", // 状态标签文案（区别于「已禁用」）
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺少临时停用接线：%s", must)
		}
	}
	// 状态列渲染函数体内必须同时引用两位——全文件搜索无法区分「注释里提到」与
	// 「真的读了字段」，删掉 renderAccounts 里的插值也会照样通过。
	body := jsFuncBody(s, "function renderAccounts(")
	if body == "" {
		t.Fatal("app.js 缺少函数 renderAccounts")
	}
	for _, must := range []string{"manual_disabled", "manual_reason", "s.disabled"} {
		if !strings.Contains(body, must) {
			t.Errorf("renderAccounts 未引用 %s（两种不可选必须分开呈现）", must)
		}
	}
	// 「解冻」按钮只针对惩罚态，临时停用走「恢复」——按钮分支里必须各自出现。
	if !strings.Contains(body, `data-a="suspend"`) || !strings.Contains(body, `data-a="resume"`) {
		t.Error("renderAccounts 未按停用位切换「停用/恢复」按钮")
	}

	// 端点注册必须与前端调用同路径（改了一边忘另一边 = 404）。
	// 路由注册的探测用 GET：已注册的路由对方法不匹配回 405（方法不允许）；未注册则回 404。
	// 不能用 POST + 空池判：那时 "uid 不存在" 也是 404，与"路由未注册"不可区分。
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	for _, p := range []string{"/panel/api/accounts/u1/suspend", "/panel/api/accounts/u1/resume"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		pn.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("panel.go 未注册 POST %s（GET 应回 405，实际 %d）", p, rec.Code)
		}
	}
}

// TestPanelOverviewExposesManualDisable 面板 overview 的每账号状态必须透出
// manual_disabled/manual_reason 两位（app.js 读的就是这些键）。缺了字段，
// 状态列永远显示不出「临时停用」——后端摘了号而界面上看不出来。
func TestPanelOverviewExposesManualDisable(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetManualDisabled("u1", true, "观察几天")

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Disabled int `json:"disabled"`
		Accounts []struct {
			UID            string `json:"uid"`
			Disabled       bool   `json:"disabled"`
			ManualDisabled bool   `json:"manual_disabled"`
			ManualReason   string `json:"manual_reason"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	// 汇总口径：临时停用计入 disabled（保证 total/healthy/cooling/disabled 闭合）。
	if resp.Disabled != 1 {
		t.Errorf("汇总 disabled=%d want 1（临时停用计入不可选）", resp.Disabled)
	}
	byUID := map[string]bool{}
	for _, a := range resp.Accounts {
		if a.ManualDisabled {
			if a.UID != "u1" {
				t.Errorf("u2 不应被连带停用: %+v", a)
			}
			if a.ManualReason != "观察几天" {
				t.Errorf("manual_reason=%q 未透出", a.ManualReason)
			}
			if a.Disabled {
				t.Errorf("临时停用不应置 disabled: %+v", a)
			}
		}
		byUID[a.UID] = a.ManualDisabled
	}
	if len(byUID) != 2 {
		t.Fatalf("accounts 应含两个号: %+v", resp.Accounts)
	}
	if byUID["u2"] {
		t.Error("u2 不应被停用")
	}
}

// TestAppJSReasoningHistoryWiring features.reasoning_history 的面板接线必须齐全：
// CFG_MAP 映射（否则表单值与后端对不上，下拉选了也存不进去）+ index.html 的三档
// 下拉项存在。app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容——
// 少一处映射，用户改了「历史推理文本裁剪」保存后后端收不到该键，静默不生效
// （与 TestAppJSMaxRotateWiring 同因，把接线完整性前移）。
func TestAppJSReasoningHistoryWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `reasoning_history: ['features', 'reasoning_history']`) {
		t.Error("app.js 缺 reasoning_history 的 CFG_MAP 映射（表单值无法读写 features.reasoning_history）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	// 三档 option 必须都在（缺一档 = 用户无法选到该档）。
	for _, want := range []string{
		`name="reasoning_history"`,
		`value="full"`,
		`value="last"`,
		`value="blank"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 缺配置表单项 %s", want)
		}
	}
}
