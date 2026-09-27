package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// accountFaultCount 读 entry.accountFaultFails（包内私有 helper，测试断言用）。
func accountFaultCount(p *Pool, uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.accountFaultFails
	}
	return -1
}

// TestNoteAccountFaultThresholdNotReached 前 2 次连续 11140 不 Disable（误判防护）。
// 旧行为一次 11140 即 Disable → 账号永久退出选号（disabled 无自动清除路径）。
func TestNoteAccountFaultThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteAccountFault("u1") {
		t.Fatal("第 1 次 11140 不应禁用")
	}
	if p.NoteAccountFault("u1") {
		t.Fatal("第 2 次 11140 不应禁用")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("连续 2 次 11140 不应禁用: %+v", st)
	}
	// 未达阈值时账号仍可选（handler 侧另加软冷却，池层不惩罚）。
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("账号应保持可选, got %+v", got)
	}
	if n := accountFaultCount(p, "u1"); n != 2 {
		t.Errorf("accountFaultFails=%d want 2", n)
	}
}

// TestNoteAccountFaultDisablesAtThird 连续第 3 次 11140 → 禁用并清计数，
// disabled_reason 含 11140（运维一眼看出是哪个码判的死）。
func TestNoteAccountFaultDisablesAtThird(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	if !p.NoteAccountFault("u1") {
		t.Fatal("第 3 次 11140 应禁用")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("第 3 次后应 disabled: %+v", st)
	}
	if !strings.Contains(st.DisabledReason, "11140") {
		t.Errorf("disabled_reason=%q 应含 11140", st.DisabledReason)
	}
	// 达阈禁用后计数清零（与 NoteSessionDead 同口径：计数是「连续未达阈」的进度）。
	if n := accountFaultCount(p, "u1"); n != 0 {
		t.Errorf("达阈禁用后 accountFaultFails=%d want 0", n)
	}
	// 禁用后不再可选。
	if p.Pick() != nil {
		t.Fatal("禁用账号不可被选中")
	}
}

// TestNoteAccountFaultUnknownUID 不存在的 uid 是空操作（返回 false，不 panic）。
func TestNoteAccountFaultUnknownUID(t *testing.T) {
	p := New("")
	if p.NoteAccountFault("nope") {
		t.Fatal("未知 uid 不应返回达阈")
	}
}

// TestNoteSuccessClearsAccountFaultCount 任意成功是账号未死的最强证据 → 清 11140 计数；
// 之后从 1 重新累计（与 sessionDeadFails 的清零点同口径）。
func TestNoteSuccessClearsAccountFaultCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	p.NoteSuccess("u1")
	if n := accountFaultCount(p, "u1"); n != 0 {
		t.Fatalf("NoteSuccess 后 accountFaultFails=%d want 0", n)
	}
	if p.NoteAccountFault("u1") {
		t.Fatal("成功清计数后第 1 次不应禁用")
	}
	p.NoteAccountFault("u1")
	if !p.NoteAccountFault("u1") {
		t.Fatal("成功清计数后第 3 次应禁用（从 1 重新计够 3 次）")
	}
}

// TestReviveDisabledClearsAccountFaultCount 人工复活清禁用 + 11140 计数：
// 复活后重新计满 3 次才禁用（不残留旧进度）。
func TestReviveDisabledClearsAccountFaultCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1") // 达阈禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已禁用")
	}
	if !p.ReviveDisabled("u1") {
		t.Fatal("revive 应清自动禁用位")
	}
	if n := accountFaultCount(p, "u1"); n != 0 {
		t.Errorf("revive 后 accountFaultFails=%d want 0", n)
	}
	if p.NoteAccountFault("u1") || p.NoteAccountFault("u1") {
		t.Fatal("复活后前 2 次不应禁用（应从新计数）")
	}
	if !p.NoteAccountFault("u1") {
		t.Fatal("复活后第 3 次应禁用")
	}
}

// TestAccountFaultDisabledRequiresManualRevive 死锁锁定（锁定现状，勿以为会自愈）：
// 11140 达阈禁用后**没有任何自动路径**能把它放回选号池——NoteSuccess（成功是账号
// 活着的最强证据）不清 disabled、ReenableIfCredits（签到/余额刷新）被 !disabled 挡住、
// ClearSessionDead 不碰它、冷却探活（CooldownProbeTargets）跳过 disabled 号、
// 全冷却兜底（pickEarliestExpiryLocked）也排除 disabled。
// 只有人工 Revive / 重新 OAuth 登录才能出来。这条测试的作用是把该现状钉死：
// 若将来有人给 disabled 加了自动恢复路径，这里会红，从而必须显式讨论「误判面」。
func TestAccountFaultDisabledRequiresManualRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 达阈应已禁用")
	}

	// 全部自动路径轮一遍：成功、余额恢复解冻、12153 计数清零、冷却探活目标枚举。
	p.NoteSuccess("u1")
	p.ReenableIfCredits("u1", 1000, 0)
	p.ClearSessionDead("u1")
	for _, tgt := range p.CooldownProbeTargets(time.Now()) {
		if tgt.UID == "u1" {
			t.Errorf("disabled 账号不应出现在冷却探活目标里: %+v", tgt)
		}
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("自动路径不得清除 11140 禁用（误判后必须人工介入）: %+v", st)
	}
	if p.Pick() != nil {
		t.Fatal("disabled 账号不得被选中（含全冷却兜底）")
	}

	// 唯一出口：人工复活（面板「解冻」/ 重新登录）。
	if !p.Revive("u1") {
		t.Fatal("Revive 应成功")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("人工复活后应回到选号池, got %+v", got)
	}
}

// TestClearAccountFault 显式清零入口（与 ClearSessionDead 对称）：清计数并标 dirty，
// 计数已为 0 时是空操作（不无谓置脏）。
func TestClearAccountFault(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.dirty.Store(false)
	p.ClearAccountFault("u1")
	if n := accountFaultCount(p, "u1"); n != 0 {
		t.Errorf("ClearAccountFault 后 accountFaultFails=%d want 0", n)
	}
	if !p.dirty.Load() {
		t.Error("ClearAccountFault 清计数应标 dirty（否则重启后残留旧进度）")
	}
	p.dirty.Store(false)
	p.ClearAccountFault("u1")
	if p.dirty.Load() {
		t.Error("计数已为 0 时 ClearAccountFault 不应置 dirty")
	}
	// 不存在的 uid：空操作，不 panic。
	p.ClearAccountFault("nope")
}

// TestAccountFaultFailsPersistRoundTrip 连续 11140 计数已持久化：落盘 → 重启 → 恢复，
// 下次 11140 从恢复值继续累计（与 session_dead_fails 同口径：重启不重学）。
func TestAccountFaultFailsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1") // accountFaultFails=2（未达阈值 3）
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "account_fault_fails") {
		t.Errorf("state.json missing account_fault_fails:\n%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if n := accountFaultCount(p2, "u1"); n != 2 {
		t.Fatalf("恢复计数=%d want 2", n)
	}
	if !p2.NoteAccountFault("u1") {
		t.Fatal("恢复计数=2 后第 3 次 11140 应禁用（从恢复值继续累计）")
	}
	if st, _ := p2.Status("u1"); !st.Disabled {
		t.Fatal("恢复后达阈应 disabled")
	}
}

// TestAccountFaultFailsPersistOmitZero 计数为 0 时落盘 omitempty 不写。
func TestAccountFaultFailsPersistOmitZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSuccess("u1") // 制造一次 dirty 让 Flush 真正写盘，accountFaultFails 保持 0
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "account_fault_fails") {
		t.Errorf("accountFaultFails=0 时不应落盘:\n%s", raw)
	}
}

// TestAccountFaultFailsClearPersists 计数被成功清零后同样落盘：重启不残留旧进度。
func TestAccountFaultFailsClearPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	p.NoteSuccess("u1") // 成功清计数
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.NoteAccountFault("u1") || p2.NoteAccountFault("u1") {
		t.Fatal("清零后前 2 次不应禁用（重启后不残留旧计数）")
	}
	if !p2.NoteAccountFault("u1") {
		t.Fatal("清零后第 3 次应禁用")
	}
}

// TestAccountFaultFailsDirtyOnIncrement 未达阈值的计数变更也要标 dirty：
// 只在达阈禁用（disableLocked）时置 dirty 会让 1→2 的进度永不落盘。
func TestAccountFaultFailsDirtyOnIncrement(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.dirty.Store(false)
	p.NoteAccountFault("u1")
	if !p.dirty.Load() {
		t.Error("NoteAccountFault 累计计数应标 dirty（否则计数变更不会落盘）")
	}
}
