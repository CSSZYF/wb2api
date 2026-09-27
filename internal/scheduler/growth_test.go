package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestNextWakeGrowthSlot growth 排程进候选 + 禁用退场（每日自动执行成长任务队列）。
// 上游 74c324a：Sequential 族任务每日零点解锁一环，此前只能手动扫描推进；
// 新增 growth 时点（默认 01:00，避开零点整的解锁竞态）让链条每天自动走一环。
func TestNextWakeGrowthSlot(t *testing.T) {
	s := New(Config{GrowthHours: []int{1}})
	at, kinds := s.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	hasGrowth := false
	for _, k := range kinds {
		if k == taskGrowth {
			hasGrowth = true
		}
	}
	if !hasGrowth || at.Hour() != 1 || at.Day() != 27 {
		t.Fatalf("growth 槽位: at=%v kinds=%v（期望 09-27 01:00 含 taskGrowth）", at, kinds)
	}
	// 禁用后不进候选（其余 kind 为空 → nextWake 零值返回）
	s2 := New(Config{GrowthHours: []int{1}, GrowthDisabled: true})
	_, kinds2 := s2.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	for _, k := range kinds2 {
		if k == taskGrowth {
			t.Fatal("禁用后 growth 仍在候选")
		}
	}
}

// TestNextWakeGrowthDefaultHour 未配 hours 时回落 01:00（New 的缺省语义，与其余
// 任务族一致：零值 Config 即启用）。01:00 而非 00:00 是刻意选择——零点整上游
// 正在做解锁状态流转，整点扫描易撞上「刚解锁但列表还没刷新」的竞态。
func TestNextWakeGrowthDefaultHour(t *testing.T) {
	s := New(Config{})
	at, kinds := s.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	if at.Hour() != 1 {
		t.Fatalf("默认 growth 时点 = %v，期望 01:00", at)
	}
	hasGrowth := false
	for _, k := range kinds {
		if k == taskGrowth {
			hasGrowth = true
		}
	}
	if !hasGrowth {
		t.Fatalf("默认配置应含 taskGrowth，kinds=%v", kinds)
	}
}

// TestSetGrowthHookFiresOnDispatch 到点时回调必须被调用（panel 在 scheduler 之后
// 构造，用 SetGrowthHook 事后挂载；调度器只管时点不管实现）。
// 同时覆盖 nil 未挂载时安全跳过（不 panic）。
func TestSetGrowthHookFiresOnDispatch(t *testing.T) {
	s := New(Config{GrowthHours: []int{1}})

	// 未挂载：dispatch 不得 panic（GrowthHook nil 时到点跳过）。
	s.dispatch(context.Background(), taskGrowth)

	var mu sync.Mutex
	fired := 0
	s.SetGrowthHook(func() {
		mu.Lock()
		fired++
		mu.Unlock()
	})
	s.dispatch(context.Background(), taskGrowth)

	mu.Lock()
	got := fired
	mu.Unlock()
	if got != 1 {
		t.Fatalf("growth hook 调用次数 = %d，期望 1", got)
	}
}

// TestReconfigureGrowthHotApplies 面板保存配置后 Reconfigure 必须让新时点/开关
// 立即生效（growth 与其余任务族同款热改语义）。
func TestReconfigureGrowthHotApplies(t *testing.T) {
	s := New(Config{GrowthHours: []int{1}})
	// 其余任务族显式禁用，只留 growth，断言才不会被别的槽位干扰。
	s.Reconfigure(nil, nil, nil, nil, nil, []int{5}, true, true, true, true, true, false)
	at, kinds := s.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	if at.Hour() != 5 {
		t.Fatalf("热改后 growth 时点 = %v，期望 05:00", at)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Fatalf("热改后候选 = %v，期望只含 taskGrowth", kinds)
	}
	// 开关热改：再关掉 growth → 无任何候选。
	s.Reconfigure(nil, nil, nil, nil, nil, nil, true, true, true, true, true, true)
	if at, kinds := s.nextWake(time.Now()); !at.IsZero() || len(kinds) != 0 {
		t.Fatalf("growth 关闭后仍有候选: at=%v kinds=%v", at, kinds)
	}
}
