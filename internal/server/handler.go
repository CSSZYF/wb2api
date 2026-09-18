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
	"strings"

	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
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
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
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
}

// SetMaxBodyBytes 热更新请求体上限（面板保存配置路径调用）。
// n<=0 与 NewHandler 兜底口径一致：回落 8MB。
func (h *Handler) SetMaxBodyBytes(n int64) {
	if n <= 0 {
		n = 8 << 20
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
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
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
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
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
func (h *Handler) fetchGlobalModelInfos() []upstream.ModelInfo {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
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

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
//
// 强制 realm=cn 选号：本表是 CN 目录（/console 家族），拿 global 账号去打
// global 域的同名路径会吃到 500/解析失败（v1.x 面板"拉取模型 500"的根因之一）。
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

	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "cn")
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
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

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 入站 body 读窗口按**当前**配置值逐请求重设（面板改 server.read_timeout_seconds
	// 后无需重启即生效；慢链路大上下文不再被启动时的静态值掐断）。必须在读 body 之前。
	h.armBodyReadDeadline(w, r)
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.maxBodyBytes.Load()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", readBodyErrMsg(err, h.readTimeoutValue()))
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：多图/长上下文会话易触发（历史图片每轮以 base64 重发）；请压缩图片或调大 server.max_body_mb（面板修改即时生效）后重试", limit>>20))
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
	defer st.done()

	tried := map[string]bool{}
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

	// 轮级兜底聚合键：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry
	// Studio 等请求体里既无 conversationId 也无 metadata）sessKey 恒空，会话头族的
	// 聚合主键只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求一条记录。
	// 这里按 body 里最后一条 user 消息派生轮级键（同轮内所有上游调用同键）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := ""
	if sessKey == "" {
		turnKey = session.TurnKey(body)
	}

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
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, started time.Time) {
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

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
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
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键客户端：轮级兜底——同轮内 tool call 多轮 / 换号重试 / 降级重发
		// 共享同键，用户发下一条消息自动换键。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// 换号上限本请求内固定一次（与 maxBodyBytes 同口径的"请求内快照"）：
	// 轮转中途面板改值不影响本请求已定的次数，避免同请求内上限漂移。
	maxRotate := h.rotateLimit()
	for i := 0; i < maxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
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
		st.nick = acct.Nickname
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
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
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
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析，见 ChatStreamContext 注释）。传输层错误（非 *Error）走
		// 抖动换号分支；防御分支（terr 为 nil 但 status>=400，如 ErrNone 兜底）回落
		// 本地 Classify，双保险不改变语义。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避，WAF P0-2）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
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
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := upstream.ContentBlockedClientMessage(string(respBody))
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与内容策略拦截同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作（不冷却/不熔断/不 NoteError，
			// 不喂连败），fail 只释放租约。error-passthrough：message 装上游 body 原文
			// （code/msg/requestId 原样，含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/Status（末端映射与冷却时长共用）+ RetryAfter
			// （末端 429 映射与冷却对齐共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账（单号偶发 403
			// 仍冷却），IP 级状态只改变「是否继续轮转」——协同不叠加。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避，WAF P0-2）
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
		// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}))
			if upstream.IsEmptyStreamError(sErr) {
				// 上游 200 但空流（0 有效帧）：Stream 已写 error 帧 + [DONE] 兜底
				// （HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/状态收敛到
				// 502 观测，与非流式 Aggregate 空流→502 upstream_parse 同语义
				// （此前 `_ =` 吞错把失败流记成 200 假成功，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				log.Printf("WARN: [server] stream uid=%s model=%s: empty upstream stream (200+0 frames)", uidPrefix(acct.UID), bareModel)
			}
			recordAttempt(acct.UID, stats.Usage(), attemptStarted)
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
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), attemptStarted)
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	code := "no_healthy_account"
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
	if errors.As(lastErr, &ue) {
		// 上游错误：hint 按 Kind + 上游原文 + 请求形态判定（upstream.GatewayHint
		// 单一事实来源）。11133/11135 形态判定在 hint 层自带，ErrClient 家族也
		// 可能带上 hint；未覆盖形态（ErrServer/ErrNotFound/ErrBadParams/ErrNone）
		// 返回空串 → 字段缺席（不编造）。
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		if ue.Kind == upstream.ErrWafBlock && h.wafIP.active() {
			code = "waf_ip_blocked"
			if s := strings.TrimSpace(ue.Msg); s != "" {
				msg = s
			} else {
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
	}
	writeOpenAIErrorHint(w, http.StatusServiceUnavailable, code, msg, hint)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：调用方已
// 准备好 lastErr 并打算 continue 换号（continue 前由 rotateBackoff 退避）。
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
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrPromptTooLong → 11115「prompt is too long」：请求的问题不是账号的问题
//     （同一 body 换任何号都超限）。零动作（不冷却/不熔断/不 NoteError、不喂连败，
//     同 ErrContentBlocked 待遇），chatCompletions 已直接透传原文返回不轮转——
//     该分支只为文档完备，不指望走到换号路径。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。这是**唯一**的熔断入口。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池——
//     「不知道原因的兜底」，与冷却「知道原因的惩罚」并存取更长者不叠加（healthy
//     或门；带权威分类的错误不喂连败，防重复计罚）。ErrNone 零防御路径不喂。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（出站裸名：6004 记模型豁免、11102 记 (账号,模型) 负缓存）。uerr 是
// ChatStreamContext 返回的分类信封（可携带 RetryAfter，P1-2）；防御路径下为 nil，
// 冷却时长回落既有计算。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；余额恢复解冻（ReenableIfCredits→reviveCoolingLocked）
// 仅对硬冷却放行（issue #199 收窄：软冷却/模型级冷却不被余额刷新/签到解冻），且只清冷却、不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
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
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录），到期后重新选号只会再撞 403 浪费一次轮换——
		//     硬禁用（Disable），不再参与选号。面板以 disabled + disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault (14017)")
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
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
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
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：
// 第 i 次轮转失败（continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，
// ±25% 抖动），ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转
// （客户端已走，换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；
// 正常单号请求（首次成功）不经过本函数，零开销。
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
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
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
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 1h 未
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
