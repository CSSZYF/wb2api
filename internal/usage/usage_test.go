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
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 3 || after.Totals.PromptTokens != 15 {
		t.Fatalf("折叠后 totals = %d/%d, want 3/15", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 2 || after.Series[0].Scope != "day" || after.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", after.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(24, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
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

// Snapshot 把小时窗口外的细粒度并入日点，时序不出现空洞。
func TestSnapshotStitching(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外 → 日点
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内 → 小时点
	s := r.Snapshot(24, nil)
	if len(s.Series) != 2 || s.Series[0].Scope != "day" || s.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v", s.Series)
	}
	if s.Series[0].PromptTokens != 5 || s.Series[1].PromptTokens != 3 {
		t.Fatalf("series tokens = %d/%d, want 5/3", s.Series[0].PromptTokens, s.Series[1].PromptTokens)
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
