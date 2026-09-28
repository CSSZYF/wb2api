package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsGlobalPromoEndToEnd 复现并锁死用户报告的「折扣一个都没显示」。
//
// 现场：纯 global 池（云端实例，v1.9.25），/panel/api/models?realm=global 的
// promo_factor / promo_credits / promo_label 全是 null。
//
// 两条根因（都在 global 路径上，CN 路径没有）：
//  1. applyGlobalV3Catalog 是 fill-only 逐字段搬运，**完全没有 Promo 字段**——v3/config
//     已把优惠挂在返回的 ModelInfo 上（applyModelPromotions），合并这一步把它丢了；
//  2. fetchModelsOnce 的 v3 覆盖走 IDE-UA **单路**，而实测 IDE 路不含 deepseek 系列
//     ——指向 deepseek-v4.1-flash 的优惠被「目录外模型不挂」丢掉。
//
// 本用例的假上游同时构造这两个形态（CLI 路独有的模型 + 挂在它上面的 promo），
// 断言面板真的把 promo_* 下发出去（而非仅内部结构对）。
func TestPanelModelsGlobalPromoEndToEnd(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	const cliUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy4-preview","name":"Hy4 Preview","maxInputTokens":200000,"maxOutputTokens":32768,"credits":"x0.29"},
				{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"},
				{"id":"deepseek-v4.1-flash","name":"DS Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03"}
			]}}`))
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("User-Agent") == cliUA {
				// CLI 路：含 deepseek 系列 + 指向它的限时免费优惠。
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
					{"id":"hy4-preview","name":"Hy4 Preview","maxInputTokens":200000,"maxOutputTokens":32768,"credits":"x0.29"},
					{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"},
					{"id":"deepseek-v4.1-flash","name":"DS Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03"}
				],"modelPromotions":[
					{"enabled":true,"priority":100,"modelIds":["deepseek-v4.1-flash"],
					 "badge":{"label":"限时免费"},
					 "discount":{"discountedCredits":"0x","factor":0},
					 "hover":{"textZh":"限时免费至 09-30"},
					 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}},
					{"enabled":true,"priority":50,"modelIds":["glm-5.2"],
					 "badge":{"label":"错峰使用"},
					 "hover":{"textZh":"每日 23:00–07:50 优惠"},
					 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
				]}}`))
				return
			}
			// IDE 路：不含 deepseek 系列（实测口径）。
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy4-preview","name":"Hy4 Preview","maxInputTokens":200000,"maxOutputTokens":32768,"credits":"x0.29"},
				{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"}
			],"modelPromotions":[
				{"enabled":true,"priority":50,"modelIds":["glm-5.2"],
				 "badge":{"label":"错峰使用"},
				 "hover":{"textZh":"每日 23:00–07:50 优惠"},
				 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
			]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL
	pn := New(Config{
		Version: "test", APIKey: "k", Pool: p, Upstream: up,
		HiddenModels: upstream.ResolveHiddenModels(nil), PinnedModels: upstream.ResolvePinnedModels(nil),
	})

	req := httptest.NewRequest("GET", "/panel/api/models?realm=global", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Realm  string           `json:"realm"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, m := range got.Models {
		if id, _ := m["id"].(string); id != "" {
			byID[id] = m
		}
	}

	// ① CLI 路独有的模型：promo 必须透出（单走 IDE 路时这里是 null，即用户看到的症状）。
	ds, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("面板列表缺 deepseek-v4.1-flash，实际=%v", idsOfModels(got.Models))
	}
	if f, ok := ds["promo_factor"].(float64); !ok || f != 0 {
		t.Errorf("deepseek-v4.1-flash promo_factor=%v want 0（限时免费）", ds["promo_factor"])
	}
	if ds["promo_credits"] != "0x" || ds["promo_label"] != "限时免费" || ds["promo_note"] != "限时免费至 09-30" {
		t.Errorf("deepseek-v4.1-flash promo 字段不全: %+v", ds)
	}
	if ds["credits"] != "x0.03" {
		t.Errorf("牌价 credits=%v want x0.03（不得被 promo 污染）", ds["credits"])
	}

	// ② badge-only 条目（无 discount）：只下标签，promo_factor 键缺席（缺失 ≠ 免费，
	// 前端不得回填）。
	glm := byID["glm-5.2"]
	if glm["promo_label"] != "错峰使用" {
		t.Errorf("glm-5.2 promo_label=%v want 错峰使用", glm["promo_label"])
	}
	if _, present := glm["promo_factor"]; present {
		t.Errorf("badge-only 条目不得下发 promo_factor（会被前端读成限时免费）: %+v", glm)
	}

	// ③ 无优惠的模型：四个 promo_* 键全部缺席。
	hy := byID["hy4-preview"]
	for _, k := range []string{"promo_factor", "promo_credits", "promo_label", "promo_note"} {
		if _, present := hy[k]; present {
			t.Errorf("未命中优惠的 hy4-preview 不得带 %s: %+v", k, hy)
		}
	}
}

// idsOfModels 面板条目的 id 列表（失败信息里用，便于看清"到底有哪些模型"）。
func idsOfModels(ms []map[string]any) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if id, _ := m["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}
