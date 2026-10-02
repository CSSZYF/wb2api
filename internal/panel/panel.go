// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// HiddenModels 对外隐藏的模型名，与 /v1/models 同口径（同一份 upstream.HiddenSet）。
	// nil = 不隐藏。
	HiddenModels upstream.HiddenSet

	// PinnedModels 强制内置的模型条目，与 /v1/models 同源：上游目录没给的模型
	// （账号差异/灰度）也稳定出现在「模型与档位」，面板看得见 = 客户端调得到。
	PinnedModels []upstream.PinnedModel
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	// 单账号对话测试（诊断用，见 testchat.go）：行内「测试」按钮的落点。
	p.mux.HandleFunc("POST /panel/api/account/test_chat", p.withAuth(p.accountTestChat))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	// cockpit tools 导出 JSON 批量导入（吸收上游 9371f7d）：Add Account 的
	// 「导入 JSON」标签页落点。写操作（落盘 + 进池）→ 同 withAuth。
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	// 临时停用/恢复（上游 a20d06f 吸收）：与上面的 disable/revive 是**两套语义**——
	// disable/revive 管「系统判定的坏号」（永久禁用，需复活），suspend/resume 管
	// 「运维主动摘除」（临时停用，可随时恢复）。两者独立，都清空才回选号池。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/suspend", p.withAuth(p.accountSuspend))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	// 账号顺序（顺序填充式选号的权威次序，pool.pick_mode=sequential 时生效）：
	// 面板拖拽排序的落点。写池 + 落盘 state.json 顶层 account_order。
	p.mux.HandleFunc("POST /panel/api/accounts/order", p.withAuth(p.accountOrder))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/status", p.withAuth(p.schoolStatus))
	p.mux.HandleFunc("POST /panel/api/school/run_all", p.withAuth(p.schoolRunAll))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("POST /panel/api/cooldown_probe/run", p.withAuth(p.cooldownProbeRun))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		// 快过期窗口（秒）：前端据此在积分列/提示里写清「快过期 N」的依据窗口
		// （「7 天内没有到期积分」与「没有快过期积分」是两种结论），不写死 168h。
		// 与调度器同源（同一原子的热生效值，面板改 pool.expiring_soon 后立即一致）。
		"expiring_soon_sec": int64(p.expiringSoonWindow().Seconds()),
		"accounts":          p.cfg.Pool.List(),
	})
}

// expiringSoonWindow 当前生效的快过期窗口：与调度器同源（热改后立即一致）。
// 无调度器（未装配/单测）时返回 0 = 禁用分桶，与 scheduler 的零值语义一致。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
//
// realm 感知：默认查「池内唯一可用域」（只登国际版账号的部署 → 查 global），可用
// ?realm=cn|global 显式指定。两条路径的端点家族必须分开——国际站的 /console 家族
// 返回 500 网关错误页（HTML），这是历史「拉取模型 500」的根因。
// 无可用账号 503；CN 域上游失败 502。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	realm := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("realm")))
	if realm != "cn" && realm != "global" {
		realm = p.defaultRealm() // 纯 CN 部署回落国内版，两域都可用时取 global
	} // 元数据路径（PickExcludingForRealmMeta）：本查询不消费积分，豁免保留积分闸门
	// ——否则池内账号全部触底时面板「模型与档位」会 503，恰好是用户最需要看清
	// "还剩什么免费模型"的时刻。理由见 internal/pool/reserve.go 文件头。
	acct := p.cfg.Pool.PickExcludingForRealmMeta(nil, realm)
	if acct == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"没有可用的 "+realm+" 账号：请先在面板添加账号再查询")
		return
	}
	// 国际站模型目录在 /v2 家族（/console 家族返回 500 网关错误页）——由
	// upstream.FetchModels 的 modelsPaths 按 realm 选路。两个域走同一条解析路径，
	// 面板才能拿到倍率/默认档/思考档/上下文/最大输出这五列。
	//
	// 价格口径（2026-10-02 复核，分支 feat/real-price）：本路径（fetchModelsOnce →
	// MergeCatalogOverlay）**保留 credits 原文**，与 /v1/models 的探测口径不同——
	// 那条链路刻意把倍率摘进旁表（PLAN §3.D2「倍率不进路由/列表口径」，见
	// upstream.detachCredits），本面板路径是**展示口径**，要的正是"这个模型此刻
	// 多少钱"。实测（真实 global 账号）：deepseek-v4.1-flash 走 v3-CLI 带出 x0.00、
	// -sg 带出 x0.03、hy4-preview 取企业端点的 x0.00（计费目录权威，不被 v3 牌价
	// x0.29 覆盖，见 MergeCatalogOverlay §2）。
	infos, diag, err := p.cfg.Upstream.FetchModelsDiag(acct)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch models: "+err.Error())
		return
	}
	countUpstream := len(infos)
	// 写死条目兜底，两级（顺序固定：先补字段，再补条目）：
	//   ① 字段级（FillMissingFromPinned）：上游给了条目但**没给该字段**时补——
	//      典型是 v3 探测半残（5min 负缓存 / 上游改 schema）时 deepseek-v4.1-flash
	//      仍在线（静态名单/裸 id）但 credits 为空；写死快照里 x0.00 是已知真值，
	//      不该因为链路半残而显示 "—"（用户点名要「看到真实价格」）。
	//   ② 条目级（MergePinned）：上游压根没有这个 id 时整条追加（既有语义不变）。
	// 两个方向都是「上游数据优先」：上游给了的字段/条目一律不被写死快照覆盖。
	infos = upstream.FillMissingFromPinned(infos, p.cfg.PinnedModels)
	infos = upstream.MergePinned(infos, p.cfg.PinnedModels)
	// 与 /v1/models 同一份隐藏名单：面板能看见的模型，客户端一定也能调用。
	infos = p.cfg.HiddenModels.FilterInfo(infos)
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		entry := map[string]any{
			"id":                   mi.ID,
			"name":                 mi.Name,
			"context_length":       mi.ContextWindow,
			"max_output_tokens":    mi.MaxTokens,
			"max_allowed_size":     mi.MaxAllowedSize,
			"default_effort":       mi.DefaultEffort,
			"supported_efforts":    mi.Efforts,
			"can_disable_thinking": mi.CanDisableThinking,
			"supports_reasoning":   mi.SupportsReasoning,
			"supports_images":      mi.SupportsImages,
			"credits":              mi.Credits,
			// prefixed_id：带 realm 前缀的完整模型名（后端路由协议 "cn:"/"global:"，
			// 见 server.splitRealmPrefix）——用户手动加模型只走一个域就靠它（复制用）。
			// id 字段保持裸名（既有契约不破，客户端/脚本按裸名比对）。
			"prefixed_id": realmPrefixedID(realm, mi.ID),
		}
		// free：生效倍率 ≤ 0（有 promo 用 promo factor，否则解析牌价）——判定与保留
		// 积分的免费判定同源（upstream.EffectiveMultiplier），前端只渲染不重算。
		// 缺失/未知一律不给该键（**缺失 ≠ 免费**，前端据此显示 "—" 而不是 0）。
		if f, ok := mi.EffectiveMultiplier(); ok && f <= upstream.FreeModelMultiplierThreshold {
			entry["free"] = true
		}
		// 限时优惠（modelPromotions，吸收上游 2b0eedd）：credits 是**牌价**，
		// promo_* 是当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——WorkBuddy
		// 客户端显示的正是生效价。前端据此显示「生效价 + 标签 + 划线牌价（悬停看
		// 时段）」。只在有值时下发（缺失 ≠ 免费，前端不得回填）。
		if mi.PromoFactor != nil {
			entry["promo_factor"] = *mi.PromoFactor
			entry["promo_credits"] = mi.PromoCredits
		}
		if mi.PromoLabel != "" {
			entry["promo_label"] = mi.PromoLabel
		}
		if mi.PromoNote != "" {
			entry["promo_note"] = mi.PromoNote
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"realm":  realm,
		"models": out,
		// realm_servable 两个域此刻是否可查（前端据此决定显示哪几个选择器）。
		// 判据与缺省域回退同源：池内有没有该域的账号 + global 逃生门——池内没有的域
		// 不给按钮，否则用户点一下必然吃 503「没有可用的 global 账号」。
		"realm_servable": map[string]bool{
			"cn":     p.realmServable("cn"),
			"global": p.realmServable("global"),
		},
		// 只读诊断：上游到底给了什么、哪一步筛掉了谁。nil 切片统一序列化成 []，
		// 前端与脚本都不用再判 null。
		"diag": map[string]any{
			"path":          diag.Path,
			"cli_agent_ids": nonNil(diag.CliAgentIDs),
			"agents":        nonNilAgents(diag.Agents),
			"raw_model_ids": nonNil(diag.RawModelIDs),
			"all_model_ids": nonNil(diag.AllModelIDs),
			"dropped":       nonNil(diag.Dropped),
			// v3_only_ids：企业端点没有、靠 /v3/config 追加进目录的 id。与 dropped
			// 配对读能回答"某模型为什么不在列表里"的全部成因（上游没给 / 被筛掉 /
			// 追加步失效——重写前面板路径缺追加步，这批 id 全部不可见）。
			"v3_only_ids":    nonNil(diag.V3OnlyIDs),
			"hidden":         p.cfg.HiddenModels.Names(),
			"pinned":         nonNilPinned(p.cfg.PinnedModels),
			"count_upstream": countUpstream,
			"count_shown":    len(out),
		},
	})
}

// realmServable 报告该域此刻是否可查（供前端决定显示哪几个 realm 选择器）。
//
// 判据与缺省域回退**同源**（这是刻意的：两处若各写一份，会出现"选择器显示 global、
// 点了却 503"）：池内有没有该域的账号；global 另受逃生门（auth.GlobalEnabled）约束
// ——关掉逃生门时 global 账号会被 Realm() 判回 cn，选择器不该还留一个必 503 的按钮。
func (p *Panel) realmServable(realm string) bool {
	if p.cfg.Pool == nil || !p.cfg.Pool.HasRealm(realm) {
		return false
	}
	if realm == "global" {
		return auth.GlobalEnabled()
	}
	return true
}

// defaultRealm 缺省查询域：两域都可用时取 global（既有语义），只有 CN 时取 cn。
// 与 models handler 的内联回退等价，抽出来是为了让 realm_servable 的判据能复用
// 同一份知识（见 realmServable）。
func (p *Panel) defaultRealm() string {
	if p.cfg.Pool.HasRealm("cn") && !p.cfg.Pool.HasRealm("global") {
		return "cn"
	}
	return "global"
}

// realmPrefixedID 拼「realm 前缀 + 裸 id」的完整模型名（面板显示与复制用）。
//
// 前缀形式与**后端路由协议**严格一致（internal/server/resolve_model.go 的
// splitRealmPrefix：第一个冒号前恰为 cn/global 才剥离，大小写敏感）——用户把这个 id
// 贴进客户端配置，网关才会按他指定的域路由。自创形式（intl: / 国际服:）会被
// splitRealmPrefix 当成裸模型名，静默落到缺省域（用户以为锁定了域，其实没有）。
//
// realm 未知（空串/其它值）或 id 为空 → 原样返回裸 id（不编造前缀）。
func realmPrefixedID(realm, id string) string {
	id = strings.TrimSpace(id)
	if id == "" || (realm != "cn" && realm != "global") {
		return id
	}
	return realm + ":" + id
}

// nonNil 把 nil 切片换成空切片，避免 JSON 里出现 null（前端 .length 会炸）。
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilAgents(in []upstream.ModelFetchAgent) []upstream.ModelFetchAgent {
	if in == nil {
		return []upstream.ModelFetchAgent{}
	}
	return in
}

func nonNilPinned(in []upstream.PinnedModel) []upstream.PinnedModel {
	if in == nil {
		return []upstream.PinnedModel{}
	}
	return in
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
// **不解除**临时停用（manualDisabled）——那是运维意图，与「系统判定的坏号」是两件事；
// 响应回显双位状态，前端据此提示「已解冻但仍处临时停用，还需点恢复」。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	// changed 恒 true：Revive 是无条件恢复，没有「已是该状态」的短路（幂等但每次都生效）。
	p.writeAccountState(w, uid, true)
}

// accountDisable 人工永久禁用（不再参与选号，需面板 revive 或重登恢复）。
// 与 accountSuspend（临时停用）的区别：本端点是**系统判定口径**——它清冷却域
// （disableLocked），恢复要走「解冻」（Revive）；临时停用不清任何惩罚维度，
// 恢复走「恢复」（resume）。面板按 disabled/manual_disabled 两位分别呈现。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountSuspend 临时停用（上游 a20d06f / issue #138/#118）：把账号摘出选号池，
// 但保留在池里——签到 / token 保活 / 排程任务照常执行，凭证与积分都是活的。
//
// 与 accountDisable 的分工：本端点只置 manualDisabled，**不清**冷却/熔断/连败降权，
// 也**不碰** disabled 位（临时停用不是"判它坏了"，而是"我主动摘的"）。两者叠加时
// 各自独立解除：suspend 后仍被系统禁用的号，resume 不会让它回池（要再 revive）。
// 幂等：重复 suspend 不报错，只更新原因文案。
func (p *Panel) accountSuspend(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	reason := panelReasonFromBody(r)
	if reason == "" {
		reason = "manual suspend (panel)"
	}
	found, changed := p.cfg.Pool.SetManualDisabled(uid, true, reason)
	if !found {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: suspend uid=%s reason=%q（临时停用：摘除选号流量，签到/保活照常）", uid, reason)
	p.writeAccountState(w, uid, changed)
}

// accountResume 解除临时停用。若账号仍被系统永久禁用（disabled），它**不会**因此回到
// 选号池——那需要「解冻」（revive）。响应里的 disabled 字段就是给前端提示用的。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	found, changed := p.cfg.Pool.ClearManualDisabled(uid)
	if !found {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（解除临时停用）", uid)
	p.writeAccountState(w, uid, changed)
}

// writeAccountState 回显操作后的双位状态：面板据此直接更新 UI 并给出准确提示
// （例如 resume 后仍 disabled → 提示"还需解冻"），不必再打一次 overview。
// changed 报告本次操作是否产生了状态变更（幂等重复调用为 false），前端据此决定
// 提示「已停用」还是「本就停用」；revive 无短路语义，调用方恒传 true。
func (p *Panel) writeAccountState(w http.ResponseWriter, uid string, changed bool) {
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"changed":         changed,
		"disabled":        st.Disabled,
		"manual_disabled": st.ManualDisabled,
		"manual_reason":   st.ManualReason,
	})
}

// accountOrder 设置账号顺序（顺序填充式选号 pool.pick_mode=sequential 的权威次序，
// 面板拖拽排序的落点）。请求体 {"uids":["uid1","uid2",...]}，响应 {"ok":true,...}。
//
// 语义（与 pool.SetOrder 一致，见 pool/order.go）：
//   - 空数组/缺 uids 键 = **清除自定义顺序** → 回落「按 UID 排序」（改动前行为）；
//   - 未知 uid（不在池内）：**过滤掉并在响应里 warning 说明**，而不是整单拒绝。
//     取舍理由：拖拽排序是一次性 UI 动作，客户端拿到的列表可能因为账号在另一处被
//     删除/添加而略微过期；整单 400 会让用户「拖了没反应」且无从修复（前端无法
//     自动重试出正确列表）。过滤后写入的是"当前池内有效次序"，同时把被忽略的 uid
//     明确回给调用方——数据没有静默丢失，用户看得到。已删除 uid 的副作用也是良性的
//     （pool 读取时本就会跳过它们，见 effectiveOrderLocked）。
//   - 未列出的池内账号（新号）由 pool 侧追加到末尾（按 UID 升序）。
//
// 顺序立即落盘（pool.SetOrder 内 saveLocked），重启后仍在。
func (p *Panel) accountOrder(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	var body struct {
		UIDs []string `json:"uids"`
	}
	if r.Body != nil {
		// 限制读取量：uid 列表是短文本（账号数有界），4KB 足够且防畸形大请求。
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
			// 空体/非法 JSON：区分「清除顺序」（空体 = 无 uids 字段）与「格式错误」。
			// 空体是最常见的"清空顺序"调用形态，不应报错；非空但解析失败才是错误。
			if !errors.Is(err, io.EOF) {
				writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
				return
			}
		}
	}
	// 过滤未知 uid（回显 warning），去重保序。
	valid := make([]string, 0, len(body.UIDs))
	var unknown []string
	seen := make(map[string]bool, len(body.UIDs))
	for _, uid := range body.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		if _, ok := p.cfg.Pool.Status(uid); !ok {
			unknown = append(unknown, uid)
			continue
		}
		valid = append(valid, uid)
	}
	p.cfg.Pool.SetOrder(valid)
	log.Printf("panel: accounts/order 已更新（%d 个有效 uid，忽略 %d 个未知 uid）", len(valid), len(unknown))
	resp := map[string]any{
		"ok": true,
		// order 回显**有效顺序**（含被追加到末尾的新号），前端据此立即重排列表，
		// 不必等下一次 overview（与 writeAccountState 的回显口径一致）。
		"order": p.cfg.Pool.Order(),
	}
	if len(unknown) > 0 {
		resp["warning"] = "已忽略不在池内的 uid（账号可能已被删除）"
		resp["unknown_uids"] = unknown
	}
	writeJSON(w, http.StatusOK, resp)
}

// panelReasonFromBody 读可选 JSON 体里的 reason 字段（截断到 200 字符）。
// 空体 / 非 JSON / 无该字段都返回空串——端点不因体格式拒绝（无体是最常见调用形态）。
func panelReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存（与 server 侧 admin 端点同口径）。
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		return ""
	}
	reason := strings.TrimSpace(body.Reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return reason
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。解冻口径同 issue #199：仅硬冷却
// （余额耗尽）账号余额恢复即解冻，软冷却/6004 模型级冷却不被签到解冻。
// 分桶口径与调度器同源（同一窗口 + 同一对入口：ReenableIfCredits 写余额/解冻，
// SetCreditsExpiring 无条件写快过架子集，含 expiring==0 的复位）。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
	}
	resp := map[string]any{"ok": true}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, diag, err := p.cfg.Upstream.UserResourceDetailedDiag(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsExpiring(uid, expiring)
	resp["credits"] = remain
	resp["credits_total"] = total
	resp["credits_expiring"] = expiring
	log.Printf("panel: checkin uid=%s msg=%q %s", logfmt.UID8(uid), checkinMsg,
		upstream.BalanceLine(remain, total, expiring, diag))
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：UserResourceDetailed → SetCredits（余额口径，**不触碰
// 冷却状态**——本按钮的既有契约）+ SetCreditsExpiring（分桶口径，含 expiring==0 的复位）。
// 响应回传 credits/credits_total/credits_expiring：面板 toast 直接显示快过期部分，
// 让运维点一下就能确认「到底有没有快过期积分」。
//
// 与调度器的差异是刻意的：后台余额刷新/签到走 ReenableIfCredits（余额恢复即解冻硬冷却），
// 而手动「余额」按钮只刷新观测量（不解冻）——保留既有契约，不顺手改解冻行为。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, expiring, diag, err := p.cfg.Upstream.UserResourceDetailedDiag(a, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsExpiring(uid, expiring)
	log.Printf("panel: balance uid=%s %s", logfmt.UID8(uid),
		upstream.BalanceLine(remain, total, expiring, diag))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "credits": remain, "credits_total": total, "credits_expiring": expiring,
	})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCheckinNow()
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunActivityNow()
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunKeepaliveNow()
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（解冻语义与后台周期
// 刷新一致：仅硬冷却账号余额恢复即解冻，软冷却/6004 模型级冷却不被解冻，见 issue #199），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// cooldownProbeRun 手动触发一轮后台冷却探活（POST /panel/api/cooldown_probe/run）。
//
// 与后台周期探活走同一入口（scheduler.RunCooldownProbeNow）：同一套目标选择
// （只探已到期的软冷却、硬冷却不探）、同一套「成功即解冻 / 失败零惩罚」语义。
// 用户场景：撞 6004 后想立刻知道上游是否已提前恢复，不必干等下一个 ticker 周期。
//
// 同步返回（不像 checkin_all 那样异步 + toast 看日志）：探活目标数通常是个位数、
// 每个请求有 60s 上限，同步等待换来「点完就知道结论」的确定性；面板紧接着拉
// overview 即是最新状态。重入锁被占（已有巡检在跑）时返回 skipped=true 而非报错
// ——那不是失败，是本轮没跑。
func (p *Panel) cooldownProbeRun(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	res := p.cfg.Scheduler.RunCooldownProbeNow()
	if res.Skipped {
		log.Printf("panel: 手动冷却探活跳过（已有巡检在执行）")
	} else {
		log.Printf("panel: 手动冷却探活完成：探 %d，解冻模型级 %d 条 / 账号级 %d 个（仍拒绝 %d，失败 %d）",
			res.Probed, res.Cleared, res.Accounts, res.StillCooling, res.Failed)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。hours 查询参数是窗口长度（小时），同时作用于汇总、
// 三张表与时序序列；缺省 72（近 3 天），上限 1440（60 天）。
//
// 这里刻意**不做**合法性判断：一律交给 Snapshot 统一回退（0 表示「未指定」，由它
// 取缺省窗口）。旧实现在这里「>1440 就钳到 1440」，而 Snapshot 内部「>1440 就回退
// 72」——两套规则并存时，响应里的生效窗口与实际统计口径可能不一致，用户看到的
// 数字对不上自己选的范围。回退策略只保留一处。
//
// 响应里的 hours 是**实际生效**值（非法入参已回退），前端据此显示「窗口：近 N 天」。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 0 // 0 = 未指定，由 Snapshot 取缺省窗口
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			hours = n
		}
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, nicks))
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
