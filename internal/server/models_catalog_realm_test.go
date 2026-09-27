package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// models_catalog_realm_test.go CN 目录拉取的选号口径 + 缓存时长（吸收上游 79bc5af）。
//
// 上游 PR #38 的选号缺陷：fetchDynamicModels 用 Pool.Pick()（**无 realm 过滤**），
// 混合池里可能选中 global 账号去打 CN 端点（/console 家族）→ 偶发失败，面板与
// /v1/models 出两套目录。上游改为 AvailableUIDsForRealm("cn") 首个 + AuthByUID
// 与面板同口径。
//
// 本仓现状：fetchDynamicModels / fetchGlobalModelInfos 早已用
// PickExcludingForRealm(nil, "", realm)（realm 硬过滤，本域无候选直接 nil、不跨域）。
// 故**选号项无需改动**——本文件把该契约钉成回归测试（防未来有人改回无过滤的 Pick）。
//
// 本仓**吸收**的是缓存时长那一半：TTL 1h → 10min（目录新增模型时公开 API 最多
// 滞后一个 TTL；再短就不值得——每次失效都是上游探测）。5min 失败负缓存语义不变。

// TestFetchDynamicModelsRealmHardFilter 混合池中只有 global 账号健康时，
// CN 目录拉取必须**零上游调用**返回 nil——不得拿 global 账号去打 CN 端点。
// 这是上游 PR #38 报告的缺陷形态（本仓靠 realm 硬过滤天然免疫，测试锁死）。
func TestFetchDynamicModelsRealmHardFilter(t *testing.T) {
	resetDynamicModelsCache()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, false
	})
	// 仅 global 账号（无 CN 账号）。
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "atg", ExpiresAt: 9999999999, Domain: "x.workbuddy.ai"})
	h := NewHandler(Config{Pool: p, Upstream: up, StripRealmPrefix: true, GlobalEnabled: true})

	if infos := h.fetchDynamicModels(); len(infos) != 0 {
		t.Fatalf("无 CN 账号时不应拉到 CN 目录: %+v", infos)
	}
	if calls != 0 {
		t.Fatalf("无 CN 账号时不得打上游（realm 硬过滤）, calls=%d", calls)
	}
}

// TestFetchDynamicModelsUsesCNEvenWithGlobalHealthy 混合池（CN + global 都健康）：
// CN 目录拉取必须走 CN 账号，且**只**用 CN 账号。用请求头区分账号身份断言。
func TestFetchDynamicModelsUsesCNEvenWithGlobalHealthy(t *testing.T) {
	resetDynamicModelsCache()
	var authz []string
	up := newFakeUpstream(t, func(a string) (int, string, bool) {
		authz = append(authz, a)
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at-global", ExpiresAt: 9999999999, Domain: "x.workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, StripRealmPrefix: true, GlobalEnabled: true})

	infos := h.fetchDynamicModels()
	if len(infos) == 0 {
		t.Fatal("混合池应能拉到 CN 目录")
	}
	if len(authz) == 0 {
		t.Fatal("应发生上游探测")
	}
	for _, a := range authz {
		if a != "Bearer at-cn" {
			t.Fatalf("CN 目录探测只应使用 CN 账号，got %v", authz)
		}
	}
}

// TestDynamicModelsTTLIsTenMinutes 目录缓存 TTL = 10min（吸收上游 79bc5af）：
// 旧值 1h 让目录新增模型后公开 API 最多滞后一小时。缓存/负缓存语义本身保留
// （上游 #38 原案整体删缓存被拒：公开端点逐请求实时探测会放大上游压力）。
func TestDynamicModelsTTLIsTenMinutes(t *testing.T) {
	if dynamicModelsTTL != 10*time.Minute {
		t.Fatalf("dynamicModelsTTL=%v want 10m（目录漂移与上游压力的折中）", dynamicModelsTTL)
	}
	if modelsFetchFailCooldown != 5*time.Minute {
		t.Fatalf("modelsFetchFailCooldown=%v want 5m（失败负缓存语义不变）", modelsFetchFailCooldown)
	}
}

// TestFetchDynamicModelsCacheExpiresAfterTTL TTL 内命中缓存（零上游调用）、
// TTL 外重新探测：把 TTL 钉进行为而非只钉常量。
func TestFetchDynamicModelsCacheExpiresAfterTTL(t *testing.T) {
	resetDynamicModelsCache()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at-cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, StripRealmPrefix: true})

	if got := h.fetchDynamicModels(); len(got) == 0 {
		t.Fatal("首轮应拉到目录")
	}
	if calls == 0 {
		t.Fatal("首轮应有上游调用")
	}
	firstCalls := calls
	if got := h.fetchDynamicModels(); len(got) == 0 {
		t.Fatal("TTL 内应命中缓存")
	}
	if calls != firstCalls {
		t.Fatalf("TTL 内不得重新探测：calls %d → %d", firstCalls, calls)
	}
	// 把抓取时间拨到 TTL 之外 → 必须重新探测。
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Now().Add(-dynamicModelsTTL - time.Second)
	dynamicModelsCache.Unlock()
	if got := h.fetchDynamicModels(); len(got) == 0 {
		t.Fatal("TTL 外应重新探测")
	}
	if calls <= firstCalls {
		t.Fatalf("TTL 外应重新探测：calls 仍为 %d", calls)
	}
	// 缓存快照（gateway_hint 的目录判据）同 TTL 口径：TTL 内可读、过期即空。
	if snap := cachedModelsSnapshot(); len(snap) == 0 {
		t.Fatal("TTL 内缓存快照应可读（gateway_hint 的 cachedModelsSnapshot 数据源）")
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Now().Add(-dynamicModelsTTL - time.Second)
	dynamicModelsCache.Unlock()
	if snap := cachedModelsSnapshot(); len(snap) != 0 {
		t.Fatalf("过期缓存快照应为空（不触发探测）: %+v", snap)
	}
}

// TestFetchGlobalModelInfosRealmHardFilter global 目录拉取同样 realm 硬过滤：
// 只有 CN 账号时零上游调用（无 global 账号 → 静态名单兜底）。
func TestFetchGlobalModelInfosRealmHardFilter(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at-cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, StripRealmPrefix: true, GlobalEnabled: true})

	infos := h.fetchGlobalModelInfos()
	if len(infos) == 0 {
		t.Fatal("无 global 账号时应回落静态名单（非空）")
	}
	if calls != 0 {
		t.Fatalf("无 global 账号时不得打上游, calls=%d", calls)
	}
	// 静态名单兜底：仅 ID，元数据留空（窗口由 context_catalog 知识表补）。
	for _, mi := range infos {
		if mi.ContextWindow != 0 || mi.MaxTokens != 0 {
			t.Fatalf("静态兜底不应带元数据: %+v", mi)
		}
	}
	var _ = upstream.ModelInfo{} // 保持 upstream 依赖（断言类型同源）
}
