package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
//
// 「不重复计数」走 Stats(0)（不限窗口的全量口径）验证：折叠后的日桶在 100 天前，
// 已落在面板窗口（最长 60 天）之外，Snapshot 看不到它——但幂等是**账本层面**的
// 性质，必须用全量口径验，不能被窗口掩盖成「看起来对」。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Stats(0)
	if after.Total.Requests != 3 || after.Total.PromptTokens != 15 {
		t.Fatalf("折叠后 totals = %d/%d, want 3/15", after.Total.Requests, after.Total.PromptTokens)
	}
	// 窗口内那一份照旧可见：折叠不得把它并进日桶或重复计数。
	snap := r.Snapshot(24, nil)
	if snap.Totals.Requests != 1 || snap.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1", snap.Totals.Requests, snap.Totals.PromptTokens)
	}

	r.Rollup(time.Now())
	again := r.Stats(0)
	if again.Total.Requests != 3 || again.Total.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Total.Requests, again.Total.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// 窗口必须作用于**汇总与两张表**：窗口内与窗口外的桶混在一起时，totals 与
// by_account / by_model 只能含窗口内那一份。
//
// 这是现象 A 的回归护栏。旧实现只把 hours 用在时序上（hourFrom 只筛
// hourSeries），total / realmAgg / acctAgg / modelAgg 对**全部**桶无条件累加，
// 于是切换范围时顶部六个汇总数字与「按账号」「按模型」两张表逐字不变，只有
// 柱状图在变——用户原话「底下的那个显示条没有变化只有条形显示柱有变化」。
func TestSnapshotWindowAppliesToTotalsAndTables(t *testing.T) {
	r := New("")
	now := time.Now()
	// 窗口内（hours=24）：一个账号、一个模型，数值小。
	r.Add(now, "cn", "uid-in", "model-in", Delta{
		PromptTokens: 10, HasPromptTokens: true,
		CompletionTokens: 5, HasCompletion: true,
		LatencyMs: 100, HasLatency: true,
	}, true)
	// 窗口外（48 小时前）：另一个账号、另一个模型、数值大，且是**失败尝试**。
	// 混进来会让下面每一项都翻好几倍，一眼可辨。
	r.Add(now.Add(-48*time.Hour), "cn", "uid-out", "model-out", Delta{
		PromptTokens: 9999, HasPromptTokens: true,
		CompletionTokens: 9999, HasCompletion: true,
		LatencyMs: 9000, HasLatency: true,
	}, false)

	s := r.Snapshot(24, map[string]string{"uid-in": "窗口内", "uid-out": "窗口外"})

	if s.Totals.Requests != 1 || s.Totals.Errors != 0 {
		t.Errorf("totals req/err = %d/%d, want 1/0（窗口外的桶必须被排除）", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 10 || s.Totals.CompletionTok != 5 {
		t.Errorf("totals pt/ct = %d/%d, want 10/5", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	// 均值分母同样要裁剪：窗口外的 9000ms 若进分母，这里会变成 4550。
	if s.Totals.AvgLatencyMs != 100 {
		t.Errorf("totals avg latency = %v, want 100（窗口外样本不得进均值分母）", s.Totals.AvgLatencyMs)
	}

	if len(s.ByAccount) != 1 || s.ByAccount[0].Key != "uid-in" {
		t.Fatalf("by_account = %+v, want 仅 uid-in", s.ByAccount)
	}
	if s.ByAccount[0].PromptTokens != 10 || s.ByAccount[0].Requests != 1 {
		t.Errorf("by_account 行 = %+v, want prompt=10 requests=1", s.ByAccount[0])
	}

	if len(s.ByModel) != 1 || s.ByModel[0].Key != "model-in" {
		t.Fatalf("by_model = %+v, want 仅 model-in", s.ByModel)
	}
	if s.ByModel[0].PromptTokens != 10 {
		t.Errorf("by_model 行 prompt = %d, want 10", s.ByModel[0].PromptTokens)
	}

	// 时序与汇总同口径：窗口内只有 1 个点，点上的量必须等于汇总。
	var sumPT int64
	for _, p := range s.Series {
		sumPT += p.PromptTokens
	}
	if sumPT != s.Totals.PromptTokens {
		t.Errorf("series 的 prompt 合计 = %d, totals = %d（时序与汇总必须同口径）", sumPT, s.Totals.PromptTokens)
	}
}

// 窗口必须作用于 by_realm。单独一条：realm 是独立累加器，漏改一处就会出现
// 「域表与账号表对不上账」——比整块不变更难发现。
func TestSnapshotWindowAppliesToRealm(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m1", Delta{PromptTokens: 3, HasPromptTokens: true}, true)
	r.Add(now.Add(-48*time.Hour), "global", "u2", "m2", Delta{PromptTokens: 700, HasPromptTokens: true}, true)

	s := r.Snapshot(24, nil)
	if len(s.ByRealm) != 1 || s.ByRealm[0].Key != "cn" {
		t.Fatalf("by_realm = %+v, want 仅 cn（窗口外的 global 必须被排除）", s.ByRealm)
	}
	if s.ByRealm[0].PromptTokens != 3 {
		t.Errorf("by_realm prompt = %d, want 3", s.ByRealm[0].PromptTokens)
	}
	// 三张表与 totals 必须同口径：窗口内只有一个域，域表请求数应等于汇总请求数。
	if s.ByRealm[0].Requests != s.Totals.Requests {
		t.Errorf("by_realm 与 totals 不同口径：%d vs %d", s.ByRealm[0].Requests, s.Totals.Requests)
	}
}

// 粒度单选：按窗口只取**一种**粒度，不再把小时点与日点拼在同一条轴上。
//
// 规则：窗口 <= 7 天（168h）→ 只用小时点；更宽 → 只用日点。阈值见
// granularityHourMaxHours 的注释（为什么是 7 天）。
//
// 这是现象 B 的回归护栏。旧实现把「窗口内的小时点」与「窗口外的日点」拼成一条
// 序列：选「近 30 天」横轴全是小时标签（22:00 / 11:00），选「近 24 小时」横轴
// 全是日标签（9-17 / 9-18），tooltip 里还混着 "(day)"——用户无法判断一根柱子
// 代表一小时还是一天。
func TestSnapshotSingleGranularity(t *testing.T) {
	r := New("")
	now := time.Now()
	// 一个桶在窗口内、一个在 48 小时前：旧实现会把后者折成日点塞进同一条轴。
	r.Add(now, "cn", "u1", "m1", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	r.Add(now.Add(-48*time.Hour), "cn", "u1", "m1", Delta{PromptTokens: 2, HasPromptTokens: true}, true)

	for _, tc := range []struct {
		name     string
		hours    int
		wantGran string
		wantPts  int
	}{
		// 24 小时窗口：48 小时前的桶在窗口外 → 只剩 1 个小时点。
		{"近 24 小时只用小时点", 24, "hour", 1},
		// 3 天 / 7 天：仍在小时粒度区间内，48 小时前的桶是小时点（不折成日点）。
		{"近 3 天只用小时点", 72, "hour", 2},
		{"近 7 天（阈值上限）只用小时点", 7 * 24, "hour", 2},
		// 30 天：越过阈值 → 整条序列改用日点，两个桶折到各自所在的日历日。
		// 48 小时跨度必然跨两个日历日，点数因此稳定为 2。
		{"近 30 天只用日点", 720, "day", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := r.Snapshot(tc.hours, nil)
			// 合法窗口必须原样生效，并如实回报（前端据此显示「窗口：近 N 天」）。
			if s.Hours != tc.hours {
				t.Errorf("hours=%d want %d（合法窗口不得被改写）", s.Hours, tc.hours)
			}
			if s.Granularity != tc.wantGran {
				t.Errorf("granularity=%q want %q（粒度由后端单选并显式回报）", s.Granularity, tc.wantGran)
			}
			if len(s.Series) != tc.wantPts {
				t.Fatalf("series=%d 点 want %d: %+v", len(s.Series), tc.wantPts, s.Series)
			}
			for _, p := range s.Series {
				if p.Scope != tc.wantGran {
					t.Errorf("点 %s 的 scope=%q want %q（一条序列里不得出现第二种粒度）", p.T, p.Scope, tc.wantGran)
				}
				// 键格式必须跟着粒度走：前端直接拿它渲染横轴标签
				// （小时粒度 "HH:00"、日粒度 "M-D"）。
				if tc.wantGran == "day" && len(p.T) != len("2006-01-02") {
					t.Errorf("日点键 %q 不是 2006-01-02 格式", p.T)
				}
				if tc.wantGran == "hour" && len(p.T) != len("2006-01-02T15") {
					t.Errorf("小时点键 %q 不是 2006-01-02T15 格式", p.T)
				}
			}
		})
	}
}

// 窗口外的桶既不进汇总也不进时序；但账本本身不受影响（全量口径仍看得见）。
//
// 旧实现会把窗口外的桶折成日点留在时序里（并因此让整张图的横轴变成日期标签），
// 那是现象 B 的直接来源；这里把「窗口外 = 彻底不出现」钉死。
func TestSnapshotWindowExcludesOutOfWindowPoints(t *testing.T) {
	r := New("")
	now := time.Now()
	// 100 天前的桶：折叠后会变成日桶，永久保留在账本里。
	r.Add(now.AddDate(0, 0, -100), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)
	r.Rollup(now)

	s := r.Snapshot(24, nil)
	if len(s.Series) != 1 {
		t.Fatalf("series = %+v, want 仅窗口内的 1 个点（100 天前的日桶不得混进来）", s.Series)
	}
	if s.Series[0].PromptTokens != 3 || s.Series[0].Scope != "hour" {
		t.Errorf("series[0] = %+v, want 窗口内的小时点 pt=3", s.Series[0])
	}
	if s.Totals.PromptTokens != 3 || s.Totals.Requests != 1 {
		t.Errorf("totals pt/req = %d/%d, want 3/1（窗口外的日桶同样不进汇总）",
			s.Totals.PromptTokens, s.Totals.Requests)
	}
	// 账本没有被窗口裁剪：全量口径（/v1/stats 的缺省语义）仍然看得到折叠后的日桶。
	if got := r.Stats(0).Total.PromptTokens; got != 8 {
		t.Errorf("Stats(0) prompt=%d want 8（窗口只裁剪面板视图，不动账本）", got)
	}
}

// 非法 hours 回退到缺省窗口（72 = 近 3 天），且回退结果必须与**显式传 72 逐字
// 一致**——回退不是「换个窗口算」，而是真的按 72 小时算。
func TestSnapshotInvalidHoursFallsBack(t *testing.T) {
	r := New("")
	now := time.Now()
	// 24 小时内一个桶、48 小时前一个桶：缺省窗口（72h）两个都该在。
	r.Add(now, "cn", "u1", "m1", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	r.Add(now.Add(-48*time.Hour), "cn", "u1", "m1", Delta{PromptTokens: 20, HasPromptTokens: true}, true)

	base := r.Snapshot(72, nil)
	if base.Totals.PromptTokens != 30 {
		t.Fatalf("显式 72 小时窗口 prompt=%d want 30（用例前提不成立）", base.Totals.PromptTokens)
	}
	for _, hours := range []int{0, -1, -72, 24*60 + 1, 999999999} {
		s := r.Snapshot(hours, nil)
		if s.Hours != 72 {
			t.Errorf("hours=%d 回退后 Hours=%d want 72（响应必须给出实际生效的窗口）", hours, s.Hours)
		}
		if s.Granularity != GranularityHour {
			t.Errorf("hours=%d 回退到 72 后 granularity=%q want %q", hours, s.Granularity, GranularityHour)
		}
		if s.Totals != base.Totals {
			t.Errorf("hours=%d 的汇总 %+v != 显式 72 的 %+v", hours, s.Totals, base.Totals)
		}
		if len(s.Series) != len(base.Series) {
			t.Errorf("hours=%d 的 series=%d 点 want %d", hours, len(s.Series), len(base.Series))
		}
	}
	// 上限本身（1440 = 60 天）是合法窗口，不得被当成非法值回退。
	if s := r.Snapshot(24*60, nil); s.Totals.PromptTokens != 30 || s.Hours != 24*60 {
		t.Errorf("hours=1440（上限）应原样生效，得到 prompt=%d hours=%d", s.Totals.PromptTokens, s.Hours)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// ------------------------------------------------------------ 并发落盘 ----

// syncLog 是并发安全的日志收集器（flush 的失败只写日志，测试靠它看见）。
type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *syncLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, string(p))
	return len(p), nil
}

func (w *syncLog) failures() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, l := range w.lines {
		if strings.Contains(l, "建临时文件失败") || strings.Contains(l, "写临时文件失败") || strings.Contains(l, "原子替换失败") {
			out = append(out, l)
		}
	}
	return out
}

// captureLog 把 log 输出重定向到并发安全收集器，返回还原函数。
func captureLog() (*syncLog, func()) {
	prev := log.Writer()
	sink := &syncLog{}
	log.SetOutput(sink)
	return sink, func() { log.SetOutput(prev) }
}

// tempDir 是并发写盘用例专用的临时目录（替代 t.TempDir）。
//
// 原因：高并发 rename 之后，Windows 上 t.TempDir 的 RemoveAll 会偶发
// "directory is not empty" ——测试函数返回时 OS 可能仍持有刚被 rename 的文件
// 句柄，删除撞上延迟释放。已用不含本包任何代码的裸探针复现同一现象，属平台
// 行为而非被测逻辑；这里自建目录并在清理时重试，不削弱任何断言。
// POSIX 上首次即成功，无额外开销。
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wb2api-usage-test-")
	if err != nil {
		t.Fatalf("建临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 40; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		// 兜底：清理失败不改变测试结论，但把路径打出来便于排查。
		t.Logf("临时目录未能清理（Windows 句柄延迟释放，非测试失败）：%s", dir)
	})
	return dir
}

// newBigRecorder 造一个桶数较多的 Recorder：写盘窗口够宽，竞态才暴露得出来
// （桶太少时 marshal+write 的临界区过短，固定 tmp 名的互踩也未必命中）。
func newBigRecorder(t *testing.T, path string, buckets int) *Recorder {
	t.Helper()
	r := New(path)
	now := time.Now()
	for i := 0; i < buckets; i++ {
		r.Add(now, "cn", fmt.Sprintf("uid-%03d", i), fmt.Sprintf("model-%02d", i%17),
			Delta{PromptTokens: int64(i), HasPromptTokens: true, TotalTokens: int64(i), HasTotal: true}, true)
	}
	return r
}

// TestConcurrentSaveFlushNoClobber 并发 Save（面板「刷新」/ 关闭前）与 30s ticker
// 的 flush 不得互踩。旧实现固定用 r.path+".tmp"：两个写者共用同一临时文件——
// 后写者截断前者正在写的文件，先完成的一方又把另一方尚未写完的临时文件 rename
// 进正式路径，后者再 rename 就报 ENOENT；半截 JSON 也会被搬进正式路径。
// 断言：①无失败日志 ②无残留临时文件 ③终态可解析且桶数完整（没丢快照）。
func TestConcurrentSaveFlushNoClobber(t *testing.T) {
	dir := tempDir(t)
	path := filepath.Join(dir, "usage.json")
	const buckets = 300
	r := newBigRecorder(t, path, buckets)

	sink, restore := captureLog()
	defer restore()

	// 写者：Save（force=true）与 ticker 式 flush（force=false）并发。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 30; n++ {
				if i%2 == 0 {
					r.Save()
				} else {
					r.flush(false)
					r.mu.Lock()
					r.dirty = true // 模拟 ticker 间隔内有新数据，保证 flush 真的写盘
					r.mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()

	if got := sink.failures(); len(got) != 0 {
		t.Fatalf("并发落盘出现失败日志 %d 条：%v", len(got), got)
	}

	// 无残留临时文件（成功路径 rename 走，失败路径 unlink 掉）。
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}

	// 终态可解析，且数据完整（桶都在，说明没有整份快照丢失）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("终态文件不可读: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("终态文件非法 JSON: %v", err)
	}
	if len(f.Buckets) != buckets {
		t.Errorf("终态桶数 = %d, want %d（有快照丢失）", len(f.Buckets), buckets)
	}
	if f.Version != 1 {
		t.Errorf("终态版本号 = %d, want 1", f.Version)
	}
}

// TestFlushReadersNeverSeePartialJSON 落盘期间读者只能看到完整快照：临时文件
// 与正式文件分离 + rename 原子替换，保证读侧不会读到写了一半的 JSON。
//
// 仅 POSIX：Windows 上 os.Rename 覆盖「正被其它句柄打开」的目标会被 OS 以
// ERROR_SHARING_VIOLATION / ACCESS_DENIED 拒绝（Go 在该平台没有 rename-by-handle
// 语义可用），这是平台差异而非本修复的缺陷。生产目标是 Linux 容器，故跳过。
func TestFlushReadersNeverSeePartialJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows：rename 覆盖被占用的目标文件由 OS 拒绝，无法在此平台验证原子替换")
	}
	dir := tempDir(t)
	path := filepath.Join(dir, "usage.json")
	r := newBigRecorder(t, path, 300)

	sink, restore := captureLog()
	defer restore()

	var stop atomic.Bool
	var readersWg sync.WaitGroup
	var badReads atomic.Int64

	// 读者：持续读正式路径，任何时刻读到的都必须是完整 JSON。
	readersWg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer readersWg.Done()
			for !stop.Load() {
				raw, err := os.ReadFile(path)
				if err != nil {
					continue // 首次落盘前不存在，正常
				}
				var f file
				if err := json.Unmarshal(raw, &f); err != nil {
					badReads.Add(1)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 30; n++ {
				if i%2 == 0 {
					r.Save()
				} else {
					r.flush(false)
					r.mu.Lock()
					r.dirty = true
					r.mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()
	stop.Store(true)
	readersWg.Wait()

	if got := sink.failures(); len(got) != 0 {
		t.Fatalf("并发落盘出现失败日志 %d 条：%v", len(got), got)
	}
	if n := badReads.Load(); n != 0 {
		t.Fatalf("读者读到 %d 次非法 JSON（半截文件被 rename 进正式路径）", n)
	}
}

// TestFlushTempNameNotFixedLegacyPath 临时文件名不再是固定的 <path>.tmp：
// 预先把该路径占成目录，旧实现（os.WriteFile 到固定名）必然写失败且不落盘；
// 唯一命名（os.CreateTemp 的 <name>.*.tmp）则完全绕开它，照常落盘。
func TestFlushTempNameNotFixedLegacyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	// 占位：旧实现会在这里撞上 "is a directory"。
	legacyTmp := path + ".tmp"
	if err := os.Mkdir(legacyTmp, 0o700); err != nil {
		t.Fatalf("占位 %s 失败: %v", legacyTmp, err)
	}

	sink, restore := captureLog()
	defer restore()

	r := New(path)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r.Save()

	if got := sink.failures(); len(got) != 0 {
		t.Fatalf("落盘不应受 <path>.tmp 占位影响，却报错：%v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应已落盘: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Buckets) != 1 {
		t.Fatalf("落盘内容异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// TestFlushTempFileSameDirAndMode 临时文件与正式文件同目录、权限 0600：
// 同目录才保证 rename 是同一文件系统内的原子替换（跨设备会 EXDEV）；
// 0600 让用量数据不对外可读（与凭证同级）。
func TestFlushTempFileSameDirAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	r := New(path)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	r.Save()

	// 落盘后目录里只应有正式文件（临时文件已被 rename 走，不留垃圾）。
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "usage.json" {
		t.Fatalf("目录内容 = %v, want 仅 usage.json", names)
	}

	// 命名方案：同目录、<name>.*.tmp（CreateTemp 会把 * 换成随机段）。
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		t.Fatalf("同目录 CreateTemp 失败: %v", err)
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)

	if filepath.Dir(tmp) != dir {
		t.Errorf("临时文件目录 = %s, want %s（必须同目录才能原子 rename）", filepath.Dir(tmp), dir)
	}
	if !strings.HasPrefix(filepath.Base(tmp), "usage.json.") || !strings.HasSuffix(filepath.Base(tmp), ".tmp") {
		t.Errorf("临时文件名 = %s, want usage.json.*.tmp", filepath.Base(tmp))
	}
	if runtime.GOOS != "windows" {
		// Windows 无 POSIX 权限位（Go 只映射只读位，恒报 0666），跳过权限断言。
		if fi, err := os.Stat(tmp); err != nil {
			t.Errorf("临时文件不可 stat: %v", err)
		} else if fi.Mode().Perm() != 0o600 {
			t.Errorf("临时文件权限 = %o, want 600（用量数据不对外可读）", fi.Mode().Perm())
		}
	}
}
