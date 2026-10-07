package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestPanelOverviewTodayUsage overview 下发 today：本地日历日零点起的用量合计
// （credit = 上游真实扣费加和），前端「今日已耗」拿它对照「日均需耗」一眼判断
// 今天是否消耗到位。
//
// 口径约定：
//   - 只计覆盖区间与「今日零点起」相交的分片（昨天的桶不得混入）；
//   - credit 只来自上游真实观测（usage.credit），无观测的请求不估算；
//   - 记录器未装配时 today 键整体缺席——前端显示「—」而非 0：
//     0 = 真没消耗，缺席 = 没这笔账，两回事不能混。
func TestPanelOverviewTodayUsage(t *testing.T) {
	rec := usage.New("")
	now := time.Now()
	// 今天：一笔真实扣费 + 一笔免费模型（无 credit 观测，只进请求/tokens）。
	rec.Add(now, "cn", "u1", "m1", usage.Delta{TotalTokens: 10, HasTotal: true, Credit: 0.4, HasCredit: true}, true)
	rec.Add(now, "cn", "u1", "free", usage.Delta{TotalTokens: 5, HasTotal: true}, true)
	// 昨天：量级放大，混进来会立刻翻车。
	rec.Add(now.Add(-48*time.Hour), "cn", "u1", "m1", usage.Delta{TotalTokens: 999, HasTotal: true, Credit: 9.9, HasCredit: true}, true)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Usage: rec})
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rsp := httptest.NewRecorder()
	pn.ServeHTTP(rsp, req)
	if rsp.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rsp.Code, rsp.Body.String())
	}

	var got struct {
		Today *struct {
			Requests    int64   `json:"requests"`
			TotalTokens int64   `json:"total_tokens"`
			Credit      float64 `json:"credit"`
		} `json:"today"`
	}
	if err := json.Unmarshal(rsp.Body.Bytes(), &got); err != nil {
		t.Fatalf("overview JSON 解析失败：%v", err)
	}
	if got.Today == nil {
		t.Fatal("装配了用量记录器时 overview 必须带 today")
	}
	if got.Today.Requests != 2 || got.Today.TotalTokens != 15 {
		t.Errorf("today.req/tok = %d/%d want 2/15（昨天的桶不得计入）", got.Today.Requests, got.Today.TotalTokens)
	}
	if got.Today.Credit < 0.39 || got.Today.Credit > 0.41 {
		t.Errorf("today.credit = %v want 0.4（真实扣费加和，无观测不估算）", got.Today.Credit)
	}

	// 未装配记录器：today 键整体缺席（前端显示 — 而非 0）。
	pn2 := New(Config{Version: "test", APIKey: "test-key", Pool: p})
	rsp2 := httptest.NewRecorder()
	pn2.ServeHTTP(rsp2, req)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rsp2.Body.Bytes(), &raw); err != nil {
		t.Fatalf("overview JSON 解析失败：%v", err)
	}
	if _, ok := raw["today"]; ok {
		t.Error("未装配用量记录器时 today 键必须缺席（没账 ≠ 0 消耗）")
	}
}

// TestAppJSDailyStatsWiring 概览条「日均需耗 / 今日已耗」的接线完整性：
// index.html 两个 chip + app.js 的合计计算与对比着色。app.js/index.html 是
// go:embed 静态资源，Go 编译器不校验——少一处接线面板上就是一个永远 "-" 的格子。
func TestAppJSDailyStatsWiring(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, must := range []string{
		`id="sDailyNeed"`, `id="sTodayUsed"`, `日均需耗`, `今日已耗`,
	} {
		if !strings.Contains(html, must) {
			t.Errorf("index.html 缺概览条接线：%s", must)
		}
	}

	raw, err = os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for _, must := range []string{
		"function paintDailyStats(", // 两个 chip 的渲染入口
		"expDailyNeed",              // 需耗合计的全局缓存
		"today.credit",              // 已耗数据源（overview.today）
		"paintDailyStats()",         // loadOverview/renderExpiry 的调用点
		"function fmtSpend(",        // 小额 credit 的显示格式
	} {
		if !strings.Contains(js, must) {
			t.Errorf("app.js 缺概览条接线：%s", must)
		}
	}
	// 需耗合计必须真的在 renderExpiry 里按「各行日均需耗」累加——
	// 与到期提醒逐行同口径，两处数字才对得上。
	body := jsFuncBody(js, "function renderExpiry(")
	if body == "" {
		t.Fatal("app.js 缺 renderExpiry")
	}
	for _, must := range []string{"needSum += daily", "expDailyNeed = needSum"} {
		if !strings.Contains(body, must) {
			t.Errorf("renderExpiry 未累计各行日均需耗：缺 %s", must)
		}
	}
}
