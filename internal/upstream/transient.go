// transient.go 传输层「抖动」判定：把「出口链路/连接层条件」与「账号故障」分开。
//
// 为什么需要它（自伤面）：本机网络抖一下（unexpected EOF / connection reset /
// i/o timeout / TLS 握手失败 / DNS 失败），handler 的传输层分支会对**每个**轮转到的
// 账号各喂一次连败计数（NoteFailures）→ 达阈（默认 5）全体降权 10 分钟 → 用户看到
// 「503 all accounts unavailable (cooling/disabled)」。形态与 11135「一张坏图拖垮整个
// 账号池」完全相同（见 internal/server/handler_image_terminal_test.go）。
//
// 为什么链路条件不该喂连败：连败降权（issue #114）的判据是「不知道原因的失败**指向
// 该账号**」；而下列条件是**出口链路**的属性——同一链路对池内全部账号一视同仁，
// 一个请求里 N 个账号全中恰恰证明根因不在账号上（证据不指向任何单个账号）。
// 上游参照 hub 的 f7bf4e2 is_transient()：判定 ssl / unexpected_eof / connection
// reset / timed out / 502|503|504 / incompleteread 这类标记 → 换号退避重试、不记冷却，
// 抖动熬过重试后如实抛 502。
//
// 两条纪律：
//  1. **保守优先**：判定不出来的（未知错误串）返回 false → 调用方保持现状继续喂连败。
//     宁可误罚也不能让真故障账号永远留在池内（漏判真故障的代价是坏号持续被选中并
//     消耗健康号配额；误罚的代价只是一个连败计数，成功一次即清零回池）。
//  2. **不并入 ErrServer**：HTTP 502/503/504 的**业务信封**响应已有权威分类
//     （Classify 的 status>=500 → ErrServer → 熔断，是唯一熔断入口），本函数只认
//     「传输层形态」的 502/503/504（CONNECT 隧道建立失败，见 proxyConnectMarkers），
//     绝不把 ErrServer 一起并进来——那会改掉 5xx 熔断的既有语义。
//
// 跨平台（本项实测踩到的坑，务必保留）：Windows 上 Go 网络栈暴露的是 **WSA 错误码**
// （10054/10061/…），与 syscall 包里 posix 风格的 ECONNRESET(536870935)/ECONNREFUSED
// 等常量 **errors.Is 不相等**（实测：`errors.Is(syscall.Errno(10054), syscall.ECONNRESET)`
// = false），且错误文本是 Windows 本地化的英文（"An existing connection was forcibly
// closed by the remote host."，不含 "connection reset" 子串）。故判定必须三路并行：
// posix 哨兵 + WSA 数字码 + 双平台错误串词表；只靠其中任一路都会在某个平台上漏判。
package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// transientMarkers 传输层抖动错误串词表（大小写不敏感子串匹配）。
//
// 词表纪律（宁缺毋滥）：每条都必须是「连接/链路层」条件，且**不指向任何账号**。
// 收录依据（net/http 与 net 的稳定错误文本 + 本机实测形态）：
//   - "unexpected eof" / "incomplete read"：连接**未写完**就断（半截读）——链路故障的
//     确定性形态（对照裸 EOF，见 IsTransient 注释）；
//   - "connection reset" / "connection aborted" / "broken pipe"：对端/中间设备重置连接
//     （POSIX 文本）；
//   - "forcibly closed by the remote host" / "aborted by the software in your host
//     machine" / "actively refused it" / "did not properly respond after a period of
//     time" / "unreachable host" / "unreachable network"：**Windows 本地化文本**
//     （WSAECONNRESET/ECONNABORTED/ECONNREFUSED/WSAETIMEDOUT/WSAEHOSTUNREACH/
//     WSAENETUNREACH 的 .Error() 输出，实测逐字，见文件头跨平台说明）；
//   - "i/o timeout" / "deadline exceeded" 家族：超时（拨号/握手/首字节），链路慢或不可达；
//   - "tls handshake" 家族：TLS 协商失败（含中间盒劫持/证书链问题）；
//   - "no such host" / "server misbehaving" / "name resolution"：DNS 解析失败（本机/
//     运营商 DNS 抖动是典型场景）；
//   - "connection refused" / "network is unreachable" / "no route to host"：连不上；
//   - "use of closed network connection" / "http: server closed idle connection"：
//     连接被本地标准库回收（空闲连接被服务端关掉后复用失败，是 keep-alive 的常态
//     竞态，与账号无关）。
//
// 刻意**不**收录的：
//   - "eof"（裸）：见 IsTransient 注释（对端正常关闭，不确定）；
//   - "canceled" / "context canceled"：客户端断连——人已走，既不是抖动也不该罚号
//     （handler 侧另有 ctx 闸门，且这种情形下退避重试无意义）；
//   - "proxy authentication required"（CONNECT 407）：代理鉴权是配置错误，退避重试
//     不会变好（保守：维持喂连败的现状语义，让它浮出来被运维看见）。
var transientMarkers = []string{
	// —— 半截读 / 连接重置（POSIX 文本）——
	"unexpected eof",
	"incomplete read",
	"connection reset",
	"connection aborted",
	"broken pipe",
	// —— 连接重置 / 拒绝 / 不可达（Windows 本地化文本，实测逐字）——
	"forcibly closed by the remote host",
	"aborted by the software in your host machine",
	"actively refused it",
	"did not properly respond after a period of time",
	"unreachable host",
	"unreachable network",
	// —— 超时 ——
	"i/o timeout",
	"timeout awaiting response headers",
	"tls handshake timeout",
	// —— TLS ——
	"tls handshake failure",
	"remote error: tls",
	// —— DNS ——
	"no such host",
	"server misbehaving",
	"temporary failure in name resolution",
	// —— 连不上（POSIX 文本）——
	"connection refused",
	"network is unreachable",
	"no route to host",
	// —— 本地连接回收 ——
	"use of closed network connection",
	"http: server closed idle connection",
}

// proxyConnectMarkers CONNECT 隧道建立失败的状态行词表（HTTP 502/503/504 的**传输层**
// 形态）。Go 标准库对 CONNECT 非 200 返回 &net.OpError{Op:"proxyconnect"}，其 Err 是
// HTTP 状态行文本（transport.go: `return nil, errors.New(text)`），再被 url.Error 包一层。
//
// 只认 502/503/504：502 Bad Gateway / 503 Service Unavailable / 504 Gateway Timeout 是
// **网关/代理**侧故障（与账号无关，重试有意义）；407 Proxy Authentication Required 是
// 配置错误（代理鉴权），退避重试不会变好，刻意不收录（保守：维持喂连败的现状语义）。
//
// 与 ErrServer 的分工：业务信封 5xx（上游真的回了 502 + body）走 Classify → ErrServer
// → 熔断；本词表只覆盖「连隧道都没建起来」的形态（Do 返回 error，根本拿不到
// *upstream.Error），两者在调用点上天然互斥（见 handler 的 uerr == nil 分支）。
var proxyConnectMarkers = []string{
	"502 bad gateway",
	"503 service unavailable",
	"504 gateway timeout",
}

// wsaTransientErrs Windows 网络栈的 WSA 错误码（**数字直判**，见文件头跨平台说明：
// 这些值不是 syscall 包导出的常量，且与 posix 风格常量 errors.Is 不相等）。
// 在非 Windows 平台上这些数字只是普通 Errno 值，不会误命中。
//
//	10051 WSAENETUNREACH / 10053 WSAECONNABORTED / 10054 WSAECONNRESET /
//	10060 WSAETIMEDOUT   / 10061 WSAECONNREFUSED / 10065 WSAEHOSTUNREACH
var wsaTransientErrs = []syscall.Errno{10051, 10053, 10054, 10060, 10061, 10065}

// IsTransient 报告 err 是否属于「传输层抖动」（连接/链路层条件，不指向任何账号）。
// 调用方（handler 的传输层错误分支）据此决定是否喂连败计数：true → 只换号退避重试，
// 不记惩罚；false → 保持既有语义（继续喂连败）。
//
// 判定顺序（严到宽，全部走 errors.As/errors.Is 穿透 url.Error/net.OpError 包装）：
//  1. ctx 取消/超时 → false（人已走，不是抖动；handler 侧 ctx 闸门另行处理）；
//  2. 裸 io.EOF → false（**判断：不算抖动**，理由见下）；
//  3. net.Error 类型：Timeout() → true（i/o timeout / 握手超时 / 首字节超时）；
//  4. 连接层哨兵：net.ErrClosed / ECONNRESET / ECONNREFUSED / ECONNABORTED / EPIPE /
//     EHOSTUNREACH / ENETUNREACH（posix 常量）+ WSA 数字码（wsaTransientErrs）；
//  5. *net.DNSError → true（DNS 是链路条件；IsNotFound/IsTemporary 都是解析失败）；
//  6. 错误串词表（transientMarkers + proxyConnectMarkers，大小写不敏感）；
//  7. 都不命中 → false（**保守优先**：未知错误维持既有语义继续喂连败）。
//
// 裸 io.EOF 的判断（**明确不算 transient**）：io.EOF 表示「连接被对端**正常关闭**」
// （TCP FIN，对端把该发的字节发完了）。它有两种无法区分的成因：
//   - 链路抖动（中间设备/代理提前收流，恰好停在消息边界上）；
//   - 上游**主动**断流（内容审核掐流、后端重启/摘流、限流器静默断连）。
//
// 后者是「上游对这次请求的决定」，若判为抖动就免罚，会让真正有问题的账号/请求永远
// 留在池内（与保守优先的取舍相悖）。同时 io.EOF 也是 http.Transport 对「服务端关掉
// 空闲连接后复用」的常见返回形态之一——但那种情形另有 "server closed idle connection"
// 词条覆盖，不必靠裸 EOF 兜。故按不确定处理：不判 transient，维持喂连败的现状。
// 对照：io.ErrUnexpectedEOF（"unexpected EOF"）是**连接未写完就断**，属链路故障的
// 确定性形态，判 transient（errors.Is(io.ErrUnexpectedEOF, io.EOF) 为 false，两者
// 在判定上天然不混淆）。
//
// nil 返回 false（无错误无抖动）。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// 1) 客户端断连/超时：不是抖动。必须最先排除——http.Client.Timeout 触发的错误文本
	//    是 "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"，
	//    会被下方的超时词表误收（那是「我们自己设的整请求上限」，不是链路抖动）。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 2) 裸 io.EOF：对端正常关闭，成因不确定 → 不算抖动（见上方注释）。
	//    io.ErrUnexpectedEOF 与 io.EOF 不相等（已实测），不会在这里被一起排除。
	if errors.Is(err, io.EOF) {
		return false
	}
	// 3) 超时类：net.Error.Timeout()（i/o timeout、TLS handshake timeout、
	//    timeout awaiting response headers 都实现该接口）。
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// 4) 连接层哨兵：posix 常量（Linux/macOS）+ WSA 数字码（Windows）。
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	for _, sentinel := range []error{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.EPIPE,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		for _, wsa := range wsaTransientErrs {
			if errno == wsa {
				return true
			}
		}
	}
	// 5) DNS 失败：解析层条件，与账号无关。
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// 6) 错误串词表（含 CONNECT 隧道状态行）。net 的错误文本跨平台稳定（Windows 是
	//    本地化英文，已在词表里逐条收录），这里只是「识别抖动」的宽松方向，
	//    误收代价仅为一个连败计数（见文件头纪律 1）。
	msg := strings.ToLower(err.Error())
	for _, m := range transientMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	for _, m := range proxyConnectMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	// 7) 判不出来 → false（保守优先，调用方保持现状）。
	return false
}
