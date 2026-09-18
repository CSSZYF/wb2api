package upstream

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNewDialerParams DialContext 拨号参数回读断言（Transport 只存闭包，参数断言
// 落在 newDialer 上——集中定义的另一头）。半死连接快速失败靠这两个值：Timeout 让
// 建连期不干等系统 TCP 重传窗口，KeepAlive 让复用期的死连接 15~30s 内被内核掐掉。
// timeout <=0 回落缺省（配置未接线 / 裸用路径）。
func TestNewDialerParams(t *testing.T) {
	d := newDialer(0)
	if d.Timeout != dialTimeout {
		t.Errorf("dialer.Timeout=%v want %v（<=0 回落缺省）", d.Timeout, dialTimeout)
	}
	if d.KeepAlive != dialKeepAlive {
		t.Errorf("dialer.KeepAlive=%v want %v", d.KeepAlive, dialKeepAlive)
	}
	if d.Timeout != 30*time.Second || d.KeepAlive != 15*time.Second {
		t.Errorf("dial params=(%v, %v) want (30s, 15s)——30s 为 2026-09-18 国内网络实测放宽", d.Timeout, d.KeepAlive)
	}
	// 显式值生效（配置注入路径）。
	if d2 := newDialer(7 * time.Second); d2.Timeout != 7*time.Second {
		t.Errorf("dialer.Timeout=%v want 7s（显式配置应生效）", d2.Timeout)
	}
}

// TestNewTransportHardening 连接层配置断言（transport.go 集中参数的回读验证）：
// h2 默认启用 / TLS 握手超时 / 空闲池 90s，且 ResponseHeaderTimeout 保持 config 驱动的
// 缺省值不变。挂在 New() 的成品 Transport 上（而非 newTransport() 裸返回）——同一
// 对象同时被 HTTP 与 ChatHTTP 持有，任何字段断言都直接对应生产出站行为。
func TestNewTransportHardening(t *testing.T) {
	tr, ok := New().ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", New().ChatHTTP.Transport)
	}
	// 1. h2 默认启用（2026-09-18 反转）：TLSNextProto 必须为 nil（标准库据此
	//    注入默认 h2 映射，ALPN 正常协商 h2），且 ForceAttemptHTTP2=true 覆盖
	//    「自定义 DialContext 保守禁 h2」的标准库默认——否则本 Transport 静默
	//    退回 HTTP/1.1（实测：不设 ForceAttemptHTTP2 → HTTP/1.1）。
	if tr.TLSNextProto != nil {
		t.Errorf("TLSNextProto must be nil to enable HTTP/2 by default, got %d entries", len(tr.TLSNextProto))
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be true when h2 enabled: custom DialContext conservatively disables h2 otherwise")
	}
	// 2. TLS 握手超时（此前完全缺失：握手挂起只能干等到 HTTP.Client.Timeout）。
	if tr.TLSHandshakeTimeout != 30*time.Second {
		t.Errorf("TLSHandshakeTimeout=%v want 30s（国内网络握手实测 >10s，10s 会误杀）", tr.TLSHandshakeTimeout)
	}
	// 3. 空闲连接池：90s（30s 太激进——连接刚建好就可能已过期，h2 下尤其亏）。
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout=%v want 90s", tr.IdleConnTimeout)
	}
	if tr.MaxIdleConns != 100 || tr.MaxIdleConnsPerHost != 20 {
		t.Errorf("pool sizes=(%d, %d) want (100, 20)", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	// 4. ResponseHeaderTimeout 保持 config 驱动：构造默认仍是 120s（cmd/server/main.go
	//    在 New() 之后用 cfg.Upstream.HeaderTimeoutSeconds 无条件覆盖本字段，硬编码
	//    收紧会在接线后失效——故此处只锁「默认未被改动」）。
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 120s（config 驱动的构造默认，main.go 会覆盖）", tr.ResponseHeaderTimeout)
	}
	// 5. DisableKeepAlives 必须保持 false：与连接复用意图相反，不吸收。
	if tr.DisableKeepAlives {
		t.Error("DisableKeepAlives must stay false (keep-alive reuse is intentional)")
	}
}

// TestNewTransportDisableH2 disableH2=true 的禁 h2 路径逐字保留（逃生门）：
// 空 TLSNextProto（非 nil 且不含 h2）让 ALPN 完成后无 h2 协议可用，连接退回
// HTTP/1.1——这是唯一正确的禁法（设 ForceAttemptHTTP2=false 对默认 TLS 无效，
// 见 transport.go 注释）。同时确认 ForceAttemptHTTP2 保持 false（空映射已足够，
// 无需再设；设了也只是无害冗余，但保持最小配置面）。
func TestNewTransportDisableH2(t *testing.T) {
	tr := newTransport(TransportOpts{DisableHTTP2: true})
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil empty map to disable HTTP/2 (nil = stdlib re-enables h2)")
	}
	if _, registered := tr.TLSNextProto["h2"]; registered {
		t.Error("TLSNextProto must not register h2")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto must be empty, got %d entries", len(tr.TLSNextProto))
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must stay false on the disable-h2 path（空映射已足够）")
	}
	// 禁 h2 不影响其余连接层参数。
	if tr.TLSHandshakeTimeout != 30*time.Second || tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("timeouts=(%v, %v) want (30s, 90s)", tr.TLSHandshakeTimeout, tr.IdleConnTimeout)
	}
}

// TestNewTransportOptsTimeouts 连接层超时从配置生效（回读断言）：显式值透传到
// Transport 与 Dialer；<=0 回落缺省（30/30/90）。
func TestNewTransportOptsTimeouts(t *testing.T) {
	// 显式值生效。
	tr := newTransport(TransportOpts{
		TLSHandshakeTimeout: 11 * time.Second,
		DialTimeout:         12 * time.Second,
		IdleConnTimeout:     13 * time.Second,
	})
	if tr.TLSHandshakeTimeout != 11*time.Second {
		t.Errorf("TLSHandshakeTimeout=%v want 11s（配置生效）", tr.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != 13*time.Second {
		t.Errorf("IdleConnTimeout=%v want 13s（配置生效）", tr.IdleConnTimeout)
	}
	if tr.TLSNextProto != nil || !tr.ForceAttemptHTTP2 {
		t.Error("未指定 DisableHTTP2 时 h2 应启用（零值 = 生产默认）")
	}

	// <=0 回落缺省（0 与负数同待遇）。
	for _, tc := range []struct {
		name string
		opts TransportOpts
	}{
		{"zero", TransportOpts{}},
		{"negative", TransportOpts{TLSHandshakeTimeout: -1, DialTimeout: -1, IdleConnTimeout: -1}},
	} {
		got := newTransport(tc.opts)
		if got.TLSHandshakeTimeout != 30*time.Second || got.IdleConnTimeout != 90*time.Second {
			t.Errorf("%s: timeouts=(%v, %v) want (30s, 90s) 缺省回落", tc.name, got.TLSHandshakeTimeout, got.IdleConnTimeout)
		}
		// Dialer 的 Timeout 只存闭包，参数断言走 newDialer（同 TestNewDialerParams）。
		if d := newDialer(tc.opts.DialTimeout); d.Timeout != 30*time.Second {
			t.Errorf("%s: dialer.Timeout=%v want 30s 缺省回落", tc.name, d.Timeout)
		}
	}
}

// TestConfigureTransportReplacesSharedTransport ConfigureTransport 重建后：
//   - HTTP 与 ChatHTTP **仍共享同一** Transport 实例（只换一个会让两个 client 各持
//     一份连接池，复用率腰斩——这是本方法存在的理由）；
//   - 新实例的四个可配字段按 opts 生效；
//   - 旧 Transport 的 ResponseHeaderTimeout（main 侧覆盖的 config 驱动值）被沿用
//     ——生产顺序是「先重建、后覆盖」，此层兜底覆盖的是反序与重复调用（幂等）。
func TestConfigureTransportReplacesSharedTransport(t *testing.T) {
	c := New()
	old, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.HTTP.Transport)
	}
	// 模拟反序：先按 config 覆盖 ResponseHeaderTimeout，再重建（沿用兜底路径）。
	old.ResponseHeaderTimeout = 77 * time.Second

	c.ConfigureTransport(TransportOpts{
		DisableHTTP2:        true,
		TLSHandshakeTimeout: 21 * time.Second,
		DialTimeout:         22 * time.Second,
		IdleConnTimeout:     23 * time.Second,
	})

	if c.HTTP.Transport != c.ChatHTTP.Transport {
		t.Fatal("HTTP 与 ChatHTTP 必须共享同一 Transport（重建时两者同步替换）")
	}
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.HTTP.Transport)
	}
	if tr == old {
		t.Fatal("ConfigureTransport 应构造新 Transport 实例")
	}
	if tr.TLSNextProto == nil {
		t.Error("DisableHTTP2=true 时新 Transport 应带空 TLSNextProto")
	}
	if tr.TLSHandshakeTimeout != 21*time.Second || tr.IdleConnTimeout != 23*time.Second {
		t.Errorf("timeouts=(%v, %v) want (21s, 23s)", tr.TLSHandshakeTimeout, tr.IdleConnTimeout)
	}
	if d := newDialer(22 * time.Second); d.Timeout != 22*time.Second {
		t.Errorf("dialer.Timeout=%v want 22s", d.Timeout)
	}
	if tr.ResponseHeaderTimeout != 77*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 77s（从旧 Transport 沿用，防被重建丢弃）", tr.ResponseHeaderTimeout)
	}

	// 反向：再按「启用 h2」重建，TLSNextProto 回 nil（配置可在两次启动间来回切）。
	c.ConfigureTransport(TransportOpts{})
	tr2, _ := c.HTTP.Transport.(*http.Transport)
	if tr2.TLSNextProto != nil || !tr2.ForceAttemptHTTP2 {
		t.Error("DisableHTTP2=false 重建后应回到 h2 启用（TLSNextProto=nil + ForceAttemptHTTP2=true）")
	}
	if c.HTTP.Transport != c.ChatHTTP.Transport {
		t.Error("第二次重建后两 client 仍须共享 Transport")
	}
}

// TestTransportOptsZeroValueEnablesH2 TransportOpts 零值语义 = 生产默认（h2 启用 +
// 30/30/90）。这条锁住「默认必须启用 h2」：任何把 DisableHTTP2 改成
// EnableHTTP2 语义（零值 = 关 h2）的改动都会在这里失败。
func TestTransportOptsZeroValueEnablesH2(t *testing.T) {
	var zero TransportOpts
	tr := newTransport(zero)
	if tr.TLSNextProto != nil {
		t.Error("零值 TransportOpts 必须启用 h2（TLSNextProto=nil）")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("零值 TransportOpts 必须启用 h2（ForceAttemptHTTP2=true）")
	}
	if tr.TLSHandshakeTimeout != 30*time.Second {
		t.Errorf("TLSHandshakeTimeout=%v want 30s", tr.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout=%v want 90s", tr.IdleConnTimeout)
	}
	if d := newDialer(zero.DialTimeout); d.Timeout != 30*time.Second {
		t.Errorf("dialer.Timeout=%v want 30s", d.Timeout)
	}
}

// TestTransportNegotiatesH2OverTLS 端到端确认「默认配置真能协商出 h2」——
// 这是本任务的核心行为，也是 ForceAttemptHTTP2 陷阱的实测回归位：httptest 起
// h2 服务端，用 newTransport(TransportOpts{}) 请求，响应协议必须是 HTTP/2.0；
// 反之 DisableHTTP2=true 时必须回落 HTTP/1.1（逃生门有效）。
func TestTransportNegotiatesH2OverTLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	probe := func(name string, opts TransportOpts) string {
		t.Helper()
		tr := newTransport(opts)
		// 测试服务器用自签证书：仅测试期信任，不触碰生产 TLSClientConfig
		// （生产该字段为 nil，由标准库按 ALPN 协商——此处设了会让
		// protocols() 走「有 TLSClientConfig」分支，但 ForceAttemptHTTP2=true
		// 已覆盖该分支，正是要验的路径）。
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		cli := &http.Client{Transport: tr, Timeout: 10 * time.Second}
		resp, err := cli.Get(srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.Proto
	}

	if got := probe("h2-enabled", TransportOpts{}); got != "HTTP/2.0" {
		t.Errorf("默认配置协商出 %s want HTTP/2.0——ForceAttemptHTTP2 未生效会让自定义 DialContext 静默退回 HTTP/1.1", got)
	}
	if got := probe("h2-disabled", TransportOpts{DisableHTTP2: true}); got != "HTTP/1.1" {
		t.Errorf("DisableHTTP2=true 协商出 %s want HTTP/1.1（逃生门失效）", got)
	}
}

// TestRoundTripCloseIdleCleansRealTransport 传输层失败清理的端到端验证：
// 真实 *http.Transport 挂空闲连接 → roundTripCloseIdle → 服务器侧连接关闭。
func TestRoundTripCloseIdleCleansRealTransport(t *testing.T) {
	srv := startIdleConnTracker(t)
	tr := newTransport(TransportOpts{})
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for i := 0; i < 2; i++ { // keep-alive 复用同一条连接并回池
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}
	if n := srv.idleConns(); n < 1 {
		t.Fatalf("expected >=1 idle conn after keep-alive requests, got %d", n)
	}

	roundTripCloseIdle(tr)

	// CloseIdleConnections 传播到服务器侧需要一小段时间（FIN 往返）。
	deadline := time.Now().Add(2 * time.Second)
	for srv.idleConns() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := srv.idleConns(); n != 0 {
		t.Errorf("idle conns after roundTripCloseIdle = %d, want 0", n)
	}
}

// TestRoundTripCloseIdleToleratesForeignTransport roundTripCloseIdle 是 best-effort：
// nil 与不实现 closeIdler 的 RoundTripper（测试注入的 rtFunc）静默跳过，不 panic、不误伤。
func TestRoundTripCloseIdleToleratesForeignTransport(t *testing.T) {
	roundTripCloseIdle(nil)
	roundTripCloseIdle(rtFunc(func(*http.Request) (*http.Response, error) { return nil, nil }))
}

// idleCountingTransport 包装 rtFunc 并实现 closeIdler（记录清池调用次数），
// 验证 ChatStreamContext 挂载点「传输层 Do 失败 → 必清池」。
type idleCountingTransport struct {
	rt       rtFunc
	closeIds *int32
}

func (t *idleCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.rt(r)
}

func (t *idleCountingTransport) CloseIdleConnections() {
	atomic.AddInt32(t.closeIds, 1)
}

// TestChatStreamTransportErrorCleansIdleConnections 挂载点行为验证：ChatHTTP 的
// Transport Do 失败（传输层错误）后 CloseIdleConnections 恰好被调用一次；成功路径
// 不触发（成功连接留在池里供复用是连接池的本意）。
func TestChatStreamTransportErrorCleansIdleConnections(t *testing.T) {
	var closeCalls int32
	c := testClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("ChatHTTP is set; plain HTTP client must not be used for chat")
		return nil, nil
	})
	// 首次请求成功（200 + SSE 流），验证成功路径不清池。
	c.ChatHTTP = &http.Client{Timeout: 0, Transport: &idleCountingTransport{
		closeIds: &closeCalls,
		rt: func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(&zeroReader{}),
			}, nil
		},
	}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("first chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if got := atomic.LoadInt32(&closeCalls); got != 0 {
		t.Errorf("CloseIdleConnections calls after success = %d, want 0", got)
	}

	// 传输层失败路径：Do 返回 net.OpError（对端掐连接的真实形态）→ 必清池一次。
	c.ChatHTTP = &http.Client{Timeout: 0, Transport: &idleCountingTransport{
		closeIds: &closeCalls,
		rt: func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
		},
	}}
	if _, _, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{}); err == nil {
		t.Fatal("second chat should fail with transport error")
	}
	if got := atomic.LoadInt32(&closeCalls); got != 1 {
		t.Errorf("CloseIdleConnections calls after transport error = %d, want 1", got)
	}
}

// zeroReader 空 Reader（构造最小 SSE 响应体，零帧即 EOF）。
type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, io.EOF }

// idleConnTracker 测试服务器 + 服务器侧空闲连接集合（ConnState 钩子）。
// 用集合而非计数器：同一连接每次复用回空闲态都会再触发 StateIdle，计数会虚增。
type idleConnTracker struct {
	*httptest.Server
	mu   sync.Mutex
	idle map[net.Conn]struct{}
}

func startIdleConnTracker(t *testing.T) *idleConnTracker {
	t.Helper()
	tr := &idleConnTracker{idle: make(map[net.Conn]struct{})}
	tr.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	tr.Server.Config.ConnState = func(c net.Conn, st http.ConnState) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		switch st {
		case http.StateIdle:
			tr.idle[c] = struct{}{}
		case http.StateClosed:
			delete(tr.idle, c)
		}
	}
	tr.Server.Start()
	t.Cleanup(func() { tr.Server.Close() })
	return tr
}

func (s *idleConnTracker) idleConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idle)
}
