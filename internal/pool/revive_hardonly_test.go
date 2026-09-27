package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// revive_hardonly_test.go 余额刷新解冻只清 CoolHard（吸收上游 602ed1b）。
//
// 背景：RunBalanceRefreshNow 每 5 分钟对 expiring==0 的账号调用 ReenableIfCredits。
// 旧实现（全清冷却域）会把 6004 模型级台账（对齐上游重置墙钟）与 CoolSoft 软限流
// 退避一并抹掉——任何限流冷却的实际寿命被压到一个刷新周期（≤5min）内：撞限号被
// 误判健康后重新选中再撞 429（上游 issue #127/#153 的两号池实测指纹：expiring==0
// 的号每 5 分钟被抹一次台账，expiring>0 的号走 SetCreditsDetailed 幸免，行为不对称）。
//
// 修法：余额恢复只解冻**余额耗尽冷却**（CoolHard 的 until/coolKind/reason——余额
// 恢复正是它的权威恢复证据）；CoolSoft 软限流、softStreak 退避计数与 modelCooldowns
// 全部保留——限流的恢复证据是上游重置墙钟到期或探测成功，不是「余额有钱」。

// TestReviveHardOnlyUnfreezesCoolHard 余额恢复（remain>0）解冻 CoolHard：
// 这是本解冻路径**唯一**该动的冷却维度（签到到账 = 余额耗尽的恢复证据）。
func TestReviveHardOnlyUnfreezesCoolHard(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.ReenableIfCredits("u1", 700, 0)

	until, kind, reason, _, _ := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 || reason != "" {
		t.Errorf("余额恢复应解冻 CoolHard：until=%v kind=%v reason=%q", until, kind, reason)
	}
	if st, _ := p.Status("u1"); st.Credits != 700 {
		t.Errorf("credits=%d want 700", st.Credits)
	}
	if !p.internalHealthy("u1") {
		t.Error("CoolHard 解冻后账号应回到可选状态")
	}
}

// TestReviveHardOnlyKeepsModelCooldowns CoolHard 解冻**不得**顺手清 6004 模型级台账：
// 旧实现走 clearCoolingLocked 整域归零，6004 的「对齐上游重置墙钟」随之消失；
// 而余额有钱与「该模型的重置墙钟到了」是两件独立事实。
//
// 状态构造用**可达**顺序：先 Cooldown(CoolHard)（该入口按既有语义清空台账——账号级
// 冷却使模型豁免作废），再 CooldownSoftForModel(resetAt) 写入模型级台账（该分支只写
// modelCooldowns，不碰 coolKind/until，故硬冷却与台账可并存）。
//
// 同时锁定：人工 Revive（面板「解冻」按钮）仍是唯一能清台账的自动/半自动入口。
func TestReviveHardOnlyKeepsModelCooldowns(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	// 硬冷却期内该账号的某模型撞 6004（有上游重置墙钟 → 落 modelCooldowns）。
	resetAt := time.Now().Add(50 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.3", "6004 model rate limit")

	p.ReenableIfCredits("u1", 500, 0)

	until, kind, _, _, mc := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 {
		t.Errorf("CoolHard 应被解冻：until=%v kind=%v", until, kind)
	}
	if mc != 1 {
		t.Fatalf("modelCooldowns=%d want 1（余额恢复不构成 6004 台账的解除证据）", mc)
	}
	p.mu.RLock()
	entry, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("6004 台账条目丢失")
	}
	if d := entry.Until.Sub(resetAt); d < -time.Second || d > time.Second {
		t.Errorf("6004 until=%v want ~%v（重置墙钟不得被余额刷新改写）", entry.Until, resetAt)
	}
	// 切模型仍豁免、同模型仍被拦（模型级语义不被解冻破坏）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got != nil {
		t.Errorf("同模型不应选中（6004 台账存活）, got %+v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Errorf("切模型应豁免选中, got %+v", got)
	}
}

// TestReviveHardOnlyKeepsSoftStreak 余额恢复不清 softStreak（软限流退避计数）：
// 退避计数与余额无关，恢复证据是 NoteSuccess（成功是最强证据）或自然到期收敛。
// 旧实现随冷却域一并归零 → 每次余额刷新都让退避回到基数，指数退避形同虚设。
func TestReviveHardOnlyKeepsSoftStreak(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")
	p.mu.Lock()
	p.byUID["u1"].until = time.Now().Add(-time.Second) // 冷却已到期，模拟跨冷却期
	p.mu.Unlock()
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit") // 第二次 → streak=2
	p.CooldownUntilTomorrow4AM("u1", "余额不足")                           // 叠加硬冷却（解冻对象）

	p.ReenableIfCredits("u1", 500, 0)

	_, kind, _, streak, _ := coolingDomain(t, p, "u1")
	if kind != 0 {
		t.Fatalf("硬冷却应被解冻, kind=%v", kind)
	}
	if streak != 2 {
		t.Errorf("softStreak=%d want 2（退避计数不被余额刷新清零）", streak)
	}
}

// TestReviveHardOnlyKeepsSoftCooldown CoolSoft 账号级软冷却不被余额刷新解冻
// （issue #199 的既有收窄，本文件与之同向）：软冷却账号的 coolKind != CoolHard，
// ReenableIfCredits 只更新 credits。
func TestReviveHardOnlyKeepsSoftCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")

	p.ReenableIfCredits("u1", 500, 0)

	until, kind, reason, _, _ := coolingDomain(t, p, "u1")
	if until.IsZero() || kind != CoolSoft || reason == "" {
		t.Errorf("软冷却不得被余额刷新解冻：until=%v kind=%v reason=%q", until, kind, reason)
	}
	if st, _ := p.Status("u1"); st.Credits != 500 {
		t.Errorf("credits=%d want 500（余额照常更新）", st.Credits)
	}
}

// TestReviveHardOnlyKeepsSoftDurationAcrossRefresh 行为锚（上游实测复现的机制）：
// 模拟「余额刷新周期任务每 5 分钟来一次」——反复 ReenableIfCredits 不得把软冷却
// 的实际寿命压到一个刷新周期内。旧实现下第一次刷新就解冻，本用例必红。
func TestReviveHardOnlyKeepsSoftDurationAcrossRefresh(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 30*time.Minute, time.Time{}, "429 rate limit")

	// 三个刷新周期（每次间隔 5 分钟）。
	for i := 0; i < 3; i++ {
		p.ReenableIfCredits("u1", 500, 0)
	}
	until, kind, _, _, _ := coolingDomain(t, p, "u1")
	if kind != CoolSoft || until.IsZero() {
		t.Fatalf("软冷却被余额刷新抹掉：until=%v kind=%v", until, kind)
	}
	if rem := time.Until(until); rem < 25*time.Minute {
		t.Errorf("软冷却剩余=%v want ≥25m（刷新周期不得压缩冷却寿命）", rem)
	}
}
