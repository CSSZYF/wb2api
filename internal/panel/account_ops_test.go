package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// ---------------------------------------------------------------------------
// 临时停用 / 恢复（上游 a20d06f 吸收）：与 disable/revive 是两套语义
// ---------------------------------------------------------------------------

// postPanel 打一个面板 POST 请求（带鉴权），返回响应记录。
func postPanel(t *testing.T, pn *Panel, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest("POST", path, rdr)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	return rec
}

// TestAccountSuspendTakesOutOfPool 临时停用 → 出池（不参与选号）但仍在池里；
// 恢复 → 回池。这是本特性的核心行为闭环。
func TestAccountSuspendTakesOutOfPool(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})

	if p.Pick() == nil {
		t.Fatal("precondition: 停用前应可选")
	}

	rec := postPanel(t, pn, "/panel/api/accounts/u1/suspend", `{"reason":"观察几天"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 响应回显双位状态，面板据此直接更新 UI（changed=true 表示本次确实置位）。
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["manual_disabled"] != true {
		t.Errorf("suspend 响应 manual_disabled=%v want true", resp["manual_disabled"])
	}
	if resp["disabled"] != false {
		t.Errorf("suspend 不应置 disabled: %v", resp["disabled"])
	}
	if resp["changed"] != true {
		t.Errorf("首次 suspend changed=%v want true", resp["changed"])
	}

	// 出池：不可选、状态仍可读且透出停用原因。
	if got := p.Pick(); got != nil {
		t.Fatalf("停用后不应被选中, got %+v", got)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("停用号仍在池里，状态应可读")
	}
	if !st.ManualDisabled || st.ManualReason != "观察几天" {
		t.Errorf("停用态未透出: %+v", st)
	}
	if st.Disabled {
		t.Error("临时停用不应置永久禁用位")
	}

	// 恢复 → 回池。
	rec = postPanel(t, pn, "/panel/api/accounts/u1/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["manual_disabled"] != false {
		t.Errorf("resume 响应 manual_disabled=%v want false", resp["manual_disabled"])
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("恢复后应回到池子, got %+v", got)
	}
}

// TestAccountSuspendResumeIdempotent 幂等：重复调用不报错、changed=false。
// 面板重试/双击不该弹错误。
func TestAccountSuspendResumeIdempotent(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})

	if rec := postPanel(t, pn, "/panel/api/accounts/u1/suspend", ""); rec.Code != http.StatusOK {
		t.Fatalf("首次 suspend code=%d", rec.Code)
	}
	rec := postPanel(t, pn, "/panel/api/accounts/u1/suspend", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 suspend code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["changed"] != false {
		t.Errorf("重复 suspend changed=%v want false", resp["changed"])
	}
	// 无体调用应回落到默认原因文案（不写空 reason，运维要看得到为什么摘的）。
	if st, _ := p.Status("u1"); st.ManualReason == "" {
		t.Error("无体 suspend 应写默认原因，不应留空")
	}
}

// TestAccountSuspendDoesNotClearAutoDisable 双位独立（反向断言）：临时停用不清
// 永久禁用标记，恢复也不清——只清自己那一位；仍禁用时 resume 回不了池。
func TestAccountSuspendDoesNotClearAutoDisable(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})

	rec := postPanel(t, pn, "/panel/api/accounts/u1/suspend", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Fatalf("临时停用不得清永久禁用位/原因: %+v", st)
	}
	if !st.ManualDisabled {
		t.Fatalf("停用位未置: %+v", st)
	}

	// 恢复：只清停用位，仍禁用 → 回不了池（响应里 disabled=true 就是给前端的提示）。
	rec = postPanel(t, pn, "/panel/api/accounts/u1/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["disabled"] != true {
		t.Errorf("resume 应回显 disabled=true（前端据此提示还需解冻）: %v", resp)
	}
	st, _ = p.Status("u1")
	if st.ManualDisabled {
		t.Error("resume 应清停用位")
	}
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Fatalf("resume 不得连带清永久禁用: %+v", st)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("永久禁用仍在，不应可选, got %+v", got)
	}
	// 解冻后才回池。
	if rec := postPanel(t, pn, "/panel/api/accounts/u1/revive", ""); rec.Code != http.StatusOK {
		t.Fatalf("revive code=%d", rec.Code)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("解冻后应回池, got %+v", got)
	}
}

// TestAccountReviveKeepsSuspend 反向：面板「解冻」（revive）**不**解除临时停用，
// 且响应回显 manual_disabled=true 让前端能提示「还需恢复」。
func TestAccountReviveKeepsSuspend(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetManualDisabled("u1", true, "运维摘除")
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})

	rec := postPanel(t, pn, "/panel/api/accounts/u1/revive", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revive code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["manual_disabled"] != true {
		t.Errorf("revive 后应回显 manual_disabled=true（前端提示还需恢复）: %v", resp)
	}
	if st, _ := p.Status("u1"); !st.ManualDisabled {
		t.Error("revive 不应清临时停用位")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("临时停用仍在，不应可选, got %+v", got)
	}
}

// TestAccountSuspendNotFound 不存在的 uid 返回 404（与既有 /accounts/* 契约一致）。
func TestAccountSuspendNotFound(t *testing.T) {
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	for _, p := range []string{"/panel/api/accounts/nope/suspend", "/panel/api/accounts/nope/resume"} {
		if rec := postPanel(t, pn, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s code=%d want 404 body=%s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestAccountSuspendAuthRequired 鉴权口径与既有 /panel/api/accounts/* 完全一致：
// 无 key → 401（且不得发生任何状态改动）。
func TestAccountSuspendAuthRequired(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})

	for _, path := range []string{"/panel/api/accounts/u1/suspend", "/panel/api/accounts/u1/resume"} {
		req := httptest.NewRequest("POST", path, nil) // 故意不带 Authorization
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无 key code=%d want 401", path, rec.Code)
		}
	}
	// 未鉴权的请求绝不能改状态。
	if st, _ := p.Status("u1"); st.ManualDisabled {
		t.Error("未鉴权请求不得置停用位")
	}
	if p.Pick() == nil {
		t.Error("未鉴权请求不得影响可选性")
	}
}
