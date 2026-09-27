package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// handler_hardcredit_14018_test.go 429 + code 14018（账号积分耗尽）端到端验收
// （吸收上游 d47219b，issue #175）。
//
// 缺陷：上游把「积分耗尽」也用 429 + 业务码 14018 表达。Classify 的通用 429 兜底
// 先于一切文案判定（fork-scan-absorb T-3 的既有裁定：429 body 高频携带跨计费/限流
// 两界的措辞，按文案判会把限流号硬冷却 12h），于是 14018 落 ErrSoftRate → 软冷却
// 到期（默认 600s）后又被选中 → 又 429。更糟的是**全池冷却兜底**
// （pickEarliestExpiryLocked 允许软冷却号参与）会把真耗尽的号反复选中白打请求。
//
// 修法：429 + 结构化 code 14018 → ErrHardCredit（硬冷却到次日 04:00，等签到恢复），
// 插在通用 429 兜底之前；只认结构化码不猜文案，故「429 + quota exceeded 文案」
// 的既有软限流裁定完全不变。

// TestChat429Code14018UsesHardCreditCooldown 端到端（移植上游 d47219b 用例）：
// 429 + code 14018 的账号必须被**硬冷却**（reason=余额不足、until 次日 04:00），
// 且全池冷却兜底不得再选中它；请求本身立即换号成功返回 200。
func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // bad 积分高 → 确定性先被选中
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" || st.Until.Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", st.Reason, st.Until)
	}
	// 关键行为断言：硬冷却号**不参与全冷却兜底**（pickEarliestExpiryLocked 排除
	// CoolHard）——否则真耗尽的号会被反复选中白打请求。
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat429QuotaTextStaysSoftCooldown 429 + 跨两界文案（无 14018 码）仍走软限流：
// 14018 的引入不得推翻 fork-scan-absorb T-3 的既有裁定（按文案判会把限流号
// 硬冷却 12h）。同一 handler 内对照两条响应，锁住「只认结构化码」的边界。
func TestChat429QuotaTextStaysSoftCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"quota exceeded, please wait"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("quota 文案的 429 必须仍是软限流: %+v", st)
	}
	if st.Reason != "429" && st.Reason == "余额不足" {
		t.Fatalf("不应被硬冷却（reason 不得是余额不足）: %+v", st)
	}
	// 软冷却时长按配置的 1min 基数（而非硬冷却的次日 04:00）。
	if d := time.Until(st.Until); d <= 0 || d > 2*time.Minute {
		t.Fatalf("软冷却时长应约为配置基数: until=%v d=%v", st.Until, d)
	}
}
