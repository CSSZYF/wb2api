// reserve_interpolate_test.go 保留积分的**余额插值**口径（NoteConsumedCredits）。
//
// 为什么必须有它（本仓与上游 credit_floor 对照后发现的真缺口）：
// 保留积分的余额判据 e.credits 此前**只**由签到 / 余额刷新 / 面板刷新写入（每 5 分钟
// 一轮，签到每天两次）。而贵模型一笔就能扣掉上百分——两次刷新之间触底的号，在下一轮
// 刷新到来之前仍以"刷新时的旧余额"参与选号，于是：
//   - 用户设的底线被跨刷新窗口花掉（最坏等于不设）；
//   - 号被打穿后连免费模型都 402，正是本功能要修的病灶。
//
// 上游做法（d19add4 的 body 明写）："余额用本地插值口径（签到权威值 - 每笔
// usage.credit 实扣），只会偏低不会偏高，是保底需要的安全方向"。本文件锁住同一口径。
//
// 三条安全不变量（比"能扣"更重要）：
//  1. 只会偏低：每笔实扣 credit 都是上游给的**真实消耗量**，扣减只让余额更保守；
//  2. 不产生负余额：扣穿钳 0（对账延迟 / 消费早于记账时不得回绕成天文数字）；
//  3. 不把"未知"变成"已知 0"：creditsTotal==0（从未刷新过余额）时插值不得让闸门
//     从 fail-open 翻成 fail-closed——那会把全新部署的付费模型全打成 503。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// creditsOf 读账号当前观测余额（测试专用；生产侧只经 Status/选号判定读它）。
func (p *Pool) creditsOf(uid string) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return -1
	}
	return e.credits
}

// creditsAndExpiringOf 读（余额, 快过期子集）二元组（测试专用）。
func (p *Pool) creditsAndExpiringOf(uid string) (int64, int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return -1, -1
	}
	return e.credits, e.creditsExpiring
}

// interpolatePool 构造单账号池并写入"余额已知"的初始余额。
func interpolatePool(t *testing.T, uid string, credits int64) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: uid})
	setKnownCredits(p, uid, credits)
	return p
}

// TestNoteConsumedCreditsDeductsBalance 核心回归：一笔实扣 credit 立即压低余额，
// 且恰好扣掉 credit（不四舍五入放大、不多扣）。
func TestNoteConsumedCreditsDeductsBalance(t *testing.T) {
	p := interpolatePool(t, "u1", 250)
	p.NoteConsumedCredits("u1", 200)
	if got := p.creditsOf("u1"); got != 50 {
		t.Fatalf("插值后余额=%d want 50（250-200）", got)
	}
	// 再扣一笔：继续累减（不是每次覆盖成同一个值）。
	p.NoteConsumedCredits("u1", 20)
	if got := p.creditsOf("u1"); got != 30 {
		t.Fatalf("二次插值后余额=%d want 30", got)
	}
}

// TestNoteConsumedCreditsRoundsUp 小数 credit 向上取整：宁可多扣一分（安全方向），
// 不得因为截断而少扣（少扣就是保底漏放）。
func TestNoteConsumedCreditsRoundsUp(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	p.NoteConsumedCredits("u1", 0.4)
	if got := p.creditsOf("u1"); got != 99 {
		t.Fatalf("0.4 应向上取整扣 1 → 99，got %d", got)
	}
	// 0.05 同样算 1（任何正消耗都必须反映到余额上，否则高频小额消耗会被完全忽略）。
	p2 := interpolatePool(t, "u1", 100)
	p2.NoteConsumedCredits("u1", 0.05)
	if got := p2.creditsOf("u1"); got != 99 {
		t.Fatalf("0.05 应向上取整扣 1 → 99，got %d", got)
	}
}

// TestNoteConsumedCreditsClampsAtZero 扣穿钳 0：不产生负余额（负余额会让权重项与
// 面板显示失真，且上游对账延迟时 credit 可能大于本地观测余额）。
func TestNoteConsumedCreditsClampsAtZero(t *testing.T) {
	p := interpolatePool(t, "u1", 30)
	p.NoteConsumedCredits("u1", 9999)
	if got := p.creditsOf("u1"); got != 0 {
		t.Fatalf("扣穿应钳 0，got %d", got)
	}
}

// TestNoteConsumedCreditsIgnoresNonPositive credit<=0（免费请求 / 观测缺失）不得改动
// 余额——上游免费请求的 usage.credit 为 0，把它当"消耗"会让余额凭空减少。
// NaN 同样不得污染余额（NaN 与任何整数比较都为 false，未经防御会写出脏值）。
func TestNoteConsumedCreditsIgnoresNonPositive(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	for _, credit := range []float64{0, -1, -0.5} {
		p.NoteConsumedCredits("u1", credit)
		if got := p.creditsOf("u1"); got != 100 {
			t.Fatalf("credit=%v 不应改动余额，got %d", credit, got)
		}
	}
}

// TestNoteConsumedCreditsUnknownUIDNoop 未登记 uid 静默忽略（与 SetCredits 同口径），
// 不得 panic——账号在请求途中被移除是正常路径。
func TestNoteConsumedCreditsUnknownUIDNoop(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	p.NoteConsumedCredits("ghost", 10) // 不得 panic
	if got := p.creditsOf("u1"); got != 100 {
		t.Fatalf("其他账号余额被改动: %d", got)
	}
}

// TestNoteConsumedCreditsKeepsUnknownBalanceUnknown 安全不变量 3：余额**未知**
// （creditsTotal==0）时插值必须保持"未知"——若把插值写成 credits -= d，则 credits
// 会变成负数，闸门的 fail-open 判据（creditsTotal<=0）虽然仍成立，但权重项/面板
// 会显示负余额。更重要的是：绝不能把"未知"翻成"已知 0"而让闸门 fail-closed。
func TestNoteConsumedCreditsKeepsUnknownBalanceUnknown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // 未 SetCredits：credits=0, creditsTotal=0（未知）
	p.SetReserveCredits(50)

	p.NoteConsumedCredits("u1", 100)

	if got := p.creditsOf("u1"); got != 0 {
		t.Fatalf("未知余额不得被插值改写（应保持 0），got %d", got)
	}
	// 闸门必须仍然 fail-open（全新部署首次余额刷新前不得把付费模型全打成 503）。
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil {
		t.Fatal("余额未知时闸门必须 fail-open（付费模型照常可选）")
	}
}

// TestNoteConsumedCreditsTriggersReserveGate 端到端语义：插值让"刷新间隔内触底"的号
// 立刻被保底拦下——这正是本缺口的功能后果（否则要等下一轮余额刷新才拦）。
func TestNoteConsumedCreditsTriggersReserveGate(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	p.SetReserveCredits(50)

	// 余额 100 > 50：贵模型照常可用。
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil {
		t.Fatal("余额在线以上时贵模型应可用")
	}
	// 一笔扣掉 60 → 余额 40 ≤ 50：贵模型立即出池，无需等下一轮余额刷新。
	p.NoteConsumedCredits("u1", 60)
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
		t.Fatalf("插值触底后贵模型必须被拦，got %s", got.UID)
	}
	// 免费模型照常（保底保的是"留余额给免费模型用"）。
	if got := p.PickExcludingForModel(nil, "hy3"); got == nil {
		t.Fatal("免费模型不得被拦")
	}
	// 粘性路径同口径（插值后的余额同样参与 PickByUIDForModel 判定）。
	if got := p.PickByUIDForModel("u1", "glm-5.2"); got != nil {
		t.Fatal("粘性路径必须与选号同口径（插值触底后返回 nil 以解绑换号）")
	}
}

// TestNoteConsumedCreditsDecrementsExpiring 快过期子集随实扣同步减少（它是 credits 的
// 子集，credits 降而它不降会破坏"expiring ⊆ credits"的不变量，让选号第四因子虚高）。
func TestNoteConsumedCreditsDecrementsExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 900, 30)

	p.NoteConsumedCredits("u1", 10)
	credits, expiring := p.creditsAndExpiringOf("u1")
	if credits != 90 || expiring != 20 {
		t.Fatalf("credits=%d expiring=%d want 90/20", credits, expiring)
	}
	// 扣穿：子集钳 0，且不得大于 credits（不变量 expiring ⊆ credits）。
	p.NoteConsumedCredits("u1", 200)
	credits, expiring = p.creditsAndExpiringOf("u1")
	if credits != 0 || expiring != 0 {
		t.Fatalf("扣穿后 credits=%d expiring=%d want 0/0", credits, expiring)
	}
}

// TestNoteConsumedCreditsDoesNotRevive 插值**不承担解冻语义**：它只是把观测余额压低，
// 不得清除硬冷却/软冷却/模型级冷却——"余额变化"与"惩罚态解除"是两件事（本仓
// ReenableIfCredits 的注释反复强调的耦合缺陷，插值绝不能重新引入）。
func TestNoteConsumedCreditsDoesNotRevive(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	p.Cooldown("u1", CoolHard, time.Hour, "balance exhausted")
	p.NoteConsumedCredits("u1", 1)

	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID["u1"]
	if !e.hardCooldownSet() {
		t.Fatal("插值不得解除硬冷却（它没有解冻语义）")
	}
	if e.coolKind != CoolHard {
		t.Fatalf("coolKind=%v want CoolHard", e.coolKind)
	}
}

// TestNoteConsumedCreditsDoesNotRaiseCredits 安全不变量 1（只会偏低）：插值只减不增。
// 余额低于权威值时，一笔 credit=0（免费请求）不得把它"修回"任何值。
func TestNoteConsumedCreditsDoesNotRaiseCredits(t *testing.T) {
	p := interpolatePool(t, "u1", 40)
	p.NoteConsumedCredits("u1", 0)
	if got := p.creditsOf("u1"); got != 40 {
		t.Fatalf("credit=0 不得抬升余额，got %d", got)
	}
}

// TestNoteConsumedCreditsRecoversViaCheckin 触底号**没有死锁**：签到 / 余额刷新（唯一
// 带解冻语义的入口）越过保留线后，贵模型立即恢复可用。
//
// 这条是"上游 tier 1 豁免理由不适用于我们"的**可执行证据**：上游 d19add4 之所以让
// 「无观测（tier 1）」不受限，理由是"拦了会让账本过期/重启清零的触底号死锁在学不回来"
// ——那个死锁成立的前提是**免费判定靠账号自己实测学习**（tier 2 = 本号本模型实测收费）。
// 我们的判定不靠学习：数据源是**共享目录快照**（倍率口径）+ 静态白名单，任何账号的
// /v1/models 拉取都会填充目录，且启动预热（server.WarmModelCatalog）会主动灌热。
// 故"触底 → 无法学习 → 永久失联"的链条在我们这里**不存在**：触底号照常签到回血
// （签到/余额刷新走 Pool.List() + AuthByUID，不经选号闸门），越过线即刻恢复。
//
// 本用例锁死这条恢复路径——若日后有人把闸门加到签到/余额刷新的账号选择上，
// 死锁才会真的出现，这里会先红。
func TestNoteConsumedCreditsRecoversViaCheckin(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	p.SetReserveCredits(50)

	// 打到线下：贵模型出池。
	p.NoteConsumedCredits("u1", 80)
	if got := p.PickExcludingForModel(nil, "glm-5.2"); got != nil {
		t.Fatalf("触底后贵模型必须出池，got %s", got.UID)
	}

	// 签到回血（ReenableIfCredits 是签到/余额刷新的真实写入口）越过保留线。
	p.ReenableIfCredits("u1", 8406, 8406)

	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil {
		t.Fatal("签到回血越过保留线后贵模型必须立即恢复（不得死锁）")
	}
}

// TestNoteConsumedCreditsDoesNotBlockMetadataPath 元数据路径（拉模型目录 / 面板查询，
// 不消费积分）不受闸门影响：它是**唯一**能在池内全部触底时把目录灌热的路径
// （预热失败后的兜底），被闸门拦住会让免费判定永久停在冷目录上。
func TestNoteConsumedCreditsDoesNotBlockMetadataPath(t *testing.T) {
	p := interpolatePool(t, "u1", 100)
	p.SetReserveCredits(50)
	p.NoteConsumedCredits("u1", 80) // 打到线下

	if got := p.PickExcludingForRealmMeta(nil, "cn"); got == nil {
		t.Fatal("元数据路径必须豁免闸门（否则触底后无法刷新目录，免费判定永远失明）")
	}
}
