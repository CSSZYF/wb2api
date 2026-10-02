// reserve_test.go 保留积分（pool.reserve_credits）：余额触底时只放行免费/低价模型。
//
// 语义（用户需求）：「设置一个保留至少积分，不然免费的 4.1 都用不了」——贵模型把积分
// 用到剩余 reserve 之后，该号对**贵模型**出池（换别的号），只有免费/低价模型继续可用。
//
// 免费/低价的判定是**模型感知**的（沿用 healthyForModel 的既有形态）：
//   - 倍率口径：目录里的**生效倍率**（牌价 × 当前生效折扣）≤ 0 → 免费（含限时免费的
//     活动模型、永久 x0.00 的模型），由 upstream.FreeModelIDs 推导后推入池；
//   - 兜底白名单：用户点名的 hy4-preview / hy3 / deepseek-v4.1-flash——目录冷/口径
//     不一致时它们必须照常可用，否则「余额触底 + 目录未加载」会把免费模型一起拦掉。
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

// reservePool 构造保留积分测试池：accounts 按给定 uid 加入，积分由 credits 指定。
// creditsTotal 一并写入（= credits 的常态额度）——**余额已知**是闸门生效的前提，
// 0/未刷新（creditsTotal==0）会 fail-open，那条路径由 TestReserveCreditsUnknownBalanceFailsOpen 覆盖。
// 顺序模式/加权模式由调用方切换（两种模式必须同口径）。
func reservePool(t *testing.T, credits map[string]int64) *Pool {
	t.Helper()
	p := New("")
	uids := make([]string, 0, len(credits))
	for _, uid := range []string{"poor", "rich"} { // 固定顺序，避免 map 迭代序影响断言
		if _, ok := credits[uid]; !ok {
			continue
		}
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, credits[uid], credits[uid]+8406) // 总额已知（与真实余额刷新的写入口径一致）
		uids = append(uids, uid)
	}
	p.SetOrder(uids)
	return p
}

// setKnownCredits 写入"余额已知"的账号余额（credits + 一个充足的总额），供不经
// reservePool 的用例复用。creditsTotal 是"余额是否已知"的判据（见 reserve.go 文件头）。
func setKnownCredits(p *Pool, uid string, credits int64) {
	p.SetCredits(uid, credits, credits+8406)
}

// bothModes 在两种选号模式下跑同一断言（保留积分必须与 pick_mode 正交）。
func bothModes(t *testing.T, fn func(t *testing.T, p *Pool, mode PickMode)) {
	t.Helper()
	for _, mode := range []PickMode{PickWeighted, PickSequential} {
		p := reservePool(t, map[string]int64{"poor": 45, "rich": 1000})
		p.SetPickMode(mode)
		t.Run(map[PickMode]string{PickWeighted: "weighted", PickSequential: "sequential"}[mode], func(t *testing.T) {
			fn(t, p, mode)
		})
	}
}

// TestReserveCreditsAboveLineUnaffected 余额 > reserve → 所有模型照常用（零回归）。
func TestReserveCreditsAboveLineUnaffected(t *testing.T) {
	bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
		p.SetReserveCredits(50)
		// poor=45 低于线，故只看 rich（1000 > 50）：贵模型必须照常可用。
		got := p.PickExcludingForModel(map[string]bool{"poor": true}, "glm-5.2")
		if got == nil || got.UID != "rich" {
			t.Fatalf("余额在保留线之上应照常选号，got %v", got)
		}
	})
}

// TestReserveCreditsBlocksExpensiveModel 核心回归：余额 ≤ reserve + 请求**贵模型** →
// 跳过该号。实现前必红（现状无任何保留积分概念，低余额号照常被选中）。
func TestReserveCreditsBlocksExpensiveModel(t *testing.T) {
	bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
		p.SetReserveCredits(50)
		// 池里只有 poor（45 ≤ 50）→ 贵模型无候选，返回 nil（不拿别的号顶，因为池里没有）。
		got := p.PickExcludingForModel(map[string]bool{"rich": true}, "glm-5.2")
		if got != nil {
			t.Fatalf("余额 45 ≤ 保留 50 时贵模型必须跳过该号，却选中了 %s", got.UID)
		}
		// 两个号都在池里时：跳过 poor、落到 rich（"换别的号"）。
		got = p.PickExcludingForModel(nil, "glm-5.2")
		if got == nil || got.UID != "rich" {
			t.Fatalf("应换到余额充足的号，got %v", got)
		}
	})
}

// TestReserveCreditsAllowsFreeModels 余额 ≤ reserve + 请求**免费模型**（用户点名的
// 三个 id）→ 放行（否则"免费的 4.1 都用不了"，正是本功能要修的）。
func TestReserveCreditsAllowsFreeModels(t *testing.T) {
	for _, model := range []string{"hy4-preview", "hy3", "deepseek-v4.1-flash"} {
		bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
			p.SetReserveCredits(50)
			got := p.PickExcludingForModel(map[string]bool{"rich": true}, model)
			if got == nil || got.UID != "poor" {
				t.Fatalf("免费模型 %s 必须放行低余额号，got %v", model, got)
			}
		})
	}
}

// TestReserveCreditsAllowsMultiplierFree 按**倍率**判定免费（不只是白名单）：
// 目录推导出的免费集合（生效倍率 x0.00 / 限时免费 factor=0）里的模型，余额触底时放行。
func TestReserveCreditsAllowsMultiplierFree(t *testing.T) {
	bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
		p.SetReserveCredits(50)
		// 未注入免费判定前：glm-5.2 不是白名单模型 → 拦截。
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, "glm-5.2"); got != nil {
			t.Fatalf("未知倍率的模型在余额触底时必须拦截，却选中了 %s", got.UID)
		}
		// 注入倍率推导的免费判定（x0.00）→ 放行。
		p.SetFreeModelLookup(func(model string) bool { return model == "glm-5.2" })
		got := p.PickExcludingForModel(map[string]bool{"rich": true}, "glm-5.2")
		if got == nil || got.UID != "poor" {
			t.Fatalf("倍率判定为免费的模型必须放行，got %v", got)
		}
		// 注入的判定与兜底白名单是**并集**：白名单模型不因注入而被排除。
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, "hy3"); got == nil {
			t.Error("兜底白名单应继续生效（与注入的免费判定取并集）")
		}
	})
}

// TestReserveCreditsZeroDisables reserve_credits = 0 → 完全关闭，行为与改动前一致
// （基线：低余额号照常服务贵模型；即便注入了免费判定也不产生任何过滤）。
func TestReserveCreditsZeroDisables(t *testing.T) {
	bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
		p.SetFreeModelLookup(func(model string) bool { return model == "hy3" })
		p.SetReserveCredits(0)
		got := p.PickExcludingForModel(map[string]bool{"rich": true}, "glm-5.2")
		if got == nil || got.UID != "poor" {
			t.Fatalf("reserve=0 必须完全关闭（改动前行为），got %v", got)
		}
		// 关掉后再设回 50：立即生效（无缓存残留）。
		p.SetReserveCredits(50)
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, "glm-5.2"); got != nil {
			t.Fatalf("重新打开保留积分后应立即拦截，got %s", got.UID)
		}
		// 负值钳 0（配置层已 fail fast，这里只做"经 API 直接注入"的兜底）：同样完全关闭。
		p.SetReserveCredits(-1)
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, "glm-5.2"); got == nil || got.UID != "poor" {
			t.Fatalf("负值应钳 0（= 关闭），got %v", got)
		}
	})
}

// TestReserveCreditsEmptyModelBlocked reqModel 为空（客户端没指定模型 / 裸名路由）：
// **保守拦截**——宁可换个号，也不要用未知模型吃掉底线。
func TestReserveCreditsEmptyModelBlocked(t *testing.T) {
	bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
		p.SetReserveCredits(50)
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, ""); got != nil {
			t.Fatalf("reqModel 为空时余额触底必须保守拦截，却选中了 %s", got.UID)
		}
		// 连"注入的判定对空串也返回 true"都不得放行：空模型名不是模型，不进免费集合。
		p.SetFreeModelLookup(func(string) bool { return true })
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, ""); got != nil {
			t.Fatalf("空模型名不得被免费判定放行（保守口径），却选中了 %s", got.UID)
		}
	})
}

// TestReserveCreditsBoundary 边界：credits == reserve → 拦截（用户口径是"剩余 50 就不能
// 再用贵模型了"）；credits == reserve+1 → 放行。
func TestReserveCreditsBoundary(t *testing.T) {
	for _, tc := range []struct {
		credits int64
		blocked bool
	}{
		{0, true}, {1, true}, {49, true}, {50, true}, {51, false}, {1000, false},
	} {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		setKnownCredits(p, "u1", tc.credits)
		p.SetReserveCredits(50)
		got := p.PickExcludingForModel(nil, "glm-5.2")
		if blocked := got == nil; blocked != tc.blocked {
			t.Errorf("credits=%d: blocked=%v want %v（credits ≤ reserve 即拦截）", tc.credits, blocked, tc.blocked)
		}
	}
}

// TestReserveCreditsUnknownBalanceFailsOpen 余额**未知**（creditsTotal==0，从未刷新过）
// 时闸门 fail-open：不拿"未知"当"余额 0"，否则全新部署在首次余额刷新（后台最长 5 分钟）
// 之前会把所有付费模型打成 503——一条预防性闸门不该把服务整体掐死。
// 首次余额刷新写入 total 之后闸门即刻按真实余额生效（本用例第二段）。
func TestReserveCreditsUnknownBalanceFailsOpen(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "fresh"})
	p.SetReserveCredits(50)
	// 全新账号：credits 与 creditsTotal 均未刷新过（0）。
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "fresh" {
		t.Fatalf("余额未知时必须 fail-open（否则首次余额刷新前整站 503），got %v", got)
	}
	// 余额刷新（ReenableIfCredits 的真实写入口）后 total 已知 → 闸门生效。
	p.ReenableIfCredits("fresh", 12, 8406)
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
		t.Fatalf("余额刷新后（12 ≤ 50）应立即拦截，got %s", got.UID)
	}
	// 免费模型不受影响。
	if got := p.PickExcludingForModel(nil, "hy3"); got == nil {
		t.Error("免费模型应放行")
	}
}

// TestReserveCreditsFallbackRespected 全冷却兜底同样受保留积分约束（贵模型）：
// 兜底是"探测冷却号是否已恢复"的最后手段，不是"花掉底线"的旁路——否则余额触底时
// 兜底会绕开保留线，用户设的底线形同虚设。免费模型照常参与兜底。
func TestReserveCreditsFallbackRespected(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	setKnownCredits(p, "poor", 45)
	p.SetReserveCredits(50)
	p.Cooldown("poor", CoolSoft, time.Minute, "429")

	if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
		t.Fatalf("兜底不得绕开保留积分（贵模型），got %s", got.UID)
	}
	if got := p.PickExcludingForModel(nil, "hy3"); got == nil || got.UID != "poor" {
		t.Fatalf("免费模型应照常参与兜底，got %v", got)
	}
}

// TestReserveCreditsAvailableAndSticky 可用集合与粘性校验同口径：余额触底的号对贵模型
// 既不进 AvailableUIDsForModel*（粘性分配拿不到它），PickByUIDForModel 也返回 nil
// （已绑定的会话解绑后回落轮换）——否则粘性路径会成为保留积分的旁路。
func TestReserveCreditsAvailableAndSticky(t *testing.T) {
	p := reservePool(t, map[string]int64{"poor": 45, "rich": 1000})
	p.SetReserveCredits(50)

	if got, want := p.AvailableUIDsForModel("glm-5.2"), []string{"rich"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModel(贵模型)=%v want %v（低余额号应排除）", got, want)
	}
	if got := p.PickByUIDForModel("poor", "glm-5.2"); got != nil {
		t.Errorf("PickByUIDForModel(低余额号, 贵模型) 必须返回 nil（粘性不得绕开保留积分），got %s", got.UID)
	}
	// 免费模型两个口径都照常放行。
	if got, want := p.AvailableUIDsForModel("hy3"), []string{"poor", "rich"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModel(免费模型)=%v want %v", got, want)
	}
	if got := p.PickByUIDForModel("poor", "hy3"); got == nil {
		t.Error("PickByUIDForModel(低余额号, 免费模型) 必须放行")
	}
	// 账号级口径不受影响：/healthz 的 ServableNow 与 /status 的 healthy 计数仍按
	// healthy() 判（保留积分是"对某模型可用"，不是"账号不可用"）。
	if !p.ServableNow() {
		t.Error("保留积分不得影响 ServableNow（账号级口径，与模型无关）")
	}
	if _, healthy, _, _, _ := p.CountsDetailed(); healthy != 2 {
		t.Errorf("CountsDetailed healthy=%d want 2（保留积分不进账号级计数）", healthy)
	}
}

// TestReserveCreditsExhaustedPredicate 末端错误出口的判据（供 handler 给出可操作的 503 文案）：
// 只在「全池候选都**只因**保留积分出局」时为真，池真空/另有原因/闸门关闭一律 false。
func TestReserveCreditsExhaustedPredicate(t *testing.T) {
	// ① 闸门关闭 → 恒 false（不得谎报"是保留积分拦的"）。
	p := reservePool(t, map[string]int64{"poor": 45})
	if _, _, ok := p.ReserveCreditsExhausted("glm-5.2", ""); ok {
		t.Error("reserve=0 时不得报保留积分耗尽")
	}
	p.SetReserveCredits(50)
	// ② 全池触底 + 贵模型 → true，且带候选数与保留线（文案要用）。
	blocked, line, ok := p.ReserveCreditsExhausted("glm-5.2", "")
	if !ok || blocked != 1 || line != 50 {
		t.Errorf("全池触底时 blocked=%d line=%d ok=%v want 1/50/true", blocked, line, ok)
	}
	// ③ 免费模型 → false（闸门放行，池里明明有号能服务它）。
	if _, _, ok := p.ReserveCreditsExhausted("hy3", ""); ok {
		t.Error("免费模型不得报保留积分耗尽")
	}
	// ④ 池里有一个余额充足的号 → false（失败另有原因，不得抢着认领）。
	p.Add(&auth.Auth{UID: "rich"})
	setKnownCredits(p, "rich", 1000)
	if _, _, ok := p.ReserveCreditsExhausted("glm-5.2", ""); ok {
		t.Error("存在可用候选时不得报保留积分耗尽")
	}
	// ⑤ 池真空（该域无候选账号）→ false（那是"池子本身没号"，保持 503 原文）。
	if _, _, ok := p.ReserveCreditsExhausted("glm-5.2", "global"); ok {
		t.Error("池真空时不得报保留积分耗尽")
	}
	// ⑥ 唯一"余额充足"的号被禁用 → 剩下的候选**全都**只因保留积分出局 → true。
	// 这不是"另有原因"：禁用的号选号器一律不考虑（不算候选），所以此刻池子对这个模型
	// 的真实障碍就是保留线，报它比报通用 cooling/disabled 更可操作。
	p.Disable("rich", "test")
	blocked, _, ok = p.ReserveCreditsExhausted("glm-5.2", "")
	if !ok || blocked != 1 {
		t.Errorf("余额充足的号被禁用后，剩余候选全因保留积分出局应报 true: blocked=%d ok=%v", blocked, ok)
	}
	// ⑦ 全池账号都被禁用 → cands==0 → false（那是"池子本身没号"，保持 503 原文）。
	p.Disable("poor", "test")
	if _, _, ok := p.ReserveCreditsExhausted("glm-5.2", ""); ok {
		t.Error("全池禁用时不得报保留积分耗尽（应保持池子空了的原文）")
	}
	// ⑧ 空模型名（保守拦截口径）→ true（全池触底 + 空名当贵模型）。
	p.Revive("poor")
	blocked, _, ok = p.ReserveCreditsExhausted("", "")
	if !ok || blocked != 1 {
		t.Errorf("空模型名 + 全池触底应报耗尽（保守口径）: blocked=%d ok=%v", blocked, ok)
	}
}

// TestReserveCreditsLogThrottled 可观测：因保留积分跳过时打一条**低频**日志
// （便于验证），但绝不刷屏——同一账号在节流窗口内至多一条。
func TestReserveCreditsLogThrottled(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old) // log.SetOutput 是进程级全局：本用例不并行

	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	setKnownCredits(p, "poor", 45)
	p.SetReserveCredits(50)

	for i := 0; i < 20; i++ {
		if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
			t.Fatalf("iter %d: 应拦截，got %s", i, got.UID)
		}
	}
	got := buf.String()
	if n := strings.Count(got, "pool: reserve skip uid=poor credits=45<=50 model=glm-5.2"); n != 1 {
		t.Fatalf("保留积分日志应节流为 1 条（20 次选号），实际 %d 条：%q", n, got)
	}
	// 免费模型不产生该日志（没跳过就不该有噪音）。
	buf.Reset()
	if got := p.PickExcludingForModel(nil, "hy3"); got == nil {
		t.Fatal("免费模型应放行")
	}
	if s := buf.String(); strings.Contains(s, "reserve skip") {
		t.Errorf("免费模型放行时不得打跳过日志: %q", s)
	}
}

// TestReserveCreditsSequentialSkipReason 顺序模式：跳过原因带上 reserve（便于用户
// 在溢出日志里看到"为什么落到下一个号"），但不因该持久条件把每次请求都刷一行
// （纯 reserve 跳过时抑制汇总行，由上面的节流日志承担可观测）。
func TestReserveCreditsSequentialSkipReason(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	p := reservePool(t, map[string]int64{"poor": 45, "rich": 1000})
	p.SetPickMode(PickSequential)
	p.SetReserveCredits(50)
	// 先让节流日志出一次，再 Reset，专看汇总行。
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "rich" {
		t.Fatalf("顺序模式应跳过 poor 落到 rich，got %v", got)
	}
	buf.Reset()
	for i := 0; i < 5; i++ {
		if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "rich" {
			t.Fatalf("iter %d: got %v", i, got)
		}
	}
	if s := buf.String(); strings.Contains(s, "pool: sequential skip") {
		t.Errorf("纯 reserve 跳过不得刷汇总行（持久条件，每次请求都会命中）: %q", s)
	}
}

// TestReserveCreditsUnknownModelIsBlockedByDesign 「无观测」模型（上游称 tier 1）在我们
// 的口径下**被拦**——这是与上游 credit_floor 的**有意差异**，本用例把理由与后果锁死。
//
// 上游 tier 1（本地台账无观测）豁免的理由（d19add4 的 body 原文）：「保底保的是留余额给
// 免费模型用；tier 1 若拦会让账本过期/重启清零的触底号死锁在学不回来」。那条理由的
// 前提是**免费判定靠本号本模型实测学习**（tier 2 = 该号在该模型上实测 cost>0）：
//   - 无观测 ⇒ 不知道贵不贵 ⇒ 放行 ⇒ 打一笔学到（可能被打穿）；
//   - 拦了 ⇒ 永远学不到 ⇒ 永久失联（死锁）。
//
// 上游后来自己也发现这个豁免是漏洞（39af6b7：高价新模型全池无观测 → 保底全放行 →
// 两笔打穿 100 分并硬冷却到次日），于是补了「上游目录倍率」作兜底判据——最终形态是
// 「本地台账 或 目录倍率，任一说收费就拦」。
//
// **我们的判定从来不是"学习"式**：免费集合来自**共享目录快照**（倍率口径）+ 静态白名单，
// 与"这个号有没有实测过该模型"完全无关。故：
//   - 死锁链条不存在：目录快照是**池级共享**的（任一账号拉一次 /v1/models 即填充，
//     启动预热还会主动灌热），不依赖触底号自己去试；且触底号照常签到回血
//     （见 TestNoteConsumedCreditsRecoversViaCheckin）；
//   - 「无观测」在我们这里等于「目录未覆盖」，按既有契约「缺失 ≠ 免费」处理 →
//     保守拦截。这与上游 39af6b7 之后的最终形态**方向一致**（未知不豁免），
//     只是我们连"本地实测台账"这一层都没有（我们没有成本台账）。
//
// 代价必须写清楚（诚实记录）：目录未覆盖的**内部/别名模型**若恰好免费，会被保底误拦。
// 兜底是白名单（用户点名的三个 id）——目录覆盖不全时用户可把模型 id 加进
// defaultFreeModels。这是"宁可误拦（换个号，服务仍可用）也不放行（打穿号，最坏
// 11.5 小时不可用）"的取舍，与本功能的方向一致。
func TestReserveCreditsUnknownModelIsBlockedByDesign(t *testing.T) {
	bothModes(t, func(t *testing.T, p *Pool, _ PickMode) {
		p.SetReserveCredits(50)
		// 目录未覆盖该模型（freeModelLookup 返回 false），且不在白名单 → 触底号被拦。
		p.SetFreeModelLookup(func(model string) bool { return false })
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, "internal-alias-model"); got != nil {
			t.Fatalf("目录未覆盖的模型在触底号上必须保守拦截（缺失 ≠ 免费），got %s", got.UID)
		}
		// 免费判定说免费 → 放行（判定的唯一权威，与"有没有实测过"无关）。
		p.SetFreeModelLookup(func(model string) bool { return model == "internal-alias-model" })
		if got := p.PickExcludingForModel(map[string]bool{"rich": true}, "internal-alias-model"); got == nil {
			t.Fatal("免费判定放行时不得拦（判定与账号是否实测过无关）")
		}
	})
}
