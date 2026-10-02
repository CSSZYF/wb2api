// handler_reserve_stale_test.go 免费判定的**陈旧目录**口径（保留积分 pool.reserve_credits）。
//
// 缺口（与上游 credit_floor 逐条对照后发现，比"启动空窗期"更严重）：
// 免费判定读 CN 侧 cachedModelsSnapshot，而该快照在**读时**按 dynamicModelsTTL（10min）
// 判过期——过期即返回 nil。写入它的只有 fetchDynamicModels，其唯一调用方是 /v1/models。
// 于是只要客户端不周期性拉 /v1/models（绝大多数客户端只在启动时拉一次，甚至从不拉），
// 启动 10 分钟后 CN 快照就恒为 nil：
//
//	目录 nil → IsFreeModel 恒 false（缺失 ≠ 免费）→ 余额触底的号把 CN 免费模型一并拦掉
//
// 而白名单只兜住用户点名的三个 id，目录里的其他限免 / x0.00 模型全被误拦——正是本功能
// 要修的病灶（用户原话「不然免费的 4.1 都用不了」），且它**每 10 分钟复发一次**，
// 不是一次性的启动空窗期。
//
// 上游的做法（对照 a4557dc 与 ModelRate）：倍率表**跨刷新持久**——每次目录刷新整体替换，
// 但读时不判过期。故上游只在"首次拉取之前"有盲区（用启动预热堵住），此后永远有值。
// 本文件锁同一口径：免费判定用**最近一次成功目录**（陈旧也认），而 /v1/models 的
// 10min TTL 语义原样保留（那是"面板实时、API 缓存"的取舍，与保底判定是两件事）。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// withStaleCNCatalog 把 CN 目录灌成给定条目，并把 fetched 拨到 TTL 之外（模拟
// "距上次 /v1/models 已超过 10 分钟"）。
func withStaleCNCatalog(t *testing.T, infos []upstream.ModelInfo) {
	t.Helper()
	dynamicModelsCache.Lock()
	prevIDs, prevFetched, prevFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now().Add(-dynamicModelsTTL - time.Minute)
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = prevIDs, prevFetched, prevFail
		dynamicModelsCache.Unlock()
	})
}

// TestFreeModelLookupSurvivesStaleCNCatalog 核心回归：CN 目录超出 TTL 后，免费判定
// 仍须按**最近一次成功目录**作答——否则保底会在启动 10 分钟后把 CN 免费模型全误拦。
func TestFreeModelLookupSurvivesStaleCNCatalog(t *testing.T) {
	withStaleCNCatalog(t, []upstream.ModelInfo{
		{ID: "promo-free", Credits: "x0.00"},
		{ID: "paid", Credits: "x1.62"},
	})

	lookup := FreeModelLookup(nil)
	if !lookup("promo-free") {
		t.Fatal("目录陈旧（超 TTL）时仍须按最近一次成功目录判免费" +
			"——否则启动 10 分钟后保底会把免费模型全误拦（每 10 分钟复发）")
	}
	if lookup("paid") {
		t.Error("陈旧目录里的收费模型仍不得判为免费")
	}
}

// TestCachedModelsSnapshotKeepsTTLSemantics 陈旧口径只作用于**免费判定**：
// cachedModelsSnapshot（/v1/models 与 gateway_hint 的数据源）的 10min TTL 语义必须
// 原样保留——那条 TTL 是"面板实时、API 缓存"的刻意取舍，放宽它会让 /v1/models 永远
// 返回不再刷新的旧目录。
func TestCachedModelsSnapshotKeepsTTLSemantics(t *testing.T) {
	withStaleCNCatalog(t, []upstream.ModelInfo{{ID: "promo-free", Credits: "x0.00"}})

	if got := cachedModelsSnapshot(); got != nil {
		t.Fatalf("cachedModelsSnapshot 超 TTL 必须仍返回 nil（TTL 语义不得放宽），got %d 条", len(got))
	}
	if got := cachedCatalogSnapshot(); len(got) != 1 {
		t.Fatalf("cachedCatalogSnapshot 必须返回最近一次成功目录（陈旧也认），got %d 条", len(got))
	}
}

// TestFreeModelLookupStaleCatalogStillExcludesUnknown 陈旧目录不得变成"一切皆免费"：
// 目录里没有的模型仍按"缺失 ≠ 免费"处理（保守），否则保底在陈旧期整体失效。
func TestFreeModelLookupStaleCatalogStillExcludesUnknown(t *testing.T) {
	withStaleCNCatalog(t, []upstream.ModelInfo{{ID: "promo-free", Credits: "x0.00"}})

	lookup := FreeModelLookup(nil)
	if lookup("some-unlisted-paid-model") {
		t.Error("陈旧目录里未列出的模型不得判为免费（缺失 ≠ 免费）")
	}
	if lookup("") {
		t.Error("空模型名不得判为免费")
	}
}
