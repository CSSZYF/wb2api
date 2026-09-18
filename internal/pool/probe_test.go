package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 后台冷却探活（background cooldown probe）池侧：目标选择 + 成功解冻原语
// ---------------------------------------------------------------------------

// TestCooldownProbeTargetsSelectsOnlyExpiredSoftCooldown 目标选择的三条边界一次钉死：
//   - 已到期的账号级软冷却（CoolSoft + until 已过）→ 选中，且 AccountLevel=true；
//   - 未到期的账号级软冷却 → 不选（上游声明的重置墙钟是权威恢复时刻，提前试探
//     在频率×账号数下会变成常态噪音流量）；
//   - 硬冷却（CoolHard：积分耗尽）→ 绝不选（恢复条件是签到到账，探了必失败还白花配额）。
func TestCooldownProbeTargetsSelectsOnlyExpiredSoftCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "due"})
	p.Add(&auth.Auth{UID: "notyet"})
	p.Add(&auth.Auth{UID: "hard"})
	now := time.Now()

	// due：账号级软冷却已到期（until 在过去）→ 应被选中。
	p.mu.Lock()
	p.byUID["due"].coolKind = CoolSoft
	p.byUID["due"].until = now.Add(-time.Minute)
	p.byUID["due"].reason = "429 rate limit"
	p.byUID["due"].softStreak = 2
	// notyet：软冷却未到期 → 不选。
	p.byUID["notyet"].coolKind = CoolSoft
	p.byUID["notyet"].until = now.Add(time.Hour)
	// hard：硬冷却（余额耗尽）→ 绝不选。
	p.byUID["hard"].coolKind = CoolHard
	p.byUID["hard"].until = now.Add(-time.Minute) // 即便 until 已过也不探（等签到）
	p.mu.Unlock()

	targets := p.CooldownProbeTargets(now)
	if len(targets) != 1 {
		t.Fatalf("targets=%+v want 恰好 1 个（只有 due 该被探）", targets)
	}
	got := targets[0]
	if got.UID != "due" {
		t.Errorf("UID=%q want due", got.UID)
	}
	if !got.AccountLevel {
		t.Error("AccountLevel=false want true（账号级软冷却到期，探活成功应一并清账号级）")
	}
	if got.Model == "" {
		t.Error("Model 为空：探活请求必须带具体模型名")
	}
	if got.Reason != "429 rate limit" {
		t.Errorf("Reason=%q want 429 rate limit（日志可读性）", got.Reason)
	}
}

// TestCooldownProbeTargetsExpiredModelEntries 已到期的模型级冷却条目逐个产出目标：
// 多个模型同时到期 → 多个目标（各探一次）；未到期的模型条目不产出；两者混在
// 同一账号上时互不影响。
func TestCooldownProbeTargetsExpiredModelEntries(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: now.Add(-time.Hour), Reason: "6004 model rate limit"}, // 已到期
		"hy3":     {Until: now.Add(-time.Minute), Reason: "6004 model rate limit"},
		"kimi-k3": {Until: now.Add(time.Hour), Reason: "6004 model rate limit"}, // 未到期
		"deep-x":  {Until: time.Time{}, Reason: "脏数据零值"},                        // 零值脏条目
	}
	p.mu.Unlock()

	targets := p.CooldownProbeTargets(now)
	if len(targets) != 2 {
		t.Fatalf("targets=%+v want 2 个（glm-5.3 + hy3 已到期；kimi-k3 未到期、零值脏条目不探）", targets)
	}
	// 输出按模型名稳定排序（map 遍历无序，排序后便于对账与测试断言）。
	if targets[0].Model != "glm-5.3" || targets[1].Model != "hy3" {
		t.Errorf("模型顺序=%q,%q want glm-5.3,hy3（稳定排序）", targets[0].Model, targets[1].Model)
	}
	for _, tg := range targets {
		if tg.AccountLevel {
			t.Errorf("%s: AccountLevel=true want false（账号级未冷却，只有模型级条目）", tg.Model)
		}
		if tg.UID != "u1" {
			t.Errorf("UID=%q want u1", tg.UID)
		}
	}
}

// TestCooldownProbeTargetsSkipsDisabledAndMergesAccountLevel 禁用账号不探
// （已退出选号，冷却无意义）；账号级到期 + 模型级到期同时存在时**合并**为一个请求
// （首个模型目标兼任账号级解冻），不把同一 (账号,模型) 探两次。
func TestCooldownProbeTargetsSkipsDisabledAndMergesAccountLevel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "off"})
	now := time.Now()
	p.mu.Lock()
	e := p.byUID["u1"]
	e.coolKind = CoolSoft
	e.until = now.Add(-time.Minute)
	e.reason = "429 rate limit"
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: now.Add(-time.Minute), Reason: "6004 model rate limit"},
		"hy3":     {Until: now.Add(-time.Minute), Reason: "6004 model rate limit"},
	}
	p.byUID["off"].disabled = true
	p.byUID["off"].coolKind = CoolSoft
	p.byUID["off"].until = now.Add(-time.Hour)
	p.mu.Unlock()

	targets := p.CooldownProbeTargets(now)
	if len(targets) != 2 {
		t.Fatalf("targets=%+v want 2 个（账号级折叠进首个模型目标；禁用账号不探）", targets)
	}
	if targets[0].Model != "glm-5.3" || !targets[0].AccountLevel {
		t.Errorf("targets[0]=%+v want {glm-5.3 AccountLevel=true}（账号级合并到首个模型目标）", targets[0])
	}
	if targets[1].Model != "hy3" || targets[1].AccountLevel {
		t.Errorf("targets[1]=%+v want {hy3 AccountLevel=false}", targets[1])
	}
	// 合并的理由：同一 (账号,模型) 只探一次——若账号级另起一个目标，glm-5.3 会被探两次。
	seen := map[string]int{}
	for _, tg := range targets {
		seen[tg.Model]++
	}
	for m, n := range seen {
		if n > 1 {
			t.Errorf("模型 %s 被探 %d 次 want 1（账号级与模型级必须合并）", m, n)
		}
	}
}

// TestCooldownProbeSuccessClearsModelAndAccount 成功解冻：模型级条目 + 账号级冷却
// （含 softStreak）一并清；**其他模型条目原样保留**——单模型成功不构成其他模型
// 恢复的证据。熔断器与统计一概不动。
func TestCooldownProbeSuccessClearsModelAndAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.mu.Lock()
	e := p.byUID["u1"]
	e.coolKind = CoolSoft
	e.until = now.Add(-time.Minute)
	e.reason = "429 rate limit"
	e.softStreak = 3
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: now.Add(-time.Minute), Reason: "6004 model rate limit"},
		"hy3":     {Until: now.Add(time.Hour), Reason: "6004 model rate limit"}, // 仍在冷却，必须保留
	}
	e.fails = 2
	e.retryCount = 1
	e.breakerUntil = now.Add(30 * time.Minute)
	e.successCount = 7
	e.errTotal = 4
	e.sessionDeadFails = 1
	p.mu.Unlock()

	modelCleared, accountCleared := p.CooldownProbeSuccess("u1", "glm-5.3", true)
	if !modelCleared || !accountCleared {
		t.Fatalf("modelCleared=%v accountCleared=%v want true,true", modelCleared, accountCleared)
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	e = p.byUID["u1"]
	if _, ok := e.modelCooldowns["glm-5.3"]; ok {
		t.Error("glm-5.3 的模型级冷却应已被清（探活成功=该模型刚被实测证明可用）")
	}
	if _, ok := e.modelCooldowns["hy3"]; !ok {
		t.Error("hy3 的模型级冷却不得被清（单模型成功不构成其他模型恢复的证据）")
	}
	if e.coolKind != 0 || !e.until.IsZero() || e.reason != "" {
		t.Errorf("账号级冷却未清: coolKind=%v until=%v reason=%q", e.coolKind, e.until, e.reason)
	}
	if e.softStreak != 0 {
		t.Errorf("softStreak=%d want 0（与 until/coolKind 同属冷却域，恢复即清零）", e.softStreak)
	}
	// 熔断器与统计不属冷却域：探活不是用户流量，不写任何统计。
	if e.fails != 2 || e.retryCount != 1 || e.breakerUntil.IsZero() {
		t.Errorf("熔断器被改动: fails=%d retryCount=%d breakerUntil=%v", e.fails, e.retryCount, e.breakerUntil)
	}
	if e.successCount != 7 || e.errTotal != 4 || e.sessionDeadFails != 1 {
		t.Errorf("统计被改动: success=%d err=%d sessionDeadFails=%d（探活不写统计）",
			e.successCount, e.errTotal, e.sessionDeadFails)
	}
}

// TestCooldownProbeSuccessKeepsFreshCooldown 探活请求在途期间账号被并发真实请求
// 重新冷却（新 until 在未来）时，探活成功**不得**推翻更新鲜的负信号——账号级解冻
// 条件在持锁下重判，不是拿着发起探活那一刻的快照硬清。
func TestCooldownProbeSuccessKeepsFreshCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.coolKind = CoolSoft
	e.until = time.Now().Add(time.Hour) // 并发真实请求刚写入的新冷却
	e.reason = "429 rate limit (retry-after)"
	e.softStreak = 1
	p.mu.Unlock()

	_, accountCleared := p.CooldownProbeSuccess("u1", "glm-5.3", true)
	if accountCleared {
		t.Error("accountCleared=true want false（账号级冷却未到期，探活成功不得推翻并发写入的新冷却）")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.byUID["u1"].until.IsZero() {
		t.Error("新冷却被误清")
	}
}

// TestCooldownProbeSuccessNoopCases 空操作边界：uid 不存在 / model 为空 /
// 账号级未到期（accountLevel=false 时不动账号级）一律零副作用。
func TestCooldownProbeSuccessNoopCases(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.mu.Lock()
	e := p.byUID["u1"]
	e.coolKind = CoolSoft
	e.until = now.Add(-time.Minute)
	e.modelCooldowns = map[string]modelCooldown{"glm-5.3": {Until: now.Add(-time.Minute)}}
	p.mu.Unlock()

	if m, a := p.CooldownProbeSuccess("nope", "glm-5.3", true); m || a {
		t.Errorf("不存在的 uid: cleared=%v,%v want false,false", m, a)
	}
	if m, a := p.CooldownProbeSuccess("", "glm-5.3", true); m || a {
		t.Errorf("空 uid: cleared=%v,%v want false,false", m, a)
	}
	// accountLevel=false：只清模型级，账号级原样（模型级探活不构成账号级恢复的证据）。
	m, a := p.CooldownProbeSuccess("u1", "glm-5.3", false)
	if !m || a {
		t.Errorf("accountLevel=false: cleared=%v,%v want true,false", m, a)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.byUID["u1"].until.IsZero() {
		t.Error("accountLevel=false 时账号级冷却不得被清")
	}
}

// TestCooldownProbeTargetsUsesLastModel 账号级到期的目标模型选择：优先该账号最近
// 成功用过的模型（最可能仍可用），无记录时回落 probeFallbackModel。
func TestCooldownProbeTargetsUsesLastModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "withlast"})
	p.Add(&auth.Auth{UID: "norec"})
	now := time.Now()
	p.mu.Lock()
	for _, uid := range []string{"withlast", "norec"} {
		e := p.byUID[uid]
		e.coolKind = CoolSoft
		e.until = now.Add(-time.Minute)
	}
	p.byUID["withlast"].tokenUsage.LastModel = "kimi-k3"
	p.mu.Unlock()

	targets := p.CooldownProbeTargets(now)
	if len(targets) != 2 {
		t.Fatalf("targets=%+v want 2", targets)
	}
	byUID := map[string]string{}
	for _, tg := range targets {
		byUID[tg.UID] = tg.Model
	}
	if got := byUID["withlast"]; got != "kimi-k3" {
		t.Errorf("withlast 探活模型=%q want kimi-k3（优先 LastModel）", got)
	}
	if got := byUID["norec"]; got != probeFallbackModel {
		t.Errorf("norec 探活模型=%q want %q（无记录回落兜底模型）", got, probeFallbackModel)
	}
}
