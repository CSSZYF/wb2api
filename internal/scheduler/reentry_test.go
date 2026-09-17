package scheduler

// reentry_test.go 巡检重入锁（hub 版 wb_scheduler 的 _run_lock 同语义）：
// 五类任务的公开入口（RunCheckinNow/RunTravelNow/RunActivityNow/RunKeepaliveNow/
// RunBlackcatNow）与定时批量 runBatch 互斥——同一时刻两趟全量巡检并发会对上游
// 重复轰炸（手动触发撞上定时是现实场景）。
//
// 语义：
//  1. 锁被占用时入口立即返回、不发任何上游调用（不阻塞调用方）；
//  2. 锁释放后入口照常执行；
//  3. runBatch 整批期间持锁，批内并行派发不自锁（同槽多类任务必须都能跑）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunEntryPointsSkipWhenBusy 手动持锁模拟「已有巡检在执行」：五个入口
// 全部立即返回且不发起任何上游调用（池内账号存在，若不跳过一次签到/保活必发请求）。
func TestRunEntryPointsSkipWhenBusy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	if !s.beginRun("test") {
		t.Fatal("前置条件失败：测试未拿到重入锁")
	}
	defer s.endRun()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunCheckinNow()
		s.RunTravelNow()
		s.RunActivityNow()
		s.RunKeepaliveNow()
		s.RunBlackcatNow()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("锁被占用时入口应立即返回（不阻塞调用方）")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("upstream calls=%d want 0（已有巡检在执行时不应发起任何调用）", n)
	}
}

// TestRunEntryPointProceedsAfterRelease 锁释放后入口照常执行（锁只是互斥，
// 不是永久跳过）：签到入口在空池上跑完即返回。
func TestRunEntryPointProceedsAfterRelease(t *testing.T) {
	p := pool.New("")
	up := &upstream.Client{}
	s := New(Config{Pool: p, Upstream: up})

	if !s.beginRun("test") {
		t.Fatal("前置条件失败：测试未拿到重入锁")
	}
	s.endRun()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunCheckinNow() // 空池：遍历 0 账号即返回
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("锁释放后入口应照常执行并返回")
	}
	if !s.runningMu.TryLock() {
		t.Fatal("入口返回后锁应已释放")
	}
	s.runningMu.Unlock()
}

// TestRunBatchDoesNotSelfLock 整批期间持锁，但批内各任务走不带锁的 run* 内部
// 函数——同槽多类任务并行派发不会被自己跳过（runBatch 返回后锁已释放）。
func TestRunBatchDoesNotSelfLock(t *testing.T) {
	fastActivity(t)
	fastTravel(t)

	p := pool.New("")
	up := &upstream.Client{}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 空池：五类任务都是遍历 0 账号即返回，但仍走完整派发路径。
		s.runBatch(ctx, []taskKind{taskCheckin, taskTravel, taskActivity, taskKeepalive, taskBlackcat})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runBatch 未在 3s 内完成（批内任务被重入锁自锁？）")
	}
	if !s.runningMu.TryLock() {
		t.Fatal("runBatch 返回后锁应已释放")
	}
	s.runningMu.Unlock()
}
