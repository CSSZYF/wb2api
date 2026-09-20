// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// 软冷却封顶、熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
package pool

import (
	"time"
)

func (p *Pool) SetCredits(uid string, credits, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total
		p.dirty.Store(true)
	}
}

// SetCreditsDetailed 一次性原子写入余额/总额 + 快过架子集（供测试与需要同时落三者的
// 调用方使用）。expiring 会被钳到 [0, credits]：上游分桶异常时不污染权重。
//
// 生产路径（签到 / 余额刷新 / 面板刷新）已拆成两步：ReenableIfCredits（余额 + 既有
// 解冻语义）+ SetCreditsExpiring（分桶，含 expiring==0 的复位）——分桶与解冻必须解耦，
// 理由见 SetCreditsExpiring 的注释（旧实现把分桶塞进 if 分支 = 缺陷 A）。
func (p *Pool) SetCreditsDetailed(uid string, credits, total, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsTotal = total
		e.creditsExpiring = expiring
		p.dirty.Store(true)
	}
}

// SetCreditsExpiring 只把「快过期积分子集」同步成真值（credits 的一部分，见
// entry.creditsExpiring），**不动** credits/creditsTotal，也不碰任何冷却/熔断/降权状态。
//
// 为什么必须有这个入口（缺陷 A）：分桶值由「本轮上游是否给出窗口内到期时间」决定，
// 而**零值同样是真值**——积分到期、窗口缩小（pool.expiring_soon 调小）、上游不再下发
// CycleEndTime 之后，expiring==0 必须把上一轮的非零值复位。旧实现在 expiring>0 才写
// （SetCreditsDetailed 落在 if 分支里），expiring==0 时该字段保持陈旧非零值：
// 选号第四因子（快过期优先消耗）与面板显示会一直按旧值行事，且**永不自我纠正**。
//
// 为什么不合并进 ReenableIfCredits / 复用 SetCreditsDetailed：
//   - ReenableIfCredits 带「解冻」语义（remain>0 且账号处于有效硬冷却才清冷却域，
//     软冷却/模型级冷却**不**被余额恢复解冻，issue #199）。把分桶写进它会让
//     「是否解冻」与「是否分桶」重新耦合——那正是旧实现 if/else 二选一的老毛病
//     （有快过期积分的硬冷却账号曾因此永不解冻）；
//   - 给 ReenableIfCredits 加 expiring 参数会改动全部既有调用点与测试的签名，且让
//     一个函数同时承担两件事（解冻判定 + 观测同步），语义不再单一；
//   - SetCreditsDetailed 会连带改写 credits/creditsTotal，而余额口径的唯一写入者
//     应当是 ReenableIfCredits（它内含解冻分支），二者叠加写同一字段属职责重叠。
//
// 调用约定：与 ReenableIfCredits **成对**调用（先它、后本函数），两者职责正交——
// 前者写余额与解冻，后者只写分桶。入参 expiring 钳到 [0, e.credits]（与
// SetCreditsDetailed / 恢复侧 applyAccountsLocked 同口径）。
func (p *Pool) SetCreditsExpiring(uid string, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if expiring < 0 {
		expiring = 0
	}
	if expiring > e.credits {
		expiring = e.credits
	}
	e.creditsExpiring = expiring
	p.dirty.Store(true)
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。软限流的精确对齐/有界退避
//     走 CooldownSoftRate（账号级）与 CooldownSoftForModel（模型级）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 非模型级冷却入口：清空 6004 模型级独立冷却表（modelCooldowns），
		// 避免上一次模型级限流的模型豁免泄漏到本次**账号级**限流上
		// （否则换模型请求会错误绕过本次冷却）。
		e.modelCooldowns = nil
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：把该模型的冷却截止
// 精确对齐到上游重置墙钟（不做指数堆加、不做 softStreak 计数）。
//
//   - resetAt 非零（带解析时间）→ modelCooldowns[model].Until = min(resetAt,
//     now+softRateMax)，ResetAt 记录上游原始墙钟（台账 ResetAt）。不写 until
//     （全账号级冷却不受模型级限流污染），切模型即可用（模型豁免）。
//   - resetAt 零值（无时间文案）→ 有界退避：base 起按 softStreak 翻倍、封顶
//     softRateMax，且**在软冷却中**（until 未到期）时不推进/不延长（兜底探测不再把
//     冷却越堆越厚）。不记录模型（不豁免）。
//
// 与旧实现的差异：有上游重置时间时绝对不做指数堆加（旧实现无论是否命中解析时间都
// softStreak++，退避计数被无谓污染）；无重置时间时，「冷却中兜底探测再 429」不再
// softStreak++ 翻倍——这正是用户「全池被推到 2h 封顶」的元凶。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			// 有上游重置时间：冷却截止 = min(resetAt, now+softRateMax)，不做指数放大。
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until:   p.cappedSoftUntilLocked(now, resetAt),
				ResetAt: resetAt,
				Reason:  reason,
			}
		} else {
			// 无解析时间（普通软冷却）：有界退避（base 起按 softStreak 翻倍、封顶
			// softRateMax）。注意：**在软冷却中**（until 未到期）时不推进/不延长。
			if e.coolKind != CoolSoft || !now.Before(e.until) {
				d := p.softDurationLocked(base, e.softStreak+1)
				e.softStreak++
				e.until = now.Add(d)
			}
			e.coolKind = CoolSoft
			e.reason = reason
			e.modelCooldowns = nil
		}
		p.dirty.Store(true)
	}
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免——普通账号级限流不该因
//     切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax（默认 2h，并经 softDurationLocked 统一封顶）。
//     softStreak 只在真正进入一次新冷却时计数，由 NoteSuccess/reviveCoolingLocked
//     清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			e.until = p.cappedSoftUntilLocked(now, resetAt)
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		e.modelCooldowns = nil // 账号级软冷却：清空模型豁免（切模型不绕过）
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax（now+softRateMax 与 resetAt
// 取较早者）。resetAt 已过期（时钟偏移/文案过期）时日长短钳到时间零点附近，立即恢复。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 余额刷新/签到后的条件解冻：仅 remain > 0 且账号处于**硬冷却**
// （CoolHard）时清冷却域；软冷却（CoolSoft/6004 模型级）不被余额恢复解冻（issue #199）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
// 人工强制解冻走 Revive（无条件恢复），不经本函数。
