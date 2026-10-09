package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// app_expiry_card_test.go 首页「积分到期提醒」卡片 + 逐包明细排序选择器的接线与逻辑
// 验收（吸收上游 PR #93 的 1d 前端项）。
//
// 我们已有：到期列三态（pkgExpiryState，窗口取 overview.expiring_soon_sec）。
// PR 新增（本文件验收）：
//   (a) 首页「积分到期提醒」卡片：按到期日聚合 + 算「日均需耗」+ 三档危险色；
//   (b) pkSortMode 排序选择器（到期升序 / 面额降序，localStorage 持久化）。
//
// 我们的缺口证据：grep renderExpiry/expBox/expList/pkSortMode 零命中。

// TestAppJSExpiryCardWiring 接线完整性：卡片 DOM（index.html）+ 渲染/加载函数与
// 路由接线（app.js）。app.js/index.html 是 go:embed 静态资源，Go 编译器不校验其
// 内容——少一处接线，面板上就是一个「永远空白/点了没反应」的卡片。
func TestAppJSExpiryCardWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		"function expBatches(",   // 按到期日聚合
		"function expDaysLeft(",  // 剩余天数
		"function renderExpiry(", // 卡片渲染
		"function loadExpiry(",   // 数据加载（与「积分构成」共用缓存）
		"$('btnExp')",            // 「检查」按钮接线
		"$('expList')",           // 列表容器
		"expiring_soon_sec",      // 窗口/口径与既有三态同源（不另立一套）
		"日均需耗",                   // 核心可操作数字
		"loadExpiry(true)",       // 路由（accounts 视图）触发
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺到期提醒卡片接线：%s", must)
		}
	}
	// 缓存共享：卡片与「积分构成」共用同一份 /panel/api/packages 数据（逐账号实时查
	// 上游，能省一次是一次）——没有缓存就会在切视图时反复打上游。
	for _, must := range []string{"lastPackages", "lastPackagesAt"} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺 packages 共享缓存：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, must := range []string{
		`id="expBox"`, `id="expList"`, `id="expNote"`, `id="btnExp"`,
		`积分到期提醒`,
		`.exp-row`, `.exp-dot`, `.exp-nm`, `.exp-main`,
	} {
		if !strings.Contains(h, must) {
			t.Errorf("index.html 缺到期提醒卡片接线：%s", must)
		}
	}
}

// TestAppJSModelLockBoxHidesWhenEmpty 模型锁池为空时整盒隐藏，而不是留下一个
// 「没有数据」的空盒子占位。隐藏/显示都必须在 renderModelLocks 内接线，DOM 上
// 初始 hidden，避免首屏闪一下空盒。
func TestAppJSModelLockBoxHidesWhenEmpty(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="mlBox" hidden`) {
		t.Error("index.html 的模型锁池容器必须是 id=mlBox 且初始 hidden（空态整盒隐藏）")
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := jsFuncBody(string(js), "function renderModelLocks(")
	if body == "" {
		t.Fatal("app.js 缺 renderModelLocks")
	}
	for _, must := range []string{"$('mlBox')", "box.hidden = true", "box.hidden = false"} {
		if !strings.Contains(body, must) {
			t.Errorf("renderModelLocks 未按行数切换 mlBox.hidden：缺 %s", must)
		}
	}
}

// TestExpiryCardExpandedUsesPageFlow 展开态不能再把长列表塞进小滚动盒：
// expBody/expList 不得有 max-height/overflow 规则，renderExpiry 必须渲染所有账号行
// （列表本身不截断），由页面自然滚动而不是盒内二次滚动。
func TestExpiryCardExpandedUsesPageFlow(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, sel := range []string{`.exp-body`, `#expBody`, `#expList`} {
		re := regexp.MustCompile(regexp.QuoteMeta(sel) + `\s*\{([^}]*)\}`)
		for _, m := range re.FindAllStringSubmatch(h, -1) {
			if strings.Contains(m[1], "max-height") || strings.Contains(m[1], "overflow") {
				t.Errorf("%s 不得有内部滚动/限高规则（展开后应完整显示）：{%s}", sel, m[1])
			}
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := jsFuncBody(string(js), "function renderExpiry(")
	if body == "" {
		t.Fatal("app.js 缺 renderExpiry")
	}
	if !strings.Contains(body, "const rows = list.map") || !strings.Contains(body, "innerHTML = rows") {
		t.Error("renderExpiry 必须渲染完整账号列表（不得在盒内截断）")
	}
	if !strings.Contains(string(js), "$('btnExpToggle')") || !strings.Contains(h, `id="expBody" hidden`) {
		t.Error("到期提醒展开/收起接线缺失")
	}
}

// TestAppJSPkSortWiring 排序选择器接线：选择器 DOM + localStorage 持久化 +
// 切换后重排（数据在内存，不重打上游）。
func TestAppJSPkSortWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		"function pkDetailCompare(",
		"pkSortMode",
		"localStorage.setItem(LS_PK_SORT", // 持久化（跨会话记住）
		"localStorage.getItem(LS_PK_SORT",
		"'end_asc'",   // 到期升序（默认）
		"'size_desc'", // 面额降序
		"$('pkSort')",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺逐包排序接线：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, must := range []string{`id="pkSort"`, `value="end_asc"`, `value="size_desc"`, `select.xs`} {
		if !strings.Contains(h, must) {
			t.Errorf("index.html 缺逐包排序接线：%s", must)
		}
	}
	// 明细行序必须真的走比较器（否则选择器只是装饰）。
	body := jsFuncBody(s, "function renderPackages(")
	if body == "" {
		t.Fatal("app.js 缺 renderPackages")
	}
	if !strings.Contains(body, "pkDetailCompare") {
		t.Error("renderPackages 未接 pkDetailCompare（切换排序不生效）")
	}
}

// TestAppJSExpBatchesLogic 纯逻辑（node 实跑）：按到期日聚合只统计 remain>0 且
// 有到期时间的包，日期升序；缺失/零余额包不参与（没余额/长期包到期没有影响）。
func TestAppJSExpBatchesLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	batches := jsFuncFull(src, "function expBatches(")
	days := jsFuncFull(src, "function expDaysLeft(")
	if batches == "" || days == "" {
		t.Fatal("app.js 缺 expBatches/expDaysLeft")
	}
	script := batches + "\n" + days + "\n" + `
const packs = [
  { remain: 100, end_time: '2026-10-18 05:24:02' },
  { remain: 50,  end_time: '2026-10-18 05:24:02' },   // 同日聚合 → 150
  { remain: 30,  end_time: '2026-10-01 00:00:00' },   // 更早 → 排最前
  { remain: 0,   end_time: '2026-09-01 00:00:00' },   // 零余额：不参与
  { remain: 77 },                                     // 无到期：不参与
  { remain: -5,  end_time: '2026-09-02 00:00:00' },   // 负余额：不参与
];
const bs = expBatches(packs);
const today = new Date('2026-10-03T00:00:00+08:00');
console.log(JSON.stringify({
  batches: bs,
  d0: expDaysLeft(bs[0].date, today),   // 2026-10-01 → -2（已过期）
  d1: expDaysLeft(bs[1].date, today),   // 2026-10-18 → 15
  empty: expBatches([]),
  nil: expBatches(null),
}));
`
	fp := filepath.Join(t.TempDir(), "exp_batches_check.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, out)
	}
	type batch struct {
		Date   string `json:"date"`
		Remain int64  `json:"remain"`
	}
	var got struct {
		Batches []batch `json:"batches"`
		D0      int     `json:"d0"`
		D1      int     `json:"d1"`
		Empty   []batch `json:"empty"`
		Nil     []batch `json:"nil"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, out)
	}
	if len(got.Batches) != 2 {
		t.Fatalf("聚合结果 = %+v want 2 批（零余额/无到期/负余额不参与）", got.Batches)
	}
	if got.Batches[0].Date != "2026-10-01" || got.Batches[0].Remain != 30 {
		t.Errorf("首批 = %+v want {2026-10-01 30}（日期升序）", got.Batches[0])
	}
	if got.Batches[1].Date != "2026-10-18" || got.Batches[1].Remain != 150 {
		t.Errorf("次批 = %+v want {2026-10-18 150}（同日聚合）", got.Batches[1])
	}
	if got.D0 != -2 || got.D1 != 15 {
		t.Errorf("剩余天数 = %d/%d want -2/15", got.D0, got.D1)
	}
	if len(got.Empty) != 0 || len(got.Nil) != 0 {
		t.Errorf("空输入应返回空数组：empty=%v nil=%v", got.Empty, got.Nil)
	}
}

// TestAppJSPkDetailCompareLogic 纯逻辑（node 实跑）：两种排序模式的键与兜底。
//   - end_asc（默认）：到期升序；无到期时间的包垫底（没有可比的日期，不掺进日期序）；
//     同一到期时间按面额降序；
//   - size_desc：面额降序；同面额按到期升序。
func TestAppJSPkDetailCompareLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	cmp := jsFuncFull(src, "function pkDetailCompare(")
	endFn := jsFuncFull(src, "function pkEndMs(")
	base := jsFuncFull(src, "function pkgEndMs(")
	if cmp == "" || endFn == "" || base == "" {
		t.Fatal("app.js 缺 pkDetailCompare/pkEndMs/pkgEndMs")
	}
	script := `let pkSortMode = 'end_asc';
` + base + "\n" + endFn + "\n" + cmp + `
const packs = [
  { name: 'late',   size: 10, end_time: '2027-03-12 22:03:50' },
  { name: 'soon',   size: 5,  end_time: '2026-10-18 05:24:02' },
  { name: 'big',    size: 100, end_time: '2027-03-12 22:03:50' },
  { name: 'noend',  size: 50 },
];
const order = (m) => { pkSortMode = m; return packs.slice().sort(pkDetailCompare).map(p => p.name); };
console.log(JSON.stringify({ endAsc: order('end_asc'), sizeDesc: order('size_desc') }));
`
	fp := filepath.Join(t.TempDir(), "pk_sort_check.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, out)
	}
	var got struct {
		EndAsc   []string `json:"endAsc"`
		SizeDesc []string `json:"sizeDesc"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, out)
	}
	wantEnd := []string{"soon", "big", "late", "noend"}
	if strings.Join(got.EndAsc, ",") != strings.Join(wantEnd, ",") {
		t.Errorf("end_asc 序 = %v want %v（到期升序、同到期按面额降序、无到期垫底）", got.EndAsc, wantEnd)
	}
	wantSize := []string{"big", "noend", "late", "soon"}
	if strings.Join(got.SizeDesc, ",") != strings.Join(wantSize, ",") {
		t.Errorf("size_desc 序 = %v want %v（面额降序、同面额按到期升序）", got.SizeDesc, wantSize)
	}
}

// TestAppJSRenderExpiryLogic 纯逻辑（node 实跑）：危险度三档色（≤3 红 / ≤7 琥珀 /
// 更远绿）+ 日均需耗 = 批次剩余 ÷ 距到期天数（至少 1 天，今天到期不除零）+ 7 天
// 内合计。用最小 DOM 桩接住 renderExpiry 的写入。
func TestAppJSRenderExpiryLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	var parts []string
	for _, sig := range []string{
		"function esc(", "function fmtTok(", "function fmtSpend(", "function pkgEndMs(",
		"function expBatches(", "function expDaysLeft(", "function paintDailyStats(",
		"function renderExpiry(",
	} {
		fn := jsFuncFull(src, sig)
		if fn == "" {
			t.Fatalf("app.js 缺 %s", sig)
		}
		parts = append(parts, fn)
	}
	script := `let lastPackages = null; let lastPackagesAt = 0;
let expDailyNeed = null; let overviewData = null;
const writes = {};
// makeEl 的最小 DOM 桩：paintDailyStats 需要 el.parentElement.title/classList。
function makeEl(id) {
  const el = { id, textContent: '', hidden: true, children: [],
    set innerHTML(v) { writes[id] = String(v); }, get innerHTML() { return writes[id] || ''; } };
  const pel = { title: '', _cls: [] };
  pel.classList = {
    toggle: (c, on) => { const i = pel._cls.indexOf(c); if (on && i < 0) pel._cls.push(c); if (!on && i >= 0) pel._cls.splice(i, 1); },
    remove: (...cs) => { for (const c of cs) { const i = pel._cls.indexOf(c); if (i >= 0) pel._cls.splice(i, 1); } },
  };
  el.parentElement = pel;
  return el;
}
const els = {};
const $ = id => (els[id] = els[id] || makeEl(id));
` + strings.Join(parts, "\n") + `
const today = new Date(); today.setHours(0, 0, 0, 0);
const iso = (days) => { const d = new Date(today.getTime() + days * 86400000);
  return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); };
renderExpiry({ accounts: [
  { uid: 'u1', nickname: 'soon',  packages: [{ remain: 90, end_time: iso(3) + ' 00:00:00' }] },
  { uid: 'u2', nickname: 'today', packages: [{ remain: 40, end_time: iso(0) + ' 00:00:00' }] },
  { uid: 'u6', nickname: 'week',  packages: [{ remain: 70, end_time: iso(5) + ' 00:00:00' }] },
  { uid: 'u3', nickname: 'far',   packages: [{ remain: 10, end_time: iso(30) + ' 00:00:00' }] },
  { uid: 'u4', nickname: 'none',  packages: [{ remain: 10 }] },
  { uid: 'u5', nickname: 'err',   error: 'boom' },
]});
// 概览条：需耗 = 30(u1)+40(u2)+14(u6)+1(u3)=85（u4 无到期、u5 失败不计）；
// 已耗在 overviewData 到位前是 —，到位后按 vs 需耗染绿/琥珀。
const before = { need: els.sDailyNeed.textContent, used: els.sTodayUsed.textContent };
overviewData = { today: { credit: 100, requests: 2, total_tokens: 5000 } };
paintDailyStats();
const after = { used: writes.sTodayUsed || els.sTodayUsed.textContent, cls: els.sTodayUsed.parentElement._cls.join(',') };
overviewData.today.credit = 10; paintDailyStats();
const under = els.sTodayUsed.parentElement._cls.join(',');
console.log(JSON.stringify({ list: writes.expList, note: writes.expNote, hidden: els.expBox.hidden, before, after, under, need: expDailyNeed }));
`
	fp := filepath.Join(t.TempDir(), "render_expiry_check.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, out)
	}
	var got struct {
		List   string `json:"list"`
		Note   string `json:"note"`
		Hidden bool   `json:"hidden"`
		Need   int    `json:"need"`
		Before struct {
			Need string `json:"need"`
			Used string `json:"used"`
		} `json:"before"`
		After struct {
			Used string `json:"used"`
			Cls  string `json:"cls"`
		} `json:"after"`
		Under string `json:"under"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, out)
	}
	if got.Hidden {
		t.Error("渲染后卡片必须可见（expBox.hidden = false）")
	}
	// 危险度三档色：≤3 天红、≤7 天琥珀、更远绿。
	if !strings.Contains(got.List, "var(--bad)") {
		t.Errorf("≤3 天批次应着红（--bad）：%s", got.List)
	}
	if !strings.Contains(got.List, "var(--warn)") {
		t.Errorf("≤7 天批次应着琥珀（--warn）：%s", got.List)
	}
	if !strings.Contains(got.List, "var(--ok)") {
		t.Errorf("更远批次/无到期应着绿（--ok）：%s", got.List)
	}
	// 日均需耗：90 ÷ 3 = 30（今天到期用 max(1,0)=1 防除零）。
	if !strings.Contains(got.List, "日均需耗") {
		t.Errorf("应给出日均需耗：%s", got.List)
	}
	// 无到期/零余额的账号显示「无到期积分」而不是 0 天。
	if !strings.Contains(got.List, "无到期积分") {
		t.Errorf("无近期到期应显式说明（不得渲染成 0 天/空）：%s", got.List)
	}
	// 查询失败账号单独一行，不吞掉其余账号。
	if !strings.Contains(got.List, "查询失败") {
		t.Errorf("单账号查询失败应就地显示：%s", got.List)
	}
	// 概览条：需耗 = 各行日均需耗之和（30+40+14+1=85），失败/无到期账号不计。
	if got.Need != 85 || got.Before.Need != "85" {
		t.Errorf("日均需耗合计 = %v/%q want 85/\"85\"（各行之和，失败账号不贡献）", got.Need, got.Before.Need)
	}
	// 已耗：overview.today 未到位 → —；到位且 ≥ 需耗 → 绿（good）；低于 → 琥珀（warn）。
	if got.Before.Used != "—" {
		t.Errorf("today 缺席时已耗应显示 —, got %q", got.Before.Used)
	}
	if !strings.Contains(got.After.Used, "100") || !strings.Contains(got.After.Used, "tok") || !strings.Contains(got.After.Cls, "good") {
		t.Errorf("已耗 100 ≥ 需耗 85 应显示 100+tok 且染绿：used=%q cls=%q", got.After.Used, got.After.Cls)
	}
	if !strings.Contains(got.Under, "warn") {
		t.Errorf("已耗 10 < 需耗 85 应染琥珀：cls=%q", got.Under)
	}
}
