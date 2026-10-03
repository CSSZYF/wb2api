// scheduler_wait_test.go 钉住 waitSlot 的三态与墙钟口径：timer 的等待基于单调
// 时钟，机器睡眠会冻结它——分段等待 + 每段墙钟重判把冻结的影响限制在一段之内
// （「睡眠不足整个等待周期时 fire 被顺延、墙钟时点被错过且不补跑」的正面修复）。
package scheduler

import (
	"context"
	"testing"
	"time"
)

func newTestScheduler() *Scheduler {
	s := New(Config{})
	return s
}

// TestWaitSlotFiresAtWallclock 到点返回 slotFired：
//   - next 已过（墙钟已越过时点、timer 尚未设置的形态）→ 立即返回，不等下一段；
//   - next 未到（多段等待）→ 到点才返回，且不早于计划时刻。
func TestWaitSlotFiresAtWallclock(t *testing.T) {
	s := newTestScheduler()

	// 墙钟已越过的形态：修复目标——睡眠冻结 timer 后醒来第一件事就是发现已到点。
	past := time.Now().Add(-time.Second)
	if got := s.waitSlot(context.Background(), past, time.Hour); got != slotFired {
		t.Fatalf("已越过时点应立即 slotFired，got %v", got)
	}

	// 未来时点：多段等待到点。
	next := time.Now().Add(45 * time.Millisecond)
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotFired {
		t.Fatalf("到点应 slotFired，got %v", got)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("提前返回：等待 %v，未到计划时刻", time.Since(start))
	}
}

// TestWaitSlotFiresWithinOneStepAfterWallclockJump 模拟「睡不足整个等待周期」：
// 睡眠把单调时钟冻结、墙钟却已越过时点，醒来后第一段末尾必须发现到点并立即补跑
// （偏差上限 = 段长，而非「剩余等待时长」）。这里用「计划时点前启动、多段短睡」
// 等价刻画同一性质：每段醒来都以 time.Until(next) 重判，最后一段 = 剩余量，
// 到点即 fire，不会被凑整到整段边界之后。
func TestWaitSlotFiresWithinOneStepAfterWallclockJump(t *testing.T) {
	s := newTestScheduler()
	step := 15 * time.Millisecond
	next := time.Now().Add(60 * time.Millisecond)
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, step); got != slotFired {
		t.Fatalf("到点应 slotFired，got %v", got)
	}
	elapsed := time.Since(start)
	if elapsed < 55*time.Millisecond {
		t.Fatalf("提前 fire：%v，早于计划时刻 %v", elapsed, 60*time.Millisecond)
	}
	// 迟到的上界 = 最后一段（≤step）+ timer 抖动；远小于「剩余等待时长被整体顺延」
	// 的量级（若不重判墙钟，冻结后 fire 会被顺延睡眠时长）。
	if elapsed > 60*time.Millisecond+8*step {
		t.Errorf("迟到超出段长上界：elapsed=%v next=+60ms step=%v", elapsed, step)
	}

	// 直接刻画「醒来时墙钟已过点」：把 next 造在过去（睡眠结束时 waitSlot 看到的
	// 就是这一形态），必须零等待 slotFired——补跑机会不被顺延。
	woke := time.Now().Add(-time.Millisecond)
	start2 := time.Now()
	if got := s.waitSlot(context.Background(), woke, step); got != slotFired {
		t.Fatalf("醒来发现已过点应 slotFired，got %v", got)
	}
	if d := time.Since(start2); d > step {
		t.Errorf("已过点仍等待了 %v（应零等待）", d)
	}
}

// TestWaitSlotRearm 等待中收到 rearmSchedule（在线改配置重排）→ 立即 slotRearm，
// 不等满剩余时长（段长不影响 rearm 响应性）。
func TestWaitSlotRearm(t *testing.T) {
	s := newTestScheduler()
	next := time.Now().Add(2 * time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		s.rearmSchedule <- struct{}{}
	}()
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotRearm {
		t.Fatalf("rearm 应返回 slotRearm，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("rearm 响应过慢：%v", time.Since(start))
	}
}

// TestWaitSlotCancel ctx 取消 → slotCancel，优雅退出。
func TestWaitSlotCancel(t *testing.T) {
	s := newTestScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	next := time.Now().Add(2 * time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if got := s.waitSlot(ctx, next, 10*time.Millisecond); got != slotCancel {
		t.Fatalf("取消应返回 slotCancel，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("取消响应过慢：%v", time.Since(start))
	}
}
