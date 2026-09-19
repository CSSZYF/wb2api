// rateexhaust_test.go 末端「模型级限流耗尽」判定（ModelRateLimitExhausted，v1.9.19）。
//
// 这是末端 429 + Retry-After 的**唯一判据**，边界必须逐条咬合：
//   - 池真空 / 该域无候选 / 全禁用 → false（保持 503，关键反向断言）；
//   - 任一候选没有该模型的限流冷却 → false（失败另有原因，不得谎报 429）；
//   - 全部候选都因该模型限流出局 → true + 最早恢复时刻（多账号取 min）；
//   - 11102「该后端无此模型」条目不算限流 → false（退避多久都不会变好）；
//   - 账号级冷却不参与判定（不是"模型被限流"）。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelRateLimitExhaustedEmptyPool 池真空 → false（保持 503 的反向断言）。
// 池里一个号都没有时，「所有候选都因模型冷却出局」是**空真**（vacuous truth），
// 若不用 cands==0 显式拦掉，就会把「池子空了」谎报成 429 让客户端白等。
func TestModelRateLimitExhaustedEmptyPool(t *testing.T) {
	p := New("")
	if wait, ok := p.ModelRateLimitExhausted("glm-5.3", ""); ok {
		t.Fatalf("空池不得判为模型级限流耗尽, wait=%v", wait)
	}
}

// TestModelRateLimitExhaustedAllDisabled 池内号全禁用/临时停用 → false：
// 「池子本身没有可用号」是 503 语义，不是「模型被限流」。
func TestModelRateLimitExhaustedAllDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Disable("u1", "test")
	p.SetManualDisabled("u2", true, "test")
	if wait, ok := p.ModelRateLimitExhausted("glm-5.3", ""); ok {
		t.Fatalf("全禁用池不得判为模型级限流耗尽, wait=%v", wait)
	}
}

// TestModelRateLimitExhaustedAnyHealthyCandidate 只要有一个候选没有该模型的限流冷却
// → false：池里明明有号能服务这个模型，本次失败另有原因（账号级冷却/传输层/5xx 等），
// 回 429 会让客户端退避一个根本不存在的"模型限流窗口"。
func TestModelRateLimitExhaustedAnyHealthyCandidate(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	// 只有 u1 对该模型限流，u2 干净。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")
	if wait, ok := p.ModelRateLimitExhausted("glm-5.3", ""); ok {
		t.Fatalf("尚有干净候选（u2）不得判为耗尽, wait=%v", wait)
	}
	// 账号级冷却不算「模型被限流」：u2 账号级冷却中但无该模型条目 → 仍 false。
	p.Cooldown("u2", CoolSoft, time.Hour, "429 rate limit")
	if wait, ok := p.ModelRateLimitExhausted("glm-5.3", ""); ok {
		t.Fatalf("账号级冷却不得判为模型级限流耗尽, wait=%v", wait)
	}
}

// TestModelRateLimitExhaustedAllCooledSameModel 核心正向：池内所有候选都因**同一模型**
// 的 6004 冷却出局 → true，且 wait 取**最早**恢复时刻（客户端按最短可恢复时间退避）。
func TestModelRateLimitExhaustedAllCooledSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	now := time.Now()
	// u1 要等 40 分钟，u2 只要等 10 分钟 → wait 必须取 10 分钟那个。
	p.CooldownSoftForModel("u1", time.Minute, now.Add(40*time.Minute), "glm-5.3", "6004 model rate limit")
	p.CooldownSoftForModel("u2", time.Minute, now.Add(10*time.Minute), "glm-5.3", "6004 model rate limit")

	wait, ok := p.ModelRateLimitExhausted("glm-5.3", "")
	if !ok {
		t.Fatal("全候选因同一模型冷却出局应判为耗尽")
	}
	// 取 min：应落在 u2 的 10 分钟附近（留足时钟漂移余量）。
	if wait < 9*time.Minute || wait > 10*time.Minute {
		t.Errorf("wait=%v want ~10m（多账号取最早恢复时刻）", wait)
	}
	// 另一个模型不受影响（模型级冷却的既有豁免语义）。
	if _, ok := p.ModelRateLimitExhausted("hy3-x", ""); ok {
		t.Error("未被限流的模型不得判为耗尽（模型级独立）")
	}
}

// TestModelRateLimitExhaustedIgnoresModelBlocked 11102「该后端无此模型」条目不算限流：
// 与 6004 共用 modelCooldowns 承载，但语义正交——模型不存在时退避多久都不会变好
// （TTL 6h 起、封顶 24h），回 429 + Retry-After 等于让客户端白等数小时而不是换模型。
func TestModelRateLimitExhaustedIgnoresModelBlocked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "deepseek-v3-2-volc", "11102 model not available")
	if wait, ok := p.ModelRateLimitExhausted("deepseek-v3-2-volc", ""); ok {
		t.Fatalf("11102 负缓存不得判为限流耗尽（应保持 503）, wait=%v", wait)
	}
	// 混排：一个号 11102、另一个号 6004 限流 → 仍有候选"不是限流"（11102 那个
	// 语义上不是限流）→ false，不谎报可退避窗口。
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownSoftForModel("u2", time.Minute, time.Now().Add(30*time.Minute), "deepseek-v3-2-volc", "6004 model rate limit")
	if wait, ok := p.ModelRateLimitExhausted("deepseek-v3-2-volc", ""); ok {
		t.Fatalf("11102 与 6004 混排时不得判为限流耗尽, wait=%v", wait)
	}
}

// TestModelRateLimitExhaustedExpiredCooldown 冷却已到期 → false：条目到期即不再是
// "出局原因"，选号器会重新放行该号，宣称可退避是假信号。
//
// 直接写 modelCooldowns 而不经 CooldownSoftForModel：后者对**已过去**的重置墙钟有
// 「钳到 now+1ms 立即恢复」的既有语义（见 cappedSoftUntilLocked），构造不出真正
// 已过期的条目（与 modelcooldown_test 的既有手法一致）。
func TestModelRateLimitExhaustedExpiredCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Hour), Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()
	if wait, ok := p.ModelRateLimitExhausted("glm-5.3", ""); ok {
		t.Fatalf("已到期的冷却不得判为耗尽, wait=%v", wait)
	}
	// 选号器同口径放行（到期即恢复，两个谓词不得裂缝）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Fatalf("到期后该模型应重新可选, got %+v", got)
	}
}

// TestModelRateLimitExhaustedRealmScope realm 谓词与选号器同口径：显式前缀场景下
// 只按该域判定（本域耗尽 → true，即便另一域干净），空 realm 按全池判定。
func TestModelRateLimitExhaustedRealmScope(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"}) // global
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn"}) // cn
	p.CooldownSoftForModel("g1", time.Minute, time.Now().Add(20*time.Minute), "glm-5.3", "6004 model rate limit")

	// 显式 global 前缀：本域只有 g1，它被限流 → 耗尽。
	if _, ok := p.ModelRateLimitExhausted("glm-5.3", "global"); !ok {
		t.Error("global 域唯一号被限流应判为耗尽")
	}
	// cn 域候选干净 → 不耗尽。
	if _, ok := p.ModelRateLimitExhausted("glm-5.3", "cn"); ok {
		t.Error("cn 域候选干净不得判为耗尽")
	}
	// 空 realm = 全池：cn 号干净 → 不耗尽（裸名归属会跨域回落）。
	if _, ok := p.ModelRateLimitExhausted("glm-5.3", ""); ok {
		t.Error("全池判定下另一域干净不得判为耗尽")
	}
}

// TestModelRateLimitExhaustedEmptyModel 空模型名无判定语义 → false（不猜）。
func TestModelRateLimitExhaustedEmptyModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if _, ok := p.ModelRateLimitExhausted("", ""); ok {
		t.Error("空模型名不得判为耗尽")
	}
}
