package pool

// inflight_global_test.go global 域在途上限分档（WAF 403 修复 P1-1）。
//
// 背景：global 域（workbuddy.ai）WAF 风控更紧，单号并发需比 CN 压得更低。
// Pool 新增 maxInFlightGlobal 档 + SetMaxInFlightGlobal 注入，inFlightLimit 按
// e.a.Realm() 分档（global → global 档，其余 → maxInFlight）；未设置（0）回落
// maxInFlight 不分档（既有部署零回归）。Acquire 与 inFlightFull（Pick/ServableNow/
// Counts 共用路径）全部走 inFlightLimit，单处定义不重复造轮子。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestInFlightGlobalTiering global 域在途上限分档：maxInFlightGlobal=2 +
// maxInFlight=3 时，global 号第 3 次 Acquire 被拒、cn 号第 3 次仍成功
// （cn 档不受影响）。global 风控更紧压低其单号并发，cn 维持原上限零回归。
func TestInFlightGlobalTiering(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	// global 档 2：前两次成功、第三次被拒。
	if !p.Acquire("g1") || !p.Acquire("g1") {
		t.Fatal("global first two acquires should succeed (tier 2)")
	}
	if p.Acquire("g1") {
		t.Fatal("global third acquire should fail (tier 2 < max_in_flight 3)")
	}
	// cn 档 3（未分档回落 maxInFlight）：第三次仍成功。
	if !p.Acquire("cn1") || !p.Acquire("cn1") || !p.Acquire("cn1") {
		t.Fatal("cn three acquires should succeed (tier 3 unchanged)")
	}
	if p.Acquire("cn1") {
		t.Fatal("cn fourth acquire should fail (max_in_flight 3)")
	}
}

// TestInFlightGlobalTierUnsetFallsBack 分档未设置（0）时 global 号回落
// maxInFlight（不分档，既有部署零回归）；设置后生效。
func TestInFlightGlobalTierUnsetFallsBack(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetMaxInFlight(3)
	// 未设置 global 档：global 号按 3 放行（旧语义）。
	if !p.Acquire("g1") || !p.Acquire("g1") || !p.Acquire("g1") {
		t.Fatal("unset global tier should fall back to max_in_flight")
	}
	if p.Acquire("g1") {
		t.Fatal("unset global tier still respects max_in_flight cap")
	}
}

// TestAcquireUnknownUIDNoPanic 分档启用 + 未知 uid 不得 panic（空 entry 解引用回归）。
// 触发面：会话粘性路由对刚被移除/禁用（如 token 失效）的账号仍持 uid 调 Acquire，
// 而 inFlightLimit 在 maxInFlightGlobal>0 时会解引用 e.a.Realm()——e 为 nil 即崩。
// 锁死「!ok 判空先于 limit 快照」这一顺序约束。
func TestAcquireUnknownUIDNoPanic(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2) // 分档启用：inFlightLimit 会走 e.a.Realm()

	if p.Acquire("ghost") {
		t.Fatal("unknown uid acquire must return false")
	}
	p.Release("ghost") // Release 同样不得 panic
}

// TestPickSkipsGlobalTierFull 选号侧分档：global 号占满 2 档后 Pick 跳过
// （inFlightFull 走同一路径），不被 cn 档的 3 误放行。
func TestPickSkipsGlobalTierFull(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "gfull", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "gfree", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetCredits("gfull", 1000, 0)
	p.SetCredits("gfree", 1, 0)
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	// gfull 占满 global 档 2（还差 cn 档 3 一个名额）→ Pick 必须跳过它选 gfree。
	if !p.Acquire("gfull") || !p.Acquire("gfull") {
		t.Fatal("warm up gfull to global tier limit")
	}
	got := p.Pick()
	if got == nil || got.UID != "gfree" {
		t.Fatalf("pick should skip global-tier-full account, got %+v", got)
	}
	p.Release("gfull")
	p.Release("gfull")
}

// TestServableNowAndCountsGlobalTier ServableForRealm / CountsDetailed 与 Pick 同口径：
// global 号占满分档后该域不可服务（ServableForRealm("global")==false），但 cn 域仍可服务；
// 且该号计入 inFlightFull 计数（四处共用 inFlightFull，分档必须在全部路径一致生效）。
func TestServableNowAndCountsGlobalTier(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.SetCredits("g1", 100, 0)
	p.SetCredits("cn1", 100, 0)
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	if !p.Acquire("g1") || !p.Acquire("g1") {
		t.Fatal("warm up g1 to global tier limit")
	}
	// g1 只占 2（< cn 档 3）但已满 global 档 2 → global 域不可服务；cn 域不受影响。
	if p.ServableForRealm("global") {
		t.Error("ServableForRealm(global)=true want false（g1 已满 global 档 2）")
	}
	if !p.ServableForRealm("cn") {
		t.Error("ServableForRealm(cn)=false want true（cn 档不受分档影响）")
	}
	_, _, _, _, full := p.CountsDetailed()
	if full != 1 {
		t.Errorf("CountsDetailed inFlightFull=%d want 1（g1 占满 global 档）", full)
	}
}
