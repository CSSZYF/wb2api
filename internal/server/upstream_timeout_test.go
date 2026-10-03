// upstream_timeout_test.go 上游超时/停滞的三态判定与「不换号」止损端到端验收
// （吸收上游 PR #93 的 isUpstreamTimeout）。
//
// 缺陷：超时此前落 handler 的传输层抖动分支（i/o timeout 在 IsTransient 词表里）
// → 只 fail + 退避换号。同一份请求换到别的号撞上的是同一个慢上游，换号注定白换，
// 只会把客户端拖到 MaxRotate × header_timeout，期间还给一串健康号白喂连败计数。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stubTimeoutErr 实现 net.Error 的超时 stub（Timeout() 可切换）。
type stubTimeoutErr struct{ timeout bool }

func (e stubTimeoutErr) Error() string   { return "stub timeout" }
func (e stubTimeoutErr) Timeout() bool   { return e.timeout }
func (e stubTimeoutErr) Temporary() bool { return e.timeout }

// TestIsUpstreamTimeout 三态判定口径（吸收 PR #93 的七例对照）：
// net.Error.Timeout / 显式 deadline / 空闲看门狗掐流（客户端仍在但 ctx 取消）。
// 客户端断连（clientGone）**不**算上游超时——人已走，语义是「中断」。
func TestIsUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		clientGone bool
		want       bool
	}{
		{"net.Error 超时", stubTimeoutErr{true}, false, true},
		{"net.Error 非超时（连接被拒）", stubTimeoutErr{false}, false, false},
		{"显式 deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), false, true},
		{"os.ErrDeadlineExceeded 包装", fmt.Errorf("read tcp: %w", os.ErrDeadlineExceeded), false, true},
		{"客户端断连（ctx 已取消）", context.Canceled, true, false},
		{"空闲看门狗掐流（客户端仍在）", context.Canceled, false, true},
		{"普通错误", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpstreamTimeout(c.err, c.clientGone); got != c.want {
				t.Fatalf("isUpstreamTimeout(%v, clientGone=%v) = %v, want %v", c.err, c.clientGone, got, c.want)
			}
		})
	}
	// 保证 stub 真的实现了 net.Error（否则上面的用例会静默走不到 Timeout 分支）。
	var ne net.Error = stubTimeoutErr{true}
	_ = ne
}

// TestChatUpstreamTimeoutNoRotate 超时止损端到端（核心回归）：
// 池内 3 个账号，上游每次调用都返回 i/o timeout → **只调一次上游**（不换号），
// 客户端拿到 503 + code=upstream_timeout 的可区分文案；账号池零惩罚
// （无冷却/禁用/熔断/连败——超时不是账号的问题）。
//
// 实现前必红：i/o timeout 落 IsTransient 分支 → 换号重试 MaxRotate 次（calls=3），
// 末端 code 是 no_healthy_account。
func TestChatUpstreamTimeoutNoRotate(t *testing.T) {
	up, calls := newErrUpstream(&net.OpError{Op: "read", Net: "tcp", Err: osDeadlineErr{}})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	p.SetDegrade(1, 0, 0) // 阈值 1：只要喂一次连败立刻可观测
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	// 核心断言 1：不换号——上游只被调一次（MaxRotate=3 时旧行为会调 3 次）。
	if got := calls(); got != 1 {
		t.Errorf("calls=%d want 1（超时止损：换号注定白换，MaxRotate 在超时场景有意失效）", got)
	}
	// 核心断言 2：末端 code 可区分（不是 no_healthy_account）。
	if body := rec.Body.String(); !strings.Contains(body, "upstream_timeout") {
		t.Errorf("末端 code 应为 upstream_timeout（与「没号可用」区分）: %s", body)
	}
	// 核心断言 3：账号零惩罚。
	for _, uid := range []string{"a1", "a2", "a3"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.BreakerFails != 0 || st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("%s 超时不得罚号: %+v", uid, st)
		}
	}
	if uids := p.AvailableUIDs(); len(uids) != 3 {
		t.Errorf("超时后整池应保持可用: %v", uids)
	}
}

// TestChatUpstreamTimeoutViaIdleWatchdog 空闲看门狗形态：出站 ctx 被 cancel
// （客户端仍在）→ 判为上游停滞，同样不换号 + upstream_timeout。
func TestChatUpstreamTimeoutViaIdleWatchdog(t *testing.T) {
	up, calls := newErrUpstream(context.Canceled)
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
	rec := httptest.NewRecorder()
	// 入站 ctx 未取消（客户端仍在）→ 取消只可能来自空闲看门狗。
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if got := calls(); got != 1 {
		t.Errorf("calls=%d want 1（空闲掐流 = 上游停滞，不换号）", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "upstream_timeout") {
		t.Errorf("code 应为 upstream_timeout: %s", body)
	}
}

// TestChatClientDisconnectNotUpstreamTimeout 对照：客户端主动断连（入站 ctx 已取消）
// **不**走 upstream_timeout 分支——人已走，语义是中断，且重试无意义。既有路径
// （抖动分支 → rotateBackoff 见 ctx 取消即终止轮转）保持原样。
func TestChatClientDisconnectNotUpstreamTimeout(t *testing.T) {
	up, calls := newErrUpstream(context.Canceled)
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已断连
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := calls(); got != 1 {
		t.Errorf("calls=%d want 1（客户端已走，退避见 ctx 取消即终止轮转）", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "upstream_timeout") {
		t.Errorf("客户端断连不得标成 upstream_timeout（语义不同）: %s", body)
	}
}

// TestChatUpstreamTimeoutRotatesForNonTimeout 对照：**非**超时的传输层抖动仍按
// 既有语义轮转 MaxRotate 次——本项只截走超时，不放宽其他抖动路径（零回归）。
func TestChatUpstreamTimeoutRotatesForNonTimeout(t *testing.T) {
	up, calls := newErrUpstream(errors.New("connection reset by peer"))
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if got := calls(); got != 3 {
		t.Errorf("calls=%d want 3（非超时抖动仍走既有 MaxRotate 轮转）", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "upstream_timeout") {
		t.Errorf("非超时抖动不得标成 upstream_timeout: %s", body)
	}
}

// TestUpstreamTimeoutSentinelNotUpstreamError errUpstreamTimeout 是 server 包本地
// 哨兵（不是 upstream.Error 信封）：末端不得被当成上游错误去 hintOf 取 hint——
// 本地调度类错误的 hint 固定 no_healthy_account 口径。
func TestUpstreamTimeoutSentinelNotUpstreamError(t *testing.T) {
	var ue *upstream.Error
	if errors.As(errUpstreamTimeout, &ue) {
		t.Fatal("errUpstreamTimeout 不得是 *upstream.Error（本地哨兵，无上游原文）")
	}
}
