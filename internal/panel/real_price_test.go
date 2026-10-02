package panel

// real_price_test.go 面板「真实价格 + realm 前缀 + 点击复制」回归（分支 feat/real-price）。
//
// 用户需求（原话）：①「我要看到真实价格——我能请求什么模型、请求这个模型在现在的
// 积分倍率是多少」；②「在国际服和国服下的模型名下面标上国际服/国服前缀」；③「点一下
// 模型名就可以复制（带前缀的完整 id），方便我手动只添加国服或只添加国际服」。
//
// 权威依据（2026-10-02，真实 global 账号直连上游，原始 dump 存于 D:/wb2tmp/upstream/
// multi_23a5927f_*.json，字节级核对）：
//   - 企业端点 /v2/enterprises/personal/models：18 条，**不含 deepseek 系列**；
//     hy4-preview / hy3 的 credits 是 x0.00（折后实扣口径）；
//   - v3-CLI /v3/config：22 条，含 deepseek-v4.1-flash（x0.00，免费写在 credits 本身）、
//     deepseek-v4.1-flash-sg（x0.03）、gpt-6-astra（x6.67）——企业端点一条都没有；
//   - v3-IDE：13 条，含 credits 为空的条目（如 o4-mini）——缺失 ≠ 免费，必须显示 "—"。
//
// 本文件的三条红线：
//   1. 面板 models 响应对**每一条**都要给出该模型此刻的真实计费依据：credits 原文 +
//      free 旗标（生效倍率 ≤ 0 才为 true；缺失/未知一律不给，绝不编造、绝不用 0 顶替）；
//   2. 每条都带 **realm 前缀的完整 id**（prefixed_id，形式取自后端路由协议
//      internal/server/resolve_model.go 的 splitRealmPrefix：cn: / global:）——用户手动
//      加模型只走一个域就靠它；
//   3. 写死条目（PinnedModels）只在**上游没给该字段**时兜底（字段级），上游给了就
//      以上游为准（写死快照是人工跟进的，可能过期）。

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

// rpFake 假上游：按路径 + UA 分路（与真实上游同形态）。空串表示该端点 404。
type rpFake struct {
	console    string // CN 企业端点 /console/enterprises/personal/models
	enterprise string // global 企业端点 /v2/enterprises/personal/models
	v3CLI      string // /v3/config + CLI UA
	v3IDE      string // /v3/config + 其他 UA
}

// rpDump 与实测 dump 同构的最小复刻（只保留影响结论的字段）。
func rpDump() rpFake {
	return rpFake{
		console: `{"code":0,"data":{"models":[
			{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00 credits"},
			{"id":"kimi-k2.7","name":"Kimi-K2.7","maxInputTokens":256000,"maxOutputTokens":32000,"credits":"x0.57"}
		],"agents":[{"name":"cli","models":["hy3","kimi-k2.7"]}]}}`,
		// 计费目录：hy4-preview/hy3 是 x0.00（折后实扣），**故意没有 deepseek 系列**。
		// hy3 挂一条生效中的限时免费 promo（factor 0 + discountedCredits "0x"）。
		enterprise: `{"code":0,"data":{
			"models":[
				{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"gpt-5.4","name":"GPT 5.4","maxInputTokens":262144,"maxOutputTokens":65536,"credits":"x1.65"},
				{"id":"no-price-model","name":"No Price","maxInputTokens":131072,"maxOutputTokens":32768}
			],
			"agents":[{"name":"cli","models":["hy4-preview","hy3","gpt-5.4","no-price-model"]}],
			"modelPromotions":[
				{"enabled":true,"priority":200,"modelIds":["hy3"],"badge":{"label":"限时免费"},
				 "discount":{"discountedCredits":"0x","factor":0},
				 "hover":{"textZh":"7月6日–10月31日，每日赠送免费额度。"},
				 "schedule":{"timezone":"Asia/Shanghai","validFrom":"2020-01-01T00:00:00+08:00","validUntil":"2099-01-01T00:00:00+08:00"}}
			]
		}}`,
		// CLI 路：企业端点没有的 id（deepseek 系列只在这里）+ hy4-preview 的**牌价**
		// x0.29（不得覆盖企业端点的 x0.00）。
		v3CLI: `{"code":0,"data":{"models":[
			{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"},
			{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.00","supportsImages":true,"supportsReasoning":true,"reasoning":{"effort":"high"}},
			{"id":"deepseek-v4.1-flash-sg","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03","supportsImages":true,"supportsReasoning":true,"reasoning":{"effort":"high"}},
			{"id":"gpt-6-astra","name":"GPT-6-Astra","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x6.67","supportsImages":true,"supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}}
		]}}`,
		// IDE 路：credits 为空的条目（实测 o4-mini 就是空）——缺失 ≠ 免费，面板必须 "—"。
		v3IDE: `{"code":0,"data":{"models":[
			{"id":"o4-mini","name":"GPT-4o-Mini","maxInputTokens":104000,"maxOutputTokens":24000}
		]}}`,
	}
}

// rpServer 起假上游（按路径 + UA 分路）。
func (f rpFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/enterprises/personal/models":
			if f.enterprise == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(f.enterprise))
		case "/console/enterprises/personal/models":
			if f.console == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(f.console))
		case "/v3/config":
			if r.Header.Get("User-Agent") == "CLI/2.63.2 CodeBuddy/2.63.2" {
				if f.v3CLI == "" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(f.v3CLI))
				return
			}
			if f.v3IDE == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(f.v3IDE))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rpPanel 建面板：池内 CN + global 账号各一，两条 base 都指向假上游。
func rpPanel(t *testing.T, f rpFake, pinned []upstream.PinnedModel) *Panel {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	srv := f.server(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "tok-cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok-gl"})

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	return New(Config{
		Version: "test", APIKey: "k", Pool: p, Upstream: up,
		HiddenModels: upstream.ResolveHiddenModels(nil),
		PinnedModels: upstream.ResolvePinnedModels(pinned),
	})
}

// rpEntry 面板 models 响应里一条模型条目（只取本文件断言的字段）。
type rpEntry struct {
	ID            string   `json:"id"`
	PrefixedID    string   `json:"prefixed_id"`
	Name          string   `json:"name"`
	Credits       string   `json:"credits"`
	Free          *bool    `json:"free"`
	PromoFactor   *float64 `json:"promo_factor"`
	PromoCredits  string   `json:"promo_credits"`
	PromoLabel    string   `json:"promo_label"`
	ContextLength int64    `json:"context_length"`
}

// rpFetch 打面板 models 端点，返回按 id 建索引的条目。
func rpFetch(t *testing.T, pn *Panel, query string) map[string]rpEntry {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/models"+query, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s code=%d body=%s", query, rec.Code, rec.Body.String())
	}
	var body struct {
		Realm  string    `json:"realm"`
		Models []rpEntry `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s json: %v", query, err)
	}
	if body.Realm == "" {
		t.Fatalf("%s 响应缺 realm 回显", query)
	}
	out := make(map[string]rpEntry, len(body.Models))
	for _, m := range body.Models {
		out[m.ID] = m
	}
	return out
}

// TestPanelModelsGlobalRealCredits 面板 global 侧必须给出**真实价格**：
//   - v3-CLI 独有的 deepseek-v4.1-flash 是 x0.00（免费写在 credits 字段本身）、
//     -sg 变体 x0.03、gpt-6-astra x6.67；
//   - 企业端点（计费目录）的 hy4-preview x0.00 权威，不被 v3 的牌价 x0.29 覆盖；
//   - 生效中的 promo 必须挂在同一条上（生效价 0x + 标签 + 划线牌价）；
//   - 上游没给倍率的条目保持空串 + 无 free 旗标（前端 "—"，不得回填 0、不得标免费）。
//
// free 旗标是「x0.00 一眼看出免费」的后端口径：生效倍率（有 promo 用 promo factor，
// 否则解析牌价）≤ 0 才为 true——判定逻辑与保留积分的免费判定（upstream.EffectiveMultiplier
// / FreeModelMultiplierThreshold）同源，前端只负责渲染，不再各写一份解析。
func TestPanelModelsGlobalRealCredits(t *testing.T) {
	pn := rpPanel(t, rpDump(), nil)
	gl := rpFetch(t, pn, "?realm=global")

	want := map[string]string{
		"deepseek-v4.1-flash":    "x0.00", // 用户点名的免费模型
		"deepseek-v4.1-flash-sg": "x0.03",
		"gpt-6-astra":            "x6.67",
		"hy4-preview":            "x0.00", // 企业端点（计费目录）权威
		"gpt-5.4":                "x1.65",
		"hy3":                    "x0.00",
	}
	for id, w := range want {
		m, ok := gl[id]
		if !ok {
			t.Errorf("global 面板缺 %s，实际 ids=%v", id, rpIDs(gl))
			continue
		}
		if m.Credits != w {
			t.Errorf("%s credits=%q want %q（真实计费倍率）", id, m.Credits, w)
		}
	}

	// free 旗标：x0.00 与「限时免费 promo（factor 0）」为 true；要收费的不给。
	for _, id := range []string{"deepseek-v4.1-flash", "hy4-preview", "hy3"} {
		if m := gl[id]; m.Free == nil || !*m.Free {
			t.Errorf("%s free=%v want true（真实免费价，必须一眼可辨）", id, m.Free)
		}
	}
	for _, id := range []string{"deepseek-v4.1-flash-sg", "gpt-6-astra", "gpt-5.4"} {
		if m := gl[id]; m.Free != nil {
			t.Errorf("%s free=%v want 不出现（要收费）", id, *m.Free)
		}
	}

	// promo：hy3 的限时免费生效中 → 生效价 0x + 标签（前端渲染「0x 限时免费 ~~x0.00~~」）。
	if m := gl["hy3"]; m.PromoFactor == nil || *m.PromoFactor != 0 || m.PromoCredits != "0x" || m.PromoLabel != "限时免费" {
		t.Errorf("hy3 promo 字段不全: factor=%v credits=%q label=%q（global 数据下 promo 必须能渲染）",
			m.PromoFactor, m.PromoCredits, m.PromoLabel)
	}

	// 上游没给倍率 → 空串 + 无 free（缺失 ≠ 免费）。前端 rateCell 渲染 "—"。
	if m := gl["no-price-model"]; m.Credits != "" || m.Free != nil {
		t.Errorf("no-price-model credits=%q free=%v want 空/不出现（不得编造）", m.Credits, m.Free)
	}
	if m := gl["o4-mini"]; m.Credits != "" || m.Free != nil {
		t.Errorf("o4-mini credits=%q free=%v want 空/不出现（上游给空）", m.Credits, m.Free)
	}
}

// TestPanelModelsCNCreditsUnchanged CN 侧既有口径不回归：牌价原样透出、带 ' credits'
// 尾巴要归一、promo 照旧挂（hy3 的限时免费是 CN 侧的既有用例形态）。
func TestPanelModelsCNCreditsUnchanged(t *testing.T) {
	pn := rpPanel(t, rpDump(), nil)
	cn := rpFetch(t, pn, "?realm=cn")

	if m, ok := cn["kimi-k2.7"]; !ok || m.Credits != "x0.57" {
		t.Errorf("kimi-k2.7 credits=%q（want x0.57，实际条目 %v）", m.Credits, ok)
	}
	// 上游把倍率发成 "x0.00 credits"（实测 CN 23/31 条带尾巴）——必须归一。
	if m := cn["hy3"]; m.Credits != "x0.00" {
		t.Errorf("hy3 credits=%q want x0.00（去 ' credits' 尾巴）", m.Credits)
	}
	if m := cn["hy3"]; m.Free == nil || !*m.Free {
		t.Errorf("hy3 free=%v want true（CN 侧 promo 生效中）", m.Free)
	}
}

// TestPanelModelsPrefixedID 每条模型都带 **realm 前缀的完整 id**（用户手动加模型用）：
// global 域 → "global:<id>"，cn 域 → "cn:<id>"；id 字段保持裸名（既有契约不破）。
// 前缀形式取自后端路由协议（internal/server/resolve_model.go 的 splitRealmPrefix：
// 第一个冒号前恰为 cn/global 才算前缀）——不许自创形式（如 "intl:"）。
func TestPanelModelsPrefixedID(t *testing.T) {
	pn := rpPanel(t, rpDump(), nil)

	for _, tc := range []struct{ realm, prefix string }{{"global", "global:"}, {"cn", "cn:"}} {
		got := rpFetch(t, pn, "?realm="+tc.realm)
		if len(got) == 0 {
			t.Fatalf("realm=%s 列表为空", tc.realm)
		}
		for id, m := range got {
			if strings.Contains(id, ":") {
				t.Errorf("%s: id 不应带前缀（既有契约），got %q", tc.realm, id)
			}
			if want := tc.prefix + id; m.PrefixedID != want {
				t.Errorf("%s: %s prefixed_id=%q want %q", tc.realm, id, m.PrefixedID, want)
			}
		}
		// 两个域各抽查一条真实模型，确认不是"整体空串"式通过。
		if tc.realm == "global" {
			if _, ok := got["deepseek-v4.1-flash"]; !ok {
				t.Errorf("global 面板缺 deepseek-v4.1-flash（复制目标），实际=%v", rpIDs(got))
			}
		} else if _, ok := got["kimi-k2.7"]; !ok {
			t.Errorf("cn 面板缺 kimi-k2.7，实际=%v", rpIDs(got))
		}
	}
}

// TestPanelModelsPinnedCreditsFallbackOnly 写死条目的价格**只在字段级缺失时兜底**：
//   - 上游返回该 id 但 credits 为空 → 用 pinned 的 x0.00（否则用户看到 "—"，而真实
//     价其实已知）；
//   - 上游给了 credits → 上游权威（写死快照是人工跟进的，可能过期）。
func TestPanelModelsPinnedCreditsFallbackOnly(t *testing.T) {
	pinned := []upstream.PinnedModel{{
		ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", Credits: "x0.00",
		ContextLength: 1000000, MaxOutputTokens: 128000, DefaultEffort: "high", SupportsReasoning: true,
	}}
	entWith := func(credits string) rpFake {
		f := rpDump()
		cr := ""
		if credits != "" {
			cr = `,"credits":"` + credits + `"`
		}
		f.enterprise = `{"code":0,"data":{"models":[
			{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000` + cr + `}
		],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`
		f.v3CLI = "" // v3-CLI 探测失败：deepseek 的价格只能来自 pinned 兜底
		return f
	}

	// 上游没给价 → pinned 兜底（字段级）。
	gl := rpFetch(t, rpPanel(t, entWith(""), pinned), "?realm=global")
	m, ok := gl["deepseek-v4.1-flash"]
	if !ok {
		t.Fatal("缺 deepseek-v4.1-flash")
	}
	if m.Credits != "x0.00" {
		t.Errorf("上游未给价时 credits=%q want x0.00（写死条目兜底）", m.Credits)
	}
	if m.Free == nil || !*m.Free {
		t.Errorf("pinned 兜底的 x0.00 也应带 free 旗标，got %v", m.Free)
	}
	if m.ContextLength != 1000000 {
		t.Errorf("上游未给窗口时 context_length=%d want 1000000（写死条目兜底）", m.ContextLength)
	}

	// 上游给了价 → 上游权威（写死值让位）。
	gl = rpFetch(t, rpPanel(t, entWith("x0.09"), pinned), "?realm=global")
	if m := gl["deepseek-v4.1-flash"]; m.Credits != "x0.09" {
		t.Errorf("上游给价时 credits=%q want x0.09（上游数据优先于写死快照）", m.Credits)
	}
	if m := gl["deepseek-v4.1-flash"]; m.Free != nil {
		t.Errorf("上游给 x0.09 时不得标免费，got free=%v", *m.Free)
	}
}

// TestAppJSModelPriceAndCopyWiring 模型页的价格渲染与点击复制接线必须齐全
// （app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其内容——少一处接线就是
// 静默缺陷：价格标签不显示、点了模型名没反应）。
func TestAppJSModelPriceAndCopyWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		`function rateCell`,            // 倍率列渲染
		`m.free`,                       // 免费旗标（后端口径）
		`免费`,                           // x0.00 的一眼可读标签
		`function prefixedModelID`,     // 带 realm 前缀的完整 id 构造（纯函数，node 可跑）
		`prefixed_id`,                  // 后端给的带前缀 id（渲染 + 复制同源）
		`data-copy-id`,                 // 行上挂复制目标
		`querySelectorAll('.md-copy')`, // 点击接线（渲染后逐行绑）
		`copyText(id)`,                 // 复用既有复制实现（含 execCommand 降级）
		`已复制`,                          // 成功反馈（toast）
		`REALM_BADGE`,                  // 国服/国际服徽章文案表
		`REALM_BADGE[d.realm]`,         // 徽章按当前域渲染（不是写死一个域）
		`class="pid"`,                  // 带前缀 id 的第三行（可见 = 可核对）
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺模型页价格/复制接线：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{`id="mdBody"`, `.md-copy`, `.who .pid`, `.realm-tag`} {
		if !strings.Contains(string(html), must) {
			t.Errorf("index.html 缺 %s", must)
		}
	}
}

// TestAppJSModelPriceAndCopyLogic 纯 JS 实跑（node）：
//   - rateCell：x0.00 + free → 带「免费」标签；x0.11 → 原样；空 → "—"（不编造）；
//     promo（0x + 标签）→ 生效价 + 标签 + 划线牌价 + 悬停说明；
//   - prefixedModelID：cn/global 加前缀，已带前缀的**不重复加**，空 id / 未知域不加。
func TestAppJSModelPriceAndCopyLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, sig := range []string{"function esc", "function rateCell", "function prefixedModelID"} {
		fn := jsFuncFull(string(src), sig)
		if fn == "" {
			t.Fatalf("app.js 里找不到 %s（被删/改名？）", sig)
		}
		parts = append(parts, fn)
	}
	script := strings.Join(parts, "\n") + `
const out = {
  free: rateCell({ credits: 'x0.00', free: true }),
  paid: rateCell({ credits: 'x0.11' }),
  none: rateCell({ credits: '' }),
  missing: rateCell({}),
  promo: rateCell({ credits: 'x0.29', free: true, promo_factor: 0, promo_credits: '0x', promo_label: '限时免费', promo_note: 'Free daily use' }),
  promoNoLabel: rateCell({ credits: 'x0.29', free: true, promo_factor: 0, promo_credits: '0x' }),
  pidGlobal: prefixedModelID('global', 'deepseek-v4.1-flash'),
  pidCN: prefixedModelID('cn', 'hy3'),
  pidAlready: prefixedModelID('global', 'cn:hy3'),
  pidEmpty: prefixedModelID('global', ''),
  pidUnknownRealm: prefixedModelID('', 'hy3'),
};
process.stdout.write(JSON.stringify(out));
`
	fp := filepath.Join(t.TempDir(), "price.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, raw)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, raw)
	}
	// x0.00 = 真实免费价：必须一眼看出（免费标签 + 原文）。
	if !strings.Contains(got["free"], "x0.00") || !strings.Contains(got["free"], "免费") {
		t.Errorf("x0.00 应显示原文 + 免费标签，got %q", got["free"])
	}
	if strings.Contains(got["paid"], "免费") || !strings.Contains(got["paid"], "x0.11") {
		t.Errorf("x0.11 不应带免费标签，got %q", got["paid"])
	}
	// 缺失 ≠ 免费：不得出现 0 或免费标签。
	for _, k := range []string{"none", "missing"} {
		if got[k] != "—" {
			t.Errorf("%s 应显示 —（不编造），got %q", k, got[k])
		}
	}
	// promo：生效价 + 标签 + 划线牌价 + 悬停说明。
	if !strings.Contains(got["promo"], "0x") || !strings.Contains(got["promo"], "限时免费") ||
		!strings.Contains(got["promo"], "<s") || !strings.Contains(got["promo"], "x0.29") ||
		!strings.Contains(got["promo"], "title=") {
		t.Errorf("promo 渲染不完整（生效价/标签/划线牌价/悬停）: %q", got["promo"])
	}
	// 无标签的免费 promo 也要能看出免费。
	if !strings.Contains(got["promoNoLabel"], "0x") || !strings.Contains(got["promoNoLabel"], "免费") {
		t.Errorf("无标签的 0x 促销应补免费标签，got %q", got["promoNoLabel"])
	}
	// 带前缀的完整 id（后端路由认的形式）。
	if got["pidGlobal"] != "global:deepseek-v4.1-flash" || got["pidCN"] != "cn:hy3" {
		t.Errorf("前缀构造错误: %q / %q", got["pidGlobal"], got["pidCN"])
	}
	// 已带前缀不重复加；空 id 不加；未知域不加。
	if got["pidAlready"] != "cn:hy3" {
		t.Errorf("已带前缀的 id 不应重复加前缀，got %q", got["pidAlready"])
	}
	if got["pidEmpty"] != "" || got["pidUnknownRealm"] != "hy3" {
		t.Errorf("空 id / 未知域处理错误: %q / %q", got["pidEmpty"], got["pidUnknownRealm"])
	}
}

// rpIDs id 列表（失败信息可读；顺序不保证）。
func rpIDs(m map[string]rpEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
