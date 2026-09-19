// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，单桶约百字节（字段一律
// 短名落盘，因为桶数量会随时间增长）。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个 (时间片, realm, uid, model) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"` // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`
	UID   string  `json:"u"`
	Model string  `json:"m"`
	Req   int64   `json:"q"`  // 请求数（含失败）
	Err   int64   `json:"e"`  // 失败数
	PT    int64   `json:"p"`  // prompt tokens
	CT    int64   `json:"c"`  // completion tokens
	TT    int64   `json:"t"`  // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`  // 延迟累计（ms）
	LatN  int64   `json:"ln"` // 延迟样本数
	TPS   float64 `json:"v"`  // 吐字速率累计
	TPSN  int64   `json:"vn"` // 速率样本数

	// 缓存三段与首字延迟（供 /v1/stats；字段名同 pool.TokenUsageDelta 的语义）。
	// 旧落盘文件缺这些字段时读入即零值，聚合侧不做除法（分母为 0 不出命中率），
	// 因此升级不改变既有视图的任何数值。
	CH     int64   `json:"ch"` // 缓存命中 tokens（prompt_cache_hit_tokens）
	CM     int64   `json:"cm"` // 缓存未命中 tokens
	CW     int64   `json:"cw"` // 缓存写入 tokens
	Cr     float64 `json:"cr"` // 真实扣费累计（上游 usage.credit，缺观测时恒 0）
	Str    int64   `json:"st"` // 流式请求数
	TtfbMs int64   `json:"fm"` // 首字延迟累计（ms，仅流式有观测）
	TtfbN  int64   `json:"fn"` // 首字延迟样本数
	GenMs  int64   `json:"gm"` // 生成时长累计（ms，端到端减去首字），供吞吐折算
	Last   int64   `json:"ls"` // 最近一次请求的 unix 秒（供 /v1/stats 的 last_seen）
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model
	dirty   bool
	started time.Time

	// saveMu 串行化落盘 I/O（快照复制与 marshal 仍在 mu 下，写文件不占数据锁）。
	// 唯一临时文件名已经消除「共用 <path>.tmp 互相截断」，但两个写者仍会同时
	// rename 同一个目标：POSIX 上后到者直接覆盖（无害），Windows 上对「正被另
	// 一个句柄重命名的目标」会报 ACCESS_DENIED，失败路径再去 unlink 已 rename
	// 走的临时文件，就会在目录里留下垃圾。串行化后写盘互斥，两个平台行为一致。
	saveMu sync.Mutex

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
//
// Has* 字段的纪律与 pool 侧一致：区分「上游没给该字段」与「上游显式给了 0」。
// 统计口径只采信上游 usage，不做 rune 估算（缺失即不累加，不伪造观测）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	LatencyMs        int64
	HasLatency       bool
	TokensPerSecond  float64
	HasTPS           bool

	// 缓存三段（上游 usage.prompt_cache_*_tokens）。命中率分母 = 命中 + 未命中，
	// 不含写入——写入是「为后续命中付的费」，计入分母会把首次请求的命中率压低。
	CacheHit        int64
	CacheMiss       int64
	CacheWrite      int64
	HasCache        bool // 上游末帧带了任一缓存字段
	Credit          float64
	HasCredit       bool
	Stream          bool  // 本次尝试是流式
	TTFBMs          int64 // 首字延迟（仅流式有观测）
	HasTTFB         bool
	GenerationMs    int64 // 生成时长（端到端 − 首字），供 tokens/s 折算
	HasGenerationMs bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	// 缓存三段只在有观测时累加（缺失 ≠ 0；分母因此不会把「没观测」算成未命中）。
	if d.HasCache {
		b.CH += d.CacheHit
		b.CM += d.CacheMiss
		b.CW += d.CacheWrite
	}
	if d.HasCredit {
		b.Cr += d.Credit
	}
	if d.Stream {
		b.Str++
	}
	// 首字延迟只在有观测时累加：非流式没有「首帧」概念（恒 0），计入会把均值
	// 拉低失真——分母用 TtfbN 而非请求数，正是为此。
	if d.HasTTFB {
		b.TtfbMs += d.TTFBMs
		b.TtfbN++
	}
	if d.HasGenerationMs && d.GenerationMs > 0 {
		b.GenMs += d.GenerationMs
	}
	// 最近活跃时间取本桶内最大值：折叠（小时→日）时取 max 而非覆盖，否则
	// last_seen 会退回到被折叠的那个小时，看起来像「很久没流量」。
	if ts := now.Unix(); ts > b.Last {
		b.Last = ts
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
			// 新增字段同样要折叠，否则小时桶转日桶时缓存/首字观测被静默丢弃。
			dst.CH += src.CH
			dst.CM += src.CM
			dst.CW += src.CW
			dst.Cr += src.Cr
			dst.Str += src.Str
			dst.TtfbMs += src.TtfbMs
			dst.TtfbN += src.TtfbN
			dst.GenMs += src.GenMs
			if src.Last > dst.Last {
				dst.Last = src.Last
			}
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	// 落盘 I/O 串行化（不占 mu：marshal 与写盘都不阻塞 Add/Snapshot）。
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	if err := writeFileAtomic(r.path, raw); err != nil {
		log.Printf("[usage] %v", err)
	}
}

// writeFileAtomic 原子写文件：同目录唯一临时文件 + rename 替换。
//
// 临时文件名必须带唯一后缀（os.CreateTemp 的随机段），不能是固定的
// <path>.tmp：Save（面板刷新 / 关闭前）与 30s ticker 的 flush 会并发进入
// 这里，共用一个临时路径时两个写者互相截断——先完成的一方把另一方尚未写完
// 的文件 rename 进正式路径，后者再 rename 就报 ENOENT（hub 版 wb_proxy.py
// 的 usage-summary 落盘是同一处竞态，修法一致）。
// 临时文件与正式文件同目录：rename 才是同文件系统内的原子替换（跨设备 EXDEV）。
//
// 调用方需持 Recorder.saveMu 串行化（见该字段注释）。
func writeFileAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建目录失败: %v", err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("建临时文件失败: %v", err)
	}
	tmp := f.Name()
	// CreateTemp 已按 0600 建；显式再设一次，避免 umask / 平台差异让用量数据外泄。
	_ = f.Chmod(0o600)
	_, werr := f.Write(raw)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// 失败路径不留垃圾临时文件。
		_ = os.Remove(tmp)
		return fmt.Errorf("写临时文件失败: %v", werr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("原子替换失败: %v", err)
	}
	return nil
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTPS        float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals    Agg        `json:"totals"`
	ByRealm   []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel   []KeyedAgg `json:"by_model"`
	Series    []Point    `json:"series"`
	Buckets   int        `json:"buckets"`
	FileBytes int64      `json:"file_bytes"`
	Since     string     `json:"since,omitempty"`
	Generated string     `json:"generated"`
}

// Snapshot 聚合当前全部桶。hours 控制时序返回多少个小时点（其余按日折叠）。
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	if hours <= 0 || hours > 24*60 {
		hours = 72
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}

	nowHour := time.Now().Truncate(time.Hour)
	hourFrom := nowHour.Add(-time.Duration(hours-1) * time.Hour)

	for i := range bs {
		b := &bs[i]
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
		// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		scope := strings.TrimPrefix(b.Scope, "h:")
		isHour := strings.HasPrefix(b.Scope, "h:")
		if isHour {
			ts, err := time.ParseInLocation(hourLayout, scope, time.Local)
			if err != nil {
				continue
			}
			if !ts.Before(hourFrom) {
				if hourSeries[scope] == nil {
					hourSeries[scope] = &aggAcc{}
				}
				hourSeries[scope].add(b)
			} else {
				// 超出小时窗口的细粒度数据并入其所在日，避免时序出现空洞。
				d := ts.Format(dayLayout)
				if daySeries[d] == nil {
					daySeries[d] = &aggAcc{}
				}
				daySeries[d].add(b)
			}
		} else {
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
	}

	snap := Snapshot{
		Totals:  total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) { return k, "" }),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:   keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		Buckets:   len(bs),
		Generated: time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// 最早的分片即数据起点。
	if len(snap.Series) > 0 {
		snap.Since = snap.Series[0].T
	}
	return snap
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
