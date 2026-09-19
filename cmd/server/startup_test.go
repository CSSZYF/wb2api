// startup_test.go 启动握手（先 bind 后打 listening）+ 端口占用诊断的回归测试。
//
// 背景：旧实现先打 "listening on ..." 再 srv.ListenAndServe()，端口被占时日志自相矛盾
// （先宣称正在监听、紧接着 Fatal 退出），且只有裸 Go 错误串。本文件把「先 bind 成功、
// 才宣告 listening」这一顺序，以及「占用时给可读排查建议」钉死在测试里。
//
// 注：本文件所有用例都不并行——log.SetOutput 是进程级全局（与本仓既有约定一致）。
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// startupLogSink 并发安全的日志接收器：Serve 在后台 goroutine 里可能继续打日志，
// 与测试 goroutine 的读断言同时发生，裸 strings.Builder 会构成数据竞争。
type startupLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *startupLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *startupLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureStartupLog 捕获 log 输出（进程级全局，调用方不得并行；结束自动还原）。
func captureStartupLog(t *testing.T) *startupLogSink {
	t.Helper()
	sink := &startupLogSink{}
	old := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(old) })
	return sink
}

// occupyPort 在 127.0.0.1 上占住一个空闲端口，返回该地址与释放函数。
// 用真实监听（而非猜测端口号）保证「被占用」是确定性的，不依赖固定端口是否空闲。
func occupyPort(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestStartupListenOccupiedPortReadableError 端口被占用时：启动失败、日志给可读诊断，
// 且**不得**出现 "listening on"（这是旧实现自相矛盾日志的回归点）。
func TestStartupListenOccupiedPortReadableError(t *testing.T) {
	addr, release := occupyPort(t)
	defer release()

	cfg := Default()
	cfg.Listen = addr // 精确指向已占用地址，bind 必然失败

	sink := captureStartupLog(t)
	ln, err := startupListen(cfg)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("端口被占用时 startupListen 应返回错误（addr=%s）", addr)
	}
	if ln != nil {
		_ = ln.Close()
		t.Errorf("bind 失败时不应返回可用 listener: %v", ln)
	}

	got := sink.String()
	// 核心回归：失败路径绝不能先宣告 listening。
	if strings.Contains(got, "listening on") {
		t.Errorf("bind 失败却打了 listening 日志（自相矛盾）:\n%s", got)
	}
	// 结论可读：明确「被占用」而非只有裸 Go 错误串。
	if !strings.Contains(got, "已被其它程序占用") {
		t.Errorf("占用诊断缺失「已被其它程序占用」结论:\n%s", got)
	}
	// 排查建议可执行：给出 netstat 命令（含端口号）。
	port := listenPort(addr)
	if port == "" {
		t.Fatalf("listenPort(%q) 应解析出端口", addr)
	}
	if !strings.Contains(got, "netstat -ano | findstr :"+port) {
		t.Errorf("占用诊断缺失 netstat 排查命令（含端口 %s）:\n%s", port, got)
	}
	// 替代端口建议：给出可照抄的下一端口。
	if !strings.Contains(got, ":"+nextPort(port)) {
		t.Errorf("占用诊断缺失换端口建议（:%s）:\n%s", nextPort(port), got)
	}
	// 原始错误保留为末行补充（便于搜索/上报），但不再是唯一信息。
	if !strings.Contains(got, "原始错误") {
		t.Errorf("诊断应保留原始错误串作补充:\n%s", got)
	}
	// 旧实现的前缀 "http: " 是裸错误串的标志；新诊断不应再以它作为唯一提示。
	if strings.Contains(got, "http: listen tcp") {
		t.Errorf("诊断不应回落到旧的裸错误串形态:\n%s", got)
	}
}

// TestStartupListenSuccessLogsAfterBind 正常路径：bind 成功后才有 listening 日志，
// 且返回的 listener 真能服务 HTTP（证明日志与「确实在监听」一致）。
func TestStartupListenSuccessLogsAfterBind(t *testing.T) {
	addr, release := occupyPort(t)
	release() // 立刻释放：拿到一个「刚空闲」的端口号，随后由 startupListen 真正绑定

	cfg := Default()
	cfg.Listen = addr
	cfg.APIKey = "k"
	cfg.Server.ReadTimeoutSeconds = 300
	cfg.Server.MaxBodyMB = 32

	sink := captureStartupLog(t)
	ln, err := startupListen(cfg)
	if err != nil {
		t.Fatalf("空闲端口 startupListen 应成功（addr=%s）: %v", addr, err)
	}
	defer ln.Close()

	got := sink.String()
	if !strings.Contains(got, "listening on "+addr) {
		t.Errorf("成功路径应打 listening 日志（含 %s）:\n%s", addr, got)
	}
	// 与主程序一致：入站读窗口一行也只在 bind 成功后出现。
	if !strings.Contains(got, "入站请求体读取窗口") {
		t.Errorf("成功路径应打入站读窗口日志:\n%s", got)
	}
	// listener 确实在监听：起一个真实 http.Server 并完成一次请求。
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("绑定后的 listener 应能服务 HTTP: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
}

// TestStartupListenServeShutdownIsGraceful 优雅停机兼容性：srv.Serve(ln) 与旧路径
// srv.ListenAndServe() 的错误语义必须一致——Shutdown 触发后 Serve 返回
// http.ErrServerClosed（main 里 `err != http.ErrServerClosed` 的判断据此成立），
// 且在途请求被放行完成（Shutdown 语义未被 Serve 改动破坏）。
func TestStartupListenServeShutdownIsGraceful(t *testing.T) {
	addr, release := occupyPort(t)
	release()

	cfg := Default()
	cfg.Listen = addr
	cfg.Server.ReadTimeoutSeconds = 300

	// 捕获日志：本用例只关心 Serve/Shutdown 语义，不让启动日志污染测试输出。
	captureStartupLog(t)
	ln, err := startupListen(cfg)
	if err != nil {
		t.Fatalf("startupListen: %v", err)
	}

	started := make(chan struct{})
	releaseReq := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-releaseReq // 挂在在途状态，验证 Shutdown 会等它完成
		_, _ = w.Write([]byte("done"))
	})}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		c := &http.Client{Timeout: 10 * time.Second}
		resp, err := c.Get("http://" + addr + "/slow")
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	select {
	case <-started:
	case err := <-errCh:
		t.Fatalf("在途请求未建立: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未在 5s 内到达 handler")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown(shutdownCtx) }()

	// Shutdown 应等在途请求：先确认它没有提前返回，再放行请求。
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown 未等在途请求即返回: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseReq)

	select {
	case resp := <-respCh:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("在途请求 status=%d want 200（优雅停机不应掐断在途请求）", resp.StatusCode)
		}
	case err := <-errCh:
		t.Fatalf("优雅停机掐断了在途请求: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未在优雅停机窗口内完成")
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown err=%v want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 未返回")
	}

	select {
	case err := <-serveErr:
		// 与 main 的判断一致：正常关闭必须是 ErrServerClosed。
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve 在 Shutdown 后返回 %v，want http.ErrServerClosed（main 的退出判断依赖此语义）", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 后 Serve 未返回")
	}
}

// TestIsAddrInUse 分类器：真实占用错误 → true；非占用类错误 → false。
func TestIsAddrInUse(t *testing.T) {
	addr, release := occupyPort(t)
	defer release()

	_, bindErr := net.Listen("tcp", addr)
	if bindErr == nil {
		t.Fatal("重复 bind 应失败")
	}
	if !isAddrInUse(bindErr) {
		t.Errorf("isAddrInUse(%v)=false want true（本平台 errno=%v）", bindErr, bindErr)
	}
	// 非占用类：非法端口。
	_, badErr := net.Listen("tcp", "127.0.0.1:99999")
	if badErr == nil {
		t.Fatal("非法端口应失败")
	}
	if isAddrInUse(badErr) {
		t.Errorf("isAddrInUse(%v)=true want false（非法端口不是占用）", badErr)
	}
}

// TestBindFailLinesNonInUseHint 非占用类失败（如 listen 写法有误）走另一套提示：
// 指向配置写法，而不是误导用户去 netstat 查占用。
func TestBindFailLinesNonInUseHint(t *testing.T) {
	_, badErr := net.Listen("tcp", "127.0.0.1:99999")
	if badErr == nil {
		t.Fatal("非法端口应失败")
	}
	lines := bindFailLines("127.0.0.1:99999", badErr)
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "已被其它程序占用") {
		t.Errorf("非占用类失败不应误报「端口被占用」:\n%s", got)
	}
	if !strings.Contains(got, "监听地址不可用") {
		t.Errorf("非占用类失败应指向监听地址/配置写法:\n%s", got)
	}
	if !strings.Contains(got, "listen") {
		t.Errorf("非占用类失败应提示检查 config.json 的 listen:\n%s", got)
	}
}

// TestListenAddrEmptyFallsBackToHTTP 空地址回落必须与标准库一致：net/http 的
// ListenAndServe 在 s.Addr == "" 时改用 ":http"。本用例把该等价性钉住——「把
// ListenAndServe 拆成 net.Listen + Serve 两步」不得引入行为差异。
func TestListenAddrEmptyFallsBackToHTTP(t *testing.T) {
	if got := listenAddr(""); got != ":http" {
		t.Errorf("listenAddr(%q)=%q want \":http\"（与 net/http ListenAndServe 的回落一致）", "", got)
	}
	for _, in := range []string{":7863", "127.0.0.1:7863", "0.0.0.0:7863"} {
		if got := listenAddr(in); got != in {
			t.Errorf("listenAddr(%q)=%q want 原样返回", in, got)
		}
	}
}

// TestListenPort 端口提取：常见 listen 形态均可解析，异常输入回落空串。
func TestListenPort(t *testing.T) {
	cases := map[string]string{
		":7863":          "7863",
		"0.0.0.0:7863":   "7863",
		"127.0.0.1:7863": "7863",
		"7863":           "7863",
		"[::]:7863":      "7863",
		"":               "",
		":abc":           "",
		":":              "",
	}
	for in, want := range cases {
		if got := listenPort(in); got != want {
			t.Errorf("listenPort(%q)=%q want %q", in, got, want)
		}
	}
}

// TestNextPort 替代端口：可解析则 +1（含边界不越界），否则回落 7864。
func TestNextPort(t *testing.T) {
	cases := map[string]string{
		"7863":  "7864",
		"65534": "65535",
		"65535": "7864", // 越界不给出非法端口
		"0":     "7864",
		"":      "7864",
		"abc":   "7864",
		"-1":    "7864",
	}
	for in, want := range cases {
		if got := nextPort(in); got != want {
			t.Errorf("nextPort(%q)=%q want %q", in, got, want)
		}
	}
}
