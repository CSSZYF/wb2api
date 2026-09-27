package upstream

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// globalAuth 构造一个判为 global realm 的测试账号（domain 后缀推断，走 Realm() 正常路径）。
func globalAuth() *auth.Auth {
	return &auth.Auth{UID: "g1", AccessToken: "tok", Domain: "www.workbuddy.ai"}
}

// callCounter 并发安全的出站计数。global 目录探测自 9dce68a 起**并发两路**
// /v3/config（IDE UA + CLI UA 取并集），测试 fake transport 里的裸 `*int++`
// 在 -race 下构成数据竞争（与上游 73fe1f8 顺手修的 handler_test 裸 calls++ 同形态）。
type callCounter struct{ n atomic.Int64 }

// Load 返回当前计数（并发安全，替代裸 `calls.Load()`）。
func (c *callCounter) Load() int { return int(c.n.Load()) }

// pathRecorder 并发安全的请求路径记录（同因：两路并发探测下裸 append 不安全）。
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) add(p string) {
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.mu.Unlock()
}

// all 返回路径快照（拷贝，调用方可安全遍历）。
func (r *pathRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// globalTestClient 起一个 fake transport 的 Client，global base 指向假上游。
// behavior 按 path 返回 (status, body)。
func globalTestClient(t *testing.T, behavior func(path string) (int, string)) (*Client, *callCounter) {
	t.Helper()
	calls := new(callCounter)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			calls.n.Add(1)
			status, body := behavior(r.URL.Path)
			return jsonResp(status, body), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"
	return c, calls
}

// TestFetchGlobalModelInfosParsesMetadata 探测产出带窗口 / 能力元数据的条目：
// maxInputTokens→ContextWindow、maxOutputTokens→MaxTokens、maxAllowedSize、
// reasoning.*、supportsImages 全部解析；credits 恒丢弃（PLAN §3.D2）。
func TestFetchGlobalModelInfosParsesMetadata(t *testing.T) {
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":{"models":[
			{"id":"gpt-5.4","name":"GPT 5.4","maxInputTokens":262144,"maxOutputTokens":65536,
			 "maxAllowedSize":262144,"credits":"x1.00","supportsReasoning":true,"supportsImages":true,
			 "reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high"]}},
			{"id":"old-key-model","maxInputTokens":128000,"reasoning":{"effort":"medium"}},
			{"name":"name-fallback","maxInputTokens":64000},
			{"id":"disabled-model","maxInputTokens":100000,"disabled":true},
			{"id":"zero-meta"}
		]}}`
	})

	infos := c.FetchGlobalModelInfos(globalAuth())
	byID := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		byID[mi.ID] = mi
	}

	m, ok := byID["gpt-5.4"]
	if !ok {
		t.Fatalf("缺 gpt-5.4，实际 ids=%v", idsOf(infos))
	}
	if m.ContextWindow != 262144 || m.MaxTokens != 65536 || m.MaxAllowedSize != 262144 {
		t.Errorf("gpt-5.4 窗口/输出/体量=%d/%d/%d want 262144/65536/262144",
			m.ContextWindow, m.MaxTokens, m.MaxAllowedSize)
	}
	if !m.SupportsReasoning || !m.CanDisableThinking || !m.SupportsImages {
		t.Errorf("gpt-5.4 能力旗标 reasoning=%v canDisable=%v images=%v want all true",
			m.SupportsReasoning, m.CanDisableThinking, m.SupportsImages)
	}
	if m.DefaultEffort != "high" {
		t.Errorf("gpt-5.4 DefaultEffort=%q want high", m.DefaultEffort)
	}
	if len(m.Efforts) != 2 || m.Efforts[0] != "low" {
		t.Errorf("gpt-5.4 Efforts=%v want [low high]", m.Efforts)
	}
	// credits 不进**路由/列表口径**（PLAN §3.D2）：FetchGlobalModelInfos 的返回值里
	// 必须为空——它服务 /v1/models 与选号。探测端点返回的 x1.00 只落缓存旁表，
	// 由 GlobalModelInfosSnapshot（展示出口）填回，见 TestGlobalModelCreditsSnapshot。
	if m.Credits != "" {
		t.Errorf("gpt-5.4 Credits=%q want 空（倍率不进 Fetch/路由口径）", m.Credits)
	}
	// 老键 reasoning.effort 兼容。
	if got := byID["old-key-model"].DefaultEffort; got != "medium" {
		t.Errorf("old-key-model DefaultEffort=%q want medium（reasoning.effort 老键）", got)
	}
	// id 缺省时回退 name。
	if _, ok := byID["name-fallback"]; !ok {
		t.Errorf("id 缺省应回退 name，实际 ids=%v", idsOf(infos))
	}
	// disabled 剔除。
	if _, has := byID["disabled-model"]; has {
		t.Error("disabled 模型不应出现在探测结果里")
	}
	// 零元数据条目仍在（窗口由调用方兜底）。
	if z, ok := byID["zero-meta"]; !ok || z.ContextWindow != 0 {
		t.Errorf("zero-meta 条目=%+v want 存在且窗口为零值", z)
	}
}

// TestFetchGlobalModelInfosMergesStaticExtras 探测成功 → 以探测为基底（保上游序），
// 静态名单里探测未返回的 id 按 id 补齐在尾部（元数据留空）。
func TestFetchGlobalModelInfosMergesStaticExtras(t *testing.T) {
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		// 只返回两个模型；静态名单里的 deepseek-v4.1-flash 等应由静态独有补齐。
		return 200, `{"code":0,"data":{"models":[
			{"id":"gpt-5.4","maxInputTokens":262144},
			{"id":"probe-only","maxInputTokens":1000}
		]}}`
	})

	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) < len(GlobalModelNames) {
		t.Fatalf("合并后条目数=%d want >=%d（静态独有需补齐）", len(infos), len(GlobalModelNames))
	}
	// 探测基底在前，且保上游返回序。
	if infos[0].ID != "gpt-5.4" || infos[1].ID != "probe-only" {
		t.Errorf("探测基底序不对：前两条=%s,%s want gpt-5.4,probe-only", infos[0].ID, infos[1].ID)
	}
	if infos[0].ContextWindow != 262144 {
		t.Errorf("探测条目元数据丢失：ContextWindow=%d want 262144", infos[0].ContextWindow)
	}
	byID := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	// 静态独有（上游未返回）：id 在、元数据留空（由知识表补窗口）。
	if ds, ok := byID["deepseek-v4.1-flash"]; !ok {
		t.Errorf("静态独有 deepseek-v4.1-flash 未补齐，实际 ids=%v", idsOf(infos))
	} else if ds.ContextWindow != 0 {
		t.Errorf("静态独有条目元数据应为零值（不编造），got %d", ds.ContextWindow)
	}
	// 去重：无重复 id。
	seen := map[string]bool{}
	for _, mi := range infos {
		if seen[mi.ID] {
			t.Errorf("id %s 重复", mi.ID)
		}
		seen[mi.ID] = true
	}
}

// TestFetchGlobalModelInfosFallsBackToStaticOnFailure 探测失败（家族端点全非 2xx）
// → 回落静态名单（仅 ID、零元数据）+ 5min 负缓存（冷却期内零上游调用）。
func TestFetchGlobalModelInfosFallsBackToStaticOnFailure(t *testing.T) {
	c, calls := globalTestClient(t, func(string) (int, string) {
		return 500, "<html>500 Internal Server Error</html>"
	})

	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) != len(GlobalModelNames) {
		t.Fatalf("失败回落条目数=%d want %d（静态名单）", len(infos), len(GlobalModelNames))
	}
	for _, mi := range infos {
		if mi.ContextWindow != 0 || mi.MaxTokens != 0 {
			t.Errorf("失败回落不应有元数据：%+v", mi)
		}
	}
	first := calls.Load()
	if first == 0 {
		t.Fatal("首次探测应打上游")
	}
	// 负缓存：第二次零上游调用。
	_ = c.FetchGlobalModelInfos(globalAuth())
	if calls.Load() != first {
		t.Errorf("负缓存期内不应再打上游：calls %d → %d", first, calls.Load())
	}
}

// TestFetchGlobalModelInfosNarrowTableIsIDOnly 窄表形态（data 为字符串数组）
// → 仅 ID、元数据留空（窗口由调用方经知识表兜底）。
func TestFetchGlobalModelInfosNarrowTableIsIDOnly(t *testing.T) {
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":["glm-5.2","probe-only"]}`
	})
	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) < 2 {
		t.Fatalf("窄表条目数=%d want >=2", len(infos))
	}
	if infos[0].ID != "glm-5.2" || infos[1].ID != "probe-only" {
		t.Errorf("窄表序=%s,%s want glm-5.2,probe-only", infos[0].ID, infos[1].ID)
	}
	for _, mi := range infos {
		if mi.ContextWindow != 0 || mi.MaxTokens != 0 {
			t.Errorf("窄表不应有元数据：%+v", mi)
		}
	}
}

// TestFetchGlobalModelsCompatWrapper 兼容薄封装返回模型名列表，与 Infos 同源同序。
func TestFetchGlobalModelsCompatWrapper(t *testing.T) {
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":{"models":[{"id":"gpt-5.4"},{"id":"probe-only"}]}}`
	})
	names := c.FetchGlobalModels(globalAuth())
	if len(names) < 2 || names[0] != "gpt-5.4" || names[1] != "probe-only" {
		t.Fatalf("FetchGlobalModels=%v want 以 gpt-5.4,probe-only 开头", names)
	}
	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) != len(names) {
		t.Errorf("薄封装与 Infos 条目数不一致：%d vs %d", len(names), len(infos))
	}
	for i, mi := range infos {
		if mi.ID != names[i] {
			t.Errorf("第 %d 项 id 不一致：%s vs %s", i, mi.ID, names[i])
		}
	}
}

// TestFetchGlobalModelInfosRespectsGlobalGate GlobalEnabled=false（逃生门）时
// 不探测、直接静态名单（零上游调用）。
func TestFetchGlobalModelInfosRespectsGlobalGate(t *testing.T) {
	c, calls := globalTestClient(t, func(string) (int, string) {
		t.Error("逃生门关闭时不应打上游")
		return 500, ""
	})
	c.GlobalEnabled = false

	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) != len(GlobalModelNames) {
		t.Fatalf("逃生门兜底条目数=%d want %d", len(infos), len(GlobalModelNames))
	}
	if calls.Load() != 0 {
		t.Errorf("逃生门关闭时上游调用=%d want 0", calls.Load())
	}
}

// TestFetchGlobalModelInfosV2PreferredV2 优先：/v2 命中即止（不碰 /console）。
func TestFetchGlobalModelInfosV2Preferred(t *testing.T) {
	var rec pathRecorder
	c, _ := globalTestClient(t, func(path string) (int, string) {
		rec.add(path)
		if path == "/v2/enterprises/personal/models" {
			return 200, `{"code":0,"data":{"models":[{"id":"gpt-5.4","maxInputTokens":262144}]}}`
		}
		return 500, "<html>500</html>"
	})
	_ = c.FetchGlobalModelInfos(globalAuth())
	paths := rec.all()
	if len(paths) == 0 || paths[0] != "/v2/enterprises/personal/models" {
		t.Errorf("首个探测路径=%v want /v2 家族优先", paths)
	}
	for _, p := range paths {
		if strings.Contains(p, "/console/") {
			t.Errorf("v2 命中后不应再打 /console：%v", paths)
		}
	}
}

// TestFetchGlobalModelInfosTTLCache 成功结果 1h 缓存：TTL 内零上游调用。
func TestFetchGlobalModelInfosTTLCache(t *testing.T) {
	c, calls := globalTestClient(t, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[{"id":"gpt-5.4","maxInputTokens":262144}]}}`
	})
	a := globalAuth()
	_ = c.FetchGlobalModelInfos(a)
	first := calls.Load()
	_ = c.FetchGlobalModelInfos(a)
	if calls.Load() != first {
		t.Errorf("TTL 内不应重复探测：calls %d → %d", first, calls.Load())
	}
	// 把成功时间戳拨到 2h 前 → 重新探测。
	c.globalModels.Lock()
	c.globalModels.fetched = time.Now().Add(-2 * time.Hour)
	c.globalModels.Unlock()
	_ = c.FetchGlobalModelInfos(a)
	if calls.Load() <= first {
		t.Errorf("TTL 过期后应重新探测：calls %d → %d", first, calls.Load())
	}
}

// TestParseGlobalModelInfosRejectsBadEnvelope 解析失败口径：非 JSON / code≠0 / 空名单
// 都返回错误（调用方回落静态，等价"该端点没给全"）。
func TestParseGlobalModelInfosRejectsBadEnvelope(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"not-json", `<html>500</html>`},
		{"bad-code", `{"code":500,"data":{"models":[{"id":"x"}]}}`},
		{"empty-models", `{"code":0,"data":{"models":[]}}`},
		{"empty-narrow", `{"code":0,"data":[]}`},
		{"all-disabled", `{"code":0,"data":{"models":[{"id":"x","disabled":true}]}}`},
	}
	for _, tc := range cases {
		if _, err := parseGlobalModelInfos([]byte(tc.raw)); err == nil {
			t.Errorf("%s: 应返回错误（调用方据此回落静态）", tc.name)
		}
	}
}

// idsOf 提取条目 id 列表（测试诊断用）。
func idsOf(infos []ModelInfo) []string {
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		out = append(out, mi.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// GlobalModelInfosSnapshot 只读快照 + 倍率旁表（对齐 sliver 1dfe750）
// ---------------------------------------------------------------------------

// TestGlobalModelCreditsSnapshot 只读快照的三个契约（issue #176）：
//   - (a) 冷客户端（从未探测）→ nil，且**零上游请求**（绝不主动探测）；
//   - (b) 一次 FetchGlobalModelInfos 预热 → 快照返回同一批条目**且带倍率原文**，
//     而 Fetch 的返回值里倍率恒为空（D2：倍率不进路由/列表口径）；
//   - (c) 预热后反复读快照 → 上游请求计数不变（只读）。
func TestGlobalModelCreditsSnapshot(t *testing.T) {
	c, calls := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":{"models":[
			{"id":"hy3","name":"Hy3","maxInputTokens":192000,"credits":"x0.05"},
			{"id":"no-credits","name":"No Credits","maxInputTokens":128000},
			{"id":"disabled-model","credits":"x9.99","disabled":true}
		]}}`
	})

	// (a) 冷：快照 nil，零上游请求。
	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Fatalf("冷快照 = %+v, want nil", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("冷快照发起 %d 次上游请求，want 0（绝不探测）", calls.Load())
	}

	// (b) 预热：Fetch 返回值**无倍率**（D2），快照**有倍率**（展示口径）。
	infos := c.FetchGlobalModelInfos(globalAuth())
	fetchByID := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		fetchByID[mi.ID] = mi
	}
	if got := fetchByID["hy3"].Credits; got != "" {
		t.Errorf("FetchGlobalModelInfos 的 hy3 Credits=%q want 空（倍率不进路由/列表口径）", got)
	}

	snap := c.GlobalModelInfosSnapshot()
	snapByID := make(map[string]ModelInfo, len(snap))
	for _, mi := range snap {
		snapByID[mi.ID] = mi
	}
	if got := snapByID["hy3"].Credits; got != "x0.05" {
		t.Errorf("快照的 hy3 Credits=%q want x0.05（展示口径透出倍率原文）", got)
	}
	// 元数据必须与 Fetch 一致（快照只是倍率增强，不改其他字段）。
	if snapByID["hy3"].ContextWindow != fetchByID["hy3"].ContextWindow ||
		snapByID["hy3"].MaxTokens != fetchByID["hy3"].MaxTokens {
		t.Errorf("快照元数据与 Fetch 不一致：snap=%+v fetch=%+v", snapByID["hy3"], fetchByID["hy3"])
	}
	// 未下发倍率的条目：快照里也是空串（**缺失 ≠ 免费**，调用方据此整体省略）。
	if got := snapByID["no-credits"].Credits; got != "" {
		t.Errorf("未下发倍率的条目 Credits=%q want 空串（缺失≠免费，不得编造 x0.00）", got)
	}
	// disabled 条目两侧都不出现。
	if _, ok := snapByID["disabled-model"]; ok {
		t.Error("disabled 条目不应进快照")
	}
	// 静态独有条目（上游未返回）无倍率——不得从别处借一个值过来。
	if mi, ok := snapByID["deepseek-v4.1-flash"]; ok && mi.Credits != "" {
		t.Errorf("静态独有条目 Credits=%q want 空（上游没给，不编造）", mi.Credits)
	}
	warm := calls.Load()

	// (c) 只读：反复读快照零新增请求。
	for i := 0; i < 5; i++ {
		if got := c.GlobalModelInfosSnapshot(); len(got) == 0 || got[0].Credits == "" {
			t.Fatalf("快照读取 #%d = %+v, want 缓存条目（含倍率）", i, got)
		}
	}
	if calls.Load() != warm {
		t.Errorf("快照读取新增 %d 次上游请求，want 0（只读）", calls.Load()-warm)
	}
}

// TestGlobalModelInfosSnapshotTTL TTL 过期视同冷：快照返回 nil，且**不因读取而刷新**
// （与 FetchGlobalModelInfos 的 miss-即-探测相反）。
func TestGlobalModelInfosSnapshotTTL(t *testing.T) {
	c, calls := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":{"models":[{"id":"hy3","credits":"x0.05"}]}}`
	})
	c.FetchGlobalModelInfos(globalAuth())
	if got := snapshotCreditOf(c.GlobalModelInfosSnapshot(), "hy3"); got != "x0.05" {
		t.Fatalf("预热后快照 hy3 Credits=%q, want x0.05", got)
	}
	warm := calls.Load()

	// 把落缓存时间拨到 TTL 之外。
	c.globalModels.Lock()
	c.globalModels.fetched = time.Now().Add(-2 * globalModelsTTL)
	c.globalModels.Unlock()

	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Errorf("TTL 过期快照 = %+v, want nil", got)
	}
	if calls.Load() != warm {
		t.Errorf("过期快照读取新增 %d 次上游请求，want 0（不因读取而刷新）", calls.Load()-warm)
	}
}

// TestGlobalModelCreditsSnapshotClearedOnProbeFailure 探测失败时倍率旁表必须一并清空：
// 留下上一轮倍率会让 /v1/stats 展示与当前目录脱节的过期数字。
func TestGlobalModelCreditsSnapshotClearedOnProbeFailure(t *testing.T) {
	fail := false
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if fail {
			return 500, "<html>500</html>"
		}
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":{"models":[{"id":"hy3","credits":"x0.05"}]}}`
	})
	c.FetchGlobalModelInfos(globalAuth())
	if got := snapshotCreditOf(c.GlobalModelInfosSnapshot(), "hy3"); got != "x0.05" {
		t.Fatalf("预热后快照 hy3 Credits=%q, want x0.05", got)
	}

	// 让缓存过期 + 探测失败（负缓存路径），快照必须为 nil（旁表已清）。
	fail = true
	c.globalModels.Lock()
	c.globalModels.fetched = time.Now().Add(-2 * globalModelsTTL)
	c.globalModels.Unlock()
	c.FetchGlobalModelInfos(globalAuth())

	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Errorf("探测失败后快照 = %+v, want nil（倍率旁表须随 models 一起清空）", got)
	}
}

// TestGlobalModelInfosSnapshotDoesNotMutateCache 快照填回的倍率不得污染缓存本体：
// 缓存里的 models 恒无倍率，否则第二次 Fetch 就会把倍率带进路由/列表口径。
func TestGlobalModelInfosSnapshotDoesNotMutateCache(t *testing.T) {
	c, _ := globalTestClient(t, func(path string) (int, string) {
		if path != "/v2/enterprises/personal/models" {
			return 500, "<html>500</html>"
		}
		return 200, `{"code":0,"data":{"models":[{"id":"hy3","credits":"x0.05"}]}}`
	})
	c.FetchGlobalModelInfos(globalAuth())
	_ = c.GlobalModelInfosSnapshot() // 读一次快照（若实现是原地填字段，这里就污染了缓存）

	// 再读缓存本体（Fetch 命中缓存路径）与快照，倍率必须仍然只在快照侧。
	again := c.FetchGlobalModelInfos(globalAuth())
	if got := snapshotCreditOf(again, "hy3"); got != "" {
		t.Errorf("Fetch 第二次 hy3 Credits=%q want 空（快照不得污染缓存本体）", got)
	}
	if got := snapshotCreditOf(c.GlobalModelInfosSnapshot(), "hy3"); got != "x0.05" {
		t.Errorf("快照 hy3 Credits=%q want x0.05", got)
	}
}

// snapshotCreditOf 取条目列表里指定 id 的倍率原文（诊断用）。
func snapshotCreditOf(infos []ModelInfo, id string) string {
	for _, mi := range infos {
		if mi.ID == id {
			return mi.Credits
		}
	}
	return ""
}
