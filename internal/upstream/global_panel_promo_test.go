package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestFetchModelsGlobalPromoUnionUA 面板 global 路径（FetchModelsDiag → fetchModelsOnce）
// 的 v3/config 覆盖必须走**双 UA 并集**，与目录探测（probeGlobalV3Capabilities）同口径。
//
// 复现的缺陷：fetchModelsOnce 的 v3 覆盖是 IDE-UA **单路**，而实测（见 codeBuddyCLIUA
// 注释）IDE 路**不含 deepseek 系列**——于是 modelPromotions 里指向 deepseek-v4.1-flash
// （正是用户点名要保的免费模型之一）的优惠会被 applyModelPromotions 的
// 「目录外模型不挂」判据直接丢掉，面板倍率列显示不出生效价。
//
// 本用例的假上游刻意模拟这个形态：promo 只挂在 CLI 路独有的模型上。
func TestFetchModelsGlobalPromoUnionUA(t *testing.T) {
	// 企业端点目录：含 CLI 独有模型（global 的 /v2 目录是权威目录，两路模型都在）。
	// v3/config：IDE 路不给 deepseek，CLI 路给且带 modelPromotions。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"},
				{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03"}
			]}}`))
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			ua := r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/json")
			if ua == codeBuddyCLIUA {
				// CLI 路：含 deepseek 系列 + 指向它的限时免费优惠。
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
					{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"},
					{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03"}
				],"modelPromotions":[
					{"enabled":true,"priority":100,"modelIds":["deepseek-v4.1-flash"],
					 "badge":{"label":"限时免费"},
					 "discount":{"discountedCredits":"0x","factor":0},
					 "hover":{"textZh":"限时免费至 09-30"},
					 "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
				]}}`))
				return
			}
			// IDE 路：**不含 deepseek 系列**（实测口径），自然也没有指向它的 promo。
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x1"}
			]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), GlobalEnabled: true}
	c.ChatBaseGlobal = srv.URL
	c.BillingBaseGlobal = srv.URL
	a := globalAuth()

	infos, _, err := c.FetchModelsDiag(a)
	if err != nil {
		t.Fatalf("FetchModelsDiag: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	ds, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("目录缺 deepseek-v4.1-flash，实际 ids=%v", idsOf(infos))
	}
	// IDE 单路下这里恒为空（promo 被「目录外模型不挂」丢掉）——并集后必须挂上。
	if ds.PromoFactor == nil || *ds.PromoFactor != 0 || ds.PromoCredits != "0x" || ds.PromoLabel != "限时免费" {
		t.Errorf("global 面板路径丢了 CLI 路模型的 promo（v3 覆盖应走双 UA 并集）: %+v", ds)
	}
	// 牌价不得被 promo 污染（口径分离，同 TestGlobalPromoNotPollutingCredits）。
	if ds.Credits != "x0.03" {
		t.Errorf("deepseek-v4.1-flash Credits=%q want x0.03（牌价原样）", ds.Credits)
	}
	// IDE 路独有的能力补全照常生效（并集不得破坏既有能力覆盖）。
	if glm := byID["glm-5.2"]; glm.ContextWindow != 131072 {
		t.Errorf("glm-5.2 ContextWindow=%d want 131072", glm.ContextWindow)
	}
}

// TestFetchModelsCNStaysIDEOnly CN 侧必须**仍走 IDE 单路**：CN 目录主源是企业端点，
// v3 只作能力覆盖，换 UA 无收益（见 codeBuddyCLIUA 注释）——本仓刻意不让 CN 多发一路。
// 锁住这条是为了防止「为 global 修 bug」时把 CN 的调用数一起翻倍。
func TestFetchModelsCNStaysIDEOnly(t *testing.T) {
	var uas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768}
			]}}`))
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			uas = append(uas, r.Header.Get("User-Agent"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":32768}
			]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	cnAuth := &auth.Auth{UID: "c1", AccessToken: "tok", Domain: "www.codebuddy.cn"}

	if _, _, err := c.FetchModelsDiag(cnAuth); err != nil {
		t.Fatalf("FetchModelsDiag(cn): %v", err)
	}
	if len(uas) != 1 || uas[0] != codeBuddyIDEUA {
		t.Errorf("CN 侧 v3/config UA 列表=%v want 恰好一条 %s（不得为 global 的修复翻倍）", uas, codeBuddyIDEUA)
	}
}
