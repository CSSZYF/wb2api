// stats.go 按模型聚合的请求统计（GET /v1/stats 的数据源）。
//
// 与 usage.go 的分工：usage.go 负责「按 (时间片, 域, 账号, 模型) 分桶累计 + 落盘」，
// 面板「用量」视图直接吃那套明细；本文件在其上做一层**按模型**的汇总投影，字段名
// 对齐上游 733d348 的 /v1/stats schema（社区面板 287775856/workbuddy2api-gui 按此
// 解析），供运维不翻日志就能看「某模型烧了多少 token、缓存命中多少、延迟多少」。
//
// 为什么不另建一套内存计数器（上游做法）：上游的 metrics 是纯内存累加、重启即清零，
// 所以必须新建；我们已有 usage 记录器（落盘、跨重启、面板在用），再建一份就是第二本
// 账——两处埋点迟早漂移，且「面板看得见、/v1/stats 看不见」这类不一致极难发现。
// 故本文件只做投影：读一次桶快照、按模型求和折算，不新增任何写入路径。
//
// 与上游的口径差异（逐条列明便于对账）：
//   - success/failed：我们按「这次尝试是否拿到上游 usage」判定（即桶里的 Err 列），
//     上游按 HTTP 状态码判定。传输错误 / 上游 >=400 / 无 usage 一律记 failed。
//   - since / uptime_sec：我们的数据跨重启，故 since = 最早分片时间（数据起点），
//     uptime_sec = 数据覆盖时长；上游是进程启动时间与进程运行时长。
//   - tokens_per_sec：优先按「生成时长」折算（端到端 − 首字，与上游同口径）；旧数据
//     没有生成时长时退回逐请求速率的样本均值（与面板 AvgTPS 同口径），不编造。
//   - 无 reset 端点：我们的桶是长期落盘账，清空等于永久删历史（见 README 说明）。
package usage

import (
	"sort"
	"strings"
	"time"
)

// StatsModel 单模型的派生统计。字段名与上游 /v1/stats 的 models[] 元素逐字对齐。
type StatsModel struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	AvgTTFBMS    float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	Credit       float64 `json:"credit"`
	CreditPerReq float64 `json:"credit_per_req"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// StatsSnapshot 是 GET /v1/stats 的响应载荷。
//
// Enabled=false 表示用量记录器不可用（未装配）；此时其余字段为零值，Message 说明
// 原因——端点仍返回 200 + 完整结构，消费方（社区面板）不必为「未启用」单独写解析
// 分支，也不会把 404/501 误读成「旧版网关不支持」。
type StatsSnapshot struct {
	Enabled   bool         `json:"enabled"`
	Message   string       `json:"message,omitempty"`
	Since     time.Time    `json:"since"`
	Now       time.Time    `json:"now"`
	UptimeSec int64        `json:"uptime_sec"`
	Total     StatsModel   `json:"total"`
	Models    []StatsModel `json:"models"`
}

// statsAcc 聚合过程中的累加器。均值必须按样本数加权（不能对各桶均值再平均），
// 故样本数留在累加器里，只有算好的结果进 StatsModel。
type statsAcc struct {
	requests  int64
	errors    int64
	streaming int64

	latSum  int64
	latN    int64
	ttfbSum int64
	ttfbN   int64
	genMs   int64
	tpsSum  float64
	tpsN    int64

	prompt     int64
	completion int64
	total      int64

	cacheHit   int64
	cacheMiss  int64
	cacheWrite int64
	credit     float64

	lastSeen time.Time
}

func (a *statsAcc) add(b *bucket) {
	a.requests += b.Req
	a.errors += b.Err
	a.streaming += b.Str
	a.latSum += b.LatMs
	a.latN += b.LatN
	a.ttfbSum += b.TtfbMs
	a.ttfbN += b.TtfbN
	a.genMs += b.GenMs
	a.tpsSum += b.TPS
	a.tpsN += b.TPSN
	a.prompt += b.PT
	a.completion += b.CT
	a.total += b.TT
	a.cacheHit += b.CH
	a.cacheMiss += b.CM
	a.cacheWrite += b.CW
	a.credit += b.Cr
	if b.Last > 0 {
		if t := time.Unix(b.Last, 0); t.After(a.lastSeen) {
			a.lastSeen = t
		}
	}
}

// finish 把累加器折算为派生统计（均值、比率、吞吐）。
func (a *statsAcc) finish(model string) StatsModel {
	p := StatsModel{
		Model:            model,
		Requests:         a.requests,
		Success:          a.requests - a.errors,
		Failed:           a.errors,
		Streaming:        a.streaming,
		PromptTokens:     a.prompt,
		CompletionTokens: a.completion,
		TotalTokens:      a.total,
		CacheHitTokens:   a.cacheHit,
		CacheMissTokens:  a.cacheMiss,
		CacheWriteTokens: a.cacheWrite,
		Credit:           a.credit,
	}
	if a.latN > 0 {
		p.AvgLatencyMS = float64(a.latSum) / float64(a.latN)
	}
	if a.ttfbN > 0 {
		p.AvgTTFBMS = float64(a.ttfbSum) / float64(a.ttfbN)
	}
	if a.genMs > 0 {
		p.TokensPerSec = float64(a.completion) * 1000 / float64(a.genMs)
	} else if a.tpsN > 0 {
		// 旧数据（无生成时长列）退回逐请求速率均值：口径与面板 AvgTPS 一致。
		p.TokensPerSec = a.tpsSum / float64(a.tpsN)
	}
	if a.requests > 0 {
		p.CreditPerReq = a.credit / float64(a.requests)
	}
	// 命中率分母 = 命中 + 未命中，不含 write：写入是「为后续命中付的费」，
	// 计入分母会把首次请求的命中率压低（与上游同口径）。
	if denom := a.cacheHit + a.cacheMiss; denom > 0 {
		p.CacheHitRate = float64(a.cacheHit) / float64(denom)
	}
	if !a.lastSeen.IsZero() {
		t := a.lastSeen
		p.LastSeen = &t
	}
	return p
}

// Stats 聚合当前全部桶，按模型汇总（跨账号、跨域）。
//
// hours > 0 时只统计最近 hours 小时内仍有数据的分片（分片按覆盖区间判定，见
// inWindow——与面板 Snapshot 共用同一条窗口边界）；hours <= 0 表示不限窗口（与
// 上游 /v1/stats 的「全量累计」同口径，也是缺省）。窗口过滤在聚合前做，因此
// total 与 models 始终同口径。
//
// 注意「缺省全量」是本端点的对外契约（社区面板按「至今累计」对账），与面板
// Snapshot 缺省 72 小时的窗口化视图是**两套有意不同的口径**：前者回答「一共用了
// 多少」，后者回答「选中的这段用了多少」。改这里之前先看
// TestStatsUnaffectedBySnapshotWindow。
//
// 只读：先在锁内复制桶快照，聚合在锁外进行——Add/Rollup 只被 memcpy 级占用，
// 不阻塞请求路径（与 Snapshot 同一纪律）。不触发任何上游请求、不落盘。
//
// 返回的 models 按请求数降序（同数按模型名升序，保证输出稳定）；total 由各模型
// 累加得出（与 models 同口径，避免两处算法分叉）。
func (r *Recorder) Stats(hours int) StatsSnapshot {
	now := time.Now()
	if r == nil {
		// 未装配记录器：仍返回完整结构（含非 nil 的 models 数组），消费方按同一套
		// 解析逻辑处理即可，不必为「未启用」特判 null。
		return StatsSnapshot{
			Enabled: false,
			Message: "usage recorder not available",
			Since:   now,
			Now:     now,
			Models:  []StatsModel{},
		}
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	// hours <= 0（缺省）时 windowCutoff 返回零值时间，inWindow 据此不裁剪。
	cutoff := windowCutoff(now, hours)

	byModel := map[string]*statsAcc{}
	var total statsAcc
	var earliest time.Time
	for i := range bs {
		b := &bs[i]
		// 窗口判据与面板 Snapshot 共用（inWindow）：按分片**覆盖区间**判定，
		// 理由见 inWindow 的注释。
		if !inWindow(b.Scope, cutoff) {
			continue
		}
		a := byModel[b.Model]
		if a == nil {
			a = &statsAcc{}
			byModel[b.Model] = a
		}
		a.add(b)
		total.add(b)
		if t, ok := bucketStart(b.Scope); ok && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}

	snap := StatsSnapshot{
		Enabled: true,
		Since:   earliest,
		Now:     now,
		Total:   total.finish("total"),
		Models:  make([]StatsModel, 0, len(byModel)),
	}
	if earliest.IsZero() {
		// 无任何分片（全新实例 / 尚无流量 / 窗口内无数据）：数据起点未知，取当前
		// 时刻，uptime_sec 随之为 0——不编造一个看似有覆盖时长的起点。
		snap.Since = now
	} else {
		snap.UptimeSec = int64(now.Sub(earliest).Seconds())
	}
	for name, a := range byModel {
		snap.Models = append(snap.Models, a.finish(name))
	}
	sort.Slice(snap.Models, func(i, j int) bool {
		if snap.Models[i].Requests != snap.Models[j].Requests {
			return snap.Models[i].Requests > snap.Models[j].Requests
		}
		return snap.Models[i].Model < snap.Models[j].Model
	})
	return snap
}

// bucketStart 解析分片键里的起点时间（本地时区，与桶的生成口径一致）。
// 无法解析时 ok=false（畸形键只影响 since 取值，不影响任何累计量）。
func bucketStart(scope string) (time.Time, bool) {
	switch {
	case strings.HasPrefix(scope, "h:"):
		t, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), time.Local)
		return t, err == nil
	case strings.HasPrefix(scope, "d:"):
		t, err := time.ParseInLocation(dayLayout, strings.TrimPrefix(scope, "d:"), time.Local)
		return t, err == nil
	}
	return time.Time{}, false
}

// bucketEnd 返回分片覆盖区间的右端点（小时桶 +1h、日桶 +24h），供窗口过滤判定。
func bucketEnd(scope string) (time.Time, bool) {
	t, ok := bucketStart(scope)
	if !ok {
		return time.Time{}, false
	}
	if strings.HasPrefix(scope, "h:") {
		return t.Add(time.Hour), true
	}
	return t.AddDate(0, 0, 1), true
}

// scopeKey 去掉分片键的粒度前缀（"h:2026-09-16T13" → "2026-09-16T13"），
// 前缀不是 "h:"/"d:" 时原样返回（畸形键的判定交给 bucketStart）。
func scopeKey(scope string) string {
	if strings.HasPrefix(scope, "h:") || strings.HasPrefix(scope, "d:") {
		return scope[2:]
	}
	return scope
}

// ------------------------------------------------------------ 窗口判据 ----
//
// 下面两个函数是 Snapshot（面板「用量总览」，按 hours 窗口化）与 Stats
// （/v1/stats，缺省全量、显式 hours 才裁剪）**共用**的窗口判据。
//
// 为什么必须共用同一份：两套视图读的是同一份桶，各写一套判定就会在窗口边缘
// 裁在不同的位置——同一分钟内「面板看到的」与「/v1/stats 报的」对不上账，
// 而且这种偏差只在边缘一两个桶上出现，极难发现。

// windowCutoff 返回「最近 hours 小时」窗口的左端点；hours <= 0 返回零值时间，
// 表示不限窗口（零值时间在 inWindow 里被识别为「不裁剪」）。
func windowCutoff(now time.Time, hours int) time.Time {
	if hours <= 0 {
		return time.Time{}
	}
	return now.Add(-time.Duration(hours) * time.Hour)
}

// inWindow 判定分片是否与窗口相交（cutoff 为零值时不裁剪）。
//
// 判据用分片**覆盖区间**的右端点（bucketEnd）而不是起点：起点早于 cutoff 但仍在
// 覆盖中的当前小时桶，其数据可能全部落在窗口内，按起点判定会把它们整片丢掉
// （窗口越小丢得越明显，如 hours=1 时当前小时桶必被丢）。日桶同理——它覆盖一整天，
// 按起点判定会多丢一天。
//
// 键解析不出来时宁留不丢：畸形键只影响 since 取值，不该影响任何累计量。
func inWindow(scope string, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return true
	}
	end, ok := bucketEnd(scope)
	if !ok {
		return true
	}
	return !end.Before(cutoff)
}
