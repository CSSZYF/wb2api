// backoff.go 轮转退避与抖动的单一事实来源（WAF 403 修复 P0-2/P0-1 共用）：
// 指数基数/封顶/抖动比例一处定义，chatCompletions 轮转退避与 WAF 软冷却基数
// （handler 取 soft_rate 基数再抖动）共用同一 jitterDur，不在 handler 与 upstream 各写一份。
package server

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rotateBackoffBase 轮转退避基数（对齐官方 intl CLI computeRequestRetryDelayMs
// 的 500ms 形态）。测试可置 0 跳过等待（TestMain 已置 0 加速轮转测试；
// 退避界断言测试用 withRotateBackoff 临时恢复）。
var rotateBackoffBase = 500 * time.Millisecond

const (
	// rotateBackoffCap 轮转退避封顶（建议 8s：轮转上限默认 3 次，实际等待序列
	// 500ms/1s，封顶只约束极端配置下的 MaxRotate）。
	rotateBackoffCap = 8 * time.Second
	// jitterFraction 抖动比例（±25%，对齐 intl CLI delay×(1±0.25) 形态）。
	jitterFraction = 0.25
)

// jitterDur 给时长施加 ±jitterFraction 的均匀抖动，返回 [d·(1-f), d·(1+f)] 区间值。
// d<=0 原样返回（零等待不抖动）。抖动目的是打散多请求同相位重试（WAF 频控按
// 密度判罚，齐步走的退避会以固定周期再次聚团）。
func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// backoffAfter 返回第 n 次轮转（0 基：首次失败换号前 n=0）前应等待的退避时长：
// base·2^n 封顶 rotateBackoffCap，再施加 ±25% 抖动。base 置 0（测试）时恒 0。
// 用逐次翻倍而非位移：base 调整后无需同步维护移位上限，溢出由封顶比较兜底。
func backoffAfter(n int) time.Duration {
	d := rotateBackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < rotateBackoffCap; k++ {
		d *= 2
		if d <= 0 { // 翻倍溢出成非正数：直接按封顶处理
			return jitterDur(rotateBackoffCap)
		}
	}
	if d > rotateBackoffCap {
		d = rotateBackoffCap
	}
	return jitterDur(d)
}

// backoffWorthwhile 报告「按该错误分类换号时是否值得先退避」——429 首字延迟修复的
// 单一判据（rotateBackoffKind 消费）。
//
// 分野依据（为什么只放行 ErrSoftRate）：
//   - ErrSoftRate（429 限流）：换号切到的是**另一个账号**，而上游频控按账号计
//     （6004/限流文案里的重置时刻是那个账号自己的窗口）。等 500ms·2^i 再打另一个
//     账号，既不改善它被限流的可能，也纯白等——用户实测的诉求就是「收到 429 立即
//     换下一个号」，白等一秒的首字延迟比 429 本身更伤（本项目明确要求首字延迟足够低）。
//     安全性：轮转次数仍由 MaxRotate 封顶，且 tried 保证每次换的是**不同**账号，
//     故不退避不会造成对同一账号的密集重打（最坏 MaxRotate 次、每次一个新号）。
//   - ErrWafBlock（403 WAF）：**必须保留退避**——它是 IP/指纹级频控，同一出口 IP 上
//     换任何账号都落在同一风控面（WAF 403 修复 P0-2 引入退避正是为此）。退避让频控
//     窗口滑过，是那条修复的核心机制，不得被本项削弱。
//   - 其余分类（ErrServer 5xx / ErrClient / ErrHardCredit 402 / ErrNotFound /
//     ErrModelBlocked / ErrAccountFault…）：一律保持既有退避。它们的失败可能与上游
//     状态或链路相关，退避仍是「换号前歇一下」的既有语义；本项刻意只动 429 一条
//     路径，不做"顺手放宽"（每个分类放宽都要各自的实测依据）。
func backoffWorthwhile(kind upstream.ErrKind) bool {
	return kind != upstream.ErrSoftRate
}

// rotateBackoffKind 按错误分类决定是否退避后再换号（分类错误轮转路径专用）：
//   - ErrSoftRate → **不退避**（立即换下一个号，首字延迟优先；理由见 backoffWorthwhile）；
//   - 其余分类（含 ErrWafBlock）→ 既有 rotateBackoff（指数退避 + 抖动）。
//
// 不退避分支仍要求 ctx 未取消：调用方「false 即终止轮转」的语义保持单一（客户端
// 已断连时不换号、不打上游），与 sleepCtx(ctx, 0) 的契约一致。
//
// 为什么不做成「直接让 backoffAfter 返回 0」：backoffAfter 是轮转退避的单一事实来源，
// WAF 路径（以及 Acquire 抢名额失败、refresh 失败、传输层错误三条轮转路径）都依赖它；
// 在分类入口按 kind 分流，改动面精确到「429 这一条路径」，其余路径逐字节不变。
func rotateBackoffKind(i int, ctx context.Context, kind upstream.ErrKind) bool {
	if !backoffWorthwhile(kind) {
		return ctx.Err() == nil
	}
	return rotateBackoff(i, ctx)
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（客户端断连/优雅停机不必等退避
// 睡醒），等满返回 true。d<=0 立即放行（此时仍要求 ctx 未取消，保持调用方
// 「false 即终止轮转」的单一语义）。与 scheduler.sleepCtx 同模式（该函数未导出
// 且 scheduler 不宜被 server 反向依赖，按等价物口径在消费侧实现）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
