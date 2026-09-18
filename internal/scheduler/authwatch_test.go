package scheduler

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// writeAuthFile 在 dir 下落一个合法 auth 文件（嵌套形，与 panel 登录落盘格式一致）。
func writeAuthFile(t *testing.T, dir, uid string) string {
	t.Helper()
	fp := filepath.Join(dir, "workbuddy-"+uid+".json")
	body := `{"auth":{"accessToken":"at-` + uid + `","refreshToken":"rt-` + uid +
		`","expiresAt":9999999999,"domain":"","realm":"cn"},"account":{"uid":"` + uid + `","nickname":"n-` + uid + `"}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// syncLogSink 并发安全的日志接收器：watcher 的后台 goroutine 打日志与测试 goroutine
// 读断言会同时发生，裸 strings.Builder 构成数据竞争（-race 实证）。本仓既有测试用的
// 都是「同一 goroutine 内写完再读」的场景，这里需要跨 goroutine，故加锁。
type syncLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncLogSink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// captureLog 捕获 log 输出（进程级全局，调用方不得并行；结束自动还原）。
func captureLog(t *testing.T) *syncLogSink {
	t.Helper()
	sink := &syncLogSink{}
	old := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(old) })
	return sink
}

// TestAuthWatcherAddsAndRemovesAccounts 一轮扫描：tempdir 里的新 auth 文件 → 池内出现该号；
// 文件删除 → 池内移除。这是本特性最核心的两条路径（手工上传/删除免重启）。
func TestAuthWatcherAddsAndRemovesAccounts(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	// 空目录：无变化（不 panic、不打日志）。
	if added, removed := w.scan(p); len(added) != 0 || len(removed) != 0 {
		t.Fatalf("空目录应为空变化: added=%v removed=%v", added, removed)
	}

	fp := writeAuthFile(t, dir, "u1")
	added, removed := w.scan(p)
	if len(added) != 1 || added[0] != "u1" {
		t.Fatalf("新增文件后 added=%v want [u1]", added)
	}
	if len(removed) != 0 {
		t.Fatalf("不应有剔除: %v", removed)
	}
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("u1 应已入池")
	}
	// 凭证与文件路径都要正确接线（FilePath 供 refresh 后写回原文件）。
	a := p.AuthByUID("u1")
	if a == nil {
		t.Fatal("AuthByUID(u1) = nil")
	}
	if got := a.AccessTokenValue(); got != "at-u1" {
		t.Errorf("accessToken=%q want at-u1", got)
	}
	if got := a.FilePath; got != fp {
		t.Errorf("FilePath=%q want %q", got, fp)
	}

	// 元信息未变的第二轮：不重复报 added（缓存命中，无副作用）。
	if added, removed := w.scan(p); len(added) != 0 || len(removed) != 0 {
		t.Fatalf("无变化轮应为空: added=%v removed=%v", added, removed)
	}

	// 删除文件 → 一轮后出池。
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	added, removed = w.scan(p)
	if len(added) != 0 {
		t.Fatalf("删除轮不应有新增: %v", added)
	}
	if len(removed) != 1 || removed[0] != "u1" {
		t.Fatalf("removed=%v want [u1]", removed)
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 应已出池")
	}
}

// TestAuthWatcherKeepsStateOnUpdate 文件内容更新（mtime/size 变化）走 upsert：
// 凭证换成新值，但池内运行态（积分/冷却/计数）保留——与启动对齐语义一致。
func TestAuthWatcherKeepsStateOnUpdate(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	fp := writeAuthFile(t, dir, "u1")
	w.scan(p)
	p.SetCredits("u1", 4242, 0)
	p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429")

	// 重写同一文件（换 accessToken；size 变化确保不依赖 mtime 精度）。
	body := `{"auth":{"accessToken":"at-u1-新","refreshToken":"rt-u1","expiresAt":9999999999,` +
		`"domain":"","realm":"cn"},"account":{"uid":"u1","nickname":"n-u1-新"}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	added, removed := w.scan(p)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("更新既有账号不应报新增/剔除: added=%v removed=%v", added, removed)
	}
	a := p.AuthByUID("u1")
	if a == nil || a.AccessTokenValue() != "at-u1-新" {
		t.Fatalf("凭证应已更新: %+v", a)
	}
	st, _ := p.Status("u1")
	if st.Credits != 4242 {
		t.Errorf("更新凭证不应丢积分: credits=%d want 4242", st.Credits)
	}
	if !st.Cooling {
		t.Errorf("更新凭证不应丢冷却状态: %+v", st)
	}
}

// TestAuthWatcherSkipsHalfWrittenFile 半截 JSON（正在写入）→ 跳过 + WARN，账号不出池；
// 写完后下一轮成功加载。这是「不要热加载正在写入的文件」的核心防呆。
func TestAuthWatcherSkipsHalfWrittenFile(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	fp := writeAuthFile(t, dir, "u1")
	w.scan(p)
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("前置：u1 应已入池")
	}
	// 模拟上传中途：文件被截断成半截 JSON（scp/编辑器保存的典型中间态）。
	if err := os.WriteFile(fp, []byte(`{"auth":{"accessToken":"at-`), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := captureLog(t)
	added, removed := w.scan(p)
	got := buf.String()

	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("半截文件不应产生增删: added=%v removed=%v", added, removed)
	}
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("半截文件不得把在用账号踢出池（keep 保护）")
	}
	if !strings.Contains(got, "WARN: auth watch:") || !strings.Contains(got, "解析失败") {
		t.Errorf("半截文件应打 WARN（含「解析失败」）便于运维识别，实际=%q", got)
	}
	// 池内凭证保持旧值（未被半截内容覆盖）。
	if a := p.AuthByUID("u1"); a == nil || a.AccessTokenValue() != "at-u1" {
		t.Fatalf("半截文件不应改写池内凭证: %+v", a)
	}

	// 写完（内容合法）→ 下一轮成功加载，且账号仍在池内。
	writeAuthFile(t, dir, "u1")
	added, removed = w.scan(p)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("补全后不应报增删（同 UID 已在池）: added=%v removed=%v", added, removed)
	}
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("补全后 u1 应在池内")
	}
}

// TestAuthWatcherKeepsAcrossMultipleBadRounds 坏文件**跨多轮**不解析时账号仍不出池。
//
// 回归钉（实现自测发现的真 bug）：scan 每轮把 w.prev 按目录现状整体重建，早先的实现
// 在坏文件轮直接丢弃其缓存条目，keep 因此只生效一轮——文件持续半截（上传中断后残留、
// 写入卡住、权限异常）时第二轮就被判成「文件已删除」剔除，与「账号只在文件确实从
// 目录消失时才出池」的约定相悖，且再入池会丢掉积分/冷却状态。
// 本用例连扫 3 轮坏文件，并断言期间不产生 removed、池内凭证保持旧值。
func TestAuthWatcherKeepsAcrossMultipleBadRounds(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	fp := writeAuthFile(t, dir, "u1")
	w.scan(p)
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("前置：u1 应已入池")
	}
	p.SetCredits("u1", 777, 0)

	if err := os.WriteFile(fp, []byte(`{"auth":{"accessToken":"at-`), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := captureLog(t)
	for i := 1; i <= 3; i++ {
		added, removed := w.scan(p)
		if len(added) != 0 || len(removed) != 0 {
			t.Fatalf("第 %d 轮坏文件不应产生增删（keep 必须跨轮结转）: added=%v removed=%v", i, added, removed)
		}
		if _, ok := p.Status("u1"); !ok {
			t.Fatalf("第 %d 轮：文件仍在目录中，账号不得出池", i)
		}
	}
	if !strings.Contains(buf.String(), "解析失败") {
		t.Errorf("每轮坏文件都应打 WARN，实际=%q", buf.String())
	}
	if a := p.AuthByUID("u1"); a == nil || a.AccessTokenValue() != "at-u1" {
		t.Fatalf("坏文件不应改写池内凭证: %+v", a)
	}
	if st, _ := p.Status("u1"); st.Credits != 777 {
		t.Errorf("跨轮 keep 期间积分不应丢失: credits=%d want 777", st.Credits)
	}

	// 补全 → 正常加载，凭证更新为文件里的新值，账号始终未出池。
	writeAuthFile(t, dir, "u1")
	if added, removed := w.scan(p); len(added) != 0 || len(removed) != 0 {
		t.Fatalf("补全后不应报增删: added=%v removed=%v", added, removed)
	}
	if a := p.AuthByUID("u1"); a == nil || a.AccessTokenValue() != "at-u1" {
		t.Fatalf("补全后应正常加载: %+v", a)
	}

	// 反面对照：文件真的从目录消失 → 仍然出池（keep 不把「已删除」也一起保护住）。
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	added, removed := w.scan(p)
	if len(added) != 0 || len(removed) != 1 || removed[0] != "u1" {
		t.Fatalf("文件删除后应剔除: added=%v removed=%v", added, removed)
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("文件已删除，账号应出池")
	}
}

// TestAuthWatcherSkipsHalfWrittenNewFile 新号上传中途（池内从未有这个 uid）：
// 半截 JSON → WARN 跳过、不入池；补全后下一轮入池。
func TestAuthWatcherSkipsHalfWrittenNewFile(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	fp := filepath.Join(dir, "workbuddy-u2.json")
	if err := os.WriteFile(fp, []byte(`{"auth":{"acce`), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := captureLog(t)
	if added, _ := w.scan(p); len(added) != 0 {
		t.Fatalf("半截新文件不得入池: %v", added)
	}
	if _, ok := p.Status("u2"); ok {
		t.Fatal("半截新文件不得入池")
	}
	if !strings.Contains(buf.String(), "WARN: auth watch:") {
		t.Errorf("应打 WARN，实际=%q", buf.String())
	}
	// 补全 → 下轮入池。
	writeAuthFile(t, dir, "u2")
	if added, _ := w.scan(p); len(added) != 1 || added[0] != "u2" {
		t.Fatalf("补全后 added=%v want [u2]", added)
	}
}

// TestAuthWatcherAddLogs 新增/删除必须打明细日志（运维据此确认手工上传已生效）。
func TestAuthWatcherAddLogs(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	buf := captureLog(t)
	fp := writeAuthFile(t, dir, "u7")
	w.scan(p)
	got := buf.String()
	if !strings.Contains(got, "auth watch: added uid=u7") {
		t.Errorf("新增应打 auth watch: added uid=... 实际=%q", got)
	}
	if !strings.Contains(got, "realm=cn") {
		t.Errorf("新增日志应带 realm（便于确认账号落哪个域）实际=%q", got)
	}

	buf.Reset()
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	w.scan(p)
	got = buf.String()
	if !strings.Contains(got, "auth watch: removed uid=u7") {
		t.Errorf("删除应打 auth watch: removed uid=... 实际=%q", got)
	}

	// 稳态轮：无任何 auth watch 明细（避免每 30 秒刷屏）。
	buf.Reset()
	writeAuthFile(t, dir, "u8")
	w.scan(p)
	buf.Reset()
	w.scan(p)
	if strings.Contains(buf.String(), "auth watch: added") || strings.Contains(buf.String(), "auth watch: removed") {
		t.Errorf("稳态轮不应打明细日志，实际=%q", buf.String())
	}
}

// TestStartAuthWatchTicker 后台循环：短间隔下自动发现新文件（不依赖真实 30s）。
func TestStartAuthWatchTicker(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	s := New(Config{Pool: p})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartAuthWatch(ctx, dir, 10*time.Millisecond)

	writeAuthFile(t, dir, "u3")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := p.Status("u3"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("后台循环未在期限内发现新账号文件")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 删除文件 → 后台循环同样在期限内出池。
	if err := os.Remove(filepath.Join(dir, "workbuddy-u3.json")); err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := p.Status("u3"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("后台循环未在期限内剔除已删除的账号")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// ctx 取消后循环退出（不泄漏 goroutine）：停止后新增文件不再被加载。
	cancel()
	time.Sleep(50 * time.Millisecond)
	writeAuthFile(t, dir, "u4")
	time.Sleep(100 * time.Millisecond)
	if _, ok := p.Status("u4"); ok {
		t.Fatal("ctx 取消后不应继续扫描")
	}
}

// TestStartAuthWatchDisabledDoesNotScan 开关关闭（interval=0）→ 不扫描：文件放着不入池。
func TestStartAuthWatchDisabledDoesNotScan(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	s := New(Config{Pool: p})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf := captureLog(t)
	s.StartAuthWatch(ctx, dir, 0) // 面板 auth_watch_enabled=false 的形态

	writeAuthFile(t, dir, "u5")
	time.Sleep(120 * time.Millisecond)
	if _, ok := p.Status("u5"); ok {
		t.Fatal("开关关闭时不得扫描目录（账号不应入池）")
	}
	if !strings.Contains(buf.String(), "auth watch: 已暂停") {
		t.Errorf("关闭状态应打一条暂停日志，实际=%q", buf.String())
	}

	// 热启用：同一循环被 rearm 唤醒后开始扫描（无需重启、无需重建 Scheduler）。
	s.SetAuthWatchInterval(10 * time.Millisecond)
	if got := s.AuthWatchInterval(); got != 10*time.Millisecond {
		t.Fatalf("AuthWatchInterval=%v want 10ms（热改应立即读到新值）", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := p.Status("u5"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("热启用后循环应在期限内开始扫描")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSetAuthWatchIntervalHotChange 间隔热改生效：先停（0）确认不扫描，再改回短间隔
// 确认恢复扫描；负值一律归 0（与 SetBalanceInterval 同口径，避免负数进 Load 后
// 被当成「已暂停」而语义不明）。
func TestSetAuthWatchIntervalHotChange(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	if got := s.AuthWatchInterval(); got != 0 {
		t.Fatalf("初始 interval=%v want 0（未启动）", got)
	}
	s.SetAuthWatchInterval(45 * time.Second)
	if got := s.AuthWatchInterval(); got != 45*time.Second {
		t.Fatalf("interval=%v want 45s", got)
	}
	s.SetAuthWatchInterval(-5 * time.Second)
	if got := s.AuthWatchInterval(); got != 0 {
		t.Fatalf("负值应归 0（暂停），got=%v", got)
	}
}

// TestStartAuthWatchEmptyDirNoop dir 为空（未配 auth_dir）时不启动：不 panic、不上报。
func TestStartAuthWatchEmptyDirNoop(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	s.StartAuthWatch(context.Background(), "", time.Second)
	if got := s.AuthWatchInterval(); got != 0 {
		t.Errorf("空目录不应启动循环（interval 应保持 0），got=%v", got)
	}
}

// TestAuthWatcherConcurrentWithPoolReads 扫描与并发读池不构成数据竞争
// （-race 下由本用例兜底：AuthByUID/AvailableUIDs 与 upsert/剔除并发）。
func TestAuthWatcherConcurrentWithPoolReads(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)
	for _, uid := range []string{"u1", "u2", "u3"} {
		writeAuthFile(t, dir, uid)
	}
	w.scan(p)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = p.AvailableUIDs()
				_ = p.List()
				_ = p.AuthByUID("u1")
			}
		}()
	}
	for i := 0; i < 20; i++ {
		w.scan(p)
	}
	close(stop)
	wg.Wait()
	if _, ok := p.Status("u3"); !ok {
		t.Fatal("u3 应在池内")
	}
}

// TestAuthWatcherIgnoresNonAuthFiles 只认 auth.FilePattern（workbuddy*.json）：
// 目录里的其它文件（备份、.tmp、读写不相关的 json）不得被当成账号加载。
func TestAuthWatcherIgnoresNonAuthFiles(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	for name, body := range map[string]string{
		"notes.txt":          "hello",
		"workbuddy-u1.json":  `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`,
		"backup-u9.json":     `{"auth":{"accessToken":"at9","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u9"}}`,
		"state.json":         `{"accounts":{}}`,
		"workbuddy-u2.json~": `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u2"}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	added, _ := w.scan(p)
	if len(added) != 1 || added[0] != "u1" {
		t.Fatalf("只应加载 workbuddy*.json: added=%v", added)
	}
	if _, ok := p.Status("u9"); ok {
		t.Error("backup-u9.json 不应被加载")
	}
	if _, ok := p.Status("u2"); ok {
		t.Error("workbuddy-u2.json~（编辑器临时文件）不应被加载")
	}
}

// TestAuthWatcherSaveAtomicTmpFileIgnored 覆盖「半截 JSON」之外的另一常见中间态：
// SaveAtomic 的 tmp 文件（workbuddy-x.json.tmp）。它不匹配 workbuddy*.json 模式，
// 天然不进扫描范围——本用例把它钉死，防止将来改模式时把写入中间态读进池。
func TestAuthWatcherSaveAtomicTmpFileIgnored(t *testing.T) {
	dir := t.TempDir()
	p := pool.New("")
	w := newAuthWatcher(dir)

	fp := writeAuthFile(t, dir, "u1")
	if err := os.WriteFile(fp+".tmp", []byte(`{"auth":{"accessToken":"half`), 0o600); err != nil {
		t.Fatal(err)
	}
	added, _ := w.scan(p)
	if len(added) != 1 || added[0] != "u1" {
		t.Fatalf("tmp 文件不应参与扫描: added=%v", added)
	}
	// tmp 移除后也照常工作（不影响正常文件）。
	if err := os.Remove(fp + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if added, removed := w.scan(p); len(added) != 0 || len(removed) != 0 {
		t.Fatalf("无变化轮应为空: added=%v removed=%v", added, removed)
	}
}

// TestAuthWatcherDirMissingIsQuiet 目录不存在（用户还没建 auths/）时不报错、不打 WARN，
// 也不影响后续创建目录后被正常发现。
func TestAuthWatcherDirMissingIsQuiet(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "auths") // 尚不存在
	p := pool.New("")
	w := newAuthWatcher(dir)

	buf := captureLog(t)
	if added, removed := w.scan(p); len(added) != 0 || len(removed) != 0 {
		t.Fatalf("目录不存在应为空变化: added=%v removed=%v", added, removed)
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("目录不存在不应打 WARN（首次部署的常态），实际=%q", buf.String())
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAuthFile(t, dir, "u6")
	if added, _ := w.scan(p); len(added) != 1 || added[0] != "u6" {
		t.Fatalf("建目录后应发现新账号: %v", added)
	}
}

// TestAuthWatchLogsStartupInterval 启动日志带间隔与目录（运维对账：日志里能看出
// 「热加载开着、每 30 秒扫一次 auths/」）。
func TestAuthWatchLogsStartupInterval(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{Pool: pool.New("")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf := captureLog(t)
	s.StartAuthWatch(ctx, dir, time.Minute)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(buf.String(), "auth watch: 每") {
		if time.Now().After(deadline) {
			t.Fatalf("应打一条启动日志，实际=%q", buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), dir) {
		t.Errorf("启动日志应含目录路径 %s，实际=%q", dir, buf.String())
	}
}
