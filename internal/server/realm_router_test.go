package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func withGlobalEnabled(t *testing.T) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
}

// TestRealmRouterBareNameSingleRealm 单域部署（只登国际版账号）裸名落 global。
//
// 这是"去掉前缀后裸名还能路由对"的核心保证：历史实现裸名恒判 cn，只登国际版
// 账号的部署把裸名发过来必然 503（线上日志实证：deepseek-v4 → 503，
// global:deep → 200）。
func TestRealmRouterBareNameSingleRealm(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})

	// 故意把 Precedence 设成 cn：单域场景下它不该被采用。
	r := RealmRouter{GlobalEnabled: true, Precedence: "cn", HasRealm: p.HasRealm}
	if realm, bare := r.Resolve("gpt-5.4"); realm != "global" || bare != "gpt-5.4" {
		t.Fatalf("Resolve(gpt-5.4)=(%q,%q) want (global,gpt-5.4)", realm, bare)
	}
}

// TestRealmRouterSingleCNRealm 反向单域：只有 CN 账号时裸名仍落 cn（零回归）。
func TestRealmRouterSingleCNRealm(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn"})

	r := RealmRouter{GlobalEnabled: true, Precedence: "global", HasRealm: p.HasRealm}
	if realm, _ := r.Resolve("glm-5.2"); realm != "cn" {
		t.Fatalf("Resolve(glm-5.2) realm=%q want cn（只有 CN 账号）", realm)
	}
}

// TestRealmRouterExplicitPrefixWins 显式前缀压过池内可用域，老客户端配置零改动。
func TestRealmRouterExplicitPrefixWins(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn"})

	r := RealmRouter{GlobalEnabled: true, Precedence: "global", HasRealm: p.HasRealm}
	if realm, bare := r.Resolve("cn:glm-5.2"); realm != "cn" || bare != "glm-5.2" {
		t.Fatalf("Resolve(cn:glm-5.2)=(%q,%q) want (cn,glm-5.2)", realm, bare)
	}
	if realm, bare := r.Resolve("global:gpt-5.4"); realm != "global" || bare != "gpt-5.4" {
		t.Fatalf("Resolve(global:gpt-5.4)=(%q,%q) want (global,gpt-5.4)", realm, bare)
	}
	// 冒号前段不是 cn/global → 整体当裸名，不能被误切。
	if realm, bare := r.Resolve("gpt-5.4:latest"); realm != "global" || bare != "gpt-5.4:latest" {
		t.Fatalf("Resolve(gpt-5.4:latest)=(%q,%q) want (global,gpt-5.4:latest)", realm, bare)
	}
}

// TestRealmRouterPrecedenceBothRealms 两域都有账号时裸名按 realm_precedence；
// global.enabled=false 的纯 CN 锁定逃生门优先于 precedence。
func TestRealmRouterPrecedenceBothRealms(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn"})

	if got := (RealmRouter{GlobalEnabled: true, Precedence: "cn", HasRealm: p.HasRealm}).BareRealm(); got != "cn" {
		t.Fatalf("BareRealm()=%q want cn", got)
	}
	if got := (RealmRouter{GlobalEnabled: true, Precedence: "global", HasRealm: p.HasRealm}).BareRealm(); got != "global" {
		t.Fatalf("BareRealm()=%q want global", got)
	}
	if got := (RealmRouter{GlobalEnabled: false, Precedence: "global", HasRealm: p.HasRealm}).BareRealm(); got != "cn" {
		t.Fatalf("global 逃生门关闭时 BareRealm()=%q want cn", got)
	}
	// HasRealm=nil（无池）不 panic，按 precedence 走。
	if got := (RealmRouter{GlobalEnabled: true, Precedence: "global"}).BareRealm(); got != "global" {
		t.Fatalf("BareRealm() with nil HasRealm=%q want global", got)
	}
}

// TestRealmAvailableGating 域可用性只看归属，且 global 受逃生门约束。
func TestRealmAvailableGating(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")

	// 空池：两域都不可用 → modelList 走双域静态兜底（不空列表）。
	h := NewHandler(Config{Pool: p, GlobalEnabled: true, StripRealmPrefix: true, RealmPrecedence: "global"})
	if h.realmAvailable("cn") || h.realmAvailable("global") {
		t.Fatal("空池时两域都应不可用")
	}

	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	if h.realmAvailable("cn") {
		t.Fatal("只有 global 账号时 cn 应不可用")
	}
	if !h.realmAvailable("global") {
		t.Fatal("有 global 账号时 global 应可用")
	}
	// 逃生门：global.enabled=false → 即便池里有 global 账号也不可用。
	hOff := NewHandler(Config{Pool: p, GlobalEnabled: false, StripRealmPrefix: true, RealmPrecedence: "global"})
	if hOff.realmAvailable("global") {
		t.Fatal("global.enabled=false 时 global 应不可用（纯 CN 锁定）")
	}
}

// stubGlobalUpstream 起一个本地假上游：/v2 家族给模型表，/console 家族返回 500
// HTML（与线上国际站行为一致），并统计各家族命中次数。
func stubGlobalUpstream(t *testing.T) (*upstream.Client, *int, *int) {
	t.Helper()
	var consoleHits, v2Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/enterprises/personal/models":
			v2Hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"gpt-5.4"},{"id":"brand-new-model"},{"id":"default-model"}]}}`))
		case "/console/enterprises/personal/models":
			consoleHits++
			http.Error(w, "<html><head><title>500 Internal Server Error</title></head></html>",
				http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL
	return up, &consoleHits, &v2Hits
}

// TestModelListGlobalOnlyNoPrefix 只登国际版账号时 /v1/models：
// id 全为裸名、只含国际版名单、且零 CN 上游请求（不碰 /console 家族）。
func TestModelListGlobalOnlyNoPrefix(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})
	up, consoleHits, v2Hits := stubGlobalUpstream(t)

	h := NewHandler(Config{
		Pool: p, Upstream: up,
		GlobalEnabled: true, StripRealmPrefix: true, RealmPrecedence: "global",
		HiddenModels: upstream.ResolveHiddenModels(nil), // 与 main 注入方式一致
		PinnedModels: upstream.ResolvePinnedModels(nil), // 同上
	})
	list := h.modelList()
	if len(list) == 0 {
		t.Fatal("modelList() 为空")
	}
	ids := make(map[string]bool, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		if id == "" {
			t.Fatalf("条目缺 id: %v", m)
		}
		if strings.Contains(id, ":") {
			t.Fatalf("id=%q 仍带域前缀（strip_realm_prefix=true 应为裸名）", id)
		}
		ids[id] = true
	}
	if ids["default-model"] {
		t.Fatal("default-model 是上游路由别名，默认应被 hidden_models 隐藏")
	}
	// 写死条目：上游**没返回** deepseek-v4.1-flash，/v1/models 仍必须列出它，
	// 且 id 是裸名（无域前缀）——客户端拉取模型时不能少这一个。
	if !ids["deepseek-v4.1-flash"] {
		t.Fatalf("缺 deepseek-v4.1-flash（写死条目未生效），实际 ids=%v", keysOf(ids))
	}
	for _, m := range list {
		if id, _ := m["id"].(string); id == "deepseek-v4.1-flash" {
			if m["credits"] != "x0.03" {
				t.Errorf("写死条目应透出 credits=x0.03，got %v", m["credits"])
			}
			if m["context_length"] != int64(1000000) {
				t.Errorf("写死条目应透出 context_length=1000000，got %v", m["context_length"])
			}
			if _, has := m["supported_efforts"]; has {
				t.Error("固定档不应输出 supported_efforts 键")
			}
		}
	}
	if !ids["brand-new-model"] {
		t.Fatalf("未含探测独有模型，ids=%v", keysOf(ids))
	}
	if ids["glm-5.1"] {
		t.Fatalf("glm-5.1 是 CN 静态表条目，纯 global 部署不应列出")
	}
	if *consoleHits != 0 {
		t.Fatalf("不应请求 /console 家族，命中 %d 次", *consoleHits)
	}
	if *v2Hits == 0 {
		t.Fatal("未请求 /v2 家族")
	}
}

// TestModelListKeepPrefixWhenDisabled strip_realm_prefix=false 时保留历史前缀协议。
func TestModelListKeepPrefixWhenDisabled(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})
	up, _, _ := stubGlobalUpstream(t)

	h := NewHandler(Config{
		Pool: p, Upstream: up,
		GlobalEnabled: true, StripRealmPrefix: false, RealmPrecedence: "global",
	})
	for _, m := range h.modelList() {
		id, _ := m["id"].(string)
		if !strings.HasPrefix(id, "global:") {
			t.Fatalf("id=%q 缺 global: 前缀（strip_realm_prefix=false）", id)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
