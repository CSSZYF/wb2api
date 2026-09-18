package scheduler

// wakeup_grace_test.go issue #152 迟到唤醒补跑派发前网络宽限（TDD RED，先于实现提交）。
//
// 场景：机器睡眠跨过槽位时刻，唤醒瞬间 timer 到期补跑（Run 主循环既有增值设计），
// 但 Windows Modern Standby exit 后网络栈/DNS 需 1-2s 才就绪——零宽限立即派发
// 等于把唯一一次补跑机会打在注定失败的窗口里（issue 实测 dial tcp lookup no
// such host 与 Kernel-Power 507 standby exit ≤1s 重合）。
//
// 本文件断言修复后的行为（Scheduler.awaitWakeupGrace 为可测面）：
//  1. 准点触发（含毫秒级 timer 抖动，<1s）零延迟放行，不被误宽限；
//  2. 迟到补跑（now 晚于槽位计划时刻 >1s）先等满宽限再放行（派发前宽限）；
//  3. 宽限等待可被 ctx 取消立即中断（优雅停机不被 5s 阻塞）；
//  4. 宽限缺省值锁 5s（生产语义防漂移）+ 实例字段零值/负值回落缺省。
//
// fix/200e-race：宽限从包级 var 改为 Scheduler 实例字段（wakeupGraceDelay，
// 零值回落 wakeupGraceDefault）——包级 var 被测试改写与 Run goroutine 的读构成
// 真实数据竞争，实例字段让测试只改自己构造的 Scheduler，不碰全局。
// 测试经 New(Config{}) 构造后直接改实例字段（同包内可写，与既有测试直接
// 构造 &Scheduler{...} 同口径）；注入发生在 awaitWakeupGrace/Run 的读之前，
// 同 goroutine 或有 goroutine 创建建立的 happens-before，无同步负担。

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAwaitWakeupGraceOnTimeZeroDelay 准点触发：槽位即当前时刻（毫秒级抖动）
// 直接放行，不引入任何宽限延迟。
func TestAwaitWakeupGraceOnTimeZeroDelay(t *testing.T) {
	s := New(Config{})
	start := time.Now()
	if !s.awaitWakeupGrace(context.Background(), time.Now()) {
		t.Fatal("准点触发应放行（true）")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("准点触发不应宽限等待，耗时 %v", e)
	}
}

// TestAwaitWakeupGraceSubThresholdJitterNotLate 迟到阈值内的毫秒级抖动
// （timer 正常触发的偏移 <1s）不算迟到补跑：零延迟放行，不被误宽限。
func TestAwaitWakeupGraceSubThresholdJitterNotLate(t *testing.T) {
	// 即便宽限被放大，sub-threshold 抖动也不应等待（判定先于等待）。
	a := New(Config{})
	a.wakeupGraceDelay = 2 * time.Second

	start := time.Now()
	// 500ms 早于 1s 阈值：timer 正常触发的抖动量级。
	planned := time.Now().Add(-500 * time.Millisecond)
	if !a.awaitWakeupGrace(context.Background(), planned) {
		t.Fatal("阈值内抖动应放行（true）")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("阈值内抖动不应宽限等待，耗时 %v", e)
	}
}

// TestAwaitWakeupGraceLateWaitsFullGrace 迟到补跑：now 晚于槽位计划时刻 >1s
// → 派发前等满宽限（等待时长 ≥ 宽限值；断言留 20ms 测量余量防 CI 抖动误报）。
func TestAwaitWakeupGraceLateWaitsFullGrace(t *testing.T) {
	s := New(Config{})
	s.wakeupGraceDelay = 120 * time.Millisecond

	planned := time.Now().Add(-30 * time.Second) // 唤醒补跑：槽位过点 30s
	start := time.Now()
	if !s.awaitWakeupGrace(context.Background(), planned) {
		t.Fatal("迟到补跑等满宽限后应放行（true）")
	}
	if e := time.Since(start); e < 100*time.Millisecond {
		t.Errorf("迟到补跑应先等满宽限（~120ms）再放行，实际 %v", e)
	}
}

// TestAwaitWakeupGraceCancelledDuringGrace 宽限等待中取消 ctx：立即返回 false
// （优雅停机不等 5s 宽限睡满）。
func TestAwaitWakeupGraceCancelledDuringGrace(t *testing.T) {
	s := New(Config{})
	s.wakeupGraceDelay = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	planned := time.Now().Add(-30 * time.Second)
	start := time.Now()
	if s.awaitWakeupGrace(ctx, planned) {
		t.Fatal("宽限中取消 ctx 应返回 false（放弃本批）")
	}
	if e := time.Since(start); e > 1*time.Second {
		t.Errorf("取消后应快速返回（远小于 2s 宽限），实际 %v", e)
	}
}

// TestAwaitWakeupGraceDefault5s 宽限缺省值锁 5s（生产语义：覆盖 Modern Standby
// exit 后 1-2s DNS 恢复窗口；防漂移），且实例字段未注入（零值/负值）时回落缺省。
func TestAwaitWakeupGraceDefault5s(t *testing.T) {
	if wakeupGraceDefault != 5*time.Second {
		t.Errorf("wakeupGraceDefault=%v want 5s（生产缺省）", wakeupGraceDefault)
	}
	if got := New(Config{}).wakeupGrace(); got != 5*time.Second {
		t.Errorf("未注入时 wakeupGrace()=%v want 5s（缺省回落）", got)
	}
	// 显式零值/负值（面板/未来注入误传）同样回落缺省，不出现"零宽限"意外。
	for _, v := range []time.Duration{0, -time.Second} {
		s := New(Config{})
		s.wakeupGraceDelay = v
		if got := s.wakeupGrace(); got != 5*time.Second {
			t.Errorf("wakeupGraceDelay=%v 时 wakeupGrace()=%v want 5s（回落缺省）", v, got)
		}
	}
}

// TestWakeupGracePerInstanceIsolation 宽限是实例字段：改一个 Scheduler 不影响
// 其他实例与缺省（包级 var 时代的全局串扰回归防线）。并发跑一遍读取路径，
// -race 下验证不再有共享写。
func TestWakeupGracePerInstanceIsolation(t *testing.T) {
	a := New(Config{})
	a.wakeupGraceDelay = 10 * time.Millisecond
	b := New(Config{})
	b.wakeupGraceDelay = 200 * time.Millisecond
	c := New(Config{}) // 未注入 → 缺省 5s

	if a.wakeupGrace() != 10*time.Millisecond || b.wakeupGrace() != 200*time.Millisecond {
		t.Fatalf("实例宽限串扰：a=%v b=%v", a.wakeupGrace(), b.wakeupGrace())
	}
	if c.wakeupGrace() != wakeupGraceDefault {
		t.Fatalf("未注入实例宽限=%v want %v（缺省）", c.wakeupGrace(), wakeupGraceDefault)
	}

	// 并发：各 goroutine 只碰自己的 Scheduler（迟到补跑走完整宽限等待路径）。
	var wg sync.WaitGroup
	for _, s := range []*Scheduler{a, b, c} {
		wg.Add(1)
		go func(s *Scheduler) {
			defer wg.Done()
			planned := time.Now().Add(-30 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			s.awaitWakeupGrace(ctx, planned) // 注入值小的两例立即取消返回；超时分支只验竞态
		}(s)
	}
	wg.Wait()
}
