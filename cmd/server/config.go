// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Server struct {
		// MaxBodyMB 聊天请求体大小上限（单位 MB，默认 8）。
		// 请求体超过该值直接返回 413 request_body_too_large，不再静默截断后喂给上游
		// （issue #41：截断的 JSON 让上游 unmarshal 报 unexpected EOF，网关却罚号）。
		// 0/负数视为非法 → normalize 回落默认并记录。
		MaxBodyMB int `json:"max_body_mb"`

		// MaxRotate 单请求最多换号次数（默认 3）。
		// 池内账号多时（如 4-8 个）默认 3 次试不满所有号，调大可让单请求覆盖更多账号。
		// 0/负数 normalize 回落默认 3（与 max_body_mb 的 fail fast 不同：此键的 0
		// 没有"不限"之类的合理语义，无从误导用户，回落默认更友好）。
		MaxRotate int `json:"max_rotate"`

		// ReadTimeoutSeconds 入站请求体读取窗口（秒，默认 300）。
		//
		// 语义：从「请求开始读」到「body 读完」的整段窗口上限，对应 http.Server 的
		// ReadTimeout。它不是"首字节超时"（那是 ReadHeaderTimeout，固定 30s，头很小
		// 不受本项影响），也不是出站超时（那些在 upstream 段）。
		//
		// 为什么默认从写死的 60s 提到 300s（v1.9.13 生产事故）：旧值让 8MB 上限的请求
		// 必须在 60s 内传完（>1.1Mbps 稳定上行），而 460KB+ 的聊天上下文经 TUN 代理 +
		// 跨境链路时上传耗时波动极大——一旦超 60s，io.ReadAll 报 i/o timeout，用户
		// 对话被拦腰截断（invalid_request）。300s 下 8MB 只需 27KB/s 上行，跨境链路
		// 可满足，同时远小于慢速攻击所需的时间尺度（ReadHeaderTimeout=30s 仍是慢速
		// 头的有效闸门）。
		//
		// 0/负数回落默认 300（与 max_rotate 同风格）。注意这里**刻意不采用**
		// http.Server 的「0 = 不限」语义：0 在配置文件里更可能被当成"没填/用默认"，
		// 而"不限"等于拆掉慢速 body 的闸门，与旧值 60s 的防护意图相悖；要放宽就显式
		// 给个大值（如 900），别靠 0 猜。面板修改后经 handler.SetReadTimeout 逐请求
		// 即时生效，无需重启（见 handler.armBodyReadDeadline）。
		ReadTimeoutSeconds int `json:"read_timeout_seconds"`
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5

		// auths 目录热加载：定时扫描 auth_dir，把手工上传/删除的 auth 文件增量对齐进池，
		// 免重启生效（云服务器场景：本地登录后把凭证文件上传到 auths/）。缺省开启。
		AuthWatchEnabled bool `json:"auth_watch_enabled"` // 缺省 true；false = 关闭
		AuthWatchSeconds int  `json:"auth_watch_seconds"` // 缺省 30；<=0 回落 30

		// 后台冷却探活：定时把「已到期的软冷却」（账号级 CoolSoft 到期 / 模型级
		// modelCooldowns 条目到期）拿出来各发一个最小 chat 请求，试探上游是否已提前
		// 恢复——上游重置文案保守（或提前放量）时，网关不必干等声明的墙钟。
		// 成功即解冻（模型级条目提前失效；账号级软冷却一并清）；**失败零惩罚**
		// （不推进 softStreak、不延长冷却、不喂熔断器——探活绝不能把账号越探越死）。
		// 硬冷却（CoolHard：积分耗尽）绝不探（恢复条件是签到到账，探了白花配额）。
		// 缺省开启（用户痛点：单账号部署撞 6004 后整站 503 且无从试探）。
		CooldownProbeEnabled bool `json:"cooldown_probe_enabled"` // 缺省 true；false = 关闭
		CooldownProbeMinutes int  `json:"cooldown_probe_minutes"` // 缺省 10；<=0 回落 10
	} `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// 默认值已对齐官方 WorkBuddy 桌面形态（三段式），用户仍可配完全自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与归属头
		// X-IDE-Version）。空 = 内置默认（对齐官方 5.5.4 分发包）。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（官方内置 CLI 2.137.1）。
		CliVersion string `json:"cli_version"`
		// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
		// DisableHTTP2 是否禁用出站 HTTP/2（默认 **false = 启用 h2**）。
		// 默认启用的依据（2026-09-18 CN 上游各 10 次实测）：走 TUN 代理
		// （verge-mihomo）时链路握手长——允许 h2 为 HTTP/2.0、连接复用 90%、
		// TLS 握手首次 1123ms 后续 92-98ms；禁 h2 为 HTTP/1.1 且 abort 模式下
		// 复用率 0%（每请求重新握手）→ 频繁 TLS handshake timeout。
		// 语义用「disable」而非「enable」：h2 是默认行为，本键只描述要不要关掉，
		// 缺键（JSON 零值 false）天然落在「启用」一侧，漏配不会被静默降级。
		// 显式 true = 逃生门（h2 半死流复用异常时退回 HTTP/1.1）。
		DisableHTTP2 bool `json:"disable_http2"`
		// TLSHandshakeTimeoutSeconds TLS 握手上限，默认 30（<=0 回落 30）。
		// 10s 在国内网络过紧（实测常态 >10s 会误杀重试），30s 与 dial 取齐。
		TLSHandshakeTimeoutSeconds int `json:"tls_handshake_timeout_seconds"`
		// DialTimeoutSeconds TCP 建连上限，默认 30（<=0 回落 30）。半死连接的
		// 第一道闸：连不上快速失败轮转换号，不干等系统 TCP 重传窗口。
		DialTimeoutSeconds int `json:"dial_timeout_seconds"`
		// IdleConnTimeoutSeconds 空闲连接池保留时长，默认 90（<=0 回落 90）。
		// 从 30s 回退到 90s：30s 太激进（连接刚建好就过期，h2 下一条连接承载
		// 全部流，被回收等于下个请求重新握手）；v1.9.6 用 90s 实测表现顺滑。
		IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`
		// MachineIDHeaders 是否在业务出站路径（chat/billing/模型目录）注入按 uid 固定盐
		// 派生的 X-Machine-ID / X-Session-ID（默认 true，与上游三仓默认一致）。
		// false = 完全还原旧行为（仅 X-Device-Token，无设备标识）——不想带设备指纹的
		// 部署逃生门。refresh/auth 类路径恒不注入（与开关无关）。改动需重启进程。
		MachineIDHeaders bool `json:"machine_id_headers"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// ZeroWidthSanitize 零宽字符脱敏（默认 false）：在 system 消息的指纹词内部插入
		// U+200B，可见文本不变、只破坏上游的逐字精确匹配。
		// 与上面那个独立：上面是"改写/删除"（默认开、覆盖面窄），这里是"插入不可见字符"
		// （默认关、覆盖面广）。两者叠加不冲突，开启顺序固定为先改写后插零宽。
		ZeroWidthSanitize bool `json:"zerowidth_sanitize"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer。
		Mode string `json:"mode"` // "passthrough" / "custom"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 403 风控分档），0 = 回落 max_in_flight
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）：
		// ErrClient/传输层这类「不罚号」失败连续计数，达阈临时出池。与冷却/熔断
		// 并存取更长者不叠加，成功即回池。默认 5 次 / 10m（时长固定，不做指数退避）。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的积分被标记为"快过期"，选号优先消耗。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
	} `json:"pool"`

	// Models 网关对外模型名协议（/v1/models 的 id 形态 + 裸名归属域）。
	Models struct {
		// StripRealmPrefix 缺省 true：/v1/models 输出裸模型名，不带 "cn:"/"global:" 前缀。
		// 显式前缀在入站方向仍被解析（老客户端配置零改动）；显式 false 恢复历史行为。
		StripRealmPrefix bool `json:"strip_realm_prefix"`
		// RealmPrecedence 裸模型名在「池内两域都有账号」时的默认归属域：
		// "global"（缺省）/ "cn"。单域部署（只登国际版账号）下此项无影响——
		// 裸名直接落唯一可用域。
		// 这是**软优先**而非硬规则：本域选不出可用账号时请求回落另一域，避免
		// 混合池下"本域全限流即 503"（跨域发生时有 "pool: realm fallback" 日志）。
		// 显式 "cn:"/"global:" 前缀是用户强指定，一律不回落，本域不可用即 503。
		RealmPrecedence string `json:"realm_precedence"`
		// HiddenModels 对外隐藏的模型名（面板「模型与档位」与 /v1/models 同口径）。
		// 键缺席 → 用内置默认：上游的路由策略别名 default-model / fast-model /
		// balanced-model / primary-model / deep-model（它们不是真实模型，选中后由
		// 上游按当时策略转派，倍率与窗口随时变）。
		// 显式 [] → 全部展示；非空数组 → 以该名单为准。
		HiddenModels []string `json:"hidden_models"`
		// PinnedModels 强制内置的模型条目：上游目录不给（账号差异/灰度）、但实际可调用
		// 的模型，写死一份能力快照让它稳定出现在面板与 /v1/models。
		// 键缺席 → 用内置默认（deepseek-v4.1-flash）；显式 [] → 不强行内置。
		// 上游若开始返回同名模型，自动以上游数据为准（写死条目让位）。
		PinnedModels []upstream.PinnedModel `json:"pinned_models"`
	} `json:"models"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	AuthWatchInterval      time.Duration `json:"-"` // 0 = 不启动/暂停（enabled=false）
	CooldownProbeInterval  time.Duration `json:"-"` // 0 = 暂停/未启用（enabled=false）
	ExpiringSoonDur        time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.MaxBodyMB = 8 // 请求体上限默认 8MB
	c.Server.MaxRotate = 3 // 单请求最多换号次数默认 3（与 handler 侧兜底口径一致）
	// 入站 body 读取窗口默认 300s：8MB/5min 只需 27KB/s 上行，跨境慢链路可满足；
	// 旧写死值 60s 是生产 503 事故根因（见字段注释）。
	// 值取自 server.DefaultReadTimeout（handler 侧兜底同一常量），避免"配置默认一个数、
	// handler 兜底另一个数"的静默漂移。
	c.Server.ReadTimeoutSeconds = int(server.DefaultReadTimeout / time.Second)
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	// 模型名协议缺省：去域前缀（裸名）+ 裸名默认落 global。键缺席时 Default() 的值
	// 被原样保留，只有显式配置才覆盖。
	c.Models.StripRealmPrefix = true
	c.Models.RealmPrecedence = "global"
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	// auths 目录热加载缺省开启、30 秒一轮（账号数是个位数，每轮只做目录列表 + stat）。
	c.Schedule.AuthWatchEnabled = true
	c.Schedule.AuthWatchSeconds = 30
	// 后台冷却探活缺省开启、10 分钟一轮：探活只在「有到期目标」时才发请求（无目标时
	// 整轮零出站流量），故默认开启的边际成本仅是每 10 分钟一次池遍历。
	c.Schedule.CooldownProbeEnabled = true
	c.Schedule.CooldownProbeMinutes = 10
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// 连接层四项（h2 / 握手 / 拨号 / 空闲池）：h2 默认启用——DisableHTTP2 不在此
	// 显式赋值，靠 bool 零值 false = 启用（语义见字段注释；反写成 enable 语义就会
	// 出现「漏配一项即静默退回 HTTP/1.1」）；三个超时显式给推荐值，<=0 在
	// normalize() 回落同一批值（幂等）。
	c.Upstream.TLSHandshakeTimeoutSeconds = 30
	c.Upstream.DialTimeoutSeconds = 30
	c.Upstream.IdleConnTimeoutSeconds = 90
	// MachineIDHeaders 缺省 true（同 Schedule 各开关的"缺省 true"手法：Load 先取
	// Default() 再 json.Unmarshal 覆盖，键缺席/为 null 时保留 true，只有显式 false 才关）。
	// 上游三仓默认就带 X-Machine-ID/X-Session-ID，缺省不带反而多一个"设备指纹缺失"特征。
	c.Upstream.MachineIDHeaders = true
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	// 零宽脱敏默认关：它改动的是"看不见的字节"，出问题时表现为两个看起来一样的字符串对不上，
	// 排查成本高。由使用者在面板显式开启（与上游 codebuddy2api 的默认关闭口径一致）。
	c.Features.ZeroWidthSanitize = false
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：透传客户端原始 system（对齐上游；custom 由用户显式选择）
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403
	// 修复 P1-1）；0/负数 normalize 回落本默认。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	// 连败降权（issue #114）默认开启：阈值 5（宽于熔断 3——ErrClient/传输层的判据
	// 比 5xx 弱，须更保守）、固定 10m（长于单次软冷却、短于熔断基数 30m）。
	// 「关闭」由把阈值设成很大（如 1000000）表达，不另设 enabled 开关——
	// 与熔断阈值族同风格（无 breaker_enabled，靠阈值表达）。
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 或删除该目录（程序会自动生成配置）", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	// 入站 body 读取窗口（秒）：<=0 由 normalize 回落默认 300（env 与 JSON 同口径）。
	if v := os.Getenv("WB2A_READ_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.ReadTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_MACHINE_ID_HEADERS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.MachineIDHeaders = b
		}
	}
	// 连接层四项 env 覆盖（与面板/JSON 同口径；名字对齐 JSON 键）。
	if v := os.Getenv("WB2A_DISABLE_HTTP2"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.DisableHTTP2 = b
		}
	}
	if v := os.Getenv("WB2A_TLS_HANDSHAKE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TLSHandshakeTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_DIAL_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.DialTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_CONN_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleConnTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_ZEROWIDTH_SANITIZE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.ZeroWidthSanitize = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	// 连败降权（issue #114）：三个键的 env 覆盖，与 JSON 同口径（名字对齐 JSON 键）。
	if v := os.Getenv("WB2A_DEGRADE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.DegradeThreshold = n
		}
	}
	if v := os.Getenv("WB2A_DEGRADE_COOLDOWN"); v != "" {
		c.Pool.DegradeCooldown = v
	}
	if v := os.Getenv("WB2A_DEGRADE_COOLDOWN_MAX"); v != "" {
		c.Pool.DegradeCooldownMax = v
	}
}

func (c *Config) normalize() error {
	var err error
	// max_body_mb 非法（0/负数）直接报错：0 若被静默当成默认 8MB，用户以为"不限"，
	// 大请求又被静默 413——不如 fail fast 提示显式配大上限。
	if c.Server.MaxBodyMB <= 0 {
		return fmt.Errorf("server.max_body_mb: %d 非法（需为正整数，单位 MB）", c.Server.MaxBodyMB)
	}
	// max_rotate 非正回落默认 3（处理风格参照 pool.max_in_flight_global）：0/负数
	// 在这里没有「不限」之类的合理语义（「不限换号」可用超大值表达），报错只会让
	// 手写配置的部署起不来；回落默认既保住零行为变更，又不必用户猜合法区间。
	if c.Server.MaxRotate <= 0 {
		c.Server.MaxRotate = 3
	}
	// read_timeout_seconds 非正回落默认（与 max_rotate 同风格，理由见字段注释：
	// http.Server 的「0 = 不限」语义在此刻意不采纳——拆掉慢速 body 闸门是反向风险）。
	// 回落值取 server.DefaultReadTimeout，与 handler 侧兜底同源。
	if c.Server.ReadTimeoutSeconds <= 0 {
		c.Server.ReadTimeoutSeconds = int(server.DefaultReadTimeout / time.Second)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	// 连败降权（issue #114）：时长两键空值先回落默认再解析（空串无法 ParseDuration），
	// 非法值 fail fast——与 breaker 族同风格（见上方 soft_rate_max 的空值回落）。
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权阈值：<=0 回落默认 5（与 breaker_threshold 同风格；0 无「关闭」语义，
	// 关闭请设一个极大值——见 Default() 注释）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	// 与 max_in_flight 的 0=不限语义不同——分档键的 0 没有合理语义（「global 不限」
	// 用超大值表达即可），回退分档默认最稳。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// 连接层三项超时：<=0 一律回落推荐值（与 Default() 同值，幂等）。不回落到
	// timeout_seconds——它们语义独立（握手/建连是连接层，timeout_seconds 是短
	// RPC 总时长），混用会让「短请求超时调小」意外把握手闸门也收紧。
	// DisableHTTP2 无回落：bool 的 false 就是「启用 h2」（默认），无需兜底。
	if c.Upstream.TLSHandshakeTimeoutSeconds <= 0 {
		c.Upstream.TLSHandshakeTimeoutSeconds = 30
	}
	if c.Upstream.DialTimeoutSeconds <= 0 {
		c.Upstream.DialTimeoutSeconds = 30
	}
	if c.Upstream.IdleConnTimeoutSeconds <= 0 {
		c.Upstream.IdleConnTimeoutSeconds = 90
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	// auths 目录热加载：启用时 seconds<=0 回落默认 30；关闭时 interval 保持 0（不扫描）。
	// 与余额刷新同一形态：0 是「开关关闭」的哨兵，故 <=0 一律先回落再判。
	if c.Schedule.AuthWatchEnabled {
		if c.Schedule.AuthWatchSeconds <= 0 {
			c.Schedule.AuthWatchSeconds = 30
		}
		c.AuthWatchInterval = time.Duration(c.Schedule.AuthWatchSeconds) * time.Second
	}
	// 后台冷却探活：启用时 minutes<=0 回落默认 10；关闭时 interval 保持 0（循环暂停）。
	// 与余额刷新/authwatch 同一形态（0 是「开关关闭」的哨兵，故 <=0 一律先回落再判）。
	if c.Schedule.CooldownProbeEnabled {
		if c.Schedule.CooldownProbeMinutes <= 0 {
			c.Schedule.CooldownProbeMinutes = 10
		}
		c.CooldownProbeInterval = time.Duration(c.Schedule.CooldownProbeMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	// models.realm_precedence：只认 cn/global；空（键缺席时 Default() 已置 global，
	// 此处覆盖显式 ""）与非法值统一回落 global，不静默变成 cn。
	switch strings.ToLower(strings.TrimSpace(c.Models.RealmPrecedence)) {
	case "cn":
		c.Models.RealmPrecedence = "cn"
	default:
		c.Models.RealmPrecedence = "global"
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom 模式）。
//
// mode 非法（非 custom/passthrough）启动报错，避免静默回落到某一分支；
// custom 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	return checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
