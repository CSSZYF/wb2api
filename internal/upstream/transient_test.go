package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

// transient_test.go 传输层抖动判定（IsTransient）的单测。
//
// 背景：本机网络抖一下（unexpected EOF / connection reset / i/o timeout / TLS 握手
// 失败 / DNS 失败），请求会逐号轮转，若每个账号各记一次连败，达阈即全池降权——
// 形态与 11135「一张坏图拖垮全池」完全相同（见 internal/server 的
// consecutive_degrade_test.go 与 handler_image_terminal_test.go）。
//
// 判定纪律（两条都必须在测试里锁住）：
//  1. 只认「连接/链路层」条件——它们**不指向任何账号**（同一出口链路对全池一视同仁）；
//  2. 判不出来的错误串一律 false（保守：宁可误罚也不能让真故障账号永远留在池内）。

// TestIsTransientMarkers 判定词表逐项锁定（正例：连接/链路层条件）。
func TestIsTransientMarkers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// —— 半截读 / 连接被对端掐断（最典型的抖动形态）——
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"wrapped unexpected EOF", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), true},
		{"string unexpected EOF", errors.New("read tcp 10.0.0.1:443: unexpected EOF"), true},
		{"connection reset by peer", errors.New("read tcp 10.0.0.1:443->1.2.3.4:80: connection reset by peer"), true},
		{"syscall ECONNRESET", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"syscall ECONNABORTED", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNABORTED}, true},
		{"broken pipe", errors.New("write tcp 10.0.0.1:443: write: broken pipe"), true},

		// —— 连不上：超时 / 拒绝 / 路由不可达 ——
		{"i/o timeout (net.Error)", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"i/o timeout (string)", errors.New("read tcp 10.0.0.1:7863->10.0.0.1:35543: i/o timeout"), true},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		{"tls handshake failure", errors.New("remote error: tls: handshake failure"), true},
		{"header timeout", errors.New("net/http: timeout awaiting response headers"), true},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"dial refused (string)", errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), true},
		{"network unreachable", errors.New("dial tcp 1.2.3.4:443: connect: network is unreachable"), true},

		// —— DNS ——
		{"DNSError no such host", &net.DNSError{Err: "no such host", Name: "copilot.tencent.com", IsNotFound: true}, true},
		{"lookup string", errors.New("dial tcp: lookup copilot.tencent.com: no such host"), true},
		{"dns temporary", &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}, true},

		// —— 连接被自己/标准库回收（本地 socket 状态，与账号无关）——
		{"net.ErrClosed", net.ErrClosed, true},
		{"closed network connection (string)", errors.New("use of closed network connection"), true},
		{"server closed idle connection", errors.New("http: server closed idle connection"), true},

		// —— HTTP 502/503/504 的**传输层**形态：CONNECT 隧道建立失败 ——
		// Go 标准库对 CONNECT 非 200 返回 &net.OpError{Op:"proxyconnect"}，其 Err 是
		// HTTP 状态行文本；再被 url.Error 包一层。代理/隧道故障是链路问题，不是账号故障。
		{"proxyconnect 502", proxyConnectErr("502 Bad Gateway"), true},
		{"proxyconnect 503", proxyConnectErr("503 Service Unavailable"), true},
		{"proxyconnect 504", proxyConnectErr("504 Gateway Timeout"), true},
		{"proxyconnect 502 (bare string)", errors.New(`Post "https://x": proxyconnect tcp: 502 Bad Gateway`), true},

		// —— 明确**不**算抖动的（保守语义，逐条锁定）——
		// 裸 io.EOF = 对端「正常关闭」连接：可能是抖动，也可能是上游主动断（审核掐流等），
		// 无法区分 → 按保守口径不算 transient（继续喂连败，宁可误罚也不漏判真故障号）。
		{"bare io.EOF", io.EOF, false},
		{"wrapped bare io.EOF", fmt.Errorf("read body: %w", io.EOF), false},
		{"string EOF only", errors.New("EOF"), false},
		// 客户端断开/超时：人已走，既不该罚号也不该当抖动（handler 侧另有 ctx 闸门）。
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		// 代理鉴权失败：配置问题，退避重试不会变好（不该被当成抖动静默吞掉）。
		{"proxyconnect 407", proxyConnectErr("407 Proxy Authentication Required"), false},
		// 判不出来的错误串 → 保持现状（继续喂连败）。
		{"nil", nil, false},
		{"unknown string", errors.New("something weird happened"), false},
		{"empty", errors.New(""), false},
		// 5xx 信封错误是 *upstream.Error（ErrServer，走熔断路径），不在本判定范围——
		// 这里显式锁住「不会因为 ErrServer 的文本被误判为抖动」。
		{"ErrServer envelope", &Error{Kind: ErrServer, Status: 500, Msg: "upstream 500"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransient(c.err); got != c.want {
				t.Errorf("IsTransient(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}

// proxyConnectErr 构造 Go 标准库 CONNECT 隧道失败的真实错误形态：
// url.Error{ Op: "Post", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("<状态行>")} }。
func proxyConnectErr(status string) error {
	return &url.Error{
		Op:  "Post",
		URL: "https://copilot.tencent.com/v2/chat/completions",
		Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New(status)},
	}
}

// TestIsTransientCoversHandlerBranchInputs 端到端形态：handler 网络抖动分支拿到的
// 就是 `Do` 的原始错误（url.Error 包装 net.OpError 包装 syscall 错误），errors.As/
// errors.Is 必须能穿透这层包装——否则真实故障在判定上全落「未知错误」保守分支。
func TestIsTransientCoversHandlerBranchInputs(t *testing.T) {
	// net/http 的真实形态：&url.Error{Op, URL, Err: &net.OpError{...}}
	wrapped := &url.Error{
		Op:  "Post",
		URL: "https://copilot.tencent.com/v2/chat/completions",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
	}
	if !IsTransient(wrapped) {
		t.Errorf("url.Error→net.OpError→ECONNRESET 应判为抖动: %v", wrapped)
	}
	// 已被 fmt.Errorf 再包一层的（client 侧 `fmt.Errorf("read body: %w", rerr)`）。
	if !IsTransient(fmt.Errorf("read body: %w", wrapped)) {
		t.Error("%w 包装后仍应判为抖动")
	}
	// 客户端 ctx 取消派生出的错误（人已走）不判抖动。
	if IsTransient(fmt.Errorf("chat stream: %w", context.Canceled)) {
		t.Error("ctx 取消不是抖动")
	}
}
