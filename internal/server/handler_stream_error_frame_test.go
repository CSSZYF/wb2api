// handler_stream_error_frame_test.go 流式「200 已开流 + 一帧 error」的账号处置
// 端到端验收（吸收上游 PR #93 的 WithErrorFrameObserver + 成功判定下沉）。
//
// 缺陷（我们此前的实现）：handler.go 在读第一帧**之前**就执行 NoteSuccess +
// BlockModelClear + Session.Bind，上游「200 + error 帧」（6004 限流 / 内容拦截 /
// 审核）是真实形态——被限流的号被记成健康、11102 负缓存被误清、粘性把整个会话
// 钉死在它身上，后续每一轮都打同一个限流号。
//
// 修复口径：成功三件套下沉到「真成功」的两个分支——流式 default（读完且无 error
// 帧）与 sErr != nil（客户端断连，上游帧无恙）；error 帧分支走 applyErrorPolicy
// 且**不记成功**。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// bindCounterStore 记录 SetBind 调用**次数**（不止最终值）：粘性分配（ResolveForModel
// 慢路径）本身会 SetBind 一次，成功路径的「粘性跟随最终成功号」会再 SetBind 一次。
// error 帧的流不得触发第二次——计数就是这件事的可观测量。
type bindCounterStore struct {
	redisstore.Noop
	mu    sync.Mutex
	n     int
	last  string
	calls []string
}

func (b *bindCounterStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.n++
	b.last = uid
	b.calls = append(b.calls, uid)
	b.mu.Unlock()
}

func (b *bindCounterStore) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

func (b *bindCounterStore) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// counterSessionFor 构建绑定到 bindCounterStore 的粘性路由（两个可用账号）。
func counterSessionFor(st *bindCounterStore) *session.Router {
	return session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"a1"} },
	})
}

// sseWithErrorFrame 上游 200 已开流 + 一帧 error（6004 模型级限流，带重置墙钟）。
// 这正是「被限流的号被记成健康」的现场形态：HTTP 层 200，业务错误只在流内。
const sseWithErrorFrame = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\"," +
	"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"error\":{\"code\":6004,\"message\":\"模型限流，将在 2030-09-27 01:00:00 重置\"}}\n\n" +
	"data: [DONE]\n\n"

// TestChatStreamErrorFrameNoSuccessNoSticky 上游 200 + error 帧（6004 模型级限流）：
//   - 账号**不得**被记成功（success_count 保持 0、last_success 为零值）；
//   - 按帧内容走 applyErrorPolicy（6004 带重置墙钟 → 该账号该模型的模型级冷却）；
//   - 粘性**不得**绑定到该账号（此前会钉死在限流号上）。
//
// 实现前必红：成功三件套在 peek.Stream 分支之前执行，读第一帧前就记成功。
func TestChatStreamErrorFrameNoSuccessNoSticky(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithErrorFrame, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	st := new(bindCounterStore)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: counterSessionFor(st), SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"prompt_cache_key":"sess-errframe","messages":[{"role":"user","content":"hi"}]}`)))

	// 头已发出（200 开流），error 帧照常透传（error-passthrough 语义不动）。
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（头已发出，error 帧在流内）body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("上游 error 帧原文必须透传: %s", rec.Body)
	}
	// 核心断言 1：不得记成功。
	ps, _ := p.Status("a1")
	if ps.SuccessCount != 0 || !ps.LastSuccessTime.IsZero() {
		t.Errorf("error 帧的流不得记成功: success=%d last_success=%v", ps.SuccessCount, ps.LastSuccessTime)
	}
	// 核心断言 2：按帧内容处置账号——6004 + 重置墙钟 → 该模型的模型级冷却。
	if len(ps.RateLimitedModels) == 0 {
		t.Errorf("error 帧应按 applyErrorPolicy 处置（6004 → 模型级冷却）: %+v", ps)
	} else if ps.RateLimitedModels[0].Model != "glm-5.2" {
		t.Errorf("模型级冷却应记在请求模型上: %+v", ps.RateLimitedModels[0])
	}
	// 核心断言 3：粘性不得被**成功路径重绑**。分配（ResolveForModel 慢路径）本身
	// 必然 SetBind 一次；error 帧的流若再绑一次（计数 2），会话就被钉死在这个正在
	// 限流的号上——后续每轮都打它。
	if got := st.count(); got != 1 {
		t.Errorf("error 帧的流不得触发成功路径的粘性重绑: SetBind 次数=%d want 1（仅分配那次）, calls=%v", got, st.snapshot())
	}
}

// TestChatStreamCleanSuccessStillMarksSuccess 正常流（无 error 帧）仍照常记成功 +
// 绑粘性——下沉不得把成功路径一起改坏（零回归的正控）。
func TestChatStreamCleanSuccessStillMarksSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	st := newBindStore()
	h := NewHandler(Config{Pool: p, Upstream: up, Session: stickySessionFor(t, st), SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"prompt_cache_key":"sess-clean","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	ps, _ := p.Status("a1")
	if ps.SuccessCount != 1 {
		t.Errorf("正常流应记成功: success=%d", ps.SuccessCount)
	}
	if len(st.binds) != 1 {
		t.Errorf("正常流应建立粘性绑定: %v", st.binds)
	}
}

// TestChatStreamErrorFrameContentBlockedPolicy error 帧内容拦截形态：审核拦截的
// error 帧同样不得记成功（不因 code 不同而漏掉成功判定下沉）。
func TestChatStreamErrorFrameContentBlockedPolicy(t *testing.T) {
	const frame = "data: {\"error\":{\"code\":400,\"message\":\"content blocked by policy\"}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, frame, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	ps, _ := p.Status("a1")
	if ps.SuccessCount != 0 {
		t.Errorf("内容拦截 error 帧不得记成功: success=%d", ps.SuccessCount)
	}
}

// TestChatStreamErrorFrameKindFromPayload 帧分类取自 payload（FrameKind）：6004 →
// ErrSoftRate（模型级冷却），审核类 → ErrContentBlocked（零动作、不罚号）。
// 这里直接断言冷却是否落在「模型级」而非「账号级」上——后者会把健康账号整体
// 冷却，切模型也救不回来。
func TestChatStreamErrorFrameKindFromPayload(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithErrorFrame, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	ps, _ := p.Status("a1")
	// 6004 帧：模型级冷却，账号级 until 不置位（切模型可用，issue #31 语义）。
	if ps.Cooling {
		t.Errorf("6004 帧应是模型级冷却（切模型豁免），不该账号级冷却: %+v", ps)
	}
	if len(ps.RateLimitedModels) != 1 {
		t.Errorf("应恰有一条模型级冷却: %+v", ps.RateLimitedModels)
	}
}
