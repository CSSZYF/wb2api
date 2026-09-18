package upstream

import (
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
func TestNewDialerParams(t *testing.T) {
	d := newDialer()
	if d.Timeout != dialTimeout {
		t.Errorf("dialer.Timeout=%v want %v", d.Timeout, dialTimeout)
	}
	if d.KeepAlive != dialKeepAlive {
		t.Errorf("dialer.KeepAlive=%v want %v", d.KeepAlive, dialKeepAlive)
	}
	if d.Timeout != 30*time.Second || d.KeepAlive != 15*time.Second {
		t.Errorf("dial params=(%v, %v) want (30s, 15s)——30s 为 2026-09-18 国内网络实测放宽", d.Timeout, d.KeepAlive)
	}
}

// TestNewTransportHardening 连接层加固配置断言（transport.go 集中参数的回读验证）：
// 真正禁 h2 / TLS 握手超时 / 空闲池收紧，且 ResponseHeaderTimeout 保持 config 驱动的
// 缺省值不变。挂在 New() 的成品 Transport 上（而非 newTransport() 裸返回）——同一
// 对象同时被 HTTP 与 ChatHTTP 持有，任何字段断言都直接对应生产出站行为。
func TestNewTransportHardening(t *testing.T) {
	tr, ok := New().ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", New().ChatHTTP.Transport)
	}
	// 1. 真正禁 h2：TLSNextProto 必须是「非 nil 且不含 h2」的空映射。
	//    nil = 标准库注入默认 h2 映射（ForceAttemptHTTP2 陷阱，见 transport.go）。
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil empty map to disable HTTP/2 (nil = stdlib re-enables h2)")
	}
	if _, registered := tr.TLSNextProto["h2"]; registered {
		t.Error("TLSNextProto must not register h2")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto must be empty, got %d entries", len(tr.TLSNextProto))
	}
	// 2. TLS 握手超时（此前完全缺失：握手挂起只能干等到 HTTP.Client.Timeout）。
	if tr.TLSHandshakeTimeout != 30*time.Second {
		t.Errorf("TLSHandshakeTimeout=%v want 30s（国内网络握手实测 >10s，10s 会误杀）", tr.TLSHandshakeTimeout)
	}
	// 3. 空闲连接池：从 90s 收到 30s（WAF 风暴后池里连接多半已死）。
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout=%v want 30s", tr.IdleConnTimeout)
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

// TestRoundTripCloseIdleCleansRealTransport 传输层失败清理的端到端验证：
// 真实 *http.Transport 挂空闲连接 → roundTripCloseIdle → 服务器侧连接关闭。
func TestRoundTripCloseIdleCleansRealTransport(t *testing.T) {
	srv := startIdleConnTracker(t)
	tr := newTransport()
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
