// reasoning_history.go 出站历史推理文本裁剪（配置 features.reasoning_history）。
//
// 实测依据（2026-09-19，真实请求打上游 deepseek-v4.1-flash / global 域）：
//
//	出站 assistant 消息                                  上游 prompt_tokens
//	带 tool_calls，无推理字段                                346
//	带 tool_calls + reasoning_content(40000 字符)          9234
//	带 tool_calls + reasoning(40000 字符)                  9234
//	带 tool_calls + 两字段各 40000 字符                     9234（不翻倍，只算一份）
//	带 tool_calls + 两字段都是单个空格 " "                    346（零成本）
//	不带 tool_calls + reasoning_content(40000 字符)           42（完全不计）
//
// 结论：上游只对「带 tool_calls 的 assistant 消息」计算推理文本的 token，两个字段
// 只算一份、空白串零成本。而真实客户端（ZCode 3.11.2）给每条 assistant 消息挂完整
// 推理文本（经 providerOptions.openaiCompatible.reasoning_content 合并进请求体），
// 真实会话实测 887 条消息里 376 条带推理、合计 178 万字符，约占**总 token 的一半**
// ——用户因此频繁撞上 1,048,576 的上下文上限并触发客户端压缩。
//
// 三档（配置值 features.reasoning_history）：
//   - "full"（默认）：零改动，出站 body 与加本功能前逐字节一致（零回归）。
//   - "last"：只保留**最后一条带推理文本**的 assistant 消息的推理原文，其余带推理
//     字段的 assistant 消息两字段一律替换为单个空格。
//   - "blank"：所有带推理字段的 assistant 消息两字段一律替换为单个空格。
//
// 为什么用单个空格占位：部分租户校验 assistant 的 reasoning 字段 len>0（缺失/null/
// 空串 400、空白串 200，见 thinking.go 的镜像写与 issue #165 追评），故裁剪必须
// 「保留字段、写成非空」。占位口径与 206b 既有镜像写完全一致（单个 U+0020、不 trim），
// 不另创 "..." 之类的新占位——自创占位等于给上游引入一个此前不存在的字节特征。
//
// 作用域（三条边界，勿越界）：
//   - 只动 assistant 消息的这两个字段（user/tool/system 一字不动）；
//   - 只改写**已存在**的推理字段，不凭空创建（无推理字段的 assistant 消息零改动）；
//   - **不看 isDeepSeekModel**：token 成本来自上游对「带 tool_calls 的 assistant 消息」
//     的计费，与模型名无关；injectThinking / backfillReasoningContent 的 deepseek 门控
//     一字未动（那些是"开思考/补字段"的语义，本步是"省 token"的语义，两者独立）。
//     档位默认 full 时本步入口即 return，故「作用于所有模型」在默认配置下零风险。
//
// 顺序（见 payload.go 调用点）：必须晚于 sanitizeMessages——sanitize 会把纯指纹块的
// rc 整段删除成空串（sanitize.go 记录的「已知残余」：rc 净化后为空 → 租户 400），
// 裁剪步在其后即可把空串补成 " "，顺带修掉这条路径。
package upstream

import (
	"log"
	"strings"
)

// 档位取值（配置 features.reasoning_history 的三个合法字符串）。
const (
	// ReasoningHistoryFull 全量保留推理原文（默认；行为与加本功能前逐字节一致）。
	ReasoningHistoryFull = "full"
	// ReasoningHistoryLast 只保留最后一条带推理文本的 assistant 消息的原文。
	ReasoningHistoryLast = "last"
	// ReasoningHistoryBlank 全部 assistant 推理字段替换为单个空格占位。
	ReasoningHistoryBlank = "blank"
)

// reasoningPlaceholder 裁剪后的占位串：单个空格（U+0020）。
// 与 thinking.go 206b 镜像写的占位同口径——上游对这两个字段做 len>0 校验且不 trim，
// 空白串过闸、空串 400；不自创别的占位（不引入新字节特征）。
const reasoningPlaceholder = " "

// NormalizeReasoningHistoryMode 归一化档位取值：大小写/首尾空白不敏感。
// 合法值只有 full / last / blank；空串与未知值一律返回 (full, false)——由调用方
// （cmd/server 的 config.normalize）据此记 warn 日志并落 full（fail-safe：不改变既有
// 行为）。本函数只做判定不做日志：日志是加载期行为，不是解析语义，分开才好测。
func NormalizeReasoningHistoryMode(v string) (string, bool) {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case ReasoningHistoryFull:
		return ReasoningHistoryFull, true
	case ReasoningHistoryLast:
		return ReasoningHistoryLast, true
	case ReasoningHistoryBlank:
		return ReasoningHistoryBlank, true
	default:
		return ReasoningHistoryFull, false
	}
}

// trimReasoningHistory 按 mode 裁剪 messages 里 assistant 消息的推理文本，
// 返回被改写的消息数（0 = 零改动；仅用于日志与测试断言）。
//
// 档位：
//   - full / 未知值：入口直接 return，**不做任何 map 写入**（零回归路径）；
//   - last：keeper = 最后一条「净化后仍带非空推理文本」的 assistant 消息，保留其原文；
//     其余带推理字段的 assistant 消息两字段写占位；
//   - blank：所有带推理字段的 assistant 消息两字段写占位。
//
// keeper 判定基于**当前（sanitize 之后）**的文本，且「带推理文本」= 该条任一推理字段
// 是非空 string。为什么不用「最后一条带字段的消息」：sanitize 会把纯指纹块的 rc 清成
// 空串，那样的消息当 keeper 等于把空串保留下来（租户 400），且会连累更早那条真原文
// 一起被清掉——「保留原文优先，空则占位」才是本档的语义。
//
// 字段口径（两字段同源同口径，与 206b 一致）：
//   - keeper：非空 string 原文一字不改；空串/null/非 string → 补占位（空则占位）；
//   - 非 keeper：存在的字段一律写占位（无论原值是什么）；
//   - 字段缺失：不凭空创建（无推理字段的消息不因本步多出字段）。
func trimReasoningHistory(obj map[string]any, mode string) int {
	if mode != ReasoningHistoryLast && mode != ReasoningHistoryBlank {
		return 0
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return 0
	}
	keeper := -1
	if mode == ReasoningHistoryLast {
		for i, mm := range msgs {
			msg, ok := mm.(map[string]any)
			if !ok || !isAssistantMsg(msg) || !hasReasoningField(msg) {
				continue
			}
			if reasoningText(msg) != "" {
				keeper = i
			}
		}
	}
	changed := 0
	for i, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok || !isAssistantMsg(msg) || !hasReasoningField(msg) {
			continue // 非 assistant / 无推理字段：一字不动
		}
		if i == keeper {
			if fillPlaceholder(msg, "reasoning_content") {
				changed++
			}
			if fillPlaceholder(msg, "reasoning") {
				changed++
			}
			continue
		}
		if blankReasoningField(msg, "reasoning_content") {
			changed++
		}
		if blankReasoningField(msg, "reasoning") {
			changed++
		}
	}
	if changed > 0 {
		model, _ := obj["model"].(string)
		log.Printf("reasoning history trimmed mode=%s model=%s fields=%d", mode, model, changed)
	}
	return changed
}

// isAssistantMsg 判定 assistant 角色，与 promoteReasoningParts / normalizeRoles 同口径
// （大小写/空白不敏感）——上游 role 白名单校验的正是归一化后的值。
func isAssistantMsg(msg map[string]any) bool {
	role, _ := msg["role"].(string)
	return strings.EqualFold(strings.TrimSpace(role), "assistant")
}

// hasReasoningField 报告消息是否携带任一推理字段（**存在即算**：空串/null 也是字段，
// 正是本步要修的形态）。缺失 = 无推理可裁，不凭空创建。
func hasReasoningField(msg map[string]any) bool {
	if _, ok := msg["reasoning_content"]; ok {
		return true
	}
	_, ok := msg["reasoning"]
	return ok
}

// reasoningText 取消息的推理文本：reasoning_content 优先、reasoning 兜底，
// 两字段皆非空 string 时才返回非空串。与 206b「两字段同源」口径一致（承载同一份推理，
// 取任一非空者即可）。
func reasoningText(msg map[string]any) string {
	if s, ok := msg["reasoning_content"].(string); ok && s != "" {
		return s
	}
	if s, ok := msg["reasoning"].(string); ok && s != "" {
		return s
	}
	return ""
}

// fillPlaceholder 把「存在但非非空字符串」的推理字段写成占位串（keeper 专用：
// 非空 string 原文一字不改）；返回是否改动。字段缺失返回 false（不凭空创建）。
func fillPlaceholder(msg map[string]any, key string) bool {
	v, ok := msg[key]
	if !ok {
		return false
	}
	if s, ok := v.(string); ok && s != "" {
		return false
	}
	msg[key] = reasoningPlaceholder
	return true
}

// blankReasoningField 把存在的推理字段无条件写成占位串；返回是否改动。
// 已是占位串时返回 false（幂等：裁剪过的历史再被裁剪不产生第二形态）。
func blankReasoningField(msg map[string]any, key string) bool {
	v, ok := msg[key]
	if !ok {
		return false // 字段缺失：不凭空创建
	}
	if s, ok := v.(string); ok && s == reasoningPlaceholder {
		return false
	}
	msg[key] = reasoningPlaceholder
	return true
}
