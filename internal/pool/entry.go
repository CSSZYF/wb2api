// Package pool 账号池：单一状态机（健康/冷却/熔断/连败降权）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
package pool

import (
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// degradeReason 连败降权（issue #114）写 reason 的固定文案：与冷却域的
// reason（"429 rate limit" / "waf 403 block" / "余额不足"）共用一个字段，
// 运维在 /status 一处即可看到「为什么被降权/冷却」，不新增台账字段。
const degradeReason = "consecutive failures"

// TokenUsage 账号聊天请求的累计 token 用量摘要（不包含任何原始凭证）。
type TokenUsage struct {
	RequestCount        int64     `json:"request_count,omitempty"`
	UsageCount          int64     `json:"usage_count,omitempty"`
	PromptTokens        int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens    int64     `json:"completion_tokens,omitempty"`
	TotalTokens         int64     `json:"total_tokens,omitempty"`
	LastLatencyMs       int64     `json:"last_latency_ms,omitempty"`
	LastTokensPerSecond *float64  `json:"last_tokens_per_second,omitempty"`
	LastUsedAt          time.Time `json:"last_used_at,omitempty"`
	LastModel           string    `json:"last_model,omitempty"`
}

// TokenUsageDelta 是一次聊天账号尝试的 usage 增量。
// 各 Has* 字段用于区分上游缺少字段与字段值确实为 0。
type TokenUsageDelta struct {
	Model               string
	HasPromptTokens     bool
	PromptTokens        int64
	HasCompletionTokens bool
	CompletionTokens    int64
	HasTotalTokens      bool
	TotalTokens         int64
	HasLatencyMs        bool
	LatencyMs           int64
	HasTokensPerSecond  bool
	TokensPerSecond     float64
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID           string    `json:"uid"`
	Nickname      string    `json:"nickname,omitempty"`
	Credits       int64     `json:"credits"`
	CreditsTotal  int64     `json:"credits_total,omitempty"` // 积分总额度（各套餐聚合）；0 = 未知（旧 state/查询失败）
	Cooling       bool      `json:"cooling"`
	CoolKind      string    `json:"cool_kind,omitempty"`
	CoolRemaining int64     `json:"cool_remaining_sec,omitempty"`
	Until         time.Time `json:"until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	SoftStreak    int       `json:"soft_streak,omitempty"` // 连续软冷却次数（有界退避指数；有重置时间时不计数，见 entry.softStreak）
	// RateLimitedModels 当前仍在限额的模型列表（issue #36 限额台账）。
	// 仅「带解析时间 6004」触发的模型级独立冷却（modelCooldowns 未到期条目）时非空，
	// 每模型一行；运维据此看到"账号 A 的模型 X 还在限额中，预计 Z 时间恢复"。到期即消失（零回归）。
	RateLimitedModels []RateLimitedModel `json:"rate_limited_models,omitempty"`
	// Realm 账号域（cn/global，auth.Realm() 计算值；含 global.enabled 开关闸）。
	// 供面板/状态接口按域分组展示。
	Realm           string    `json:"realm,omitempty"`
	Disabled        bool      `json:"disabled"`
	DisabledReason  string    `json:"disabled_reason,omitempty"` // 仅 disabled 账号：禁用原因（运维可见）
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
	// ConsecutiveFails 连续失败计数（连败降权用，见 entry.consecutiveFails）。
	// 零值也透出（运维口径：与 err_total/session_dead_fails 一致，零值缺失会让人
	// 误以为"没记录"，实际是零值被 omitempty 省略）。
	ConsecutiveFails int        `json:"consecutive_fails"`
	DegradeUntil     time.Time  `json:"degrade_until,omitempty"` // 连败降权截止（非零且未过 = 降权中）
	TokenUsage       TokenUsage `json:"token_usage,omitempty"`
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}

// RateLimitedModel 单个被限流模型的台账行（issue #36）。
type RateLimitedModel struct {
	Model string `json:"model"`
	// Until 冷却到期时刻 = 该模型的独立冷却截止（modelCooldowns[m].Until，截断后），
	// 多模型限流时不再等于 Status.Until（账号级）。
	Until time.Time `json:"until,omitempty"`
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）；未截断时
	// Until==ResetAt（两者同值）。截断/未截断都透出，台账始终可见上游权威时点。
	ResetAt time.Time `json:"reset_at,omitempty"`
	// Reason 触发原因（运维可读文案）。
	Reason string `json:"reason,omitempty"`
}

// modelCooldown 单个 (账号, 模型) 的模型级独立冷却记录。运行态结构，经 stateModelCooldown
// 持久化（stateAccount.ModelCooldowns）：重启后恢复，恢复时惰性过滤已过期条目。
// 承载两种「该模型在此账号上不可用」语义：
//   - 6004 模型级限流：Until 对齐上游重置墙钟；ResetAt 记录权威恢复时刻。
//   - 11102 该后端无此模型：Until 为指数退避 TTL（6h 起、封顶 24h）；Hits 记录
//     累计命中次数驱动退避（6004 无 hits 概念，Hits 恒 0）。
type modelCooldown struct {
	// Until 该模型的冷却截止（6004：now+min(resetAt-now, soft_rate_max)；11102：now+退避 TTL）。
	Until time.Time
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）。
	// 与 Until 的区别：Until 可能截断，ResetAt 是上游权威恢复时刻。
	// 11102 无重置文案，ResetAt 恒零值。
	ResetAt time.Time
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string
	// Hits 11102 负缓存的累计命中次数（驱动指数退避）。运行态不落盘（同 modelCost 口径：
	// 重启后从 6h 基数重新学习）；6004 条目 Hits 恒 0。持久化来回不会写入该字段。
	Hits int
}
type entry struct {
	a            *auth.Auth
	credits      int64
	creditsTotal int64 // 积分总额度（UserResource 聚合；0 = 未知）
	// creditsExpiring 即将过期（签到时按 expiring_soon 窗口判定）的可用积分子集，
	// 是 credits 的一部分（credits = creditsExpiring + 长期积分）。选号权重对其
	// 额外加成：优先消耗快过期积分，避免官方活动赠送的奖励积分到期作废。
	// 持久化（stateAccount.CreditsExpiring）：重启后到下次签到之间第四因子
	// （weightOf ×8）不应失忆——签到 09:00/21:00 定期刷新，窗口外重启会丢快过期
	// 积分偏好，可能让奖励积分到期作废。恢复时钳到 [0, credits]（防脏数据放大）。
	creditsExpiring int64
	successCount    int64      // 累计成功
	errTotal        int64      // 累计错误（供成功率权重 successRate = successCount/(successCount+errTotal)，不清零）
	lastErr         time.Time  // 最近一次错误时间
	lastSuccess     time.Time  // 最近一次成功时间
	tokenUsage      TokenUsage // 聊天请求 token 用量摘要（持久化）
	coolKind        CoolKind
	until           time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled        bool
	reason          string
	lastUsed        time.Time // 最近被选中时刻（防并发撞号）
	// usedSeq 单调递增的选中序号：每次被 pick 选中时取 p.pickSeq 自增值。
	// Windows 等平台 time.Now() 精度有限（~0.5ms），高并发/快速连续选号时多个
	// 账号 lastUsed 完全相等，基于 wall-clock 的 LRU/防惊群判定失效。
	// usedSeq 提供严格全序，与时间精度无关。运行态，不持久化。
	usedSeq uint64
	// breakerUntil / fails / retryCount 为熔断器运行态。
	// breakerUntil + retryCount 持久化（stateAccount.BreakerUntil/RetryCount）：
	// breakerUntil 持久化以避免熔断期重启失忆（账号立即回到可选池再撞 5xx 雷区），
	// retryCount 持久化以保留"越熔越长"的退避累积（重启归零会失去累积保护）。
	// fails 不持久化——短期计数，重启从 0 累计可接受（达 breakerThreshold=3 才熔断）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）
	// softStreak 连续软冷却次数（CoolSoft），独立于熔断器 fails 的**冷却域**计数器：
	// fails 会被熔断触发清零、且被 hard 冷却与 NoteError 污染，无法表达"连续软限流"。
	// 只在 CooldownSoftRate / CooldownSoftForModel（无解析时间分支）**进入一次新冷却**
	// 时递增——冷却中的兜底探测不推进（旧实现每次探测都翻倍，是"全池被推到 2h 封顶"
	// 的元凶）。有上游权威重置时间时绝不计数（对齐墙钟即最终时长，无退避）。
	// 重置点只有三处（都是账号被证明恢复的时刻）：NoteSuccess、人工 Revive、
	// 以及硬冷却解冻（ReenableIfCredits 对 CoolHard 放行——硬冷却不参与 streak，
	// 此处清的只是历史软冷却累积；软冷却账号不再被余额刷新/签到解冻，见 issue #199）。
	// 持久化（stateAccount.SoftStreak）：重启后软限流仍在退避，不因重启回到基数。
	softStreak int
	// modelCooldowns 6004 模型级 limit 的**独立**冷却表：model → 该模型的冷却截止/重置。
	// 与 until（全账号级）正交：6004 只写本表、不写 until，因此多个模型同时 6004 时
	// 各自独立计时，互不覆盖（A 触发后 B 再触发，A 的冷却截止不被 B 覆盖——这是
	// 单 until 字段做不到的）。仅 6004 触发时记录；空 map = 无模型级限流（不豁免）。
	// 持久化语义（stateAccount.ModelCooldowns）：重启后恢复，恢复时惰性过滤已过期
	// 条目。6004 精确对齐上游重置墙钟后，单模型冷却可长达数小时，跨重启是常态；
	// 不持久化会导致 healthyForModel 重启失忆、重新踩 6004 雷区。
	modelCooldowns map[string]modelCooldown
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 持久化（stateAccount.SessionDeadFails）：上游持续 session dead 时重启归零会导致
	// 重学（再吃 2 次失败才禁用，期间每次都白打一轮上游）；清零点（refresh/chat 成功、
	// 手工复活）同样落盘，重启后不残留旧计数。
	sessionDeadFails int
	// consecutiveFails 连续失败计数（连败降权，issue #114）——「不知道原因的兜底」：
	// 覆盖 ErrClient（未知 4xx）与传输层失败（连不上上游）这类 applyErrorPolicy
	// default 分支不罚号的形态。与 sessionDeadFails 同构但独立计数：12153 的终态
	// 是 Disable，这里的终态是临时出池（degradeUntil）。清零点：NoteSuccess。
	// 持久化（stateAccount.ConsecutiveFails + DegradeUntil）：restart 归零会让
	// 「上游持续故障 + 频繁重启」的组合重新学满阈值；degradeUntil 持久化让降权期
	// 重启不失忆（与 breakerUntil 同口径）。
	//
	// 与既有计数器正交（勿"顺手"合并）：
	//   - fails（熔断）由 NoteError 唯一喂入、达阈熔断时清零，且被硬冷却/统一复活
	//     连带污染（见 fails 注释），无法表达"连续 N 次不罚号的失败"；
	//   - softStreak 是冷却域的软限流退避指数（只在进入新软冷却时递增），
	//     ErrClient/传输层失败根本不进冷却路径；
	//   - 本计数器只由 NoteFailures 递增、NoteSuccess 清零，不读也不写上面两者。
	consecutiveFails int
	// degradeUntil 连败降权截止：非零且未到期时该账号不参与 normal 选号（临时
	// 出池）。与冷却/熔断**取更长者不叠加**（healthy 判定是并列或门，任一未到期
	// 即不可选，生效的一定是最远者），到期自动回池，无需显式复位。落盘仅未过期
	// 条目（同 breakerUntil 惰性过滤口径）。
	degradeUntil time.Time
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64
}

// hardCooldownSet 报告账号身上是否有「硬冷却被施加过且尚未被清理」的痕迹
// （coolKind==CoolHard 且 until 非零）。
//
// 为什么不能只判 coolKind == CoolHard：CoolHard 是 CoolKind 的**零值**，未设过
// coolKind 的账号天然等于 CoolHard。典型反例是仅 6004 模型级冷却的号——
// CooldownSoftForModel 有解析时间分支只写 modelCooldowns、不写 coolKind/until，
// 于是它看起来「coolKind==CoolHard」。若只按零值判定，余额刷新会把这类账号误当
// 硬冷却「解冻」，连带 clearCoolingLocked 清掉 modelCooldowns，模型级豁免被刷新
// 抹掉（issue #31 语义回归）。叠加 until 非零后，判定精确等于「确有硬冷却被
// Cooldown/CooldownUntilTomorrow4AM 施加过」：软冷却（CoolSoft）与模型级冷却
// （until 为零）都被排除。
//
// 不要求 until 尚未到期：自然到期后的硬冷却残留（coolKind=CoolHard + 已过期的
// until + 残留 reason）本就该被余额刷新清理（余额恢复是硬冷却的恢复条件），
// 放宽到「已施加过」可保持 CoolHard 路径与收窄前的既有行为完全一致。
func (e *entry) hardCooldownSet() bool {
	return e.coolKind == CoolHard && !e.until.IsZero()
}

// healthy 报告账号当前是否可选（未禁用、未处于任一冷却/熔断/连败降权期）。
// 连败降权与冷却/熔断同入本判定（取更长者不叠加：三个截止是并列的或门，
// 只要任一未到期即不可选，天然「并存取更远者」——不需要显式比较长短）。
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		return false
	}
	return true
}

// modelExempt 报告账号是否处于「6004 模型级软冷却」形态：存在任一有效的 6004
// 模型级冷却（modelCooldowns 非空），且尚未禁用、未熔断、未降权。
// 此形态下账号仅对限流中的模型不可用，对其他模型仍可选（issue #31）。
// healthyForModel 与 ServableNow 共用本谓词，保证 chat 选号与探活口径一致。
// 调用方负责 now 与冷却有效性的判断（本方法只看形态，不看冷却是否已过期）。
//
// 连败降权（degradeUntil）纳入本形态判定：降权是「临时整体出池」，账号对**所有**
// 模型都不可选（healthy 或门直接 false），不是"仅某模型不可用"的豁免形态。不排除
// 会让 /healthz 在「全池仅剩一个降权号 + 它恰好有模型级冷却」时报 servable，而 chat
// 选号实际无候选——正是本谓词要消灭的探活/选号口径裂缝。形态判定与 breakerUntil
// 同风格（只看字段是否被设过）；降权自然到期后由 healthy(now) 覆盖该账号的可服务性
// （degradeUntil 过期即 healthy=true），不产生假阴性。
func (e *entry) modelExempt() bool {
	return len(e.modelCooldowns) > 0 &&
		!e.disabled && e.breakerUntil.IsZero() && e.degradeUntil.IsZero()
}

// modelCooled 报告账号对指定 model 是否正处 6004 模型级冷却（该模型的独立冷却未过期）。
// 空 reqModel / 未记录 → false（不因模型级维度限制账号）。
func (e *entry) modelCooled(now time.Time, reqModel string) bool {
	if reqModel == "" {
		return false
	}
	mc, ok := e.modelCooldowns[reqModel]
	if !ok {
		return false
	}
	return !mc.Until.IsZero() && now.Before(mc.Until)
}

// healthyForModel 报告账号对指定 model 是否可选（含 6004 模型级独立冷却判定）：
//   - disabled → 永不可选（最高优先级）；
//   - 该模型正处 6004 独立冷却（modelCooldowns[reqModel] 未过期）→ 不可选
//     （多模型限流时各自独立，互不影响）；
//   - 否则 → 回落到账号级 healthy（until/breakerUntil 维度）。
//
// 对比旧实现（softRateModel 单字段豁免"仅锁一个模型、其他豁免"），新语义天然支持
// 任意多个模型同时限流：被 B 限流的账号对 A 请求仍可选（A 不在 modelCooldowns 拦截
// 且账号级 healthy 成立）。空 reqModel / 未记录模型 → 等价 healthy。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if e.disabled {
		return false
	}
	if e.modelCooled(now, reqModel) {
		// 该模型在 6004 独立冷却中 → 不可选。
		return false
	}
	// 账号级冷却/熔断先判；若未冷却则由账号级健康决定。
	return e.healthy(now)
}

// pruneExpiredModelCooldowns 删除 modelCooldowns 中已过期的条目（惰性清理）。
// pick 写锁路径与 revive 调用，防止 map 无限膨胀；status 只读遍历天然跳过过期项，
// 无需清理。调用方必须已持有 p.mu 写锁。
func (e *entry) pruneExpiredModelCooldowns(now time.Time) {
	if len(e.modelCooldowns) == 0 {
		return
	}
	for m, mc := range e.modelCooldowns {
		if mc.Until.IsZero() || !now.Before(mc.Until) {
			delete(e.modelCooldowns, m)
		}
	}
}

// expiry 返回账号当前仍在生效的最近冷却/熔断/降权截止时间（三个截止取最早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。连败降权计入兜底口径：降权号参与兜底（其失败
// 形态是「不知道原因」，到期放行半开试探正是兜底语义——CoolHard 才被排除）。
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		if t.IsZero() || e.degradeUntil.Before(t) {
			t = e.degradeUntil
		}
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却/连败降权；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft
// （连败降权与软冷却同归 soft：都按各自截止到期放行，兜底处置无差异）。
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			if e.degradeUntil.IsZero() || !now.Before(e.degradeUntil) || e.breakerUntil.Before(e.degradeUntil) {
				return "breaker"
			}
		}
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits      int64     `json:"credits"`
	CreditsTotal int64     `json:"credits_total,omitempty"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	ErrTotal    int64      `json:"err_total,omitempty"`
	ErrCount    int        `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time  `json:"last_success,omitempty"`
	LastErr     time.Time  `json:"last_err,omitempty"`
	TokenUsage  TokenUsage `json:"token_usage,omitempty"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak,omitempty"`
	// SessionDeadFails 连续 12153 计数（判定 session 死亡的进度）。持久化以保留
	// 「重启后连续计数继续累计」——上游持续 session dead 时重启归零会重学 2 次失败。
	// 零值省略（omitempty）。
	SessionDeadFails int `json:"session_dead_fails,omitempty"`
	// ConsecutiveFails 连续失败计数（连败降权进度，见 entry.consecutiveFails）。
	// 零值也显式写出（运维口径，同 session_dead_fails 的兄弟字段 err_total）。
	ConsecutiveFails int `json:"consecutive_fails"`
	// DegradeUntil 连败降权截止（issue #114）。仅未过期才持久化（落盘/恢复均惰性
	// 过滤），避免降权期重启失忆；过期/零值不写。指针语义同 BreakerUntil。
	DegradeUntil *time.Time `json:"degrade_until,omitempty"`

	// BreakerUntil 熔断截止（指数退避）。仅未过期才持久化（落盘/恢复均惰性过滤），
	// 避免熔断期重启失忆：breakerUntil 在未来时重启后仍阻断选号。过期/零值不写。
	// 用 *time.Time（而非 time.Time）：Go 的 omitempty 对非指针 time.Time 的零值
	// 不生效（会序列化成 0001-01-01T00:00:00Z）；指针 nil 才能真正被 omitempty 省略，
	// 与落盘"过期不写"的口径一致。
	BreakerUntil *time.Time `json:"breaker_until,omitempty"`
	// RetryCount 已熔断次数（指数退避的指数）。持久化以保留"越熔越长"的退避累积——
	// 重启归零会让反复熔断只从最小退避开始。仅在 BreakerUntil 未过期时才有意义，
	// 恢复时若 BreakerUntil 已过期则 retryCount 归零（不保留无用退避指数）。
	RetryCount int `json:"retry_count,omitempty"`
	// CreditsExpiring 快过期积分子集（credits 的子集）。持久化以保留第四因子
	// （weightOf ×8）的快过期积分偏好——重启后到下次签到之间不应失忆。
	// 恢复时钳到 [0, credits]：上游分桶异常/手工改文件留下的脏数据不得经
	// 落盘-恢复往返被放大（weightOf 的占比项会被越界值撑爆）。
	CreditsExpiring int64 `json:"credits_expiring,omitempty"`

	// ModelCooldowns 6004 模型级独立冷却表（model → 冷却记录）。持久化：
	// 6004 精确对齐上游重置墙钟后，单模型冷却可长达数小时，跨重启是常态；
	// 不持久化导致每次重启 healthyForModel 失忆、重新踩一遍 6004 雷区
	// （选号撞限流号耗尽 MaxRotate → 429）。恢复时惰性过滤已过期条目。
	ModelCooldowns map[string]stateModelCooldown `json:"model_cooldowns,omitempty"`
}

// stateModelCooldown 单个 (账号, 模型) 的 6004 独立冷却持久化记录，与运行态
// modelCooldown 同构（Until/ResetAt/Reason 字段名与语义对齐），落盘/恢复往返无损。
type stateModelCooldown struct {
	Until   time.Time `json:"until,omitempty"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却的默认封顶（有界退避的封顶 + 重置墙钟的截断上限）：
// softRateMax 未注入（<=0）时按此值算，避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// 11102「该后端无此模型」负缓存的退避参数（复用 modelCooldowns 机制承载）。
// 首次命中冷却 6h，半开到期放行重试；再命中按 Hits 指数退避（×2^min(hits-1,6)），
// 封顶 24h（最多一天再试一次）；该模型请求成功即清。6004 限流不参与本退避（各自独立语义）。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockMaxTTL  = 24 * time.Hour
	modelBlockShift   = 6 // 2^6=64 倍后封顶：6h×64>24h，实际封顶锚定 24h
)

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）
// 的默认参数，与熔断器参数族同风格（SetDegrade 注入，默认值在此）。
//   - defaultDegradeThreshold=5：比熔断阈值 3 宽——熔断管 5xx（ErrServer，确定性
//     上游故障），连败兜底管的是 ErrClient/传输层这类「不知道原因」的失败，判据
//     更弱，阈值须更保守以免误伤（偶发失败被成功清零的逻辑下，5 连败已是很强的异常信号）。
//   - defaultDegradeCooldown=10m：出池时长。取软冷却封顶（2h）与熔断基数（30m）
//     之间：长于单次软冷却（60s 级），短于熔断基数——连败的证据强度低于熔断，
//     惩罚不应重于熔断。
//   - defaultDegradeCooldownMax=2h：降权时长的**上限钳制**（非指数退避封顶——
//     连败降权为固定时长，见 degrade.go 注释「不做指数升级」），对齐
//     defaultSoftRateMax 的量级。仅当显式配置的 degrade_cooldown 大于该值时钳制。
const (
	defaultDegradeThreshold   = 5
	defaultDegradeCooldown    = 10 * time.Minute
	defaultDegradeCooldownMax = 2 * time.Hour
)

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
