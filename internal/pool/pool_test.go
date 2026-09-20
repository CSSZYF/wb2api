package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// withNoPickGap 临时关闭防并发撞号窗口（minPickGap=0），让纯加权分布测试不受影响。
func withNoPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })
}

func TestPickHighestCredits(t *testing.T) {
	withNoPickGap(t)
	// 三因子加权（credits 比例×10 + 闲置 + 成功率）：积分悬殊时高积分账号应被多数选中，
	// 但不再像纯 credits 加权那样接近 99%（闲置补偿 + 成功率中性 1.5 拉平了基线）。
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50000, 0)
	p.SetCredits("u3", 300, 0)
	counts := map[string]int{}
	for i := 0; i < 3000; i++ {
		counts[p.Pick().UID]++
	}
	if counts["u2"] <= counts["u1"] || counts["u2"] <= counts["u3"] {
		t.Errorf("u2 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100, 0)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllDisabled(t *testing.T) {
	// 全禁用 → 兜底不参与（禁用账号永不参与兜底）→ 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil (all disabled), got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcludingStaysWithinHealthy(t *testing.T) {
	withNoPickGap(t)
	// 加权随机不能选出冷却/禁用账号。
	p := New("")
	p.Add(&auth.Auth{UID: "u-cold"})
	p.Add(&auth.Auth{UID: "u-hot"})
	p.SetCredits("u-cold", 9999, 0)
	p.SetCredits("u-hot", 1, 0)
	p.Cooldown("u-cold", CoolHard, time.Hour, "x")
	for i := 0; i < 20; i++ {
		got := p.PickExcluding(nil)
		if got == nil || got.UID != "u-hot" {
			t.Fatalf("iter %d: picked %+v, want only healthy u-hot", i, got)
		}
	}
}

func TestPickWeightedSkewTowardHighCredits(t *testing.T) {
	withNoPickGap(t)
	// Top5 三因子加权：单账号 credits 占比足够高时，多数挑中它。
	p := New("")
	for _, u := range []string{"w1", "w2", "w3", "w4", "w5", "w6"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 1, 0)
	}
	p.SetCredits("w1", 1000, 0)
	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		counts[p.Pick().UID]++
	}
	mx, mxUID := 0, ""
	for uid, n := range counts {
		if n > mx {
			mx, mxUID = n, uid
		}
	}
	if mxUID != "w1" {
		t.Errorf("w1 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickWeightedUniformWhenAllZero(t *testing.T) {
	withNoPickGap(t)
	// credits 全为 0 → 退化为均匀随机，不能只挑固定一个。
	p := New("")
	for _, u := range []string{"z1", "z2", "z3"} {
		p.Add(&auth.Auth{UID: u})
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[p.Pick().UID] = true
	}
	if len(seen) != 3 {
		t.Errorf("uniform fallback should hit all, seen=%v", seen)
	}
}

func TestPickWeightedTopFiveOnly(t *testing.T) {
	withNoPickGap(t)
	// 第 6 高 credits 的账号在 Top5 之外，权重抽签永远轮不到它。
	p := New("")
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a1", 1000, 0)
	p.SetCredits("a2", 1000, 0)
	p.SetCredits("a3", 1000, 0)
	p.SetCredits("a4", 1000, 0)
	p.SetCredits("a5", 1000, 0)
	p.SetCredits("a6", 5, 0) // Top5 之外
	for i := 0; i < 2000; i++ {
		if got := p.Pick(); got == nil || got.UID == "a6" {
			t.Fatalf("iter %d: picked %+v, a6 must stay outside top-5", i, got)
		}
	}
}

func TestPickTopFiveBySuccessRateNotCredits(t *testing.T) {
	withNoPickGap(t)
	// C1 回归：top5 短名单必须按三因子权重（含成功率）而非纯 credits 截断。
	// a1..a5 credits=100 但成功率极低（1/100），a6 credits=90 但成功率 100%。
	// 纯 credits 排序时 a6（90 < 100）是第 6 名，永远进不了 top5；
	// 三因子权重下 a6 权重最高，首轮必被选中。仅断言首轮（后续 a6 闲置补偿衰减会合法发散）。
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 选权重最高的候选
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100, 0)
		for i := 0; i < 99; i++ {
			p.NoteError(u) // 成功率 1/(1+99)≈0.03
		}
		p.NoteSuccess(u)
	}
	p.Add(&auth.Auth{UID: "a6"})
	p.SetCredits("a6", 90, 0)
	p.NoteSuccess("a6") // 成功率 100%

	if got := p.Pick(); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (high-success low-credit must enter top5 by weight)", got)
	}
}

func TestPickTopFiveByIdleNotCredits(t *testing.T) {
	withNoPickGap(t)
	// C1 回归：闲置补偿同样影响短名单。a1..a5 credits=100 但刚被用过（闲置 0），
	// a6 credits=90 但从未使用（闲置满分）。纯 credits 排序时 a6 进不了 top5；
	// 三因子权重下 a6 权重最高，首轮必被选中。仅断言首轮。
	p := New("")
	now := time.Now()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100, 0)
	}
	p.Add(&auth.Auth{UID: "a6"})
	p.SetCredits("a6", 90, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	// a1..a5 全部"刚被用过"，闲置补偿归零；a6 从未使用 → 闲置满分。
	p.mu.Lock()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.byUID[u].lastUsed = now
	}
	p.mu.Unlock()

	if got := p.Pick(); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (idle low-credit must enter top5 by weight)", got)
	}
}

func TestPickDeterministicViaSetRandomSource(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	// r=0 ∈ [0,50) → 命中 u1。注入源应使选号完全确定。
	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("iter %d: pick=%+v want u1 (deterministic)", i, got)
		}
	}
}

func TestPickAntiThunderingHerd(t *testing.T) {
	// 100 goroutine 同时 Pick：防并发撞号窗口内同一账号不应被重复选中。
	// credits 相同 → 无注入源时加权随机应天然打散；为保证稳定，全部置 0 走均匀随机。
	//
	// minPickGap 置 0（源码注释标注的测试开关）：Windows 时钟粒度粗，整轮并发
	// Pick 可落在同一时钟刻度内——所有 lastUsed 时间戳相等，eligible 恒空走 LRU
	// 兜底，而 LRU 对相等时间戳按 UID 稳定 tie-break，结果 100 次全命中 c00。
	// 关闭窗口后本用例回归其真实断言口径：加权随机自身的打散性（跨平台稳定）。
	oldGap := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = oldGap })

	p := New("")
	for i := 0; i < 10; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("c%02d", i)})
	}
	// 关键：验证并发中任意瞬间不会全选同一账号。
	const N = 100
	var wg sync.WaitGroup
	picked := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if a := p.Pick(); a != nil {
				picked[idx] = a.UID
			}
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, uid := range picked {
		if uid != "" {
			counts[uid]++
		}
	}
	// 选号必须覆盖多个账号，且最热门的账号不超过一半。
	if len(counts) < 2 {
		t.Fatalf("anti-thundering-herd failed: all %d picks hit %d account(s) %v", N, len(counts), counts)
	}
	for uid, n := range counts {
		if n > N/2 {
			t.Errorf("account %s picked %d/%d (>50%%): thundering herd", uid, n, N)
		}
	}
}

func TestPickLRUFallbackWhenTopAllRecentlyUsed(t *testing.T) {
	// top5 全部刚被选中 → LRU 兜底应挑最近最少使用的那个（= 最早 lastUsed）。
	old := minPickGap
	minPickGap = time.Hour // 超大窗口：任何 lastUsed 都在窗口内
	defer func() { minPickGap = old }()

	p := New("")
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("a%d", i)})
	}
	// 直接构造 lastUsed：不经过 Pick（避免 Pick 改写 lastUsed）。
	order := []string{"a4", "a3", "a2", "a1", "a0"}
	p.mu.Lock()
	for i, uid := range order {
		p.byUID[uid].lastUsed = time.Now().Add(-time.Duration(len(order)-i) * time.Second) // a4 最旧
	}
	p.mu.Unlock()

	got := p.Pick()
	if got == nil {
		t.Fatal("pick returned nil")
	}
	if got.UID != "a4" {
		t.Errorf("LRU fallback picked %s want a4 (oldest lastUsed)", got.UID)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush() // 状态变更走 dirty 标志，落盘由 Flush / 后台 goroutine 负责
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling || st.Reason != "余额不足" {
		t.Fatalf("cooldown lost after reload: %+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

// TestReenableIfCredits 硬冷却（CoolHard）账号余额恢复即解冻（issue #199 收窄后
// 仍保留的唯一自动解冻路径：硬冷却的恢复条件正是余额恢复）。
func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500, 0)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

// TestReenableIfCreditsKeepsSoftCooldown issue #199 语义收窄：软冷却（CoolSoft）
// 账号经余额刷新/签到（ReenableIfCredits）**不得**被解冻——余额恢复不证明限流解除，
// 旧实现无条件解冻会让软冷却账号被刷新解冻 → 选号重新选中 → 又撞 429 的循环。
// credits 照常更新（观测量新鲜），冷却域（until/coolKind/modelCooldowns/softStreak）
// 原样保留。
func TestReenableIfCreditsKeepsSoftCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 账号级软冷却 + 6004 模型级冷却（含 softStreak 累计）。
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")

	p.ReenableIfCredits("u1", 500, 0)

	st, _ := p.Status("u1")
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500（余额照常更新，只收窄解冻）", st.Credits)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("软冷却不得被余额刷新解冻: %+v", st)
	}
	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if until.IsZero() || kind != CoolSoft || reason == "" || streak == 0 || mc != 1 {
		t.Errorf("冷却域应原样保留：until=%v kind=%v reason=%q streak=%d modelCooldowns=%d",
			until, kind, reason, streak, mc)
	}
	// 行为断言：该号在状态机口径下仍不可用（注意不能用 Pick()==nil——全冷却兜底
	// pickEarliestExpiryLocked 允许软冷却账号参与，这正是软冷却账号在池内仍会被
	// 兜底探测选中的既有语义）。
	if p.internalHealthy("u1") {
		t.Error("软冷却中的账号不应 healthy（未被余额刷新解冻）")
	}
}

// TestReenableIfCreditsKeepsModelOnlyCooldown 仅 6004 模型级冷却（账号级不 cooling）
// 的账号：余额刷新后该模型的独立冷却必须保留（切模型豁免语义不被刷新破坏）。
func TestReenableIfCreditsKeepsModelOnlyCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")

	p.ReenableIfCredits("u1", 500, 0)

	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	p.mu.RUnlock()
	if n != 1 {
		t.Fatalf("modelCooldowns=%d want 1（模型级软冷却不被余额刷新清）", n)
	}
	// 同模型仍被冷却，切模型仍豁免（与 issue #31 的模型独立性一致）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got != nil {
		t.Errorf("同模型请求不应选中被 6004 冷却的账号，got %+v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Errorf("切模型应豁免选中，got %+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

// TestReviveForcesSoftCooldownClear 人工强制解冻（面板「解冻」按钮 → Revive）不受
// issue #199 收窄影响：人工判断该号可用时一键清掉软冷却/模型级冷却/熔断（运维口径
// 无条件恢复）。这是软冷却的**唯一**人工出口（自动解冻只对硬冷却放行）。
func TestReviveForcesSoftCooldownClear(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断

	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true（账号存在）")
	}
	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 || reason != "" || streak != 0 || mc != 0 {
		t.Errorf("人工解冻应清冷却域：until=%v kind=%v reason=%q streak=%d modelCooldowns=%d",
			until, kind, reason, streak, mc)
	}
	if bt, _ := p.breakerUntil("u1"); !bt.IsZero() {
		t.Errorf("人工解冻应清熔断运行态：breakerUntil=%v", bt)
	}
	if !p.internalHealthy("u1") {
		t.Error("人工解冻后账号应回到可选状态")
	}
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Errorf("人工解冻后同模型请求也应可选，got %+v", got)
	}
}

func TestNoteErrorAccumulatesErrTotal(t *testing.T) {
	// NoteError 语义变更：不再有独立的 err 冷却（CoolErr 已并入熔断器），
	// 只累计 errTotal（不清零，供成功率权重）并喂熔断器 fails。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("NoteError alone must not set cooling (no CoolErr): %+v", st)
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err not set")
	}
}

func TestNoteSuccessResetsBreakerNotErrTotal(t *testing.T) {
	// NoteSuccess 清 fails/熔断（运行态），但不清 errTotal（累计值）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1") // 触发熔断
	if p.internalHealthy("u1") {
		t.Fatal("breaker should be open (unhealthy) after 2 failures")
	}
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2 (cumulative, not cleared by success)", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("success should clear breaker: %+v", st)
	}
}

func TestNoteSuccessIncrementsAndRecords(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.LastSuccessTime.Before(before) {
		t.Errorf("last_success=%v before call", st.LastSuccessTime)
	}
	if !st.LastErrTime.IsZero() {
		t.Errorf("last_err should be zero for fresh success: %v", st.LastErrTime)
	}
}

func TestReenableClearsCoolingNotBreaker(t *testing.T) {
	// C5：签到解冻只清冷却（until/coolKind/reason）+ 更新 credits，不清熔断
	// （fails/retryCount/breakerUntil）。签到成功只证明余额与 billing 通道恢复，
	// 不证明 chat 通道健康——熔断仍按 breakerUntil 退避到期或 NoteSuccess 恢复。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足") // 硬冷却（不再喂 fails：熔断只由 NoteError 驱动）
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断（fails→阈值1→fails=0, retryCount=1, breakerUntil 非零）
	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if st.Reason != "" || st.Credits != 500 {
		t.Errorf("signin should clear reason + set credits=500: %+v", st)
	}
	if st.Until != (time.Time{}) {
		t.Errorf("signin should clear hard-cooling until: %+v", st.Until)
	}
	if st.BreakerUntil.IsZero() {
		t.Fatal("signin must NOT clear breakerUntil (chat health unresolved)")
	}
	// 熔断仍在 → 账号仍不可选（直至 breakerUntil 到期）。
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestReenableKeepsBreaker(t *testing.T) {
	// C5 回归锁定新语义：仅熔断（无冷却）的账号，签到解冻不得清熔断。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	p.ReenableIfCredits("u1", 500, 0)
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("signin must not clear breakerUntil")
	}
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestCoolKindPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush()

	// 旧文件缺新字段时零值 → 冷却应仍工作（向后兼容）。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling {
		t.Fatalf("cooldown state lost after reload: %+v ok=%v", st, ok)
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind after reload=%q want hard_credit", st.CoolKind)
	}
}

func TestStateRoundTripExtendedFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.NoteSuccess("u1") // successCount=1，last_success 非零
	p.NoteSuccess("u1") // successCount=2
	p.NoteError("u1")   // errTotal=1（累计），last_err 非零
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// JSON tag 全小写下划线；err_total 落盘，err_count 不再落盘。
	for _, want := range []string{`"cool_kind"`, `"success_count"`, `"err_total"`, `"last_success"`, `"last_err"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"err_count"`) {
		t.Errorf("state.json should not write legacy err_count:\n%s", raw)
	}

	// 重载后字段保留
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.SuccessCount != 2 || st.CoolKind != "hard_credit" {
		t.Errorf("reloaded portrait=%+v", st)
	}
	if st.ErrTotal != 1 {
		t.Errorf("reloaded err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() || st.LastErrTime.IsZero() {
		t.Error("last_success/last_err lost after reload")
	}
}

// ---------------------------------------------------------------------------
// 池状态持久化三连（v1.9.10）：breaker/retryCount、creditsExpiring、sessionDeadFails
// 入 stateAccount；恢复侧统一惰性过滤过期项（与 modelCooldowns 同设计模式）。
// ---------------------------------------------------------------------------

// breakerRetryCount 曝露 entry.retryCount 供测试断言（包内私有 helper）。
func (p *Pool) breakerRetryCount(uid string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.retryCount, true
}

// creditsExpiringOf 曝露 entry.creditsExpiring 供测试断言（包内私有 helper）。
func (p *Pool) creditsExpiringOf(uid string) (int64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.creditsExpiring, true
}

// TestBreakerPersistRoundTrip 熔断器 breakerUntil + retryCount 已持久化：
// 落盘 → 重启 → 恢复。修复熔断期重启失忆：breakerUntil 在未来时重启后仍阻断选号，
// retryCount 保留"越熔越长"的退避累积。
func TestBreakerPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 触发两次熔断：retryCount=2，breakerUntil=+2h（1h*2^1，未触顶）。
	p.SetBreaker(1, time.Hour, 6*time.Hour)
	p.NoteError("u1") // 第 1 次熔断：retryCount=1，breakerUntil=+1h（fails 清零）
	p.NoteError("u1") // 第 2 次熔断：retryCount=2，breakerUntil=+2h
	p.Flush()

	btBefore, _ := p.breakerUntil("u1")
	rcBefore, _ := p.breakerRetryCount("u1")
	if btBefore.IsZero() || rcBefore != 2 {
		t.Fatalf("precondition: breakerUntil=%v retryCount=%d, want 非零/2", btBefore, rcBefore)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"breaker_until"`, `"retry_count"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}
	// fails 是短期计数：不持久化（重启归零，需重新累计到阈值）。
	if strings.Contains(string(raw), `"breaker_fails"`) {
		t.Errorf("fails 不应落盘（短期计数）:\n%s", raw)
	}

	// 重启：breakerUntil + retryCount 应保留，账号仍不可选。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	btAfter, _ := p2.breakerUntil("u1")
	rcAfter, _ := p2.breakerRetryCount("u1")
	if btAfter.IsZero() {
		t.Fatal("重启后 breakerUntil 丢失（熔断期未持久化）")
	}
	if d := btAfter.Sub(btBefore); d < -time.Second || d > time.Second {
		t.Errorf("恢复后 breakerUntil=%v want ~%v (diff %v)", btAfter, btBefore, d)
	}
	if rcAfter != rcBefore {
		t.Errorf("恢复后 retryCount=%d want %d（退避指数未持久化）", rcAfter, rcBefore)
	}
	if p2.internalHealthy("u1") {
		t.Fatal("重启后熔断中的账号应仍不可选（breakerUntil 未过期）")
	}
	if fails := p2.breakerFails("u1"); fails != 0 {
		t.Errorf("重启后 fails=%d want 0（短期计数不持久化）", fails)
	}
}

// TestBreakerPersistExpiryFilter 落盘与恢复都做过期过滤：breakerUntil 过期/零值时
// 不写 breaker_until + retry_count（过期退避无意义），恢复时归零（不保留无用退避指数）。
func TestBreakerPersistExpiryFilter(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 手写一个已过期的 breakerUntil + retryCount=3。
	p.mu.Lock()
	e := p.byUID["u1"]
	e.breakerUntil = time.Now().Add(-time.Hour)
	e.retryCount = 3
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "breaker_until") {
		t.Errorf("已过期的 breakerUntil 不应落盘:\n%s", raw)
	}
	if strings.Contains(string(raw), "retry_count") {
		t.Errorf("breakerUntil 过期时 retryCount 不应落盘:\n%s", raw)
	}

	// 重启：过期 → 归零。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if bt, _ := p2.breakerUntil("u1"); !bt.IsZero() {
		t.Errorf("过期的 breakerUntil 恢复后=%v want 零值", bt)
	}
	if rc, _ := p2.breakerRetryCount("u1"); rc != 0 {
		t.Errorf("breakerUntil 过期时 retryCount 应归零, got %d", rc)
	}
}

// TestCreditsExpiringPersistRoundTrip creditsExpiring 已持久化：落盘 → 重启 → 恢复。
// 修复第四因子（weightOf ×8）重启失忆：重启后到下次签到之间不应丢失快过期积分偏好。
func TestCreditsExpiringPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0, 500) // credits=1000, creditsExpiring=500
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"credits_expiring"`) {
		t.Errorf("state.json missing credits_expiring:\n%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	expiring, ok := p2.creditsExpiringOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if expiring != 500 {
		t.Errorf("恢复后 creditsExpiring=%d want 500", expiring)
	}
	// 选号第四因子用恢复的值：weightOf 应含 expiring 项。
	p2.mu.RLock()
	e := p2.byUID["u1"]
	wWith := p2.weightOf(e, 1000, time.Now())
	saved := e.creditsExpiring
	e.creditsExpiring = 0
	wWithout := p2.weightOf(e, 1000, time.Now())
	e.creditsExpiring = saved
	p2.mu.RUnlock()
	if wWith <= wWithout {
		t.Errorf("恢复的 creditsExpiring 应让权重更大: wWith=%.3f wWithout=%.3f", wWith, wWithout)
	}
}

// TestCreditsExpiringPersistOmitZero creditsExpiring=0 时落盘 omitempty 不写。
func TestCreditsExpiringPersistOmitZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0, 0) // creditsExpiring=0
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credits_expiring") {
		t.Errorf("creditsExpiring=0 时不应落盘:\n%s", raw)
	}
}

// TestCreditsExpiringPersistClamped 恢复时钳到 [0, credits]：state.json 是可被手工编辑/
// 旧版本写坏的输入，越界值会经 weightOf 的占比项（×8）放大成选号偏置（防脏数据 ×8 放大）。
func TestCreditsExpiringPersistClamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 手写脏数据：credits=100 但 credits_expiring=9999（越界）+ 另一个负值账号。
	dirty := `{"accounts":{
		"over":{"credits":100,"credits_expiring":9999},
		"neg":{"credits":100,"credits_expiring":-50},
		"ok":{"credits":100,"credits_expiring":80}
	}}`
	if err := os.WriteFile(fp, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	for _, uid := range []string{"over", "neg", "ok"} {
		p.Add(&auth.Auth{UID: uid})
	}
	if got, _ := p.creditsExpiringOf("over"); got != 100 {
		t.Errorf("越界 creditsExpiring=%d want 100（钳到 credits）", got)
	}
	if got, _ := p.creditsExpiringOf("neg"); got != 0 {
		t.Errorf("负值 creditsExpiring=%d want 0（钳到 0）", got)
	}
	if got, _ := p.creditsExpiringOf("ok"); got != 80 {
		t.Errorf("合法 creditsExpiring=%d want 80（不误伤）", got)
	}
	// 权重侧验证：钳制后的"越界号"与显式设成上限（=credits）的参照号权重应完全一致
	// （若越界值漏钳，占比项会从 1.0 被撑到 99.99 → 权重相差近百）。
	p.Add(&auth.Auth{UID: "ref"})
	p.SetCreditsDetailed("ref", 100, 0, 100)
	p.mu.RLock()
	wOver := p.weightOf(p.byUID["over"], 100, time.Now())
	wRef := p.weightOf(p.byUID["ref"], 100, time.Now())
	p.mu.RUnlock()
	if wOver != wRef {
		t.Errorf("钳制后权重 %.3f 应等于上限参照 %.3f（越界值被 ×8 放大）", wOver, wRef)
	}
}

// TestSessionDeadFailsPersistRoundTrip 连续 12153 计数已持久化：落盘 → 重启 → 恢复。
// 修复重启归零重学：上游持续 session dead 时不用再吃 2 次失败才禁用。
func TestSessionDeadFailsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // sessionDeadFails=2（未达阈值 3）
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "session_dead_fails") {
		t.Errorf("state.json missing session_dead_fails:\n%s", raw)
	}

	// 重启：连续计数应恢复；下次 12153 从恢复值继续累计，第 3 次即达阈值禁用。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if !p2.NoteSessionDead("u1") {
		t.Fatal("恢复计数=2 后第 3 次 12153 应禁用（从恢复值继续累计）")
	}
	if st, _ := p2.Status("u1"); !st.Disabled {
		t.Fatal("恢复后达阈应 disabled")
	}
}

// TestSessionDeadFailsPersistOmitZero sessionDeadFails=0 时落盘 omitempty 不写。
func TestSessionDeadFailsPersistOmitZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 制造一次 dirty（成功入账）让 Flush 真正写盘，sessionDeadFails 保持 0。
	p.NoteSuccess("u1")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "session_dead_fails") {
		t.Errorf("sessionDeadFails=0 时不应落盘:\n%s", raw)
	}
}

// TestSessionDeadFailsClearPersists 计数清零（refresh/chat 成功）同样落盘：
// 重启后不残留旧计数（NoteSessionDead/ClearSessionDead 两入口补标 dirty）。
func TestSessionDeadFailsClearPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // 模拟 refresh 成功：清计数
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.NoteSessionDead("u1") || p2.NoteSessionDead("u1") {
		t.Fatal("清零后前 2 次不应禁用")
	}
	if !p2.NoteSessionDead("u1") {
		t.Fatal("清零后第 3 次应禁用（重启后不残留旧计数）")
	}
}

// TestSessionDeadFailsDirtyOnIncrement 未达阈值的计数变更也要标 dirty：
// 旧实现只在达阈禁用（disableLocked）时置 dirty，计数 1→2 的变更永不触发落盘。
func TestSessionDeadFailsDirtyOnIncrement(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.dirty.Store(false) // 清掉 Add 可能的脏位，只看计数入口
	p.NoteSessionDead("u1")
	if !p.dirty.Load() {
		t.Error("NoteSessionDead 累计计数应标 dirty（否则计数变更不会落盘）")
	}
	p.dirty.Store(false)
	p.ClearSessionDead("u1")
	if !p.dirty.Load() {
		t.Error("ClearSessionDead 清零应标 dirty（否则重启后残留旧计数）")
	}
	// 计数已为 0 时清零是空操作，不应无谓置脏。
	p.dirty.Store(false)
	p.ClearSessionDead("u1")
	if p.dirty.Load() {
		t.Error("sessionDeadFails 已为 0 时 ClearSessionDead 不应置 dirty")
	}
}

// TestLegacyStateFileLoadsNewFieldsAsZero 旧 state.json（无 v1.9.10 新字段）兼容加载：
// 5 个新字段全部零值，不报错、不影响既有字段；随后正常运行可重新写入。
func TestLegacyStateFileLoadsNewFieldsAsZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":123,"until":"2099-01-01T04:00:00+08:00","cool_kind":1,"reason":"余额不足","soft_streak":2}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if got, _ := p.creditsExpiringOf("u1"); got != 0 {
		t.Errorf("旧文件 creditsExpiring=%d want 0", got)
	}
	if rc, _ := p.breakerRetryCount("u1"); rc != 0 {
		t.Errorf("旧文件 retryCount=%d want 0", rc)
	}
	if bt, _ := p.breakerUntil("u1"); !bt.IsZero() {
		t.Errorf("旧文件 breakerUntil=%v want 零值", bt)
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	modelN := len(e.modelCooldowns)
	sessionFails := e.sessionDeadFails
	p.mu.RUnlock()
	if modelN != 0 {
		t.Errorf("旧文件 modelCooldowns=%d want 0", modelN)
	}
	if sessionFails != 0 {
		t.Errorf("旧文件 sessionDeadFails=%d want 0", sessionFails)
	}
	// 既有字段照常加载（兼容不回退）。
	st, _ := p.Status("u1")
	if st.Credits != 123 || !st.Cooling || st.Reason != "余额不足" || st.SoftStreak != 2 {
		t.Errorf("旧文件既有字段误加载: %+v", st)
	}
	// 旧值不阻碍后续写入：新字段可正常落盘并被下一轮恢复。
	p.SetCreditsDetailed("u1", 123, 0, 50)
	p.NoteSessionDead("u1")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got, _ := p2.creditsExpiringOf("u1"); got != 50 {
		t.Errorf("兼容加载后写入的 creditsExpiring=%d want 50", got)
	}
}

// TestStatusOfClearsExpiredReason until 过期后 status 的 reason/cool_kind 应清空
// （与落盘清理 cooledReasonLocked 同口径）：此前 statusOf 直接透出 e.reason，
// 过期 reason 会残留到下一次落盘清理（最多 5s 落盘窗口）才消失，两口径不一致。
func TestStatusOfClearsExpiredReason(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 手写一个已过期的 until + reason（模拟冷却刚到期、尚未被任何清理路径改写）。
	p.mu.Lock()
	e := p.byUID["u1"]
	e.until = time.Now().Add(-time.Minute)
	e.coolKind = CoolSoft
	e.reason = "429 rate limit"
	p.mu.Unlock()

	st, _ := p.Status("u1")
	if st.Reason != "" {
		t.Errorf("until 过期后 status reason=%q want \"\"（应惰性清空）", st.Reason)
	}
	if st.Cooling {
		t.Error("until 过期后 Cooling 应 false")
	}
	if st.CoolKind != "" {
		t.Errorf("until 过期后 CoolKind=%q want \"\"（应清空）", st.CoolKind)
	}
}

// TestStatusOfKeepsReasonWhenCooling until 在未来时 reason/cool_kind 正常透出（零回归）。
func TestStatusOfKeepsReasonWhenCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")

	st, _ := p.Status("u1")
	if st.Reason != "429 rate limit" {
		t.Errorf("冷却中 status reason=%q want %q", st.Reason, "429 rate limit")
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("冷却中 Cooling/CoolKind 应正常透出: %+v", st)
	}
}

// TestStatusOfKeepsDisabledReason disabled 账号的 reason 是禁用原因，不清空（零回归）。
func TestStatusOfKeepsDisabledReason(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")

	st, _ := p.Status("u1")
	if st.Reason != "12153 session dead" || st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled 账号 reason 应保留: Reason=%q DisabledReason=%q", st.Reason, st.DisabledReason)
	}
	if !st.Disabled {
		t.Error("disabled 账号 Disabled 应 true")
	}
}

func TestLoadLegacyErrCountMigratesToErrTotal(t *testing.T) {
	// 迁移测试：旧 state.json 只含 err_count（连续错误）→ 加载后 err_total 正确。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":100,"err_count":7}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.ErrTotal != 7 {
		t.Errorf("err_total=%d want 7 (migrated from legacy err_count)", st.ErrTotal)
	}
	// 新字段优先：二者并存时取较大者。
	both := `{"accounts":{"u1":{"credits":100,"err_count":3,"err_total":9}}}`
	if err := os.WriteFile(fp, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st2, _ := p2.Status("u1"); st2.ErrTotal != 9 {
		t.Errorf("err_total=%d want 9 (new field wins over legacy)", st2.ErrTotal)
	}
}

func TestStatusCoolKindDefaultsWhenNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if st.CoolKind != "" || st.CoolRemaining != 0 {
		t.Errorf("non-cooling portrait=%+v", st)
	}
}

func TestNextDay4AMBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  string // RFC3339 (UTC 表示)
		want string // 下一个 04:00（同一时区，UTC 表示）
	}{
		{"普通日", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		// 凌晨 00:00~04:00 触发硬冷却：当天 04:00 尚未到，冷却应落在当天（而非次日），
		// 否则多冷约一天（原 bug）。
		{"凌晨02:30", "2026-08-28T02:30:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨00:00", "2026-08-28T00:00:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨03:59:59", "2026-08-28T03:59:59+08:00", "2026-08-28T04:00:00+08:00"},
		{"正好4点", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4点刚过", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"月末(31天月)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"月末(28天月)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"闰年月末", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"年末", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", c.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	after := time.Now()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatalf("should be cooling: %+v", st)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 冷却截止必须是"此刻之后的最近一个 04:00"：晚于 now、距今不超过 24h
	//（凌晨 00:00~04:00 触发时落在当天 04:00，其余时段落在次日 04:00，跨度恒 < 24h）。
	if st.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", st.Until, before, after)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if d := st.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", st.Until, d)
	}
	// 全冷却时余额耗尽（hard）号不参与兜底 → 返回 nil（等签到恢复）。
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard-cooling should return nil (hard excluded from fallback), got %+v", got)
	}
}

func TestCooldownUntilTomorrow4AMPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Until.Hour() != 4 || st.Reason != "余额不足" {
		t.Errorf("status after reload=%+v ok=%v", st, ok)
	}
}

// ---------------------------------------------------------------------------
// 软冷却指数退避（softStreak）
// ---------------------------------------------------------------------------

// wantCoolSec 断言账号当前冷却剩余秒数 ≈ want（±tol 秒，容忍测试内的 tick 漂移）。
func wantCoolSec(t *testing.T, p *Pool, uid string, want int64, tol int64) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("%s should be in soft_rate cooling: %+v", uid, st)
	}
	if got := st.CoolRemaining; got < want-tol || got > want+tol {
		t.Errorf("cool_remaining_sec=%d want ~%d (±%d)", got, want, tol)
	}
}

// TestCooldownSoftBoundedBackoffIfNotCooling 无重置时间的 429 仍有界退避：
// 软冷却**从非冷却态**触发时按 2 倍指数增长（封顶 1h 本用例不触及），softStreak
// 计数随新冷却递增。退避只在**进入一次新冷却**时发生——冷却中途的兜底探测不推进
// （见 TestCooldownSoftRateNoDoubleWhenAlreadyCooling），既保留「无权威时间时的有界
// 退避」，又废除「越重试越冷」。
func TestCooldownSoftBoundedBackoffIfNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour) // 封顶 1h：本用例三步（600/1200/2400）都不触及

	for i, want := range []int64{600, 1200, 2400} {
		// 软冷却到期（until 过期）后再触发新冷却 → 退避继续推进。
		p.forceSoftExpired("u1")
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("after call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
	}
}

// TestCooldownSoftRateAlignsReset 带权威重置时间的账号级软冷却：写 until 对齐到
// 上游重置墙钟、绝不 touch softStreak（旧实现每次都 softStreak++），且不走退避。
func TestCooldownSoftRateAlignsReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute) // 远超 600s 基数，对齐将远超退避基数
	p.SetSoftRateMax(time.Hour)               // reset 在封顶内，不被截断

	p.CooldownSoftRate("u1", 600*time.Second, reset, "429 rate limit")
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("应为 soft_rate 冷却: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("带重置时间的 429 绝不 softStreak 堆加，soft_streak=%d want 0", st.SoftStreak)
	}
	if d := st.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("until=%v want ~reset=%v（精确对齐，无退避）", st.Until, reset)
	}
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("账号级 CooldownSoftRate 不写模型台账: %+v", st.RateLimitedModels)
	}
}

// TestCooldownSoftRateNoDoubleWhenAlreadyCooling 核心回归：冷却中的兜底探测再次撞 429
// **不得**翻倍/延长（旧实现每次都 softStreak++ 指数翻倍，把全池推到 2h 封顶）。
func TestCooldownSoftRateNoDoubleWhenAlreadyCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)

	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit") // streak=1, until≈now+600s
	wantCoolSec(t, p, "u1", 600, 3)
	if st, _ := p.Status("u1"); st.SoftStreak != 1 {
		t.Fatalf("soft_streak=%d want 1", st.SoftStreak)
	}
	// 冷却中重复触发（兜底探测）→ 时长/streak 均不变。
	for i := 0; i < 3; i++ {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	}
	wantCoolSec(t, p, "u1", 600, 3)
	if st, _ := p.Status("u1"); st.SoftStreak != 1 {
		t.Fatalf("already-cooling probe must not advance soft_streak, got %d", st.SoftStreak)
	}
}

// TestCooldownNoLongerFeedsBreaker 冷却入口不再喂熔断器（P1 语义反转）：Cooldown 与
// CooldownSoftRate 均不得推进 breaker_fails（熔断只由 NoteError 驱动）。
func TestCooldownNoLongerFeedsBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour) // 阈值 1：若误喂一次即熔断

	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429")
	p.CooldownSoftRate("u1", time.Minute, time.Now().Add(5*time.Minute), "429")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429")
	if got := p.breakerFails("u1"); got != 0 {
		t.Errorf("cooldown entries must not feed breaker: breaker_fails=%d want 0", got)
	}
	if st, _ := p.Status("u1"); st.BreakerFails != 0 {
		t.Errorf("status breaker_fails=%d want 0", st.BreakerFails)
	}
}

// TestCoolRemainingIncludesBreaker 熔断冷却的号 CoolRemaining 必须 > 0（口径与
// Cooling 判定一致，取 until 与 breakerUntil 中更远者）。
func TestCoolRemainingIncludesBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, 30*time.Minute, 30*time.Minute)
	p.NoteError("u1") // 阈值 1 → 立即熔断，until 未设（零值）

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("breaker should mark cooling: %+v", st)
	}
	if st.CoolRemaining < 30*60-5 || st.CoolRemaining > 30*60+5 {
		t.Errorf("cool_remaining_sec=%d want ~%d（补 breakerUntil 口径）", st.CoolRemaining, 30*60)
	}
}

func TestCooldownSoftCappedBySoftRateMax(t *testing.T) {
	// 注入封顶：streak 3 的 400s 被压到 250s。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(250 * time.Second)

	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 100, 3)
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 200, 3)
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 250, 3)
}

func TestCooldownSoftDefaultCapWhenUnset(t *testing.T) {
	// 未注入 softRateMax → 按 2h 封顶（避免裸用池时退避无上限）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i, want := range []int64{600, 1200, 2400, 4800, 7200} {
		p.forceSoftExpired("u1")
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("soft_streak=%d want %d", st.SoftStreak, i+1)
		}
	}
}

func TestSetSoftRateMaxIgnoresNonPositive(t *testing.T) {
	// 非正值保留原值（风格同 SetBreaker）：0 不应把封顶清零导致无上限。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(0)
	p.SetSoftRateMax(-time.Second)
	for i := 0; i < 6; i++ {
		p.forceSoftExpired("u1")
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	}
	wantCoolSec(t, p, "u1", 7200, 3) // 仍是 2h 封顶（第 6 步 19200s → 7200s）
}

func TestCooldownSoftStreakResetBySuccess(t *testing.T) {
	// 成功即证明账号恢复 → streak 归零，下次软冷却回到基数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 1200, 3)

	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("success should reset soft_streak, got %d", st.SoftStreak)
	}
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

// TestCooldownSoftStreakKeptByReenable issue #199 语义收窄：软冷却账号经余额刷新/
// 签到（ReenableIfCredits）不解冻，故 softStreak 也**不再**归零（冷却域原样保留，
// 退避指数不被刷新抹掉）。softStreak 的既有重置点仍是 NoteSuccess 与人工 Revive。
func TestCooldownSoftStreakKeptByReenable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	failsBefore := p.breakerFails("u1")

	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if st.SoftStreak != 2 {
		t.Errorf("软冷却不被余额刷新解冻 → soft_streak 应保留 2, got %d", st.SoftStreak)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("软冷却应原样保留（issue #199）: %+v", st)
	}
	if failsAfter := p.breakerFails("u1"); failsAfter != failsBefore {
		t.Errorf("reenable must not touch breaker: fails %d → %d", failsBefore, failsAfter)
	}

	// 人工强制解冻（Revive）才清软冷却域 → softStreak 归零、退避回到基数。
	p.Revive("u1")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Errorf("人工解冻应清 soft_streak, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestCooldownHardDoesNotAdvanceSoftStreak(t *testing.T) {
	// 硬冷却（余额耗尽）时长由签到时点决定，不参与软退避指数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("hard cooldown must not touch soft_streak, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestSoftStreakPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"soft_streak"`) {
		t.Fatalf("state.json missing soft_streak:\n%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.SoftStreak != 2 {
		t.Fatalf("soft_streak after reload=%d want 2", st.SoftStreak)
	}
	// 退避从持久化的 streak 继续：第 3 次 → 2400s。
	p2.forceSoftExpired("u1")
	p2.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p2, "u1", 2400, 3)
}

func TestSoftStreakMissingInLegacyStateFile(t *testing.T) {
	// 旧 state.json 无 soft_streak → 零值兼容，退避从基数重新开始。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("legacy file should load soft_streak=0, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

// forceSoftExpired 把账号的软冷却强制标记为已到期（until 归零、保留 coolKind=soft），
// 让下一次 CooldownSoftRate 视作「新限流」继续推进退避，而不依赖真实 sleep。仅测试用。
func (p *Pool) forceSoftExpired(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Time{}
	}
}

// ---------------------------------------------------------------------------
// issue #31：429 6004 模型级限流 → 按上游重置时间收窄冷却 + 模型级豁免选号
// ---------------------------------------------------------------------------

func TestCooldownSoftForModelParsedUntil(t *testing.T) {
	// 6004 msg 带「将在 … 重置」→ 该模型的独立冷却截止精确等于解析时间（wall-clock 判断）。
	// 用未来 5 分钟的时间戳：解析后 Until ≈ now+5m，远短于固定 600s 基数的指数退避，
	// 证明"上游明说重置时间"优先于"600s 起指数退避"。
	// 新语义：只写 modelCooldowns（不写账号级 until）→ 账号不 cooling、台账单行。
	reset := time.Now().Add(5 * time.Minute)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row for glm-5.3: %+v", st.RateLimitedModels)
	}
	until := st.RateLimitedModels[0].Until
	if d := until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", until, reset, d)
	}
}

func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	// 解析时间超出 soft_rate_max → 截断到 soft_rate_max（不无限期拉黑）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超过封顶 10m
	before := time.Now()
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("want model ledger row: %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Until.Sub(before) > 10*time.Minute+time.Second {
		t.Errorf("model until=%v want capped at soft_rate_max=10m", st.RateLimitedModels[0].Until)
	}
}

func TestCooldownSoftForModelNoResetFallbackBackoff(t *testing.T) {
	// 无解析时间（resetAt 零值）→ 退回账号级有界退避；冷却中的兜底探测不翻倍。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	// 仍在冷却中：兜底探测不推进退避。
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	// 软冷却到期后：续一次新限流 → 退避推进到 1200s。
	p.forceSoftExpired("u1")
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 1200, 3)
}

// TestPickExcludingForModelSkipsSoftCoolingSameModel 冷却中账号（6004 带解析时间，
// 已记录模型）+ 同 model 请求 → 仍不可选（现状语义保持）。
func TestPickExcludingForModelSkipsSoftCoolingSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("same-model request must skip cooling u1, got %+v", got)
	}
}

// TestPickExcludingForModelAllowsDifferentModel 6004 冷却中的账号 + 不同 model
// → 视为可用，可选到该号（真·单模型限流，切模型立即可用）。
func TestPickExcludingForModelAllowsDifferentModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u1" {
		t.Fatalf("different-model request should bypass u1 soft cooling, got %+v", got)
	}
}

// TestCooldownSoftWithoutModelRecordsNone 非 6004 的普通软冷却（resetAt 零值，
// 不记录 softRateModel）→ 不因模型切换而豁免（现状语义）。
func TestCooldownSoftWithoutModelRecordsNone(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429 rate limit")
	// 冷却中 + 不同 model 请求仍跳过 u1（无 softRateModel，不豁免）。
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("no model recorded → must not bypass, got %+v", got)
	}
}

// TestPickExcludingForModelBreakerStillBlocks 模型豁免只豁免软冷却维度，
// 熔断（breakerUntil）仍拦截：6004 冷却 + 熔断中的账号，切模型也不可选。
func TestPickExcludingForModelBreakerStillBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // u1 熔断
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("breaker must still block, got %+v", got)
	}
}

// TestSoftRateModelClearedByPlainCooldown 回归：6004 模型冷却后，若账号又经历一次
// **非模型级**软冷却（plain Cooldown），softRateModel 必须被清空——否则上次 6004 的
// 模型豁免会泄漏到本次账号级限流上，导致"换模型请求"错误绕过本次冷却。
func TestSoftRateModelClearedByPlainCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// 1) 6004 带解析时间 → 记录模型 glm-5.3。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: different-model should bypass, got %+v", got)
	}
	// 2) 账号恢复后经历普通账号级软冷却（无模型语义）。
	p.NoteSuccess("u1") // 还原 fresh 状态（Cooldown 会重设 until）
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	// 3) 换模型请求不得再豁免（softRateModel 已清空）。
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("plain cooldown must clear softRateModel (no bypass), got %+v", got)
	}
}

// TestSoftRateModelPersistedToState 模型级独立冷却（modelCooldowns）现已持久化。
//
// **语义反转**（v1.9.10 起）：本用例原为 TestSoftRateModelNotPersistedToState——反向
// 锁定「运行态、落盘不写 model_cooldowns、重启清零」的旧语义（含
// "state.json should not persist model_cooldowns" 断言）。6004 精确对齐上游重置墙钟后
// 单模型冷却可长达数小时，跨重启是常态，重启失忆会把账号重新送回 6004 限流模型上。
// 故反转为断言落盘写出 + 重载恢复台账（限额台账随持久化跨重启可见）。
// 不变的一条：6004-with-reset 只写 modelCooldowns、**不写**账号级 until，
// 重载后账号整体不 cooling（模型豁免语义与持久化正交）。
func TestSoftRateModelPersistedToState(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// resetAt 必须显著晚于 now：传 time.Now() 会走"重置时间已过"分支把冷却压到 1ms。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "glm-5.3", "429 rate limit")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json should persist model_cooldowns:\n%s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatalf("account missing after reload")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset 不写账号级 until，重载后不应 cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Errorf("modelCooldowns 应随持久化恢复（台账跨重启可见），got %+v", st.RateLimitedModels)
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42, 0)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

// TestSyncToDirExceptKeepsMissingUIDs keep 集合内的 uid 即便不在本轮扫描结果里也不被剔除。
// 这是 auths 目录热加载的关键保护：某轮读不出文件（上传中途的半截 JSON / 瞬时权限错误）
// 必须被当成「本轮没有该账号的新信息」，而不是「文件被删了」——否则一次写入中间态就会把
// 在用账号从池里抹掉，连带丢掉它的积分/冷却状态（再入池是全新 entry）。
func TestSyncToDirExceptKeepsMissingUIDs(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})

	// 本轮只扫到 u2，但 u1 在 keep 里（文件仍在、只是这轮读不出来）。
	added, removed := p.SyncToDirExcept([]*auth.Auth{{UID: "u2", AccessToken: "at2b"}},
		map[string]bool{"u1": true})
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("keep 内的 uid 不应被剔除: added=%v removed=%v", added, removed)
	}
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("u1 应被 keep 保留")
	}

	// keep 为空 → u1 被剔除（等价 SyncToDir），并如实上报 removed。
	added, removed = p.SyncToDirExcept([]*auth.Auth{{UID: "u2", AccessToken: "at2b"}}, nil)
	if len(added) != 0 || len(removed) != 1 || removed[0] != "u1" {
		t.Fatalf("added=%v removed=%v want removed=[u1]", added, removed)
	}

	// 新 uid 仍按 SyncToDir 语义加入并上报。
	added, removed = p.SyncToDirExcept([]*auth.Auth{{UID: "u3", AccessToken: "at3"}},
		map[string]bool{"u2": true})
	if len(added) != 1 || added[0] != "u3" || len(removed) != 0 {
		t.Fatalf("added=%v removed=%v want added=[u3]", added, removed)
	}
}

// TestSyncToDirUnchangedStillWorks SyncToDir 的对外语义不因抽出共用实现而漂移
// （启动路径 p.SyncToDir 现在委托给内部 syncToDirLocked）：新号加入、消失的号剔除、
// 留存账号的运行态（积分）原样保留。
func TestSyncToDirUnchangedStillWorks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 99, 0)

	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 应被剔除")
	}
	st, ok := p.Status("u2")
	if !ok || st.Credits != 99 {
		t.Fatalf("u2 应保留状态: %+v ok=%v", st, ok)
	}
}

func TestFlushPersistsCredits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 42 {
		t.Fatalf("flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestAutoFlush(t *testing.T) {
	old := flushInterval
	flushInterval = 20 * time.Millisecond
	defer func() { flushInterval = old }()

	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77, 0)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(fp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state.json not written by background flusher")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("auto flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestFlushIdempotentWhenClean(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush() // 无 dirty，不应写盘
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("flush on clean pool should not write: %v", err)
	}
}

func TestSaveFailureRecordedAndRecovers(t *testing.T) {
	// stateFp 的父路径是一个普通文件（非目录）→ MkdirAll/WriteFile 必失败，
	// root 也不可绕过，可靠地触发落盘失败路径。
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(block, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	if p.persistFails == 0 {
		t.Fatal("persist failure should be recorded (visible), got 0")
	}

	// 换回可写目录 → 成功后 persistFails 归零（恢复日志由零值门槛触发）。
	good := filepath.Join(t.TempDir(), "state.json")
	p2 := New(good)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetCredits("u1", 42, 0)
	p2.Flush()
	if p2.persistFails != 0 {
		t.Fatalf("successful save should reset persistFails, got %d", p2.persistFails)
	}
	if raw, err := os.ReadFile(good); err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("state.json not written on success: %v %s", err, raw)
	}
}

// ---------------------------------------------------------------------------
// T2 熔断器 + 全冷却兜底 + 指数退避
// ---------------------------------------------------------------------------

// breakerUntil 曝露内部运行态供测试断言（包内私有 helper）。
func (p *Pool) breakerUntil(uid string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}, false
	}
	return e.breakerUntil, true
}

// breakerFails 曝露 entry.fails 供测试断言（包内私有 helper）。
func (p *Pool) breakerFails(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid].fails
}

// internalHealthy 曝露 entry.healthy 供测试断言（包内私有 helper）。
func (p *Pool) internalHealthy(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.healthy(time.Now())
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	for i := 0; i < 2; i++ {
		p.NoteError("u1") // NoteError 只驱动熔断（不再有单独 err 冷却）
		if bt, ok := p.breakerUntil("u1"); ok && !bt.IsZero() {
			t.Fatalf("breaker tripped too early at %d: %v", i+1, bt)
		}
	}
	p.NoteError("u1")
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatalf("breaker should trip at threshold: until=%v ok=%v", bt, ok)
	}
}

func TestBreakerSuccessClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	p.NoteSuccess("u1")
	if bt, _ := p.breakerUntil("u1"); !bt.IsZero() {
		t.Fatalf("success should clear breaker, until=%v", bt)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("account should be healthy after success clears breaker")
	}
}

func TestBreakerExponentialBackoffCapped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Minute, 4*time.Minute) // threshold=3：连续 3 次失败熔断一次
	// 连续 9 次失败（无成功）→ 熔断 3 次，retryCount 1→2→3，退避 1m→2m→4m(封顶)。
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			p.NoteError("u1") // 连续失败只驱动熔断
		}
	}
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	d := time.Until(bt)
	// 第 3 次熔断：d = min(1m * 2^2, 4m) = 4m
	if d < 4*time.Minute-time.Second || d > 4*time.Minute+time.Second {
		t.Errorf("backoff should cap at max=4m, got %v", d)
	}

	// 对比第 1 次熔断（新账号重新来）：退避应更短。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetBreaker(3, time.Minute, 4*time.Minute)
	for j := 0; j < 3; j++ {
		p2.NoteError("u1")
	}
	bt1, _ := p2.breakerUntil("u1")
	if d1 := time.Until(bt1); d1 > time.Minute+time.Second {
		t.Errorf("first trip should be ~1m, got %v", d1)
	}
}

func TestFallbackPicksEarliestExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	// 两个都软冷却；early 更早到期 → 兜底选 early。
	p.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("early", CoolSoft, time.Hour, "x")
	got := p.Pick()
	if got == nil || got.UID != "early" {
		t.Fatalf("fallback should pick earliest expiry (early), got %+v", got)
	}
}

func TestFallbackSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "dead"})
	p.Cooldown("cooled", CoolSoft, time.Hour, "x")
	p.Disable("dead", "session dead") // 禁用不参与兜底
	got := p.Pick()
	if got == nil || got.UID != "cooled" {
		t.Fatalf("fallback should skip disabled, got %+v", got)
	}
}

func TestFallbackSkipsHardCooldown(t *testing.T) {
	// D3：余额耗尽（CoolHard）号不参与兜底——调了必 402，浪费轮换并产生噪音日志。
	p := New("")
	p.Add(&auth.Auth{UID: "hard"})
	p.Cooldown("hard", CoolHard, time.Hour, "余额不足")
	if got := p.Pick(); got != nil {
		t.Fatalf("hard-cooled account must not be fallback-picked, got %+v", got)
	}
}

func TestFallbackAllHardReturnsNil(t *testing.T) {
	// 全 hard 冷却 → 无软冷却/熔断号可兜底 → 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "h1"})
	p.Add(&auth.Auth{UID: "h2"})
	p.Cooldown("h1", CoolHard, time.Hour, "x")
	p.Cooldown("h2", CoolHard, 2*time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard should return nil, got %+v", got)
	}
}

func TestFallbackSoftAndBreakerParticipate(t *testing.T) {
	// D3：soft 与 breaker 冷却号允许参与兜底，取最早到期者。
	p := New("")
	p.Add(&auth.Auth{UID: "soft"})
	p.Add(&auth.Auth{UID: "brk"})
	p.Cooldown("soft", CoolSoft, 10*time.Minute, "429") // soft: until=10m，不喂熔断
	p.SetBreaker(2, 5*time.Minute, 5*time.Minute)       // 阈值 2：soft 冷却不参与熔断
	p.NoteError("brk")                                  // brk: fails=1
	p.NoteError("brk")                                  // brk: 熔断，breakerUntil=5m
	got := p.Pick()
	if got == nil {
		t.Fatal("fallback should pick breaker (earliest) account")
	}
	if got.UID != "brk" {
		t.Fatalf("fallback should pick earliest expiry brk (5m < soft 10m), got %+v", got)
	}
}

func TestFallbackNilWhenAllDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil when all disabled, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T3 三因子加权选取
// ---------------------------------------------------------------------------

// idleWeightOf 曝露 weightOf 的单因子拆解不便，改用完整权重断言（包内私有 helper）。
func (p *Pool) entryWeight(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	var maxCredits int64
	for _, x := range p.byUID {
		if x.credits > maxCredits {
			maxCredits = x.credits
		}
	}
	return p.weightOf(e, maxCredits, time.Now())
}

func TestWeightHighCreditsDominates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "hi"})
	p.Add(&auth.Auth{UID: "lo"})
	p.SetCredits("hi", 1000, 0)
	p.SetCredits("lo", 10, 0)
	wHi, wLo := p.entryWeight("hi"), p.entryWeight("lo")
	if wHi <= wLo {
		t.Errorf("high credits should weigh more: hi=%v lo=%v", wHi, wLo)
	}
}

func TestWeightIdleCompensation(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "used"})
	p.Add(&auth.Auth{UID: "idle"})
	p.SetCredits("used", 100, 0)
	p.SetCredits("idle", 100, 0)
	// used 1 小时前被选中过、idle 从未使用 → idle 权重更高（闲置补偿）。
	p.mu.Lock()
	p.byUID["used"].lastUsed = time.Now().Add(-1 * time.Hour)
	p.mu.Unlock()
	wUsed, wIdle := p.entryWeight("used"), p.entryWeight("idle")
	if wIdle <= wUsed {
		t.Errorf("idle should weigh more: used=%v idle=%v", wUsed, wIdle)
	}
}

func TestWeightLowSuccessRateDowngrades(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "good"})
	p.Add(&auth.Auth{UID: "bad"})
	p.SetCredits("good", 100, 0)
	p.SetCredits("bad", 100, 0)
	p.NoteSuccess("good")
	p.NoteError("bad")
	p.NoteError("bad")
	wGood, wBad := p.entryWeight("good"), p.entryWeight("bad")
	if wBad >= wGood {
		t.Errorf("low success rate should weigh less: good=%v bad=%v", wGood, wBad)
	}
}

func TestWeightAllZeroCreditsStillWeighted(t *testing.T) {
	// credits 全 0：权重完全由 idle+successRate 决定，不退化均匀随机（仍可选出更高分者）。
	p := New("")
	p.Add(&auth.Auth{UID: "idle"})
	p.Add(&auth.Auth{UID: "bursty"})
	// idle 从未使用、bursty 半分钟前刚用过 → idle 权重更高。
	p.mu.Lock()
	p.byUID["bursty"].lastUsed = time.Now().Add(-30 * time.Second)
	p.mu.Unlock()
	wIdle, wBursty := p.entryWeight("idle"), p.entryWeight("bursty")
	if wIdle <= wBursty {
		t.Errorf("idle should outweigh recently-used when credits all zero: idle=%v bursty=%v", wIdle, wBursty)
	}
}

func TestWeightTopFiveSelectionChanges(t *testing.T) {
	withNoPickGap(t)
	// credits 相差不大时，闲置补偿可让"低分但久置"的账号权重反超"高分但刚用"的账号，
	// 即使 credits 排序里 b 在前（Top5 内权重排序可与 credits 排序不同）。
	p := New("")
	for _, u := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a", 90, 0) // a credits 略低，但久置
	p.SetCredits("b", 100, 0)
	p.mu.Lock()
	p.byUID["b"].lastUsed = time.Now()
	p.byUID["a"].lastUsed = time.Now().Add(-48 * time.Hour)
	p.mu.Unlock()
	if wA, wB := p.entryWeight("a"), p.entryWeight("b"); wA <= wB {
		t.Errorf("idle a should outweigh busy higher-credit b: a=%v b=%v", wA, wB)
	}
}

// ---------------------------------------------------------------------------
// T4 在途租约（单账号并发上限）
// ---------------------------------------------------------------------------

func TestAcquireReleaseLifecycle(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	if !p.Acquire("u1") {
		t.Fatal("first acquire should succeed")
	}
	if !p.Acquire("u1") {
		t.Fatal("second acquire should succeed")
	}
	if p.Acquire("u1") {
		t.Fatal("third acquire should fail (limit 2)")
	}
	p.Release("u1")
	if !p.Acquire("u1") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestAcquireUnlimited(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// max=0 不限：连续 acquire 永不拒绝。
	for i := 0; i < 100; i++ {
		if !p.Acquire("u1") {
			t.Fatalf("unlimited acquire %d failed", i)
		}
	}
}

func TestAcquireUnknownUID(t *testing.T) {
	p := New("")
	if p.Acquire("nope") {
		t.Fatal("acquire unknown uid should fail")
	}
	p.Release("nope") // 不 panic
}

func TestPickSkipsInFlightFull(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "full"})
	p.Add(&auth.Auth{UID: "free"})
	p.SetCredits("full", 1000, 0)
	p.SetCredits("free", 1, 0)
	p.SetMaxInFlight(1)
	// full 占满唯一名额 → Pick 应跳过它，选 free（即使 credits 更低）。
	p.Acquire("full")
	got := p.Pick()
	if got == nil || got.UID != "free" {
		t.Fatalf("pick should skip in-flight-full account, got %+v", got)
	}
	p.Release("full")
	// 释放后可重新被选中（确定性随机源 r=0 → 选 credits 最高的 full）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.Pick(); got == nil || got.UID != "full" {
		t.Fatalf("after release full should be pickable, got %+v", got)
	}
	p.Release("full")
}

func TestInFlightCountNotExceedLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)

	// 并发 50 次 acquire：CAS 保证任一时刻在途数不超上限；每次成功后立即 release。
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Acquire("u1") {
				// 峰值检查：acquire 成功后立即读计数，应 ≤ 2。
				p.mu.RLock()
				if n := p.byUID["u1"].inFlight.Load(); n > 2 {
					t.Errorf("in-flight exceeded limit: %d", n)
				}
				p.mu.RUnlock()
				p.Release("u1")
			}
		}()
	}
	wg.Wait()

	// 全部释放后计数必须为 0。
	p.mu.RLock()
	n := p.byUID["u1"].inFlight.Load()
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("in-flight should be 0 after all releases, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// T6 向后兼容 + 运行态 Status 扩展
// ---------------------------------------------------------------------------

func TestLoadLegacyStateFile(t *testing.T) {
	// 旧 state.json 只含 credits/until/disabled 等老字段，缺熔断/在途/成功率新字段。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"legacy":{"credits":123,"until":"2027-01-01T04:00:00+08:00","cool_kind":1,"reason":"余额不足"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy"})
	st, ok := p.Status("legacy")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.Credits != 123 || !st.Cooling || st.Reason != "余额不足" {
		t.Errorf("legacy state misloaded: %+v", st)
	}
	// 运行态新字段默认零值。
	if st.InFlight != 0 || st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Errorf("runtime fields should be zero for legacy load: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// T7 D5: Redis 状态快照镜像 + 择新恢复
// ---------------------------------------------------------------------------

// memStore 内存假 Store：记录 SaveState（模拟 Redis 快照）并可按需返回 LoadState。
type memStore struct {
	mu       sync.Mutex
	saved    []byte
	loadData []byte
	loadOK   bool
}

func (m *memStore) SaveState(data []byte) {
	m.mu.Lock()
	m.saved = append([]byte(nil), data...)
	m.mu.Unlock()
}
func (m *memStore) LoadState() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loadOK {
		return nil, false
	}
	return append([]byte(nil), m.loadData...), true
}

func TestSaveMirrorsSnapshot(t *testing.T) {
	// Flush 落盘时同步 fire-and-forget SaveState（带 saved_at）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	ms := &memStore{}
	p.SetStore(ms)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	ms.mu.Lock()
	raw := string(ms.saved)
	ms.mu.Unlock()
	if !strings.Contains(raw, `"saved_at"`) {
		t.Fatalf("snapshot should carry saved_at: %s", raw)
	}
	if !strings.Contains(raw, `"credits":42`) {
		t.Fatalf("snapshot should carry account state: %s", raw)
	}
}

// TestSnapshotCarriesPersistedNewFields Redis 快照与本地 state.json 同源（stateFile），
// 三连新增的持久化字段应一并镜像，且恢复路径（applyAccountsLocked）同样做过期过滤。
func TestSnapshotCarriesPersistedNewFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	ms := &memStore{}
	p.SetStore(ms)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0, 400)
	p.SetBreaker(1, time.Hour, 6*time.Hour)
	p.NoteError("u1") // 熔断：breakerUntil 非零 + retryCount=1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	p.NoteSessionDead("u1") // sessionDeadFails=1
	p.Flush()

	ms.mu.Lock()
	raw := string(ms.saved)
	ms.mu.Unlock()
	for _, want := range []string{"credits_expiring", "breaker_until", "retry_count", "model_cooldowns", "session_dead_fails"} {
		if !strings.Contains(raw, want) {
			t.Errorf("Redis 快照 missing %s:\n%s", want, raw)
		}
	}

	// 走快照恢复路径（本地不可用 → 采用快照），过滤逻辑与本地 load 一致。
	ms2 := &memStore{loadOK: true, loadData: []byte(raw)}
	p2 := New(filepath.Join(dir, "missing", "state.json"))
	p2.SetStore(ms2)
	p2.RestoreFromSnapshot()
	p2.Add(&auth.Auth{UID: "u1"})
	if got, _ := p2.creditsExpiringOf("u1"); got != 400 {
		t.Errorf("快照恢复 creditsExpiring=%d want 400", got)
	}
	if rc, _ := p2.breakerRetryCount("u1"); rc != 1 {
		t.Errorf("快照恢复 retryCount=%d want 1", rc)
	}
	if bt, _ := p2.breakerUntil("u1"); bt.IsZero() {
		t.Error("快照恢复 breakerUntil 丢失")
	}
	p2.mu.RLock()
	modelN := len(p2.byUID["u1"].modelCooldowns)
	sessionFails := p2.byUID["u1"].sessionDeadFails
	p2.mu.RUnlock()
	if modelN != 1 {
		t.Errorf("快照恢复 modelCooldowns=%d want 1", modelN)
	}
	if sessionFails != 1 {
		t.Errorf("快照恢复 sessionDeadFails=%d want 1", sessionFails)
	}
}

func TestRestoreUsesRedisWhenNewer(t *testing.T) {
	// Redis 快照比本地 state.json 新 → 采用 Redis。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 本地较旧
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 把本地 mtime 设到过去
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fp, old, old); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot: %+v ok=%v", st, ok)
	}
}

func TestRestoreUsesLocalWhenNewer(t *testing.T) {
	// 本地 state.json 比 Redis 快照新 → 本地优先。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":77}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now().Add(-time.Hour)}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("should keep local (newer): %+v ok=%v", st, ok)
	}
}

func TestRestoreNoRedisUsesLocal(t *testing.T) {
	// 无 Redis 快照 → 本地优先。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":55}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: false}
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 55 {
		t.Fatalf("no redis → use local: %+v ok=%v", st, ok)
	}
}

func TestStatusExposesRuntimeFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	p.Acquire("u1") // in_flight=1
	st, _ := p.Status("u1")
	if st.InFlight != 1 {
		t.Errorf("in_flight=%d want 1", st.InFlight)
	}
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1") // breaker_fails=1
	st, _ = p.Status("u1")
	if st.BreakerFails != 1 {
		t.Errorf("breaker_fails=%d want 1", st.BreakerFails)
	}
	p.Release("u1")
}

func TestRecordTokenUsage(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model:               "glm-5.2",
		HasPromptTokens:     true,
		PromptTokens:        5,
		HasCompletionTokens: true,
		CompletionTokens:    7,
		HasTotalTokens:      true,
		TotalTokens:         12,
		HasLatencyMs:        true,
		LatencyMs:           1250,
		HasTokensPerSecond:  true,
		TokensPerSecond:     9.6,
	})
	p.RecordTokenUsage("u1", TokenUsageDelta{Model: "glm-5.2", HasLatencyMs: true, LatencyMs: 300})
	st, _ := p.Status("u1")
	if st.TokenUsage.RequestCount != 2 {
		t.Errorf("request_count=%d want 2", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 1 {
		t.Errorf("usage_count=%d want 1", st.TokenUsage.UsageCount)
	}
	if st.TokenUsage.PromptTokens != 5 || st.TokenUsage.CompletionTokens != 7 || st.TokenUsage.TotalTokens != 12 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 300 || st.TokenUsage.LastTokensPerSecond != nil {
		t.Errorf("latest performance should replace speed with unknown: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastModel != "glm-5.2" || st.TokenUsage.LastUsedAt.Before(before) {
		t.Errorf("last usage=%+v", st.TokenUsage)
	}
}

func TestTokenUsagePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model:               "deepseek-v4",
		HasPromptTokens:     true,
		PromptTokens:        11,
		HasCompletionTokens: true,
		CompletionTokens:    13,
		HasTotalTokens:      true,
		TotalTokens:         24,
		HasLatencyMs:        true,
		LatencyMs:           2300,
		HasTokensPerSecond:  true,
		TokensPerSecond:     5.65,
	})
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("account missing after reload")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.TotalTokens != 24 || st.TokenUsage.LastModel != "deepseek-v4" {
		t.Errorf("token usage lost after reload: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 2300 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond != 5.65 {
		t.Errorf("latest performance lost after reload: %+v", st.TokenUsage)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"token_usage"`) {
		t.Fatalf("state.json missing token_usage: %s", raw)
	}
	if strings.Contains(string(raw), "AccessToken") || strings.Contains(string(raw), "RefreshToken") {
		t.Fatalf("state.json contains credential field: %s", raw)
	}
}

// TestFallbackEarliestExpiryAdvancesUsedSeq 全冷却兜底选号
// （pickEarliestExpiryLocked）同样推进 usedSeq/pickSeq。
//
// entry.usedSeq 的契约是「每次被选中时取 p.pickSeq 自增值」（entry.go），pick() 正常
// 路径与粘性命中（PickByUIDForModel）都已遵守。兜底路径此前只写 lastUsed 就 return：
// 被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的 LRU 兜底（按 usedSeq 取最旧，pick.go）
// 眼里永远是「最旧」，刚被用过就被立刻再选——防集中/防惊群失效，且与同一函数里已更新
// lastUsed 的事实自相矛盾。
func TestFallbackEarliestExpiryAdvancesUsedSeq(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	// 两个都软冷却（全冷却 → 走兜底）；early 更早到期 → 兜底选 early。
	p.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("early", CoolSoft, time.Hour, "x")

	got := p.Pick()
	if got == nil || got.UID != "early" {
		t.Fatalf("全冷却兜底应选最早到期的 early, got %+v", got)
	}

	p.mu.RLock()
	seqEarly := p.byUID["early"].usedSeq
	seqLate := p.byUID["late"].usedSeq
	pickSeq := p.pickSeq
	p.mu.RUnlock()

	if seqEarly == 0 {
		t.Errorf("兜底选号未推进 usedSeq: early=%d（兜底也是选中，违反 entry.usedSeq 契约「每次被选中时取 p.pickSeq 自增值」）", seqEarly)
	}
	if seqEarly != pickSeq {
		t.Errorf("兜底推进的 usedSeq 应等于 pickSeq: early=%d pickSeq=%d", seqEarly, pickSeq)
	}
	if seqEarly <= seqLate {
		t.Errorf("兜底被选中的 early usedSeq=%d 应高于未被选中的 late=%d（否则 LRU 兜底误判其为最旧）", seqEarly, seqLate)
	}
}

// TestStickyPickAdvancesUsedSeq 粘性命中（PickByUIDForModel）推进 usedSeq/pickSeq
// ——LRU 兜底不再把粘性重度使用的号当「最旧」。
//
// 本仓自查发现：上游 49930b2 只修了兜底路径，本仓的粘性路径同样缺 usedSeq 推进
// （PickByUIDForModel 只写 lastUsed），一并补上以维持 entry.usedSeq 契约。
func TestStickyPickAdvancesUsedSeq(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.Add(&auth.Auth{UID: "other"})
	p.mu.RLock()
	seqBefore := p.byUID["sticky"].usedSeq
	p.mu.RUnlock()
	for i := 0; i < 5; i++ {
		if a := p.PickByUIDForModel("sticky", "m"); a == nil {
			t.Fatalf("粘性选号第 %d 次返回 nil", i)
		}
	}
	p.mu.RLock()
	seqAfter := p.byUID["sticky"].usedSeq
	p.mu.RUnlock()
	if seqAfter <= seqBefore {
		t.Errorf("粘性选号应推进 usedSeq: before=%d after=%d", seqBefore, seqAfter)
	}
}

// TestStickyUsedSeqMaintainsTotalOrder 粘性推进后，LRU 兜底眼中粘性重度号不再是
// 「最旧」：粘性号被 PickByUIDForModel 连续使用后，其 usedSeq 应高于从未使用的 other。
func TestStickyUsedSeqMaintainsTotalOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.Add(&auth.Auth{UID: "other"})
	for i := 0; i < 3; i++ {
		if a := p.PickByUIDForModel("sticky", "m"); a == nil {
			t.Fatalf("粘性选号第 %d 次返回 nil", i)
		}
	}
	p.mu.RLock()
	sSticky, sOther := p.byUID["sticky"].usedSeq, p.byUID["other"].usedSeq
	p.mu.RUnlock()
	if sSticky <= sOther {
		t.Errorf("粘性重度号的 usedSeq=%d 应高于从未选中的 other=%d（LRU 兜底不误判）", sSticky, sOther)
	}
}

// TestLRUFallbackStrictTotalOrderAfterFallbackPick 兜底推进 usedSeq 后，LRU 兜底
// 维持严格全序：刚被兜底选中的账号不再被 LRU 立刻再选。
//
// 场景：三号全软冷却 → 第一次 Pick 走兜底选最早到期的 early（推进 usedSeq）；
// 随后把三号都置为「刚被用过」（minPickGap 超大 → top5 全不合格）走 LRU 兜底，
// 此时 early 因 usedSeq 已被推进，不应再被选中（否则「刚用过就被立刻再选」）。
func TestLRUFallbackStrictTotalOrderAfterFallbackPick(t *testing.T) {
	oldGap := minPickGap
	minPickGap = time.Hour // 超大窗口：所有 lastUsed 都在窗口内 → 走 LRU 兜底
	defer func() { minPickGap = oldGap }()

	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.Add(&auth.Auth{UID: "a3"})
	// 全软冷却 → 第一次 Pick 走全冷却兜底，a1 最早到期被选中。
	p.Cooldown("a1", CoolSoft, time.Minute, "x")
	p.Cooldown("a2", CoolSoft, 2*time.Minute, "x")
	p.Cooldown("a3", CoolSoft, 3*time.Minute, "x")
	first := p.Pick()
	if first == nil || first.UID != "a1" {
		t.Fatalf("首次兜底应选 a1, got %+v", first)
	}

	// 让三号重新 healthy（冷却过期）且 lastUsed 都是 now → 走 LRU 兜底。
	p.mu.Lock()
	now := time.Now()
	for _, uid := range []string{"a1", "a2", "a3"} {
		e := p.byUID[uid]
		e.until = time.Time{} // 清冷却截止 → healthy（health 只看 until/breakerUntil/disabled）
		e.lastUsed = now
	}
	p.mu.Unlock()

	second := p.Pick()
	if second == nil {
		t.Fatal("第二次 Pick 返回 nil")
	}
	if second.UID == "a1" {
		t.Errorf("LRU 兜底不应再选刚被兜底选中的 a1（usedSeq 未推进 → 误判为最旧）")
	}
	// 严格全序：a2/a3 的 usedSeq 均为 0，选中的应是其中之一；被选中者 usedSeq 已推进。
	p.mu.RLock()
	seqPicked := p.byUID[second.UID].usedSeq
	seqA1 := p.byUID["a1"].usedSeq
	p.mu.RUnlock()
	if seqPicked <= seqA1 {
		t.Errorf("LRU 兜底选中者 usedSeq=%d 应高于刚被兜底选中的 a1=%d", seqPicked, seqA1)
	}
}

func TestRestoreUsesRedisWhenLocalMissing(t *testing.T) {
	// 本地 state.json 不存在（首次在新卷/新节点启动）+ 有效 Redis 快照 → 必须采用快照。
	// 此时本地没有可"优先"的状态，快照是本轮唯一来源（快照作为"启动恢复备份"的核心场景，
	// 见 StoreSnapshotter 契约）。旧实现把该情形并进「本地较新」的 fall-through：快照被
	// 静默丢弃（既不改内存也不置 dirty），全池运行态清零，且打出"本地较新于快照"的假日志。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json") // 刻意不创建：模拟新卷首启

	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)

	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatalf("本地缺失时应采用 Redis 快照恢复账号，但池内无 u1（有效快照被丢弃）")
	}
	if st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot when local missing: credits=%d want 999", st.Credits)
	}
}

// TestRestoreFromSnapshotMarksDirtyWhenLocalMissing 采用快照后必须置 dirty：
// 否则快照只在内存生效，下一次崩溃恢复又回到旧的本地文件（adoptSnapshot 的存在意义）。
func TestRestoreFromSnapshotMarksDirtyWhenLocalMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json") // 本地缺失

	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 7}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)

	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	if !p.dirty.Load() {
		t.Errorf("采用快照后应置 dirty（让下一次落盘把快照物化回本地 state.json）")
	}
	// Flush 后本地文件应已写出快照内容（物化验证）。
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("Flush 后本地 state.json 应存在: %v", err)
	}
	if !strings.Contains(string(raw), `"credits": 7`) {
		t.Errorf("本地 state.json 应物化快照内容: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// 快过期积分（creditsExpiring）：同步口径 + 对外透出三态
//
// 背景（issue「快过期积分完全不显示」两处缺陷）：
//   - A：签到/余额刷新只在 expiring>0 时写分桶（SetCreditsDetailed），expiring==0 时
//     旧值永不复位（陈旧值永久留存）；
//   - B：Status 无 CreditsExpiring 字段、statusOf 也不赋值 → 面板 accounts[] 里根本
//     没有这个键，前端无从显示。
// ---------------------------------------------------------------------------

// TestSetCreditsExpiringResetsToZero 缺陷 A 的池侧核心回归：expiring==0 时也必须把
// creditsExpiring 复位为 0（构造「先设非零、再刷新为 0」的序列）。
//
// 为什么需要这个独立入口（而不是复用 SetCreditsDetailed / 合并进 ReenableIfCredits）：
//   - SetCreditsDetailed 不含「解冻」语义（它只写 credits/total/expiring），而
//     ReenableIfCredits 带解冻语义（remain>0 且有效硬冷却才清冷却域，软冷却不动）——
//     把分桶写进解冻入口会让「是否解冻」与「是否分桶」重新耦合；
//   - 给 ReenableIfCredits 加参数会改动全部既有调用点与测试的签名（纯增量入口不碰它们）。
//
// 故本入口只做一件事：把分桶观测量同步成真值（含 0），不碰 credits/冷却/熔断/降权。
func TestSetCreditsExpiringResetsToZero(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 先设非零（模拟上一轮窗口内分桶），再同步为 0（积分到期/窗口缩小/上游不再给到期字段）。
	p.SetCreditsDetailed("u1", 1000, 2000, 400)
	if got, _ := p.creditsExpiringOf("u1"); got != 400 {
		t.Fatalf("前置：creditsExpiring=%d want 400", got)
	}
	p.SetCreditsExpiring("u1", 0)
	if got, _ := p.creditsExpiringOf("u1"); got != 0 {
		t.Errorf("creditsExpiring=%d want 0（expiring==0 必须复位，否则陈旧值永久留存）", got)
	}
	// 只动分桶观测量：余额口径不被本入口改写（credits/creditsTotal 由 ReenableIfCredits 写）。
	st, _ := p.Status("u1")
	if st.Credits != 1000 || st.CreditsTotal != 2000 {
		t.Errorf("SetCreditsExpiring 不应改余额口径：credits=%d total=%d want 1000/2000", st.Credits, st.CreditsTotal)
	}
	// 越界钳制与 SetCreditsDetailed / 恢复侧同口径（上游分桶异常不得污染权重）。
	p.SetCreditsExpiring("u1", 5000)
	if got, _ := p.creditsExpiringOf("u1"); got != 1000 {
		t.Errorf("越界 expiring=%d want 1000（钳到 credits）", got)
	}
	p.SetCreditsExpiring("u1", -5)
	if got, _ := p.creditsExpiringOf("u1"); got != 0 {
		t.Errorf("负值 expiring=%d want 0", got)
	}
	// 不存在的 uid：空操作（面板/调度并发下 uid 可能已被移除，不得 panic）。
	p.SetCreditsExpiring("nope", 10)
}

// TestSetCreditsExpiringKeepsCooldownState 分桶同步不得触碰任何惩罚态（解冻语义不回归）：
// 软冷却账号调 SetCreditsExpiring 后仍在冷却，熔断/降权同理。
func TestSetCreditsExpiringKeepsCooldownState(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")
	p.SetCreditsExpiring("u1", 100)
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Errorf("SetCreditsExpiring 不得解冻软冷却账号：%+v", st)
	}
	if got, _ := p.creditsExpiringOf("u1"); got != 0 {
		// credits 为 0（未刷新余额）→ 钳到 0；这里只断言不 panic 且不越界。
		t.Errorf("credits=0 时 expiring=%d want 0（钳到 credits）", got)
	}
}

// TestStatusExposesCreditsExpiring 缺陷 B 的池侧核心回归：Status 必须透出
// credits_expiring（面板 overview 的 accounts[] 直接序列化 Status），且三态在 JSON
// 上可区分：
//
//	① credits_expiring > 0            → 键带数值（明确显示快过期部分）；
//	② == 0 且总额已知（credits_total）→ 键缺席（omitempty）+ credits_total 在场
//	                                    → 「窗口内确实没有快过期积分」；
//	③ 旧 state / 未知（两者都 0）      → 键缺席且 credits_total 也缺席 → 「未知」。
//
// ② 与 ③ 必须可分：前端据此避免把「没有快过期积分」显示成「0 分快过期」。
func TestStatusExposesCreditsExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // ① 有快过期
	p.Add(&auth.Auth{UID: "u2"}) // ② 总额已知、无快过期
	p.Add(&auth.Auth{UID: "u3"}) // ③ 未知（旧 state）
	p.SetCreditsDetailed("u1", 1000, 2000, 300)
	p.SetCreditsDetailed("u2", 500, 800, 0)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status for u1")
	}
	if st.CreditsExpiring != 300 {
		t.Errorf("Status.CreditsExpiring=%d want 300（字段缺失即缺陷 B）", st.CreditsExpiring)
	}

	// JSON 序列化断言：面板读的就是这些键。
	states := map[string]map[string]any{}
	for _, s := range p.List() {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %s: %v", s.UID, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", s.UID, err)
		}
		states[s.UID] = m
	}
	if v, ok := states["u1"]["credits_expiring"]; !ok {
		t.Errorf("① u1 的 JSON 缺 credits_expiring：%v", states["u1"])
	} else if v.(float64) != 300 {
		t.Errorf("① u1 credits_expiring=%v want 300", v)
	}
	if _, ok := states["u2"]["credits_expiring"]; ok {
		t.Errorf("② u2 无快过期时不应出现 credits_expiring（避免前端显示成 0 分快过期）：%v", states["u2"])
	}
	if _, ok := states["u2"]["credits_total"]; !ok {
		t.Errorf("② u2 必须有 credits_total（前端据此判定「窗口内没有」而非「未知」）：%v", states["u2"])
	}
	if _, ok := states["u3"]["credits_total"]; ok {
		t.Errorf("③ u3（旧 state）不应有 credits_total：%v", states["u3"])
	}
	if _, ok := states["u3"]["credits_expiring"]; ok {
		t.Errorf("③ u3（旧 state）不应有 credits_expiring：%v", states["u3"])
	}
}
