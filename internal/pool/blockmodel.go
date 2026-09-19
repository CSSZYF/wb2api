// blockmodel.go 11102「该后端无此模型」的 (账号, 模型) 级负缓存入口。
//
// 复用既有 modelCooldowns 机制（不新建平行状态）：写 modelCooldowns[model]，
// Until 为指数退避 TTL，选号侧 healthyForModel 自动对该账号避开该模型。
package pool

import (
	"strings"
	"time"
)

// modelBlockedReasonPrefix 11102「该后端无此模型」条目在 modelCooldowns[].Reason 上的
// 判别前缀（写入方 upstream.ModelBlockReason 以 "11102" 开头）。与 6004 条目
// （"6004 model rate limit"）共用同一 map 承载，凡需要区分二者语义的读取侧一律
// 以本常量判定，不散落字面量（BlockModelClear 与 modelRateLimitUntil 同一口径）。
const modelBlockedReasonPrefix = "11102"

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口（handler.applyErrorPolicy
// 调用）。复用 modelCooldowns 机制（不新建平行状态）：写 modelCooldowns[model]，Until 为指数退避
// TTL，选号侧 healthyForModel 自动对该账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端无此模型、
// 重试无意义，只能换模型/换账号」。退避口径：
//   - 首次命中 TTL = modelBlockBaseTTL（6h）；
//   - 半开到期后允许放行重试；再次命中 Hits++ → TTL = base × 2^min(hits-1, modelBlockShift)；
//   - 封顶 modelBlockMaxTTL（24h，最多一天再试一次）；
//   - 该模型请求成功即由 BlockModelClear 清除（注意：6004 的 NoteSuccess 不清 modelCooldowns，
//     故清理必须由 handler 在成功且 model 命中 11102 条目时显式调用，见 BlockModelClear）。
//
// resetAt 无需传（11102 无重置文案），ResetAt 保持零值，与 6004 台账（rateLimitedModelsLocked）
// 共用 Until 判定——11102 条目会以 11102 reason 出现在 /status 台账，运维可见。
// Hits 为运行态（不落盘），重启后从 6h 基数重新学习。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	p.dirty.Store(true)
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。半开探测或正常
// 请求对该模型成功后调用（handler 成功路径）。只清 11102 条目、不碰 6004 独立冷却表——
// 6004 有自身上游重置墙钟语义，成功不该抹掉（见 NoteSuccess 注释）。reason 前缀判定区分两者：
// 11102 条目的 reason 恒以 "11102" 开头（见 upstream.ModelBlockReason）。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, modelBlockedReasonPrefix) {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}
