package pool

import (
	"bytes"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// realmPool 构造一个含 cn/global 账号的池，并确保 globalEnabled 开关开启（缺省）。
func realmPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	// CN 账号（显式 cn realm 或空 realm+cn domain 均可）→ Realm()=="cn"
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn
	// global 账号 → Realm()=="global"
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "workbuddy.ai"})
	return p
}

// realmOf 直接读账号 realm（等价 e.a.Realm()）。
func realmOf(a *auth.Auth) string { return a.Realm() }

func TestAvailableUIDsForRealm(t *testing.T) {
	p := realmPool(t)

	// 全部 healthy → cn 集合只有 cn1/cn2，global 集合只有 g1/g2。
	got := p.AvailableUIDsForRealm("cn")
	want := []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(cn)=%v want %v", got, want)
	}
	got = p.AvailableUIDsForRealm("global")
	want = []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(global)=%v want %v", got, want)
	}

	// realm=="" 退化为现状（等价 AvailableUIDs：全部 healthy）。
	got = p.AvailableUIDsForRealm("")
	want = []string{"cn1", "cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm()=%v want %v", got, want)
	}
}

func TestAvailableUIDsForModelRealm(t *testing.T) {
	p := realmPool(t)
	// 6004 模型冷却只落在 cn-1 上 → AvailableUIDsForModelRealm("glm-5.2","cn") 排除它，
	// 但 cn 集合至少还保底 cn-2；global 集合不受 cn 冷却影响。
	p.CooldownSoftForModel("cn1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")

	got := p.AvailableUIDsForModelRealm("glm-5.2", "cn")
	if len(got) != 1 || got[0] != "cn2" {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,cn)=%v want [cn2]", got)
	}
	got = p.AvailableUIDsForModelRealm("glm-5.2", "global")
	want := []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,global)=%v want %v", got, want)
	}
	// realm=="" → 等价 AvailableUIDsForModel（模型豁免照常：cn1 仍被该模型冷却排除）。
	got = p.AvailableUIDsForModelRealm("glm-5.2", "")
	want = []string{"cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2)=%v want %v", got, want)
	}
}

func TestPickExcludingForRealm(t *testing.T) {
	p := realmPool(t)
	// realm=cn → 只从 cn 集合选。
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "cn")
		if a == nil {
			t.Fatal("PickExcludingForRealm(cn) returned nil")
		}
		seen[a.UID] = true
		if realmOf(a) != "cn" {
			t.Fatalf("PickExcludingForRealm(cn) returned global account %s", a.UID)
		}
	}
	// global 同样只从 global 集合。
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "global")
		if a == nil {
			t.Fatal("PickExcludingForRealm(global) returned nil")
		}
		if realmOf(a) != "global" {
			t.Fatalf("PickExcludingForRealm(global) returned cn account %s", a.UID)
		}
	}
	// realm=="" → 退化现状：所有 healthy 都能选。
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "")
		if a == nil {
			t.Fatal("PickExcludingForRealm() returned nil")
		}
	}
}

func TestPickExcludingForRealmTried(t *testing.T) {
	p := realmPool(t)
	// tried 排除在 realm 过滤之后（维持请求级轮换语义）。
	a := p.PickExcludingForRealm(map[string]bool{"cn1": true}, "", "cn")
	if a == nil {
		t.Fatal("tried cn1 → nil")
	}
	if a.UID == "cn1" {
		t.Fatalf("tried cn1 still picked: %s", a.UID)
	}
	if realmOf(a) != "cn" {
		t.Fatalf("picked non-cn %s", a.UID)
	}
}

// ---------------------------------------------------------------------------
// 跨域回落（realm fallback，issue #199c）
// ---------------------------------------------------------------------------

// TestPickRealmFallbackPrefersOwnRealm 零回归：本域有候选时只选本域，
// 即使另一域候选更多/更健康也不跨域（软优先的"优先"部分）。
func TestPickRealmFallbackPrefersOwnRealm(t *testing.T) {
	p := realmPool(t)
	for i := 0; i < 100; i++ {
		a := p.PickExcludingForRealmFallback(nil, "", "cn")
		if a == nil {
			t.Fatal("本域有候选时不应返回 nil")
		}
		if realmOf(a) != "cn" {
			t.Fatalf("本域有候选却回落了另一域: %s", a.UID)
		}
	}
}

// TestPickRealmFallbackCrossRealm 本域无候选（全部 tried）→ 回落另一域成功。
// 这是 issue #199c 的核心场景：1 global + 1 cn 的混合池，global 号对该模型限流后
// 回落 cn，不再 503。
func TestPickRealmFallbackCrossRealm(t *testing.T) {
	p := realmPool(t)
	// cn 两号全部 tried → cn 域无候选 → 回落 global。
	tried := map[string]bool{"cn1": true, "cn2": true}
	a := p.PickExcludingForRealmFallback(tried, "", "cn")
	if a == nil {
		t.Fatal("本域无候选时应回落另一域，得到 nil")
	}
	if realmOf(a) != "global" {
		t.Fatalf("回落应落到 global 域，得到 %s(%s)", a.UID, realmOf(a))
	}
	// 硬过滤入口（显式前缀语义）在同条件下必须仍然返回 nil（不回落）。
	if hard := p.PickExcludingForRealm(tried, "", "cn"); hard != nil {
		t.Fatalf("硬过滤入口不得跨域回落，得到 %s", hard.UID)
	}
}

// TestPickRealmFallbackModelCooldown 本域号在该模型上被 6004 模型级冷却（healthyForModel
// 过滤掉）→ 回落另一域。覆盖 Chen 报告的链路：global 号 429/6004 后写 modelCooldowns，
// 第二轮选号在 healthyForModel 过滤之后本域已无候选 → 回落 cn。
func TestPickRealmFallbackModelCooldown(t *testing.T) {
	// 形态 1（带上游重置时间，Chen 现场）：6004 写 modelCooldowns，账号级仍 healthy。
	p := realmPool(t)
	resetAt := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("g1", 10*time.Minute, resetAt, "deepseek-v4.1-flash", "6004 model rate limit")
	p.CooldownSoftForModel("g2", 10*time.Minute, resetAt, "deepseek-v4.1-flash", "6004 model rate limit")

	a := p.PickExcludingForRealmFallback(nil, "deepseek-v4.1-flash", "global")
	if a == nil {
		t.Fatal("本域该模型全限流时应回落 cn，得到 nil")
	}
	if realmOf(a) != "cn" {
		t.Fatalf("应回落到 cn，得到 %s(%s)", a.UID, realmOf(a))
	}
	// 账号级仍 healthy 的 global 号对**其他模型**可用 → 不回落（模型维度照旧硬过滤，
	// 软优先只在"该模型下本域无候选"时才跨域）。
	b := p.PickExcludingForRealmFallback(nil, "glm-5.2", "global")
	if b == nil || realmOf(b) != "global" {
		t.Fatalf("其他模型下 global 域有候选，不应回落: %v", b)
	}

	// 形态 2（无解析时间）：6004 退化为账号级软冷却（until 非零）→ 本域无 healthy
	// 候选 → 同样回落 cn。两种形态都必须回落，否则无重置时间的 429 仍会 503。
	p2 := realmPool(t)
	p2.CooldownSoftForModel("g1", 10*time.Minute, time.Time{}, "deepseek-v4.1-flash", "6004 model rate limit")
	p2.CooldownSoftForModel("g2", 10*time.Minute, time.Time{}, "deepseek-v4.1-flash", "6004 model rate limit")
	a2 := p2.PickExcludingForRealmFallback(nil, "deepseek-v4.1-flash", "global")
	if a2 == nil || realmOf(a2) != "cn" {
		t.Fatalf("账号级软冷却形态也应回落 cn，得到 %v", a2)
	}
}

// TestPickRealmFallbackPrefersHealthyOtherRealmOverCoolingOwnRealm 本域只剩冷却号、
// 另一域有健康号时，回落健号优先于探测本域冷却号（探测是最后手段，不该白打一轮 429）。
// 反过来：另一域也只有冷却号时，仍走本域冷却兜底（既有语义：不因回落把兜底顺序打乱）。
func TestPickRealmFallbackPrefersHealthyOtherRealmOverCoolingOwnRealm(t *testing.T) {
	p := realmPool(t)
	// global 两号账号级软冷却（本域无 healthy），cn 健康。
	p.Cooldown("g1", CoolSoft, 10*time.Minute, "429 rate limit")
	p.Cooldown("g2", CoolSoft, 10*time.Minute, "429 rate limit")
	a := p.PickExcludingForRealmFallback(nil, "", "global")
	if a == nil || realmOf(a) != "cn" {
		t.Fatalf("另一域有健康号时应回落健号，得到 %v", a)
	}
	// cn 也全部冷却 → 本域冷却兜底优先（tried 排除掉另一域健号→无健号可选时，
	// 兜底仍先在本域找，不在两域间任意跳）。
	p2 := realmPool(t)
	p2.Cooldown("g1", CoolSoft, 10*time.Minute, "429 rate limit")
	p2.Cooldown("g2", CoolSoft, 10*time.Minute, "429 rate limit")
	p2.Cooldown("cn1", CoolSoft, 10*time.Minute, "429 rate limit")
	p2.Cooldown("cn2", CoolSoft, 10*time.Minute, "429 rate limit")
	b := p2.PickExcludingForRealmFallback(nil, "", "global")
	if b == nil {
		t.Fatal("两域全冷却时仍应有冷却兜底（不因回落变 503）")
	}
	if realmOf(b) != "global" {
		t.Fatalf("两域全冷却时应收敛到本域冷却兜底，得到 %s(%s)", b.UID, realmOf(b))
	}
}

// TestPickRealmFallbackNoCandidateBothRealms 两域都无候选 → nil（不无限回落）。
func TestPickRealmFallbackNoCandidateBothRealms(t *testing.T) {
	p := realmPool(t)
	tried := map[string]bool{"cn1": true, "cn2": true, "g1": true, "g2": true}
	if a := p.PickExcludingForRealmFallback(tried, "", "cn"); a != nil {
		t.Fatalf("两域都无候选应返回 nil，得到 %s", a.UID)
	}
	if a := p.PickExcludingForRealmFallback(tried, "", "global"); a != nil {
		t.Fatalf("两域都无候选应返回 nil，得到 %s", a.UID)
	}
	// 全禁用同样 nil（不因回落把禁用号捞出来）。
	p2 := realmPool(t)
	p2.Disable("cn1", "test")
	p2.Disable("cn2", "test")
	p2.Disable("g1", "test")
	p2.Disable("g2", "test")
	if a := p2.PickExcludingForRealmFallback(nil, "", "cn"); a != nil {
		t.Fatalf("全禁用应返回 nil，得到 %s", a.UID)
	}
}

// TestPickRealmFallbackInFlightFull 在途占满视作"无候选"：本域号占满 → 回落另一域。
// 回落只放宽 realm 谓词，inFlightFull 谓词照旧生效（不会选到占满号）。
func TestPickRealmFallbackInFlightFull(t *testing.T) {
	p := realmPool(t)
	p.SetMaxInFlight(1)
	for _, uid := range []string{"cn1", "cn2"} {
		if !p.Acquire(uid) {
			t.Fatalf("Acquire(%s) 失败", uid)
		}
		defer p.Release(uid)
	}
	a := p.PickExcludingForRealmFallback(nil, "", "cn")
	if a == nil {
		t.Fatal("本域在途占满时应回落 global，得到 nil")
	}
	if realmOf(a) != "global" {
		t.Fatalf("应回落到 global，得到 %s(%s)", a.UID, realmOf(a))
	}
}

// TestPickRealmFallbackLogsFallback 回落时打一条日志（运维据此确认跨域发生）。
// log.SetOutput 是进程级全局，本用例不并行、结束后立即还原。
func TestPickRealmFallbackLogsFallback(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	p := realmPool(t)
	a := p.PickExcludingForRealmFallback(map[string]bool{"cn1": true, "cn2": true}, "hy3-preview", "cn")
	if a == nil {
		t.Fatal("回落应成功")
	}
	got := buf.String()
	if !strings.Contains(got, "pool: realm fallback cn -> global (model=hy3-preview)") {
		t.Fatalf("缺跨域回落日志，实际输出=%q", got)
	}
	// 未回落时不打该日志（避免正常路径刷屏）。
	buf.Reset()
	if a := p.PickExcludingForRealmFallback(nil, "hy3-preview", "cn"); a == nil {
		t.Fatal("本域有候选应成功")
	}
	if s := buf.String(); strings.Contains(s, "realm fallback") {
		t.Fatalf("未回落却打了回落日志: %q", s)
	}
}

// TestPickRealmFallbackSingleRealm 单域池（只有 cn 号）：
//   - 请求本域（cn）→ 命中本域，零回归；
//   - 请求池内不存在的 global → 软优先语义下"偏好"不成立即回落，仍选中唯一可用号
//     （realm 只是倾向，不是硬门槛）。生产上 handler 传的裸名归属必落在池内已有域
//     （RealmRouter.BareRealm 单域即返回该域），显式 global: 前缀走硬过滤入口，
//     故本形态在 chat 路径上不可达——此处只锁定软优先入口自身的契约。
func TestPickRealmFallbackSingleRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})

	if a := p.PickExcludingForRealmFallback(nil, "", "cn"); a == nil || a.UID != "cn1" {
		t.Fatalf("本域应命中 cn1: %v", a)
	}
	a := p.PickExcludingForRealmFallback(nil, "", "global")
	if a == nil || a.UID != "cn1" {
		t.Fatalf("池内仅有 cn 号时软优先应回落选中它: %v", a)
	}
	// 硬过滤入口（显式前缀语义）在同条件下仍为 nil：显式 global 且无 global 号 → 503，
	// 不得拿 cn 号去冒充（跨域错配正是硬过滤要防的）。
	if h := p.PickExcludingForRealm(nil, "", "global"); h != nil {
		t.Fatalf("硬过滤入口不得跨域取号，得到 %s", h.UID)
	}
}
