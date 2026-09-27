package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// transient_transport_test.go 传输层抖动的账号池语义端到端验收（任务 A）。
//
// 缺陷（自伤面）：本机网络抖一下（unexpected EOF / connection reset / i/o timeout /
// TLS 握手失败 / DNS 失败），请求会逐号轮转，而 handler 的传输层分支对**每个**账号
// 各喂一次连败计数（NoteFailures）→ 达阈（默认 5）全体降权 10 分钟 → 用户看到
// 「503 all accounts unavailable (cooling/disabled)」。形态与 11135「一张坏图拖垮
// 整个账号池」完全相同（见 handler_image_terminal_test.go）。
//
// 为什么传输层抖动不该喂连败：连败降权（issue #114）的目标是「不知道原因的失败」
// 指向**账号**的情形；而 unexpected EOF / connection reset / i/o timeout / DNS 失败
// 是**出口链路**的条件——同一链路对池内全部账号一视同仁，单次请求里 N 个账号全中
// 恰恰证明根因不在账号上（证据不指向任何单个账号，不该由账号池承担代价）。
//
// 保守优先（**有意保留**的语义）：判不出来的错误串（未知错误）保持原行为继续喂连败
// ——宁可误罚也不能让真故障账号永远留在池内（见 upstream.IsTransient 注释）。
//
// 裸 io.EOF **不算**抖动（判定见 upstream.IsTransient 注释）：它是「连接被对端正常
// 关闭」，可能是抖动，也可能是上游主动断（内容审核掐流等），无法区分 → 保守口径。

// newErrUpstream 假上游：传输层直接失败（Do 返回 err，非 *upstream.Error），
// 每次调用计数。handler 侧走「网络抖动分支」（uerr == nil && terr != nil）。
func newErrUpstream(err error) (*upstream.Client, func() int) {
	var mu sync.Mutex
	var calls int
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, err
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return c, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// TestChatTransientTransportErrorNoConsecutiveFails 单账号抖动（核心回归）：
// 传输层抖动**不喂连败**（consecutive_fails 保持 0）、不降权、无冷却/熔断/禁用，
// 账号留在可用池内；客户端仍拿到 503（上游确实不可达，错误照实回）。
// 修复前：一次抖动即 consecutive_fails=1；默认阈值 5 下连抖 5 次该号出池。
func TestChatTransientTransportErrorNoConsecutiveFails(t *testing.T) {
	for name, terr := range map[string]error{
		"unexpected EOF":     io.ErrUnexpectedEOF,
		"connection reset":   &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		"i/o timeout":        &net.OpError{Op: "read", Net: "tcp", Err: osDeadlineErr{}},
		"dns no such host":   &net.DNSError{Err: "no such host", Name: "copilot.tencent.com", IsNotFound: true},
		"tls handshake fail": errors.New("net/http: TLS handshake timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			up, calls := newErrUpstream(terr)
			p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
			// 阈值 1：只要喂一次连败就立刻降权——把「喂没喂」这个动作放大成可观测状态。
			p.SetDegrade(1, time.Hour, 2*time.Hour)
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
			}
			if calls() == 0 {
				t.Fatal("precondition: 假上游应被调用")
			}
			st, _ := p.Status("a1")
			if st.ConsecutiveFails != 0 {
				t.Errorf("传输层抖动不得喂连败: consecutive_fails=%d", st.ConsecutiveFails)
			}
			if !st.DegradeUntil.IsZero() || st.CoolKind == "degrade" || st.Reason == "consecutive failures" {
				t.Errorf("传输层抖动不得降权: %+v", st)
			}
			if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
				t.Errorf("传输层抖动不得冷却/禁用/熔断: %+v", st)
			}
			if uids := p.AvailableUIDs(); len(uids) != 1 {
				t.Errorf("抖动后账号应留在可用池: %v", uids)
			}
		})
	}
}

// TestChatTransientThreeAccountsNoPoolPollution 池内 3 个账号全抖动（自伤面本体）：
// 三个账号 consecutive_fails 全为 0、全部留在池内，且轮转次数受既有 MaxRotate 约束
// （不因抖动自创第二套重试策略：每次轮转仍走 rotateBackoff + MaxRotate 上限）。
// 修复前：3 个账号各记一次连败，一个请求就吃掉 3/5 的降权额度。
func TestChatTransientThreeAccountsNoPoolPollution(t *testing.T) {
	up, calls := newErrUpstream(errors.New("read tcp 10.0.0.1:443->1.2.3.4:80: connection reset by peer"))
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	p.SetDegrade(1, time.Hour, 2*time.Hour) // 阈值 1：喂一次即降权（放大可观测性）
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	// 轮转上限沿用既有 MaxRotate（不无限重试、不自创第二套策略）。
	if got := calls(); got != 3 {
		t.Errorf("calls=%d want 3（MaxRotate 上限内轮转完三个号即止）", got)
	}
	for _, uid := range []string{"a1", "a2", "a3"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 {
			t.Errorf("%s consecutive_fails=%d want 0（链路条件不指向任何账号）", uid, st.ConsecutiveFails)
		}
		if !st.DegradeUntil.IsZero() || st.Cooling {
			t.Errorf("%s 不应降权/冷却: %+v", uid, st)
		}
	}
	if uids := p.AvailableUIDs(); len(uids) != 3 {
		t.Errorf("抖动后整池应保持可用: %v", uids)
	}
}

// TestChatUnknownTransportErrorStillFeedsConsecutiveFails 保守语义（**有意保留**）：
// 判定不出来的错误串（未知错误）**仍然**喂连败计数——宁可误罚也不能让真故障账号
// 永远留在池内。此用例锁住「保守优先」这条取舍不被后续放宽判定时无意打破。
func TestChatUnknownTransportErrorStillFeedsConsecutiveFails(t *testing.T) {
	up, calls := newErrUpstream(errors.New("something weird happened"))
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetDegrade(1, time.Hour, 2*time.Hour) // 阈值 1：喂一次即降权
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if calls() == 0 {
		t.Fatal("precondition: 假上游应被调用")
	}
	st, _ := p.Status("a1")
	// 达阈降权 = 连败确实被喂进去了（未知错误维持既有保守行为）。
	if st.CoolKind != "degrade" || st.Reason != "consecutive failures" {
		t.Fatalf("未知传输错误应维持既有语义（继续喂连败 → 达阈降权）: %+v", st)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Errorf("达阈后应出池: %v", uids)
	}
	if st.BreakerFails != 0 {
		t.Errorf("传输层错误仍不喂熔断（既有语义不回归）: fails=%d", st.BreakerFails)
	}
}

// TestChatBareEOFStillFeedsConsecutiveFails 裸 io.EOF 的判断：**不算**抖动。
// 裸 EOF 是「连接被对端正常关闭」——可能是抖动，也可能是上游主动断（内容审核掐流、
// 后端重启等），无法区分；保守口径下继续喂连败（宁可误罚，也不让真故障号留在池内）。
// 与「unexpected EOF 算抖动」形成对照：后者是连接**未写完**就断（链路故障的确定性形态）。
func TestChatBareEOFStillFeedsConsecutiveFails(t *testing.T) {
	up, _ := newErrUpstream(io.EOF)
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetDegrade(1, time.Hour, 2*time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	st, _ := p.Status("a1")
	if st.CoolKind != "degrade" {
		t.Fatalf("裸 io.EOF 判为不确定 → 维持既有保守语义（喂连败）: %+v", st)
	}
}

// TestChatEnvelope5xxStillTripsBreaker 5xx **信封**错误不受本项影响：ErrServer 仍走
// 既有熔断路径（NoteError），不得被并进「传输层抖动」免罚分支——那是既有语义
// （upstream.Classify 的 ≥500 分类），也是唯一熔断入口。
//
// 断言口径：阈值 1 时 recordBreakerFailureLocked 触发熔断会把 fails 清零
// （见 pool/cooldown.go：`e.fails = 0` + retryCount++ + breakerUntil 置位），
// 故此处以 Cooling/BreakerUntil 为准（既有的 TestChatHTTP5xxPenalizes 同口径），
// 而非 BreakerFails。
func TestChatEnvelope5xxStillTripsBreaker(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 502, `{"code":500,"msg":"bad gateway from upstream business layer"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("a1")
	if !st.Cooling || st.BreakerUntil.IsZero() {
		t.Errorf("5xx 信封应触发熔断（既有语义，唯一熔断入口）: %+v", st)
	}
	if st.ConsecutiveFails != 0 {
		t.Errorf("带权威分类的错误不喂连败（不重复计罚）: %d", st.ConsecutiveFails)
	}
	if !st.DegradeUntil.IsZero() {
		t.Errorf("5xx 不得走连败降权路径: %+v", st)
	}
}

// osDeadlineErr 复现 net 层超时错误的形态（i/o timeout）：实现 net.Error 且
// Timeout()=true（真实来源是 os.ErrDeadlineExceeded / 系统 ETIMEDOUT）。
type osDeadlineErr struct{}

func (osDeadlineErr) Error() string   { return "read tcp 10.0.0.1:443: i/o timeout" }
func (osDeadlineErr) Timeout() bool   { return true }
func (osDeadlineErr) Temporary() bool { return true }
