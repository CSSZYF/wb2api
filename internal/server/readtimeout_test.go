package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// timeoutErr 模拟 net.Error 超时错误（如 "read tcp ...: i/o timeout"）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "read tcp 10.42.0.245:7863->10.42.0.1:35543: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// nonTimeoutNetErr 是 net.Error 但 Timeout()=false（如 connection reset）。
// 文案分支必须按 Timeout() 判定，不能只判"是不是 net.Error"。
type nonTimeoutNetErr struct{}

func (nonTimeoutNetErr) Error() string   { return "read tcp: connection reset by peer" }
func (nonTimeoutNetErr) Timeout() bool   { return false }
func (nonTimeoutNetErr) Temporary() bool { return false }

// errReader 读 body 时返回指定错误的 ReadCloser。
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
func (r errReader) Close() error             { return nil }

// readDeadlineRecorder 记录 handler 为本请求设置的读截止（ResponseController
// 走 interface{ SetReadDeadline(time.Time) error } 断言，故实现该方法即可被捕获）。
type readDeadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *readDeadlineRecorder) SetReadDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlines = append(r.deadlines, t)
	return nil
}

func (r *readDeadlineRecorder) recorded() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

// newReadDeadlineRecorder 构造可观测读截止的 recorder。
func newReadDeadlineRecorder() *readDeadlineRecorder {
	return &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

// TestReadBodyErrMsgTimeoutHint 读超时文案必须带可操作提示（键名 + 生效值），
// 否则用户只看到 "i/o timeout"——不知道是哪一端超时、也不知道能调哪个键。
func TestReadBodyErrMsgTimeoutHint(t *testing.T) {
	msg := readBodyErrMsg(timeoutErr{}, 300*time.Second)
	if !strings.Contains(msg, "read body: ") {
		t.Errorf("文案必须保留原始错误前缀: %q", msg)
	}
	if !strings.Contains(msg, "read_timeout_seconds") {
		t.Errorf("超时文案必须指向 server.read_timeout_seconds: %q", msg)
	}
	if !strings.Contains(msg, "300") {
		t.Errorf("超时文案必须带上当前生效值（300s）: %q", msg)
	}
	// 生效值要跟着配置走（面板调大后文案也应反映新值）。
	if got := readBodyErrMsg(timeoutErr{}, 900*time.Second); !strings.Contains(got, "900") {
		t.Errorf("文案应反映当前生效值 900s: %q", got)
	}
}

// TestReadBodyErrMsgTimeoutWrapped 包装过的超时错误同样要识别（errors.As 穿透 %w）。
// 上游/标准库的读错误常被 fmt.Errorf 包装后再传出。
func TestReadBodyErrMsgTimeoutWrapped(t *testing.T) {
	wrapped := fmt.Errorf("read body: %w", timeoutErr{})
	msg := readBodyErrMsg(wrapped, 300*time.Second)
	if !strings.Contains(msg, "read_timeout_seconds") {
		t.Errorf("包装的超时错误应带提示: %q", msg)
	}
}

// TestReadBodyErrMsgNonTimeoutNoHint 非超时错误**不得**带提示：普通读失败
// （客户端提前断开、body 截断）与 read_timeout_seconds 无关，加上去是误导。
// 覆盖三种非超时输入：普通 error、net.Error 但 Timeout()=false、io.ErrUnexpectedEOF。
func TestReadBodyErrMsgNonTimeoutNoHint(t *testing.T) {
	for name, err := range map[string]error{
		"plain":          errors.New("connection reset by peer"),
		"net-nontimeout": nonTimeoutNetErr{},
		"unexpected-eof": io.ErrUnexpectedEOF,
	} {
		msg := readBodyErrMsg(err, 300*time.Second)
		if want := "read body: " + err.Error(); msg != want {
			t.Errorf("%s: 非超时错误文案必须原样保留，got %q want %q", name, msg, want)
		}
		if strings.Contains(msg, "read_timeout_seconds") {
			t.Errorf("%s: 非超时错误不得带超时提示: %q", name, msg)
		}
	}
}

// TestChatReadBodyTimeoutHintInResponse 端到端：读 body 超时 → 400 invalid_request
// 且响应文案带提示（用户实际看到的错误体，即事故现场那条）。
func TestChatReadBodyTimeoutHintInResponse(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		ReadTimeout: 300 * time.Second})
	req := httptest.NewRequest("POST", "/v1/chat/completions", errReader{err: timeoutErr{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "invalid_request") {
		t.Errorf("错误码应保持 invalid_request: %s", body)
	}
	if !strings.Contains(body, "read_timeout_seconds") {
		t.Errorf("读超时响应应带 read_timeout_seconds 提示: %s", body)
	}
}

// TestChatReadBodyNonTimeoutNoHintInResponse 端到端：普通读错误 → 400，且**不含**
// 超时提示（错误码与文案均保持旧行为，零回归）。
func TestChatReadBodyNonTimeoutNoHintInResponse(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})})
	req := httptest.NewRequest("POST", "/v1/chat/completions", errReader{err: errors.New("connection reset by peer")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "read body: connection reset by peer") {
		t.Errorf("非超时错误文案应原样透出: %s", body)
	}
	if strings.Contains(body, "read_timeout_seconds") {
		t.Errorf("非超时错误不得带超时提示: %s", body)
	}
}

// TestReadTimeoutValueFallback 运行期读窗口的兜底口径：NewHandler 注入值生效，
// <=0 回落 DefaultReadTimeout（不采纳 http.Server 的「0 = 不限」语义）。
func TestReadTimeoutValueFallback(t *testing.T) {
	// 注入值生效。
	h := NewHandler(Config{ReadTimeout: 42 * time.Second})
	if got := h.readTimeoutValue(); got != 42*time.Second {
		t.Errorf("readTimeoutValue=%v want 42s", got)
	}
	// 未注入（<=0）→ 兜底 300s，而非 0（0 会被 ResponseController 解释为"清零截止"，
	// 等于拆掉慢速 body 闸门）。
	h = NewHandler(Config{})
	if got := h.readTimeoutValue(); got != DefaultReadTimeout {
		t.Errorf("未注入时 readTimeoutValue=%v want %v", got, DefaultReadTimeout)
	}
	if DefaultReadTimeout != 300*time.Second {
		t.Errorf("DefaultReadTimeout=%v want 300s（与 config 侧默认同口径）", DefaultReadTimeout)
	}
	// SetReadTimeout 热改 + 非正值回落。
	h.SetReadTimeout(900 * time.Second)
	if got := h.readTimeoutValue(); got != 900*time.Second {
		t.Errorf("热改后 readTimeoutValue=%v want 900s", got)
	}
	for _, v := range []time.Duration{0, -1} {
		h.SetReadTimeout(v)
		if got := h.readTimeoutValue(); got != DefaultReadTimeout {
			t.Errorf("SetReadTimeout(%v) 应回落 %v，got %v", v, DefaultReadTimeout, got)
		}
	}
	// 零值 Handler（未经 NewHandler 装配）也不得退化成"不限"。
	var bare Handler
	if got := bare.readTimeoutValue(); got != DefaultReadTimeout {
		t.Errorf("零值 Handler readTimeoutValue=%v want %v", got, DefaultReadTimeout)
	}
}

// TestArmBodyReadDeadlineUsesCurrentValue 逐请求读截止必须按**当前**配置值设置，
// 且热改后下一个请求即生效——这是 read_timeout_seconds 能"面板保存即时生效"的
// 全部依据（http.Server.ReadTimeout 是启动期静态字段，改不了）。
func TestArmBodyReadDeadlineUsesCurrentValue(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		ReadTimeout: 900 * time.Second})

	// 一次请求 → 记录到的读截止 ≈ now+900s（留足容差避免慢机器 flake）。
	rec := newReadDeadlineRecorder()
	body := strings.NewReader(`{"model":"glm-5.2","messages":[]}`)
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", body))
	ds := rec.recorded()
	if len(ds) != 1 {
		t.Fatalf("应有 1 次 SetReadDeadline 调用，got %d: %v", len(ds), ds)
	}
	if d := time.Until(ds[0]); d < 880*time.Second || d > 910*time.Second {
		t.Errorf("读截止距now=%v want≈900s", d)
	}

	// 面板热改到 120s：不重启、不重建 handler，下一个请求即按新值。
	h.SetReadTimeout(120 * time.Second)
	rec = newReadDeadlineRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	ds = rec.recorded()
	if len(ds) != 1 {
		t.Fatalf("热改后应有 1 次 SetReadDeadline 调用，got %d: %v", len(ds), ds)
	}
	if d := time.Until(ds[0]); d < 100*time.Second || d > 130*time.Second {
		t.Errorf("热改后读截止距now=%v want≈120s（热应用未接线？）", d)
	}
}

// TestArmBodyReadDeadlineSkipsBodyless 无 body 的请求不得设读截止：
// 标准库已在 handler 入口前清零（startBackgroundRead），此刻再设只会把
// keep-alive 空闲等待拉长到 read_timeout（与 IdleTimeout 职责重叠）。
func TestArmBodyReadDeadlineSkipsBodyless(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		ReadTimeout: 300 * time.Second})
	// GET /healthz（无 body）。
	rec := newReadDeadlineRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if ds := rec.recorded(); len(ds) != 0 {
		t.Errorf("无 body 请求不应设读截止，got %v", ds)
	}
	// 显式 Content-Length: 0。
	rec = newReadDeadlineRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", http.NoBody)
	h.ServeHTTP(rec, req)
	if ds := rec.recorded(); len(ds) != 0 {
		t.Errorf("Content-Length=0 请求不应设读截止，got %v", ds)
	}
}

// TestArmBodyReadDeadlineUnsupportedWriter 不支持 SetReadDeadline 的 ResponseWriter
// （如测试用 ResponseRecorder、自定义包装层）必须静默降级：请求照常处理，不 panic、
// 不因热改能力缺失而拒服务（退回 http.Server.ReadTimeout 静态兜底）。
func TestArmBodyReadDeadlineUnsupportedWriter(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		ReadTimeout: 300 * time.Second})
	rec := httptest.NewRecorder() // 不支持 SetReadDeadline
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Errorf("不支持 SetReadDeadline 时应正常处理，code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestSlowUploadSurvivesStaticReadTimeout 端到端（真实 HTTP 连接）：上传耗时超过
// http.Server.ReadTimeout 的静态值时，请求仍能成功——因为 handler 在入口按
// **配置值**重设了本请求的读截止（armBodyReadDeadline），后设的 deadline 覆盖
// 标准库在 readRequest 里按静态 ReadTimeout 设的那一份。
//
// 这正是本次生产事故的修复语义，也是"热改能生效"的根据：http.Server.ReadTimeout
// 只能在启动时写死且无并发安全 setter，故新值必须由 handler 逐请求施加。
// 反过来说，若有人把 armBodyReadDeadline 删了/改成不生效，本用例立刻失败。
//
// 参数刻意取小（静态 1s / 配置 10s / 上传约 2.5s）：既不慢到拖累测试，又明确跨越
// 静态值——旧行为下这个请求必然被掐断（i/o timeout），新行为下必须 200。
func TestSlowUploadSurvivesStaticReadTimeout(t *testing.T) {
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		ReadTimeout: 10 * time.Second, // 面板/配置值：宽于静态值
	})
	srv := httptest.NewUnstartedServer(h)
	// 模拟旧版写死的窄窗口（生产上是 60s；这里压到 1s 以便快速验证语义）。
	srv.Config.ReadTimeout = 1 * time.Second
	srv.Config.ReadHeaderTimeout = 1 * time.Second
	srv.Start()
	defer srv.Close()

	// 慢速 body：分片写入，总耗时约 2.5s（> 静态 1s，< 配置 10s）。
	pr, pw := io.Pipe()
	go func() {
		chunks := []string{
			`{"model":"glm-5.2","messages":[{"role":"user","content":"`,
			strings.Repeat("x", 200),
			`"}]}`,
		}
		for i, c := range chunks {
			if i > 0 {
				time.Sleep(1200 * time.Millisecond)
			}
			if _, err := pw.Write([]byte(c)); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	// 不用 Content-Length（chunked）：慢速分片上传，正是跨境慢链路的形态。
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("慢速上传应成功（handler 重设读截止后不应被静态 ReadTimeout 掐断）: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("慢速上传 status=%d want 200 body=%s", resp.StatusCode, body)
	}
}

// TestSlowUploadTimesOutWithConfiguredHint 端到端：配置值本身很窄时，慢速上传仍会被
// 按**配置值**掐断（不是被静态值），且响应文案带可操作提示。
//
// 语义双向锁定：armBodyReadDeadline 既能放宽也能收紧，且收紧时用户的错误信息里有
// 明确的键名与生效值可查——这是事故复盘里最缺的一环（旧文案只有 i/o timeout）。
func TestSlowUploadTimesOutWithConfiguredHint(t *testing.T) {
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		ReadTimeout: 500 * time.Millisecond, // 配置值刻意很窄 → 慢上传必被掐断
	})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = 30 * time.Second // 静态值很宽：证明是"配置值"在管，而非静态值
	srv.Config.ReadHeaderTimeout = 5 * time.Second
	srv.Start()
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"`)); err != nil {
			return
		}
		time.Sleep(3 * time.Second) // 超过配置的 500ms
		_, _ = pw.Write([]byte(`x"}]}`))
		_ = pw.Close()
	}()

	req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		// 服务端掐断后客户端可能直接拿到传输层错误（连接被关），这同样是"被掐断"。
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 {
		t.Fatalf("配置 500ms 下 3s 慢上传不应成功: %s", body)
	}
	if !strings.Contains(string(body), "read_timeout_seconds") {
		t.Errorf("被配置值掐断的响应应带 read_timeout_seconds 提示: %s", body)
	}
}
