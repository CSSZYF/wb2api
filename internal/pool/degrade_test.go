package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// consecutiveStateOf 曝露 entry 的连败计数与降权截止（包内私有 helper）。
func (p *Pool) consecutiveStateOf(uid string) (fails int, degradeUntil time.Time, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, time.Time{}, false
	}
	return e.consecutiveFails, e.degradeUntil, true
}

// TestNoteFailuresTriggersDegradeAtThreshold 达阈触发降权（核心验收点）：
// 前 4 次只计数不动作（账号保持可选），第 5 次触发 degradeUntil，账号出池。
func TestNoteFailuresTriggersDegradeAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for n := 1; n < 5; n++ {
		p.NoteFailures("u1")
		fails, until, _ := p.consecutiveStateOf("u1")
		if fails != n {
			t.Fatalf("第 %d 次后 consecutiveFails=%d want %d", n, fails, n)
		}
		if !until.IsZero() {
			t.Fatalf("第 %d 次不应触发降权（未达阈值 5）", n)
		}
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("未达阈时账号应保持可选（第 %d 次）", n)
		}
	}
	p.NoteFailures("u1") // 第 5 次：达阈
	fails, until, _ := p.consecutiveStateOf("u1")
	if fails != 0 {
		t.Fatalf("达阈后计数应清零供下一轮累计，got %d", fails)
	}
	if until.IsZero() || !time.Now().Before(until) {
		t.Fatalf("第 5 次应触发降权，degradeUntil=%v", until)
	}
	// 降权期 normal 选号不选它（healthy 或门；单号池 Pick 走全冷却兜底另计——
	// 与 CoolSoft 同语义，用 AvailableUIDs 断言 normal 口径）。
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("降权期账号不应出现在可用列表, got %v", uids)
	}
	st, _ := p.Status("u1")
	if st.Cooling != true || st.Reason != degradeReason || st.CoolKind != "degrade" {
		t.Fatalf("降权期 Status 应呈非健康+连败文案: cooling=%v reason=%q kind=%q", st.Cooling, st.Reason, st.CoolKind)
	}
	if st.ConsecutiveFails != 0 || st.DegradeUntil.IsZero() {
		t.Fatalf("Status 应透出降权态: fails=%d until=%v", st.ConsecutiveFails, st.DegradeUntil)
	}
}

// TestNoteFailuresSuccessClearsCountAndDegrade 成功清零（核心验收点）：
// 连败中途成功 → 计数归零；降权期内成功 → 降权撤销（成功即回池，同 NoteSuccess
// 清 breakerUntil 口径）。
func TestNoteFailuresSuccessClearsCountAndDegrade(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 中途成功清计数：4 次失败 + 成功 + 4 次失败 → 仍不降权（清零后重新累计）。
	for n := 0; n < 4; n++ {
		p.NoteFailures("u1")
	}
	p.NoteSuccess("u1")
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("成功应清连败计数, got %d", fails)
	}
	for n := 0; n < 4; n++ {
		p.NoteFailures("u1")
	}
	if _, until, _ := p.consecutiveStateOf("u1"); !until.IsZero() {
		t.Fatal("成功清零后重新累计的 4 次不应触发降权")
	}
	// 降权期内成功 → 撤销降权。
	for n := 0; n < 5; n++ {
		p.NoteFailures("u1") // 达阈降权
	}
	if _, until, _ := p.consecutiveStateOf("u1"); until.IsZero() {
		t.Fatal("precondition: 应已降权")
	}
	p.NoteSuccess("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if !until.IsZero() {
		t.Fatalf("成功应撤销降权（回池）, degradeUntil=%v", until)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatal("成功后账号应立即可选")
	}
}

// TestDegradeExpiresAutoReturn 降权到期自动回池（无需显式复位）。
func TestDegradeExpiresAutoReturn(t *testing.T) {
	p := New("")
	p.SetDegrade(2, 50*time.Millisecond, 2*time.Hour) // 阈 2、50ms 便于测试
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	if _, until, _ := p.consecutiveStateOf("u1"); until.IsZero() {
		t.Fatal("precondition: 应已降权")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.Pick(); got != nil && got.UID == "u1" {
			return // 到期回池
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("降权到期后账号应自动回池（2s 内）")
}

// TestDegradeAndCooldownTakeLonger 与冷却「并存取更长者不叠加」（核心验收点）：
// 降权与冷却同时存在时，生效的是更远的截止；较近者先到期不提前放行。
func TestDegradeAndCooldownTakeLonger(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 冷却 1h（更远），随后触发降权 10m（较近）→ healthy 应被 1h 冷却拦住。
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	for n := 0; n < 5; n++ {
		p.NoteFailures("u1")
	}
	_, degradeUntil, _ := p.consecutiveStateOf("u1")
	if degradeUntil.IsZero() || !time.Now().Before(degradeUntil) {
		t.Fatal("precondition: 应已降权")
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("冷却+降权并存时账号应不可选")
	}
	if st.CoolRemaining < int64((55 * time.Minute).Seconds()) {
		t.Fatalf("生效截止应是更远的 1h 冷却（取更长者），remaining=%ds", st.CoolRemaining)
	}
	// reason 呈现：有生效冷却时以冷却 reason 为准（语义更具体），降权 reason 不抢。
	if st.Reason != "429 rate limit" {
		t.Fatalf("并存时 reason 应以冷却为准, got %q", st.Reason)
	}
}

// TestDegradeNoRetryStacking 降权期内重试达阈不延长（不越堆越厚）：
// 与 CooldownSoftRate 兜底探测同哲学。
func TestDegradeNoRetryStacking(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 触发：degradeUntil ≈ now+1h
	_, first, _ := p.consecutiveStateOf("u1")
	// 降权期内再连败 2 次（达阈）：不延长。
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	_, second, _ := p.consecutiveStateOf("u1")
	if !second.Equal(first) {
		t.Fatalf("降权期内达阈不应延长: first=%v second=%v", first, second)
	}
	// 且计数已清零：到期后需重新满 2 次才再降。
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("降权期内达阈后计数应清零, got %d", fails)
	}
}

// TestNoteFailuresSporadicNotDegraded 不误伤偶发失败（核心验收点）：
// 偶发失败被中间的成功打断 → 永远累计不满阈值，账号不出池。
func TestNoteFailuresSporadicNotDegraded(t *testing.T) {
	p := New("")
	p.SetDegrade(3, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	// 模式：失败失败成功 失败成功 失败……计数最多到 2，永不达阈 3。
	for round := 0; round < 20; round++ {
		p.NoteFailures("u1")
		p.NoteFailures("u1")
		p.NoteSuccess("u1")
		p.NoteFailures("u1")
		p.NoteSuccess("u1")
		p.NoteFailures("u1")
	}
	_, until, _ := p.consecutiveStateOf("u1")
	if !until.IsZero() {
		t.Fatal("偶发失败（被成功打断）不应触发降权")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatal("偶发失败账号应保持可选")
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatal("偶发失败账号不应呈冷却态")
	}
}

// TestDegradeOrthogonalToBreakerAndSoftStreak 正交性断言（任务书核心）：
// NoteFailures 不得改变熔断计数 fails、软退避 softStreak、冷却截止 until，
// 也不得改变 errTotal（那由 NoteError 累计）。
func TestDegradeOrthogonalToBreakerAndSoftStreak(t *testing.T) {
	p := New("")
	p.SetBreaker(100, time.Hour, 6*time.Hour) // 熔断阈值放大：避免 NoteError 顺带熔断干扰断言
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})

	// 先制造一份「既有状态」：软冷却（softStreak=1 + until 在未来）+ 熔断计数 fails=1。
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	p.NoteError("u1")
	p.mu.RLock()
	beforeUntil := p.byUID["u1"].until
	beforeStreak := p.byUID["u1"].softStreak
	beforeFails := p.byUID["u1"].fails
	beforeErrTotal := p.byUID["u1"].errTotal
	p.mu.RUnlock()
	if beforeStreak != 1 || beforeFails != 1 {
		t.Fatalf("precondition: streak=%d fails=%d want 1/1", beforeStreak, beforeFails)
	}

	// 喂连败到降权（阈值 2）。
	p.NoteFailures("u1")
	p.NoteFailures("u1")

	p.mu.RLock()
	afterUntil := p.byUID["u1"].until
	afterStreak := p.byUID["u1"].softStreak
	afterFails := p.byUID["u1"].fails
	afterErrTotal := p.byUID["u1"].errTotal
	afterDegrade := p.byUID["u1"].degradeUntil
	p.mu.RUnlock()

	if !afterUntil.Equal(beforeUntil) {
		t.Errorf("NoteFailures 不应改冷却截止 until: before=%v after=%v", beforeUntil, afterUntil)
	}
	if afterStreak != beforeStreak {
		t.Errorf("NoteFailures 不应改 softStreak: before=%d after=%d", beforeStreak, afterStreak)
	}
	if afterFails != beforeFails {
		t.Errorf("NoteFailures 不应改熔断计数 fails: before=%d after=%d", beforeFails, afterFails)
	}
	if afterErrTotal != beforeErrTotal {
		t.Errorf("NoteFailures 不应改 errTotal: before=%d after=%d", beforeErrTotal, afterErrTotal)
	}
	if afterDegrade.IsZero() {
		t.Error("precondition: 连败达阈应已降权")
	}
}

// TestDegradeNotClearedByReenableIfCredits 余额刷新/签到解冻不得误清降权（issue #199 同向）：
// ReenableIfCredits 只对有效硬冷却放行，且只清冷却域——降权与连败计数原样保留。
func TestDegradeNotClearedByReenableIfCredits(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 降权
	_, before, _ := p.consecutiveStateOf("u1")
	if before.IsZero() {
		t.Fatal("precondition: 应已降权")
	}
	// 余额刷新（remain>0）：非硬冷却形态 → 只更新 credits。
	p.ReenableIfCredits("u1", 500, 1000)
	_, after, _ := p.consecutiveStateOf("u1")
	if after.IsZero() || !after.Equal(before) {
		t.Fatalf("余额刷新不得清降权: before=%v after=%v", before, after)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("余额刷新后账号仍应处于降权出池态, got %v", uids)
	}
	// 连败中途的计数同样不得被余额刷新清掉（半开进度保留）。
	p2 := New("")
	p2.SetDegrade(5, time.Hour, 2*time.Hour)
	p2.Add(&auth.Auth{UID: "u2"})
	p2.NoteFailures("u2")
	p2.NoteFailures("u2")
	p2.ReenableIfCredits("u2", 500, 1000)
	if fails, _, _ := p2.consecutiveStateOf("u2"); fails != 2 {
		t.Fatalf("余额刷新不得清连败进度: got %d want 2", fails)
	}
}

// TestDegradeClearedByRevive 人工解冻（Revive）清降权：面板「解冻」是无条件恢复入口。
func TestDegradeClearedByRevive(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	if _, until, _ := p.consecutiveStateOf("u1"); until.IsZero() {
		t.Fatal("precondition: 应已降权")
	}
	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	fails, until, _ := p.consecutiveStateOf("u1")
	if fails != 0 || !until.IsZero() {
		t.Fatalf("Revive 应清连败计数与降权: fails=%d until=%v", fails, until)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatal("Revive 后账号应立即可选")
	}
}

// TestNoteFailuresClassifiedErrorsNotFed 带权威分类的错误不喂连败（不重复计罚）：
// NoteError（5xx 熔断路径）与 Cooldown（429 冷却路径）不推进 consecutiveFails。
func TestNoteFailuresClassifiedErrorsNotFed(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for n := 0; n < 10; n++ {
		p.NoteError("u1")
	}
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("NoteError 不应推进连败计数, got %d", fails)
	}
	p.NoteSuccess("u1") // 清熔断
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("CooldownSoftRate 不应推进连败计数, got %d", fails)
	}
}

// TestDegradePersistRoundTrip 连败降权持久化：计数 + 降权截止落盘 → 重启 → 恢复。
func TestDegradePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // consecutiveFails=2（未达阈 5，半开进度）
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	fails, until, ok := p2.consecutiveStateOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if fails != 2 {
		t.Fatalf("重启后 consecutiveFails=%d want 2（半开进度持久化）", fails)
	}
	if !until.IsZero() {
		t.Fatalf("未触发降权时重启不应有 degradeUntil, got %v", until)
	}
	// 恢复的计数继续累计：重启后再 3 次即达阈降权。
	p2.NoteFailures("u1")
	p2.NoteFailures("u1")
	p2.NoteFailures("u1")
	_, until, _ = p2.consecutiveStateOf("u1")
	if until.IsZero() || !time.Now().Before(until) {
		t.Fatal("恢复计数=2 后再 3 次应触发降权")
	}
	// 降权中重启：degradeUntil 恢复（降权期不失忆）。
	p2.Flush()
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	_, until2, _ := p3.consecutiveStateOf("u1")
	if until2.IsZero() || !time.Now().Before(until2) {
		t.Fatal("降权期内重启 degradeUntil 应恢复")
	}
	if uids := p3.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("恢复降权期的账号不应出现在可用列表, got %v", uids)
	}
}

// TestDegradeWritesZeroFails consecutiveFails=0 显式落盘（运维口径，同
// session_dead_fails：零值缺失会误解为"没记录"）。
func TestDegradeWritesZeroFails(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSuccess("u1") // 制造 dirty 让 Flush 真正写盘，consecutiveFails 保持 0
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"consecutive_fails": 0`) {
		t.Errorf("consecutiveFails=0 时也应显式写出:\n%s", raw)
	}
}

// TestDegradeUntilExpiredNotPersisted 已过期的 degradeUntil 不落盘（惰性过滤，
// 同 breakerUntil 口径）：避免 state.json 残留无效截止。
func TestDegradeUntilExpiredNotPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.SetDegrade(2, 20*time.Millisecond, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 降权 20ms
	time.Sleep(40 * time.Millisecond)
	p.NoteSuccess("u1") // 只制造 dirty（同时会清降权）……
	// 重新造一个「已过期但未清」的降权截止：直接写内表（模拟到期后未走 NoteSuccess）。
	p.mu.Lock()
	p.byUID["u1"].degradeUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "degrade_until") {
		t.Errorf("已过期 degradeUntil 不应落盘:\n%s", raw)
	}
}

// TestDegradeCountsAsCooling 降权计入 CountsDetailed 的 cooling 计数
// （/status 池级健康度口径覆盖降权号，不误报 healthy）。
func TestDegradeCountsAsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	for n := 0; n < 5; n++ {
		p.NoteFailures("u1")
	}
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 2 || healthy != 1 || cooling != 1 || disabled != 0 {
		t.Fatalf("counts=(%d,%d,%d,%d) want (2,1,1,0)", total, healthy, cooling, disabled)
	}
}

// TestDegradeFallbackEarliestExpiry 全冷却兜底口径：降权号参与兜底（按 expiry 最早到期），
// 与软冷却同处置（CoolHard 才排除）。
func TestDegradeFallbackEarliestExpiry(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 降权 1h
	// 全池无 healthy → Pick 走全冷却兜底，应选中该降权号（探测语义）。
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("全冷却时降权号应参与兜底, got %+v", got)
	}
}

// TestDegradeConcurrentNoteFailures 并发喂入无竞态/无丢失超阈：
// N 个 goroutine 各喂若干次，总喂次数 ≥ 阈值时必须观察到降权发生
// （锁保护下计数串行一致；-race 下验证零竞态）。
func TestDegradeConcurrentNoteFailures(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	const goroutines = 8
	const perG = 5 // 总 40 次 ≥ 阈值 5
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < perG; n++ {
				p.NoteFailures("u1")
			}
		}()
	}
	wg.Wait()
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("并发喂入 40 次（≥阈值）应至少触发一次降权")
	}
}

// TestDegradeConcurrentNoteFailuresAndClose 并发喂入 + 停机 Close 落盘竞态
// （任务书重点：连败计数器并发更新/停机持久化竞态检查）。生产停机顺序是
// 「在途请求退出 → Close」；Close 与 NoteFailures 并发（未等到 goroutine 全退
// 就 Close）也必须不 panic/不半写——Close 内部持锁 Flush，与在途写入串行化。
func TestDegradeConcurrentNoteFailuresAndClose(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				p.NoteFailures("u1")
				p.NoteSuccess("u1")
			}
		}()
	}
	// 并发中直接 Close（乱序停机路径）：Close 持锁 Flush 与在途写入串行，不出竞态。
	p.Close()
	wg.Wait()
	// wg 全退后再补一次 Flush（模拟最后一笔入账后的停机快照）。
	p.Flush()
	// 停机后状态文件应存在且可解析（无半写/损坏）。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("停机后 state.json 应存在: %v", err)
	}
	if !strings.Contains(string(raw), `"consecutive_fails"`) {
		t.Fatalf("停机快照应含连败计数字段:\n%s", raw)
	}
	// 重启可加载（JSON 完整性）。
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	if _, ok := p3.Status("u1"); !ok {
		t.Fatal("重启后账号应可恢复")
	}
}

// TestSetDegradeInjection SetDegrade 注入生效（阈值/时长/封顶），非法值保留默认。
func TestSetDegradeInjection(t *testing.T) {
	p := New("")
	p.SetDegrade(2, 100*time.Millisecond, time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("注入阈值 2 应在第 2 次触发")
	}
	if d := time.Until(until); d > time.Hour {
		t.Fatalf("降权时长不得超封顶, got %v", d)
	}
	// 非法注入（0/负值）保留默认：阈值仍 5。
	p2 := New("")
	p2.SetDegrade(0, 0, 0)
	p2.Add(&auth.Auth{UID: "u2"})
	for n := 0; n < 4; n++ {
		p2.NoteFailures("u2")
	}
	if _, until, _ := p2.consecutiveStateOf("u2"); !until.IsZero() {
		t.Fatal("非法注入后默认阈值 5：4 次不应触发")
	}
}

// TestSetDegradeClampsCooldownByMax 降权时长被 cooldownMax 钳制（固定时长 + 上限，
// 非指数退避）。
func TestSetDegradeClampsCooldownByMax(t *testing.T) {
	p := New("")
	p.SetDegrade(1, 5*time.Hour, time.Hour) // cooldown 5h > max 1h → 钳到 1h
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("阈值 1 应首次即触发")
	}
	if d := time.Until(until); d > time.Hour+time.Second {
		t.Fatalf("降权时长应被 cooldownMax 钳到 1h, got %v", d)
	}
}

// TestSetDegradeHotChangeTakesEffect 热改阈值/时长立即生效（任务书验收点）：
// 面板改配置后走 SetDegrade，下一次判定即按新值。
func TestSetDegradeHotChangeTakesEffect(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 默认阈值 5：喂 4 次不降权。
	for n := 0; n < 4; n++ {
		p.NoteFailures("u1")
	}
	if _, until, _ := p.consecutiveStateOf("u1"); !until.IsZero() {
		t.Fatal("默认阈值 5：4 次不应降权")
	}
	// 热改阈值 3 + 时长 30m：再喂 1 次（累计 5 ≥ 3）即达阈，时长按新值。
	p.SetDegrade(3, 30*time.Minute, 2*time.Hour)
	p.NoteFailures("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("热改阈值 3 后累计 5 次应触发降权")
	}
	if d := time.Until(until); d > 30*time.Minute+time.Second || d < 29*time.Minute {
		t.Fatalf("热改后的降权时长应为 30m, got %v", d)
	}
}

// TestDegradeThresholdHugeNeverTriggers 反向断言：阈值设成极大 → 连败不触发降权
// （证明配置真的生效，也是本机制的「关闭」方式）。
func TestDegradeThresholdHugeNeverTriggers(t *testing.T) {
	p := New("")
	p.SetDegrade(1_000_000, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	for n := 0; n < 100; n++ {
		p.NoteFailures("u1")
	}
	if _, until, _ := p.consecutiveStateOf("u1"); !until.IsZero() {
		t.Fatalf("阈值 1e6 时 100 连败不应降权, until=%v", until)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatal("未达阈账号应保持可选")
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("未达阈账号不应呈冷却态: %+v", st)
	}
	if st.ConsecutiveFails != 100 {
		t.Fatalf("计数应如实累计: got %d want 100", st.ConsecutiveFails)
	}
}

// TestDegradeDefaultThresholdIsFive 默认阈值 5（与熔断 3 区分，保守不误伤）。
func TestDegradeDefaultThresholdIsFive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for n := 0; n < 4; n++ {
		p.NoteFailures("u1")
	}
	if _, until, _ := p.consecutiveStateOf("u1"); !until.IsZero() {
		t.Fatal("默认阈值应为 5：4 次不降权")
	}
	p.NoteFailures("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("默认阈值应为 5：第 5 次降权")
	}
	if d := time.Until(until); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("默认降权时长应为 10m, got %v", d)
	}
}

// TestDegradeUnknownUIDNoop 未知 uid 是空操作（不 panic、不建条目）。
func TestDegradeUnknownUIDNoop(t *testing.T) {
	p := New("")
	p.NoteFailures("nope")
	if _, ok := p.Status("nope"); ok {
		t.Fatal("未知 uid 不应被创建")
	}
}

// TestDegradeNotClearedByCooldownProbeSuccess 冷却探活成功不清连败降权（正交性 + 边界）：
// CooldownProbeSuccess 的职责是「该模型/该账号的冷却刚被证明可用」，只清冷却域；
// 探活不是用户流量、不写成功统计（probe.go 明确不调 NoteSuccess），因此不构成
// 「ErrClient/传输层连败的根因已消失」的证据——降权按 degradeUntil 自己到期放行。
// 若在这里顺手清降权，等于让后台探活成为降权的旁路解冻（与 issue #199 收窄同向的破口）。
func TestDegradeNotClearedByCooldownProbeSuccess(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	// 造一份「已到期的账号级软冷却 + 已到期的模型级条目」——探活成功会清它们。
	// 直接写内表（包内测试）：CooldownSoftRate 会清 modelCooldowns（账号级冷却清模型
	// 豁免），且 cappedSoftUntilLocked 对已过期的 resetAt 返回 now+1ms（非真过去），
	// 两个入口都造不出「双到期」形态。
	p.mu.Lock()
	past := time.Now().Add(-time.Hour)
	e := p.byUID["u1"]
	e.until = past
	e.coolKind = CoolSoft
	e.reason = "429 rate limit"
	e.modelCooldowns = map[string]modelCooldown{"glm-5.2": {Until: past, Reason: "6004 model rate limit"}}
	p.mu.Unlock()
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 降权
	_, before, _ := p.consecutiveStateOf("u1")
	if before.IsZero() {
		t.Fatal("precondition: 应已降权")
	}

	modelCleared, accountCleared := p.CooldownProbeSuccess("u1", "glm-5.2", true)
	if !modelCleared && !accountCleared {
		t.Fatal("precondition: 应清掉已到期的冷却（模型级/账号级）")
	}
	// 降权不受影响：探活成功不构成连败根因消失的证据。
	_, after, _ := p.consecutiveStateOf("u1")
	if after.IsZero() || !after.Equal(before) {
		t.Fatalf("探活成功不得清降权: before=%v after=%v", before, after)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("探活后降权号仍应出池, got %v", uids)
	}
	// 反证：真正的成功路径（NoteSuccess）才回池。
	p.NoteSuccess("u1")
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "u1" {
		t.Fatalf("NoteSuccess 后应回池, got %v", uids)
	}
}

// TestDegradeNotClearedByDisableReviveDisabled 禁用/ReviveDisabled 不清降权：
// 降权与授权/session 无关（disableLocked 只清冷却域），且禁用期间计数继续累计——
// 复活后若根因未消失按既有进度继续判罚，不重新学。
func TestDegradeNotClearedByDisableReviveDisabled(t *testing.T) {
	p := New("")
	p.SetDegrade(5, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 半开进度 = 2
	p.Disable("u1", "12153 session dead")
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 2 {
		t.Fatalf("禁用不得清连败进度: got %d want 2", fails)
	}
	p.ReviveDisabled("u1")
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 2 {
		t.Fatalf("ReviveDisabled 不得清连败进度: got %d want 2", fails)
	}
}

// TestDegradeNotPersistedWhenDisabledDegradeExpired 组合边界：降权已过期 + 账号禁用时，
// 落盘不写 degrade_until（惰性过滤），consecutive_fails 照常写出。
func TestDegradeNotPersistedWhenDisabledDegradeExpired(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.SetDegrade(1, 20*time.Millisecond, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1") // 降权 20ms
	time.Sleep(40 * time.Millisecond)
	p.Disable("u1", "test")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "degrade_until") {
		t.Errorf("已过期 degradeUntil 不应落盘:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"consecutive_fails": 0`) {
		t.Errorf("consecutive_fails 应显式写出:\n%s", raw)
	}
}

// TestDegradeExcludedFromServable 降权号不计入可服务（/healthz 口径）：
// 单降权号 → ServableNow/ServableForRealm 均 false（chat 选号确实无候选）。
func TestDegradeExcludedFromServable(t *testing.T) {
	p := New("")
	p.SetDegrade(1, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	if !p.ServableNow() {
		t.Fatal("precondition: 健康号应可服务")
	}
	p.NoteFailures("u1") // 降权
	if p.ServableNow() {
		t.Error("全池仅降权号时 ServableNow 应为 false（探活/选号口径一致）")
	}
	if p.ServableForRealm("cn") {
		t.Error("降权号不应计入 realm 可服务")
	}
	// 到期后恢复可服务（不产生假阴性）。
	p2 := New("")
	p2.SetDegrade(1, 20*time.Millisecond, 2*time.Hour)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.NoteFailures("u1")
	time.Sleep(40 * time.Millisecond)
	if !p2.ServableNow() {
		t.Error("降权到期后应恢复可服务")
	}
}

// TestDegradeWithModelCooldownNotServable 降权号即便有模型级冷却（豁免形态）也不计入
// 可服务：降权是「整体临时出池」，不是「仅某模型不可用」的豁免形态。
// 若 modelExempt 不看 degradeUntil，/healthz 会在「全池仅剩一个降权号 + 它有 6004 条目」
// 时报 servable，而 chat 选号无候选——正是该谓词要消灭的探活/选号口径裂缝。
func TestDegradeWithModelCooldownNotServable(t *testing.T) {
	p := New("")
	p.SetDegrade(1, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	// 造「模型级冷却（豁免形态）+ 降权」并存：直接写内表（同前置测试的理由）。
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{"glm-5.2": {Until: time.Now().Add(time.Hour), Reason: "6004 model rate limit"}}
	p.mu.Unlock()
	p.NoteFailures("u1") // 降权
	if p.ServableNow() {
		t.Error("降权 + 模型级冷却并存时不应报可服务（降权是整体出池，非模型豁免）")
	}
	// 对照：无降权时该形态确实算可服务（模型豁免既有语义不回归）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u2"})
	p2.mu.Lock()
	p2.byUID["u2"].modelCooldowns = map[string]modelCooldown{"glm-5.2": {Until: time.Now().Add(time.Hour), Reason: "6004 model rate limit"}}
	p2.mu.Unlock()
	if !p2.ServableNow() {
		t.Error("仅模型级冷却（无降权）应保持可服务（issue #31 豁免语义不回归）")
	}
}
