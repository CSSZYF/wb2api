package server

// real_price_scope_test.go 口径边界（分支 feat/real-price）：
//
//  1. /v1/models（对外 OpenAI 契约）**保持现状**——倍率不进该路径（PLAN §3.D2）。
//     用户要的是**面板**能看到真实价，不是改对外契约；这里加用例把现状锁死，
//     防"顺手把面板的修法扩散到 /v1/models"（那会让所有客户端的模型列表多出一个
//     未在 OpenAI schema 内的字段，且倍率随促销变动会污染客户端的列表缓存）。
//  2. 面板 prefixed_id 用的前缀形式必须与**后端路由协议**一致（cn: / global:）——
//     自创形式（如 intl: / oversea:）会让用户复制出来的 id 在网关侧被当成裸模型名
//     （splitRealmPrefix 只认 cn/global），静默路由到缺省域。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestModelListStaysCreditsFree /v1/models 的口径**保持现状**（本分支不改对外契约）。
//
// 现状（2026-10-02 线上实测 /v1/models 复核）：两条路径的口径**本来就不一致**——
//   - global 条目：倍率被 upstream.detachCredits 摘进旁表，**不带 credits**
//     （PLAN §3.D2「倍率不进路由/列表口径」）；例外是写死条目（pinned.Entry() 带
//     credits，实测 deepseek-v4.1-flash 的 x0.00 就是这么出来的）；
//   - CN 条目：fetchModelsOnce 保留 credits，modelEntry 原样透出（实测 hy3-x 的
//     x0.05 / kimi-k2.7 的 x0.57 都在）。
//
// 这个不对称是既有事实，本分支**不动它**（用户要的是面板能看到真实价，不是改
// OpenAI 契约——倍率随促销变动，透进客户端列表会污染其模型缓存）。本用例把现状
// 钉死，防"顺手把面板的修法扩散到 /v1/models"。
func TestModelListStaysCreditsFree(t *testing.T) {
	withGlobalEnabled(t)
	resetDynamicModelsCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/console/enterprises/personal/models":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00 credits"},
				{"id":"kimi-k2.7","maxInputTokens":256000,"maxOutputTokens":32000,"credits":"x0.57"}
			],"agents":[{"name":"cli","models":["hy3","kimi-k2.7"]}]}}`))
		case "/v2/enterprises/personal/models":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"gpt-5.4","maxInputTokens":262144,"maxOutputTokens":65536,"credits":"x1.65"}
			],"agents":[{"name":"cli","models":["gpt-5.4"]}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at-gl", Domain: "x.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{
		Pool: p, Upstream: up, GlobalEnabled: true,
		StripRealmPrefix: true, RealmPrecedence: "global",
		HiddenModels: upstream.ResolveHiddenModels(nil),
		// 显式空数组：本用例只验上游路径，排除写死条目自带的 credits 干扰。
		PinnedModels: upstream.ResolvePinnedModels([]upstream.PinnedModel{}),
	})

	list := h.modelList()
	if len(list) == 0 {
		t.Fatal("/v1/models 列表为空")
	}
	byID := make(map[string]map[string]any, len(list))
	for _, e := range list {
		if id, _ := e["id"].(string); id != "" {
			byID[id] = e
		}
	}
	// global 条目：倍率不进该路径（含 hy3——两域都有，precedence=global 保留 global 那份）。
	for _, id := range []string{"gpt-5.4", "hy3"} {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("/v1/models 缺 %s，实际 ids=%v", id, keysOfAny(byID))
		}
		if _, has := e["credits"]; has {
			t.Errorf("global 条目 %s 不应带 credits（PLAN §3.D2 口径保持），got %v", id, e["credits"])
		}
	}
	// CN 条目：现状是带 credits（本分支不改；这里锁死，防被"顺手统一"）。
	e, ok := byID["kimi-k2.7"]
	if !ok {
		t.Fatalf("/v1/models 缺 kimi-k2.7，实际 ids=%v", keysOfAny(byID))
	}
	if e["credits"] != "x0.57" {
		t.Errorf("CN 条目 kimi-k2.7 credits=%v want x0.57（既有口径，本分支不动）", e["credits"])
	}
}

// TestRealmPrefixFormAcceptedByRouter 面板 prefixed_id 的前缀形式必须被路由认。
// splitRealmPrefix 是唯一真相：第一个冒号前恰为 cn/global 才剥离（大小写敏感）；
// 其它形态（intl: / CN: / 多冒号）整体按裸名处理——所以面板不能自创前缀。
func TestRealmPrefixFormAcceptedByRouter(t *testing.T) {
	r := RealmRouter{
		GlobalEnabled: true,
		Precedence:    "global",
		HasRealm:      func(string) bool { return true },
	}
	cases := []struct {
		model    string
		realm    string
		bare     string
		explicit bool
	}{
		{"global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash", true},
		{"cn:hy3", "cn", "hy3", true},
		{"hy3", "global", "hy3", false},           // 裸名按 precedence
		{"intl:hy3", "global", "intl:hy3", false}, // 自创前缀 = 裸名（会 404）
		{"CN:hy3", "global", "CN:hy3", false},     // 大小写敏感
		{"cn:", "cn", "", true},                   // 空前缀体：剥出空裸名（后续选号失败）
	}
	for _, tc := range cases {
		realm, bare, explicit := r.ResolveWithSource(tc.model)
		if realm != tc.realm || bare != tc.bare || explicit != tc.explicit {
			t.Errorf("Resolve(%q)=(%q,%q,%v) want (%q,%q,%v)",
				tc.model, realm, bare, explicit, tc.realm, tc.bare, tc.explicit)
		}
	}
}
