// freemodels_test.go 免费/低价模型判定闭包（server.FreeModelLookup）的行为契约。
//
// 保留积分（pool.reserve_credits）的"免费"判据来自模型目录的**只读快照**，本包负责把
// CN（cachedModelsSnapshot）与 global（upstream.GlobalModelInfosSnapshot）两个出口折算成
// 一个纯函数回调。这里锁三件事：
//  1. 倍率口径真的生效（x0.00 / 限时免费 factor=0 → 免费）；
//  2. **缺失 ≠ 免费**（目录冷、条目倍率未知、空模型名一律 false）；
//  3. 牌价与生效价的口径分离（promo 覆盖牌价时按生效价判）。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// withCNCatalog 把 CN 目录缓存灌成给定条目（测试结束还原）。dynamicModelsCache 是包级
// 变量，本用例不并行（与 handler_hint_test.go 同约定）。
func withCNCatalog(t *testing.T, infos []upstream.ModelInfo) {
	t.Helper()
	dynamicModelsCache.Lock()
	prevIDs, prevFetched, prevFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = prevIDs, prevFetched, prevFail
		dynamicModelsCache.Unlock()
	})
}

// TestFreeModelLookupMultiplierByPrice 倍率口径：牌价 x0.00 → 免费；x0.79 → 不免费。
func TestFreeModelLookupMultiplierByPrice(t *testing.T) {
	withCNCatalog(t, []upstream.ModelInfo{
		{ID: "free-model", Credits: "x0.00"},
		{ID: "paid-model", Credits: "x0.79"},
		{ID: "unknown-multiplier"}, // 倍率未知：缺失 ≠ 免费
	})
	lookup := FreeModelLookup(nil) // up=nil：只查 CN 侧（global 侧空）

	for id, want := range map[string]bool{
		"free-model":         true,
		"paid-model":         false,
		"unknown-multiplier": false,
		"not-in-catalog":     false,
		"":                   false,
	} {
		if got := lookup(id); got != want {
			t.Errorf("lookup(%q)=%v want %v", id, got, want)
		}
	}
}

// TestFreeModelLookupUsesEffectivePrice 生效价口径（牌价 vs 折扣的分离）：
// 有限时免费（PromoFactor=0）的模型即便牌价 x0.29 也算免费——保留积分要回答的是
// "此刻要花多少积分"，不是"转正后多少钱"。反过来，折扣结束后（promo 消失）回到牌价判定。
func TestFreeModelLookupUsesEffectivePrice(t *testing.T) {
	free := 0.0
	withCNCatalog(t, []upstream.ModelInfo{
		{ID: "promo-free", Credits: "x0.29", PromoFactor: &free, PromoCredits: "0x", PromoLabel: "限时免费"},
	})
	lookup := FreeModelLookup(nil)
	if !lookup("promo-free") {
		t.Error("限时免费（factor=0）的模型必须判为免费（按生效价而非牌价）")
	}
	// 折扣结束：promo 清掉后按牌价 x0.29 判定 → 不再免费。
	withCNCatalog(t, []upstream.ModelInfo{{ID: "promo-free", Credits: "x0.29"}})
	if lookup("promo-free") {
		t.Error("折扣结束后（仅剩牌价 x0.29）不得再判为免费——时变字段必须跟着目录刷新")
	}
}

// TestFreeModelLookupColdCatalogIsNotFree 目录缓存冷 → 一切皆非免费。
//
// 这条是"缺失 ≠ 免费"的现场形态：网关刚起、还没拉过目录时，若把"查不到"当成"免费"，
// 保留积分闸门会对所有模型失效（等于没开）；反过来把"查不到"当"付费"才是安全方向
// （宁可不放行，也不要用未知倍率吃掉底线）。
func TestFreeModelLookupColdCatalogIsNotFree(t *testing.T) {
	dynamicModelsCache.Lock()
	prevIDs, prevFetched, prevFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = nil, time.Time{}, time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = prevIDs, prevFetched, prevFail
		dynamicModelsCache.Unlock()
	})

	lookup := FreeModelLookup(nil)
	if lookup("glm-5.2") || lookup("hy4-preview") {
		t.Error("目录缓存冷时不得把任何模型判为免费（缺失 ≠ 免费）")
	}
}

// TestFreeModelLookupNilClientIsSafe up=nil（无 global 客户端/测试裸用）不得 panic，
// 且只认 CN 侧结果——main 装配时 up 恒非 nil，这条是裸用与测试路径的健壮性。
func TestFreeModelLookupNilClientIsSafe(t *testing.T) {
	withCNCatalog(t, []upstream.ModelInfo{{ID: "cn-free", Credits: "x0.00"}})
	lookup := FreeModelLookup(nil)
	if !lookup("cn-free") {
		t.Error("up=nil 时 CN 侧判定应照常生效")
	}
	if lookup("global-only-free") {
		t.Error("up=nil 时不得凭空判免费")
	}
}

// TestIsFreeModelParsesBothMultiplierForms 倍率原文的两种形态都要认：
// 牌价是前缀式（"x0.00"），折扣后价是后缀式（"0x" / "0.50x"）——上游两种都用。
func TestIsFreeModelParsesBothMultiplierForms(t *testing.T) {
	for _, credits := range []string{"x0.00", "x0", "0x", "0.0x", "x0.0", "0"} {
		infos := []upstream.ModelInfo{{ID: "m", Credits: credits}}
		if !upstream.IsFreeModel(infos, "m") {
			t.Errorf("credits=%q 应解析为 0（免费）", credits)
		}
	}
	for _, credits := range []string{"", "x", "免费", "x0.01", "0.50x"} {
		infos := []upstream.ModelInfo{{ID: "m", Credits: credits}}
		if upstream.IsFreeModel(infos, "m") {
			t.Errorf("credits=%q 不得判为免费（未知或非零）", credits)
		}
	}
}
