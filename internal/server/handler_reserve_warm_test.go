// handler_reserve_warm_test.go 保留积分的**启动预热**与**/status 透出**两条缺口。
//
// 背景（与上游 credit_floor 逐条对照后确认的缺口）：
//   - 上游 a4557dc：「倍率表只在 FetchModels/FetchGlobalModelInfos 成功时填充，两者都是
//     懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发之间的空窗期里
//     ModelRate 恒返回空串，积分保底的目录兜底判不出收费，触底号被当成「收费未知」
//     放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）」。
//   - 上游 d19add4：「/status 透出 credit_floor 生效值」。
//
// 我们的对应物是 server.FreeModelLookup 的**只读目录快照**：重启后 CN/global 两个目录
// 缓存都是冷的 → 一切皆非免费（freemodels.go 的既有契约「缺失 ≠ 免费」）→ 余额触底的
// 号把免费模型也一起拦掉。而那恰恰是本功能要修的用户痛点（「不然免费的 4.1 都用不了」）。
// 白名单只兜住用户点名的三个 id，目录里的其他限免/x0.00 模型全在空窗期内不可用。
//
// 故本文件锁两件事：
//  1. WarmModelCatalog 启动预热把两个目录缓存灌热（异步、失败不致命、逃生门关锁时
//     不探 global）；
//  2. /status 透出 reserve_credits 的**生效值**（0 = 关闭，与实际值一致），
//     让运维不必翻 config.json 就能确认闸门当前是否生效。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// zeroTime 零值时刻（清缓存用；dynamicModelsCache 的三个时间字段各有语义，显式写零）。
func zeroTime() time.Time { return time.Time{} }

// jsonRespFixture 构造一个 JSON HTTP 响应（本包测试用；upstream 包的同名助手不可跨包）。
func jsonRespFixture(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// pathRecorder 并发安全的请求路径记录（本包测试用）。
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) add(p string) {
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.mu.Unlock()
}

func (r *pathRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// resetCatalogCaches 清空 CN 与 global 两个目录缓存（模拟"刚重启"），并注册还原。
func resetCatalogCaches(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	prevIDs, prevFetched, prevFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = nil, zeroTime(), zeroTime()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = prevIDs, prevFetched, prevFail
		dynamicModelsCache.Unlock()
	})
}

// TestWarmModelCatalogPopulatesCNCache 启动预热必须把 CN 目录灌热：预热前
// FreeModelLookup 对目录里的限免模型答"非免费"（缺失 ≠ 免费），预热后答"免费"
// ——这正是"重启后保底空窗期"的修复点。
func TestWarmModelCatalogPopulatesCNCache(t *testing.T) {
	resetCatalogCaches(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"promo-free","credits":"x0.00","maxInputTokens":131072},{"id":"paid","credits":"x1.62","maxInputTokens":131072}],"agents":[{"name":"cli","models":["promo-free","paid"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	lookup := FreeModelLookup(up)
	if lookup("promo-free") {
		t.Fatal("前置条件：目录冷时不应判为免费（缺失 ≠ 免费）")
	}

	WarmModelCatalog(context.Background(), up, p)

	if !lookup("promo-free") {
		t.Fatal("预热后目录里的 x0.00 模型必须判为免费（否则重启空窗期内免费模型被保底拦掉）")
	}
	if lookup("paid") {
		t.Error("预热后收费模型不得被判为免费")
	}
}

// TestWarmModelCatalogPopulatesGlobalCache global 域独立目录端点，同样必须预热
// （两个域各有各的缓存，只热一个会让另一域的空窗期照旧）。
func TestWarmModelCatalogPopulatesGlobalCache(t *testing.T) {
	resetCatalogCaches(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, "<html>500</html>", false
	})
	up.GlobalEnabled = true
	up.ChatBaseGlobal = "https://fake.example"
	up.BillingBaseGlobal = "https://fake.example"

	ga := &auth.Auth{UID: "g1", AccessToken: "gt1", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	p := testPoolWith(ga)
	// global 域探测走 /v2 家族；给该路径返回 global 目录（x0.00 的限免模型）。
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/personal/models") {
			return jsonRespFixture(200, `{"code":0,"data":{"models":[{"id":"gpt-5.4","credits":"x0.00","maxInputTokens":262144}],"agents":[{"name":"cli","models":["gpt-5.4"]}]}}`), nil
		}
		return jsonRespFixture(500, "<html>500</html>"), nil
	})

	WarmModelCatalog(context.Background(), up, p)

	if got := up.GlobalModelInfosSnapshot(); len(got) == 0 {
		t.Fatal("预热后 global 目录缓存必须已填充（否则 global 域保底空窗期照旧）")
	}
}

// TestWarmModelCatalogGlobalSkippedWhenGateClosed 逃生门（GlobalEnabled=false）关锁时
// 不得探测 global（按 CN 处理，与面板/目录同口径：零上游调用）。
func TestWarmModelCatalogGlobalSkippedWhenGateClosed(t *testing.T) {
	resetCatalogCaches(t)
	paths := &pathRecorder{}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"cn-model","credits":"x0.00"}],"agents":[{"name":"cli","models":["cn-model"]}]}}`, false
	})
	up.GlobalEnabled = false
	up.ChatBaseGlobal = "https://fake.example"
	up.BillingBaseGlobal = "https://fake.example"
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths.add(r.URL.Path)
		return jsonRespFixture(200, `{"code":0,"data":{"models":[{"id":"cn-model","credits":"x0.00"}],"agents":[{"name":"cli","models":["cn-model"]}]}}`), nil
	})

	ga := &auth.Auth{UID: "g1", AccessToken: "gt1", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	p := testPoolWith(ga)

	WarmModelCatalog(context.Background(), up, p)

	for _, path := range paths.all() {
		if strings.Contains(path, "/v2/enterprises/personal/models") {
			t.Fatalf("逃生门关锁时不得探测 global 目录，实际请求 %s", path)
		}
	}
}

// TestWarmModelCatalogCanceledContextReturns 预热不得拖住进程退出：ctx 已取消时
// 立刻返回，不发任何上游请求。
func TestWarmModelCatalogCanceledContextReturns(t *testing.T) {
	resetCatalogCaches(t)
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{"code":0,"data":{"models":[]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGINT/SIGTERM 已到达
	WarmModelCatalog(ctx, up, p)

	if calls != 0 {
		t.Fatalf("ctx 已取消时不得发上游请求，实际 %d 次", calls)
	}
}

// TestWarmModelCatalogNoAccountsIsNoop 池内无该域账号时不发探测（与面板 models 同口径：
// 无账号即无可用凭证，避免无谓上游调用）。
func TestWarmModelCatalogNoAccountsIsNoop(t *testing.T) {
	resetCatalogCaches(t)
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{"code":0,"data":{"models":[]}}`, false
	})
	p := pool.New("") // 空池

	WarmModelCatalog(context.Background(), up, p)

	if calls != 0 {
		t.Fatalf("空池不得发上游请求，实际 %d 次", calls)
	}
}

// TestStatusExposesReserveCredits /status 必须透出保留积分的**生效值**：运维据此确认
// 闸门当前是否生效（0 = 关闭）与线在哪，不必翻 config.json。
func TestStatusExposesReserveCredits(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 8448)
	p.SetReserveCredits(50)
	h := NewHandler(Config{Pool: p})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if got := body["reserve_credits"]; got != float64(50) {
		t.Fatalf("reserve_credits=%v want 50（/status 必须透出生效值）", got)
	}
}

// TestStatusReserveCreditsReflectsHotApply 面板热改后 /status 的透出值必须跟着变
// （否则运维看到的是过期值，"面板保存即生效"的契约在观测面上失效）。
func TestStatusReserveCreditsReflectsHotApply(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})

	p.SetReserveCredits(0) // 关闭
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if got := body["reserve_credits"]; got != float64(0) {
		t.Fatalf("关闭时 reserve_credits=%v want 0", got)
	}

	p.SetReserveCredits(150) // 热改
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	body = map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if got := body["reserve_credits"]; got != float64(150) {
		t.Fatalf("热改后 reserve_credits=%v want 150", got)
	}
}
