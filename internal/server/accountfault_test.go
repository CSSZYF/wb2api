package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 11140 request illegal 的处置回归（误杀修复）。
//
// 旧行为：一次 11140 即 Disable → 账号永久退出选号（disabled 无自动清除路径，
// 见 pool.TestAccountFaultDisabledRequiresManualRevive），而 11140 的根因**未定**
// （本仓历史上被归因为出站 UA 平台段、又被归因为账号状态，两者互斥且都无实测锚定；
// 现场 19 个历史实例与 4 个部署目录里没有任何账号因 11140 被禁、也没有真实 11140 报文）。
// 代价不对称：误判「不罚号」成本 = 多一次轮换（有界）；误判「Disable」成本 = 永久摘掉
// 健康账号 + 需人工登录（无界）。故改为照 NoteSessionDead 的既有模式阈值化：
// 未达阈值只软冷却（到期自愈），连续达阈才禁用（保留对「真封禁」的兜底）。

// TestApplyErrorPolicy11140SoftCoolsBeforeThreshold 首次 11140 不 Disable，只软冷却。
// 核心回归：实现前必红（旧实现直接 Disable）。
func TestApplyErrorPolicy11140SoftCoolsBeforeThreshold(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"code":11140,"msg":"request illegal"}`, "", nil, nil)
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if st.Disabled {
		t.Fatalf("首次 11140 不得禁用（误杀面：disabled 无自动恢复路径）: %+v", st)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("首次 11140 应软冷却（到期自愈）: %+v", st)
	}
	if st.Reason != "account fault (11140)" {
		t.Errorf("reason=%q want account fault (11140)", st.Reason)
	}
	// 软冷却有界：基数取自 SoftCooldown，不是永久态。
	if st.CoolRemaining <= 0 || st.CoolRemaining > 600 {
		t.Errorf("cool_remaining_sec=%d want in (0,600]", st.CoolRemaining)
	}
}

// TestApplyErrorPolicy11140DisablesAtThird 连续第 3 次 11140 → 禁用（保留真封禁兜底），
// 且 disabled_reason 含 11140（运维一眼看出判死依据）。
func TestApplyErrorPolicy11140DisablesAtThird(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"code":11140,"msg":"request illegal"}`
	for i := 1; i <= 2; i++ {
		h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次 11140 不应禁用", i)
		}
	}
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续第 3 次 11140 应禁用: %+v", st)
	}
	if !strings.Contains(st.DisabledReason, "11140") {
		t.Errorf("disabled_reason=%q 应含 11140", st.DisabledReason)
	}
	if st.Cooling {
		t.Errorf("禁用是比冷却更强的终态，不应叠加冷却: %+v", st)
	}
}

// TestApplyErrorPolicy11140SuccessClearsCount 中间成功一次（账号被证明是活的）→ 计数清零，
// 再来 2 次 11140 仍不禁（成功是「账号未死」的最强证据）。
func TestApplyErrorPolicy11140SuccessClearsCount(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"code":11140,"msg":"request illegal"}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
	p.NoteSuccess("u1") // chat 成功（成功路径必调，见 NoteSuccess 注释）
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("成功后重新计数，2 次 11140 不应禁用: %+v", st)
	}
}

// TestApplyErrorPolicy14017AlwaysSoftCools 14017（trial not activated）语义**一字不动**：
// 永远只软冷却、绝不禁用（补完 register 可能自愈）。连打 5 次也不禁用。
func TestApplyErrorPolicy14017AlwaysSoftCools(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"code":14017,"msg":"trial not activated"}`
	for i := 1; i <= 5; i++ {
		h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil, nil)
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("第 %d 次 14017 不得禁用（register 补完可自愈）: %+v", i, st)
		}
		if !st.Cooling || st.Reason != "account fault (14017)" {
			t.Fatalf("第 %d 次 14017 应软冷却且 reason 不变: %+v", i, st)
		}
	}
}

// TestApplyErrorPolicySessionDeadThreshold chat 路径的 12153 与 scheduler/keepalive 侧
// （scheduler.go 走 NoteSessionDead）**口径统一**：一次 12153 不再直接 Disable
// （state.go 明载「一次 12153 即 Disable 导致 13 个 disabled 号全是误判受害者」），
// 连续 3 次才禁用。
func TestApplyErrorPolicySessionDeadThreshold(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"code":12153,"msg":"Offline user session not found"}`
	for i := 1; i <= 2; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil, nil)
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次 12153 不应禁用（chat 路径口径须与 scheduler 一致）", i)
		}
	}
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil, nil)
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续第 3 次 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestApplyErrorPolicySessionDeadClearedBySuccess chat 路径 12153 计数同样被成功清零
// （NoteSuccess 清 sessionDeadFails 的既有语义，经 applyErrorPolicy 路径可观测）。
func TestApplyErrorPolicySessionDeadClearedBySuccess(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"code":12153,"msg":"Offline user session not found"}`
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil, nil)
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil, nil)
	p.NoteSuccess("u1")
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil, nil)
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil, nil)
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("成功后重新计数，2 次 12153 不应禁用: %+v", st)
	}
}

// TestChatRefreshSessionDeadThreshold chat 轮转内的 **token 临近过期先 refresh** 分支
// （handler.go 的 NeedsRefresh 分支）同样口径统一：refresh 返回 12153 不再单次
// Disable，连续 3 次才禁（与 scheduler.RunKeepaliveNow 的 keepalive 路径一致）。
func TestChatRefreshSessionDeadThreshold(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/auth/token/refresh") {
				return &http.Response{
					StatusCode: 401,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// ExpiresAt=1 → NeedsRefresh 恒真，每次 chat 都先走 refresh 分支。
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 1})
	h := NewHandler(Config{Pool: p, Upstream: up})
	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次 refresh 12153 不应禁用（chat refresh 分支须与 keepalive 同口径）: %+v", i, st)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续第 3 次 refresh 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}
