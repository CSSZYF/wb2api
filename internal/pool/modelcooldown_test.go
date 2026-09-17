package pool

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 6004 模型级 limit 独立冷却（issue：多模型独立计时）
// ---------------------------------------------------------------------------

// TestModelCooldownsIndependent 核心：模型 A 触发 6004（重置 2h 后），模型 B 再触发
// 6004（重置 1h 后）→
//  1. A 的冷却独立保留：1h 后 A 仍在限额中、B 已恢复；
//  2. until（全账号级）不被任何模型的 6004 覆盖。
func TestModelCooldownsIndependent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	base := time.Now()
	resetA := base.Add(2 * time.Hour)
	resetB := base.Add(1 * time.Hour)

	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x":   {Until: resetB, ResetAt: resetB},
	}
	p.mu.Unlock()

	now := base.Add(90 * time.Minute)
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatalf("90m 后 A(glm-5.3, reset 2h) 仍应限额中，但 healthyForModel 放行了")
	}
	if !e.healthyForModel(now, "hy3-x") {
		t.Fatalf("90m 后 B(hy3-x, reset 1h) 应已恢复，但 healthyForModel 仍拦截")
	}
	if !e.until.IsZero() {
		t.Errorf("until=%v 应为零值（6004 模型级冷却不写 until）", e.until)
	}
}

// TestModelCooldownsBDoesNotOverwriteA 模型 B 触发 6004 后，A 的冷却截止不被覆盖：
// 这是本 issue 的核心——旧实现用单 until 字段，B 会覆盖 A。
func TestModelCooldownsBDoesNotOverwriteA(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	// 模拟真实路径两次 6004：A(2h) 然后 B(1h)。
	p.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(1*time.Hour), "hy3-x", "6004 model rate limit")

	p.mu.RLock()
	e := p.byUID["u1"]
	mcA, okA := e.modelCooldowns["glm-5.3"]
	until := e.until
	p.mu.RUnlock()
	if !okA {
		t.Fatalf("A(glm-5.3) 的模型冷却条目丢失（被 B 覆盖？）")
	}
	if d := mcA.Until.Sub(resetA); d < -time.Second || d > time.Second {
		t.Errorf("A until=%v want ~%v（B 的冷却不得覆盖 A 的截止）", mcA.Until, resetA)
	}
	if !until.IsZero() {
		t.Errorf("until=%v 应为零值（6004 从不写账号级 until）", until)
	}
}

// TestCooldownSoftForModelDoesNotClobberUntil 带解析时间的 6004 不写 until
// （否则全账号级冷却被模型重置时间污染），只写 modelCooldowns[model]，且不推进
// softStreak（有上游权威重置时间时绝不做指数堆加）。
func TestCooldownSoftForModelDoesNotClobberUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	e := p.byUID["u1"]
	until := e.until
	streak := e.softStreak
	mc, ok := e.modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !until.IsZero() {
		t.Errorf("until=%v 应零值（6004 不写 until）", until)
	}
	if streak != 0 {
		t.Errorf("soft_streak=%d 应保持 0（有重置时间绝不指数堆加）", streak)
	}
	if !ok || mc.Until.IsZero() {
		t.Errorf("modelCooldowns[glm-5.3]=%+v ok=%v，应已记录模型冷却", mc, ok)
	}
}

// TestCooldownSoftForModelCapsUntilKeepsResetAt 6004 写 modelCooldowns：
// until 截断到 soft_rate_max，reset_at 保留上游原始墙钟（issue #36 台账语义迁移）；
// 且不推进 softStreak（截断是封顶而非退避）。
func TestCooldownSoftForModelCapsUntilKeepsResetAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超封顶 → until 截断到 10m，reset_at 保留 2h
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	mc, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	streak := p.byUID["u1"].softStreak
	p.mu.RUnlock()
	if !ok {
		t.Fatal("modelCooldowns 缺少 glm-5.3")
	}
	if streak != 0 {
		t.Errorf("soft_streak=%d 应保持 0（有重置时间绝不指数堆加）", streak)
	}
	if rem := mc.Until.Sub(time.Now()); rem <= 0 || rem > 10*time.Minute+time.Second {
		t.Errorf("Until 应在 (0,10m] 区间，实际剩余 %v", rem)
	}
	if d := mc.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("ResetAt=%v want ~2h 后=%v", mc.ResetAt, reset)
	}
}

// TestHealthyForModelAfterModelSpecific6004 6004 只锁该模型：
// 账号对触发模型不可选、对其他模型仍可选（issue #31 豁免保持）；账号级 healthy 仍真。
func TestHealthyForModelAfterModelSpecific6004(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	now := time.Now()
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatal("触发模型 glm-5.3 应不可选")
	}
	if !e.healthyForModel(now, "hy3-x") {
		t.Fatal("其他模型 hy3-x 应可选（模型豁免）")
	}
	if !e.healthy(now) {
		t.Fatal("账号级 healthy 应仍 true（6004 只锁模型，不锁账号）")
	}
}

// TestModelCooldownsTwoLimitsBothBlock 同一账号两个模型同时 6004：这两个模型都不可选
// （无账号级冷却），其他模型仍可选。
func TestModelCooldownsTwoLimitsBothBlock(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(2 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x":   {Until: resetB, ResetAt: resetB},
	}
	p.mu.Unlock()

	now := time.Now()
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatal("glm-5.3 应被自身冷却拦截")
	}
	if e.healthyForModel(now, "hy3-x") {
		t.Fatal("hy3-x 应被自身冷却拦截")
	}
	if !e.healthyForModel(now, "other") {
		t.Fatal("other 模型应可选（多模型限流不应让账号级不可选）")
	}
}

// TestModelCooldownsPreservedByNoteSuccess 成功（NoteSuccess）不得清除模型级 6004 冷却：
// 若清除，B 模型成功会抹掉 A 模型的独立冷却——正是本 issue 要修的核心缺陷。
func TestModelCooldownsPreservedByNoteSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	p.NoteSuccess("u1") // 其他模型成功
	p.mu.RLock()
	mc, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatalf("NoteSuccess 后 A(glm-5.3) 独立冷却被清除——模型独立性被破坏")
	}
	if d := mc.Until.Sub(resetA); d < -time.Second || d > time.Second {
		t.Errorf("A until=%v want ~%v（不得被 NoteSuccess 干扰）", mc.Until, resetA)
	}
}

// TestModelCooldownsKeptByReenable issue #199 语义收窄：余额刷新/签到（ReenableIfCredits）
// **不再**清模型级 6004 冷却——仅 6004 冷却的账号 coolKind 未设（零值 = CoolHard），
// 旧实现按 remain > 0 无条件解冻会把它误当硬冷却清掉 modelCooldowns，模型级豁免被
// 刷新抹掉。收窄后模型级冷却原样保留，直到到期或人工 Revive。
func TestModelCooldownsKeptByReenable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.ReenableIfCredits("u1", 500, 0)
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	p.mu.RUnlock()
	if n != 1 {
		t.Errorf("ReenableIfCredits 后 modelCooldowns=%d want 1（模型级冷却不被余额刷新清，issue #199）", n)
	}
	// 人工强制解冻（Revive）才清模型级冷却。
	p.Revive("u1")
	p.mu.RLock()
	n = len(p.byUID["u1"].modelCooldowns)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("Revive 后 modelCooldowns=%d want 0（人工解冻清一切）", n)
	}
}

// TestModelCooldownsLazyCleanup 已过期的模型冷却在 pick（写锁路径）时被惰性清理。
func TestModelCooldownsLazyCleanup(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"old": {Until: time.Now().Add(-time.Minute), ResetAt: time.Now().Add(-time.Minute)},
	}
	p.mu.Unlock()
	got := p.PickExcludingForModel(nil, "fresh")
	if got == nil || got.UID != "u1" {
		t.Fatalf("过期模型冷却不应拦截 u1, got %+v", got)
	}
	p.mu.RLock()
	_, still := e.modelCooldowns["old"]
	p.mu.RUnlock()
	if still {
		t.Error("过期模型冷却条目应在 pick 时被清理")
	}
}

// TestModelCooldownsExpiredAllowsSameModel 过期后同模型请求也放行（read 路径无清理也可选）。
func TestModelCooldownsExpiredAllowsSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Nanosecond), ResetAt: time.Now().Add(-time.Nanosecond)},
	}
	p.mu.Unlock()
	if !e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("过期后同模型请求应放行")
	}
}

// TestRateLimitedModelsMultiModel 6004 多模型同时限流 → /status 台账全部展示。
func TestRateLimitedModelsMultiModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "6004 model rate limit"},
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if len(st.RateLimitedModels) != 2 {
		t.Fatalf("rate_limited_models=%+v want 2 行", st.RateLimitedModels)
	}
	wantModels := map[string]bool{"glm-5.3": true, "hy3-x": true}
	for _, row := range st.RateLimitedModels {
		if !wantModels[row.Model] {
			t.Errorf("unexpected row model=%q", row.Model)
		}
		delete(wantModels, row.Model)
	}
	if len(wantModels) != 0 {
		t.Errorf("缺行: %v", wantModels)
	}
}

// TestRateLimitedModelsMultiModelStableOutput 多模型台账行按模型名排序（稳定输出）。
func TestRateLimitedModelsMultiModelStableOutput(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "r"},
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
	}
	p.mu.Unlock()
	st, _ := p.Status("u1")
	got := []string{st.RateLimitedModels[0].Model, st.RateLimitedModels[1].Model}
	want := []string{"glm-5.3", "hy3-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows=%v want sorted %v", got, want)
	}
}

// TestRateLimitedModelsEachModelHasOwnUntil 台账行 Until = 该模型独立冷却截止，
// Status.Until（账号级）不受 6004 影响（无账号级冷却时为零值）。
func TestRateLimitedModelsEachModelHasOwnUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "r"},
	}
	p.mu.Unlock()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if !st.Until.IsZero() {
		t.Fatalf("Status.Until 应零值（无账号级冷却），got %v", st.Until)
	}
	for _, row := range st.RateLimitedModels {
		want := resetA
		if row.Model == "hy3-x" {
			want = resetB
		}
		if d := row.Until.Sub(want); d < -time.Second || d > time.Second {
			t.Errorf("%s row.Until=%v want ~%v", row.Model, row.Until, want)
		}
	}
}

// TestModelCooldownsPickSkipsLimitedModel 被模型 X 6004 的账号，请求 X 时选到别的号，
// 请求其他模型时可选到该号（模型豁免进入 normal 选号）。
func TestModelCooldownsPickSkipsLimitedModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u2" {
		t.Fatalf("glm-5.3 请求应跳过 u1, got %+v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("hy3-x 请求应豁免 u1, got %+v", got)
	}
}

// TestServableNowModelCooldownStillServable 单模型 6004 限流不破坏探活（池还可服务），
// 全账号冷却（until）则不可服务。
func TestServableNowModelCooldownStillServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if !p.ServableNow() {
		t.Fatal("6004 模型冷却不锁账号，ServableNow 应 true")
	}
}

// ---------------------------------------------------------------------------
// healthyForModel 优先级（全账号级先判，模型级 6004 后判）
// ---------------------------------------------------------------------------

// TestHealthyForModelAccountCooledBeatsModelNotCooled 全账号冷却（until）优先于模型
// 独立冷却：账号级 until 未到期的账号，即使该模型没有 6004 独立冷却也不可选
// （旧实现先查 modelCooled 再查 healthy，逻辑上等价的短路位置不同；锁死新语义：
// 全账号冷却优先）。
func TestHealthyForModelAccountCooledBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // 全账号级 until 冷却，无 modelCooldowns
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("全账号 until 冷却中且模型无独立冷却：该模型也不可选（全账号冷却优先）")
	}
	if e.healthyForModel(time.Now(), "") {
		t.Fatal("空模型名同样不可选（等价 healthy 短路到全账号冷却）")
	}
}

// TestHealthyForModelAccountCooledWithModelCooldownStillBlocked 全账号冷却 + 该模型
// 也有 6004 独立冷却 → 不可选（无论哪条拦截都一致，优先级短路不误放行）。
func TestHealthyForModelAccountCooledWithModelCooldownStillBlocked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("全账号冷却 + 模型独立冷却双拦截，仍应不可选")
	}
}

// TestHealthyForModelDisabledBeatsModelNotCooled disabled 是全账号级的最强冷却：
// 即使模型没有独立冷却也永不可选（disabled > 模型级）。
func TestHealthyForModelDisabledBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("disabled 账号即使模型无独立冷却也不可选")
	}
}

// TestHealthyForModelBreakerBeatsModelNotCooled 熔断（breakerUntil）是全账号级的
// 冷却：熔断期内即使模型没有独立冷却也不可选。
func TestHealthyForModelBreakerBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断
	p.mu.RLock()
	e := p.byUID["u1"]
	bt := e.breakerUntil
	p.mu.RUnlock()
	if bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("熔断期内即使模型无独立冷却也不可选")
	}
}

// TestHealthyForModelHealthyAccountNoModelCooldown 全账号健康 + 无任何模型独立冷却 →
// 所有模型都可选（基线）。
func TestHealthyForModelHealthyAccountNoModelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	for _, m := range []string{"glm-5.3", "hy3-x", ""} {
		if !e.healthyForModel(time.Now(), m) {
			t.Errorf("健康账号对模型 %q 应可选", m)
		}
	}
}

// TestHealthyForModelAccountCooledAllowsNothingAfterUntil 优先级锁死的另一端：
// 全账号 until 冷却到期后，模型无独立冷却的请求恢复正常（模型级判定接管）。
func TestHealthyForModelAccountCooledAllowsNothingAfterUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	// until 尚未到期：不可选（优先级：全账号级先判）。
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until 冷却内不可选")
	}
	// 等 until 过期：模型无独立冷却 → 恢复可选。
	time.Sleep(10 * time.Millisecond)
	if !e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until 过期后应恢复可选")
	}
}

// TestHealthyForModelPriorityViaPick 端到端：全账号冷却中的账号即使对某模型无独立冷却
// 也不被选出；健康 + 仅该模型 6004 的账号走模型豁免（同一账号对不同模型口径分离）。
func TestHealthyForModelPriorityViaPick(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "exempt"})
	p.SetCredits("cooled", 100, 0)
	p.SetCredits("exempt", 50, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 cooled
	p.Cooldown("cooled", CoolSoft, time.Hour, "429")    // 全账号级冷却，无模型级记录
	p.CooldownSoftForModel("exempt", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	// 请求 other：cooled 被全账号冷却拦截（即使无模型独立冷却），exempt 模型豁免
	// （6004 只锁 glm-5.3）→ 唯一候选 exempt。
	if got := p.PickExcludingForModel(nil, "other"); got == nil || got.UID != "exempt" {
		t.Fatalf("other 模型请求应豁免 exempt（全账号冷却的 cooled 仍拦截），got %+v", got)
	}
	// 请求 glm-5.3：cooled 全账号冷却拦截；exempt 自身 6004 拦截 → 无健康候选 →
	// 全冷却兜底只认账号级冷却（exempt 无 until/breakerUntil,expiry 零值被排除），
	// 选 cooled（软冷却参与兜底）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "cooled" {
		t.Fatalf("glm-5.3 请求：exempt 被自身 6004 拦截，兜底应选全账号冷却的 cooled，got %+v", got)
	}
}

// TestModelCooldownsPersistRoundTrip modelCooldowns 现已持久化：落盘 → 重启 → 恢复。
//
// **语义反转**（v1.9.10 起）：本用例原为 TestModelCooldownsNotPersisted——反向锁定
// 「运行态、不持久化、重启清零」的旧语义。6004 改成精确对齐上游重置墙钟后，单模型
// 冷却可长达数小时，跨重启是常态；不持久化等于每次重启都让 healthyForModel 失忆、
// 重新踩一遍所有 6004 雷区（选号撞限流号耗尽 MaxRotate → 429 rate_limit_exceeded）。
// 故改为断言持久化 + 往返字段一致：重启不再失忆。
func TestModelCooldownsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 带解析时间 6004 → 写入 modelCooldowns[glm-5.3]，Until 在未来。
	reset := time.Now().Add(5 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 model rate limit")
	p.Flush()

	// 重启：重新从同一份 state.json 加载，modelCooldowns 应恢复。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	mc, ok := p2.byUID["u1"].modelCooldowns["glm-5.3"]
	p2.mu.RUnlock()
	if !ok {
		t.Fatal("重启后 modelCooldowns[glm-5.3] 缺失（持久化未恢复）")
	}
	if d := mc.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("恢复后 model until=%v want ~reset=%v (diff %v)", mc.Until, reset, d)
	}
	if !mc.ResetAt.Equal(reset) {
		t.Errorf("恢复后 model reset_at=%v want %v", mc.ResetAt, reset)
	}
	if mc.Reason != "6004 model rate limit" {
		t.Errorf("恢复后 model reason=%q want %q", mc.Reason, "6004 model rate limit")
	}
	// 重启后该模型仍被拦（不因重启失忆），其他模型照常可选（模型豁免保留）。
	p2.mu.RLock()
	e := p2.byUID["u1"]
	stillCooled := e.healthyForModel(time.Now(), "glm-5.3")
	otherHealthy := e.healthyForModel(time.Now(), "hy3-x")
	p2.mu.RUnlock()
	if stillCooled {
		t.Error("重启恢复后 healthyForModel(glm-5.3) 应 false（模型级冷却未失忆）")
	}
	if !otherHealthy {
		t.Error("重启恢复后 healthyForModel(hy3-x) 应 true（切模型豁免保留）")
	}
}

// TestModelCooldownsPersistExpiryFilter 落盘与恢复都做过期过滤：只有 Until 在未来的
// 条目写出/恢复，过期与零值条目被惰性丢弃（与 pick 路径的 pruneExpiredModelCooldowns
// 同口径），避免 state.json 残留已过期的模型级冷却、重启后把陈旧条目重新装回内存。
func TestModelCooldownsPersistExpiryFilter(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"future": {Until: time.Now().Add(time.Hour), ResetAt: time.Now().Add(2 * time.Hour), Reason: "6004 future"},
		"past":   {Until: time.Now().Add(-time.Hour), ResetAt: time.Now().Add(-time.Hour), Reason: "6004 past"},
		"zero":   {Until: time.Time{}, ResetAt: time.Time{}, Reason: "6004 zero"},
	}
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	// 落盘侧过滤：过期/零值条目不应写出。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json should persist model_cooldowns:\n%s", raw)
	}
	if strings.Contains(string(raw), "6004 past") || strings.Contains(string(raw), "6004 zero") {
		t.Errorf("已过期/零值条目不应落盘（惰性清理）:\n%s", raw)
	}

	// 恢复侧过滤：past/zero 应被丢弃，只恢复 future。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	mcs := p2.byUID["u1"].modelCooldowns
	n := len(mcs)
	_, futureOK := mcs["future"]
	_, pastOK := mcs["past"]
	_, zeroOK := mcs["zero"]
	p2.mu.RUnlock()
	if n != 1 || !futureOK {
		t.Fatalf("恢复后 modelCooldowns=%+v want 仅 future 1 条", mcs)
	}
	if pastOK {
		t.Error("过期条目 past 不应恢复")
	}
	if zeroOK {
		t.Error("零值 Until 条目 zero 不应恢复")
	}
}

// TestModelCooldownsPersistCompatOldState 旧 state.json 无 modelCooldowns 字段正常加载
// （缺字段零值，模型冷却清零退化为账号级，向后兼容）。
func TestModelCooldownsPersistCompatOldState(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	old := `{"accounts":{"u1":{"credits":100,"until":"2099-01-01T00:00:00Z","cool_kind":1}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	until := p.byUID["u1"].until
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("旧文件加载后 modelCooldowns=%d want 0", n)
	}
	if until.IsZero() {
		t.Error("旧文件 until 应照常加载（账号级冷却兼容）")
	}
}
