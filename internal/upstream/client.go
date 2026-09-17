// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrPromptTooLong                 // 11115「prompt is too long」→ 请求级错误（上下文超限是请求的问题非账号的问题）：不罚号、不轮转，末端透传原文
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// accountFaultMarkers 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控，
//     需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（softRateMarkers 层
// 判定，见 Classify 顺序）。故此处只收 msg 关键词 "request illegal"（auth_forbidden
// 的真实文案）。14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultMarkers = []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}

// promptTooLongMarkers 11115「prompt is too long」关键词（大小写不敏感子串匹配 +
// JSON 空格容差 code 形态）。
//
// 定位：上下文超限是**请求的问题不是账号的问题**——同一个 body 换任何账号发都会
// 超限，与内容策略拦截（ErrContentBlocked）同哲学（确定与账号无关的错误不罚号不
// 轮转，白白浪费健康号的请求配额）。双通道 marker：
//   - `"code":11115`：业务信封 code 字段（JSON 空格容差，与 11101/6004 的 code 判定
//     同形态；`"code":"11115"` 字符串形态也命中）；
//   - "prompt is too long"：msg 文案（大小写不敏感）。
//
// 只认 400/404/413 请求级状态码（见 isPromptTooLongStatus）——429 限流语义、5xx
// 服务端故障优先（与 IsModelRateLimit 只认 6004 同口径：只认确定语义的状态码）。
// 误判代价（好 body 被归 prompt_too_long）：不罚号 + 不轮转 + 透传原文，客户端
// 看到上游原文可自行排查。
var promptTooLongMarkers = []string{
	`"code":11115`,
	`"code":"11115"`,
	"prompt is too long",
}

// isPromptTooLongStatus 11115 只在请求级 4xx 上判（见 promptTooLongMarkers 注释）。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// IsPromptTooLong 报告 status+body 是否为上游 11115「prompt is too long」答复。
// handler 末端透传分支用（透传原文，不罚号不轮转）。
func IsPromptTooLong(status int, body string) bool {
	if !isPromptTooLongStatus(status) {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range promptTooLongMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// contentBlockedClientMsg 内容拦截返回给调用方的固定文案。
// [关键词] 填分类词（色情 / nsfw / 暴力 等），绝不填业务 code、账号、冷却、upstream 前缀。
const contentBlockedClientMsg = "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。"

const contentBlockedFallbackKeyword = "违禁词"

// contentBlockedKeywords 审核分类词，按优先级扫描上游文案（大小写不敏感）。
// 只收录可直接展示给调用方的分类标签，不收录错误码（如 11128）。
var contentBlockedKeywords = []string{
	"色情", "porn", "nsfw", "adult",
	"暴力", "violence",
	"政治", "politics",
	"赌博", "gambling",
	"毒品", "drug",
	"违禁词",
}

// ContentBlockedClientMessage 把上游内容拦截改写成网关防火墙口径，不含账号/错误码。
func ContentBlockedClientMessage(body string) string {
	return fmt.Sprintf(contentBlockedClientMsg, contentBlockedKeyword(body))
}

// contentBlockedKeyword 从审核文案抽出分类关键词；抽不到则回「违禁词」。
func contentBlockedKeyword(body string) string {
	text := body
	var env struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && strings.TrimSpace(env.Msg) != "" {
		text = env.Msg
	}
	lower := strings.ToLower(text)
	for _, kw := range contentBlockedKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return kw
		}
	}
	return contentBlockedFallbackKeyword
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"已签到", "already"}
var badParamsMarkerCode = `"code":11101`

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetReCN 中文文案（CN 主站）匹配「将在 … 重置」，捕获中间的时间串。
var softRateResetReCN = regexp.MustCompile(`将在 (.+?) 重置`)

// softRateResetReEN 英文文案（国际版实测）匹配「will reset at <时间>」，捕获时间串。
// 上游原文：`... your usage will reset at 2026-09-18 09:31:32 UTC+8, alternatively,
// you can switch to the other models to continue using it.`——时间后可能紧跟
// ` UTC+8` 后缀与逗号，故捕获到逗号/句号/分号（中英文标点）前，剩余后缀由
// normalizeSoftRateResetTime 统一剥掉（两种文案共用同一套 time.Parse 口径）。
// 大小写不敏感（(?i)）：上游大小写形态未完全固定，匹配本身已足够具体。
var softRateResetReEN = regexp.MustCompile(`(?i)will reset at ([^,.;，。；]+)`)

// softRateResetRes 两种文案形态的包级预编译正则（依次尝试，任一命中即解析）。
// 中文优先：CN 主站口径不变，英文形态是国际版的补充，两者不会同时命中同一 body。
var softRateResetRes = []*regexp.Regexp{softRateResetReCN, softRateResetReEN}

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// softRateTimeLen 上游时间串的固定长度（layout 的格式串与输出等长：19 字符）。
const softRateTimeLen = len(softRateTimeLayout)

// softRateUTCSuffix 上游时间串后可能出现的时区后缀（固定 UTC+8；剥掉后统一按
// softRateResetLoc 解释，不依赖容器时区）。
const softRateUTCSuffix = "UTC+8"

// modelRateLimitRe 包级预编译（选号/冷却热路径，不在函数体内 MustCompile）。
// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
var modelRateLimitRe = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
// 只认 code 字段（数字/字符串双形态），与文案语言无关：英文 body 里 code 仍是
// 数字 6004，同样命中。
func IsModelRateLimit(body string) bool {
	return modelRateLimitRe.MatchString(body)
}

// normalizeSoftRateResetTime 规整正则捕获到的时间串：剥掉尾随标点与 ` UTC+8` 后缀。
//   - 尾随标点：英文文案时间后常紧跟 ","（"… UTC+8, alternatively …"）或句号；
//   - 时区后缀：大小写不敏感剥离（上游实测 "UTC+8"），剥离后按 softRateResetLoc 解释；
//   - 超出时间串长度的残余（英文文案无标点分隔时整段说明被捕获）：仅当截断到时间
//     长度后能解析成功才截断——避免把合法长串误截成半截时间。
func normalizeSoftRateResetTime(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ",.;，。；")
	s = strings.TrimSpace(s)
	if len(s) >= len(softRateUTCSuffix) && strings.EqualFold(s[len(s)-len(softRateUTCSuffix):], softRateUTCSuffix) {
		s = strings.TrimSpace(s[:len(s)-len(softRateUTCSuffix)])
	}
	if len(s) > softRateTimeLen {
		if _, err := time.ParseInLocation(softRateTimeLayout, s[:softRateTimeLen], softRateResetLoc); err == nil {
			s = s[:softRateTimeLen]
		}
	}
	return s
}

// ParseRateReset 从任何限流响应 body 里统一解析上游明说的重置时间（UTC+8 墙钟）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// 双形态支持（同一口径，依次尝试）：
//   - 中文（CN 主站）：`将在 <时间> 重置`；
//   - 英文（国际版实测）：`will reset at <时间>`，时间后可能带 ` UTC+8` 后缀与
//     尾随逗号/句号，另有 "alternatively, you can switch to the other models…"
//     等后续说明文本——后缀与标点由 normalizeSoftRateResetTime 统一剥掉。
//
// 与旧 ParseSoftRateReset 的关键差异：不再被 IsModelRateLimit（6004）门禁。只要是
// 带重置时间文案的限流——6004 模型级、11140 "The model provider is
// rate-limiting requests." 等任意形态——都提取同一上游权威重置墙钟。是否走模型级
// 豁免、时点对齐到 until 还是 modelCooldowns，由冷却决策侧（pool）按
// IsModelRateLimit 判定，本函数只负责「把上游明说的恢复时刻抽出来」。没有时间文案
// 的限流也照常由调用方退回有界退避（绝不臆造时间）——解析失败一律返回 false，
// 不猜、不兜底造时间。
func ParseRateReset(body string) (time.Time, bool) {
	for _, re := range softRateResetRes {
		m := re.FindStringSubmatch(body)
		if len(m) < 2 {
			continue
		}
		ts := normalizeSoftRateResetTime(m[1])
		t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
		if err != nil {
			continue // 命中正则但时间非法（如 "will reset at tomorrow"）→ 试下一形态
		}
		return t, true
	}
	return time.Time{}, false
}

// ParseSoftRateReset 旧函数名的兼容别名：等价于 ParseRateReset（统一入口）。
// 保留仅为避免旧调用点/外部引用断裂；新增代码应直接使用 ParseRateReset。
func ParseSoftRateReset(body string) (time.Time, bool) {
	return ParseRateReset(body)
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 —— 真正的计费余额耗尽状态码，最严、最不可自愈，最先判。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultMarkers —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。与 429 一起纳入轮换冷却，且必须
//     先于 status==429 判定：14017 常带 429 状态码，若落到 status==429 会误归
//     soft_rate（"限流"语义不符：限流可退避等自愈，账号级故障等不来）。
//     11140 的 model 级限流变体（rate-limiting 文案）因 marker 不含该文案而天然
//     不在此层命中，后续走 softRateMarkers 层，不受影响。
//  4. status==429 —— 限流状态码兜底（本层先于 hardMarkers，fork-scan-absorb T-3）：
//     429 body 高频携带 "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，
//     若 hardMarkers 先判会把限流误归 ErrHardCredit 硬冷却到次日 04:00，白扔号约
//     12h。状态码是比关键词更权威的信号：上游既然给了 429，就按限流语义处理
//     （宁可短冷却自愈，不可长冷却弃号）；真正的余额耗尽由 402（第 1 层）捕获，
//     非 429 状态码的 quota 措辞仍走下方 hardMarkers（第 5 层）。
//  5. hardMarkers —— 非 429 响应携带计费关键词（200 业务信封 / 403 信封等）。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit；429 场景已由
//     第 4 层前置接管（issue #28 记录的非 429 反向误判风险保持原样，待上游
//     原始响应确认后再定）。
//  6. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时已被第 4 层
//     短路，结果同为 soft_rate。
//  7. 11115（IsPromptTooLong）—— 「prompt is too long」请求级语义：判在 404/5xx 与
//     通用 4xx 兜底之前（404 上打 11115 若落 ErrNotFound 会误冷却账号——上下文超限
//     与账号无关）。只认 400/404/413（429/5xx 已在上方各自状态码层短路）。
//  8. 404 / 5xx —— 与限流无关的常规分类。
//  9. 内容策略/参数错误/其他 4xx —— 通用兜底（内容策略拦截须先于通用 ErrClient，
//     前者是误报信号、不罚账号，由网关降级重试处理）。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	// sessionDead / accountFault 先于 status==429（原顺序已如此，此处只是跟随
	// 429 前移保持相对次序）：账号级终态等不来自愈，限流状态码不得掩盖它们
	// （429+14017 必须 accountFault，401+12153 混排 "rate limit" 必须 sessionDead）。
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range accountFaultMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrAccountFault
		}
	}
	// status==429 先于 hardMarkers（fork-scan-absorb T-3，本次修复点）：限流响应 body
	// 高频携带 "quota exceeded"/"额度不足" 等跨两界措辞，hardMarkers 先判会误归
	// ErrHardCredit 硬冷却到次日 04:00。402 真余额在上层已判；非 429 的 quota
	// 措辞仍走下方 hardMarkers，历史语义不变。
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	// 11115「prompt is too long」：判在 404/5xx/内容策略/参数错误/通用 4xx
	// 之前——请求级语义最具体（上下文超限），须先于宽泛的状态码兜底（404 兜底会
	// 误归 ErrNotFound 只冷却不透传；ErrClient 只换号，浪费健康号配额）。只认
	// 请求级 4xx 状态码（见 isPromptTooLongStatus），429/5xx 在上方已被各自
	// 状态码层短路（限流/服务端故障语义优先）。
	if IsPromptTooLong(status, body) {
		return ErrPromptTooLong
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
		// 模型权限，值得再试一次）。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按 realm 分层桶（cn/global）：同模型名跨域探测的 effort 集合可能不同，
	// 混桶会互相污染（C-2）。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string
	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool
	// ZeroWidthSanitize 零宽字符脱敏开关（默认 false）：在 system 消息的指纹词内部
	// 插入 U+200B，破坏上游逐字匹配。与 SanitizeFingerprints 独立，可各自开关。
	ZeroWidthSanitize bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
	// 空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空）。
	UserAgent string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + X-IDE-Version）。
	// 空 = 内置默认（对齐官方 5.5.4 分发包）。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认（官方内置 CLI 2.137.1）。
	CliVersion string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	ClientName string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 传入 ChatStream。
	PassthroughIP bool

	// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底来源：config upstream.device_token。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceToken string

	// DeviceTokenFile 设备 token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
	DeviceTokenFile string

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 时即便用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		WebBaseCN:            "https://www.workbuddy.cn",
		// GlobalEnabled 缺省 true（与 config global.enabled 缺省 true 一致；纯 CN 部署行为不变：
		// CN 账号恒判 cn，global base 只在 realm=global 的账号上被使用）。
		GlobalEnabled: true,
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// 路径常量：CN 现状路径（chatCompletionsPath）与 global 双候选路径。
const (
	chatCompletionsPath   = "/v2/chat/completions"
	globalChatConsolePath = "/console/chat/completions"
)

// chatPaths 按 realm 返回 chat 端点路径候选序列：
// global → [v2, console]（v2 优先，404/405 时 fallback）；cn → [v2]（现状逐字，零回归）。
//
// 2026-09-17 实测修正：/console 通道对请求内容更敏感——同一 body 直连 /console
// 返回 403 WAF Block Page、/v2 返回 200（触发为历史文本的累计安全评分，与体积无关，
// 4KB 触发段在 /console 稳定 403、在 /v2 稳定 200）。私人构建（fb07cc9）与 intl
// 项目均直接走 /v2。原 [console, v2] 顺序来自 PLAN R9，实测会误伤长历史对话
// （ZCode 会话历史含安全术语时 console 首路径必 403 → 单账号池 503）。
// 改为 v2 优先后，console 仅作 v2 404/405 的兼容兜底（上游新旧路径分叉场景）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{chatCompletionsPath, globalChatConsolePath}
	}
	return []string{chatCompletionsPath}
}

func chatFallbackHTTPStatus(status int) bool { return status == 404 || status == 405 }

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// realm 为账号 Realm()（cn/global），供 efforts 缓存分桶（跨域 effort 集合不互相污染）。
//
// WB2A_DEBUG_REASONING 非空时，改写前后各打一行"思考字段"诊断（in/out）：
// 用于回答"我调了 off/low/medium/high 感觉一样"到底是客户端没发、还是网关改写了、
// 还是模型不在能力缓存里。该诊断是唯一能看到出站档位的地方——正常日志只在降级时才出声。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts := c.effortsSnapshot(realm)
	defaults := c.defaultEffortsSnapshot(realm)
	logReasoning("in ", body, efforts, defaults)
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints, c.ZeroWidthSanitize, efforts, defaults)
	logReasoning("out", body, efforts, defaults)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)
	return body
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 workbuddy.ai，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
// refreshIOTimeout 刷新端点网络 I/O 上限（两段式锁外执行，防上游 hang 长占锁）。
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
// 实测上游响应恒 5184000=60d）。超限视为上游脏数据，不写 ExpiresAt（保留旧值），
// 防止 NeedsRefresh 永假导致 token 永不刷新反而真过期失效。
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（两段式，缩小持锁窗口）：
//   - 锁内仅做「读 refreshToken 快照」与「校验未变后写回新 token」两小段内存操作；
//   - 网络 I/O（doJSON）在**锁外**执行，带 30s ctx 超时——避免上游 hang 时长时间
//     独占 a.mu，阻塞同账号的 SaveAtomic / 其他刷新（issue:持锁 120s I/O）。
//   - 写回前重新校验快照一致性：若锁外期间另一 goroutine 已完成刷新（refreshToken
//     已变），本次结果直接采用（新 token 已生效），不再重复写回。
func (c *Client) RefreshToken(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	endpoint := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外，30s 上限）。
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	// 写回守卫是 AND 语义：锁外期间另一刷新已完成 → 两 token 必同时变化（上游
	// refresh 响应 accessToken/refreshToken 总是一起 rotate，写回也同时写两个），
	// AND 即「并发刷新已完成」判据；AND 与 OR 在真实形态下等价。唯 OR 会额外放弃的
	// 「只有单 token 变化」（如手工只改 auth 文件一个字段）不构成放弃条件——本次
	// 结果覆盖手工编辑。
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （两个并发刷新拿到的新 token 都有效，后写会覆盖先写，但二者等价可用；
		// 提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 实测上游响应恒带 expiresIn=5184000（60d）——缺省分支仅为防御，保留旧值
	// 避免过期判定漂移。同理，超过 10 年的 expiresIn 按脏值处理保留旧值：
	// 超量级值只会是上游脏数据，照写会把 ExpiresAt 推到荒谬未来 → NeedsRefresh
	// 永假 → token 永不刷新反而真过期失效。
	//
	// 守卫写法：用 int64 比较（秒 < 上限秒数），**不**写
	// time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax——后者
	// 在 int64 溢出窗口内（ExpiresIn 极大时 Duration 溢出成负数）会判为「在上限内」
	// 而写回过去时刻，直接引发刷新风暴。int64 秒比较无溢出。
	if tok.ExpiresIn > 0 && tok.ExpiresIn < int64(refreshTokenExpiresInMax/time.Second) {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 需要客户端断开联动的调用方用 ChatStreamContext 传入请求 ctx。
//
// global realm：先打 /v2/chat/completions，404/405 时同一 base 二次换 /console/chat/completions
// （v2 优先，2026-09-17 实测修正；console 仅作路径分叉兼容兜底）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但出站请求挂在调用方的 reqCtx 上：
// handler 传 r.Context() → 客户端断开时上游请求随之取消（不再白白消耗账号积分
// 与上游连接继续生成无人消费的流）。成功流的 cancel 仍由 monitorBody 的 Close
// 接管（reqCtx 取消与显式 Close 任一触发即断）。
// global 首次路径 404/405 时换 fallback 路径重试；ensureConsoleSystem 在 prepareBody 后统一套用
// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11128）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// global 首次路径 404/405 时换 fallback 路径重试。
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx 的 cancel 在每个出口显式调用（Do 失败 / ≥400 / 成功分支移交 monitorBody），
	// 循环本身各分支必 return——无循环尾兜底代码（此前外层 var cancel 从未赋值 + 尾部
	// 不可达 cancel() 是潜伏 nil-panic，已删；chatPaths 恒非空由构造保证）。
	for attempt, path := range c.chatPaths(a) {
		endpoint := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
		// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			cancel()
			log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			// body 读失败（掐流/截断）→ 传输层错误：半截 raw 不交回调用方进 Classify，
			// 否则 handler 侧 applyErrorPolicy 会按误判分类罚号。
			if rerr != nil {
				log.Printf("chat_stream uid=%s: read body: %v", a.UID, rerr)
				return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
			}
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
				a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
			// global 首次路径 404/405 → 换 fallback 路径重试；其余状态码直接返回。
			if attempt < len(c.chatPaths(a))-1 && chatFallbackHTTPStatus(resp.StatusCode) {
				continue
			}
			return nil, resp.StatusCode, raw, nil
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
		return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	panic("unreachable: chatPaths is never empty") // for range 空集时编译器仍要求兜底 return；chatPaths 恒非空（构造保证），永不触达
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID                 string
	Name               string
	ContextWindow      int64    // = maxInputTokens
	MaxTokens          int64    // = maxOutputTokens（思考与最终回答共享此预算，上游无独立思考上限字段）
	MaxAllowedSize     int64    // = maxAllowedSize（单请求体大小上限，通常等于 maxInputTokens）
	Efforts            []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort      string   // reasoning.defaultEffort（新模型键）或 reasoning.effort（老模型键）；空=未返回
	CanDisableThinking bool     // reasoning.canDisableThinking：思考可关（off 档可用）
	SupportsReasoning  bool     // supportsReasoning：模型支持思考
	SupportsImages     bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）
	Credits            string   // credits：积分倍率（如 "x0.79"）
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含 text-to-image：图片生成模型，非本网关用途。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if t == "text-to-image" {
			return true
		}
	}
	return false
}

// codeBuddyIDEUA /v3/config 要求能解析出 CodeBuddy 版本号的 UA。
// CLI 三段式 WorkBuddy UA 会拿到精简目录（flash 输出 128K、无 supportedEfforts）；
// 官方 IDE 头 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 才返回完整能力
// （flash：393216 + low/high/max）。
// 版本号需随上游 IDE 发版跟进：UAn 版本过旧时该端点可能同样返回精简目录。
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// modelsPaths 按 realm 返回模型目录端点候选序列（按序尝试，首个成功即采用）。
//
//   - global（国际站）：/v2 家族优先。国际站的 /console 家族返回 500 + HTML 网关
//     错误页（见 globalModelsProbePaths 注释与线上实测），此前 panel「拉取模型」
//     拿到 502 + `models api status 500: <html>…500 Internal Server Error…` 即此故；
//   - cn（国内站）：只有 /console 家族。
func (c *Client) modelsPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{
			"/v2/enterprises/personal/models",
			"/console/enterprises/personal/models",
		}
	}
	return []string{"/console/enterprises/personal/models"}
}

// ModelFetchDiag 一次模型目录拉取的诊断快照。
//
// 存在的理由：面板「模型与档位」与 /v1/models 看到的都是**筛选后**的结果，
// 「上游到底给了什么、哪一步筛掉的」不可见时，只能靠猜（例如"deepseek 怎么不在
// 列表里"——是上游没给、还是 agents 名单没列它、还是被 nonChatModel 滤了）。
// 本结构把这条链路摊开，只读诊断，不参与任何路由决策。
type ModelFetchDiag struct {
	Path        string            `json:"path"`          // 成功的端点路径
	CliAgentIDs []string          `json:"cli_agent_ids"` // agents[name=cli].models；空 = 上游未给 agents（走全表兜底）
	Agents      []ModelFetchAgent `json:"agents"`        // 上游 agents 全量：核对"只取 cli"有没有漏掉模型
	RawModelIDs []string          `json:"raw_model_ids"` // 上游 models[] 的原始 id（nonChatModel 过滤前）
	AllModelIDs []string          `json:"all_model_ids"` // 通过 nonChatModel 过滤后的全部模型 id（保序）
	Dropped     []string          `json:"dropped"`       // 在 AllModelIDs 但未进入结果（agents 未列 / disabled）
}

// ModelFetchAgent 上游 agents 数组的一项（只取诊断需要的字段）。
type ModelFetchAgent struct {
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	Models []string `json:"models"`
}

// FetchModels 调上游动态模型接口，端点按 realm 选择（modelsPaths）。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
// 模型目录决定「能调哪些模型」；IDE /v3/config 覆盖同名模型的窗口 / 思考档
// （覆盖用 c.chatBase(a)，因此同样 realm 感知；失败则静默保留目录字段）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	out, _, err := c.FetchModelsDiag(a)
	return out, err
}

// FetchModelsDiag 同 FetchModels，另外返回诊断快照（供面板展示"上游给了什么"）。
func (c *Client) FetchModelsDiag(a *auth.Auth) ([]ModelInfo, ModelFetchDiag, error) {
	var lastErr error
	var lastDiag ModelFetchDiag
	for _, path := range c.modelsPaths(a) {
		out, diag, err := c.fetchModelsOnce(a, path)
		if err == nil {
			return out, diag, nil
		}
		lastDiag, lastErr = diag, err
	}
	return nil, lastDiag, lastErr
}

// fetchModelsOnce 单个候选端点拉取 + 解析（含 /v3/config 能力覆盖与 effort 缓存刷新）。
func (c *Client) fetchModelsOnce(a *auth.Auth, path string) ([]ModelInfo, ModelFetchDiag, error) {
	diag := ModelFetchDiag{Path: path}
	// 局部变量名避开 url（本包已 import net/url，同名会造成阅读混淆）。
	endpoint := c.chatBase(a) + path
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, diag, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, diag, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误（handler 侧该路径不 NoteError）。
		return nil, diag, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, diag, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                string   `json:"id"`
				Name              string   `json:"name"`
				MaxInputTokens    int64    `json:"maxInputTokens"`
				MaxOutputTokens   int64    `json:"maxOutputTokens"`
				MaxAllowedSize    int64    `json:"maxAllowedSize"`
				Disabled          bool     `json:"disabled"`
				Credits           string   `json:"credits"`
				SupportsReasoning bool     `json:"supportsReasoning"`
				SupportsImages    bool     `json:"supportsImages"`
				Tags              []string `json:"tags"`
				Reasoning         struct {
					Effort             string   `json:"effort"`        // 老模型键（auto/hy3/glm-5.2 系）
					DefaultEffort      string   `json:"defaultEffort"` // 新模型键（glm-5.3 系只返回这个）
					CanDisableThinking bool     `json:"canDisableThinking"`
					SupportedEfforts   []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, diag, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, diag, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	diag.CliAgentIDs = cliIDs // 空 = 上游未给 cli agents（下游走全表兜底并记录）
	// agents 全量入诊断：我们只取 name=="cli"，若上游还有 ide/buddyapp 等其他 agent
	// 且它们列了 cli 没有的模型（"官方客户端看得到、网关看不到"的场景），这里能看出来。
	for _, ag := range env.Data.Agents {
		diag.Agents = append(diag.Agents, ModelFetchAgent{
			Name: ag.Name, Count: len(ag.Models), Models: ag.Models,
		})
	}
	type parsed struct {
		mi       ModelInfo
		disabled bool
	}
	dynMap := make(map[string]parsed, len(env.Data.Models))
	allIDs := make([]string, 0, len(env.Data.Models)) // 保上游返回序，供无 agents 时兜底
	for _, m := range env.Data.Models {
		// 非对话模型（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
		// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		def := m.Reasoning.Effort
		if def == "" {
			def = m.Reasoning.DefaultEffort // 新旧双键兼容：glm-5.3 系只返回 defaultEffort
		}
		dynMap[m.ID] = parsed{ModelInfo{
			ID:                 m.ID,
			Name:               normalizeModelName(m.Name),
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			MaxAllowedSize:     m.MaxAllowedSize,
			Efforts:            m.Reasoning.SupportedEfforts,
			DefaultEffort:      def,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsImages:     m.SupportsImages,
			Credits:            m.Credits,
		}, m.Disabled}
		allIDs = append(allIDs, m.ID)
	}
	// 原始 id（过滤前）入诊断：区分"上游压根没返回"与"被 nonChatModel 滤掉了"。
	diag.RawModelIDs = make([]string, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		diag.RawModelIDs = append(diag.RawModelIDs, m.ID)
	}
	// agents 名单决定「能调哪些」（CN /console 形态）。国际站 /v2 形态可能不带 agents：
	// 此时以过滤后的全表为可用集，而不是整条路径判失败（否则 global 目录永远拿不到
	// 倍率/档位/上下文，面板那几列全空）。
	if len(cliIDs) == 0 {
		cliIDs = allIDs
	}
	if len(cliIDs) == 0 {
		return nil, diag, fmt.Errorf("models api returned empty list")
	}
	diag.AllModelIDs = allIDs
	seen := make(map[string]bool, len(cliIDs))
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if pm, ok := dynMap[id]; ok && !pm.disabled {
			out = append(out, pm.mi)
		}
	}
	if len(out) == 0 {
		return nil, diag, fmt.Errorf("models api returned empty list")
	}
	// 记账：agent 名单里列了、但最终没进结果的（disabled / 不在 models 表里）。
	inOut := make(map[string]bool, len(out))
	for _, mi := range out {
		inOut[mi.ID] = true
	}
	for _, id := range diag.AllModelIDs {
		if !inOut[id] {
			diag.Dropped = append(diag.Dropped, id)
		}
	}
	if overlay, err := c.fetchV3ConfigModelMap(a); err == nil && len(overlay) > 0 {
		out = mergeModelCapabilities(out, overlay)
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	c.efforts[realmKey(a.Realm())] = cache
	c.defaultEfforts[realmKey(a.Realm())] = defCache
	c.effortsMu.Unlock()
	return out, diag, nil
}

// v3ConfigDomain /v3/config 的 X-Domain：优先账号落盘 domain，否则 chatBase host。
func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		if d := strings.TrimSpace(a.Domain); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(chatBase); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

// fetchV3ConfigModelMap 拉官方 IDE 配置目录，按模型 id 建能力表。
// 该端点对 UA 敏感：必须带 CodeBuddy/CodeBuddyIDE 版本，否则 400 code=12403。
func (c *Client) fetchV3ConfigModelMap(a *auth.Auth) (map[string]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", v3ConfigDomain(a, c.chatBase(a)))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", codeBuddyIDEUA)
	c.injectCodeBuddyRequest(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3/config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                string `json:"id"`
				Name              string `json:"name"`
				MaxInputTokens    int64  `json:"maxInputTokens"`
				MaxOutputTokens   int64  `json:"maxOutputTokens"`
				MaxAllowedSize    int64  `json:"maxAllowedSize"`
				Credits           string `json:"credits"`
				SupportsReasoning bool   `json:"supportsReasoning"`
				SupportsImages    bool   `json:"supportsImages"`
				Reasoning         struct {
					Effort             string   `json:"effort"`
					DefaultEffort      string   `json:"defaultEffort"`
					CanDisableThinking bool     `json:"canDisableThinking"`
					SupportedEfforts   []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3/config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config code=%d", env.Code)
	}
	out := make(map[string]ModelInfo, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		def := m.Reasoning.DefaultEffort
		if def == "" {
			def = m.Reasoning.Effort
		}
		out[m.ID] = ModelInfo{
			ID:                 m.ID,
			Name:               normalizeModelName(m.Name),
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			MaxAllowedSize:     m.MaxAllowedSize,
			Efforts:            m.Reasoning.SupportedEfforts,
			DefaultEffort:      def,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsImages:     m.SupportsImages,
			Credits:            m.Credits,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// mergeModelCapabilities 用 IDE /v3/config 覆盖 CLI 目录里同 id 的窗口与思考档。
// 只填 overlay 里有值的字段，避免空配置把 CLI 已解析结果抹掉。
func mergeModelCapabilities(base []ModelInfo, overlay map[string]ModelInfo) []ModelInfo {
	if len(overlay) == 0 {
		return base
	}
	for i, mi := range base {
		ov, ok := overlay[mi.ID]
		if !ok {
			continue
		}
		if ov.ContextWindow > 0 {
			mi.ContextWindow = ov.ContextWindow
		}
		if ov.MaxTokens > 0 {
			mi.MaxTokens = ov.MaxTokens
		}
		if ov.MaxAllowedSize > 0 {
			mi.MaxAllowedSize = ov.MaxAllowedSize
		}
		if len(ov.Efforts) > 0 {
			mi.Efforts = ov.Efforts
		}
		if ov.DefaultEffort != "" {
			mi.DefaultEffort = ov.DefaultEffort
		}
		if ov.CanDisableThinking {
			mi.CanDisableThinking = true
		}
		if ov.SupportsReasoning {
			mi.SupportsReasoning = true
		}
		if ov.SupportsImages {
			mi.SupportsImages = true
		}
		if ov.Credits != "" {
			mi.Credits = ov.Credits
		}
		if ov.Name != "" {
			mi.Name = ov.Name
		}
		base[i] = mi
	}
	return base
}

// UserResource 查询账号积分余额与总额度（所有套餐聚合）。remain 负值钳 0；
// total 取与 remain 同源的额度字段（CycleCapacitySize 优先，无周期额度退
// CapacitySize），上游缺 size 的套餐按 remain 兜底，保证百分比不超 100%。
// CreditPackage 单个积分包的构成明细（面板「积分构成」用）。
//
// 两个账号即使任务完成度完全一致，余额也可能相差上千——差别藏在包的**面额与
// 来源**里（「国内运营裂变包」「拉新权益包」按次发放，面额 6~1500 不等）。
// 只看聚合值看不出这件事，所以把逐包明细暴露出来。
type CreditPackage struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
	// EndTime 该包的周期结束时间（上游 ExpiredTime / PackageEndTime 二者取有值者）。
	EndTime string `json:"end_time,omitempty"`
	// CreatedAt 发放时刻，RFC3339。**这是区分「首登赠送」与「活动奖励」的唯一依据**：
	// 两类包的 PackageName 与 PackageCode 完全相同（例如都是「国内运营裂变包」+
	// TCACA_code_007_*），只看名字无法区分，只有时间能说明它是不是账号首次授权那刻发的。
	CreatedAt string `json:"created_at,omitempty"`
	// PackageCode / SubProductCode 上游的包类型标识。同 Name 不同 Code 的包可能
	// 是不同来源；同 Code 不同面额则是同来源分批发放（首登 1500 与活动 300 即如此）。
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	// Cycle 为 true 表示按周期发放的包（读 Cycle* 字段），否则读 Capacity*。
	Cycle bool `json:"cycle,omitempty"`
}

// CreditPackages 返回账号当前的逐包构成。remain/size 为各包求和。
//
// 字段选择与 UserResourceDetailed 的聚合口径一致：CycleCapacitySize > 0 时按
// 周期字段算，否则按 Capacity 字段算——两条路径不能混，否则同一个包会被算两次。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, 0, 0, err
	}
	// 注意层级：doJSON 已经解过 apiEnvelope 并返回 env.Data，所以这里从
	// Response 开始解析——**不能**再套一层 Code/Data，否则 Accounts 恒为空，
	// 表现为「每个号都 0 个包」（实测踩过）。
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacitySize        int64  `json:"CapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					// 到期时间字段名在上游同时存在两种口径，都读，谁有值用谁。
					ExpiredTime    string `json:"ExpiredTime"`
					PackageEndTime string `json:"PackageEndTime"`
					// 发放时刻（epoch 毫秒）。
					CreateTime     int64  `json:"CreateTime"`
					PackageCode    string `json:"PackageCode"`
					SubProductCode string `json:"SubProductCode"`
					SubProductName string `json:"SubProductName"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))
	var sumRemain, sumSize int64
	for _, p := range packs {
		cp := CreditPackage{
			Name:           p.PackageName,
			PackageCode:    p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
		}
		if p.ExpiredTime != "" {
			cp.EndTime = p.ExpiredTime
		} else {
			cp.EndTime = p.PackageEndTime
		}
		// CreateTime 是 epoch 毫秒；0 表示上游没给，留空而不是伪造 1970。
		if p.CreateTime > 0 {
			cp.CreatedAt = time.UnixMilli(p.CreateTime).Format(time.RFC3339)
		}
		if p.CycleCapacitySize > 0 {
			cp.Cycle = true
			cp.Remain, cp.Size = p.CycleCapacityRemain, p.CycleCapacitySize
			cp.Used = cp.Size - cp.Remain
			if p.CycleCapacityUsed > cp.Used {
				cp.Used = p.CycleCapacityUsed
				cp.Remain = cp.Size - cp.Used
			}
			if cp.Remain < 0 {
				cp.Remain = 0
			}
		} else {
			cp.Remain, cp.Used, cp.Size = p.CapacityRemain, p.CapacityUsed, p.CapacitySize
			if cp.Used == 0 && cp.Size > cp.Remain {
				cp.Used = cp.Size - cp.Remain
			}
		}
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	// 面额降序：大包一眼可见，正是差异最可能出现的地方。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out, sumRemain, sumSize, nil
}

func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(a, 0)
	return remain, total, err
}

// packageEndLayout 上游套餐到期时间的墙钟格式（UTC+8，与 softRateResetLoc 同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// UserResourceDetailed 在 UserResource 基础上额外返回「快过期」积分子集：
// soon > 0 且套餐 PackageEndTime 解析成功且到期时刻 ≤ now+soon 的余额计入 expiring
// （pool 据此优先消耗，避免官方活动赠送的奖励积分到期作废）；soon ≤ 0 时 expiring
// 恒 0（禁用分桶，行为与引入前一致）。expiring 是 remain 的一部分。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					PackageEndTime      string `json:"PackageEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r, size int64
		switch {
		case acct.CycleCapacitySize > 0:
			r, size = acct.CycleCapacityRemain, acct.CycleCapacitySize
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r, size = acct.CycleCapacityRemain, acct.CycleCapacitySize
		default:
			r, size = acct.CapacityRemain, acct.CapacitySize
		}
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		// 分桶：仅 soon>0 且能解析出有效到期时间、且确实在窗口内 → expiring。
		if soon > 0 && r > 0 && acct.PackageEndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, acct.PackageEndTime, softRateResetLoc); perr == nil {
				if !end.After(now.Add(soon)) {
					expiring += r
				}
			}
		}
	}
	return remain, total, expiring, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
	return err
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
