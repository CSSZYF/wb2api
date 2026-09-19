package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 按模型聚合：同一模型的多账号/多域请求合并，派生字段按加权口径折算。
func TestStatsAggregatesByModel(t *testing.T) {
	r := New("")
	now := time.Now()
	mk := func(realm, uid, model string, pt, ct int64, latMs int64, hit, miss, wr int64, credit float64, stream bool, ttfbMs int64) {
		r.Add(now, realm, uid, model, Delta{
			PromptTokens: pt, HasPromptTokens: true,
			CompletionTokens: ct, HasCompletion: true,
			TotalTokens: pt + ct, HasTotal: true,
			LatencyMs: latMs, HasLatency: true,
			CacheHit: hit, CacheMiss: miss, CacheWrite: wr, HasCache: true,
			Credit: credit, HasCredit: true,
			Stream: stream, TTFBMs: ttfbMs, HasTTFB: stream,
			GenerationMs: latMs - ttfbMs, HasGenerationMs: true,
		}, true)
	}
	// 同一模型的两次流式成功（不同域不同账号，必须合并成一行）。
	mk("cn", "u1", "glm-5.2", 50, 100, 5000, 800, 200, 0, 0.02, true, 1000)
	mk("global", "u2", "glm-5.2", 60, 200, 7000, 900, 100, 0, 0.04, true, 2000)
	// 同模型的失败尝试：无 usage，只计请求数与失败数（重试放大只能靠这一列看出来）。
	// 延迟照记（recordAttempt 恒带 latency），故它也进端到端均值分母。
	r.Add(now, "cn", "u1", "glm-5.2", Delta{LatencyMs: 1000, HasLatency: true}, false)
	// 另一个模型 + 一次失败尝试。
	mk("cn", "u1", "claude-4.6", 10, 5, 1000, 0, 0, 300, 0, false, 0)
	r.Add(now, "cn", "u1", "claude-4.6", Delta{LatencyMs: 1000, HasLatency: true}, false)

	s := r.Stats(0)
	if !s.Enabled {
		t.Fatal("enabled=false，记录器已装配时应为 true")
	}
	if len(s.Models) != 2 {
		t.Fatalf("models=%d want 2（按模型聚合，跨域跨账号合并）", len(s.Models))
	}
	// 按请求数降序：glm-5.2（3 次）在前。
	m := s.Models[0]
	if m.Model != "glm-5.2" {
		t.Fatalf("models[0]=%q want glm-5.2（请求数降序）", m.Model)
	}
	if m.Requests != 3 || m.Success != 2 || m.Failed != 1 || m.Streaming != 2 {
		t.Errorf("req/succ/fail/stream = %d/%d/%d/%d want 3/2/1/2", m.Requests, m.Success, m.Failed, m.Streaming)
	}
	// 端到端均值按样本加权 = (5000+7000+1000)/3 = 4333.33ms（失败尝试也计入）
	if m.AvgLatencyMS < 4333.32 || m.AvgLatencyMS > 4333.34 {
		t.Errorf("avg_latency=%.2f want ~4333.33", m.AvgLatencyMS)
	}
	// 首字均值 = (1000+2000)/2 = 1500ms：失败尝试无首字观测，不得计入分母
	// （计入会把均值拉低失真）。
	if m.AvgTTFBMS != 1500 {
		t.Errorf("avg_ttfb=%.2f want 1500", m.AvgTTFBMS)
	}
	if m.PromptTokens != 110 || m.CompletionTokens != 300 || m.TotalTokens != 410 {
		t.Errorf("tokens pt/ct/tt = %d/%d/%d want 110/300/410", m.PromptTokens, m.CompletionTokens, m.TotalTokens)
	}
	// 命中率 = 1700/(1700+300) = 0.85（分母不含 write）
	if m.CacheHitRate < 0.8499 || m.CacheHitRate > 0.8501 {
		t.Errorf("cache_hit_rate=%.4f want 0.85", m.CacheHitRate)
	}
	if m.CacheHitTokens != 1700 || m.CacheMissTokens != 300 || m.CacheWriteTokens != 0 {
		t.Errorf("cache hit/miss/wr = %d/%d/%d want 1700/300/0", m.CacheHitTokens, m.CacheMissTokens, m.CacheWriteTokens)
	}
	// 吞吐按生成时长折算：completion 300 / genMs (4000+5000)ms = 33.33 tok/s
	// （失败尝试没有 completion 观测，不进分母——否则吞吐被系统性拉低）。
	if got := m.TokensPerSec; got < 33.32 || got > 33.34 {
		t.Errorf("tokens_per_sec=%.4f want ~33.33", got)
	}
	if m.Credit < 0.0599 || m.Credit > 0.0601 {
		t.Errorf("credit=%.4f want 0.06", m.Credit)
	}
	if m.CreditPerReq < 0.0199 || m.CreditPerReq > 0.0201 {
		t.Errorf("credit_per_req=%.4f want 0.02", m.CreditPerReq)
	}
	if m.LastSeen == nil {
		t.Error("last_seen 缺失：有流量时必须给出最近活跃时间")
	}

	// 第二个模型：1 成功 + 1 失败。
	m2 := s.Models[1]
	if m2.Model != "claude-4.6" || m2.Requests != 2 || m2.Success != 1 || m2.Failed != 1 {
		t.Errorf("models[1]=%+v want claude-4.6 2/1/1", m2)
	}
	if m2.Streaming != 0 {
		t.Errorf("streaming=%d want 0（非流式）", m2.Streaming)
	}
	// 无首字观测：TTFB 均值不得被 0 拉低（分母为 0 时不做除法）。
	if m2.AvgTTFBMS != 0 {
		t.Errorf("avg_ttfb=%.2f want 0（无首字观测）", m2.AvgTTFBMS)
	}

	// total 必须等于各模型之和（同口径，不另算一份）。
	if s.Total.Requests != 5 || s.Total.Success != 3 || s.Total.Failed != 2 {
		t.Errorf("total req/succ/fail = %d/%d/%d want 5/3/2", s.Total.Requests, s.Total.Success, s.Total.Failed)
	}
	if s.Total.PromptTokens != 120 || s.Total.CompletionTokens != 305 {
		t.Errorf("total tokens = %d/%d want 120/305", s.Total.PromptTokens, s.Total.CompletionTokens)
	}
	if s.Total.Model != "total" {
		t.Errorf("total.model=%q want total", s.Total.Model)
	}
	if s.UptimeSec < 0 {
		t.Errorf("uptime_sec=%d 不得为负", s.UptimeSec)
	}
}

// 请求数相同时按模型名升序（输出稳定，前端 diff 不抖）。
func TestStatsStableOrderOnTie(t *testing.T) {
	r := New("")
	now := time.Now()
	for _, m := range []string{"zeta", "alpha", "mid"} {
		r.Add(now, "cn", "u1", m, Delta{LatencyMs: 1, HasLatency: true}, true)
	}
	s := r.Stats(0)
	if len(s.Models) != 3 {
		t.Fatalf("models=%d want 3", len(s.Models))
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if s.Models[i].Model != w {
			t.Errorf("models[%d]=%q want %q（同请求数按名字升序）", i, s.Models[i].Model, w)
		}
	}
}

// 空数据（全新实例）不 panic，且给出完整结构 + enabled 标记。
func TestStatsEmpty(t *testing.T) {
	r := New("")
	s := r.Stats(0)
	if !s.Enabled {
		t.Error("enabled=false want true（记录器已装配）")
	}
	if len(s.Models) != 0 {
		t.Errorf("models=%d want 0", len(s.Models))
	}
	if s.Total.Requests != 0 || s.Total.PromptTokens != 0 {
		t.Errorf("total 应为零值，得到 %+v", s.Total)
	}
	if s.Since.IsZero() || s.Now.IsZero() {
		t.Error("since/now 不得为零值时间（消费方直接解析时间戳）")
	}
	// 空数据时 since=now：不编造一个看起来有覆盖时长的起点。
	if s.UptimeSec != 0 {
		t.Errorf("uptime_sec=%d want 0（无数据起点）", s.UptimeSec)
	}
	// JSON 里 models 必须是数组而非 null（消费方按数组遍历）。
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"models":[]`) {
		t.Errorf("空 models 必须序列化成 []，得到 %s", raw)
	}
}

// nil 记录器（handler 未装配 usage）返回 enabled=false 的完整结构，不 panic。
func TestStatsNilRecorder(t *testing.T) {
	var r *Recorder
	s := r.Stats(0)
	if s.Enabled {
		t.Error("nil 记录器应 enabled=false")
	}
	if s.Message == "" {
		t.Error("enabled=false 时必须给出 message 说明原因")
	}
	if s.Since.IsZero() || s.Now.IsZero() {
		t.Error("since/now 不得为零值")
	}
}

// 缓存字段缺失时不产生假命中率：分母为 0 则比率为 0（不做除法）。
func TestStatsNoCacheObservationNoRate(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m1", Delta{
		PromptTokens: 100, HasPromptTokens: true,
		CompletionTokens: 10, HasCompletion: true,
		LatencyMs: 100, HasLatency: true,
	}, true) // 没有 HasCache
	s := r.Stats(0)
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.CacheHitRate != 0 || m.CacheHitTokens != 0 || m.CacheMissTokens != 0 {
		t.Errorf("无缓存观测时不得产出命中率，得到 %+v", m)
	}
	if m.PromptTokens != 100 {
		t.Errorf("prompt=%d want 100（token 观测不受缓存缺失影响）", m.PromptTokens)
	}
}

// hours 窗口过滤：窗口外的小时桶不计入，窗口内的计入。
func TestStatsWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	// 5 小时前的桶（hours=1 窗口外）
	r.Add(now.Add(-5*time.Hour), "cn", "u1", "old-model", Delta{
		PromptTokens: 999, HasPromptTokens: true, LatencyMs: 1, HasLatency: true,
	}, true)
	// 当前小时的桶（窗口内）
	r.Add(now, "cn", "u1", "new-model", Delta{
		PromptTokens: 7, HasPromptTokens: true, LatencyMs: 1, HasLatency: true,
	}, true)

	all := r.Stats(0)
	if len(all.Models) != 2 || all.Total.PromptTokens != 1006 {
		t.Fatalf("全量窗口 = %d 模型 / %d token, want 2/1006", len(all.Models), all.Total.PromptTokens)
	}
	win := r.Stats(1)
	if len(win.Models) != 1 || win.Models[0].Model != "new-model" {
		t.Fatalf("1 小时窗口 = %+v, want 仅 new-model（当前小时桶必须保留）", win.Models)
	}
	if win.Total.PromptTokens != 7 {
		t.Errorf("窗口内 total prompt=%d want 7", win.Total.PromptTokens)
	}
}

// 落盘→恢复后统计口径不变（新字段随桶一起持久化）。
func TestStatsSurvivesFlushLoad(t *testing.T) {
	path := t.TempDir() + "/usage.json"
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{
		PromptTokens: 50, HasPromptTokens: true,
		CompletionTokens: 100, HasCompletion: true,
		LatencyMs: 3000, HasLatency: true,
		CacheHit: 800, CacheMiss: 200, HasCache: true,
		Credit: 0.02, HasCredit: true,
		Stream: true, TTFBMs: 500, HasTTFB: true,
		GenerationMs: 2500, HasGenerationMs: true,
	}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Stats(0)
	if len(s.Models) != 1 {
		t.Fatalf("恢复后 models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.CacheHitTokens != 800 || m.CacheMissTokens != 200 {
		t.Errorf("恢复后 cache hit/miss = %d/%d want 800/200", m.CacheHitTokens, m.CacheMissTokens)
	}
	if m.CacheHitRate != 0.8 {
		t.Errorf("恢复后 hit_rate=%.4f want 0.8", m.CacheHitRate)
	}
	if m.Streaming != 1 {
		t.Errorf("恢复后 streaming=%d want 1", m.Streaming)
	}
	if m.AvgTTFBMS != 500 {
		t.Errorf("恢复后 avg_ttfb=%.2f want 500", m.AvgTTFBMS)
	}
	if m.Credit < 0.0199 || m.Credit > 0.0201 {
		t.Errorf("恢复后 credit=%.4f want 0.02", m.Credit)
	}
	if m.LastSeen == nil {
		t.Error("恢复后 last_seen 丢失")
	}
}

// 小时桶折叠为日桶时新增字段必须一并折叠（否则缓存/首字观测被静默丢弃）。
func TestStatsRollupKeepsNewFields(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 超出 90 天小时保留
	r.Add(old, "cn", "u1", "m1", Delta{
		PromptTokens: 10, HasPromptTokens: true,
		CompletionTokens: 5, HasCompletion: true,
		LatencyMs: 200, HasLatency: true,
		CacheHit: 700, CacheMiss: 100, HasCache: true,
		Credit: 0.01, HasCredit: true,
		Stream: true, TTFBMs: 100, HasTTFB: true,
		GenerationMs: 100, HasGenerationMs: true,
	}, true)

	r.Rollup(time.Now())

	s := r.Stats(0)
	if len(s.Models) != 1 {
		t.Fatalf("折叠后 models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.CacheHitTokens != 700 || m.CacheMissTokens != 100 {
		t.Errorf("折叠后 cache = %d/%d want 700/100（新字段必须在折叠里累加）",
			m.CacheHitTokens, m.CacheMissTokens)
	}
	if m.Streaming != 1 {
		t.Errorf("折叠后 streaming=%d want 1", m.Streaming)
	}
	if m.AvgTTFBMS != 100 {
		t.Errorf("折叠后 avg_ttfb=%.2f want 100", m.AvgTTFBMS)
	}
	if m.Credit < 0.0099 || m.Credit > 0.0101 {
		t.Errorf("折叠后 credit=%.4f want 0.01", m.Credit)
	}
}

// 并发：Add 与 Stats 同时跑不触发数据竞争（-race 下必须干净）。
func TestStatsConcurrentWithAdd(t *testing.T) {
	r := New("")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.Add(time.Now(), "cn", "u1", "m1", Delta{
				PromptTokens: 1, HasPromptTokens: true,
				CompletionTokens: 1, HasCompletion: true,
				LatencyMs: 1, HasLatency: true,
				CacheHit: 1, CacheMiss: 1, HasCache: true,
			}, true)
		}
	}()
	for i := 0; i < 50; i++ {
		_ = r.Stats(0)
	}
	<-done
	if got := r.Stats(0).Total.Requests; got != 200 {
		t.Errorf("requests=%d want 200", got)
	}
}

// 旧版落盘文件（无新增字段）读入后统计不 panic、旧视图口径不变。
func TestStatsLoadLegacyFile(t *testing.T) {
	path := t.TempDir() + "/usage.json"
	// 模拟 v1.9.16 之前写出的文件：桶里只有旧字段。
	legacy := `{"version":1,"saved":"2026-09-01T00:00:00+08:00","buckets":[
		{"s":"h:2026-09-01T10","r":"cn","u":"u1","m":"glm-5.2","q":3,"e":1,"p":100,"c":50,"t":150,"l":900,"ln":3,"v":10,"vn":2}]}`
	if err := writeFileAtomic(path, []byte(legacy)); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Stats(0)
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.Requests != 3 || m.Success != 2 || m.Failed != 1 {
		t.Errorf("req/succ/fail = %d/%d/%d want 3/2/1", m.Requests, m.Success, m.Failed)
	}
	if m.PromptTokens != 100 || m.CompletionTokens != 50 {
		t.Errorf("tokens = %d/%d want 100/50", m.PromptTokens, m.CompletionTokens)
	}
	// 旧数据没有生成时长列：退回逐请求速率均值（口径与面板 AvgTPS 一致），不编造。
	if m.TokensPerSec != 5 {
		t.Errorf("tokens_per_sec=%.4f want 5（旧数据退回速率样本均值）", m.TokensPerSec)
	}
	if m.CacheHitRate != 0 {
		t.Errorf("旧数据无缓存观测，hit_rate 应为 0，得到 %.4f", m.CacheHitRate)
	}
}
