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

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsRealmsDifferAndBothServable 两个域的列表**确实不同**，且都能查：
//
//	CN     ：/console 家族 + 国内模型（hy4-preview-x / kimi-k2.7 …）
//	global ：/v2 家族 + 国际模型（deepseek-v4.1-flash-sg / glm-5.3-flash …）
//
// 这是「面板只有一个列表、看不到两个域差异」的结构性缺陷的回归点：?realm= 早在
// panel.go 里实现，但前端没有选择器，用户永远只看得到一个域。
//
// 同时锁 realm_servable：前端据它决定显示哪几个域的选择器（池内没有的域不该出现
// 一个点了必然 503 的按钮）。
func TestPanelModelsRealmsDifferAndBothServable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/console/enterprises/personal/models":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00 credits"},
				{"id":"hy4-preview-x","name":"Hy4 preview X","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"},
				{"id":"kimi-k2.7","name":"Kimi-K2.7","maxInputTokens":256000,"maxOutputTokens":32000,"credits":"x0.57 credits"}
			],"agents":[{"name":"cli","models":["hy3","hy4-preview-x","kimi-k2.7"]}]}}`))
		case "/v2/enterprises/personal/models":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.00"}
			],"agents":[{"name":"cli","models":["hy3","hy4-preview"]}]}}`))
		case "/v3/config":
			// CLI 路：5 条国际独有的 v3-only 模型都在这里。
			if r.Header.Get("User-Agent") == "CLI/2.63.2 CodeBuddy/2.63.2" {
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
					{"id":"deepseek-v4.1-flash","name":"DS Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.00"},
					{"id":"deepseek-v4.1-flash-sg","name":"DS Flash SG","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03"},
					{"id":"glm-5.3-flash","name":"GLM-5.3-Flash","maxInputTokens":1000000,"maxOutputTokens":32000,"credits":"x0.06","supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}},
					{"id":"kimi-k2.8-preview","name":"Kimi-K2.8-Preview","maxInputTokens":1000000,"maxOutputTokens":32000,"credits":"x0.77","supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}},
					{"id":"gpt-6-astra","name":"GPT-6-Astra","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x6.67","supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","medium","high","xhigh","max"]}}
				]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[],"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
				{"modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14}
			]}}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "tok-cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok-gl"})

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	pn := New(Config{
		Version: "test", APIKey: "k", Pool: p, Upstream: up,
		HiddenModels: upstream.ResolveHiddenModels(nil),
		PinnedModels: upstream.ResolvePinnedModels(nil),
	})

	fetch := func(query string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/panel/api/models"+query, nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s code=%d body=%s", query, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s json: %v", query, err)
		}
		return body
	}
	idsOfBody := func(body map[string]any) map[string]bool {
		out := map[string]bool{}
		ms, _ := body["models"].([]any)
		for _, raw := range ms {
			m, _ := raw.(map[string]any)
			if id, _ := m["id"].(string); id != "" {
				out[id] = true
			}
		}
		return out
	}

	cnBody := fetch("?realm=cn")
	glBody := fetch("?realm=global")
	if cnBody["realm"] != "cn" || glBody["realm"] != "global" {
		t.Fatalf("realm 回显=%v/%v want cn/global", cnBody["realm"], glBody["realm"])
	}
	cn, gl := idsOfBody(cnBody), idsOfBody(glBody)

	// 两个域确实不同（核心断言：不是同一份列表）。
	if cn["kimi-k2.7"] == gl["kimi-k2.7"] {
		t.Errorf("kimi-k2.7 应只在 CN：cn=%v gl=%v", cn["kimi-k2.7"], gl["kimi-k2.7"])
	}
	if !cn["kimi-k2.7"] || !cn["hy4-preview-x"] {
		t.Errorf("CN 列表缺国内独有模型：%v", sortedIDKeys(cn))
	}
	if gl["hy4-preview-x"] || gl["kimi-k2.7"] {
		t.Errorf("global 列表混入 CN 模型：%v", sortedIDKeys(gl))
	}

	// 任务 A 的核心：5 条 v3-only 必须出现在 global 面板里。
	for _, id := range []string{"deepseek-v4.1-flash-sg", "glm-5.3-flash", "kimi-k2.8-preview", "gpt-6-astra", "hy4-preview-f"} {
		if !gl[id] {
			t.Errorf("global 面板缺 %s（v3 独有 id 未追加），实际=%v", id, sortedIDKeys(gl))
		}
	}
	// 价格优先级：hy4-preview 企业端点 x0.00 权威（v3 无同名条目时也不得凭空生成）。
	for _, raw := range glBody["models"].([]any) {
		m, _ := raw.(map[string]any)
		if m["id"] == "hy4-preview" && m["credits"] != "x0.00" {
			t.Errorf("hy4-preview credits=%v want x0.00（企业端点权威）", m["credits"])
		}
	}

	// realm_servable：前端据此决定显示哪几个域的选择器。
	rs, _ := cnBody["realm_servable"].(map[string]any)
	if rs["cn"] != true || rs["global"] != true {
		t.Fatalf("realm_servable=%v want cn/global 都 true（池内两域都有账号）", cnBody["realm_servable"])
	}
	// 缺省域：两域都在时后端默认 global（既有语义），前端据回显选中它。
	if def := fetch(""); def["realm"] != "global" {
		t.Errorf("缺省 realm=%v want global（两域都可用时的既有默认）", def["realm"])
	}
}

// TestPanelModelsRealmServableSingleRealm 池内只有 CN 账号：realm_servable.global=false
// （前端不显示国际版选择器），缺省域回落到 cn（既有语义）。
func TestPanelModelsRealmServableSingleRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/console/enterprises/personal/models" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"hy3","maxInputTokens":192000}],"agents":[{"name":"cli","models":["hy3"]}]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "tok-cn"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})

	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["realm"] != "cn" {
		t.Errorf("纯 CN 部署缺省 realm=%v want cn", body["realm"])
	}
	rs, _ := body["realm_servable"].(map[string]any)
	if rs["cn"] != true || rs["global"] != false {
		t.Errorf("realm_servable=%v want cn=true global=false", body["realm_servable"])
	}
}

// TestAppJSModelsRealmSwitch 模型页的 realm 选择器接线必须齐全（app.js/index.html
// 是 go:embed 静态资源，Go 编译器不校验其内容——少一处接线就是"后端支持 ?realm=
// 但用户永远切不了"的静默缺陷）。
func TestAppJSModelsRealmSwitch(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		`$('mdRealms')`,   // 选择器容器（index.html 里定义、app.js 里渲染）
		`realm_servable`,  // 可用域判据（池内没有的域不显示按钮）
		`'?realm='`,       // 带 realm 的请求
		`$('mdRealmCur')`, // 当前域 + 模型数回显
		`data-realm`,      // 按钮 → 切换的接线
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺模型页 realm 切换接线：%s", must)
		}
	}
	// 切换必须真的重发请求（只改文案不发请求 = 装饰控件）。
	if !strings.Contains(s, "mdRealm = b.dataset.realm") || !strings.Contains(s, "loadModels();") {
		t.Error("app.js 的 realm 按钮未接线到 loadModels（切换不会重新查询）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{`id="mdRealms"`, `id="mdRealmCur"`} {
		if !strings.Contains(string(html), must) {
			t.Errorf("index.html 缺元素 %s", must)
		}
	}
}

// TestAppJSRealmRenderLogic 选择器渲染逻辑（纯 JS，用 node 实跑 renderModelRealms）：
//
//	两域可用 → 两个按钮、当前域带 on、回显「国际版 · N 个模型」；
//	仅一域可用 → 不渲染按钮（一个按钮的"选择器"是噪音）、回显标注"池内只有这一个域"。
//
// 无 node 环境时跳过（同 frontend_test.go 既有做法）。
func TestAppJSRealmRenderLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := jsFuncFull(string(src), "function renderModelRealms")
	if fn == "" {
		t.Fatal("app.js 里找不到 renderModelRealms（realm 选择器渲染函数被删/改名？）")
	}
	const label = `const REALM_LABEL = { cn: '国内版', global: '国际版' };`
	script := `
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
` + label + `
let mdRealm = null;
function makeEl() {
  const el = { innerHTML: '', textContent: '', buttons: [] };
  el.querySelectorAll = () => el.buttons;
  return el;
}
const els = { mdRealms: makeEl(), mdRealmCur: makeEl() };
const $ = id => els[id];
` + fn + `
const out = {};
renderModelRealms({ realm: 'global', realm_servable: { cn: true, global: true }, models: [{id:'a'},{id:'b'}] });
out.two = { html: els.mdRealms.innerHTML, cur: els.mdRealmCur.textContent };
renderModelRealms({ realm: 'cn', realm_servable: { cn: true, global: false }, models: [{id:'a'}] });
out.one = { html: els.mdRealms.innerHTML, cur: els.mdRealmCur.textContent };
process.stdout.write(JSON.stringify(out));
`
	fp := filepath.Join(t.TempDir(), "realms.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, raw)
	}
	var got map[string]struct {
		HTML string `json:"html"`
		Cur  string `json:"cur"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, raw)
	}
	// 两域：两个按钮，当前域（global）带 on。
	if !strings.Contains(got["two"].HTML, `data-realm="cn"`) || !strings.Contains(got["two"].HTML, `data-realm="global"`) {
		t.Errorf("两域可用时应渲染两个按钮: %s", got["two"].HTML)
	}
	if !strings.Contains(got["two"].HTML, `data-realm="global" class="xs chip on"`) &&
		!strings.Contains(got["two"].HTML, `class="xs chip on" data-realm="global"`) {
		t.Errorf("当前域按钮应带 on: %s", got["two"].HTML)
	}
	if got["two"].Cur != "国际版 · 2 个模型" {
		t.Errorf("回显=%q want 国际版 · 2 个模型", got["two"].Cur)
	}
	// 单域：无按钮 + 回显标注。
	if strings.Contains(got["one"].HTML, "data-realm") {
		t.Errorf("单域可用时不应渲染按钮: %s", got["one"].HTML)
	}
	if !strings.Contains(got["one"].Cur, "国内版 · 1 个模型") || !strings.Contains(got["one"].Cur, "池内只有这一个域") {
		t.Errorf("单域回显=%q want 含「国内版 · 1 个模型」与「池内只有这一个域」", got["one"].Cur)
	}
}

// sortedIDKeys 排序后的 id 列表（失败信息可读）。
func sortedIDKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
