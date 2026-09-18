// cooldownprobe.go 后台冷却探活循环：定时把「已到期的软冷却」目标拿出来，各发一个
// 最小 chat 请求试探上游是否已提前恢复，成功即解冻。
//
// 为什么需要（用户痛点）：账号撞上 6004 模型级限流后被冷却到上游声明的重置墙钟
// （可长达 6-20 小时）。若上游文案保守（或提前放量），网关**没有任何机制去试**——
// 单账号部署下唯一账号撞 6004 就是整站 503，用户只能干等或手点面板「测试」。
// v1.9.12 的 panel test_chat 已实现手动探活（成功即 ClearModelCooldown），本文件把它
// 变成后台自动执行的循环：同一套请求构造与判定口径，同一套「成功即解冻」语义。
//
// 三条硬约束（本特性的第一原则，改代码时勿"顺手"放宽）：
//  1. **失败零惩罚**：失败路径不调用任何 Cooldown* / NoteError / 熔断喂入函数，
//     不推进 softStreak、不延长冷却。否则探活会把账号越探越死（旧版软冷却的
//     "越重试越冷"就是这么来的）。
//  2. **硬冷却（CoolHard）绝不探**：积分耗尽的恢复条件是签到到账，探了必失败且白花
//     一次上游配额（选择逻辑在 pool.CooldownProbeTargets）。
//  3. **走既有出站路径**：请求经 upstream.ChatStreamContext（内部 prepareBody：
//     脱敏/指纹/prompt_cache_key 全套），不绕过任何既有出站处理。
//
// 形态与 StartBalanceRefresh / StartAuthWatch 一致（atomic 间隔 + rearm 通知 +
// 独立 goroutine + 可取消 ctx）。与 authwatch 相同的刻意差异：interval<=0（面板关了
// 开关）时不是"不启动"而是空转等重排通知，否则面板热启用后没有路径能把它拉起来。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// cooldownProbeMaxTokens 探活请求的输出上限。极小值是刻意的：探活只要一个
	// 「上游还认这个号/这个模型吗」的判据，不要任何实际输出——把积分消耗压到可忽略。
	// 8（而非 1）留一点余量：思考型模型会把预算花在 reasoning 上，上限 1 时上游可能
	// 直接回一个无有效帧的空流，把"链路通"误判成失败。
	cooldownProbeMaxTokens = 8
	// cooldownProbePrompt 探活消息（单条 user）。内容取夜猫子任务同款的最小可答问题：
	// 该文案在本仓对上游的实测路径里已验证可用（blackcat RunNightChats），
	// 不引入任何新的内容形态。
	cooldownProbePrompt = "1+1等于几？直接回答。"
	// cooldownProbeTimeout 单次探活的整体上限（出站请求与流读取都挂在这个 ctx 上）。
	// 与 panel test_chat 同量级：探活是后台任务，卡住的代价是这一轮剩余目标延后，
	// 不阻塞任何用户请求。
	cooldownProbeTimeout = 60 * time.Second
	// cooldownProbeGapDefault 两个探活请求之间的最小间隔（默认 1s）。串行之外的额外
	// 保险：池内多个账号同时到期的场景（例如一次上游抖动把全池打进冷却）不该在
	// 同一秒内向同一上游打出一串请求。测试可把 Scheduler.cooldownProbeGap 设成
	// 极小值绕过。
	cooldownProbeGapDefault = time.Second
)

// CooldownProbeResult 一轮探活的结果汇总（日志 + 面板手动触发回执）。
type CooldownProbeResult struct {
	// Probed 本轮实际发起探活请求的目标数（不含在途占满被跳过的）。
	Probed int `json:"probed"`
	// Cleared 被本次探活清掉的模型级冷却条目数（6004 提前失效 / 11102 负缓存提前失效）。
	Cleared int `json:"cleared"`
	// Accounts 被本次探活清掉账号级软冷却的账号数。
	Accounts int `json:"accounts"`
	// StillCooling 上游仍拒绝（429/6004/5xx 等分类信封）的目标数。
	StillCooling int `json:"still_cooling"`
	// Failed 传输层/解析失败（status=0，上游没给出可判读响应）的目标数。
	Failed int `json:"failed"`
	// SkippedInflight 因账号在途名额占满而跳过的目标数（不与用户请求抢配额）。
	SkippedInflight int `json:"skipped_inflight,omitempty"`
	// Skipped 重入锁被占（已有巡检在执行），本轮整体跳过。
	Skipped bool `json:"skipped,omitempty"`
}

// cooldownProbeGap 返回当前生效的探活间隔（实例字段 <=0 回落默认）。
// 形态同 wakeupGrace：实例字段而非包级 var，避免测试改写与运行中 goroutine 的读
// 构成数据竞争（fix/200e-race 的教训）。构造后由测试注入，生产恒缺省。
func (s *Scheduler) cooldownProbeGapDur() time.Duration {
	if s.cooldownProbeGap <= 0 {
		return cooldownProbeGapDefault
	}
	return s.cooldownProbeGap
}

// StartCooldownProbe 启动后台冷却探活循环（ctx 取消即停）。
//
// interval<=0（schedule.cooldown_probe_enabled=false）时仍然启动、空转等重排通知：
// 面板热启用后立即生效，无需重启（与 StartAuthWatch 同因，见其注释）。
// 池或上游未注入（测试构造/装配未完成）时不启动。
func (s *Scheduler) StartCooldownProbe(ctx context.Context, interval time.Duration) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	if interval < 0 {
		interval = 0
	}
	s.cooldownProbeInterval.Store(int64(interval))
	go func() {
		// logged 初值 -1（非法负值）保证首轮必打一条启动/暂停日志，后续仅在真变化时打
		// （不与「0 = 暂停」的合法值混淆，同 authwatch 口径）。
		logged := time.Duration(-1)
		for {
			cur := time.Duration(s.cooldownProbeInterval.Load())
			if cur != logged {
				if cur <= 0 {
					log.Printf("scheduler: 冷却探活已暂停（schedule.cooldown_probe_enabled=false，改回 true 后立即恢复）")
				} else {
					log.Printf("scheduler: 冷却探活每 %s 一轮（到期的软冷却自动试探，失败零惩罚）", cur)
				}
				logged = cur
			}
			if cur <= 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.rearmCooldownProbe:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmCooldownProbe:
				timer.Stop() // 间隔/开关已变：立刻按新值重算
			case <-timer.C:
				s.RunCooldownProbeNow()
			}
		}
	}()
}

// SetCooldownProbeInterval 热改冷却探活间隔；<=0 表示暂停循环（面板关闭该开关时）。
// 下一轮生效（正睡眠的 timer 被 rearm 唤醒后按新值重算）。
func (s *Scheduler) SetCooldownProbeInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.cooldownProbeInterval.Store(int64(d))
	poke(s.rearmCooldownProbe)
}

// CooldownProbeInterval 返回当前生效的探活间隔（0 = 暂停/未启动）。
// 供测试与运维观测：热改后立即读到新值，不必等下一轮。
func (s *Scheduler) CooldownProbeInterval() time.Duration {
	return time.Duration(s.cooldownProbeInterval.Load())
}

// RunCooldownProbeNow 立即执行一轮冷却探活（无 ctx 的外部入口：面板手动触发、测试），
// 内部走 runCooldownProbe 取背景 ctx，返回本轮结果汇总。
//
// 重入保护：与其余巡检入口共用 runningMu（beginRun），已有巡检在执行时立即返回
// （Skipped=true，不阻塞调用方）——同一时刻两趟探活/探活撞上定时批量都会对上游
// 重复轰炸。
func (s *Scheduler) RunCooldownProbeNow() CooldownProbeResult {
	if !s.beginRun("cooldown_probe") {
		return CooldownProbeResult{Skipped: true}
	}
	defer s.endRun()
	return s.runCooldownProbe(context.Background())
}

// runCooldownProbe 一轮探活：取目标 → 逐个串行探活 → 汇总。
// ctx 取消时在目标边界收尾（剩余目标下轮再探）。
func (s *Scheduler) runCooldownProbe(ctx context.Context) CooldownProbeResult {
	var res CooldownProbeResult
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return res
	}
	targets := s.cfg.Pool.CooldownProbeTargets(time.Now())
	if len(targets) == 0 {
		return res // 无到期目标：静默（每轮打一行"无目标"会变成常态噪音）
	}
	log.Printf("scheduler: 冷却探活开始，本轮 %d 个到期目标", len(targets))
	first := true
	for _, t := range targets {
		if ctx.Err() != nil {
			break // 优雅停机：剩余目标下轮再探
		}
		if !first {
			if !sleepCtx(ctx, s.cooldownProbeGapDur()) {
				break
			}
		}
		first = false
		outcome, modelCleared, accountCleared := s.probeOne(ctx, t)
		if outcome == probeOutcomeSkippedInflight {
			// 在途占满：没发请求，不计入 Probed（Probed 的口径是"实际发出的探活请求数"）。
			res.SkippedInflight++
			continue
		}
		res.Probed++
		if modelCleared {
			res.Cleared++
		}
		if accountCleared {
			res.Accounts++
		}
		switch outcome {
		case probeOutcomeStillCooling:
			res.StillCooling++
		case probeOutcomeFailed:
			res.Failed++
		}
	}
	if res.Cleared > 0 || res.Accounts > 0 {
		log.Printf("scheduler: 冷却探活完成：探 %d，解冻模型级 %d 条 / 账号级 %d 个（上游仍拒绝 %d，失败 %d）",
			res.Probed, res.Cleared, res.Accounts, res.StillCooling, res.Failed)
	}
	return res
}

// probeOutcome 单次探活的判定结果（内部用，仅驱动日志与计数）。
type probeOutcome int

const (
	probeOutcomeStillCooling probeOutcome = iota // 上游仍拒绝（分类信封/≥400）
	probeOutcomeFailed                           // 传输层/解析失败（无有效响应）
	probeOutcomeCleared                          // 探活成功并已解冻
	probeOutcomeOKNoClear                        // 探活成功但无冷却可清（并发已被清/条目已不在）
	// probeOutcomeSkippedInflight 账号在途名额占满：本轮不探（不与用户请求抢配额），
	// 且**不计入 Probed**——Probed 的口径是"实际发出的探活请求数"。
	probeOutcomeSkippedInflight
)

// probeOne 对单个 (账号, 模型) 目标发一次最小探活请求，返回
// (判定结果, 模型级条目是否被清, 账号级冷却是否被清)。
//
// 三条路径的副作用边界（严格）：
//   - 成功 → pool.CooldownProbeSuccess（清模型级条目；accountLevel 时按持锁重判清
//     账号级冷却）。**不**调 NoteSuccess（探活不是用户流量，不写成功统计）。
//   - 上游拒绝 / 传输失败 → **只记日志**：不 Cooldown*、不 NoteError、不喂熔断、
//     不延长任何冷却（失败零惩罚，本特性第一原则）。
//   - 在途租约：请求期间持 pool.Acquire 名额，避免与用户请求抢同账号配额；
//     池内 maxInFlight=0（不限）时 Acquire 恒真、计数仍累加供观测。
func (s *Scheduler) probeOne(ctx context.Context, t pool.CooldownProbeTarget) (probeOutcome, bool, bool) {
	// 目标在池内已消失（并发移除/热加载对账剔除）时跳过。
	a := s.cfg.Pool.AuthByUID(t.UID)
	if a == nil {
		return probeOutcomeFailed, false, false
	}
	if !s.cfg.Pool.Acquire(t.UID) {
		// 在途占满：本轮不探（不与用户请求抢名额），下轮再试。
		log.Printf("pool: cooldown probe uid=%s model=%s skipped（在途占满）", logfmt.UID8(t.UID), t.Model)
		return probeOutcomeSkippedInflight, false, false
	}
	defer s.cfg.Pool.Release(t.UID)

	body, err := json.Marshal(map[string]any{
		"model":      t.Model,
		"messages":   []map[string]any{{"role": "user", "content": cooldownProbePrompt}},
		"stream":     true,
		"max_tokens": cooldownProbeMaxTokens,
	})
	if err != nil {
		log.Printf("WARN: pool: cooldown probe uid=%s model=%s marshal body: %v", logfmt.UID8(t.UID), t.Model, err)
		return probeOutcomeFailed, false, false
	}

	pctx, cancel := context.WithTimeout(ctx, cooldownProbeTimeout)
	defer cancel()
	start := time.Now()
	rc, status, _, err := s.cfg.Upstream.ChatStreamContext(pctx, a, body, "", upstream.ChatMeta{})
	ms := func() int64 { return time.Since(start).Milliseconds() }

	// 分类信封（*upstream.Error）与传输层失败要分开判：前者是"上游明确拒绝"
	// （号/模型仍不可用，正常结果），后者是"没拿到可判读响应"。口径同 panel testchat。
	var uerr *upstream.Error
	if err != nil && !errors.As(err, &uerr) {
		// 传输层失败/超时：只记日志，零惩罚。
		log.Printf("pool: cooldown probe uid=%s model=%s still cooling（transport %dms: %v）",
			logfmt.UID8(t.UID), t.Model, ms(), err)
		return probeOutcomeFailed, false, false
	}
	if rc != nil {
		defer rc.Close()
	}
	if status >= 400 {
		// 上游明确拒绝：仍不可用（正常结果，不动任何状态）。带 Kind 便于对账
		// （soft_rate=还在限流；model_blocked=该后端无此模型；hard_credit=余额耗尽）。
		kind := "?"
		if uerr != nil {
			kind = uerr.Kind.String()
		}
		log.Printf("pool: cooldown probe uid=%s model=%s still cooling（status=%d kind=%s %dms）",
			logfmt.UID8(t.UID), t.Model, status, kind, ms())
		return probeOutcomeStillCooling, false, false
	}
	// 200：读完整条流（Aggregate 顺带校验"确有有效 SSE 帧"，空流按失败处理）。
	// 探活只判"链路通不通"，回复内容与 token 用量一律不取、不记账。
	if _, aerr := upstream.Aggregate(rc); aerr != nil {
		log.Printf("pool: cooldown probe uid=%s model=%s still cooling（parse %dms: %v）",
			logfmt.UID8(t.UID), t.Model, ms(), aerr)
		return probeOutcomeFailed, false, false
	}
	modelCleared, accountCleared := s.cfg.Pool.CooldownProbeSuccess(t.UID, t.Model, t.AccountLevel)
	log.Printf("pool: cooldown probe uid=%s model=%s ok（%dms model_cleared=%v account_cleared=%v）",
		logfmt.UID8(t.UID), t.Model, ms(), modelCleared, accountCleared)
	if !modelCleared && !accountCleared {
		return probeOutcomeOKNoClear, false, false
	}
	return probeOutcomeCleared, modelCleared, accountCleared
}
