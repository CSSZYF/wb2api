// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.disableLocked(e, reason)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		p.dirty.Store(true)
		return false
	}
	e.sessionDeadFails = 0
	p.disableLocked(e, sessionDeadReason)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.sessionDeadFails != 0 {
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
//
// 注意：**不动** manualDisabled——永久禁用（系统判定）与临时停用（运维意图）是独立的
// 两位，本方法只解系统判定；运维意图要由 ClearManualDisabled 单独解除。否则一次
// revive 会把运维明确摘除的号悄悄放回选号池。
// 返回 true 表示本次确实清除了自动禁用位。
func (p *Pool) ReviveDisabled(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.disabled {
		return false
	}
	e.disabled = false
	e.reason = ""
	e.sessionDeadFails = 0
	p.dirty.Store(true)
	return true
}

// SetManualDisabled 运维临时停用/恢复（上游 a20d06f / issue #138/#118）：置位时只摘除
// 选号流量，账号仍在池里——签到、token 保活、排程任务照常执行，凭证与积分是活的。
//
// 与永久禁用（Disable）互相独立：本方法不清 disabled，也不清冷却/熔断/连败降权维度；
// 恢复时同理只清 manualDisabled。两位都清空后账号自然回到选号池。
// 幂等：重复置位/清除不报错，重复操作只更新原因文案（面板重试友好）。
// 返回 (found, changed)：uid 不存在 → (false,false)；状态无变化 → (true,false)。
func (p *Pool) SetManualDisabled(uid string, disabled bool, reason string) (found, changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false, false
	}
	if e.manualDisabled == disabled && (!disabled || e.manualReason == reason) {
		return true, false
	}
	p.setManualDisabledLocked(e, disabled, reason)
	return true, true
}

// ClearManualDisabled 解除临时停用（面板「恢复」按钮入口）。等价于
// SetManualDisabled(uid, false, "")，单独命名是为了让调用点语义自明
// （恢复运维意图 vs 复活系统判定是两件不同的事）。
func (p *Pool) ClearManualDisabled(uid string) (found, changed bool) {
	return p.SetManualDisabled(uid, false, "")
}

// ManualDisabledState 读单个账号的临时停用态（供端点回显）。uid 不存在时 ok=false。
func (p *Pool) ManualDisabledState(uid string) (disabled bool, reason string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, found := p.byUID[uid]
	if !found {
		return false, "", false
	}
	return e.manualDisabled, e.manualReason, true
}

// Revive 运维口径的"无条件恢复"：清禁用、冷却（含软退避计数）、熔断运行态与
// 连败降权（consecutiveFails/degradeUntil）。
// 与 ReviveDisabled（只清禁用）和 ReenableIfCredits（只清冷却、不动熔断）的区别：
// 本方法清除全部**惩罚**状态，供管理面板"解冻"按钮使用——人工判断该号可用时一键恢复。
// uid 不存在返回 false（供调用方区分"账号不存在"与"已复活"）。
//
// **不清** manualDisabled（临时停用）：惩罚态是系统判定（坏号该修），临时停用是运维
// 意图（我想让它歇着）——解冻一个"坏了"的号不等于取消"我主动摘掉它"的决定。两者语义
// 正交、各自解除（与上游 a20d06f 的 enable/revive 分工一致）：要让它回池，先解冻
// （本方法）再恢复（ClearManualDisabled），或直接用面板「恢复」按钮。
// 这一条同样保护 login.go 的「重新登录即复活」路径——换凭证不构成取消停用的理由。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 模型级限流豁免随冷却一并清（防泄漏到后续账号级限流）
	e.sessionDeadFails = 0
	e.fails = 0
	e.retryCount = 0
	e.breakerUntil = time.Time{}
	e.consecutiveFails = 0
	e.degradeUntil = time.Time{}
	p.dirty.Store(true)
	return true
}

// ReenableIfCredits 余额刷新/签到后的**条件解冻**：仅当 remain > 0、账号非禁用且
// 当前处于**有效硬冷却**（CoolHard：余额耗尽，等签到/次日 04:00 恢复）时清冷却域并
// 更新 credits；其余情形只更新 credits/creditsTotal，**不动任何冷却状态**。
//
// 收窄原因（issue #199）：旧实现 remain > 0 即无条件 reviveCoolingLocked，导致软限流
// （CoolSoft 429 / 6004 模型级）中的账号被余额刷新、签到一并解冻 → 选号重新选中 →
// 又撞 429 的循环。影响面尤其大的是**后台余额刷新周期任务**（默认每 5 分钟一次，
// 与面板"刷新"按钮共用 RunBalanceRefreshNow）：旧实现等于全池软冷却账号每 5 分钟被
// 自动解冻一次。软冷却的恢复时刻由上游权威重置墙钟或有界退避决定，余额恢复
// **不能证明**限流已解除；硬冷却的恢复条件恰是「余额恢复」（签到到账），这才是
// 自动解冻的正当理由。
//
// 判定用 hardCooldownSet（coolKind==CoolHard **且** until 非零）：CoolHard 是 CoolKind
// 零值，只看零值会把「仅 6004 模型级冷却」的账号（coolKind 未设、until 为零）误判为
// 硬冷却，连带清掉其 modelCooldowns。
//
// 连败降权（degradeUntil）**不在本解冻的清理域内**（clearCoolingLocked 只清冷却域）：
// 余额恢复既不证明 chat 通道健康（同熔断不动的理由），也不证明「ErrClient/传输层
// 连败」的根因已消失；降权只由到期（degradeUntil 过期）或 NoteSuccess（成功即回池）
// 解除。这条与 issue #199 的收窄同向——余额刷新不得成为任何非硬冷却维度的旁路解冻。
//
// 人工解冻不受本收窄影响：面板「解冻」按钮走 Revive（无条件恢复，清禁用/冷却域/
// 熔断运行态/连败降权），是唯一能人工清软冷却与降权的入口。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
func (p *Pool) ReenableIfCredits(uid string, remain, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled && e.hardCooldownSet() {
			p.reviveCoolingLocked(e, remain, total)
		} else {
			e.credits = remain
			e.creditsTotal = total
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
// 连败降权（issue #114）同样按「成功是恢复的最强证据」清零：consecutiveFails 归零、
// degradeUntil 清空——成功即回池，不等降权到期（与 NoteSuccess 清 breakerUntil 同口径）。
// **不碰 modelCooldowns**：6004 模型级 limit 每模型独立计时，其他模型成功不得抹掉
// 本模型的冷却截止（这正是"每模型独立"的语义）。模型级冷却只由到期/复活/账号级
// 冷却（Cooldown/reviveCoolingLocked）清除；另有两条**同模型**的提前解冻路径：
// 11102 条目由 handler 成功路径的 BlockModelClear 清，任意条目由面板「测试」成功
// 触发的 ClearModelCooldown 清（都是"该模型刚被证明可用"，不属本函数职责）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		p.dirty.Store(true)
	}
}

// RecordTokenUsage 记录一次实际发起的聊天账号尝试及上游返回的 usage 增量。
// usage 字段缺失时仍累计请求次数，但只累计明确存在的 token 字段。
func (p *Pool) RecordTokenUsage(uid string, delta TokenUsageDelta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	usage := &e.tokenUsage
	usage.RequestCount++
	usage.LastUsedAt = time.Now()
	if delta.Model != "" {
		usage.LastModel = delta.Model
	}
	known := false
	if delta.HasPromptTokens && delta.PromptTokens >= 0 {
		usage.PromptTokens += delta.PromptTokens
		known = true
	}
	if delta.HasCompletionTokens && delta.CompletionTokens >= 0 {
		usage.CompletionTokens += delta.CompletionTokens
		known = true
	}
	if delta.HasTotalTokens && delta.TotalTokens >= 0 {
		usage.TotalTokens += delta.TotalTokens
		known = true
	}
	if known {
		usage.UsageCount++
	}
	if delta.HasLatencyMs && delta.LatencyMs >= 0 {
		usage.LastLatencyMs = delta.LatencyMs
	}
	if delta.HasTokensPerSecond && delta.TokensPerSecond >= 0 {
		speed := delta.TokensPerSecond
		usage.LastTokensPerSecond = &speed
	} else {
		// 失败或缺少 completion_tokens 时不展示上一次请求的旧吞吐速度。
		usage.LastTokensPerSecond = nil
	}
	p.dirty.Store(true)
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入（模型豁免）。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 同 PickByUID，但用 healthyForModel 校验：绑定号在当前模型被
// 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换。
// 这是粘性能"换得动"的关键：绑定只记 uid，若只按账号级 healthy 校验，
// 被模型级限额的号（账号整体仍健康）会被持续选中直到轮换次数耗尽。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	// 粘性路径同样推进 usedSeq/pickSeq：粘性重度使用的账号在 LRU 兜底
	// （pick 按 usedSeq 选最旧）眼中不再是"最旧"，与 pick 的严格全序语义对齐
	// （entry.usedSeq 注释声明「每次被选中时取 pickSeq 自增值」，粘性命中也是选中）。
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm("")
}

// CountsDetailedForRealm 同 CountsDetailed，但仅统计 Realm()==realm 的账号；
// realm=="" 不加谓词（= CountsDetailed）。供 /status 按域分组透出。
func (p *Pool) CountsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm(realm)
}

// countsDetailedForRealm 是两函数共用的遍历实现；realm=="" 不加谓词。
func (p *Pool) countsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		total++
		switch {
		// 临时停用与永久禁用同归 disabled 计数：对「多少号不参与选号」这个运维问题
		// 二者等价，分开会让 total/healthy/cooling/disabled 不闭合。
		// 具体是哪一种看 /status 账号级的 manual_disabled/disabled 两位（面板据此区分
		// 「临时停用」与「已禁用」标签）。
		case e.disabled || e.manualDisabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个 healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
func (p *Pool) ServableNow() bool {
	return p.ServableForRealm("")
}

// ServableForRealm 报告某 realm 是否可服务：存在至少一个该 realm 的 healthy 且未占满在途名额的账号。
// 与 ServableNow 同口径（healthy 或模型豁免、排除 inFlightFull），仅叠加 Realm()==realm 谓词。
// realm=="" 退化为 ServableNow（现状语义）。供 /healthz 按 realm 暴露 CN/global 各自可达性。
func (p *Pool) ServableForRealm(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		// 存在性语义：账号级 healthy，或处于模型级豁免形态（6004 单模型软冷却——
		// 对触发模型不可用，对其他模型仍可选）。探活无请求模型上下文，取"存在可服务
		// 模型"与 chat 实际可达性等价（issue #31 探活侧补齐）。
		if e.healthy(now) || e.modelExempt() {
			return true
		}
	}
	return false
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	// reason 过期清理：非 disabled 账号若 until 已过期/零值，reason 清空（与落盘
	// 清理 cooledReasonLocked 同口径）。disabled 账号的 reason 是禁用原因，保留。
	_, reason := cooledReasonLocked(e, now)
	st := Status{
		UID: uid,
		// 限额台账（issue #36）：仅「带解析时间 6004 的模型级软冷却」仍在生效时非空，
		// 每模型一行（modelCooldowns 内未到期的条目），多模型同时限流全部展示。
		// 到期判据 = 该模型的独立冷却 until 未过；条件满足才输出，随到期自然消失，
		// 普通软冷却（无模型级表）/硬冷却不产生台账（零回归）。
		RateLimitedModels: p.rateLimitedModelsLocked(e, now),
		Realm:             e.a.Realm(),
		Nickname:          e.a.Nickname,
		Credits:           e.credits,
		CreditsTotal:      e.creditsTotal,
		// Cooling 口径含连败降权（degradeUntil）：降权期账号不可选，运维在 /status
		// 应看到它处于非健康态（CoolRemaining 取三截止最远者，与 healthy 或门同口径）。
		Cooling:          now.Before(e.until) || now.Before(e.breakerUntil) || now.Before(e.degradeUntil),
		Reason:           reason,
		Disabled:         e.disabled,
		ManualDisabled:   e.manualDisabled,
		SuccessCount:     e.successCount,
		ErrTotal:         e.errTotal,
		TokenUsage:       e.tokenUsage,
		LastSuccessTime:  e.lastSuccess,
		LastErrTime:      e.lastErr,
		ConsecutiveFails: e.consecutiveFails,
		DegradeUntil:     e.degradeUntil,
		Until:            e.until,
		SoftStreak:       e.softStreak,
		InFlight:         int(e.inFlight.Load()),
		BreakerFails:     e.fails,
		BreakerUntil:     e.breakerUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.ManualDisabled {
		// 临时停用原因。与 DisabledReason 分开两个字段：叠加态下运维要能同时看到
		//「我为什么摘它」和「系统为什么判它坏」，合并成一个字段会互相覆盖。
		st.ManualReason = e.manualReason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。口径与 Cooling 判定一致：
		// 取 until / breakerUntil / degradeUntil 中更远的截止（发现 5——熔断冷却的号
		// 原实现只算 until，显示"冷却中却 0 秒恢复"；BreakerUntil 虽单独透出，两口径
		// 不一致误导排查）。全部过期不会进入本分支（Cooling=false）。
		remain := time.Until(e.until)
		if b := time.Until(e.breakerUntil); b > remain {
			remain = b
		}
		if d := time.Until(e.degradeUntil); d > remain {
			remain = d
		}
		st.CoolRemaining = int64(remain.Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
		// 纯降权形态（无生效的 until/熔断）时 reason 取连败文案：降权由 NoteFailures
		// 触发，不写 until/reason（coolKind 也不是它写的），运维在 /status 需要看到
		// "为什么非健康"。有生效冷却时以冷却 reason 为准（冷却通常语义更具体）。
		if st.Reason == "" && now.Before(e.degradeUntil) {
			st.Reason = degradeReason
			st.CoolKind = "degrade"
		}
	}
	return st
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// rateLimitedModelsLocked 收集账号当前仍在限额的模型台账行（issue #36）。
// modelCooldowns 未到期条目按模型名稳定排序输出；全部到期/空表返回 nil。
// 调用方必须已持有锁（statusOf 只读路径持 RLock，本函数只读不写）。
func (p *Pool) rateLimitedModelsLocked(e *entry, now time.Time) []RateLimitedModel {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	// 先排序模型名，保证输出稳定（map 遍历无序）。
	models := make([]string, 0, len(e.modelCooldowns))
	for m := range e.modelCooldowns {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]RateLimitedModel, 0, len(models))
	for _, m := range models {
		mc := e.modelCooldowns[m]
		if !mc.Until.IsZero() && now.Before(mc.Until) {
			row := RateLimitedModel{
				Model:  m,
				Until:  mc.Until,
				Reason: mc.Reason,
			}
			// 上游「将在 … 重置」的原始墙钟：无论是否被 soft_rate_max 截断都透出——
			// 未截断时 Until==ResetAt（两者同值），截断时 ResetAt 是真实恢复时刻，
			// 台账据此始终可见上游权威时点（omitempty 仅在无 ResetAt 的旧数据上省略）。
			if !mc.ResetAt.IsZero() {
				row.ResetAt = mc.ResetAt
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}
