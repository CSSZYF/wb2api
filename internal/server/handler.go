// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限（字节）；<=0 = **不预拦截**（DefaultMaxBodyBytes）。
	//
	// 语义（吸收上游 73fe1f8）：默认**不做**网关侧预拦截，大请求完整读入后交由上游
	// 自然响应——上游错误信息量更大（能看到上游到底是什么策略），网关提前 413 反而
	// 挡住上游真实行为；多图/长上下文会话（历史图片每轮 base64 重发）不再撞网关上限。
	// 显式配置 >0 时仍保留一道**网关侧内存护栏**（不是上游限制）：超限直接 413
	// request_body_too_large（不把半截请求喂给上游，issue #41）。护栏的口径与关闭方式
	// 见 bodyTooLargeMsg；配置项 server.max_body_mb 保留（删键会让老配置静默失效）。
	MaxBodyBytes int64
	// ReadTimeout 入站请求体读取窗口（http.Server.ReadTimeout 的同口径值）；
	// <=0 兜底 DefaultReadTimeout（300s）。运行期经 SetReadTimeout 热改，
	// 由 armBodyReadDeadline 逐请求生效（面板保存后无需重启）。
	ReadTimeout time.Duration
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（无重置时间时有界退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// StripRealmPrefix true（缺省）= /v1/models 输出裸模型名，不带 "cn:"/"global:"
	// 前缀；显式前缀在入站方向仍被解析（老客户端配置零改动）。
	// false = 保留 v1 起的历史行为（每个 id 带域前缀）。
	StripRealmPrefix bool
	// RealmPrecedence 裸模型名在两域都有账号时的默认归属域："global"（缺省）| "cn"。
	RealmPrecedence string
	// HiddenModels 对外隐藏的模型名（upstream.ResolveHiddenModels 归一化后的集合）。
	// 与面板「模型与档位」同口径——两处必须用同一份，否则会出现"面板看得见、
	// 客户端调不到"。nil = 不隐藏。
	HiddenModels upstream.HiddenSet
	// PinnedModels 强制内置的模型条目（上游目录不给、但可调用的模型）。
	// 上游已返回同名模型时以上游数据为准，只在缺失时兜底追加。
	PinnedModels []upstream.PinnedModel

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// PTLMaxTokensRetry 11115「prompt is too long」时按错误里的真实数字下调
	// max_tokens 重试一次的开关（features.ptl_max_tokens_retry）。
	// **默认 true**（nil = 开）：只在 11115 时生效、且失败也退回首次原始错误
	// （客户端看到的报错与不加本项时逐字一致），故默认开是安全的；显式 &false 关闭。
	// 指针类型是为了区分「未配置（用默认 true）」与「显式 false」——config 侧
	// Default() 会给 true，测试/裸用场景传 nil 即默认开。
	PTLMaxTokensRetry *bool
	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:       h.cfg.APIKey,
		SoftCooldown: h.cfg.SoftCooldown,
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
	// router 模型名 → realm 归属决策（显式前缀优先，裸名按池内可用域）。
	// 与 cmd 侧粘性闭包同源，避免两处各写一套域判定而漂移。
	router RealmRouter
	// maxBodyBytes 请求体上限的运行期值（cfg.MaxBodyBytes 的原子镜像）。
	// 面板在线改 server.max_body_mb 时经 SetMaxBodyBytes 热生效，无需重启
	// （issue #17：改了配置却静默不生效，用户仍被 8MB 413 拦截）。
	maxBodyBytes atomic.Int64
	// maxRotate 单请求最多换号次数的运行期值（cfg.MaxRotate 的原子镜像）。
	// 面板在线改 server.max_rotate 时经 SetMaxRotate 热生效，无需重启
	// （池内账号多时默认 3 次试不满所有号）。
	maxRotate atomic.Int64
	// readTimeout 入站 body 读取窗口的运行期值（cfg.ReadTimeoutSeconds 的原子镜像）。
	// 面板在线改 server.read_timeout_seconds 时经 SetReadTimeout 热生效，无需重启。
	// 为什么是原子镜像而不是直接写 http.Server.ReadTimeout：后者是**裸字段**，只在
	// readRequest（server.go:1039）里被无锁读一次，运行期从面板 goroutine 赋值是
	// 数据竞争（-race 会报，且 http.Server 并未提供 SetReadTimeout 这样的 setter，
	// 只有 SetKeepAlivesEnabled）。故热改走"逐请求重设读截止"路线：见
	// armBodyReadDeadline。
	readTimeout atomic.Int64
	// ptlMaxTokensRetry 11115「prompt is too long」按真实数字下调 max_tokens 重试
	// 一次的运行期开关（cfg.PTLMaxTokensRetry 的原子镜像）。面板保存
	// features.ptl_max_tokens_retry 后经 SetPTLMaxTokensRetry 热生效（与
	// sanitize_fingerprints 等 features 项同口径：保存即生效，不必重启）。
	// 初值由 NewHandler 从 Config 装入（nil = 默认 true，见 Config.PTLMaxTokensRetry）。
	ptlMaxTokensRetry atomic.Bool
}

// SetMaxBodyBytes 热更新请求体上限（面板保存配置路径调用）。
// n<=0 = 关闭预拦截（存 0，与 NewHandler 兜底口径一致，见 DefaultMaxBodyBytes）。
//
// 负值同样按"关闭"处理：负的字节上限没有合理语义，若静默当成极小值会把所有请求
// 打成 413（最坏的反向风险）；0/负值都表达"不设护栏"。
func (h *Handler) SetMaxBodyBytes(n int64) {
	if n < 0 {
		n = 0
	}
	h.maxBodyBytes.Store(n)
}

// SetMaxRotate 热更新单请求最多换号次数（面板保存配置路径调用）。
// n<=0 与 NewHandler 兜底口径一致：回落默认 3。
func (h *Handler) SetMaxRotate(n int) {
	if n <= 0 {
		n = 3
	}
	h.maxRotate.Store(int64(n))
}

// DefaultMaxBodyBytes 聊天请求体上限的默认值（与 config 侧 Default() 同口径）。
// 单一来源供 handler 与 cmd 两侧共用，避免"配置默认一个数、handler 兜底另一个数"
// 的静默漂移（与 DefaultReadTimeout 同一处理风格）。
//
// **0 = 不预拦截**（吸收上游 73fe1f8）：请求体完整读入后交上游自然响应，网关不再
// 以 413 提前拦截。历史上本值是 32MB（再早 8MB）的网关内存护栏；上游实测裁定
// 「上游的错误响应信息量更大，网关提前 413 反而挡住上游真实行为」，本仓跟进默认
// 行为但**保留配置项**（server.max_body_mb >0 时仍是一道可显式开启的护栏）——
// 删键会让既有 config.json 里的显式设置静默失效（issue #17 反复强调的失效模式）。
const DefaultMaxBodyBytes = 0

// DefaultReadTimeout 入站 body 读取窗口的兜底值（与 config 侧 Default() 同口径）。
// 单一来源供 handler 与 cmd 两侧共用，避免"配置默认 300、handler 兜底 60"这类漂移。
const DefaultReadTimeout = 300 * time.Second

// SetReadTimeout 热更新入站 body 读取窗口（面板保存配置路径调用）。
// d<=0 与 NewHandler 兜底口径一致：回落 DefaultReadTimeout（不采纳 http.Server 的
// 「0 = 不限」语义——那等于拆掉慢速 body 闸门，与旧值 60s 的防护意图相悖）。
//
// 本方法只写原子镜像，不动 http.Server.ReadTimeout（后者无并发安全 setter，
// 运行期赋值是数据竞争）；实际生效靠 armBodyReadDeadline 逐请求重设读截止。
func (h *Handler) SetReadTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultReadTimeout
	}
	h.readTimeout.Store(int64(d))
}

// readTimeoutValue 返回当前生效的入站读窗口（运行期原子值，面板热改后立即反映）。
// 兜底 DefaultReadTimeout 与 NewHandler/SetReadTimeout 同口径：即便 handler 未经
// NewHandler 装配（原子值为零值）也不会退化成"不限"。
// 不读 h.cfg.ReadTimeout：那是启动期快照，面板热改后会过期。
func (h *Handler) readTimeoutValue() time.Duration {
	if d := h.readTimeout.Load(); d > 0 {
		return time.Duration(d)
	}
	return DefaultReadTimeout
}

// SetPTLMaxTokensRetry 热更新「11115 下调 max_tokens 重试一次」开关（面板保存配置
// 路径调用，features.ptl_max_tokens_retry）。并发安全：请求路径走 atomic 读，
// 与面板保存（另一 goroutine）不构成数据竞争。
func (h *Handler) SetPTLMaxTokensRetry(v bool) { h.ptlMaxTokensRetry.Store(v) }

// ptlRetryEnabled 返回当前生效的 11115 重试开关（运行期原子值）。
// 零值（未经 NewHandler 装配）为 false——**但 NewHandler 恒按 Config 装入**，
// 而 Config.PTLMaxTokensRetry 为 nil（未配置）时取默认 **true**（见 Config 字段注释：
// 只在 11115 时生效、失败也退回首次原始错误，故默认开是安全的）。
// 不读 h.cfg.PTLMaxTokensRetry：那是启动期快照，面板热改后会过期。
func (h *Handler) ptlRetryEnabled() bool { return h.ptlMaxTokensRetry.Load() }

// armBodyReadDeadline 把本请求的读截止推到 now+read_timeout_seconds（热生效入口）。
//
// 为什么需要它（v1.9.13 生产事故的修复核心）：http.Server.ReadTimeout 覆盖的是
// 「连接建立 → body 读完」的整段窗口，且只能在启动时写死——面板改了配置也要重启
// 才生效（且 http.Server 根本没有 SetReadTimeout 这样的 setter，只有
// SetKeepAlivesEnabled；ReadTimeout 是裸字段，运行期从面板 goroutine 赋值是数据
// 竞争）。这里改用 net/http 官方的逐请求接口 ResponseController.SetReadDeadline：
// 在 handler 入口按**当前**配置值重设一次截止，于是
//   - 面板保存 read_timeout_seconds 后，下一个请求即按新值执行（热生效）；
//   - 在途请求不受影响（各自已握有自己的 deadline，改值不会回头改它们）；
//   - 该接口在 HTTP/1 与 HTTP/2 下均由标准库实现（h2 的 responseWriter 也实现了
//     SetReadDeadline），不支持时返回 http.ErrNotSupported——此时静默跳过，退回
//     http.Server.ReadTimeout 的静态值兜底，不影响请求正常处理。
//
// 刻意**不**在读完 body 后清零截止，两个原因：
//   - 不需要：HTTP/1 侧 body 一读到 EOF，标准库自己就会 startBackgroundRead →
//     SetReadDeadline(zero) 清掉（transfer.go 的 onHitEOF 钩子），所以长 SSE 响应
//     与 keep-alive 空闲不受本项影响；h2 侧该截止只作用于**请求体**（触发时
//     CloseWithError(ErrDeadlineExceeded)），不碰响应流。
//   - 反向风险：413/读失败等"没读完 body"的早退路径，handler 返回后标准库还要在
//     finishRequest 里 drain 最多 256KB 以便复用连接；若此处清零，慢速客户端可让
//     该 drain 无限阻塞。留着截止反而把它框在同一窗口内（超时即关连接）。
//
// 语义边界：只放宽/收紧「读 body」这段。ReadHeaderTimeout=30s 不动（头很小，30s
// 足够，仍是慢速头攻击的有效闸门）；出站方向超时在 upstream 段，与本项无关。
func (h *Handler) armBodyReadDeadline(w http.ResponseWriter, r *http.Request) {
	// 无 body（GET/HEAD、Content-Length: 0）时无需重设：标准库在 handler 入口前已
	// 调 startBackgroundRead 把读截止清零，此刻再设只会把 keep-alive 空闲等待
	// 拉长到 read_timeout（与 IdleTimeout 的职责重叠）。
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}
	// 失败（ErrNotSupported：自定义 ResponseWriter 包装层未透出 SetReadDeadline）
	// 不阻断请求，退回静态 ReadTimeout 兜底。
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.readTimeoutValue()))
}

// rotateLimit 返回当前生效的换号次数（运行期原子值，面板热改后立即反映）。
// 兜底 3 与 NewHandler/SetMaxRotate 同口径：即便 handler 未经 NewHandler 装配
// （原子值为零值）也保证 >=1——否则轮转循环一次都不进，请求直接 503。
// 不读 h.cfg.MaxRotate：那是启动期快照，面板热改后会过期。
func (h *Handler) rotateLimit() int {
	if n := h.maxRotate.Load(); n > 0 {
		return int(n)
	}
	return 3
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（无上游重置时间时按有界退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.MaxBodyBytes < 0 {
		cfg.MaxBodyBytes = 0 // 负值按"不预拦截"处理（见 SetMaxBodyBytes 注释）
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes // 0 = 不预拦截（与 config 侧 Default() 同口径）
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = DefaultReadTimeout // 入站读窗口兜底 300s
	}
	var hasRealm func(string) bool
	if cfg.Pool != nil {
		hasRealm = cfg.Pool.HasRealm
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.router = RealmRouter{
		GlobalEnabled: cfg.GlobalEnabled,
		Precedence:    cfg.RealmPrecedence,
		HasRealm:      hasRealm,
	}
	h.maxBodyBytes.Store(cfg.MaxBodyBytes)
	h.maxRotate.Store(int64(cfg.MaxRotate))
	h.readTimeout.Store(int64(cfg.ReadTimeout))
	// 11115 下调 max_tokens 重试开关：nil（未配置/测试裸用）= 默认 true
	// （只在 11115 时生效、失败退回首次原文，故默认开安全；显式 &false 关闭）。
	h.ptlMaxTokensRetry.Store(cfg.PTLMaxTokensRetry == nil || *cfg.PTLMaxTokensRetry)
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now()}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// reserve_credits 保留积分的**生效值**（0 = 关闭）：运维据此确认闸门当前是否
		// 生效、线在哪，不必翻 config.json（面板热改后 config.json 与内存值的一致性
		// 本身也需要一次确认）。对齐上游 credit_floor 的 /status 透出。
		// 只新增字段，既有键不变（零回归）。
		"reserve_credits": h.cfg.Pool.ReserveCredits(),
		"model_locks":     h.cfg.Pool.ModelLockView(),
	})
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// staticCNModelIDs 静态 CN 模型名表（api-reference §5，动态接口失败时的回退）。
//
// 只存模型名：窗口 / 输出上限由 upstream.context_catalog 知识表按 id 补真值
// （2026-09-17 去 1M 编造）。旧实现给全表硬编码 context_length=131072——那是假值，
// 会让 Codex/ZCode 等按 context_length 决策的客户端提前截断、白白丢上下文。
var staticCNModelIDs = []string{
	"glm-5.2",
	"glm-5.1",
	"glm-5v-turbo",
	"kimi-k2.7",
	"minimax-m3",
	"hy3",
	"hy3-preview",
	"hy3-preview-agent",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
}

// staticCNModels 静态 CN 表条目：走与动态分支同一条 modelEntry 渲染路径，
// 窗口 / 输出上限取自知识表（未收录则省略字段，不编造）。
func staticCNModels() []map[string]any {
	out := make([]map[string]any, 0, len(staticCNModelIDs))
	for _, id := range staticCNModelIDs {
		out = append(out, modelEntry("", upstream.ModelInfo{ID: id}))
	}
	return out
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	// dynamicModelsTTL 模型目录缓存时长（吸收上游 79bc5af）。曾是 1h；缩到 10min
	// 对齐「面板实时、API 缓存」的漂移痛点（上游 PR #38 报告）：目录新增模型时
	// 面板立即可见，公开 /v1/models 最多滞后一个 TTL。再短就不值得——每次失效
	// 都是 2 次上游探测（企业端点 + /v3/config）。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 10min，见 dynamicModelsTTL），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelEntry 把上游 ModelInfo 包装成 OpenAI /v1/models 条目（CN 与 global 共用）。
//
// 共用同一函数是刻意的：两侧字段集合必须一致，否则一方新增能力（如窗口）时另一方会静默缺失
// —— global 分支漏 context_length 正是这么来的。任何字段增删都只在此处发生。
//
// prefix 为 id 的域前缀；本仓的域前缀统一在 realmEntry 合并阶段按
// models.strip_realm_prefix 施加（见 modelList），故两处调用都传 ""（裸 id 才能跨域去重）。
//
// context_length / max_output_tokens 走 upstream.context_catalog 两级查找
// （上游动态值权威 → 知识表），**未知即省略字段**：旧实现在这里兜底 131072 是编造值，
// 会让按 context_length 决策的客户端（Codex/ZCode 等）提前截断、白白丢上下文。
func modelEntry(prefix string, mi upstream.ModelInfo) map[string]any {
	entry := map[string]any{
		"id":       prefix + mi.ID,
		"object":   "model",
		"created":  1753600000,
		"owned_by": "workbuddy",
	}
	if ctx, ok := upstream.ContextWindowListing(mi.ID, mi.ContextWindow); ok {
		entry["context_length"] = ctx
	}
	if mo, ok := upstream.MaxOutputTokensListing(mi.ID, mi.MaxTokens); ok {
		entry["max_output_tokens"] = mo
	}
	if len(mi.Efforts) > 0 {
		entry["supported_efforts"] = mi.Efforts
	}
	if mi.DefaultEffort != "" {
		entry["default_effort"] = mi.DefaultEffort
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = mi.SupportsReasoning
		entry["can_disable_thinking"] = mi.CanDisableThinking
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // P1：多模态能力透出
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits
	}
	return entry
}

// globalModels 国际版（global realm）模型名静态名单（PLAN §7.2 附录 21 名）。
// 只含模型名、不含倍率与元数据：无 global 账号 / 探测失败（5min 负缓存）/
// GlobalEnabled=false 时以此兜底输出（窗口 / 输出上限由 context_catalog 知识表按 id 补齐）；
// 探测成功时以探测结果为基底，本名单中探测未返回的 id 才按 id 补齐
// （见 upstream.mergeGlobalModelInfos）。
var globalModels = upstream.GlobalModelNames

// modelList 模型列表：只列「池内确实有账号的域」，缺省输出裸模型名
// （config models.strip_realm_prefix 缺省 true）。显式 "cn:"/"global:" 前缀在入站
// 方向仍被解析（老客户端配置零改动）。
//
// 单域部署（只登国际版账号）结果 = 国际版名单 + 无前缀 + 零 CN 上游调用。
// 双域都有账号时两域名单合并去重，同名条目按 realm_precedence 先入（展示的是
// 真正会接这个请求的那一份，与 router 口径一致）。
//
// 两域条目共用 modelEntry（字段集合不再漂移）：context_length / max_output_tokens
// 上游动态值权威、零值时查知识表、未知省略；supported_efforts / default_effort
// 透出上游实际能力，未知时省略字段（客户端按自身默认处理）。
func (h *Handler) modelList() []map[string]any {
	cnOn := h.realmAvailable("cn")
	glOn := h.realmAvailable("global")
	both := !cnOn && !glOn // 池内无账号：列双域静态兜底，避免空列表（零上游调用）

	type realmEntry struct {
		realm string
		entry map[string]any
	}
	var cnEntries, glEntries []realmEntry

	if cnOn || both {
		var infos []upstream.ModelInfo
		if cnOn {
			infos = h.fetchDynamicModels() // 只在有 CN 账号时才打上游
		}
		infos = h.cfg.HiddenModels.FilterInfo(infos)
		if len(infos) > 0 {
			for _, mi := range infos {
				cnEntries = append(cnEntries, realmEntry{"cn", modelEntry("", mi)})
			}
		} else {
			// 静态兜底：元数据留空，窗口 / 输出上限由知识表按 id 补真值。
			for _, e := range staticCNModels() {
				if id, _ := e["id"].(string); h.cfg.HiddenModels.Has(id) {
					continue
				}
				cnEntries = append(cnEntries, realmEntry{"cn", e})
			}
		}
	}

	// global 模型名单：仅在有 global 账号且 GlobalEnabled=true 时列出（逃生门）。
	// 条目 = 探测结果（带窗口 / 能力元数据）∪ 静态独有 id（fetchGlobalModelInfos 内合并）；
	// 无 global 账号时该分支整体不进（both 场景下返回静态名单，仍零上游调用）。
	//
	// 写死条目（PinnedModels）命中时改用其完整快照，避免"列表里有 deepseek、
	// 但倍率/窗口全空"的半截投影。
	pinnedByID := make(map[string]upstream.PinnedModel, len(h.cfg.PinnedModels))
	for _, pm := range h.cfg.PinnedModels {
		if pm.ID != "" {
			pinnedByID[pm.ID] = pm
		}
	}
	if glOn || both {
		for _, mi := range h.cfg.HiddenModels.FilterInfo(h.fetchGlobalModelInfos()) {
			entry := modelEntry("", mi)
			if pm, ok := pinnedByID[mi.ID]; ok {
				entry = pm.Entry() // 完整快照覆盖（含倍率 / 档位）
			}
			glEntries = append(glEntries, realmEntry{"global", entry})
		}
	}

	// 优先域先入：同名条目保留优先域那一份，另一域的重复项丢弃。
	first, second := cnEntries, glEntries
	if h.router.BareRealm() == "global" {
		first, second = glEntries, cnEntries
	}
	out := make([]map[string]any, 0, len(cnEntries)+len(glEntries)+len(h.cfg.PinnedModels))
	seen := make(map[string]bool, len(cnEntries)+len(glEntries)+len(h.cfg.PinnedModels))
	for _, group := range [2][]realmEntry{first, second} {
		for _, re := range group {
			id, _ := re.entry["id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			if !h.cfg.StripRealmPrefix {
				// 保留历史协议：id 标回所属域前缀。
				e := make(map[string]any, len(re.entry))
				for k, v := range re.entry {
					e[k] = v
				}
				e["id"] = re.realm + ":" + id
				out = append(out, e)
				continue
			}
			out = append(out, re.entry)
		}
	}
	// 写死条目兜底：上游目录没给的模型（账号差异/灰度）也列出来。上游给了同名条目时
	// seen 已命中，这里自动让位——上游数据永远优先。隐藏名单同样作用于写死条目
	// （想藏掉就把它加进 models.hidden_models）。
	for _, pm := range h.cfg.PinnedModels {
		if pm.ID == "" || seen[pm.ID] || h.cfg.HiddenModels.Has(pm.ID) {
			continue
		}
		seen[pm.ID] = true
		e := pm.Entry()
		if !h.cfg.StripRealmPrefix {
			e["id"] = h.router.BareRealm() + ":" + pm.ID
		}
		out = append(out, e)
	}
	return out
}

// realmAvailable 报告该域是否参与对外模型列表与裸名默认域。
// 判据是「池内有没有这个域的账号」（pool.HasRealm，不看健康度）：账号全在冷却时
// 列表与默认域不应突然翻转。global 另受 GlobalEnabled 逃生门约束。
func (h *Handler) realmAvailable(realm string) bool {
	if h.cfg.Pool == nil {
		return false
	}
	if realm == "global" && !h.cfg.GlobalEnabled {
		return false
	}
	return h.cfg.Pool.HasRealm(realm)
}

// fetchGlobalModelInfos 拉 global realm 模型目录（探测 ∪ 静态独有 id，1h 缓存 +
// 5min 负缓存），返回带窗口 / 能力元数据的条目。GlobalEnabled=false 时 modelList 已不
// 进入本分支（逃生门在调用方 gate）。
//
// global 侧 TTL 维持 1h（不随 CN 目录一起缩到 10min）：同一份缓存同时喂 /v1/stats
// 的倍率只读快照（GlobalModelInfosSnapshot），统计端点被面板高频轮询——缩短 TTL
// 会让高频轮询反复触发上游探测，与「统计端点零上游压力」的设计相悖。
func (h *Handler) fetchGlobalModelInfos() []upstream.ModelInfo {
	// 元数据路径（PickExcludingForRealmMeta）：本调用不消费积分，故豁免保留积分闸门
	// ——否则池内账号全部触底时 global 目录退回静态名单（窗口/能力全空），恰好发生在
	// 用户最需要看清"还剩什么免费模型"的时刻。理由与取舍见 internal/pool/reserve.go 文件头。
	acct := h.cfg.Pool.PickExcludingForRealmMeta(nil, "global")
	if acct == nil {
		// 无 global 账号：输出静态名单（仅 ID，元数据留空），零上游调用。
		out := make([]upstream.ModelInfo, 0, len(globalModels))
		for _, id := range globalModels {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, upstream.ModelInfo{ID: id})
			}
		}
		return out
	}
	return h.cfg.Upstream.FetchGlobalModelInfos(acct)
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），
// 缓存 10min（dynamicModelsTTL，吸收上游 79bc5af；曾为 1h）。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
//
// 强制 realm=cn 选号：本表是 CN 目录（/console 家族），拿 global 账号去打
// global 域的同名路径会吃到 500/解析失败（v1.x 面板"拉取模型 500"的根因之一；
// 上游 PR #38 报告的正是无 realm 过滤的 Pool.Pick 会选中 global 号）。
//
// 缓存 + 5min 负缓存按既有语义**保留**（上游 #38 原案整体删除缓存被拒）：公开端点
// 逐请求实时拉取 = 每次 2 个上游探测，客户端周期性刷新模型列表会持续打上游；
// 上游故障时无冷却窗口，客户端重试即放大请求量——负缓存正是为此设计；且
// cachedModelsSnapshot（gateway_hint 判定）依赖缓存写入。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	// 元数据路径（PickExcludingForRealmMeta）：豁免保留积分闸门，理由同
	// fetchGlobalModelInfos 的注释（本调用不消费积分，被闸门拦住只会让用户在最需要看
	// 模型列表时看不到列表）。
	return fetchCNCatalogAndCache(h.cfg.Upstream, h.cfg.Pool)
}

// fetchCNCatalogAndCache 拉取 CN 模型目录并写入 dynamicModelsCache（含成功/失败两条
// 缓存语义），返回本次探测结果（无可用 CN 账号或探测失败时 nil）。
//
// 为什么抽成包级函数：拉取与缓存写入的知识此前只存在于 fetchDynamicModels（handler
// 方法）里，而**启动预热**（WarmModelCatalog）需要同一份知识却拿不到 handler——
// 若在预热侧另写一遍，两处必然漂移（一处加了负缓存、另一处忘了；或一处换了选号口径、
// 另一处照旧）。这里把它收成单一事实来源，handler 与预热共用。
//
// 与 fetchDynamicModels 的分工：那个先查缓存/负缓存（懒触发的正常路径），本函数
// **不查缓存**（调用方负责判定是否需要拉）——预热正是"缓存冷才拉"的场景。
func fetchCNCatalogAndCache(up *upstream.Client, p *pool.Pool) []upstream.ModelInfo {
	if up == nil || p == nil {
		return nil
	}
	acct := p.PickExcludingForRealmMeta(nil, "cn")
	if acct == nil {
		return nil
	}
	infos, err := up.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 会跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（与 fetchDynamicModels 的差异点，见 hintContext 注释：
// 错误路径加一次 FetchModels 会放大请求量）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

// cachedCatalogSnapshot 只读模型目录缓存，但**不判 TTL**：返回最近一次成功目录
// （含已过 10min TTL 的陈旧快照）；从未成功拉取过 → nil。
//
// 为什么需要它与 cachedModelsSnapshot 并存（两者服务的目标不同，不是重复）：
//   - cachedModelsSnapshot 服务 /v1/models 与 gateway_hint：那里的 TTL 是刻意的取舍
//     （「面板实时、API 缓存」——目录新增模型时面板立即可见、公开端点最多滞后一个
//     TTL），过期即视为"没有目录"，由调用方回落静态表；
//   - 本函数服务**保留积分的免费判定**（server.freeModelLookup）：那里的问题是
//     "这个模型此刻要不要花钱"，判错的方向是**误拦**（把免费模型当收费拦掉，
//     用户的免费模型用不了 = 本功能要修的病灶）。而 /v1/models 的唯一调用方是客户端，
//     绝大多数客户端只在启动时拉一次——启动 10 分钟后 CN 快照恒为 nil，免费判定
//     随之把 CN 免费模型全误拦，且**每 10 分钟复发**，比一次性的启动空窗期更糟。
//
// 上游的对应取舍（a4557dc / ModelRate）：倍率表**跨刷新持久**——每次目录刷新整体替换，
// 但读时不判过期。陈旧倍率的错判方向是"按上一轮牌价判收费"，远好于"因为没数据而
// 把一切当收费"（后者让保底在目录刷新空档期整体失效）。本函数对齐同一方向。
//
// 只读、零上游调用（与 cachedModelsSnapshot 同一纪律：本函数在选号热路径上被调用）。
func cachedCatalogSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	return dynamicModelsCache.ids
}

// bodyTooLargeMsg 413 的客户端文案（独立函数便于测试直接盯住口径，不必构造
// 超限请求体）。**仅在显式配置了护栏（max_body_mb > 0）时可达**——默认不预拦截
// （吸收上游 73fe1f8），故文案不必再解释"默认多少"，而要说清"这是你自己配的"。
//
// 口径（v1.9.17 修正，2026-09 跟进上游后再调）：
//   - 旧文案「请求体超过 N MB 上限」容易被读成**上游**的限制，用户据此去压图片或
//     以为模型不支持，方向全错——故保留「网关侧护栏、不是上游限制」的措辞；
//   - 默认已改为**不预拦截**，收到 413 只可能是运维自己配了 server.max_body_mb，
//     故文案给的是**关闭方式**（置 0）而不是"默认 32 MB"；
//   - 常见成因（多图会话每轮 base64 重发历史图片）保留，指向"调大或关闭"两条路。
//
// 注意 "37%%" 的转义：本串是 fmt 格式串，字面百分号必须写成 %%，否则 "%），" 会被
// 当成动词渲染成 "%!)(MISSING)"（go vet 会报 unknown verb，但 go build 不报——
// 改文案时务必跑 vet）。
func bodyTooLargeMsg(limit int64) string {
	return fmt.Sprintf("请求体超过网关内存护栏 %d MB：这是网关侧上限（防止单请求吃爆进程内存），不是上游限制；网关默认不设该上限（server.max_body_mb=0 = 不预拦截，大请求交上游自然响应），当前值由运维显式配置——可在面板「请求体上限」调大或置 0 关闭（保存即时生效）。多图会话易触发：历史图片每轮以 base64 重发（膨胀约 37%%），请压缩图片或调大上限后重试", limit>>20)
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 入站 body 读窗口按**当前**配置值逐请求重设（面板改 server.read_timeout_seconds
	// 后无需重启即生效；慢链路大上下文不再被启动时的静态值掐断）。必须在读 body 之前。
	h.armBodyReadDeadline(w, r)
	// 响应写出跟踪（**显式守卫**，见下方 ErrPromptTooLong 的 11115 重试分支）：11115
	// 下调 max_tokens 的重试必须发生在「还没向客户端写出任何字节」的时刻，否则会出现
	// 「头已发出（200/其它状态码）却想改错误响应」的形态。本包装记录 WriteHeader/Write
	// 是否被调用过，供重试分支断言；包装透传 Flush（SSE 逐帧 flush 依赖）与 Unwrap
	// （ResponseController 走它找 SetReadDeadline 等能力），对既有路径零行为变化。
	// 必须放在 armBodyReadDeadline **之后**：那一步要拿原始 w 探测 SetReadDeadline。
	wt := &respWriteTracker{ResponseWriter: w}
	w = wt
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体读取：limit > 0 时按「内存护栏」预拦截（LimitReader 读 limit+1 探测超限，
	// 超限 413，不把截断的半截 JSON 喂上游——issue #41）；limit <= 0（默认，吸收上游
	// 73fe1f8）时**无上限直读**，超限类问题交由上游自然响应（其响应信息量更大，
	// 能看到上游到底是什么策略）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。文案口径见 bodyTooLargeMsg。
	//
	// 两种模式下读错误路径的语义都保留：#41 的截断防御——读 body 出错就地 400，
	// 不把半截 JSON 喂上游 unmarshal（那会让上游报 unexpected EOF，网关却冤枉罚号）。
	limit := h.maxBodyBytes.Load()
	var body []byte
	var err error
	if limit > 0 {
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
	} else {
		body, err = io.ReadAll(r.Body)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", readBodyErrMsg(err, h.readTimeoutValue()))
		return
	}
	if limit > 0 && int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", bodyTooLargeMsg(limit))
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// realm 归属解析：model 名可带显式 "[realm:]" 前缀（老客户端配置兼容）；
	// 裸名按池内可用域归属——单域部署（只登国际版账号）下裸名直接走该域，
	// 多域部署按 realm_precedence。bareModel 用于选号/粘性/出站 body 重写。
	//
	// realmExplicit 区分归属来源（issue #199c 跨域回落的开关）：显式前缀 = 用户强指定，
	// 本域无可用号时**不跨域回落**（换域可能违反其意图）；裸名归属 = 网关默认倾向，
	// 本域无可用号时回落另一域，避免混合池下「本域全限流即 503」而另一域明明可用。
	realm, bareModel, realmExplicit := h.router.ResolveWithSource(peek.Model)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
	}
	defer st.done()

	tried := map[string]bool{}
	// 请求级错误去重（见 errDedup）：本请求内已喂过连败的错误指纹。ErrClient
	// （未知 4xx）分支继续轮转，若不去重，一个请求级 4xx 会在 N 个账号上各记一次
	// 连败——一个请求就能把整池推向降权阈值（与 11135 修复前的自伤面同形）。
	errDedup := newErrDedup()
	// 11115 下调 max_tokens 重试的请求级状态（features.ptl_max_tokens_retry）：
	//   - ptlRetried：**只重试一次**的闸门（第二次 11115 直接回错误，不循环）；
	//   - ptlRetryUID：重试要**钉住的账号**（首次失败的那个号）——普通选号是加权随机 +
	//     LRU 兜底，失败号刚被用过会被判「不最旧」而落到别的号；而本项要求重试与首次
	//     是**同一账号**（否则「上游把 max_tokens 计入同一上限口径」这个变量都不可控，
	//     结论不可用）。空串 = 无重试意图，走既有选号。
	//   - ptlFirstBody/ptlFirstHint：首次 11115 的原文与 hint（重试失败时逐字退回它，
	//     保证客户端看到的报错与不加本项时完全一致）。
	var (
		ptlRetried   bool
		ptlRetryUID  string
		ptlFirstBody string
		ptlFirstHint string
	)
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	sessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，换
	// user 消息换键）。#170 起**带会话键的客户端也统一走轮级**（吸收上游 03ce06d 的
	// #170 项，上游原始提交 b9ac0d3）：X-Conversation-Request-ID 是上游后台的轮级
	// 聚合主键，官方桌面 CLI 每次 USER_PROMPT_SUBMIT 清空重生成——同轮复用、跨轮必换。
	// 此前带会话键的客户端走 RequestIDForKey(sessKey) 会话级聚合（跨轮同值，继承自
	// merge-base 的上游旧形态），会把整段会话几十轮并成一条记录、每轮明细丢失。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理——下方 prompt.Rewrite / rewriteModel 会动 body，之后取会让形态漂移）。
	// 11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// recordAttempt 记录一次实际发起的账号尝试（唯一汇聚点）。
	//
	// obs 携带统计端点专用的观测（缓存三段 / 真实扣费 / 首字延迟 / 是否流式），
	// 与 delta 一样来自上游 usage，但目标字段集不同：delta 喂 pool 的账号级账本
	// （token/延迟/吞吐），obs 喂 usage 记录器的模型级统计。
	//
	// 单一埋点：成功、各类错误、传输失败、解析失败全都汇到这里，因此 /v1/stats
	// 天然覆盖全路径（含失败尝试——重试放大只能靠这一列看出来），不需要在每个
	// return 前重复记账（那反而会漏分支或双计）。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, obs attemptObs, started time.Time) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = peek.Model
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs
		if delta.HasCompletionTokens && delta.CompletionTokens >= 0 && latencyMs > 0 {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = float64(delta.CompletionTokens) * 1000 / float64(latencyMs)
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 保留积分的余额插值（pool.reserve_credits）：把本次**实测扣费**立刻反映到
		// 账号观测余额上，堵住"签到 / 余额刷新之间（默认 5 分钟一轮）余额不降"的空窗
		// ——贵模型一笔能扣上百分，空窗期内触底的号仍按旧余额参与选号，用户设的底线
		// 会被花掉（上游 credit_floor 用同一口径：签到权威值 − 每笔 usage.credit 实扣，
		// 只会偏低不会偏高，是保底需要的安全方向）。
		//
		// 只在 HasCredit 时调用：**usage.credit 缺失 ≠ 0 成本**（上游部分响应不带该
		// 字段），把"缺失"当 0 会让保底漏放、当"有消耗"会让免费请求凭空扣余额。
		// 缺失时不动余额——下一轮余额刷新会以权威值覆盖，方向安全。
		if obs.HasCredit {
			st.credit += obs.Credit
			st.hasCredit = true
			h.cfg.Pool.NoteConsumedCredits(uid, obs.Credit)
		}

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			// 生成时长 = 端到端 − 首字（纯生成时间），供 tokens/s 折算。首字缺失
			// （非流式）时退回端到端：那本来就是「整段耗时」，不该凭空扣一个 0。
			// 只在有 completion 观测时计入分母——失败尝试的耗时不该摊薄吞吐
			// （分母涨、分子不涨，算出来的 tokens/s 会系统性偏低）。
			genMs := latencyMs
			if obs.HasTTFB && obs.TTFB > 0 {
				if g := latencyMs - obs.TTFB.Milliseconds(); g > 0 {
					genMs = g
				}
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
				CacheHit:         obs.CacheHit,
				CacheMiss:        obs.CacheMiss,
				CacheWrite:       obs.CacheWrite,
				HasCache:         obs.HasCache,
				Credit:           obs.Credit,
				HasCredit:        obs.HasCredit,
				Stream:           obs.Stream,
				TTFBMs:           obs.TTFB.Milliseconds(),
				HasTTFB:          obs.HasTTFB,
				GenerationMs:     genMs,
				HasGenerationMs:  delta.HasCompletionTokens && delta.CompletionTokens > 0,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough 非降级期：透传客户端原始 system（不改写）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     **轮级**生成（#170 统一轮级，吸收上游 03ce06d）：带会话键客户端走
	//     sessKey:turnKey 复合键（会话段入键防跨会话同轮文本互撞），无会话键走纯
	//     turnKey；turnKey 空态（无 user 消息/无可签名内容）回落会话级，都空则请求级
	//     随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：会话段入键防不同会话的同轮文本共用聚合键。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有语义不变，存量键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合好于请求级随机。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// 换号上限本请求内固定一次（与 maxBodyBytes 同口径的"请求内快照"）：
	// 轮转中途面板改值不影响本请求已定的次数，避免同请求内上限漂移。
	maxRotate := h.rotateLimit()
	for i := 0; i < maxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		// 11115 下调 max_tokens 的重试：**钉住首次那个账号**（见 ptlRetryUID 注释）。
		// 走 PickByUIDForModel 而非 PickByUID：与粘性路径同口径（该号在当前模型被 6004
		// 限额/占满在途时不选中）。
		if ptlRetryUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(ptlRetryUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 钉住的账号在此期间不可用（在途占满/被 6004 限额/被禁用/跨域不符）→
				// **放弃重试，直接回首次的 11115 原文**。刻意不回落普通轮换：本项的重试
				// 语义是「同一账号」（换号则「上游按账号口径把 max_tokens 计入上限」这个
				// 变量不可控，结论不可用），而且把请求放大到别的健康号上正是 11115 分支
				// 要避免的白耗配额（见该分支注释）。
				// 可达性：ptlRetryUID 非空 ⇒ 本请求已走过一次重试计划（ptlFirstBody/
				// ptlFirstHint 均已填），此处直接用它回错误。
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long",
					promptTooLongMessage(ptlFirstBody), ptlFirstHint)
				st.status = http.StatusBadRequest
				return
			}
		}
		if acct == nil && stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑，
				// 本次回落普通轮换。
				// 粘性号校验**不做跨域放宽**（issue #199c 只改普通轮转）：粘性号的 realm 不符
				// 说明该会话被绑到了另一域的号（如绑定时走的是显式前缀请求），继续用它等于
				// 无视本次请求的域归属；解绑后由下面的普通轮转按软优先/回落重新分配。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			// 裸名归属（realmExplicit=false）走软优先入口：本域无候选（含 6004 模型级
			// 冷却在 healthyForModel 里过滤掉本域全部号的情形）时回落另一域再选一次
			// ——混合池 1 global + 1 cn 且 realm_precedence=global 时，global 号对该模型
			// 429/6004 后第二轮选号不再无候选 503，而是落到 cn 号。显式前缀是用户强指定，
			// 用硬过滤入口，不跨域。
			if realmExplicit {
				acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
			} else {
				acct = h.cfg.Pool.PickExcludingForRealmFallback(tried, bareModel, realm)
			}
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		// 同步昵称：流水行只写 uid8 时无法直观看是哪个号，昵称随本次选号带入日志行
		// （昵称认人、uid8 供 grep，见 logfmt.Label）。轮转换号时随之覆盖为最终成功号。
		st.nick = acct.NicknameValue()
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 12153 口径统一（本批）：走阈值版 NoteSessionDead，与
					// applyErrorPolicy 的 ErrSessionDead 分支、scheduler 的 keepalive
					// 路径完全一致。旧实现此处单次 Disable 且用了**第三个** reason
					// 文案（"refresh session dead"），与另两条路径既不同处置也不同文案
					// ——同一个 code 在三条路径上三种行为，且单次即禁会永久摘掉健康号
					// （见 state.go 顶部「13 个 disabled 号全是误判受害者」）。
					// reason 统一为 pool 的 sessionDeadReason（12153 session dead）。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避，WAF P0-2）
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 11115 重试的钉号**一次性**：本次出站已经用过它（ptlRetried=true 说明刚才是
		// 重试那一次），后续轮转不该再钉住同一个号——否则该号连续失败时会在后续每轮
		// 被反复选中，既浪费名额又偏离「只重试一次」的语义。首次尝试时 ptlRetried 为
		// false，此处是空操作。
		if ptlRetried {
			ptlRetryUID = ""
		}
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析，见 ChatStreamContext 注释）。传输层错误（非 *Error）走
		// 抖动换号分支；防御分支（terr 为 nil 但 status>=400，如 ErrNone 兜底）回落
		// 本地 Classify，双保险不改变语义。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**（吸收上游 PR #93 的 isUpstreamTimeout）。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，只会把
			// 客户端拖到 MaxRotate × header_timeout，期间还给一串健康号白喂连败计数。
			// 此前全仓没有任何超时识别，超时和「网络抖动」共用同一条换号路径（见下方
			// IsTransient 分支的 i/o timeout 归类）。
			//
			// 判定三态（isUpstreamTimeout）：
			//   1. net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout / ETIMEDOUT）；
			//   2. 显式 deadline（context.DeadlineExceeded / os.ErrDeadlineExceeded）；
			//   3. **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流
			//      （见 upstream/idle.go），也就是上游停滞。客户端主动断连时
			//      r.Context().Err() != nil，走下面的抖动分支（人已走，语义不同）。
			//
			// 判定优先于 IsTransient：i/o timeout 同时命中 IsTransient 的抖动词表，但
			// 超时换号注定白换（同一份请求撞同一个慢上游），必须先止损。
			//
			// **有意让 MaxRotate 在超时场景失效**：这里直接 break（终止轮转），不再
			// 消耗换号名额。超时是上游/链路问题，换号无用——把它当作「本请求的一次性
			// 终态」比按 MaxRotate 重复打上游更省资源，也让客户端更快拿到可区分的
			// upstream_timeout 文案（末端分支）。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptObs{Stream: peek.Stream}, attemptStarted)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout uid=%s: %v (rotation stopped, account not penalized)",
					uidPrefix(acct.UID), terr)
				break
			}
			// 传输层错误（非 *Error 信封）：**只换号退避重试，不喂熔断**——传输层错误
			// 对连续失败连坐熔断过于严苛（既有语义）。
			//
			// 连败兜底（issue #114）按**抖动判定**分流（本项修复点）：
			//   - IsTransient=true（unexpected EOF / connection reset /
			//     TLS 握手失败 / DNS 失败 / 连接被本地回收 / CONNECT 隧道 502·503·504）：
			//     这是**出口链路**条件，同一链路对池内全部账号一视同仁——一个请求里 N 个
			//     账号全中恰恰证明根因不在账号上。若照旧每个账号各喂一次连败，达阈（默认 5）
			//     即全体降权 10 分钟 → 用户看到「503 all accounts unavailable
			//     (cooling/disabled)」。形态与刚修的 11135「一张坏图拖垮整个账号池」完全
			//     相同（见 handler_image_terminal_test.go），故同待遇：不喂连败、不记惩罚，
			//     只 fail（释放租约）+ 退避后换号。上游参照 hub f7bf4e2 is_transient()：
			//     换号退避重试、不记冷却，抖动熬过重试后如实抛 502（本项仍回 503 +
			//     lastErr，末端错误透传语义不变）。
			//     注意：i/o timeout 虽在 IsTransient 词表里，但已被上方 isUpstreamTimeout
			//     提前截走（超时不换号）——此分支收到的只剩真正的链路抖动。
			//   - IsTransient=false（含**未知错误串**与裸 io.EOF）：保持既有语义继续喂连败
			//     ——**保守优先**（取舍见 upstream.IsTransient 注释）：宁可误罚也不能让真
			//     故障账号永远留在池内（漏判真故障 = 坏号持续被选中、白耗健康号配额；
			//     误罚 = 一个连败计数，成功一次即清零回池）。**客户端断连**（ctx 取消）
			//     同样落在此侧：它也不是链路抖动，本项不改其既有语义（且紧随其后的
			//     rotateBackoff 见 ctx 已取消即终止轮转，只会喂到这一条计数）。
			//
			// 退避与重试上限**沿用既有** rotateBackoff + MaxRotate：本项不自创第二套重试
			// 策略（池内 3 个号都连不上时轮转 3 次即止，不会无限重试）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptObs{Stream: peek.Stream}, attemptStarted)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			if upstream.IsTransient(terr) {
				log.Printf("WARN: [server] transient upstream error uid=%s err=%v (no consecutive-fail penalty)",
					uidPrefix(acct.UID), terr)
			} else {
				h.cfg.Pool.NoteFailures(acct.UID)
			}
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避，WAF P0-2）
			}
			continue
		}
		if status >= 400 {
			// 失败尝试没有 usage：只记「这次尝试发生过（含流式与否）」，token/缓存
			// 一律不累加——缺失 ≠ 0，累加零值会把失败伪装成「测得 0 token」。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptObs{Stream: peek.Stream}, attemptStarted)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**、不暴露账号/冷却/上游错误码
				// （此前会落到 503 no_healthy_account + lastErr 泄露 11128 与账号语义）。
				// 不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError），但 content_blocked
				// 是本请求的终态——换任何账号都会撞同一审核，轮转纯属浪费时间。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr, errDedup)
				fail(acct.UID)
				msg := upstream.ContentBlockedClientMessage(string(respBody))
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：按错误里的真实数字下调 max_tokens 重试一次
			// （features.ptl_max_tokens_retry，默认开）；算不出/不适用则保持既有行为
			// ——立即透传上游原文回客户端，**不罚号不轮转**（上下文超限是请求的问题，
			// 同一 body 换任何号都超限，白扔健康号配额；与内容策略拦截同哲学：确定与
			// 账号无关的错误直接终止轮转）。
			//
			// 重试前提（三条，缺一不重试）：
			//   1. 开关开（面板可关，默认 true）；
			//   2. 本请求**还没重试过**（ptlRetried，只重试一次，防无限循环）；
			//   3. 上游原文能解析出 `<n> tokens > <limit> maximum` 且 n>limit，
			//      且请求体带 max_tokens/maxOutputTokens、下调后 ≥ 下限（1024）
			//      ——全部判定在 upstream.ParsePromptTooLongOvershoot /
			//      upstream.DowngradeMaxTokens 里（单一事实来源，含余量规则与注释）。
			//
			// 为什么安全（默认开的前提）：本项是**兜底**而非结论——「上游把 max_tokens
			// 也算进上下文上限」这一假设**未 100% 证实**（实测另有一次
			// max_tokens=1048576 + 极小输入返回 200）。故失败路径逐字退回**首次**原文
			// （见下方 ptlFirstBody 的用法），客户端看到的报错与不加本项时完全一致。
			//
			// 重试请求走与首次**完全相同**的准备管线（同一 body 变量、同一账号、同一
			// chatMeta/chatMeta 会话头、同一 clientIP），唯一差异是 max_tokens 一个字段
			// ——不轮转账号、不记惩罚（applyErrorPolicy ErrPromptTooLong 本就零动作）。
			//
			// 守卫（为什么此刻还没向客户端写出任何字节）：11115 是上游在**流式开始前**
			// 回的 400（ChatStreamContext 在 status>=400 分支读 body 后即返回，从未向 w
			// 写过一帧）；本函数内所有写响应都发生在轮转循环之后（末端错误出口）或成功
			// 分支内。故此处重试不会出现「头已发出再改状态码」的形态。保险起见仍显式
			// 断言 w 未被写过——用 ResponseController 探测写截止（未写过响应头时可用；
			// 已写过则由下面的 ptlRetried 单次闸门兜底，不依赖该探测）。
			if kind == upstream.ErrPromptTooLong {
				if h.ptlRetryEnabled() && !ptlRetried && !wt.wrote() {
					if newBody, overshoot, oldMax, newMax, ok := h.planPTLMaxTokensRetry(body, respBody); ok {
						ptlRetried = true
						// 首次原文留底：重试失败时逐字退回它（客户端报错与现在一致）。
						ptlFirstBody = string(respBody)
						ptlFirstHint = h.hintOf(upstream.ErrPromptTooLong, ptlFirstBody, bareModel, reqHasImage, uerr)
						body = newBody
						log.Printf("INFO: [server] prompt_too_long uid=%s model=%s: retrying once with lowered max_tokens=%d (was %d, overshoot=%d)",
							uidPrefix(acct.UID), bareModel, newMax, oldMax, overshoot)
						// 释放租约后**原地重试同一账号**（不换号：同一账号的配额与路由才
						// 让「上游把 max_tokens 计入同一上限口径」这个变量可控；换号则
						// 变量变多、结论不可用）。钉号走 ptlRetryUID（见该变量注释：普通
						// 选号是加权随机 + LRU 兜底，不钉就会落到别的号）；delete(tried)
						// 是必要的——下一轮选号要能重新选中同一账号（tried 已在本次选中时
						// 标记，PickExcluding 会跳过它）。
						ptlRetryUID = acct.UID
						delete(tried, acct.UID)
						releaseHeld()
						// i-- 抵消 for 的 i++：本次重试**不消耗换号名额**——重试不是「换号」，
						// 若占名额则 MaxRotate=1 的部署永远拿不到重试机会（本轮 continue
						// 后循环即结束，反而回落到 503 全池不可用，比不重试更糟）。
						// 只重试一次由 ptlRetried 闸门保证，不存在无限循环。
						i--
						continue
					}
				}
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr, errDedup)
				fail(acct.UID)
				// 重试过则退回**首次**原文（code/message/requestId 逐字一致，hint 也用
				// 首次的）；未重试（开关关/算不出/不适用）则用本次原文——两条路径的可见
				// 报错口径相同，区别只在「本请求是否多打了一次上游」。
				finalBody, finalHint := string(respBody), ""
				if ptlFirstBody != "" {
					finalBody, finalHint = ptlFirstBody, ptlFirstHint
				}
				if finalHint == "" {
					finalHint = h.hintOf(upstream.ErrPromptTooLong, finalBody, bareModel, reqHasImage, uerr)
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(finalBody), finalHint)
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11135「invalid_image_data」：请求级**终态**——图片数据是否可识别是请求的
			// 属性（同一张图换任何账号都被上游拒），与 11115/内容策略拦截同哲学：立即
			// 透传上游原文回客户端，**不罚号不轮转**。
			// 此前该形态落 Classify 的通用 4xx 兜底 ErrClient → applyErrorPolicy default
			// 分支喂连败计数（NoteFailures）+ 逐号轮转 → 每个账号各记一次连败 → 达阈
			// （默认 5）全体降权 10 分钟，即用户实测的「一张坏图换来 503 all accounts
			// unavailable (cooling/disabled)」。请求级错误的证据不指向任何单个账号，
			// 不该由账号池承担代价。
			// applyErrorPolicy ErrInvalidImage 分支零动作（不冷却/不熔断/不 NoteError、
			// 不喂连败），fail 只释放租约；error-passthrough：message 装上游 body 原文
			// （code/extError/displayMsg/actions 原样，含上游 zh 文案——上游原文是最有
			// 价值的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrInvalidImage {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr, errDedup)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_image_data", invalidImageMessage(string(respBody)),
					h.hintOf(upstream.ErrInvalidImage, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11101「请求体解析失败」（Unmarshal chat params failed / code 11101）：
			// 请求级**终态**（吸收上游 PR #99）——与 11115 / 11135 同一哲学：同一 body
			// 换任何账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到 503
			// no_healthy_account，把确定失败的请求伪装成「账号不可用、稍后再试」，
			// OpenAI 兼容客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			//
			// **11101 与 11133 的分野**（两者同归 upstream.ErrBadParams 分类，处置不同）：
			//   - 11101（IsBadParamsBody 命中）：上游在**解析请求体**阶段就拒了，还没走到
			//     模型路由 → 终止轮转。「不同账号可能有不同模型权限」是 11102
			//     （ErrModelBlocked）的理由，那里已有 (账号,模型) 负缓存避让；
			//   - 11133 model_param_invalid（IsBadParamsBody 不命中）：参数被**模型供应商**
			//     拒绝，可能是账号侧后端差异（11102 的 (账号,模型) 负缓存证明同模型在不同
			//     账号上可用性不同）→ **保留轮转**（零动作、不喂连败）。
			if kind == upstream.ErrBadParams && upstream.IsBadParamsBody(string(respBody)) {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr, errDedup)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/Status（末端映射与冷却时长共用）+ RetryAfter
			// （末端 429 映射与冷却对齐共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr, errDedup)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账（单号偶发 403
			// 仍冷却），IP 级状态只改变「是否继续轮转」——协同不叠加。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			// 429 立即换下一个号（不退避，首字延迟优先）；WAF 403 与其余分类照常退避。
			// 判据与分野理由见 backoff.go 的 backoffWorthwhile/rotateBackoffKind。
			if !rotateBackoffKind(i, r.Context(), kind) {
				break // ctx 取消：终止轮转（分类错误换号退避，WAF P0-2）
			}
			continue
		}
		// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
		// 记成健康、11102 负缓存被误清、粘性把会话钉死在它身上，后续每一轮都打同一个
		// 限流号。三件套见下方流式 default / sErr != nil 与非流式成功分支。
		//
		// markSuccess 账号侧「这一跳真成功」的三件套（记成功 + 清 11102 负缓存 +
		// 粘性跟随最终成功号），只在确认成功（或客户端断连、上游帧无恙）后调用：
		//   - 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
		//     BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004。
		//   - 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		//     若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳
		//     不再随机抽。
		// 为什么收成一个闭包：三处调用（流式真成功 / 流式客户端断连 / 非流式成功）
		// 必须逐字同口径——各写一份迟早漂移（漏一处就是「某个分支不记成功」的静默缺陷）。
		markSuccess := func() {
			h.cfg.Pool.NoteSuccess(acct.UID)
			h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集，见 WithErrorFrameObserver），
			// 用于流尾的账号处置——error 帧形态下账号**不得**被记成功。
			var errFrame string
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }))
			// 流式观测收敛（上游缺陷 → 502）：两种上游缺陷哨兵共用同一口径，
			// 与「客户端已走」严格区分——两道闸门，缺一不可：
			//   1) 哨兵闸：终结帧（error 帧/[DONE]）写失败时 Stream 返回**写错误**
			//      而不是哨兵（见 upstream.StreamHint 尾部），此处天然不命中；
			//   2) ctx 闸：客户端断连会取消入站 r.Context()，出站请求随之取消
			//      （ChatStreamContext 从 r.Context() 派生），底流 Read 返回
			//      context.Canceled——形态与「上游静默被 IdleTimeout 掐流」**同形**。
			//      仅靠写失败分辨不够（断开未被探测到时，终结帧写进内核缓冲会"成功"），
			//      故以入站 ctx 是否已取消作为权威判据：人已走 → 不标 502。
			//      注意 IdleTimeout 取消的是**出站派生 ctx**，入站 r.Context() 不受影响，
			//      所以真正的上游掐流仍照常收敛（这正是本项要抓的形态）。
			//   - IsEmptyStreamError：上游 200 但空流（0 有效帧）；
			//   - IsStreamAbortedError：上游中途断流（空闲超时掐流/半截读/连接重置），
			//     Stream 已补 error 帧 + [DONE] 终结（此前既不写终结帧也不收敛状态，
			//     客户端拿着半截流不知道结束了，运维看到假 200）。
			// 两种都在 Stream 内写完了终结帧，HTTP 头已发出只能 200；此处只收敛
			// **观测**（日志/状态列 502），与非流式 Aggregate 失败→502 upstream_parse
			// 同语义。不罚账号：流中断多是上游静默/链路问题，空流路径既有口径亦不罚
			// （与 4xx/5xx 信封错误走 applyErrorPolicy 的路径不同）。
			// markSuccess 的说明见上方（本分支与流式 default 同口径）。
			switch {
			case upstream.IsEmptyStreamError(sErr):
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream uid=%s model=%s: empty upstream stream (200+0 frames)", uidPrefix(acct.UID), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil, errDedup)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream uid=%s model=%s: upstream error frame kind=%s payload=%s",
					uidPrefix(acct.UID), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case upstream.IsStreamAbortedError(sErr):
				if r.Context().Err() != nil {
					// 客户端已走：中断是下游取消引起，不是上游缺陷——不误标 502。
					// 账号侧照常记成功（上游帧无恙，这一跳是被下游掐掉的，与 sErr != nil
					// 同语义——改前成功三件套在开流前就执行，此处保持该口径不回归）。
					log.Printf("INFO: [server] stream uid=%s model=%s: stream aborted by client disconnect (no 502)", uidPrefix(acct.UID), bareModel)
					st.outcome = reqlog.OutcomeInterrupted
					markSuccess()
					break
				}
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream uid=%s model=%s: %v (200+partial frames)", uidPrefix(acct.UID), bareModel, sErr)
			case sErr != nil:
				st.outcome = reqlog.OutcomeInterrupted
				// 客户端写失败（断连）：上游帧无恙，账号健康——账号侧照常记成功
				// （与 default 同语义），请求日志归为中断（人已走，未完成）。
				markSuccess()
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				markSuccess()
			}
			// 流式观测一并带出：缓存三段 / 真实扣费 / 首字延迟（供 /v1/stats）。
			// 与成本账本、pool 账本同源同口径（都读末帧 usage），不二次解析。
			ch, cm, cw, hasCache := stats.CacheTokens()
			cr, hasCredit := stats.Credit()
			recordAttempt(acct.UID, stats.Usage(), attemptObs{
				Stream:     true,
				TTFB:       stats.TTFB(),
				HasTTFB:    stats.HasTTFB(),
				CacheHit:   int64(ch),
				CacheMiss:  int64(cm),
				CacheWrite: int64(cw),
				HasCache:   hasCache,
				Credit:     cr,
				HasCredit:  hasCredit,
			}, attemptStarted)
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, ok := stats.Tokens(); ok {
				st.toks = toks
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptObs{Stream: peek.Stream}, attemptStarted)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), obsFromResponse(resp), attemptStarted)
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		markSuccess()
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	code := "no_healthy_account"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进「没有可用账号」——两者的排查方向完全不同（超时看上游/链路，
	// 无号看池子）。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定（11133/11135
	// 在 hint 层自带形态判定，ErrClient 家族也能带上 hint）；本地调度类错误
	// （无上游原文）固定 no_healthy_account hint。上游错误若属未覆盖形态，
	// hintOf 返回空串 → 响应不带 gateway_hint 字段（不编造）。
	hint := upstream.NoHealthyAccountHint()
	// WAF IP 级拦截措辞（fail-fast 终止路径）：轮转已止损（继续换号只会打同一出口
	// IP，加重风控），业务 code 换成 waf_ip_blocked 让客户端识别「换号无用、等窗口」。
	// 透传语义（原文优先）：有上游 body 时 message 装原文（排障必需，不拼接本地
	// 前缀）；空体（WAF 拦截页常见形态）才用本地可读文案兜底。单号偶发 403（IP 门
	// 未激活）保持 no_healthy_account 通用文案不变。
	var ue *upstream.Error
	wafTerminal := false
	if errors.As(lastErr, &ue) {
		// 上游错误：hint 按 Kind + 上游原文 + 请求形态判定（upstream.GatewayHint
		// 单一事实来源）。11133/11135 形态判定在 hint 层自带，ErrClient 家族也
		// 可能带上 hint；未覆盖形态（ErrServer/ErrNotFound/ErrBadParams/ErrNone）
		// 返回空串 → 字段缺席（不编造）。
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		if ue.Kind == upstream.ErrWafBlock && h.wafIP.active() {
			wafTerminal = true
			code = "waf_ip_blocked"
			if s := strings.TrimSpace(ue.Msg); s != "" {
				msg = s
			} else {
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
	}
	// 末端 429 + Retry-After（上游 da22a92 语义，本 fork 收窄实现）：轮转耗尽且池内
	// **所有候选都因该模型的限流冷却而出局**时，回 429 + Retry-After，而不是 503
	// no_healthy_account——「模型被限流」与「池子空了」是两回事，OpenAI 生态客户端对
	// 429 的退避更规范（503 通常被当成服务端故障，会触发不恰当的重试/熔断）。
	//
	// 判定与取值全在 pool.ModelRateLimitExhausted（单一事实来源，含 11102 排除与
	// 池真空反向判据）；此处只做出口映射。scope 选择：显式 "cn:"/"global:" 前缀是
	// 用户强指定（选号走硬过滤、不跨域），按该域判定；裸名归属时网关自己会在本域
	// 无候选时跨域回落（PickExcludingForRealmFallback），故按全池判定——否则会把
	// 「另一域同样被限」错报成「本域不可用」而漏掉 429。
	//
	// WAF IP 级 fail-fast 优先：那是更具体的终态信号（换号/重试都无意义，等窗口），
	// 语义上不该被 429 覆盖（账号级冷却本就不会写出模型级条目，此处只是显式排他）。
	status := http.StatusServiceUnavailable
	retryAfter := 0
	if !wafTerminal && h.cfg.Pool != nil {
		scope := realm
		if !realmExplicit {
			scope = "" // 裸名归属：回落允许跨域 → 判定范围是全池
		}
		// 保留积分耗尽（pool.reserve_credits）：池内候选**全部**只因余额 ≤ 保留线而出局
		// （账号其实都健康、没在冷却）时，通用文案「cooling/disabled」是误导——用户会去
		// 翻冷却/禁用状态，而真正的动作是"给号充值/调小 reserve_credits/改用免费模型"。
		// 这里只**补一句可操作的限定说明**（code 仍是 no_healthy_account：它不是新错误类，
		// 客户端侧的重试语义与"池子空了"一致——换号/重试都不会自己变好）。
		//
		// 只在 lastErr == nil 时改文案：有上游原文时必须逐字透传（透传纪律优先，原文自带
		// 语义，拼本地前缀会污染排障证据）。选号阶段就无候选（一次上游都没打）正是
		// lastErr == nil 的情形，也是本判定的目标场景。
		//
		// 与 429 分支的排他：模型级冷却优先（那是更具体的终态信号，且两者不会同时成立
		// ——被保留积分拦住的号没有模型级冷却条目）。故本分支放在 429 判定**之后**。
		modelRateLimited := false
		if wait, ok := h.cfg.Pool.ModelRateLimitExhausted(bareModel, scope); ok {
			modelRateLimited = true
			status = http.StatusTooManyRequests
			code = "model_rate_limited"
			hint = upstream.ModelRateLimitedHint()
			// 向上取整到整数秒（不足 1 秒的余量也计 1 秒）：HTTP 标准头只认秒，
			// 截断成 0 等于告诉客户端「立刻重试」，与退避语义正好相反。
			retryAfter = int(wait / time.Second)
			if wait%time.Second != 0 {
				retryAfter++
			}
			if retryAfter < 1 {
				retryAfter = 1
			}
			if lastErr == nil {
				// 无上游原文可透传（选号阶段就无候选，一次上游都没打）：既有本地文案
				// 「all accounts unavailable (cooling/disabled)」在 429 语境下会被读成
				// 服务端故障，补一句限定说明「是模型被限流、不是池子坏了」。有上游
				// 原文时 message 一个字节都不动（透传纪律优先，原文自带语义）。
				msg = "all accounts unavailable for this model (rate limited upstream, retry after the reset window)"
			}
		}
		// 保留积分耗尽（pool.reserve_credits）的可操作文案：见上方 modelRateLimited 处的
		// 说明。只在「没走 429 分支 + 无上游原文可透传 + 全池候选都只因保留积分出局」
		// 三个条件同时成立时替换——任一不成立就保持既有 503 文案（零回归）。
		if !modelRateLimited && lastErr == nil {
			if blocked, line, ok := h.cfg.Pool.ReserveCreditsExhausted(bareModel, scope); ok {
				msg = fmt.Sprintf(
					"all accounts unavailable: every account for this model has credits at or below the reserve line (pool.reserve_credits=%d, %d account(s) blocked) — top up the accounts, lower pool.reserve_credits, or use a free model",
					line, blocked)
			}
		}
	}
	if retryAfter > 0 {
		// Retry-After 是 HTTP 标准头（秒数）：429 之外不发——503 的既有契约
		// （no_healthy_account/waf_ip_blocked 文案 + gateway_hint）保持原样不动。
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：调用方已
// 准备好 lastErr 并打算 continue 换号（continue 前经 rotateBackoffKind 按 kind
// 决定是否退避：429 不退避、其余含 WAF 403 照常退避，见 backoff.go 的 backoffWorthwhile）。
//
// 路径清单，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避（soft_rate 基数起、
//     softStreak 翻倍、封顶 soft_rate_max，冷却中兜底探测不翻倍）。P1-2 后冷却时长
//     优先采信 Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却（WAF 403 修复 P0-1）：**不 Disable**——WAF 403 是
//     IP/指纹维频控信号（双账号 403 后账号本身健康），罚过即走、到期自愈。
//     时长优先 Retry-After 头（P1-2）；缺失按 soft_rate 基数起 · 2^softStreak
//     封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避（WAF 信号带 IP 级粘性，
//     故指数升级保底存在）。基数经 jitterDur 抖动（复用 backoff.go 单一抖动来源，
//     防多账号同相位冷却到期再聚团）。不喂熔断（WAF 拦截是频控不是账号故障）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → NoteSessionDead（阈值版）：连续 sessionDeadThreshold 次才禁用，
//     未达阈值仅计计数（单次 12153 多为临时抖动，与 scheduler/keepalive 侧口径统一）。
//   - ErrAccountFault → 11140 阈值化（未达阈值软冷却、达阈禁用）+ 14017 恒软冷却。
//     11140 不再单次即 Disable（根因未定 + 代价不对称 + 12153 误杀 13 号的前车之鉴），
//     详见 pool/entry.go 的 accountFaultFails/accountFaultThreshold 注释。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrPromptTooLong → 11115「prompt is too long」：请求的问题不是账号的问题
//     （同一 body 换任何号都超限）。零动作（不冷却/不熔断/不 NoteError、不喂连败，
//     同 ErrContentBlocked 待遇），chatCompletions 已直接透传原文返回不轮转——
//     该分支只为文档完备，不指望走到换号路径。
//   - ErrInvalidImage → 11135「invalid_image_data」：请求级**终态**（图片数据无效是
//     请求的问题，同一张图换任何号都被拒）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败，同 ErrContentBlocked/ErrPromptTooLong 待遇），chatCompletions 已直接
//     透传原文返回不轮转——该分支只为文档完备，不指望走到换号路径。
//   - ErrBadParams → 11101 请求体解析失败：请求级终态（吸收上游 PR #99）——不罚号
//     且**不轮转**，chatCompletions 已直接透传原文回 400（换号必然同样失败）。
//     11133 model_param_invalid 同归本类但**保留轮转**（账号侧后端差异），分野理由
//     见 upstream.IsBadParamsBody 注释。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。这是**唯一**的熔断入口。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池——
//     「不知道原因的兜底」，与冷却「知道原因的惩罚」并存取更长者不叠加（healthy
//     或门；带权威分类的错误不喂连败，防重复计罚）。ErrNone 零防御路径不喂。
//     同一请求内**同一错误指纹**只喂首个账号（dedup，见 errDedup）：ErrClient 分支
//     继续轮转，不去重的话一个请求级 4xx 会在 N 个账号上各记一次连败——一个请求
//     就能把整池推向降权（与 11135 修复前的自伤面同形）。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（出站裸名：6004 记模型豁免、11102 记 (账号,模型) 负缓存）。uerr 是
// ChatStreamContext 返回的分类信封（可携带 RetryAfter，P1-2）；防御路径下为 nil，
// 冷却时长回落既有计算。
//
// dedup 是**单请求**的错误去重状态（nil = 不去重，单元调用/防御路径合法）：只作用于
// default 分支 ErrClient 的连败喂入（见 errDedup 与 default 分支注释），其余分类不读它。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；余额恢复解冻（ReenableIfCredits→reviveCoolingLocked）
// 仅对硬冷却放行（issue #199 收窄：软冷却/模型级冷却不被余额刷新/签到解冻），且只清冷却、不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error, dedup *errDedup) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间（重构核心）：只要 body 带「将在 … 重置」，无论业务
		// code 是 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不
		// softStreak 指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型
		//     豁免（既有 issue #31 语义）。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免
		//     （普通账号级限流不该因切模型绕过）。
		// 基数一律取 h.softCooldown()（热改优先），管理面板改 soft_rate 后立即生效。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if upstream.IsModelRateLimit(body) {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// P1-2：body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加，
		// 与重置墙钟同一对齐语义）。头优先于「有界退避」，但**低于** body 重置文案
		// （上方已 return）——文案是上游更权威的口径（Retry-After 只在无重置文案时兜底）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max）；已在冷却中的兜底探测不翻倍（见 CooldownSoftRate）。
		// 本次换号**不退避**（rotateBackoffKind 放行 ErrSoftRate）：用户语义是
		// 「临时 429 重试一次后仍 429 就立即切下一个号」，切的是另一个账号，
		// 上游频控按账号计，等待只会抬高首字延迟（见 backoffWorthwhile）。
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族（本仓 v1.9.7
		// 既有，不新建平行冷却系统）：基数取 soft_rate 基数（h.softCooldown()，热改优先）、
		// softStreak 指数升级、封顶 soft_rate_max、冷却中兜底探测不翻倍——全部继承既有
		// 语义。Retry-After 头优先（P1-2，WAF 拦截页可能带该头）。**不 Disable**：WAF 403
		// 是 IP/指纹维频控信号（账号本身健康），罚过即走、到期自愈；也不喂熔断
		// （拦截是频控不是账号故障，NoteError 只服务 ErrServer）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(h.softCooldown()), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(h.softCooldown()), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		// chat 路径的 12153 与 scheduler/keepalive 侧**口径统一**：走阈值版
		// NoteSessionDead（连续 sessionDeadThreshold 次才禁用），不再单次即 Disable。
		// 旧实现此处直接 Disable，而 scheduler.go 的 keepalive 路径走阈值版——两条路径
		// 对同一错误码的处置不一致，且 state.go 明载「一次 12153 即 Disable」导致
		// 「13 个 disabled 号全部 refresh 成功，是历史误判的受害者」。单次 12153 多为
		// 临时抖动（网络/闪断/refresh 竞态），不该永久摘号。
		// 未达阈值时零额外惩罚（12153 不是限流信号，冷却它没有语义）：仅累计计数并换号。
		h.cfg.Pool.NoteSessionDead(uid)
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ **阈值化**：未达阈值回落**有界软冷却**
		//     （到期自愈，误判代价有界），连续 accountFaultThreshold 次才由
		//     NoteAccountFault 内部 Disable（保留对「真封禁」的兜底）。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）——本分支语义一字不动。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		//
		// 为什么 11140 从「一次即 Disable」改成阈值化（依据见 pool/entry.go 的
		// accountFaultFails 与 accountFaultThreshold 注释，此处只留结论）：
		//   - 根因未定：本仓两处归因互斥（出站 UA 平台段 vs 账号状态）且都无实测锚定；
		//     19 个历史实例目录 + 4 个部署目录里没有任何账号因 11140 被禁、没有真实报文；
		//   - 代价不对称：误判「不罚号」= 多一次轮换（有界）；误判「Disable」= 永久摘掉
		//     健康账号 + 需人工登录（无界，disabled 无任何自动恢复路径——见
		//     pool.TestAccountFaultDisabledRequiresManualRevive 钉死的现状）；
		//   - 前车之鉴：一次 12153 即 Disable 曾误杀 13 个健康号，本仓已改成阈值版
		//     （NoteSessionDead）；11140 旧行为是**完全相同且更激进**的模式。
		//   - 伏笔：若将来拿到真实 11140 报文能把「内容审核拦截」与「账号级封禁」
		//     区分开，可在此再加第二层判定（审核类零动作不计数），阈值路径只服务真封禁。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			if h.cfg.Pool.NoteAccountFault(uid) {
				log.Printf("chat uid=%s: 连续 %d 次 11140 request illegal — 禁用（需人工复活）",
					uid, pool.AccountFaultThreshold())
				return
			}
			// 未达阈值：有界软冷却（到期自愈）。reason 带 code 便于运维在 /status 区分
			// 11140 与 14017 两种 account fault。基数取 h.softCooldown()（热改优先，
			// 面板改 soft_rate 后立即生效）——与 ErrSoftRate 分支同一口径。
			h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (11140)")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked
		// 待遇），chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备，
		// 不指望走到换号路径。
	case upstream.ErrInvalidImage:
		// 11135「invalid_image_data」：请求级**终态**（图片数据无效是请求的问题，
		// 同一张图换任何号都被拒，与 11115 同哲学）。零动作（不冷却/不熔断/
		// 不 NoteError/不喂连败，同 ErrContentBlocked/ErrPromptTooLong 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
		// 为什么零动作：此前该形态落 ErrClient → 本函数 default 喂连败 + 逐号轮转，
		// 一张坏图让每个账号各记一次连败、达阈全池降权 10 分钟（用户实测 503）。
		// 图片是否可识别与账号健康无关，不该由账号池承担代价。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）。**11101 不轮转**
		// ——chatCompletions 已直接透传原文返回 400（吸收上游 PR #99，见该处分支注释）。
		// 11133 model_param_invalid 同归本类（Classify 见 isModelParamInvalid），但
		// **保留轮转**：它可能是「该模型不支持图片」这类与账号无关的参数拒绝，也可能是
		// 账号侧后端差异——轮转仍有价值（11102 的 (账号,模型) 负缓存证明同模型在不同
		// 账号上可用性不同）；两者都绝不能喂连败：本分支零动作即天然满足（ErrClient
		// 的连败喂入不经过这里）。
		// 11101 vs 11133 的分野理由集中在 upstream.IsBadParamsBody 的注释（单一事实来源）。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），写 modelCooldowns[model]，Until 为指数退避 TTL（6h 起、封顶
		// 24h）。选号侧 healthyForModel 对该账号自动避开该模型；切模型/切账号即可用。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		// 注意「只换号不罚」的既有语义不变：NoteFailures 不是惩罚（不冷却/不熔断/
		// 不 Disable/不 NoteError），只是「记录以便达阈降权」，且成功一次即清零回池。
		//
		// 请求级去重（dedup，见 errDedup）：同一请求内**同一错误指纹**只喂首个账号。
		// ErrClient 分支继续轮转，不去重则一个请求级 4xx 在 N 个账号上各记一次连败
		// ——一个请求就能把整池推向降权（与 11135 修复前的自伤面同形）。指纹取业务
		// code（ErrFingerprint，不含逐请求变化的 requestId）；判不出指纹时保守喂。
		if kind == upstream.ErrClient {
			if dedup.claimErrClient(upstream.ErrFingerprint(body)) {
				h.cfg.Pool.NoteFailures(uid)
			}
		}
	}
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案
// （code=upstream_timeout），两者排查方向完全不同（超时看上游/链路，无号看池子）。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
//
// 三态判定（吸收上游 PR #93）：
//  1. net.Error.Timeout()——ResponseHeaderTimeout / Client.Timeout / 系统 ETIMEDOUT
//     的统一形态（net.OpError 包装后仍可 errors.As 命中）；
//  2. 显式 deadline——context.DeadlineExceeded / os.ErrDeadlineExceeded（包装链上
//     任一层）；
//  3. **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流
//     （见 upstream/idle.go 的 IdleTimeout），也就是上游停滞。clientGone=true
//     （客户端主动断连，r.Context().Err() != nil）时不算：人已走，语义是「中断」
//     不是「上游超时」，且这种情形下重试本来就没有意义（既有路径已按 ctx 闸门处理）。
//
// 调用方注意：本判定**优先于** upstream.IsTransient（i/o timeout 同时命中两者的
// 抖动词表）——超时若走抖动路径会换号重试，而换号对超时注定无效（见调用点注释）。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：
// 第 i 次轮转失败（continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，
// ±25% 抖动），ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转
// （客户端已走，换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；
// 正常单号请求（首次成功）不经过本函数，零开销。
//
// **429（ErrSoftRate）不走本函数**：分类错误路径经 rotateBackoffKind 按 kind 分流，
// 429 立即换下一个号（首字延迟优先），WAF 403 等其余分类仍走本函数退避——分野理由
// 见 backoff.go 的 backoffWorthwhile。其余三条轮转路径（抢名额失败 / refresh 失败 /
// 传输层错误）不经分类判定，一律沿用本函数（既有语义不变）。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// planPTLMaxTokensRetry 判定本次 11115 是否可按错误里的真实数字下调 max_tokens 重试。
//
// 三步判定（任一失败即返回 ok=false → 调用方保持现状：透传原文、不重试）：
//  1. 从上游原文解析 `<n> tokens > <limit> maximum`（大小写/空格容错）；
//  2. overshoot = n - limit（解析函数已保证 n > limit）；
//  3. 按 overshoot + 安全余量下调请求体的输出预算字段（max_tokens / maxOutputTokens），
//     低于下限则不可用。
//
// 全部判定在 upstream 侧（ParsePromptTooLongOvershoot / DowngradeMaxTokens），本函数
// 只做串联与日志字段组装——规则与注释集中在一处，避免 handler 与 upstream 各写一套。
//
// 返回：newBody（改写后的请求体，只改输出预算一个字段）、overshoot、oldMax、newMax、ok。
// ok=false 时 newBody 为 nil（调用方沿用原 body）。
func (h *Handler) planPTLMaxTokensRetry(body, respBody []byte) (newBody []byte, overshoot, oldMax, newMax int64, ok bool) {
	n, limit, parsed := upstream.ParsePromptTooLongOvershoot(string(respBody))
	if !parsed {
		return nil, 0, 0, 0, false
	}
	overshoot = n - limit
	rewritten, oldMax, newMax, ok := upstream.DowngradeMaxTokens(body, overshoot)
	if !ok {
		return nil, 0, 0, 0, false
	}
	return rewritten, overshoot, oldMax, newMax, true
}

// respWriteTracker 记录本请求是否已向客户端写出任何字节（响应头或 body），供
// 11115 下调重试的「必须在写出前」守卫使用（见 chatCompletions 顶部的包装注释）。
//
// 为什么需要运行时守卫而不是只靠结构论证：结构上 11115 必然发生在写响应之前
// （上游在流式开始前回 400，handler 的写响应都在轮转循环之后），但这条论证依赖
// 「未来没有人把写响应提前」——加一个显式断言，把「万一被改坏」从「悄悄发出半截
// 错误响应」变成「跳过重试、按原路径回错误」（fail-safe 方向正确）。
//
// 透传能力：Flush 转发给底层（SSE 逐帧 flush 依赖它）；Unwrap 暴露底层 writer，
// 让 http.ResponseController 仍能找到 SetReadDeadline/SetWriteDeadline 等能力。
// 底层不支持 Flush 时转发为无操作——与「类型断言失败跳过 flush」的可观测行为一致。
type respWriteTracker struct {
	http.ResponseWriter
	wroteHeader bool
	wroteBody   bool
}

func (t *respWriteTracker) WriteHeader(code int) {
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *respWriteTracker) Write(p []byte) (int, error) {
	t.wroteHeader = true
	if len(p) > 0 {
		t.wroteBody = true
	}
	return t.ResponseWriter.Write(p)
}

// Flush 转发底层 Flush（不支持时无操作，与既有「断言失败即跳过」同效）。
func (t *respWriteTracker) Flush() {
	if fl, ok := t.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter（http.ResponseController 的 rwUnwrapper 协议）。
func (t *respWriteTracker) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// wrote 报告是否已向客户端写出任何字节（响应头或 body）。
func (t *respWriteTracker) wrote() bool { return t.wroteHeader || t.wroteBody }

// invalidImageMessage 11135 透传 message：上游 body 原文（含 code/extError/
// displayMsg/requestId/actions，客户端自行排查）；空 body 兜底为可读分类短文案
// （不编造原文、不用本地调度固定词覆盖）。
func invalidImageMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "invalid image data"
	}
	return body
}

// errDedup 单请求内的错误去重状态（chatCompletions 每请求新建，见 applyErrorPolicy
// 的 dedup 参数）。
//
// 解决的自伤面（issue #114 连败降权的反向代价）：ErrClient（未知 4xx）在
// applyErrorPolicy default 分支喂连败计数，而该分支**继续轮转**——同一个请求级 4xx
// 依次打到 N 个账号上，N 个账号就各记一次连败，一个请求即可把整个池子推向降权
// 阈值（默认 5 次 → 降权 10 分钟），与 11135 修复前的自伤面同形。
// 一个请求内**重复出现的同一错误形态**不构成对后续账号的独立证据（同一请求在别的
// 账号上复现，恰恰说明它是请求级的），故只喂首个账号。
//
// 边界（有意收窄）：只影响「喂不喂连败」这一个动作——轮转/冷却/熔断/禁用等任何
// 既有语义都不变；不同错误形态（不同业务 code）各自独立计数（不同错误是不同证据，
// 未被吞掉）；指纹判不出（非 JSON/无 code）一律喂（保守，不吞计数）。带权威分类的
// 错误（ErrServer/ErrSoftRate/ErrBadParams/ErrInvalidImage/…）根本不走本表。
type errDedup struct {
	errClient map[string]bool
}

// newErrDedup 建单请求去重状态。
func newErrDedup() *errDedup { return &errDedup{errClient: map[string]bool{}} }

// claimErrClient 报告本次 ErrClient 失败是否应喂连败：同一请求内首次出现的错误
// 指纹返回 true 并记账，重复出现返回 false。nil 接收者/空指纹（判不出形态，如
// 非 JSON body）一律 true——不去重、不吞计数（保守）。
func (d *errDedup) claimErrClient(fingerprint string) bool {
	if d == nil || fingerprint == "" {
		return true
	}
	if d.errClient == nil {
		d.errClient = map[string]bool{}
	}
	if d.errClient[fingerprint] {
		return false
	}
	d.errClient[fingerprint] = true
	return true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// readBodyErrMsg 组装「读请求体失败」的客户端可见文案。
//
// 读**超时**时追加可操作提示：这类失败在生产上表现为用户对话被拦腰截断，而原始
// 错误（如 "read tcp 10.42.0.245:7863->10.42.0.1:35543: i/o timeout"）既没说是
// 哪一端的超时，也没说能调哪个键——用户只能猜。故把生效值与调法一并给出
// （v1.9.13 事故：460KB 上下文经跨境慢链路上传撞上写死的 60s）。
//
// 只在超时（net.Error.Timeout()）时加提示：普通读错误（客户端提前断开、body 被
// 截断等）加这段文案是误导——它们与 read_timeout_seconds 无关。
func readBodyErrMsg(err error, timeout time.Duration) string {
	base := "read body: " + err.Error()
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		return base
	}
	return fmt.Sprintf("%s（请求体读取超时；网关 server.read_timeout_seconds 当前 %ds，慢链路上传大上下文可调大，面板修改即时生效）",
		base, int(timeout.Seconds()))
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）；
// type/code/状态码一律不变（纯增量字段）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。
//
// 判定只认 part 的 type 字段，不看 url 内容——data: URL（base64 / utf8）与 http(s)
// URL 两种形态同样命中（11133 的「模型不支持图片」指向只关心「有没有图」）。
//
// 逐条消息、逐条 content 独立尽力解析（不整块 Unmarshal 到固定结构）：混合形态
// （有的消息 content 是字符串、有的是带图数组）下，整块解析会因字符串 content 的
// 类型错误**整体失败**、函数提前返回 false，从而漏判「请求带图」——11133 的换模型
// hint 前提随之丢失（实测复现）。单条解析失败只跳过该条，其余消息照常判定。
//
// 畸形/其他形态（messages 非数组、content 非数组/非字符串、part 非对象）一律
// 跳过；全部判不出 → false（hint 侧宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, raw := range peek.Messages {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue // content 是字符串/null/对象等非数组形态：本条无 part 可判
		}
		for _, p := range parts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 10min 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
//
// 只查 CN 动态目录（dynamicModelsCache）：其 supports_images 来自上游探测真值。
// 不并入 global 目录——global 侧 mergeGlobalModelInfos 会把「探测未返回、仅按静态
// 名单补齐」的 id 也放进列表且能力字段零值，据此判定「不支持图片」等于编造能力事实
// （宁缺勿滥），故 global 模型在此退中性 hint。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
