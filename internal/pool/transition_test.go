package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 状态机迁移正交性测试：聚焦 transition.go 收敛出的「单一权威状态机」语义。
// entry 的可选择性由四个正交维度决定（disabled / 冷却域 until+coolKind+softStreak+
// modelCooldowns / 熔断器 fails+retryCount+breakerUntil / sessionDeadFails），本文件
// 锁定迁移原语对这四个维度的边界，特别是旧实现的缺陷点：
//   - Disable 旧实现只置 disabled+reason，不碰 until/modelCooldowns → 「disabled 但
//     cooling」杂交态（疑点 4）。新语义：disableLocked 置 disabled 并清冷却域。
//   - reviveCoolingLocked（签到解冻）只清冷却域、不动熔断器（C5 语义）。
//
// 与 pool_test.go / modelcooldown_test.go / sessiondead_test.go 的差异：那些测试
// 锁定各维度的行为，本文件锁定「跨维度」的迁移边界（冷却↔禁用↔熔断互不越界）。

// 直接读取 entry 的冷却域原始字段（Status 不暴露 coolKind 数值与 modelCooldowns）。
func coolingDomain(t *testing.T, p *Pool, uid string) (until time.Time, coolKind CoolKind, reason string, softStreak int, modelCooldowns int) {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("uid %s missing", uid)
	}
	return e.until, e.coolKind, e.reason, e.softStreak, len(e.modelCooldowns)
}

// TestTransitionDisableClearsCoolingDomain 疑点 4 修正：先冷却（软冷却 + 6004 模型级
// 冷却）再 Disable → 冷却域全清（until/coolKind/reason/softStreak/modelCooldowns），
// 只留 disabled 终态。旧实现只置 disabled+reason，会出现「disabled=true 但 cooling=true」
// 与残留 modelCooldowns 的杂交态。
func TestTransitionDisableClearsCoolingDomain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, 600*time.Second, "429")                                          // until + coolKind=soft + softStreak=1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004") // modelCooldowns=1（softStreak 增到 2）

	_, _, reason, _, mc := coolingDomain(t, p, "u1")
	if reason == "" || mc != 1 {
		t.Fatalf("precondition: 应先处于软冷却+模型级冷却态 (reason=%q modelCooldowns=%d)", reason, mc)
	}

	p.Disable("u1", "account banned")

	st, _ := p.Status("u1")
	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if !st.Disabled {
		t.Fatal("Disable 后应 disabled")
	}
	if !until.IsZero() {
		t.Errorf("Disable 后 until=%v 应为零值（冷却域清零）", until)
	}
	if kind != 0 {
		t.Errorf("Disable 后 coolKind=%v 应为零值", kind)
	}
	if reason != "account banned" {
		t.Errorf("Disable 后 reason=%q 应为禁用原因", reason)
	}
	if streak != 0 {
		t.Errorf("Disable 后 softStreak=%d 应为 0（冷却域清零）", streak)
	}
	if mc != 0 {
		t.Errorf("Disable 后 modelCooldowns=%d 应为 0（模型豁免随冷却域清零）", mc)
	}
	if st.Cooling {
		t.Errorf("Disable 后不应呈现 cooling（禁用是更强终态，绝不杂交）: %+v", st)
	}
}

// TestTransitionDisablePreservesBreaker 疑点 4 修正的另一半：Disable 只清冷却域、
// 不动熔断器。熔断是「连续 5xx 失败」信号（与授权/session 正交），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func TestTransitionDisablePreservesBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断（fails=0, retryCount=1, breakerUntil=1h）

	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}

	p.Disable("u1", "session dead")

	if bt, ok := p.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("Disable 后 breakerUntil 应保留（熔断与禁用正交）")
	}
	if fails := p.breakerFails("u1"); fails != 0 {
		t.Errorf("Disable 后 fails=%d（已置 0，不应被 Disable 刻意改动）", fails)
	}
	// disabled 优先：即使熔断仍在，账号也不可选。
	if got := p.Pick(); got != nil {
		t.Fatalf("disabled 账号不可选（含熔断期），got %+v", got)
	}
}

// TestTransitionSessionDeadDisableClearsCooling session 死亡走 NoteSessionDead 连续
// 计数，达阈值后经 disableLocked：冷却域一并清零，不得留下「disabled 但仍 cooling」
// 的杂交态（此前 handler 走 Disable、阈值路径却残留冷却，同一信号不同处置）。
func TestTransitionSessionDeadDisableClearsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // 软冷却中
	until, _, _, _, _ := coolingDomain(t, p, "u1")
	if until.IsZero() {
		t.Fatal("precondition: 应处于软冷却")
	}

	if p.NoteSessionDead("u1") || p.NoteSessionDead("u1") || !p.NoteSessionDead("u1") {
		t.Fatal("第 3 次 NoteSessionDead 应达阈值禁用")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("应 disabled")
	}
	if st.Cooling || !st.Until.IsZero() || st.SoftStreak != 0 {
		t.Errorf("session dead 禁用应清冷却域（无杂交态）: Cooling=%v Until=%v SoftStreak=%d",
			st.Cooling, st.Until, st.SoftStreak)
	}
	if st.DisabledReason != sessionDeadReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, sessionDeadReason)
	}
}

// TestTransitionReviveClearsCoolingAndBreaker 人工强制解冻（Revive）语义：
// 清冷却域（until/coolKind/reason/softStreak/modelCooldowns）+ 清熔断运行态（运维口径
// 无条件恢复）。既有单维度测试已各自锁定 reason/softStreak/modelCooldowns，本用例
// 一次性断言完整字段集，锁定迁移原语对冷却域/熔断域的处置永远一致。
//
// 注意与 ReenableIfCredits 的区别（issue #199 收窄）：后者是余额刷新/签到的自动解冻，
// 只对有效硬冷却放行、且不动熔断；本用例锁定的是人工「解冻」按钮走的 Revive。
func TestTransitionReviveClearsCoolingAndBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 冷却域：软冷却 + 6004 模型级冷却（softStreak 累计）。
	p.Cooldown("u1", CoolSoft, 600*time.Second, "429")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	// 熔断域：人工解冻一并清除（与 ReenableIfCredits 的「不清熔断」不同）。
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1")

	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true（账号存在）")
	}

	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 || reason != "" || streak != 0 || mc != 0 {
		t.Errorf("人工解冻应清冷却域：until=%v kind=%v reason=%q streak=%d modelCooldowns=%d",
			until, kind, reason, streak, mc)
	}
	if bt, ok := p.breakerUntil("u1"); !ok || !bt.IsZero() {
		t.Fatal("人工解冻应清熔断运行态（运维口径无条件恢复）")
	}
	if !p.internalHealthy("u1") {
		t.Fatal("人工解冻后账号应回到可选状态")
	}
}

// TestTransitionReenableOnlyHardCooling issue #199 语义收窄的迁移边界：
// 余额刷新/签到（ReenableIfCredits）对**硬冷却**清冷却域、对**软冷却**只更新 credits；
// 两者都不动熔断域（熔断只由到期/NoteSuccess 恢复）。
func TestTransitionReenableOnlyHardCooling(t *testing.T) {
	// 硬冷却：解冻（余额恢复正是硬冷却的恢复条件）。
	pHard := New("")
	pHard.Add(&auth.Auth{UID: "u1"})
	pHard.CooldownUntilTomorrow4AM("u1", "余额不足")
	pHard.SetBreaker(1, time.Hour, time.Hour)
	pHard.NoteError("u1")
	pHard.ReenableIfCredits("u1", 700, 0)
	until, kind, reason, streak, mc := coolingDomain(t, pHard, "u1")
	if !until.IsZero() || kind != 0 || reason != "" || streak != 0 || mc != 0 {
		t.Errorf("硬冷却应被余额恢复解冻：until=%v kind=%v reason=%q streak=%d modelCooldowns=%d",
			until, kind, reason, streak, mc)
	}
	if st, _ := pHard.Status("u1"); st.Credits != 700 {
		t.Errorf("credits=%d want 700", st.Credits)
	}
	if bt, ok := pHard.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("余额刷新不得清熔断（chat 通道健康未证明）")
	}
	if pHard.internalHealthy("u1") {
		t.Fatal("熔断期内不应 healthy（熔断域未被余额刷新覆盖）")
	}

	// 软冷却：只更新 credits，冷却域原样保留。
	pSoft := New("")
	pSoft.Add(&auth.Auth{UID: "u1"})
	pSoft.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")
	pSoft.SetBreaker(1, time.Hour, time.Hour)
	pSoft.NoteError("u1")
	pSoft.ReenableIfCredits("u1", 700, 0)
	st, _ := pSoft.Status("u1")
	if st.Credits != 700 {
		t.Errorf("软冷却账号 credits=%d want 700（余额照常更新）", st.Credits)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("软冷却不得被余额刷新解冻（issue #199）：%+v", st)
	}
	if bt, ok := pSoft.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("余额刷新不得清熔断")
	}
}
