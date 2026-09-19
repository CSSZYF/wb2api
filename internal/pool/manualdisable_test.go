// manualdisable_test.go 运维临时停用（上游 a20d06f / issue #138/#118）的状态位语义测试。
//
// 覆盖四条不变量：
//  1. 临时停用后账号不被选号（含全冷却兜底与探活），但仍留在池里（状态可读、
//     签到/保活路径不受阻——scheduler 只判 Status.Disabled）；
//  2. 临时停用与永久禁用互相独立——Revive/重新登录/签到解冻都不会解除运维意图，
//     反向：临时停用也不清永久禁用标记与惩罚维度；
//  3. 临时停用状态持久化，重启后保留；
//  4. 面板运维入口（SetManualDisabled/ClearManualDisabled）幂等且可回池。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// manualStateOf 曝露 entry 的临时停用双字段（包内私有 helper，与 consecutiveStateOf 同风格）。
func (p *Pool) manualStateOf(uid string) (disabled bool, reason string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return false, "", false
	}
	return e.manualDisabled, e.manualReason, true
}

// TestManualDisabledStopsSelection 停用后不参与选号（含全冷却兜底路径），
// 但仍在池里且状态可读。
func TestManualDisabledStopsSelection(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if got := p.Pick(); got == nil {
		t.Fatal("precondition: 停用前应可选")
	}

	found, changed := p.SetManualDisabled("u1", true, "观察几天")
	if !found || !changed {
		t.Fatalf("SetManualDisabled = (%v,%v), want (true,true)", found, changed)
	}

	// 仍在池里：状态可读、透出停用位与原因。
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("停用后账号不应从池里消失（状态仍应可读）")
	}
	if !st.ManualDisabled {
		t.Fatalf("manual_disabled 未置位: %+v", st)
	}
	if st.ManualReason != "观察几天" {
		t.Errorf("manual_reason=%q want %q", st.ManualReason, "观察几天")
	}
	// 反向断言：临时停用**不得**置永久禁用位，也不得写禁用原因。
	if st.Disabled {
		t.Error("临时停用不应置自动禁用位")
	}
	if st.DisabledReason != "" {
		t.Errorf("临时停用不应写 disabled_reason: %q", st.DisabledReason)
	}

	// 不参与正常选号
	if got := p.Pick(); got != nil {
		t.Fatalf("停用后不应被选中, got %+v", got)
	}
	// 也不参与全冷却兜底（pickEarliestExpiryLocked 路径）：先造一个生效冷却，
	// 否则 expiry 为零值、该账号本来就不会进兜底，断言会变成假阳性。
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	if got := p.pick(nil, "", "", false); got != nil {
		t.Fatalf("停用后不应参与兜底选号, got %+v", got)
	}

	// 计数口径：total 含它、healthy 不含、disabled 含（与永久禁用同归一类，
	// 保证 total/healthy/cooling/disabled 闭合）。
	total, healthy, _, disabled, _ := p.CountsDetailed()
	if total != 1 || healthy != 0 || disabled != 1 {
		t.Fatalf("计数 = total%d healthy%d disabled%d, want 1/0/1", total, healthy, disabled)
	}

	// 恢复后立即可选（冷却已到期）。
	if _, changed := p.ClearManualDisabled("u1"); !changed {
		t.Fatal("恢复应报告状态变化")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("恢复后应回到池子, got %+v", got)
	}
}

// TestManualDisabledIsIdempotent 重复调用幂等：第二次不报告变化、不报错。
func TestManualDisabledIsIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if _, changed := p.SetManualDisabled("u1", true, "r1"); !changed {
		t.Fatal("首次停用应报告变化")
	}
	if found, changed := p.SetManualDisabled("u1", true, "r1"); !found || changed {
		t.Fatalf("重复停用 = (%v,%v), want (true,false)", found, changed)
	}
	// 同状态但原因不同 → 仍算变化（文案要更新给运维看）
	if _, changed := p.SetManualDisabled("u1", true, "r2"); !changed {
		t.Fatal("原因变化应报告变化")
	}
	if _, reason, _ := p.ManualDisabledState("u1"); reason != "r2" {
		t.Errorf("reason=%q want r2", reason)
	}
	// 未停用的号上执行「恢复」：found=true 但无变化（面板重试友好，不报错）。
	if found, changed := p.SetManualDisabled("u1", false, ""); !found || !changed {
		t.Fatalf("首次恢复 = (%v,%v), want (true,true)", found, changed)
	}
	if found, changed := p.SetManualDisabled("u1", false, ""); !found || changed {
		t.Fatalf("重复恢复 = (%v,%v), want (true,false)", found, changed)
	}
	// 未知 uid：found=false（端点据此回 404）
	if found, _ := p.SetManualDisabled("nope", true, "x"); found {
		t.Error("未知 uid 应返回 found=false")
	}
	if _, _, ok := p.ManualDisabledState("nope"); ok {
		t.Error("未知 uid 的 ManualDisabledState 应 ok=false")
	}
}

// TestManualDisabledIndependentFromAutoDisable 与永久禁用互相独立（双向）：
// 复活路径（ReviveDisabled/Revive）都不解除临时停用；临时停用也不清永久禁用标记。
func TestManualDisabledIndependentFromAutoDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 先被系统自动禁用（连续 12153 达阈）
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已自动禁用")
	}
	// 再叠加临时停用
	p.SetManualDisabled("u1", true, "叠加")

	st, _ := p.Status("u1")
	if !st.Disabled || !st.ManualDisabled {
		t.Fatalf("叠加态应两位都为真: %+v", st)
	}
	// 叠加态两个原因分别透出（不互相覆盖）。
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("叠加态应透出自动禁用原因, got %q", st.DisabledReason)
	}
	if st.ManualReason != "叠加" {
		t.Errorf("叠加态应透出手动原因, got %q", st.ManualReason)
	}

	// 系统侧复活：只清永久禁用位，临时停用位保留 → 仍不可选
	if !p.ReviveDisabled("u1") {
		t.Fatal("ReviveDisabled 应报告清除了禁用位")
	}
	st, _ = p.Status("u1")
	if st.Disabled {
		t.Error("ReviveDisabled 应清自动禁用位")
	}
	if !st.ManualDisabled {
		t.Error("ReviveDisabled 不应清临时停用位（运维意图）")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("临时停用位仍在，不应可选, got %+v", got)
	}

	// 运维口径的无条件恢复（Revive，面板「解冻」）同样不解除临时停用：
	// 解冻一个"坏了"的号 ≠ 取消"我主动摘掉它"的决定。
	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true（账号存在）")
	}
	if md, _, _ := p.manualStateOf("u1"); !md {
		t.Error("Revive 不应清临时停用位")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("Revive 后仍应保持摘除, got %+v", got)
	}

	// 签到解冻路径（ReenableIfCredits）同样不解除临时停用
	p.ReenableIfCredits("u1", 100, 100)
	if md, _, _ := p.manualStateOf("u1"); !md {
		t.Fatal("签到解冻不应解除临时停用")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("签到回血后仍应保持摘除, got %+v", got)
	}

	// 任意成功（NoteSuccess）也不解除——停用号不会有真实成功流量，但会话粘性等
	// 旁路仍可能写入成功事件，这里锁死"成功不清运维意图"。
	p.NoteSuccess("u1")
	if md, _, _ := p.manualStateOf("u1"); !md {
		t.Fatal("NoteSuccess 不应解除临时停用")
	}

	// 只有显式恢复才回池
	p.ClearManualDisabled("u1")
	if got := p.Pick(); got == nil {
		t.Fatal("恢复后应回池")
	}
}

// TestManualDisabledDoesNotClearAutoDisable 反向断言：临时停用/恢复**都不清**
// 永久禁用标记——两个方向的独立性都要咬合（只测一边会让「顺手清对方」的实现漏网）。
func TestManualDisabledDoesNotClearAutoDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")

	// 停用：不动 disabled / disabled_reason
	if _, changed := p.SetManualDisabled("u1", true, "叠加"); !changed {
		t.Fatal("首次停用应报告变化")
	}
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Fatalf("临时停用不得清永久禁用位/原因: %+v", st)
	}

	// 恢复：同样不动 disabled——只清自己那一位。
	if _, changed := p.ClearManualDisabled("u1"); !changed {
		t.Fatal("恢复应报告变化")
	}
	st, _ = p.Status("u1")
	if st.ManualDisabled || st.ManualReason != "" {
		t.Fatalf("恢复应清临时停用位与原因: %+v", st)
	}
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Fatalf("恢复临时停用不得连带清永久禁用: %+v", st)
	}
	// 仍不可选（永久禁用还在），复活后才回池。
	if got := p.Pick(); got != nil {
		t.Fatalf("永久禁用仍在，不应可选, got %+v", got)
	}
	p.ReviveDisabled("u1")
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("复活后应回池, got %+v", got)
	}
}

// TestManualDisabledPreservesPenaltyDimensions 停用不碰冷却/熔断/降权维度：
// 恢复后拿到的是停用期间真实发生的状态，而不是被清空的一刀切
// （与 disableLocked 清冷却域形成对照）。
func TestManualDisabledPreservesPenaltyDimensions(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.Cooldown("u1", CoolSoft, 10*time.Minute, "429 rate limit")
	p.SetBreaker(3, time.Hour, time.Hour)
	p.NoteError("u1")
	p.SetDegrade(5, 10*time.Minute, 2*time.Hour)
	p.NoteFailures("u1")

	before, _ := p.Status("u1")
	if _, changed := p.SetManualDisabled("u1", true, "临时摘除"); !changed {
		t.Fatal("停用应报告变化")
	}
	after, _ := p.Status("u1")

	if !after.Until.Equal(before.Until) {
		t.Errorf("停用不应改冷却截止: %v → %v", before.Until, after.Until)
	}
	if after.BreakerFails != before.BreakerFails || !after.BreakerUntil.Equal(before.BreakerUntil) {
		t.Errorf("停用不应改熔断状态: %+v → %+v", before, after)
	}
	if after.ConsecutiveFails != before.ConsecutiveFails || !after.DegradeUntil.Equal(before.DegradeUntil) {
		t.Errorf("停用不应改连败降权: %+v → %+v", before, after)
	}
	if after.CoolKind != before.CoolKind || after.SoftStreak != before.SoftStreak {
		t.Errorf("停用不应改冷却种类/退避指数: %+v → %+v", before, after)
	}

	// 恢复后惩罚维度原样（说明停用期间确实没被清）。
	p.ClearManualDisabled("u1")
	post, _ := p.Status("u1")
	if !post.Until.Equal(before.Until) || post.BreakerFails != before.BreakerFails {
		t.Errorf("恢复后惩罚维度应原样保留: %+v → %+v", before, post)
	}
}

// TestManualDisabledPersists 临时停用落盘持久化——重启保留运维意图
// （这正是该功能要解决的痛点：旧权宜做法改 state.json 会被 5s flush 覆盖）。
func TestManualDisabledPersists(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetManualDisabled("u1", true, "重启也要保留")
	p.Flush()

	// 模拟重启：新池读同一 state 文件
	p2 := New(state)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后应能读到状态")
	}
	if !st.ManualDisabled {
		t.Fatal("重启后临时停用状态丢失")
	}
	if st.ManualReason != "重启也要保留" {
		t.Errorf("manual_reason=%q 未保留", st.ManualReason)
	}
	// 反向：重启不得把临时停用位变成永久禁用位。
	if st.Disabled {
		t.Error("重启后不应变成永久禁用（两位独立落盘）")
	}
	// 未停用的账号不受影响
	if st2, _ := p2.Status("u2"); st2.ManualDisabled {
		t.Error("u2 不应被连带停用")
	}
	// 重启后仍不可选
	if got := p2.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("重启后应只能选到 u2, got %+v", got)
	}
	// 恢复后落盘，再重启确认已清
	p2.ClearManualDisabled("u1")
	p2.Flush()
	p3 := New(state)
	p3.Add(&auth.Auth{UID: "u1"})
	if st3, _ := p3.Status("u1"); st3.ManualDisabled {
		t.Fatal("清除后重启不应复活临时停用位")
	}
}

// TestManualDisabledRealmScoped 停用某个域的账号不影响另一域。
func TestManualDisabledRealmScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "gl1", Domain: "www.workbuddy.ai"})

	p.SetManualDisabled("cn1", true, "只摘 CN")

	if got := p.pick(nil, "", "cn", false); got != nil {
		t.Fatalf("CN 域应无可选账号, got %+v", got)
	}
	if got := p.pick(nil, "", "global", false); got == nil || got.UID != "gl1" {
		t.Fatalf("Global 域应不受影响, got %+v", got)
	}
}

// TestManualDisableServable 临时停用对探活谓词的行为锚：modelExempt 是旁路谓词中
// 最容易漏改的一条（healthy/pickEarliestExpiryLocked 已被上面的用例咬合）。
// 锁死两个形态：
//  1. 唯一号有 6004 模型级冷却（modelExempt 形态，本应计入 ServableNow）→
//     临时停用后 ServableNow/ServableForRealm 必须 false；
//  2. 恢复后探活回归 true。
func TestManualDisableServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 6004 模型级冷却：u1 是「未禁用、未熔断、存在模型冷却条目」的豁免形态，
	// 无临时停用时 ServableNow 因 modelExempt 为 true。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if !p.ServableNow() {
		t.Fatal("precondition: 模型豁免形态应 ServableNow=true")
	}

	p.SetManualDisabled("u1", true, "摘除")
	if p.ServableNow() {
		t.Error("唯一号临时停用后 ServableNow 应 false（modelExempt 排除临时停用号）")
	}
	if p.ServableForRealm("cn") {
		t.Error("唯一号临时停用后 ServableForRealm(cn) 应 false")
	}
	// 探活也不该挑它做目标：探活成功也换不回可用性，纯浪费上游配额。
	if got := p.CooldownProbeTargets(time.Now()); len(got) != 0 {
		t.Errorf("临时停用号不应进探活目标, got %+v", got)
	}

	// 恢复后探活回归
	p.ClearManualDisabled("u1")
	if !p.ServableNow() {
		t.Error("解除临时停用后 ServableNow 应回归 true")
	}
}

// TestManualDisabledDoesNotBlockTokenLifecycle 临时停用不影响凭证生命周期路径：
// AuthByUID 照常可读（签到/保活要拿凭证打上游），状态计数照常（积分台账不断更）。
// 这是「对话流量摘除」而非「账号冻结」的核心可观测差异。
func TestManualDisabledDoesNotBlockTokenLifecycle(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)

	p.SetManualDisabled("u1", true, "观察")

	if got := p.AuthByUID("u1"); got == nil {
		t.Fatal("临时停用号仍应可取凭证（签到/保活要打上游）")
	}
	// 积分回填照常（scheduler 签到路径会调用）。
	p.ReenableIfCredits("u1", 500, 1000)
	st, _ := p.Status("u1")
	if st.Credits != 500 {
		t.Errorf("临时停用号积分应照常回填, got %d want 500", st.Credits)
	}
	if !st.ManualDisabled {
		t.Error("积分回填不得解除临时停用")
	}
	// AvailableUIDs（粘性路由候选）不含它。
	for _, uid := range p.AvailableUIDs() {
		if uid == "u1" {
			t.Error("临时停用号不应出现在 AvailableUIDs")
		}
	}
	// 但粘性直取（PickByUID）也拦得住——否则绑定的会话会一直打到摘除号上。
	if got := p.PickByUID("u1"); got != nil {
		t.Errorf("临时停用号不应被粘性直取命中, got %+v", got)
	}
}
