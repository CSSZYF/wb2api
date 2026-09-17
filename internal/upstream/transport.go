// transport.go 出站 Transport 构造的单一事实来源（连接层加固）：真正禁 h2 /
// TLS 握手超时 / 短 keepalive 探测，参数集中定义、测试可回读断言。
//
// 吸收的实测经验（kongjianguan 4 连击）：半死连接最典型的形态是「对端已断、本地
// 无感知」——默认 Dialer 既无建连上限、keepalive 又要 2h 才发首个探测，连接池里
// 90s 内的空闲连接多半已死却照旧被复用，表现为单请求 TTFB 卡到几百秒。四件加固
// 里前三件 + 失败清池在本文件与调用点落地；DisableKeepAlives 不吸收（与
// MaxIdleConnsPerHost=20 的连接复用意图相反，每请求 TLS 握手开销更大）。
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// 连接层参数集中定义（与 server/backoff.go 同风格：一处定义，测试可回读断言）。
const (
	// dialTimeout TCP 连接建立上限。半死连接的第一道闸：连不上就快速失败
	// 轮转换号，不再干等系统 TCP 重传窗口（实测半开 TCP 单次 TTFB 卡 936s
	// ——默认 Dialer 无超时上限）。
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive 探测周期。默认 Dialer 2h 才发首个探测——
	// NAT 黑洞里 2h 足够连接半死且被复用。15s 周期让死连接在 15~30s 内被
	// 内核掐掉（RST/ETIMEDOUT），复用侧立即感知而非卡到重传窗口。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限。此前完全缺失——握手挂起时无任何层
	// 兜底（ResponseHeaderTimeout 只在请求写完后才计时），只能干等到
	// HTTP.Client.Timeout(120s)。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout 空闲连接池保留时长。从 90s 收到 30s：WAF 风暴后上游
	// 常态性掐闲置连接，90s 池里的连接多半已死；复用侧仍有 15s keepalive
	// 兜底识别。
	idleConnTimeout = 30 * time.Second
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

// newDialer 构造出站拨号器（DialContext 的 Timeout/KeepAlive 参数集中于此，
// 供测试回读断言）。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例，连接池不重复）。
// 分两层防半死连接：
//   - TLS 层：空 TLSNextProto 真正禁 h2（实测二次修正：ForceAttemptHTTP2=false
//     只对自定义 Dial 生效，默认 TLS 经 ALPN 仍协商出 h2，半死 h2 流复用表现为
//     "http2: timeout awaiting response headers"——唯一正确写法是置空映射，让
//     ALPN 完成后无 h2 协议可用，连接退回 HTTP/1.1）。
//   - TCP 层：DialContext 10s 建连上限 + 15s keepalive 探测，半开连接在建立期
//     和复用期都能被快速识别（见 dialTimeout/dialKeepAlive 注释）。
func newTransport() *http.Transport {
	dialer := newDialer()
	return &http.Transport{
		DialContext: dialer.DialContext,
		// 空 TLSNextProto（非 nil）真正禁 h2：见函数注释。必须 make 而非 nil——
		// nil 表示「让标准库注入默认 h2 映射」（设 ForceAttemptHTTP2=false 后
		// 日志仍报 h2 timeout，正是这个陷阱）。
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: defaultResponseHeaderTimeout,
	}
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
