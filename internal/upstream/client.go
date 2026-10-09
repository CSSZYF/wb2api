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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
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
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 请求级错误：不罚号、不轮转，末端 400 透传原文（11133 同归本类但保留轮转，见 IsBadParamsBody）
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrPromptTooLong                 // 11115「prompt is too long」→ 请求级错误（上下文超限是请求的问题非账号的问题）：不罚号、不轮转，末端透传原文
	ErrInvalidImage                  // 11135「invalid_image_data」→ 请求级终态（图片数据无效是请求的问题非账号的问题）：不罚号、不轮转，末端透传原文
	ErrModelBlocked                  // 11102「该后端无此模型」→ (账号,模型) 负缓存避让，切模型/切账号
	ErrWafBlock                      // 403 + 非业务信封体（WAF 拦截页/空体）→ 账号软冷却 + 抖动退避（WAF 403 修复 P0-1）
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
	case ErrInvalidImage:
		return "invalid_image"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrWafBlock:
		return "waf_block"
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
	// RetryAfter 上游明示的等待时长（Retry-After 秒 / retry-after-ms /
	// x-ratelimit-reset 头解析，见 ParseRetryAfter）。零值 = 上游未明示，
	// 冷却时长回落调用方计算值。挂载点选在 Error 信封：Kind 决定「罚不罚」，
	// RetryAfter 决定「罚多久」，同为上游响应的一等公民，与 Kind/Status/Msg
	// 同居信封而非另开解析层（WAF 403 修复 P1-2）。
	RetryAfter time.Duration
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
// "frequency limit" 是上游英文 6004 文案的原句形态（实测原文：
// "usage exceeds frequency limit, but don't worry, your usage will reset at …"）：
// 该文案在非 429 状态码返回时既无 "rate limit" 也无 "usage limit" 子串，
// 只靠 429 状态码兜底会漏判（频率限制同样是限流语义，须进软冷却而非只换号）。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"frequency limit", // 上游英文 6004 原文（usage exceeds frequency limit）
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

// isInvalidImageStatus 11135 只在请求级 4xx 上判（与 isPromptTooLongStatus 同口径，
// 见 Classify 里 11135 分支注释）：429/5xx 的限流/服务端故障语义更权威。
func isInvalidImageStatus(status int) bool {
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
// 与账号健康无关——不罚号，且**不轮转**（请求级终态，吸收上游 PR #99：
// 11101 发生在上游解析请求体阶段，还没走到模型路由，换号必然同样失败）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"已签到", "already"}
var badParamsMarkerCode = `"code":11101`

// IsBadParamsBody 报告上游错误 body 是否属 **11101 请求体解析失败**（"Unmarshal
// chat params failed" / code 11101）。handler 的请求级终态分支据此把该形态与
// **11133 model_param_invalid** 区分开——两者同归 ErrBadParams 分类，但处置不同：
//
//   - 11101（本函数命中）：上游在**解析请求体**阶段就拒了，还没走到模型路由，
//     同一 body 换任何账号结果相同 → 终止轮转、400 透传原文（吸收上游 PR #99）。
//   - 11133（本函数不命中，见 isModelParamInvalid）：参数被**模型供应商**拒绝，
//     可能是账号侧后端差异（11102 的 (账号,模型) 负缓存证明同模型在不同账号上
//     可用性不同）→ 保留轮转（零动作，不罚号、不喂连败）。
//
// 单一事实来源：判定词表与 Classify 共用（badParamsMarkerMsg / badParamsMarkerCode），
// 不在 handler 里另写一套子串。
func IsBadParamsBody(body string) bool {
	return strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode)
}

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

// modelBlockCode 明确指向「该后端无此模型」的业务 code（上游 11102）。
const modelBlockCode = "11102"

// modelBlockMsgMarker 11102 答复的确定性文案（官方 error message 固定短语）。
// 只收这个窄短语，不收 "model ... not found" 宽正则——后者会误伤其他业务的
// not found 措辞（宁缺毋滥）。
const modelBlockMsgMarker = "service info not found"

// ModelBlockReason 11102 负缓存条目在 pool.modelCooldowns 里的 reason 前缀。
// handler 写 BlockModelBackoff；pool.BlockModelClear 按 "11102" 前缀识别条目
// （与 6004 条目的 "6004 model rate limit" reason 互不干扰，两者共存于同一 map 键）。
const ModelBlockReason = "11102 model not available"

// IsModelBlocked 报告 body 是否是「该后端无此模型」(11102) 的确定性答复。
//
// 只比对 code/msg 等独立字段，绝不做整段文本子串匹配：错误体还带 requestId 等字段，
// 拿整段文本匹配会把 "11102" 撞在 ID 上、误避让一个本来能用的模型。判定 = code 字段
// 精确等于 "11102"，或 msg/message 字段命中窄短语 "service info not found"（任一命中
// 即真）。只看 400/404：429 带 11102 属限流语义（不在此判定范围）。字段遍历覆盖顶层
// 与 error 子对象两层（对齐上游 OpenAI 风格信封的 error 嵌套形态）。
func IsModelBlocked(status int, body string) bool {
	if (status != http.StatusBadRequest && status != http.StatusNotFound) || body == "" {
		return false
	}
	// 轻量预检：body 既无 "11102" 又无 marker 时直接短路（大多数 4xx 零分配返回）。
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	code, msg := "", ""
	for _, node := range nodes {
		for _, key := range []string{"code", "errCode", "error_code"} {
			if v, ok := node[key]; ok && v != nil && code == "" {
				code = strings.TrimSpace(fmt.Sprint(v))
			}
		}
		for _, key := range []string{"msg", "message"} {
			if v, ok := node[key].(string); ok && v != "" && msg == "" {
				msg = strings.TrimSpace(v)
			}
		}
	}
	if code == modelBlockCode {
		return true
	}
	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// hasBusinessCode 报告 JSON 错误信封里是否存在**精确等于** want 的业务码
// （吸收上游 d47219b）。上游信封形态在顶层 / error / data / extError 之间来回变，
// 故遍历解码后的结构找名为 "code" 的字段。
//
// 与 codeMarker（hint.go）的分工：那个是**子串**口径、用于补充说明（宁宽勿漏）；
// 本函数是**结构化精确**口径、用于分类决策——判错的代价是把限流号硬冷却到次日
// 04:00（12h），故不接受 "requestId":"req-14018" 这类子串噪声。
// 数字与字符串形态都认（`"code":14018` / `"code":"14018"`），JSON 空白天然容差
// （走 json.Unmarshal，不做字面量匹配）。
func hasBusinessCode(body, want string) bool {
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch node := value.(type) {
		case map[string]any:
			if code, ok := node["code"]; ok && strings.TrimSpace(fmt.Sprint(code)) == want {
				return true
			}
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}

// hasBusinessEnvelope 报告错误 body 是否携带上游业务信封形态（JSON 且含
// `"code":` 或 `"msg":` 字段）。WAF 403 判定（IsWafBlocked）用「无业务信封」
// 区分 WAF 拦截页（HTML/空体/纯文本）与上游业务层 403（带 code/msg 信封，
// 正常走既有分类）。JSON 解析不做：信封存在性只需字段名命中——畸形 JSON 但含
// `"msg":` 字样仍按业务响应保守处理（宁漏判 WAF 也不误罚业务 403，后者有各自
// 的权威分类）。
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// IsWafBlocked recognizes non-business 403 responses and explicit WAF HTML
// served with status 503. Other 5xx responses retain server-error semantics.
func IsWafBlocked(status int, body string) bool {
	if hasBusinessEnvelope(body) {
		return false
	}
	if status == http.StatusForbidden {
		return true
	}
	// Tencent also serves its WAF HTML with HTTP 503. Only identify explicit
	// block pages; ordinary 503 maintenance pages remain server errors.
	lower := strings.ToLower(body)
	return status == http.StatusServiceUnavailable &&
		strings.Contains(lower, "<html") &&
		(strings.Contains(lower, "<title>waf block page</title>") ||
			(strings.Contains(lower, "tencent cloud waf") && strings.Contains(lower, "access blocked")))
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列（P1-2）：
// retry-after（秒，RFC 7231）/ retry-after-ms（毫秒）/ x-ratelimit-reset
// （epoch 秒或毫秒，取 now+ 剩余量）。大小写不敏感（http.Header.Get 已归一）。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果的上限（超过视为上游异常值丢弃，回落本地计算）：
// 与 pool 的 softRateMax 默认 2h 同量级（上游不该明示比冷却封顶更长的等待）。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流/拦截响应头解析上游明示的等待时长（P1-2）：
// 依次尝试 Retry-After（整数秒）→ retry-after-ms（整数毫秒）→
// x-ratelimit-reset（纯数字按 epoch 秒/毫秒推断，HTTP-Date 形态不支持——
// 上游族实践发的是数字）。任一头缺失/非法/非正/超上限则尝试下一头；
// 全部不可用返回 0（调用方回落既有计算值，绝不臆造等待时长；0 = 上游未明示，
// 与「非正值」同义——冷却时长回落调用方计算值）。
func ParseRetryAfter(h http.Header) time.Duration {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue // 非纯数字（如 HTTP-Date）不解析，宁缺毋滥
		}
		n, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if n <= 0 || n > retryAfterSanity {
			continue // 非正/异常大：丢弃（回落本地计算）
		}
		return n
	}
	return 0
}

// isAllDigits 报告 s 是否为纯数字（前置快筛，免 strconv 之后再判语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRetryNumber 按头名口径把纯数字串折算成时长。x-ratelimit-reset 是
// epoch 时刻而非时长：秒口径（10 位）与毫秒口径（13 位）都按「now+ 该时刻
// 的剩余量」折算，已在过去则不可用（负值由调用方丢弃）。位数不足（8 位以下）
// 无法判定 epoch 语义的丢弃（宁缺毋滥：该头族实践发 epoch，短串多半是序号
// 之类的误用头）。
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	// 上限 16 位防 int64 溢出（超过 epoch 毫秒的现实量级必非法）。
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for _, r := range v {
		n = n*10 + int64(r-'0')
	}
	switch headerName {
	case "Retry-After":
		// 先做上限校验再乘 time.Second：16 位数字乘 1e9 会溢出 int64 回绕成
		// 小正数（9223372036854776 → 192ms），进而通过调用方的 retryAfterSanity
		// 校验被当作合法等待时长。上限与 sanity 同口径（2h）。
		if n > int64(retryAfterSanity/time.Second) {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		// 同上：乘 1e6 的回绕路径（同值回绕到 192µs）一并拦截。
		if n > int64(retryAfterSanity/time.Millisecond) {
			return 0, false
		}
		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → 剩余量
		sec := n
		if len(v) >= 12 { // 毫秒口径（13 位）；11 位边界按秒（误判代价是多算 1000 倍）
			sec = n / 1000
		}
		return time.Until(time.Unix(sec, 0)), true
	}
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
//  0. 11102（IsModelBlocked）——「该后端无此模型」确定性答复，语义最具体，最先判
//     （详见 IsModelBlocked 注释）。
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
//  4. 429 + code 14018 —— 明确的账号积分耗尽，归 ErrHardCredit（吸收上游 d47219b，
//     issue #175）。只认结构化业务码，不靠跨计费/限流两界的文案猜测。
//  5. status==429 —— 限流状态码兜底（本层先于 hardMarkers，fork-scan-absorb T-3）：
//     429 body 高频携带 "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，
//     若 hardMarkers 先判会把限流误归 ErrHardCredit 硬冷却到次日 04:00，白扔号约
//     12h。状态码是比关键词更权威的信号：上游既然给了 429，就按限流语义处理
//     （宁可短冷却自愈，不可长冷却弃号）；真正的余额耗尽由 402（第 1 层）或
//     14018（第 4 层）捕获，非 429 状态码的 quota 措辞仍走下方 hardMarkers（第 6 层）。
//  6. hardMarkers —— 非 429 响应携带计费关键词（200 业务信封 / 403 信封等）。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit；429 场景已由
//     第 5 层前置接管（issue #28 记录的非 429 反向误判风险保持原样，待上游
//     原始响应确认后再定）。
//  7. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时已被第 5 层
//     短路，结果同为 soft_rate。
//  8. 11115（IsPromptTooLong）—— 「prompt is too long」请求级语义：判在 404/5xx 与
//     通用 4xx 兜底之前（404 上打 11115 若落 ErrNotFound 会误冷却账号——上下文超限
//     与账号无关）。只认 400/404/413（429/5xx 已在上方各自状态码层短路）。
//  9. 11135（isInvalidImageData）—— 「invalid_image_data」请求级**终态**：判在
//     404/5xx 与通用 4xx 兜底之前（与第 8 层同位置、同理由：图片数据无效是请求的
//     属性，与账号无关；落 ErrClient 兜底会喂连败计数并逐号轮转 → 一张坏图把整个
//     账号池拖降权）。只认 400/404/413，词表复用 hint.go 的 isInvalidImageData。
//  10. 404 / 5xx —— 与限流无关的常规分类。
//  11. IsWafBlocked —— 403 且无业务信封（HTML 拦截页/空体/纯文本）：WAF 拦截形态
//     （WAF 403 修复 P0-1）。判在通用 4xx 兜底**之前**：此前该形态落 ErrClient →
//     applyErrorPolicy 只换号不罚 → 连环 403。带业务信封的 403 已被上方 1-7 层
//     捕获（11140 request illegal → ErrAccountFault 禁用语义不变），走不到本层。
//     插在 404/5xx 之后是「只加不重排」：404/5xx 层只认各自状态码，403 不与之
//     相交，插入点不改变任何既有分类结果。
//  12. 内容策略/参数错误/其他 4xx —— 通用兜底（内容策略拦截须先于通用 ErrClient，
//     前者是误报信号、不罚账号，由网关降级重试处理）。11133 model_param_invalid
//     在参数层归 ErrBadParams（**保留轮转**：可能是账号侧后端差异）；11101
//     （IsBadParamsBody）同归 ErrBadParams 但**不轮转**（请求级终态）——分野理由
//     见 IsBadParamsBody 与 handler 的 11101 分支注释。
func Classify(status int, body string) ErrKind {
	// Explicit 503 WAF pages must precede the generic 5xx branch. Business
	// envelopes are excluded by IsWafBlocked and keep their existing priority.
	if status == http.StatusServiceUnavailable && IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	// 11102「该后端无此模型」须最先判：它是「模型在后端不存在」的确定性答复，语义比
	// 计费/限流都更具体——若不先判，msg 里的 "service info not found" 虽不含余额词、
	// 但可能被更宽的 4xx 兜底归为 ErrClient（只换号不避让），该坏号会留在池内反复被选中。
	// 先于 hardRule：11102 答复的 msg 是模型不存在，不含 credit/quota/积分 等计费词，
	// 正常不会撞 hardRule，但前置判定让语义零歧义（防上游未来在 msg 里混入余额词）。
	if IsModelBlocked(status, body) {
		return ErrModelBlocked
	}
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
	// status==429 + code 14018 —— 明确的账号积分耗尽，归 ErrHardCredit（吸收上游
	// d47219b，issue #175）：上游把积分耗尽也用 429 + 业务码表达。若不先判，会落
	// 下方通用 429 兜底成软限流——软冷却自愈不了真耗尽，全池冷却时的兜底选号会
	// 反复选中它白打请求。
	//
	// 只认结构化 code（hasBusinessCode），不猜文案：429 body 高频携带
	// "quota exceeded"/"额度不足" 这类跨计费/限流两界的措辞，按文案判会把限流号
	// 硬冷却到次日 04:00（白扔 12h）；无该码的 "credits exhausted" 文案仍保持
	// 普通 429 的软限流语义（fork-scan-absorb T-3 的既有裁定，见下方注释）。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return ErrHardCredit
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
	// 11135「图片数据无效」（invalid_image_data，Discussion #77 实测形态）——请求级
	// **终态**：同一张图换任何账号都会被上游拒绝（图片是否可识别是请求的属性，与
	// 账号健康无关），与 11115/内容策略拦截同哲学。判在通用 ErrClient 兜底之前：
	// 此前该形态落 ErrClient → applyErrorPolicy default 分支喂连败计数（NoteFailures）
	// → **每个账号各记一次**（ErrClient 分支继续轮转）→ 达阈（默认 5）全体降权
	// 10 分钟 → 用户实测的「一张坏图换来 503 all accounts unavailable (cooling/
	// disabled)」自伤。判定复用 hint.go 的 isInvalidImageData 词表（单一事实来源，
	// 不另立第二套）。
	// 只认请求级 4xx（见 isInvalidImageStatus，与 isPromptTooLongStatus 同口径）：
	// 429 限流、5xx 服务端故障在上方各自状态码层已短路，语义更权威。
	if isInvalidImageStatus(status) && isInvalidImageData(body) {
		return ErrInvalidImage
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// WAF 403（无业务信封的拦截形态）：判在内容策略/参数错误/通用 4xx 之前——
	// 这些层只认带文案的 body，WAF 空体/HTML 永远不会命中它们的 marker，
	// 但落 ErrClient 兜底的代价是「只换号不罚」（连环 403 的根因），必须在
	// 兜底前分流。带信封的 403 在上方各层已有权威分类（11140 request illegal
	// → ErrAccountFault 禁用语义不变），走不到本层。
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 11133 model_param_invalid（"Invalid request parameters" / 参数被模型供应商
		// 拒绝）：与下一层的 ErrBadParams 同属「请求参数问题」，**不能落 ErrClient**
		// ——ErrClient 分支喂连败计数（NoteFailures）且继续轮转，N 个账号就记 N 次，
		// 一个 11133 请求即可把整个池子推向降权（与 11135 同一自伤面）。
		// 归 ErrBadParams 的既有语义：不罚号（无冷却/熔断/连败）但**仍然轮转**——
		// 不同账号可能路由到不同后端、模型能力/权限不同（11102 的 (账号,模型) 负缓存
		// 正是「同模型在不同账号上可用性不同」的既有证据），轮转仍有价值；与
		// badParamsMarker 两条互不相交，先后不影响既有分类。判定复用 hint.go 的
		// isModelParamInvalid（单一事实来源，hint 层已用它给「换模型」指向）。
		if isModelParamInvalid(body) {
			return ErrBadParams
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，且**不轮转**——11101 发生在上游解析
		// 请求体阶段，还没走到模型路由，所以"不同账号可能有不同模型权限"其实是
		// 11102（ErrModelBlocked）的理由，那里已有 (账号,模型) 负缓存避让。
		// handler 侧用 upstream.IsBadParamsBody 区分本形态与 11133（后者保留轮转）。
		if IsBadParamsBody(body) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// ErrFingerprint 返回上游错误 body 的**请求级稳定指纹**：同一错误形态的跨账号
// 去重键（handler 侧 ErrClient 连败喂入去重，见 server.applyErrorPolicy 的 dedup）。
//
// 为什么取业务 code 而不是整个 body：上游每个响应都带 **requestId**，每次请求都
// 不同（11133/11135 的实测原始 body 均含 requestId），整 body 比对永远不等，去重
// 形同虚设。code 才是「同一错误形态」的稳定标识（11148/60001/… 同码即同因）。
//
// 取值顺序：顶层 code → extError.code（上游两处都放业务码，实测 11135 顶层
// code=11135、extError.code=invalid_image_data）。数字与字符串形态都认
// （`"code":11148` / `"code":"400001"`）。判不出（非 JSON / 无 code 字段）→ 返回
// 空串，调用方按「判不出」保守处理（不去重、不吞计数）。
func ErrFingerprint(body string) string {
	var env struct {
		Code     json.RawMessage `json:"code"`
		ExtError struct {
			Code json.RawMessage `json:"code"`
		} `json:"extError"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	for _, raw := range []json.RawMessage{env.Code, env.ExtError.Code} {
		s := strings.TrimSpace(string(raw))
		if s == "" || s == "null" {
			continue
		}
		// code 可能是 JSON 字符串（"400001"）或数字（11148）：两种形态归一为裸值。
		if s = strings.TrimSpace(strings.Trim(s, `"`)); s != "" {
			return s
		}
	}
	return ""
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

	chatFallback chatProtocolFallback

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

	// sanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	// 面板保存配置时热改（SetSanitizeFingerprints），与请求路径 prepareBody 的读
	// 属于不同 goroutine——裸 bool 在 Go 内存模型下是数据竞争，故用 atomic。
	// 私有不导出：写必须走 setter，防回归成裸赋值。
	sanitizeFingerprints atomic.Bool
	// zeroWidthSanitize 零宽字符脱敏开关（默认 false）：在 system 消息的指纹词内部
	// 插入 U+200B，破坏上游逐字匹配。与 sanitizeFingerprints 独立，可各自开关。
	zeroWidthSanitize atomic.Bool
	// reasoningHistory 出站历史推理文本裁剪档位（features.reasoning_history，
	// 默认 full；见 reasoning_history.go）。存的是**已归一化**的档位字符串
	// （SetReasoningHistory 内归一，非法值落 full）。与上面两个开关同理：面板保存
	// 配置的热改与请求路径 prepareBody 的读分属不同 goroutine，必须走 atomic。
	// 用 atomic.Value（而非 atomic.Pointer[string]）是为了零值可用：未 Store 时
	// Load 返回 nil，ReasoningHistoryMode 回落 full。
	reasoningHistory atomic.Value

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

	// MachineIDHeaders 是否在业务出站路径（chat/billing/models 目录）注入按 uid 固定盐
	// 派生的 X-Machine-ID / X-Session-ID（config upstream.machine_id_headers，缺省 true）。
	// 关掉 = 完全还原旧行为（只有 X-Device-Token，无设备标识），供不想带设备指纹的
	// 部署作逃生门。refresh/auth 类路径恒不注入（与开关无关）；CommonHeaders 本身
	// 不加（它被 RefreshHeaders 共用），见 injectAccountStableHeaders 的调用点清单。
	// 启动装配期写入；面板改后需重启（不在热改清单）。
	MachineIDHeaders bool

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

// New 生产默认值。Transport 由 newTransport() 集中构造（连接层参数：h2 默认启用 /
// TLS 握手超时 / 短 keepalive 探测，参数见 transport.go），配置连接池减少 TLS 握手。
//
// 此处走 TransportOpts 零值 = 生产默认（h2 启用 + 30s/30s/90s）；cmd/server/main.go
// 在 New() 之后立刻用 cfg.Upstream.* 调 ConfigureTransport 按配置重建。
func New() *Client {
	tr := newTransport(TransportOpts{})
	c := &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:      &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		ChatBaseCN:    "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN:     "https://www.workbuddy.cn",
		// GlobalEnabled 缺省 true（与 config global.enabled 缺省 true 一致；纯 CN 部署行为不变：
		// CN 账号恒判 cn，global base 只在 realm=global 的账号上被使用）。
		GlobalEnabled: true,
	}
	// 指纹脱敏默认开（零宽脱敏默认关 = 零值），与改 atomic 前的字段默认值一致。
	c.SetSanitizeFingerprints(true)
	// 账号级设备指纹头默认开（与 config upstream.machine_id_headers 缺省 true 一致，
	// 上游三仓默认就带）。零值 &Client{} 不注入，生产装配恒经本构造函数。
	c.MachineIDHeaders = true
	return c
}

// ConfigureTransport 按连接层配置重建共享出站 Transport，并同步替换 HTTP 与
// ChatHTTP 两个 client 的 Transport 字段——二者**共享同一 *http.Transport 实例**
// （连接池不重复，见 Client.ChatHTTP 注释），重建时必须一起换：只换一个会让两个
// client 各持一份连接池，连接复用率腰斩，旧池也无人在用（泄漏到 GC 回收）。
//
// 调用时机：**启动装配期**（cmd/server/main.go 在 New() 之后立刻调用，且先于
// HeaderTimeout 覆盖——重建会换掉 Transport 实例，顺序固定可避免覆盖被丢弃）。
// 这四项（h2 开关 / TLS 握手 / 拨号 / 空闲池超时）在 restartRequiredFields 里
// 列为「需重启」——运行期重建会换掉在途请求脚下的 Transport，不做热改。
//
// ResponseHeaderTimeout 从旧 Transport 沿用（旧值 >0 时）：该字段由 main 侧按
// cfg.Upstream.HeaderTimeoutSeconds 覆盖，不属于 TransportOpts 的四项。启动期
// 旧值仍是构造默认 120s，沿用即等价；这层兜底是为「先覆盖、后重建」的调用顺序
// 与重复调用（幂等，不丢已配置值）。旧 Transport 的空闲连接随即关闭（启动期无
// 连接，等价空操作；重复调用也不会把已死连接留给下一个请求）。
func (c *Client) ConfigureTransport(opts TransportOpts) {
	tr := newTransport(opts)
	if old := c.sharedTransport(); old != nil {
		if old.ResponseHeaderTimeout > 0 {
			tr.ResponseHeaderTimeout = old.ResponseHeaderTimeout
		}
		old.CloseIdleConnections()
	}
	if c.HTTP != nil {
		c.HTTP.Transport = tr
	}
	if c.ChatHTTP != nil {
		c.ChatHTTP.Transport = tr
	}
}

// sharedTransport 返回当前共享出站 Transport（HTTP 优先，非 *http.Transport 或
// 未设置时返回 nil）。供 ConfigureTransport 沿用旧字段与测试回读断言。
func (c *Client) sharedTransport() *http.Transport {
	if c.HTTP == nil {
		return nil
	}
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		return tr
	}
	return nil
}

// SetSanitizeFingerprints 热改出站请求体指纹脱敏开关（面板保存配置路径调用）。
// 并发安全：请求路径 prepareBody 走 atomic 读，无需调用方加锁。
func (c *Client) SetSanitizeFingerprints(v bool) { c.sanitizeFingerprints.Store(v) }

// SetZeroWidthSanitize 热改零宽字符脱敏开关（面板保存配置路径调用）。并发安全同 setter。
func (c *Client) SetZeroWidthSanitize(v bool) { c.zeroWidthSanitize.Store(v) }

// SanitizeFingerprintsOn 报告指纹脱敏当前是否开启（读与写相对原子，取最近一次 Store）。
func (c *Client) SanitizeFingerprintsOn() bool { return c.sanitizeFingerprints.Load() }

// ZeroWidthSanitizeOn 报告零宽脱敏当前是否开启。
func (c *Client) ZeroWidthSanitizeOn() bool { return c.zeroWidthSanitize.Load() }

// SetReasoningHistory 热改出站历史推理文本裁剪档位（面板保存配置路径调用）。
// 写入前归一化（大小写/空白不敏感；空串与未知值落 full）：Client 是档位的运行时载体，
// 不留未归一化字符串在内部（加载期的 warn 日志在 cmd/server 的 config.normalize，
// 与本 setter 各司其职）。并发安全：请求路径 prepareBody 走 atomic 读。
func (c *Client) SetReasoningHistory(mode string) {
	m, _ := NormalizeReasoningHistoryMode(mode)
	c.reasoningHistory.Store(m)
}

// ReasoningHistoryMode 返回当前裁剪档位；未设置（零值/非 string）回落 full。
func (c *Client) ReasoningHistoryMode() string {
	if v, ok := c.reasoningHistory.Load().(string); ok && v != "" {
		return v
	}
	return ReasoningHistoryFull
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

// chatCompletionsPath CN 与 global 共用的 chat 出站路径（/v2 单路径，吸收上游 03ce06d
// 的 #119 项）。
const chatCompletionsPath = "/v2/chat/completions"

// chatPaths 返回按 realm 的 chat 路径候选序列（两域都是 /v2 单元素）。
//
// global 为何收敛成单路径（吸收上游 03ce06d 的 #119 项 + 本仓 2026-09-17 实测）：
// /console 挂腾讯云 WAF body 内容规则（命令执行特征确定性 403），/v2 同 base 不挂该
// 规则、实测等价端点。同一 body 直连 /console 返回 403 WAF Block Page、/v2 返回 200
// （触发为历史文本的累计安全评分，与体积无关）。原 [console, v2] 顺序来自 PLAN R9，
// 实测会误伤长历史对话（ZCode 会话历史含安全术语时 console 首路径必 403 → 单账号池 503）。
//
// 兜底为什么也去掉（本仓新增理由）：保留 console 作 404/405 兜底会把**路径级 404**
// 升级成**账号级惩罚**——/v2 若 404，同请求改打 /console，后者对命令执行特征文案
// 确定性 403 WAF → Classify 归 ErrWafBlock → 健康账号被软冷却 + 抖动退避。兜底不仅
// 大概率救不回来（console 本身 WAF 敏感），还会为一个「上游路径变了」的事实惩罚无辜
// 账号。已知取舍：若上游未来关闭 /v2，global chat 整体不可用——届时应重新启用
// /console 路径，本注释即"坏了再说"的锚点。
func (c *Client) chatPaths(a *auth.Auth) []string {
	return []string{chatCompletionsPath}
}

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

// prepareBody 组装出站请求体（脱敏/裁剪开关由 Client.sanitizeFingerprints /
// zeroWidthSanitize / reasoningHistory 控制，均经 atomic 读取——本函数在请求 goroutine
// 内被调，与面板保存配置并发）。
// realm 为账号 Realm()（cn/global），供 efforts 缓存分桶（跨域 effort 集合不互相污染），
// 并决定是否做 CN 专属的 reasoning content-part 转换（见 reasoning_parts.go）。
//
// WB2A_DEBUG_REASONING 非空时，改写前后各打一行"思考字段"诊断（in/out）：
// 用于回答"我调了 off/low/medium/high 感觉一样"到底是客户端没发、还是网关改写了、
// 还是模型不在能力缓存里。该诊断是唯一能看到出站档位的地方——正常日志只在降级时才出声。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts := c.effortsSnapshot(realm)
	defaults := c.defaultEffortsSnapshot(realm)
	logReasoning("in ", body, efforts, defaults)
	// 三个开关各读一次并就地使用：读点与写点（Set*）成对走 atomic，消除数据竞争。
	// 不缓存到局部再跨阶段复用——避免把"一次读到的旧值"错当成当前配置。
	// realm 一并传入：CN 域在管线内做 reasoning part → reasoning_content 转换，
	// global 域原样透传（上游接受该 part 类型，改它反而引入风险）。
	body = PrepareBodyOptRealmHistory(body, realm, c.SanitizeFingerprintsOn(), c.ZeroWidthSanitizeOn(),
		c.ReasoningHistoryMode(), efforts, defaults)
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
// global chat 自 #119 实测后固定走 /v2（chat 层无 fallback 链；billing 层的 404
// fallback 独立存在，语义不受影响）。ensureConsoleSystem 在 prepareBody 后统一套用
// 全局脚本：首条消息非 system 时前置兜底 system（防上游 code 11128；#119 后 global
// 出站固定 /v2，该兜底保留——上游对 /v2 是否需要 system 无实测反证，删了无回滚路径）。
//
// 错误路径（≥400）除 (status, respBody) 外还返回**已分类的**
// *Error（Kind 信封 + Retry-After 头解析，WAF 403 修复 P0-1/P1-2）：客户端错误
// 分类在此一次完成，handler 不再对 body 二次 Classify（消除「上游分类一次、
// 网关再分类一次」的双路径漂移面），Retry-After 也随信封流动。respBody 仍原样
// 返回（错误透传语义：message 透传上游原文）。判定为 ErrNone 的响应
// （理论上不存在，防御）err 为 nil，handler 按 respBody 自行兜底。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx 的 cancel 在每个出口显式调用（Do 失败 / ≥400 / 成功分支移交 monitorBody），
	// 循环本身各分支必 return——无循环尾兜底代码（此前外层 var cancel 从未赋值 + 尾部
	// 不可达 cancel() 是潜伏 nil-panic，已删；chatPaths 恒非空由构造保证）。
	for _, path := range c.chatPaths(a) {
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
		httpClient := c.chatHTTPFor(req.URL)
		resp, err := httpClient.Do(req)
		if err != nil {
			cancel()
			log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
			// 传输层失败 → 清空共享连接池的空闲连接（连接层加固）：失败连接可能
			// 仍留在空闲池里，下一个请求会继续捡到它（仅靠 IdleConnTimeout 等过期
			// 不够，主动清池才断根）。CloseIdleConnections 只关空闲连接，不影响在途请求。
			roundTripCloseIdle(httpClient.Transport)
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
				a.UID, resp.StatusCode, kind, truncate(DisplayBody(string(raw)), 200))
			// ≥400 直接返回（#119 后 global 单路径 /v2，chat 层无 fallback 链）。
			// 分类一次、随 Kind 信封返回（含 Retry-After 头解析，P1-2）：
			// ErrNone 是防御分支（≥400 不应产生 None），返回原文让 handler 兜底。
			if kind == ErrNone {
				return nil, resp.StatusCode, raw, nil
			}
			ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(DisplayBody(string(raw)), 200)}
			if d := ParseRetryAfter(resp.Header); d > 0 {
				ue.RetryAfter = d
			}
			return nil, resp.StatusCode, raw, ue
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
		body := c.observeChatProtocol(resp.Body, reqCtx, req.URL, resp.ProtoMajor)
		return monitorBody(body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	panic("unreachable: chatPaths is never empty") // for range 空集时编译器仍要求兜底 return；chatPaths 恒非空（构造保证），永不触达
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	Description        string
	Vendor             string
	IsDefault          bool
	SupportsToolCall   bool
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
	// Tags 上游条目的 tags（如 ["text-to-image"] / ["craft"] / ["badge:企业版:#…"]）。
	// 只用于 nonChatModel 判定（image-to-image 等非对话模型没有 nes-/completion-/
	// codewise- 前缀、maxOutputTokens 也非 tiny，tags 是唯一识别依据），
	// 不透出到 /v1/models（面板也不需要）。
	Tags    []string
	Credits string // credits：积分倍率（如 "x0.79"）

	// 优惠（modelPromotions，/v3/config data.modelPromotions，见 modelpromo.go）：
	// Credits 是**牌价**（转正后基准倍率），Promo* 是当前生效的限时优惠——面板据此
	// 显示「生效价 + 标签 + 划线牌价」。PromoFactor 为 nil 表示无 machine-readable
	// 折扣（如「错峰使用」只有时段文案无 factor），仅挂标签/提示。
	PromoFactor  *float64 // 折扣系数（0=限时免费，0.5=五折）；nil=无
	PromoCredits string   // 折扣后倍率原文（如 "0x" / "0.50x"），仅展示
	PromoLabel   string   // 徽章文案（限时免费 / 夜间折扣 / 错峰使用）
	PromoNote    string   // hover 说明原文（含时段/日期描述）
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。四类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。**注意判据含 >0**：字段缺失（0）
//     是"未知"而非"tiny"，不得据此过滤（enhance-1.0 就不返回 maxOutputTokens）。
//   - tags 含 text-to-image / image-to-image：图像生成 / 图像编辑模型，非本网关用途
//     （实测 CN 下发 hunyuan-image-alpha-edit 带 image-to-image，重写前漏挡）。
//   - tags 里的营销/能力标注（"badge:企业版:#3B82F6"、"craft"）**不得**触发过滤：
//     它们描述的是订阅档位与能力标签，不是模型类型（o4-mini 带 badge: 仍是对话模型）。
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
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "text-to-image", "image-to-image", "text-to-image-edit":
			return true
		}
	}
	return false
}

// codeBuddyIDEUA /v3/config 要求能解析出 CodeBuddy 版本号的 UA。
// 官方 IDE 头 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0`。
// 版本号需随上游 IDE 发版跟进：UA 版本过旧时该端点可能同样返回精简目录。
//
// 注意（上游 9dce68a 实测修正，勿再按旧注释推断）：旧注释称「CLI UA 拿到精简目录、
// IDE UA 才返回完整能力」——实测**模型数量恰好相反**（IDE 14 条 / CLI 22 条，CLI 路
// 多出 deepseek 系列等），但 IDE 响应体积更大（26003B vs 21111B），故「完整能力」
// 应理解为**单条字段更全**，而非模型更多。两路各有独有模型，缺一不可——见
// codeBuddyCLIUA 与 probeGlobalV3Capabilities。
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// codeBuddyCLIUA CLI 三段式 UA（吸收上游 9dce68a）。**实测（2026-09-22）该端点对
// 不同 UA 下发的模型集合不同**：
//   - IDE UA  → 14 条（含 o4-mini / enhance-1.0 / auto-chat，**无 deepseek 系列**）
//   - CLI UA  → 22 条（**含 deepseek-v4.1-flash / deepseek-v4.1-flash-sg /
//     gpt-6-astra / kimi-k2.8-preview**，但无 o4-mini / enhance-1.0 / auto-chat）
//
// 该常量仅用于 global 侧第二路探测（probeGlobalV3Capabilities）；CN 侧仍走
// codeBuddyIDEUA 单路（CN 目录主源是企业端点，v3 只作能力覆盖，换 UA 无收益）。
const codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"

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
	// V3OnlyIDs 企业端点**没有**、由 /v3/config 追加进目录的 id（升序）。
	//
	// 为什么单独摊出来：这批 id 是"面板上看不到某个模型"这类问题的第一个嫌疑
	// （实测 2026-10-02：global 侧有 8 条，重写前面板路径缺追加步导致它们全部不可见）。
	// 与 Dropped 配对读——Dropped 是"上游给了但被筛掉"，V3OnlyIDs 是"只有 v3 给了、
	// 靠追加才进得来"，两者合起来能回答"这个模型为什么不在列表里"的全部成因。
	V3OnlyIDs []string `json:"v3_only_ids"`
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

// v3OverlayFor 取本次目录拉取要用的 /v3/config 覆盖表（能力 + 限时优惠 promo_*）。
// 按 realm 分路（理由见调用点注释）：
//   - global → probeGlobalV3Capabilities（双 UA 并发取并集，两路各有独有模型）；
//   - cn     → fetchV3ConfigModelMap(codeBuddyIDEUA) 单路（CN 目录主源是企业端点）。
//
// ok=false 表示两路都没拿到可用覆盖（global 双路全失败 / cn 单路失败或空表）：
// 调用方保持目录原样（fail-soft，v3 挂了不得把目录整体打空——能力字段由
// context_catalog 知识表兜底），并记一条 WARN（promo_* 的唯一来源，静默丢会让人
// 无从判断"上游没给"还是"网关丢了"）。
//
// 为什么与 FetchGlobalModelInfos 的探测共用 probeGlobalV3Capabilities 而不各写一份：
// 同一个"哪几路 UA、怎么合并"的知识写两遍必然漂移（一处改了另一处忘了，症状是
// "目录探测有 deepseek、面板列表没有"这类极难发现的错配）。
func (c *Client) v3OverlayFor(a *auth.Auth) (map[string]ModelInfo, bool) {
	if c.globalOn(a) {
		cap, ok := c.probeGlobalV3Capabilities(a)
		if !ok {
			log.Printf("WARN: [upstream] models: v3/config overlay failed (realm=global, 双 UA 全失败) —— 模型能力补全与限时优惠 promo_* 本轮不可用")
		}
		return cap, ok
	}
	overlay, err := c.fetchV3ConfigModelMap(a, codeBuddyIDEUA)
	if err != nil {
		log.Printf("WARN: [upstream] models: v3/config overlay failed (realm=cn) —— 模型能力补全与限时优惠 promo_* 本轮不可用: %v", err)
		return nil, false
	}
	return overlay, len(overlay) > 0
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
	// 模型目录是账号级业务路径（带 Authorization）→ 与 chat/billing 同口径注入
	// 设备指纹头（CommonHeaders 被 refresh 共用，故注入不放在那里，见 headers.go）。
	c.injectAccountStableHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
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
				SupportsToolCall  bool     `json:"supportsToolCall"`
				IsDefault         bool     `json:"isDefault"`
				Vendor            string   `json:"vendor"`
				Description       string   `json:"descriptionZh"`
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
			// 企业端点也会下发 modelPromotions（schema 与 /v3/config 同构）。
			// 此前注释断言「企业端点不下发」——那对 CN 成立（实测 0 条），但对
			// **global 不成立**：global 的 /v2/enterprises/personal/models 是
			// 唯一会下发 promo 的端点（实测 2 条）。因为本结构缺该字段，
			// global 企业端点的 promo 被 json.Unmarshal 结构性丢弃 ——
			// 表现为「上游续期了限时免费，面板却一直不显示」。
			ModelPromotions []v3ModelPromotion `json:"modelPromotions"`
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
		// tags 含 text-to-image / image-to-image）根本不进返回列表
		// （来源：harness buddy.ts:547-555，image-to-image 为 2026-10-02 补）。
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
			SupportsToolCall:   m.SupportsToolCall,
			IsDefault:          m.IsDefault,
			Vendor:             m.Vendor,
			Description:        m.Description,
			Tags:               m.Tags,
			Credits:            normalizeCredits(m.Credits),
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
	// v3/config 覆盖（能力补全 + 限时优惠 promo_*）。**按 realm 分路**：
	//
	//   - global：**双 UA 并集**（probeGlobalV3Capabilities，与目录探测
	//     FetchGlobalModelInfos 同一入口）。理由：实测该端点对 UA 下发的模型集合不同
	//     ——IDE 路**不含 deepseek 系列**（见 codeBuddyCLIUA 注释），而
	//     deepseek-v4.1-flash 正是用户点名要保的免费模型之一。单走 IDE 路时
	//     applyModelPromotions 的「目录外模型不挂」判据会把指向它的优惠整条丢掉，
	//     面板倍率列显示不出生效价——用户看到的症状就是"折扣一个都没显示"。
	//     并集是唯一能覆盖两路独有模型的取法（单换 UA 只会引入另一侧的缺失）。
	//   - cn：固定 IDE UA 单路（吸收上游 9dce68a：CN 目录主源是企业端点，v3 只作
	//     能力覆盖，换 UA 无收益）。刻意不为 global 的修复把 CN 的调用数翻倍
	//     （TestFetchModelsCNStaysIDEOnly 锁住这条）。
	//
	// 失败时**显式记一条 WARN**（而非静默跳过）：v3/config 是 promo_* 的**唯一**来源
	// （企业端点不下发 modelPromotions），它一挂，面板倍率列的「生效价 + 标签」就全空
	// ——而静默跳过时日志里没有任何线索，只能猜是上游没给还是网关丢了（这正是"没显示"
	// 类问题最贵的排查成本）。频率安全：本函数只在目录缓存 miss 时调用（/v1/models
	// 10min 缓存、面板按需），不是请求级热路径。
	//
	// 上面那句「企业端点不下发」**对 global 不成立**（2026-10-02 实测更正）：global 的
	// /v2/enterprises/personal/models 确实下发 modelPromotions（实测 2 条，schema 与
	// /v3/config 同构）。所以这里先把企业端点的 promo 挂到基底，再让 v3 覆盖——
	// 顺序不能反：v3 是更高优先级的来源（合并对 promo 取「有值即覆盖」），
	// 企业端点只是**补上 v3 没给的那部分**（例如 global 的 hy3 限时免费）。
	if len(env.Data.ModelPromotions) > 0 {
		base := make(map[string]ModelInfo, len(out))
		for i := range out {
			base[out[i].ID] = out[i]
		}
		applyModelPromotions(base, env.Data.ModelPromotions)
		for i := range out {
			out[i] = base[out[i].ID]
		}
	}
	if overlay, ok := c.v3OverlayFor(a); ok {
		// 单一合并入口（catalog.go）：能力覆盖 + credits 以企业端点优先 +
		// 追加 v3 独有 id。重写前这里调 mergeModelCapabilities——**没有追加步**，
		// 于是 CLI 路独有的 5 条（deepseek-v4.1-flash-sg / glm-5.3-flash /
		// kimi-k2.8-preview / gpt-6-astra / hy4-preview-f）在面板上一条都看不到，
		// 而 /v1/models 路径看得到（同目录两种投影漂移）。
		//
		// upstreamIDs 取**过滤前的全表**（diag.AllModelIDs）∪ base：判据必须是
		// "上游目录有没有列过它"，不是"白名单留没留它"——CN 的 agents[cli] 白名单
		// 刻意排除了企业端点列出的 11 条（deepseek-v4-flash / glm-4.6 / kimi-k2.5 …），
		// 用 base 当判据会把它们当成"v3 独有"重新追加回来，白名单语义被推翻。
		upstreamIDs := StringSet(diag.AllModelIDs)
		for i := range out {
			upstreamIDs[out[i].ID] = true
		}
		//
		// 追加清单入诊断（V3OnlyIDs）：这是"面板看不到某模型"类问题的第一嫌疑，
		// 摊在 diag 里就不必靠猜（重写前的 diag 只记 Dropped——那是"上游给了但被筛掉"，
		// 回答不了"只有 v3 给了、追加步有没有生效"）。
		seenOut := make(map[string]bool, len(out))
		for i := range out {
			seenOut[out[i].ID] = true
		}
		out = MergeCatalogOverlay(out, overlay, upstreamIDs)
		for i := range out {
			if !seenOut[out[i].ID] {
				diag.V3OnlyIDs = append(diag.V3OnlyIDs, out[i].ID)
			}
		}
		sort.Strings(diag.V3OnlyIDs)
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
	// realm 在取 effortsMu 之前先快照：Realm() 现为加锁读（持 a.mu），避免在
	// effortsMu 临界区内再取另一把锁（两锁独立但少一次嵌套更清晰）。
	realmBucket := realmKey(a.Realm())
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	c.efforts[realmBucket] = cache
	c.defaultEfforts[realmBucket] = defCache
	c.effortsMu.Unlock()
	return out, diag, nil
}

// v3ConfigDomain /v3/config 的 X-Domain：优先账号落盘 domain，否则 chatBase host。
// Domain 经访问器加锁快照（keepalive 刷新可在 a.mu 内改写它）。
func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		if d := strings.TrimSpace(a.DomainValue()); d != "" {
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

// fetchV3ConfigModelMap 拉官方配置目录，按模型 id 建能力表（吸收上游 b498416 +
// 9dce68a）。该端点对 UA 敏感：必须带 CodeBuddy/CodeBuddyIDE 版本，否则 400 code=12403；
// 且**不同 UA 下发不同模型集合**（见 codeBuddyCLIUA 注释）——global 探测据此并发
// 两路取并集（probeGlobalV3Capabilities），CN 侧仍传 codeBuddyIDEUA 单路。
//
// ua 为该次请求的 User-Agent；空串等价 codeBuddyIDEUA。
func (c *Client) fetchV3ConfigModelMap(a *auth.Auth, ua string) (map[string]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", v3ConfigDomain(a, c.chatBase(a)))
	req.Header.Set("X-Product", "SaaS")
	if ua == "" {
		ua = codeBuddyIDEUA // 空串兜底（既有调用点语义不变）
	}
	req.Header.Set("User-Agent", ua)
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
				ID                string   `json:"id"`
				Name              string   `json:"name"`
				MaxInputTokens    int64    `json:"maxInputTokens"`
				MaxOutputTokens   int64    `json:"maxOutputTokens"`
				MaxAllowedSize    int64    `json:"maxAllowedSize"`
				Credits           string   `json:"credits"`
				SupportsReasoning bool     `json:"supportsReasoning"`
				SupportsImages    bool     `json:"supportsImages"`
				SupportsToolCall  bool     `json:"supportsToolCall"`
				IsDefault         bool     `json:"isDefault"`
				Vendor            string   `json:"vendor"`
				Description       string   `json:"descriptionZh"`
				Tags              []string `json:"tags"`
				Reasoning         struct {
					Effort             string   `json:"effort"`
					DefaultEffort      string   `json:"defaultEffort"`
					CanDisableThinking bool     `json:"canDisableThinking"`
					SupportedEfforts   []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			// 试用模型横幅（吸收上游 b498416）：上游把「N 天免费试用」的模型**只**放在
			// 这里，不在 data.models 中——纯目录解析会漏掉，客户端选不到（该模型实际
			// 可调用）。实测 global 侧 hy4-preview-f 即如此：
			//   {"firstUseTimeKey":"hy4.first_user_time","modelId":"hy4-preview-f",
			//    "targetModelId":"hy4-preview","trialDays":14}
			ProductFeaturesConfig struct {
				ModelTrialBanner struct {
					Banners []struct {
						ModelID       string `json:"modelId"`
						TargetModelID string `json:"targetModelId"`
					} `json:"banners"`
				} `json:"ModelTrialBanner"`
			} `json:"productFeaturesConfig"`
			// 限时优惠（见 modelpromo.go）：credits 是牌价，这里是当前生效折扣。
			// 与上面的试用横幅是 data 下的两个平级字段，互不覆盖。
			ModelPromotions []v3ModelPromotion `json:"modelPromotions"`
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
			SupportsToolCall:   m.SupportsToolCall,
			IsDefault:          m.IsDefault,
			Vendor:             m.Vendor,
			Description:        m.Description,
			Tags:               m.Tags,
			Credits:            normalizeCredits(m.Credits),
		}
	}
	// 补入试用横幅模型（ModelTrialBanner，吸收上游 b498416）：上游把「N 天免费试用」
	// 的模型只放在这里，data.models 里没有，故纯目录解析会漏（实测 global 侧
	// hy4-preview-f 即如此，但该模型**实际可调用**）。
	//
	// 元数据口径：能力字段（context/maxTokens/efforts/reasoning 等）从 targetModelId
	// 的既有条目继承——试用版与其转正目标是同族模型，能力应当一致；
	// 但 **Credits 与 Tags 显式清空**——它们描述的是"转正后"的计费与营销信息
	// （如 hy4-preview 的 x0.29 与 badge），用在免费试用版上会误导下游展示。
	//
	// firstUseTimeKey / trialDays 属**账号级**试用状态，不透出给下游。
	for _, b := range env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := out[id]; exists {
			continue
		}
		mi := ModelInfo{ID: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if base, ok := out[tgt]; ok {
				mi = base
				mi.ID = id
			}
		}
		mi.Credits = ""
		out[id] = mi
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	// 挂当前生效的限时优惠（modelPromotions）：Credits 字段是**牌价**（转正后基准
	// 倍率，如 hy4-preview-f 的 x0.29），而 WorkBuddy 客户端显示的是生效价（试用/
	// 折扣窗口内 factor 打折）——面板据此展示「生效价 + 标签 + 牌价」。
	applyModelPromotions(out, env.Data.ModelPromotions)
	return out, nil
}

// mergeModelCapabilities 已删除（2026-10-02 目录重写）：能力覆盖与追加 id 的语义
// 收进 catalog.go 的 MergeCatalogOverlay，两条链路共用一份实现。
// 删它的直接理由是它**缺追加步**——v3 独有 id 完全不进结果，面板因此比
// /v1/models 少 8 个模型；保留一份"少一半语义"的合并函数就是留着第二次漂移的机会。

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
	// EndTime 该包的到期时间，上游墙钟串（packageEndLayout "2006-01-02 15:04:05"，
	// UTC+8），**原样透传**不做格式归一：面板「到期」列按 slice(0,10) 取日期、并按同
	// 口径算剩余天数。取值四级兜底 ExpiredTime → PackageEndTime → CycleEndTime →
	// DeductionEndTime（最后一级是 epoch 毫秒，出站前格式化成同一墙钟串；理由与
	// 「为什么只做兜底」见 CreditPackages 内赋值处）；空串 = 上游没给到期时间
	// （面板渲染「-」，不是 0、也不是「已过期」）。
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
//
// 到期时间读 CycleEndTime（见下方赋值处的三级兜底与实证依据）；本方法只透出明细，
// 不参与任何分桶/选号判定。
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
					// 到期时间字段四级兜底（取值顺序见下方赋值处）。前两个字段上游
					// 实测从不下发（恒空串，这正是面板「到期」列全是「-」的根因），
					// 真实到期字段是 CycleEndTime——与 UserResourceDetailed 的
					// Expiring 分桶判据同源（同一端点 get-user-resource，同一种
					// 墙钟格式 packageEndLayout）。
					ExpiredTime    string `json:"ExpiredTime"`
					PackageEndTime string `json:"PackageEndTime"`
					CycleEndTime   string `json:"CycleEndTime"`
					// DeductionEndTime 可抵扣窗口结束（epoch 毫秒）——PR #93 声称它是
					// 「这个包什么时候不能再花」的真失效时刻（CycleEndTime 是周期边界
					// 即额度重置点）。本仓的既有实证（sliver 75c15e8 + 本仓 cfa10cf）
					// 说真实到期字段是 CycleEndTime 且上游不下发 ExpiredTime/
					// PackageEndTime——两者可能都对（不同 realm / 时间点），故本仓
					// **只把它加为第四级兜底**：有值才用、缺失回落既有三级，即使 PR
					// 的判断在本域不成立也不回归。单位差异见下方赋值处（格式化成同一
					// 墙钟串出站，不让前端解析路径分叉）。
					DeductionEndTime int64 `json:"DeductionEndTime"`
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
		// 到期时间四级兜底 ExpiredTime → PackageEndTime → CycleEndTime →
		// DeductionEndTime（「有值就用」：空串不覆盖真值，故用 if/else if 而不是后写
		// 覆盖前写）。
		//
		// 为什么是这个顺序：前两个字段是修复前就在读的，排在前面保证「真下发它们的域」
		// 取值与修复前逐字一致（零回归）；但上游 CN/global 两域实测**都不下发**这两个
		// 字段（恒空串，面板「到期」列全是「-」的根因），所以真正补上缺口的是第三位的
		// CycleEndTime。若哪天实测发现某域真的下发了前两个字段且语义与包到期不同，
		// 才需要重新评估这个顺序。
		//
		// 为什么读 CycleEndTime：与 UserResourceDetailed 的 Expiring 分桶判据同源
		// （同一端点 get-user-resource，两域字段全集均无 PackageEndTime，真实到期
		// 字段是 CycleEndTime——上游 sliver 75c15e8 + 本仓 cfa10cf 两处实证），
		// 照它的口径，不自创第二套。
		//
		// 为什么 DeductionEndTime 只排第四（兜底）：PR #93 声称它（epoch 毫秒）才是真
		// 失效时刻、CycleEndTime 只是周期边界——与本仓实证（两处：真实到期字段就是
		// CycleEndTime）冲突。两者可能都对（不同 realm / 时间点），故**只做兜底**：
		// 前三者全空时才有值可用，即使 PR 的判断在本域不成立也不会让既有取值回归。
		//
		// 原样透传、不做格式归一：CycleEndTime 上游形态即 packageEndLayout
		// "2006-01-02 15:04:05"（UTC+8 墙钟），前端「到期」列按 slice(0,10) 取日期、
		// 三态判定按同口径算剩余天数，原样已够用；归一成 RFC3339 要多一次格式转换
		// （且必须同步改前端），零收益而引入格式风险。
		//
		// 第四级的**单位差异**处理：DeductionEndTime 是 epoch 毫秒，与前三级的墙钟串
		// 不同源——这里把它格式化成同一 packageEndLayout 串出站（而非照搬 PR 的
		// RFC3339），前端 pkgEndMs 只认墙钟形态，解析路径因此**不分叉**。
		// 非正值（0/负数）视为未下发，不格式化（不得把 1970 渲染成「已过期」）。
		switch {
		case p.ExpiredTime != "":
			cp.EndTime = p.ExpiredTime
		case p.PackageEndTime != "":
			cp.EndTime = p.PackageEndTime
		case p.CycleEndTime != "":
			cp.EndTime = p.CycleEndTime
		case p.DeductionEndTime > 0:
			cp.EndTime = time.UnixMilli(p.DeductionEndTime).In(softRateResetLoc).Format(packageEndLayout)
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

// packageEndLayout 上游 CycleEndTime / 请求体过滤串的时间格式（墙钟，UTC+8，
// 与 softRateResetLoc 同口径）。响应侧到期字段与请求侧过滤串实测同格式，共用一个常量。
const packageEndLayout = "2006-01-02 15:04:05"

// ResourceDiag 一次余额查询的分桶诊断（只读，不参与任何判定）：回答「快过期=0 到底是
// 窗口内确实没有，还是根本没解析到到期时间」——上游字段变化/解析失败时，只看 expiring=0
// 无法区分这两件事，正是「快过期积分不显示」的排查盲区。
type ResourceDiag struct {
	// Soon 本次分桶使用的窗口（<=0 = 禁用分桶，全部归长期）。
	Soon time.Duration
	// Packages 上游返回的套餐条目数（billing meter Accounts 数组长度）。
	Packages int
	// WithEnd CycleEndTime 非空且解析成功的条目数。>0 说明「窗口内没有」是**可证的**
	// （字段投递正常，只是都不在窗口内）；==0 且 Packages>0 说明上游没给/字段变了。
	// 只看字段是否可解析，不看余额是否为 0（诊断的是字段投递，不是分桶结果）。
	WithEnd int
	// BadEnd CycleEndTime 非空但解析失败的条目数（>0 = 上游字段格式变了，不是"没有到期"）。
	BadEnd int
	// NearestEnd 最早的可解析到期时刻（含已过期的条目：上游按 PackageEndTimeRangeBegin=now
	// 过滤，正常不该出现已过期的包，出现即上游数据异常，日志里直接可见）。
	// 零值 = 一条都没解析到。
	NearestEnd time.Time
	// ExpiringEnd 本次分桶计入 expiring 的套餐里最早的到期时刻（仅 soon>0、余额 r>0、
	// 且到期时刻在窗口内的条目参与）。它是「快过期积分优先消耗」的排序依据：哪个账号
	// 的快过期积分最先作废，就先消耗哪个账号。零值 = 没有快过期积分 / 分桶未启用。
	ExpiringEnd time.Time
}

// NearestEndText 最近到期时刻的可读文案（按上游墙钟 UTC+8 输出，与官网/面板展示同口径）；
// 没有任何可解析到期时间时返回空串（调用方据此省略该段）。
func (d ResourceDiag) NearestEndText() string {
	if d.NearestEnd.IsZero() {
		return ""
	}
	return d.NearestEnd.In(softRateResetLoc).Format(packageEndLayout)
}

// windowText 窗口的可读文案：<=0 显式标注「禁用分桶」——否则日志里的「快过期=0」
// 会被读成"确实没有快过期积分"，而实际是分桶被配置关掉了（pool.expiring_soon=0）。
// 整小时/整分钟窗口去掉 Duration.String 的 0m0s 尾巴（168h0m0s → 168h），与
// pool.expiring_soon 配置值的写法一致（运维一眼对上配置）。
func (d ResourceDiag) windowText() string {
	if d.Soon <= 0 {
		return "0s(禁用分桶)"
	}
	switch {
	case d.Soon%time.Hour == 0:
		return strconv.FormatInt(int64(d.Soon/time.Hour), 10) + "h"
	case d.Soon%time.Minute == 0:
		return strconv.FormatInt(int64(d.Soon/time.Hour), 10) + "h" +
			strconv.FormatInt(int64(d.Soon/time.Minute)%60, 10) + "m"
	default:
		return d.Soon.String()
	}
}

// BalanceLine 余额分桶的日志正文（调度器/面板共用同一口径，便于 grep 与对照），形如：
//
//	剩余=8059/8406 快过期=0（窗口=168h，包裹数=3，可解析到期=2，最近到期=2026-09-25 00:00:00）
//
// 括号内全是诊断：窗口（分桶是否被禁用）、包裹数、可解析到期数（0 = 上游没给/字段变了，
// 与"窗口内没有"是两回事）、到期字段坏（仅在解析失败时出现）、最近到期（仅在可解析时出现）。
func BalanceLine(remain, total, expiring int64, d ResourceDiag) string {
	var b strings.Builder
	fmt.Fprintf(&b, "剩余=%d/%d 快过期=%d（窗口=%s，包裹数=%d，可解析到期=%d",
		remain, total, expiring, d.windowText(), d.Packages, d.WithEnd)
	if d.BadEnd > 0 {
		fmt.Fprintf(&b, "，到期字段坏=%d", d.BadEnd)
	}
	if t := d.NearestEndText(); t != "" {
		fmt.Fprintf(&b, "，最近到期=%s", t)
	}
	b.WriteString("）")
	return b.String()
}

// UserResourceDetailed 在 UserResource 基础上额外返回「快过期」积分子集：
// soon > 0 且套餐 CycleEndTime 解析成功且到期时刻 ≤ now+soon 的余额计入 expiring
// （pool 据此优先消耗，避免官方活动赠送的奖励积分到期作废）；soon ≤ 0 时 expiring
// 恒 0（禁用分桶，行为与引入前一致）。expiring 是 remain 的一部分。
//
// 到期字段必须读 CycleEndTime：上游 get-user-resource 的响应字段全集（CN/global
// 两域实测）**没有** PackageEndTime——旧实现读该字段恒 miss，导致 expiring 恒 0、
// 选号第四因子（expiringWeight）自上线从未生效。真实到期字段是 CycleEndTime。
// 解析失败/缺失的套餐保守归入 Stable（不误标为快过期而插队）。
//
// 注意：请求体里的 PackageEndTimeRangeBegin/End 是**过滤参数**，与响应侧到期字段
// 同名但无关，不得改动。
//
// 需要分桶诊断（「窗口内没有」vs「没解析到到期时间」）时用 UserResourceDetailedDiag，
// 本函数即其薄封装（分桶语义只有一处实现，不存在两条路径漂移）。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	remain, total, expiring, _, err = c.UserResourceDetailedDiag(a, soon)
	return remain, total, expiring, err
}

// UserResourceDetailedDiag 同 UserResourceDetailed，额外返回分桶诊断（ResourceDiag）：
// 调用方（调度器/面板）据此把「窗口内确实没有快过期积分」与「根本没解析到任何
// CycleEndTime」区分开并写进日志——前者不是故障，后者是上游字段变化/解析失败。
//
// 为什么是变体而不是给 UserResourceDetailed 加返回值：分桶语义被 6 个既有用例逐字锁定
// （soon<=0 禁用分桶、忽略 PackageEndTime、缺/坏 EndTime 归长期），改签名等于改动全部
// 调用点与断言；变体是纯增量，既有签名/用例逐字不变。
func (c *Client) UserResourceDetailedDiag(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, diag ResourceDiag, err error) {
	if a.IsEnterprise() {
		remain, total, expiring, end, _, err := c.enterpriseResource(a, soon)
		if expiring > 0 {
			diag.ExpiringEnd = end
		}
		return remain, total, expiring, diag, err
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	// 余额查询同样做瞬时错误有界重试（签到后紧接着的 user-resource 偶发 500 会让
	// 该账号错过本次解冻/到期快照更新，只能等下一个刷新周期）。
	var data json.RawMessage
	err = c.retryBillingTransient(func() error {
		var e error
		data, e = c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
		return e
	})
	if err != nil {
		return 0, 0, 0, diag, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
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
		return 0, 0, 0, diag, fmt.Errorf("resource parse: %w", err)
	}
	diag.Soon = soon
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
		diag.Packages++
		// 分桶：仅 soon>0 且能解析出有效到期时间、且确实在窗口内 → expiring。
		if acct.CycleEndTime != "" {
			// 上游时间为 UTC+8 墙钟（与 softRateResetLoc 同口径，官网展示时区）。
			end, perr := time.ParseInLocation(packageEndLayout, acct.CycleEndTime, softRateResetLoc)
			if perr != nil {
				diag.BadEnd++ // 字段在但格式变了：诊断上必须与"字段缺失"分开
			} else {
				diag.WithEnd++
				if diag.NearestEnd.IsZero() || end.Before(diag.NearestEnd) {
					diag.NearestEnd = end
				}
				if soon > 0 && r > 0 && !end.After(now.Add(soon)) {
					expiring += r
					if diag.ExpiringEnd.IsZero() || end.Before(diag.ExpiringEnd) {
						diag.ExpiringEnd = end
					}
				}
			}
		}
	}
	return remain, total, expiring, diag, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
// 偶发上游 5xx（code 10000）做有界重试（见 retryBillingTransient）——单次抖动不再
// 让该账号整天漏签；「已签到」等业务错误不重试。逐号串行调用，重试只延长单号耗时，
// 不阻塞其它账号（每号之间另有既有间隔）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.retryBillingTransient(func() error {
		_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		return err
	})
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

// truncate 转发 logfmt.Truncate（按 rune 边界截断 + 省略标记，见该函数契约）：
// 上游错误 body 多为中文（"将在 … 重置"），按字节切会出半截 UTF-8 序列乱码。
func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
