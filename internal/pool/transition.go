// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、账号级冷却(until/coolKind)、
// 模型级冷却(modelCooldowns)、熔断(breakerUntil)、连败降权(degradeUntil)。
// 维度之间以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口（applyErrorPolicy / refresh / keepalive /
// 签到 / 选号）对状态的改动都必须经本文件的原语或经 Cooldown/NoteError/NoteSuccess 等
// 封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked（整域）清，
//	                     单模型提前解冻走 clearModelCooldownLocked（ClearModelCooldown）
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 唯一喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess/Revive/reviveCoolingLocked（仅硬冷却）清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//	consecutiveFails   ← NoteFailures（degrade.go，唯一喂入）；NoteSuccess/Revive 清
//	degradeUntil       ← NoteFailures 达阈（固定时长，不做指数）；NoteSuccess/Revive 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - 连败降权（consecutiveFails/degradeUntil）与上述两域都正交：喂入口只有 NoteFailures
//     （ErrClient/传输层这类「不罚号」失败），不读不写 fails/softStreak/until；
//     clearCoolingLocked 不清它（禁用/余额解冻都不构成「连败根因已消失」的证据），
//     只有 NoteSuccess（成功即回池）与 Revive（人工无条件恢复）清。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked、Revive 与
//     reviveCoolingLocked（余额恢复解冻，issue #199 收窄后仅硬冷却）共用，
//     对冷却域的处置因此永远一致。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）与连败降权
// （consecutiveFails/degradeUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// clearAccountCooldownLocked 清**账号级**冷却字段（until/coolKind/reason/softStreak），
// **保留 modelCooldowns**（其他模型仍有效的冷却条目原样留着）。
//
// 与 clearCoolingLocked 的分工：那个是「冷却域整体归零」（禁用/复活/硬冷却解冻时用，
// 模型级豁免随之消失是对的——账号整体退出选号或整体恢复）；本函数服务「单模型的
// 成功证据只解除账号级冷却、不动其他模型负缓存」的场景——后台冷却探活
// （probe.go CooldownProbeSuccess）的账号级维度。账号级软冷却到期本身不构成
// 「其他被限流模型已恢复」的证据，故 modelCooldowns 必须原样保留。
//
// softStreak 一并清零：它与 until/coolKind 同属冷却域（见 entry.softStreak 注释），
// 且本次清账的触发条件正是「账号刚被上游实测证明可用」——与 NoteSuccess /
// reviveCoolingLocked 的恢复语义一致（恢复即清零，退避回基数）。
// 熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 连败降权（consecutiveFails/degradeUntil）同样不动：本函数的触发条件是「冷却已到期
// 且上游实测可用」，而降权的独立证据链是「ErrClient/传输层连败」——探活请求成功
// 不经过 NoteSuccess（探活不是用户流量，不写成功统计），故**不**在这里顺手清降权；
// 降权号按 degradeUntil 到期自行放行（若探活目标恰好是降权号，其请求成功也只解冻
// 冷却维度，降权仍按自己的截止走）。这样探活的「失败零惩罚」与「只清冷却」两条
// 边界都不被破坏。
// 调用方必须已持有 p.mu 写锁，并负责置 dirty。
func (e *entry) clearAccountCooldownLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
}

// clearModelCooldownLocked 清单个 (账号, 模型) 的模型级冷却记录，返回是否确有条目被删。
// 只动 modelCooldowns[model] 一个键：其他模型的冷却、账号级冷却域（until/coolKind/
// softStreak/reason）与熔断器（fails/retryCount/breakerUntil）全部原样——单模型被
// 证明可用不构成账号级恢复的证据。map 为 nil 或 key 不存在时是空操作（返回 false）。
// 调用方必须已持有 p.mu 写锁，并负责置 dirty。
func (e *entry) clearModelCooldownLocked(model string) bool {
	if _, ok := e.modelCooldowns[model]; !ok {
		return false
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil // 空表归 nil：与 clearCoolingLocked/落盘口径一致（不残留空 map）
	}
	return true
}

// ClearModelCooldown 清除指定 (账号, 模型) 的模型级冷却记录，返回是否真清了。
//
// 语义：面板「测试」（POST /panel/api/account/test_chat）成功 = 该模型在此账号上刚刚
// 被上游实测证明可用 → 立刻解除这条负缓存，不必再等 TTL/重置墙钟。覆盖两种承载语义：
//   - 6004 模型级限流：提前失效，切回该模型不再被 healthyForModel 拦截（限流豁免提前生效）；
//   - 11102「该后端无此模型」：负缓存提前失效，无需等 6h 起步的指数退避到期。
//
// 边界（刻意收窄，勿在后续迭代"顺手"放宽）：
//   - 只清这一个模型的条目：其他模型、账号级冷却（until/coolKind/softStreak）与熔断
//     运行态全部不动——一次单模型成功不构成账号级恢复的证据（与 NoteSuccess 不碰
//     modelCooldowns 的既有正交性互为对偶）。
//   - 账号不存在 / model 条目不存在均为空操作，返回 false（调用方据此决定是否打日志）。
//   - 不清 reason 前缀分流：6004 与 11102 都清。二者都表达"该模型在此账号上不可用"，
//     而本入口的触发条件正是"该模型刚刚可用"，前提被证伪即应解除（与 BlockModelClear
//     只认 11102 的窄口径不同：那是 chat 成功路径的最小改动，这里是人工诊断的显式解冻）。
func (p *Pool) ClearModelCooldown(uid, model string) bool {
	if uid == "" || model == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.clearModelCooldownLocked(model) {
		return false
	}
	p.dirty.Store(true)
	return true
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，会出现
// 「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个先被
// 硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
// 连败降权（consecutiveFails/degradeUntil）同样保留：它是「ErrClient/传输层连败」
// 信号，与授权/会话无关；禁用期间计数继续累计（keepalive 跳过 disabled 号，
// 实际几乎不会增长），复活后若根因未消失应立即按既有进度继续判罚，而不是重新学。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 只清冷却域（until/coolKind/reason/softStreak/modelCooldowns）
// 并更新 credits/creditsTotal，不动熔断器（fails/retryCount/breakerUntil）。
//
// 调用方只有 ReenableIfCredits（余额刷新/签到），且**仅对硬冷却（CoolHard）**放行
// （issue #199 收窄）：硬冷却的恢复条件正是「余额恢复」，签到到账即解冻；
// 软冷却（CoolSoft/6004 模型级）的恢复时刻由上游重置墙钟或有界退避决定，余额恢复
// 不构成解冻依据——旧实现无条件解冻会让软冷却账号被刷新解冻 → 再撞 429 循环。
// 人工强制解冻走 Revive（无条件恢复，不受本收窄影响）。
//
// 熔断器不动的原因：余额恢复只证明 billing 通道健康，不证明 chat 通道健康，熔断
// （连续 5xx 信号）不应被签到覆盖。softStreak 属冷却域（与 until/coolKind 同域），
// 随冷却一并清零——与「解冻只清冷却不清熔断」的既有 C5 语义一致；硬冷却（CoolHard）
// 本就不参与 streak，这里清的是历史软冷却累积。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits, total int64) {
	e.credits = credits
	e.creditsTotal = total
	e.clearCoolingLocked()
}
