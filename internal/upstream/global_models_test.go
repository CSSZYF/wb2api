package upstream

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// globalAuth 构造一个判为 global realm 的测试账号（domain 后缀推断，走 Realm() 正常路径）。
func globalAuth() *auth.Auth {
	return &auth.Auth{UID: "g1", AccessToken: "tok", Domain: "www.workbuddy.ai"}
}

// globalTestClient 起一个 fake transport 的 Client，global base 指向假上游。
// behavior 按 path 返回 (status, body)。
func globalTestClient(t *testing.T, behavior func(path string) (int, string)) (*Client, *int) {
	t.Helper()
	calls := new(int)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			*calls++
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
	// credits 必须被丢弃：探测端点返回了 x1.00 也不进 global 路径。
	if m.Credits != "" {
		t.Errorf("gpt-5.4 Credits=%q want 空（倍率不进 global 路径）", m.Credits)
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
	first := *calls
	if first == 0 {
		t.Fatal("首次探测应打上游")
	}
	// 负缓存：第二次零上游调用。
	_ = c.FetchGlobalModelInfos(globalAuth())
	if *calls != first {
		t.Errorf("负缓存期内不应再打上游：calls %d → %d", first, *calls)
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
	if *calls != 0 {
		t.Errorf("逃生门关闭时上游调用=%d want 0", *calls)
	}
}

// TestFetchGlobalModelInfosV2PreferredV2 优先：/v2 命中即止（不碰 /console）。
func TestFetchGlobalModelInfosV2Preferred(t *testing.T) {
	var paths []string
	c, _ := globalTestClient(t, func(path string) (int, string) {
		paths = append(paths, path)
		if path == "/v2/enterprises/personal/models" {
			return 200, `{"code":0,"data":{"models":[{"id":"gpt-5.4","maxInputTokens":262144}]}}`
		}
		return 500, "<html>500</html>"
	})
	_ = c.FetchGlobalModelInfos(globalAuth())
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
	first := *calls
	_ = c.FetchGlobalModelInfos(a)
	if *calls != first {
		t.Errorf("TTL 内不应重复探测：calls %d → %d", first, *calls)
	}
	// 把成功时间戳拨到 2h 前 → 重新探测。
	c.globalModels.Lock()
	c.globalModels.fetched = time.Now().Add(-2 * time.Hour)
	c.globalModels.Unlock()
	_ = c.FetchGlobalModelInfos(a)
	if *calls <= first {
		t.Errorf("TTL 过期后应重新探测：calls %d → %d", first, *calls)
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
