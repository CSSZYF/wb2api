package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestAccountReviveForcesSoftCooldownClear 面板「解冻」按钮（POST /panel/api/accounts/{uid}/revive
// → accountRevive → Pool.Revive）必须保持**强制解冻**语义：人工干预不受 issue #199
// 自动解冻收窄（只对 CoolHard 放行）的影响，软冷却/6004 模型级冷却/熔断/禁用一并清除。
// 这是软冷却的唯一人工出口——收窄后余额刷新不再解冻软冷却账号，若无本端点运维将
// 只能干等冷却到期。
func TestAccountReviveForcesSoftCooldownClear(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	// 软冷却 + 模型级 6004 冷却 + 熔断（三类惩罚态全占）。
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1")

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/revive", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Cooling || st.Disabled || st.BreakerUntil != (time.Time{}) {
		t.Errorf("人工解冻应清冷却/熔断/禁用: %+v", st)
	}
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("人工解冻应清模型级 6004 冷却台账: %+v", st.RateLimitedModels)
	}
	// 解冻后账号回到可选状态（同模型请求也应可选）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Errorf("解冻后账号应可被选中，got %+v", got)
	}
}

// TestAccountReviveNotFound 不存在的 uid 返回 404（既有契约不变）。
func TestAccountReviveNotFound(t *testing.T) {
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	req := httptest.NewRequest("POST", "/panel/api/accounts/nope/revive", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}
