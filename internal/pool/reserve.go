// reserve.go 保留积分（pool.reserve_credits）：余额触底时只放行免费/低价模型。
//
// 用户需求（原话）：「要设置一个配置 保留至少积分 不然免费的 4.1 都用不了……默认搞个 50
// 吧 这样贵模型把积分用到剩余 50 了 就不能在用这个号了 只有免费模型……才可以使用」。
//
// 语义：credits ≤ reserve 的账号对**贵模型**出池（换别的号），对**免费/低价模型**照常
// 可用。它是**事前预防**，不替代 402 硬冷却（ErrHardCredit 路径一字不动）——余额真的
// 耗尽时仍走原有硬冷却；本闸门只是让余额在"还没耗尽但已触底"时不再被贵模型吃掉。
//
// ── 免费/低价的判定（两层，按优先级）─────────────────────────────────────
//
//  1. **倍率口径**（主判据，注入）：模型目录里的**生效倍率** ≤ 0 即免费。生效倍率 =
//     有 modelPromotions 折扣就用折扣系数（factor=0 = 限时免费），否则用牌价 credits
//     （"x0.00"）。推导在 upstream.FreeModelSet（见 internal/upstream/freemodel.go），
//     经 SetFreeModelLookup 注入——**池层刻意不认识模型目录**（不 import upstream），
//     与 SetMaxInFlight 那批依赖注入同形。
//  2. **兜底白名单**（defaultFreeModels）：用户点名的 hy4-preview / hy3 /
//     deepseek-v4.1-flash。目录缓存冷、口径不一致（如上游把 4.1-flash 的牌价改成
//     x0.03 而非 0）时，这三个必须照常可用——否则"余额触底 + 目录未加载"会把用户
//     明确要保的免费模型一起拦掉，正好是本功能要修的病灶。
//
// 两层取**并集**（命中任一即免费）：倍率是"上游此刻的计费事实"，白名单是"用户点名要保
// 的底线"，两者语义正交，谁都不能覆盖谁。
//
// ── 余额未知时 fail-open ────────────────────────────────────────────────
//
// credits 与 creditsTotal 都是"观测值"，未刷新过时均为 0。若把"未知"当成"余额 0"，
// 全新部署（或清了 data/ 的部署）在首次余额刷新（后台周期最长 5 分钟）之前会**把所有
// 付费模型打成 503**——用一条预防性闸门把服务整体掐死，代价远大于收益。故以
// creditsTotal <= 0（entry 注释已定义 0 = 未知）为"余额未知"判据，此时闸门放行。
// 首次余额刷新（签到/余额刷新/面板刷新）写入 total 之后，闸门即刻按真实余额生效。
//
// ── reqModel 为空时保守拦截 ─────────────────────────────────────────────
//
// 客户端没给 model / 裸名解析出空模型名时，上游会按其默认策略转派（大概率是付费别名
// 模型）——**宁可换个号，也不要用未知模型吃掉底线**，故空模型名一律当贵模型拦。
// 唯一的例外是元数据路径（见 PickExcludingForRealmMeta）：拉模型目录/面板查询这类
// GET 不消费积分，被闸门拦住只会让用户在最需要看模型列表的时候看不到列表。
package pool

import (
	"log"
	"sync"
	"time"
)

// defaultFreeModels 兜底免费白名单（用户点名的三个 id，见文件头）。
// 只作并集里的一层：命中倍率口径的模型同样放行，本表不是"完整清单"。
var defaultFreeModels = map[string]bool{
	"hy4-preview":         true,
	"hy3":                 true,
	"deepseek-v4.1-flash": true,
}

// reserveLogWindow 保留积分跳过日志的节流窗口（每个 uid 每窗口至多一条）。
//
// 为什么必须节流：该判定是**持久条件**——账号一旦触底，它上面每一次贵模型选号都会
// 命中；不节流就是一个持续刷屏的日志源（与 pickSequentialLocked 的溢出日志同一个
// 取舍：那次是"每次请求都会命中"的跳过不单独打汇总行）。1 分钟足以让用户验证
// "闸门生效了"，又不会在长跑里堆日志。
const reserveLogWindow = time.Minute

// reserveLogThrottle 保留积分跳过日志的节流器（uid → 上次打日志时刻）。
//
// 为什么自持锁而不复用 p.mu：本类型会在**两种**锁态下被调用——pick 的写锁路径，以及
// AvailableUIDsForModel*/PickByUIDForModel 的读锁路径。写 map 不能发生在 RLock 下
// （-race 会报数据竞争），自持独立 mutex 才能在两种锁态下都安全；它从不回调 pool，
// 故不会与 p.mu 形成环。零值可用（last 惰性初始化）。
type reserveLogThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// record 记一条"因保留积分跳过"的日志（同一 uid 在节流窗口内至多一条）。
// 打印在锁外：log.Printf 自带全局锁，持本锁做 I/O 会把它变成选号路径上的串行点。
func (t *reserveLogThrottle) record(uid, model string, credits, line int64) {
	now := time.Now()
	t.mu.Lock()
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	if prev, ok := t.last[uid]; ok && now.Sub(prev) < reserveLogWindow {
		t.mu.Unlock()
		return
	}
	t.last[uid] = now
	// 惰性清理：账号被移除后其条目不该永久留着（阈值取 128，远超正常池规模时才扫）。
	if len(t.last) > 128 {
		for k, ts := range t.last {
			if now.Sub(ts) >= reserveLogWindow {
				delete(t.last, k)
			}
		}
	}
	t.mu.Unlock()
	if model == "" {
		model = "(unspecified)"
	}
	log.Printf("pool: reserve skip uid=%s credits=%d<=%d model=%s", uid, credits, line, model)
}

// SetReserveCredits 注入保留积分线（main 从 config pool.reserve_credits 解析后调用；
// 面板保存后热生效，下一次选号即按新值）。**0 = 关闭**（缺省零值即关闭，故库使用者
// 与未注入的测试路径行为与改动前逐字节相同）；负数钳 0（配置层已 fail fast，
// 这里只做"经 API 直接注入"的兜底）。
func (p *Pool) SetReserveCredits(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 {
		n = 0
	}
	p.reserveCredits = n
}

// SetFreeModelLookup 注入「该模型是否免费/低价」的判定（main 装配，数据源是模型目录的
// 只读快照）。nil = 只认内置兜底白名单（目录没加载时的降级形态）。
//
// **回调契约**（必须遵守，否则会自锁/死锁）：
//   - 只读快照、绝不发起上游请求（与 /v1/stats 的倍率透出同一纪律：选号路径在高频热
//     路径上，任何上游调用都会被放大成对上游的额外压力）；
//   - **不得回调 pool**（本函数在 p.mu 内被调用，回调 pool 会自锁）。取目录快照的锁
//     （upstream 的 globalModels / server 的 dynamicModelsCache）与 p.mu 之间不存在
//     反向获取（没有任何路径持目录锁再去取 p.mu），故 p.mu → 目录锁的顺序无环。
func (p *Pool) SetFreeModelLookup(fn func(model string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.freeModelLookup = fn
}

// modelIsFreeLocked 报告模型是否免费/低价（两层并集，见文件头）。空模型名恒 false
// （"未知模型"不是"免费模型"，保守拦截由调用方按此判）。
// 调用方需已持 p.mu（读锁即可）。
func (p *Pool) modelIsFreeLocked(model string) bool {
	if model == "" {
		return false
	}
	if p.freeModelLookup != nil && p.freeModelLookup(model) {
		return true
	}
	return defaultFreeModels[model]
}

// reserveGate 一次选号的保留积分闸门。
//
// 为什么做成"入口现算一次的值"而不是逐候选查目录：免费判定要读模型目录快照（可能
// 拷贝数十条条目），逐候选调用会把一次选号变成 N 次目录查询。gate 在选号入口按
// reqModel 算一次，之后所有候选共用同一个判定结果（本闸门只依赖 reqModel 与池级配置，
// 与候选账号无关，故这么做不改变语义）。
type reserveGate struct {
	p      *Pool
	active bool  // 是否生效（reserve_credits > 0 且本次选号不豁免）
	line   int64 // 保留线
	free   bool  // 该模型免费/低价 → 余额触底也放行
}

// allows 报告候选账号是否通过保留积分闸门。未命中时打一条节流日志并返回 false。
//
// 判定顺序（每条都有理由）：
//  1. 闸门未生效 → 放行（零回归路径）；
//  2. credits > line → 放行（余额在底线之上，正是绝大多数账号的常态）；
//  3. creditsTotal <= 0 → 放行（余额**未知**，fail-open，理由见文件头）；
//  4. 模型免费 → 放行；
//  5. 其余（余额已知且触底 + 模型非免费）→ 拦截。
//
// 调用方需已持 p.mu（读锁或写锁；日志节流器自持锁，见其类型注释）。
func (g reserveGate) allows(e *entry, reqModel string) bool {
	if !g.active || e.credits > g.line || e.creditsTotal <= 0 || g.free {
		return true
	}
	g.p.reserveLog.record(e.a.UID, reqModel, e.credits, g.line)
	return false
}

// ReserveCreditsExhausted 报告「当前路由范围内所有可用候选都只因**保留积分**而出局」
// 以及触发它的模型（供 handler 末端错误出口给出可操作的 503 文案）。
//
// 为什么需要它：保留积分把池内全部账号挡在贵模型之外时，末端错误是通用的
// 「all accounts unavailable (cooling/disabled)」——用户看到的是"池子空了/号都在冷却"，
// 而账号其实全都健康、只是余额都在保留线以下。这正是本仓反复修的那类**误导性口径裂缝**
// （与 ModelRateLimitExhausted 把"模型被限流"从"池子空了"里分出来同一个动机）。
//
// 判据刻意收窄（与 ModelRateLimitExhausted 同款反向断言）：
//   - 闸门未生效（reserve=0）→ false；
//   - 候选为**空**（池真空/该域无账号/全部禁用）→ false：那是"池子本身没号"，保持 503 原文；
//   - 只要存在**任一**候选不被闸门拦住 → false：池里明明有号能服务这个模型，本次失败
//     另有原因（账号级冷却/熔断/传输层/上游 5xx），不得谎报"是保留积分拦的"。
//
// 返回 blocked 为被闸门拦住的候选数，供文案透出（"N 个号都在保留线以下"）。
func (p *Pool) ReserveCreditsExhausted(model, realm string) (blocked int, line int64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	gate := p.reserveGateFor(model, false)
	if !gate.active {
		return 0, 0, false
	}
	cands := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if e.disabled || e.manualDisabled {
			continue // 禁用/临时停用号选号器一律不考虑：不是"保留积分拦的"
		}
		cands++
		if gate.allows(e, model) {
			return 0, 0, false // 有候选能服务该模型：失败另有原因
		}
	}
	if cands == 0 {
		return 0, 0, false // 池真空（该域无候选账号）：保持 503 原文
	}
	return cands, gate.line, true
}

// reserveGateFor 按本次选号的模型构造闸门。metaOnly=true（元数据路径：拉模型目录 /
// 面板查询，不消费积分）时闸门整体不生效——被它拦住只会让用户在最需要看模型列表的
// 时候看不到列表，而这类 GET 一分钱都不花。
// 调用方需已持 p.mu（读锁即可）。
func (p *Pool) reserveGateFor(reqModel string, metaOnly bool) reserveGate {
	if p.reserveCredits <= 0 || metaOnly {
		return reserveGate{} // active=false（零值即"不拦截"）
	}
	g := reserveGate{p: p, active: true, line: p.reserveCredits}
	// 空模型名不查目录：modelIsFreeLocked("") 恒 false，等价于"保守拦截"。
	if reqModel != "" {
		g.free = p.modelIsFreeLocked(reqModel)
	}
	return g
}
