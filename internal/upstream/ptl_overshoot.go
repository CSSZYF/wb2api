// ptl_overshoot.go 11115「prompt is too long」的 overshoot 解析与 max_tokens 下调。
//
// 背景（实测，**未 100% 证实**）：客户端发 max_tokens=128000、输入约 95 万 token 的请求，
// 上游回 `{"code":11115,"msg":"prompt is too long: 1083265 tokens > 1048576 maximum",
// "requestId":"…"}`。1083265 - 1048576 = 34689；同一会话在另一条（同内容、
// max_tokens=20000）请求里被上游计为 955192，1083265 - 955192 = 128073 —— 与
// max_tokens(128000) 只差 73。**强烈提示上游把 max_tokens 也算进上下文上限检查**。
//
// 未证实之处（必须诚实记下，别当结论用）：另有一次 max_tokens=1048576 + 极小输入的请求
// 返回 200，说明要么不计入、要么 max_tokens 被截到模型上限后再判。因此本文件提供的是
// **安全兜底**能力：能算就按错误里的真实数字下调一次重试，算不出/不适用一律保持现状
// （调用方 handler 仍透传原文、不轮转、不罚号）。
//
// 两个纯函数：
//   - ParsePromptTooLongOvershoot：从上游 message 里抽出 <n> tokens > <limit> maximum；
//   - DowngradeMaxTokens：按 overshoot + 安全余量下调请求体的输出预算（只动这一个字段）。
package upstream

import (
	"encoding/json"
	"regexp"
	"strconv"
)

// ptlOvershootRe 11115 文案里「n tokens > limit maximum」的捕获正则。
//
// 容错（上游文案大小写/空格未完全固定，实测形态见文件头）：
//   - 大小写不敏感（(?i)）：PROMPT IS TOO LONG / TOKENS / MAXIMUM 都认；
//   - 空白宽松（\s+）：`:  1083265   tokens   >   1048576   maximum` 也命中；
//   - 数字只收纯十进制整数（\d+）：千分位 "1,083,265" 不解析（判不出 → 不重试，
//     好过猜错数字把 max_tokens 下调到荒谬值）；小数/科学计数法同理不认。
//   - 不锚定行首行尾（文案可能被前缀包裹，如 "request rejected: prompt is too long: …"）。
var ptlOvershootRe = regexp.MustCompile(`(?i)(\d+)\s*tokens?\s*>\s*(\d+)\s*maximum`)

// ptlMaxTokenLiteral 十进制字面量上限（防 strconv 溢出后静默回绕成小值）。
const ptlMaxTokenLiteral = int64(1) << 62

// ParsePromptTooLongOvershoot 从 11115 的错误原文里解析「实际 token 数」与「上下文上限」。
//
// 返回 (n, limit, true) 当且仅当：文案命中 `<n> tokens > <limit> maximum` 形态、
// 两个数字都能解析为 int64、且 **n > limit**（真的超限——等值/反向说明不是本形态，
// 或上游口径变化，一律不重试）。
//
// 判不出（无数字 / 千分位 / 非法数字 / 溢出 / n<=limit）→ (0, 0, false)。
// 调用方据此**保持现状**（不重试），绝不臆造数字。
func ParsePromptTooLongOvershoot(body string) (n, limit int64, ok bool) {
	m := ptlOvershootRe.FindStringSubmatch(body)
	if len(m) < 3 {
		return 0, 0, false
	}
	n, err1 := strconv.ParseInt(m[1], 10, 64)
	limit, err2 := strconv.ParseInt(m[2], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if n <= 0 || limit <= 0 || n > ptlMaxTokenLiteral || limit > ptlMaxTokenLiteral {
		return 0, 0, false
	}
	if n <= limit {
		return 0, 0, false // 未超限：不是「prompt too long」的真实形态，不重试
	}
	return n, limit, true
}

// ptlMaxTokensMinRetry 下调后 max_tokens 的下限：低于它就不重试。
//
// 理由：max_tokens 是**输出预算**，压到过低等于让模型回答被截断——重试成功却给出半截
// 回答，比直接报错更糟（用户看不出是网关干的）。1024 是「一次完整短回答」的量级
// （够一段工具调用 + 说明文字），再低就属于「为了通过检查而牺牲可用性」。
const ptlMaxTokensMinRetry = 1024

// ptlMarginFloor 安全余量的下限（512 token）。
//
// 余量存在的理由：上游把 max_tokens 计入上限这件事**未被证实**，且即便成立，其计数口径
// 与我们按 max_tokens 字面值扣减也可能有偏差（实测 128000 vs 128073 差 73，说明口径
// 接近但不完全等值）。留一段余量让第二次请求有机会落在限制内，而不是「下调后仍然差几十
// token 再次 11115」白跑一趟。
const ptlMarginFloor = 512

// ptlMarginFraction 安全余量按原 max_tokens 的比例（5%）：大预算时按比例留，
// 与 ptlMarginFloor 取**大者**（见 downgradeMargin）。
const ptlMarginFraction = 0.05

// downgradeMargin 返回本次下调的安全余量：max(512, 原 max_tokens 的 5%)。
//
// 取大者的理由：小预算（如 4000）下 5% 只有 200，不足以覆盖「口径偏差 + 上游其它
// 计入项」，故设 512 地板；大预算（如 128000）下 512 相对太小（不足 0.4%），
// 按 5% 留才与预算规模相称（128000 → 6400，足够覆盖实测那 73 token 的口径差与
// 后续可能的上游微调）。
func downgradeMargin(oldMax int64) int64 {
	m := int64(float64(oldMax) * ptlMarginFraction)
	if m < ptlMarginFloor {
		m = ptlMarginFloor
	}
	return m
}

// ptlMaxTokenFields 输出预算字段名（两个都支持）：
//   - "max_tokens"：OpenAI 兼容协议的历史字段，也是上游唯一认的字段（出站管线已把
//     max_completion_tokens 别名翻译成它，见 payload.go 的 translateMaxCompletionTokens）；
//   - "maxOutputTokens"：上游原生字段名（模型目录里的叫法），客户端可能直接用。
var ptlMaxTokenFields = []string{"max_tokens", "maxOutputTokens"}

// DowngradeMaxTokens 按 overshoot 下调请求体里的输出预算字段，返回改写后的 body 与
// 新旧值。**只动这一个字段**，其余键与值原样保留（同一 body 变换管线的产物）。
//
// 返回值：
//   - out：改写后的 body；不可重试时**原样返回入参**（调用方可直接沿用，不做二次判断）；
//   - oldMax/newMax：原值与下调值（不可重试时均为 0）；
//   - ok：是否给出可用方案（false = 调用方必须保持现状）。
//
// 判定规则：
//  1. overshoot<=0 → 不重试（上游没超限，别乱改请求）；
//  2. body 必须是可解析的 JSON 对象，且带 max_tokens / maxOutputTokens 中至少一个
//     正整数（0/负数/浮点/字符串一律视为「未设置输出预算」→ 不重试：0 与负数在上游
//     语义里是「用默认」，翻成正数等于把「未设置」变成「限制」，见 payload.go 对别名
//     翻译的同款取舍）；
//  3. 两键同时存在时**取更小者为基准**（输出预算取更严的那个才可能通过检查），
//     并把两键都写成同一下调值（避免上游按另一个键判、重试白跑）；
//  4. newMax = oldMax - overshoot - downgradeMargin(oldMax)；newMax < 1024（含负数）
//     → 不重试（见 ptlMaxTokensMinRetry）。
//
// 不改写 messages/model/stream 等任何其它字段：重试请求与首次走**完全相同的准备管线**，
// 差异只有这一个字段（handler 侧注释与测试锁定）。
func DowngradeMaxTokens(body []byte, overshoot int64) (out []byte, oldMax, newMax int64, ok bool) {
	if overshoot <= 0 || len(body) == 0 {
		return body, 0, 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, 0, 0, false // 畸形 JSON：绝不臆造请求体
	}
	// 取两个字段里的最小正整数作基准（都存在时两键都会被改成同一下调值）。
	base := int64(0)
	for _, key := range ptlMaxTokenFields {
		v, has := obj[key]
		if !has {
			continue
		}
		f, isNum := v.(float64)
		if !isNum || f <= 0 || f != float64(int64(f)) {
			continue // 0/负数/浮点/非数值 → 视为未设置（不参与基准）
		}
		n := int64(f)
		if base == 0 || n < base {
			base = n
		}
	}
	if base <= 0 {
		return body, 0, 0, false // 没有可用的输出预算字段 → 不重试
	}
	next := base - overshoot - downgradeMargin(base)
	if next < ptlMaxTokensMinRetry {
		return body, base, 0, false // 低于下限（含负数）→ 不重试
	}
	// 只有真正存在的键才改写（不凭空新增另一个键）。
	for _, key := range ptlMaxTokenFields {
		if _, has := obj[key]; has {
			obj[key] = next
		}
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body, base, 0, false
	}
	return rewritten, base, next, true
}
