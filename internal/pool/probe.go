// probe.go 后台冷却探活（background cooldown probe）的**池侧**实现：目标选择 +
// 探活成功后的解冻原语。周期循环（ticker / 间隔热改 / 手动触发）在 scheduler 侧
// （internal/scheduler/cooldownprobe.go）——本文件只管「探谁」与「探到了怎么解冻」。
//
// 背景（用户痛点）：账号撞上 6004 模型级限流后被冷却到上游声明的重置墙钟（可长达
// 数小时）。上游提前恢复时网关无从知晓——没有任何机制去试，用户只能干等或手点
// 面板「测试」。v1.9.12 的 panel test_chat 已实现**手动**探活（成功即
// ClearModelCooldown），本文件把同一套语义变成后台可自动执行的目标选择与解冻入口。
//
// 设计边界（刻意收窄，勿在后续迭代"顺手"放宽）：
//   - **只选已到期**的目标：账号级 CoolSoft 的 until 已过，或 modelCooldowns 中
//     Until 已过的条目。未到期的冷却不探——上游声明的重置墙钟是权威恢复时刻，
//     提前试探在频率×账号数下会变成常态噪音流量（要提前探需另加"即将到期"窗口，
//     属独立决策，不在本次范围）。
//   - **硬冷却（CoolHard）绝不探**：积分耗尽的恢复条件是签到到账，探了必失败还白花
//     一次上游配额（与 pickEarliestExpiryLocked 排除 CoolHard 同口径）。
//   - **失败零惩罚**：本文件只有"成功后的解冻"入口，没有失败写入口——不推进
//     softStreak、不延长冷却、不喂熔断器。探活失败绝不能把账号越探越死（这是本特性
//     的第一原则，scheduler 侧同样不调用任何 Cooldown*/NoteError）。
package pool

import (
	"sort"
	"time"
)

// probeFallbackModel 账号级软冷却目标的兜底探活模型：账号没有任何 LastModel 记录
// （从未成功服务过请求）时用它。glm-5.2 在 CN 与 global 两份目录里都存在，是最小
// 风险的通用探针（夜猫子任务同款模型）。模型名在此硬编码是刻意的：探活需要一个具体
// 模型，而 pool 不持有模型目录（目录是 upstream/handler 侧的投影，本包不反向依赖）。
const probeFallbackModel = "glm-5.2"

// CooldownProbeTarget 一个待探活的 (账号, 模型) 目标。
type CooldownProbeTarget struct {
	// UID 账号标识（完整 uid；日志侧自行截断）。
	UID string
	// Model 探活请求使用的裸模型名（不带 realm 前缀，与 chat 写入 modelCooldowns
	// 的 bareModel 同形）。
	Model string
	// AccountLevel 该目标是否**同时**代表「账号级 CoolSoft 已到期」：为 true 时探活
	// 成功除清单模型冷却外，还应清账号级冷却（until/coolKind/reason/softStreak）。
	AccountLevel bool
	// Reason 触发该冷却的原始原因（6004/11102/429 文案），仅用于日志可读性。
	Reason string
}

// CooldownProbeTargets 返回本轮待探活的目标（按 uid → 模型名稳定排序）。
//
// 选择口径（与文件头设计边界一致）：
//   - 跳过 disabled 账号（已退出选号，冷却无意义）；
//   - 跳过 hardCooldownSet 的账号（CoolHard + until 非零：积分耗尽，等签到）；
//   - modelCooldowns 中 Until 已过的条目 → 每个 (账号, 模型) 一个目标；
//   - 账号级 coolKind==CoolSoft 且 until 已过 → 一个账号级目标。探活模型优先取
//     「该账号已到期的模型条目」，其次 LastModel，最后 probeFallbackModel——
//     账号级目标与模型级目标**合并**到同一个请求（AccountLevel 折叠进该模型目标），
//     避免同一 (账号, 模型) 被探两次。
//
// 纯读路径（RLock）：不做任何状态改动、不清理过期条目（惰性清理仍由 pick 负责，
// 探活成功后由 CooldownProbeSuccess 负责）。
func (p *Pool) CooldownProbeTargets(now time.Time) []CooldownProbeTarget {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]CooldownProbeTarget, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if e.disabled {
			continue
		}
		if e.hardCooldownSet() {
			// 硬冷却（余额耗尽）：恢复条件是签到到账，探活必失败且白花配额。
			continue
		}
		expired := expiredModelNamesLocked(e, now)
		accountDue := e.coolKind == CoolSoft && !e.until.IsZero() && !now.Before(e.until)
		switch {
		case accountDue && len(expired) > 0:
			// 账号级到期 + 有已到期模型条目：合并——首个模型目标兼任账号级解冻。
			out = append(out, CooldownProbeTarget{
				UID: uid, Model: expired[0], AccountLevel: true,
				Reason: firstNonEmpty(e.modelCooldowns[expired[0]].Reason, e.reason),
			})
			for _, m := range expired[1:] {
				out = append(out, CooldownProbeTarget{
					UID: uid, Model: m, Reason: e.modelCooldowns[m].Reason,
				})
			}
		case accountDue:
			out = append(out, CooldownProbeTarget{
				UID: uid, Model: probeModelLocked(e, now), AccountLevel: true, Reason: e.reason,
			})
		default:
			for _, m := range expired {
				out = append(out, CooldownProbeTarget{
					UID: uid, Model: m, Reason: e.modelCooldowns[m].Reason,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UID != out[j].UID {
			return out[i].UID < out[j].UID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// expiredModelNamesLocked 返回该账号 modelCooldowns 中已到期（Until 非零且不在未来）
// 的模型名，按名字升序（map 遍历无序，排序保证输出稳定、便于测试与日志对账）。
// 调用方必须已持有 p.mu（只读）。
func expiredModelNamesLocked(e *entry, now time.Time) []string {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	names := make([]string, 0, len(e.modelCooldowns))
	for m, mc := range e.modelCooldowns {
		if mc.Until.IsZero() || now.Before(mc.Until) {
			continue // 未到期（或零值脏条目）：不探
		}
		names = append(names, m)
	}
	sort.Strings(names)
	return names
}

// probeModelLocked 为「账号级软冷却已到期」的目标挑一个探活模型：
// LastModel（该账号最近成功用过的模型，最可能仍然可用）→ 前提是它当前未被
// 模型级冷却；其次 probeFallbackModel（同样要求未被冷却）；两者都被冷却时仍返回
// LastModel/兜底（探活失败零惩罚，下轮再试即可——不因"挑不到干净模型"而放弃探活）。
// 调用方必须已持有 p.mu（只读）。
func probeModelLocked(e *entry, now time.Time) string {
	last := e.tokenUsage.LastModel
	if last != "" && !e.modelCooled(now, last) {
		return last
	}
	if !e.modelCooled(now, probeFallbackModel) {
		return probeFallbackModel
	}
	if last != "" {
		return last
	}
	return probeFallbackModel
}

// firstNonEmpty 返回首个非空串（日志文案回退用）。
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// CooldownProbeSuccess 探活成功后的解冻：把「该 (账号, 模型) 刚被上游实测证明可用」
// 翻译成状态机动作。返回 (模型级条目是否真被清, 账号级冷却是否真被清)，供调用方决定
// 日志字段与面板回执。
//
// 语义（与 panel test_chat 成功路径的 ClearModelCooldown 同源，多一个账号级维度）：
//   - 模型级：清 modelCooldowns[model] 这一条（6004 限流提前失效 / 11102 负缓存提前
//     失效）。**其他模型条目原样保留**——单模型成功不构成其他模型恢复的证据。
//   - 账号级（accountLevel=true 时）：仅当账号级软冷却**确已到期**才清
//     until/coolKind/reason/softStreak。条件在持锁下**重判**是刻意的：探活请求在途
//     期间账号可能被并发真实请求重新冷却（新 until 在未来），此时探活成功不得推翻
//     更新鲜的负信号。
//   - 熔断器（fails/retryCount/breakerUntil）、成功/失败统计、credits、sessionDeadFails
//     一律不动：探活不是用户流量，不写任何统计（与 testchat「零副作用」边界一致）。
//   - 不存在的 uid / 空的 model 为空操作（返回 false,false）。
func (p *Pool) CooldownProbeSuccess(uid, model string, accountLevel bool) (modelCleared, accountCleared bool) {
	if uid == "" {
		return false, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false, false
	}
	if model != "" {
		modelCleared = e.clearModelCooldownLocked(model)
	}
	if accountLevel && e.coolKind == CoolSoft && !e.until.IsZero() && !time.Now().Before(e.until) {
		// 只清账号级字段（保留 modelCooldowns 中仍有效的其他条目，见函数注释）。
		e.clearAccountCooldownLocked()
		accountCleared = true
	}
	if modelCleared || accountCleared {
		p.dirty.Store(true)
	}
	return modelCleared, accountCleared
}
