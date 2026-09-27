// sequential_backoff_test.go 429 轮转不退避（首字延迟修复）的端到端锁定。
//
// 两条必须同时锁死（缺一即回归）：
//  1. ErrSoftRate（429）触发轮转**不等待**——切的是另一个账号，上游频控按账号计，
//     等待纯白等且抬高首字延迟；断言总耗时不随轮转序号 i 增长（i 越大等待越长，
//     是退避仍在生效的特征）。
//  2. ErrWafBlock（403）**仍然退避**——WAF 是 IP/指纹级频控，同一出口 IP 换任何
//     账号都在同一风控面，退避是该修复（P0-2）的核心机制，不得被本项削弱。
//
// 测试手法：把 rotateBackoffBase 临时恢复到可观测值（TestMain 已置 0 加速其他用例），
// 用「上游调用次数 + 总耗时」两个可观测量区分「退了」与「没退」。
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSoftRateRotateDoesNotBackoff 429 轮转零退避（核心验收点）。
// 池内 3 个号全部 429（无恢复时间文案，走有界退避冷却），MaxRotate 默认 3：
//   - 修复前：每次轮转前等 backoffAfter(i)（base=200ms → 0/200/400ms），总耗时 ≥ 600ms；
//   - 修复后：三次尝试连续发出，总耗时 < 200ms（仅本地开销）。
//
// 断言用「总耗时 < 首轮退避的下界」：即使只退避一次也必然超标，故该断言对
// 「部分退避」「抖动下限」都成立，不依赖精确时长。
func TestSoftRateRotateDoesNotBackoff(t *testing.T) {
	withRotateBackoff(t, 200*time.Millisecond)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"rate limited, please try again later"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	elapsed := time.Since(start)

	// 全池 429 → 末端 429（模型级冷却耗尽）或 503（账号级冷却），两者都是合法出口；
	// 本用例只锁「不退避」这一件事。
	if rec.Code != 429 && rec.Code != 503 {
		t.Fatalf("全池 429 的出口应为 429/503，code=%d body=%s", rec.Code, rec.Body)
	}
	// 三次尝试必然发生（每轮换一个新号，tried 保证），总耗时仍应远小于一次退避。
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("429 轮转不得退避：elapsed=%v（≥ base=200ms 说明至少退避了一次）", elapsed)
	}
}

// TestSoftRateRotateElapsedDoesNotGrowWithIndex 断言「总耗时不随 i 增长」：
// 对比 MaxRotate=1（零次轮转）与 MaxRotate=4（三次轮转）的耗时——若退避生效，
// 后者会多出 (0+200+400)=600ms；不退避时两者同量级。
// 该形态对「实现改成只在 i>=1 才退避」这类半吊子修复也敏感（差值会明显跳变）。
func TestSoftRateRotateElapsedDoesNotGrowWithIndex(t *testing.T) {
	withRotateBackoff(t, 200*time.Millisecond)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"rate limited"}`, false
	})
	run := func(maxRotate int) time.Duration {
		p := testPoolWith(
			&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
			&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
			&auth.Auth{UID: "a4", AccessToken: "at4", ExpiresAt: 9999999999},
		)
		h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: maxRotate})
		start := time.Now()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		return time.Since(start)
	}
	one := run(1)
	four := run(4)
	// 三次额外轮转若各退避一次，差值 ≥ 600ms；不退避时差值应在一轮退避基数以内。
	if d := four - one; d >= 200*time.Millisecond {
		t.Fatalf("429 轮转耗时应与轮转次数无关：MaxRotate=1 用 %v，=4 用 %v（差 %v ≥ 一次退避基数）",
			one, four, d)
	}
}

// TestWafBlockRotateStillBacksOff WAF 403 轮转**仍然退避**（反向断言，防「顺手把
// 所有退避都删了」）。池内 2 个号：首个 WAF 403（HTML 拦截页形态）→ 退避后换号成功。
// 修复前该用例已通过（本项刻意不动 WAF 路径），此处锁死它不被后续改动误伤。
func TestWafBlockRotateStillBacksOff(t *testing.T) {
	withRotateBackoff(t, 200*time.Millisecond)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			// 无业务信封的 403（HTML 拦截页）→ Classify 归 ErrWafBlock。
			return 403, "<html><body>403 Forbidden</body></html>", false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	elapsed := time.Since(start)

	if rec.Code != 200 {
		t.Fatalf("WAF 403 后换号应成功，code=%d body=%s", rec.Code, rec.Body)
	}
	// 退避 200ms（±25% → ≥150ms）：必须观察到等待，否则 WAF P0-2 机制被削弱。
	if elapsed < 150*time.Millisecond {
		t.Fatalf("WAF 403 轮转必须保留退避（IP 级频控），elapsed=%v want ≥150ms", elapsed)
	}
}

// TestBackoffWorthwhilePolicy 判据表（单一事实来源的行为快照）：
// 只有 ErrSoftRate 免退避，WAF 403 与其余分类一律退避。任何"顺手放宽"都会在此变红。
func TestBackoffWorthwhilePolicy(t *testing.T) {
	cases := []struct {
		kind upstream.ErrKind
		want bool
	}{
		{upstream.ErrSoftRate, false},    // 429：换的是另一个账号，等待无意义
		{upstream.ErrWafBlock, true},     // 403 WAF：IP 级频控，退避是核心机制
		{upstream.ErrServer, true},       // 5xx
		{upstream.ErrClient, true},       // 未知 4xx
		{upstream.ErrHardCredit, true},   // 402 余额耗尽
		{upstream.ErrNotFound, true},     // 404
		{upstream.ErrModelBlocked, true}, // 11102
		{upstream.ErrSessionDead, true},  // 12153
		{upstream.ErrNone, true},         // 防御路径
	}
	for _, c := range cases {
		if got := backoffWorthwhile(c.kind); got != c.want {
			t.Errorf("backoffWorthwhile(%v)=%v want %v", c.kind, got, c.want)
		}
	}
}

// TestRotateBackoffKindSoftRateNoWait 429 分支：即便 base 很大也不等待，且 ctx 取消
// 仍返回 false（调用方「false 即终止轮转」语义单一）。
func TestRotateBackoffKindSoftRateNoWait(t *testing.T) {
	withRotateBackoff(t, 30*time.Second)
	start := time.Now()
	if !rotateBackoffKind(3, context.Background(), upstream.ErrSoftRate) {
		t.Fatal("429 不退避分支在 ctx 未取消时应返回 true")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("429 分支不得等待：elapsed=%v", elapsed)
	}
	// 其余分类仍退避（用 0 基数放行，避免真睡 30s）。
	withRotateBackoff(t, 0)
	if !rotateBackoffKind(3, context.Background(), upstream.ErrWafBlock) {
		t.Fatal("WAF 分支在 ctx 未取消时应返回 true")
	}
}
