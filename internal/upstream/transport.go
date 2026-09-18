// transport.go 出站 Transport 构造的单一事实来源（连接层加固）：h2 开关 /
// TLS 握手超时 / 短 keepalive 探测，参数集中定义、测试可回读断言。
//
// 吸收的实测经验（kongjianguan 4 连击）：半死连接最典型的形态是「对端已断、本地
// 无感知」——默认 Dialer 既无建连上限、keepalive 又要 2h 才发首个探测，连接池里
// 90s 内的空闲连接多半已死却照旧被复用，表现为单请求 TTFB 卡到几百秒。四件加固
// 里前三件 + 失败清池在本文件与调用点落地；DisableKeepAlives 不吸收（与
// MaxIdleConnsPerHost=20 的连接复用意图相反，每请求 TLS 握手开销更大）。
//
// 2026-09-18 h2 反转（默认启用）：此前「空 TLSNextProto 禁 h2」是为躲半死 h2 流
// （"http2: timeout awaiting response headers"）。但 CN 上游走 TUN 代理
// （verge-mihomo）时链路握手长，实测对比（CN 上游各 10 次）——
//   - 允许 h2：HTTP/2.0，连接复用 90%，TLS 握手首次 1123ms、后续 92-98ms；
//   - 禁 h2  ：HTTP/1.1，abort 模式下复用率 0%（每请求重新握手）→ 频繁
//     TLS handshake timeout。
//
// 结论：该环境 h2 明显更优，故 h2 改为**默认启用**（config upstream.disable_http2
// 可关，默认 false = 启用）；禁 h2 路径逐字保留（空 TLSNextProto），供 h2 异常时
// 逃生。连接层四项参数（h2 / 握手 / 拨号 / 空闲池）全部配置化，见 TransportOpts。
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// 连接层参数集中定义（与 server/backoff.go 同风格：一处定义，测试可回读断言）。
// 这些常量是**缺省值**：生产生效值由 config upstream.* 经 TransportOpts 注入
// （见 TransportOpts / Config 注释），常量只在「配置未接线 / 测试裸用」时兜底。
const (
	// dialTimeout TCP 连接建立上限。半死连接的第一道闸：连不上就快速失败
	// 轮转换号，不再干等系统 TCP 重传窗口（实测半开 TCP 单次 TTFB 卡 936s
	// ——默认 Dialer 无超时上限）。
	//
	// 2026-09-18 与 tlsHandshakeTimeout 同步放宽到 30s：TCP 建连与 TLS 握手
	// 是同一网络路径上的相邻阶段，握手实测 >10s 的链路建连也可能偏慢；
	// 两者取齐避免「建连 10s 截断、握手 30s 放行」的口径分裂。
	// 可配：config upstream.dial_timeout_seconds（<=0 回落本值）。
	dialTimeout = 30 * time.Second
	// dialKeepAlive TCP keepalive 探测周期。默认 Dialer 2h 才发首个探测——
	// NAT 黑洞里 2h 足够连接半死且被复用。15s 周期让死连接在 15~30s 内被
	// 内核掐掉（RST/ETIMEDOUT），复用侧立即感知而非卡到重传窗口。
	// 不配置化：它不是「用户会调」的量，固定值即可（保持既有行为）。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限。此前完全缺失——握手挂起时无任何层
	// 兜底（ResponseHeaderTimeout 只在请求写完后才计时），只能干等到
	// HTTP.Client.Timeout(120s)。
	//
	// 2026-09-18 实测修正：10s 过紧。CN 上游 copilot.tencent.com 在国内网络下
	// 握手常态 >10s，实测单次请求连续两次 TLS handshake timeout（各白等 10.5s）
	// 后第三次才成功——用户观感是「开头卡一两分钟」。放宽到 30s：正常握手
	// （<3s）零影响，慢握手不再被误杀；半死连接仍由 dialKeepAlive(15s) 与
	// ResponseHeaderTimeout 兜底。可配：config upstream.tls_handshake_timeout_seconds。
	tlsHandshakeTimeout = 30 * time.Second
	// idleConnTimeout 空闲连接池保留时长。
	//
	// 2026-09-18 反转回 90s（此前从 90s 收到 30s）：30s 太激进——连接刚建好
	// 就可能已过期，h2 下尤其亏（一条 h2 连接承载全部并发流，被回收等于下个
	// 请求重新握手）。v1.9.6 用的就是 90s 且用户实测表现顺滑；半死连接仍有
	// dialKeepAlive(15s) 兜底识别，不必靠缩短池寿命来兜。可配：
	// config upstream.idle_conn_timeout_seconds。
	idleConnTimeout = 90 * time.Second
	// defaultResponseHeaderTimeout 聊天 SSE 首字节前（响应头）上限的构造默认。
	// **不在此处收紧**：生产生效值由 config 驱动——cmd/server/main.go 在 New()
	// 之后用 cfg.Upstream.HeaderTimeoutSeconds 无条件覆盖本字段（缺省 normalize
	// 回落 timeout_seconds），硬编码写死会在接线后失效。本值只作「配置未接线 /
	// 测试裸用 newTransport」时的安全网，与既有 New() 默认一致（120s），零行为
	// 变更。语义提醒：该超时只计响应头到达前，头到达后 SSE 长流不受影响（流中
	// 空闲由 IdleTimeout 监控，见 idle.go）。
	defaultResponseHeaderTimeout = 120 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost 连接池容量（既有值，一并集中定义）。
const (
	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

// TransportOpts 出站 Transport 的连接层可配参数（config upstream.* 的注入载体）。
// 零值即**生产默认**（h2 启用 + 30s/30s/90s），故 main 侧漏配某项时行为与
// 「全部走缺省」一致，不会因零值把某项配成 0 导致禁用（<=0 一律回落缺省）。
type TransportOpts struct {
	// DisableHTTP2 真正禁 h2（空 TLSNextProto）。零值 false = **启用 h2**，
	// 对应 config upstream.disable_http2（默认 false）。语义刻意用「disable」
	// 而非「enable」：h2 是默认行为，配置项只描述「要不要关掉它」，于是
	// JSON/面板缺键（零值）天然落在「启用 h2」这一侧，无需 Default() 反向补值
	// 也不会被 json 零值意外关掉（enable 语义下漏配一项就静默退回 HTTP/1.1）。
	DisableHTTP2 bool
	// TLSHandshakeTimeout TLS 握手上限；<=0 回落 tlsHandshakeTimeout(30s)。
	TLSHandshakeTimeout time.Duration
	// DialTimeout TCP 建连上限；<=0 回落 dialTimeout(30s)。
	DialTimeout time.Duration
	// IdleConnTimeout 空闲连接池保留时长；<=0 回落 idleConnTimeout(90s)。
	IdleConnTimeout time.Duration
}

// newDialer 构造出站拨号器（DialContext 的 Timeout/KeepAlive 参数集中于此，
// 供测试回读断言）。timeout <=0 时回落缺省 dialTimeout。
func newDialer(timeout time.Duration) *net.Dialer {
	if timeout <= 0 {
		timeout = dialTimeout
	}
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例，连接池不重复）。
// 分两层防半死连接：
//   - TLS 层：h2 默认启用（opts.DisableHTTP2=false 时**不设 TLSNextProto**，
//     让标准库按 ALPN 正常协商 h2）；opts.DisableHTTP2=true 时置空映射真正禁
//     h2（实测二次修正：ForceAttemptHTTP2=false 只对自定义 Dial 生效，默认 TLS
//     经 ALPN 仍协商出 h2，半死 h2 流复用表现为 "http2: timeout awaiting
//     response headers"——唯一正确写法是置空映射，让 ALPN 完成后无 h2 协议可用，
//     连接退回 HTTP/1.1）。
//   - TCP 层：DialContext 建连上限 + 15s keepalive 探测，半开连接在建立期和
//     复用期都能被快速识别（见 dialTimeout/dialKeepAlive 注释）。
//
// h2 启用路径必须显式设 ForceAttemptHTTP2=true：标准库的保守逻辑（transport.go
// 的 protocols()：设了自定义 Dial/DialContext/TLSClientConfig 且
// ForceAttemptHTTP2=false 时不自动启用 h2）会让「自定义 DialContext + 不设
// TLSNextProto」静默退回 HTTP/1.1——实测（本机 Go 1.27 + httptest h2 服务端）：
// 不设 ForceAttemptHTTP2 → HTTP/1.1；设 true → HTTP/2.0。这正是任务书提到的
// 「ForceAttemptHTTP2 语义陷阱」的另一面：**关掉它并不能关 h2（有 TLSNextProto
// 注入路径），但不开它则开不了 h2（自定义 dialer 路径）**。故：禁 h2 靠空
// TLSNextProto（ForceAttemptHTTP2 保持 false，空映射使 h2 无实现可用），
// 启 h2 靠 ForceAttemptHTTP2=true + TLSNextProto 保持 nil。
func newTransport(opts TransportOpts) *http.Transport {
	dialer := newDialer(opts.DialTimeout)
	tr := &http.Transport{
		DialContext: dialer.DialContext,
		// MaxIdleConns/MaxIdleConnsPerHost 连接池容量（h2 下同一条连接承载多流，
		// 池容量仍按「连接」计，无需因 h2 调小）。
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		// TLSHandshakeTimeout 与 IdleConnTimeout 均可配（<=0 回落缺省）。
		TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: defaultResponseHeaderTimeout,
	}
	if tr.TLSHandshakeTimeout <= 0 {
		tr.TLSHandshakeTimeout = tlsHandshakeTimeout
	}
	if tr.IdleConnTimeout <= 0 {
		tr.IdleConnTimeout = idleConnTimeout
	}
	if opts.DisableHTTP2 {
		// 空 TLSNextProto（非 nil）真正禁 h2：见函数注释。必须 make 而非 nil——
		// nil 表示「让标准库注入默认 h2 映射」（设 ForceAttemptHTTP2=false 后
		// 日志仍报 h2 timeout，正是这个陷阱）。
		tr.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	} else {
		// 启用 h2：TLSNextProto 保持 nil（标准库据此注入 h2 映射），但必须
		// ForceAttemptHTTP2=true 覆盖「自定义 DialContext 保守禁 h2」的默认
		// ——否则本 Transport 静默退回 HTTP/1.1（实测见函数注释）。
		tr.ForceAttemptHTTP2 = true
	}
	return tr
}

// closeIdler 实现该接口的 RoundTripper 支持清空空闲连接池（*http.Transport、
// http2.Transport 等均满足；测试注入的自定义 RoundTripper 可选择性实现）。
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle 在传输层请求失败后清掉 rt 所属 Transport 的空闲连接池：
// 失败连接可能仍留在空闲池里，等 IdleConnTimeout 才过期，下一个请求会继续捡到它。
//
// 挂载点：错误分类（Classify）只见业务信封——传输层失败根本没有 body 可分类
// （见 ChatStreamContext 对 Do 失败 / 读 body 失败的处理：不进 Classify、不罚号）。
// 这类失败的正确处理正是连接层的池清理，故挂在与 Do 并列的传输层出口，而非错误
// 策略（applyErrorPolicy）。
//
// 关闭是 best-effort：rt 为 nil 或未实现 closeIdler（如测试注入的 rtFunc）时
// 静默跳过。CloseIdleConnections 只关空闲连接，不影响在途请求；瞬时代价是下个
// 请求多一次 TCP+TLS 握手，与半死连接被复用卡到首字节超时的风险完全不成比例。
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}
