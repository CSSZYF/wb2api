// thinking.go DeepSeek 思维链开启：出站请求体注入 thinking:{type:"enabled"} + 默认档位。
//
// 根因（issue #43，Hermes 逆向官方客户端 codebuddy.js 已确认）：
// 官方客户端对 deepseek 系模型标记 thinkingFormat:"deepseek" + requiresReasoningContentOnAssistantMessages，
// 发请求时「开思考」必须显式带 thinking:{type:"enabled"}，否则上游默认按不思考应答
// （思维链不返回）。网关 payload 层此前完全不感知该字段，透传请求没有这个开关
// → 上游不给思维链；glm/kimi 走其他 thinkingFormat（qwen 系 enable_thinking 或默认开）所以正常。
//
// 打回修复（Hermes #43 验收实测）：
//
//	thinking.type=enabled 单一字段不足——真实上游 deepseek-v4-flash 对「不带 reasoning_effort」的裸请求
//	仍然按不思考应答（reasoning_content 长度 0），带 reasoning_effort:high 才有思维链。
//	逆向 codebuddy.js 证实：isThinkingEnabled = !!(reasoning_summary || reasoning_effort || reasoning?.effort)，
//	case "deepseek" 的 enabled 分支在实际出站里同时保留 reasoning_effort，官方「开思考」= thinking.type:enabled
//	+ 某档 effort；默认档来自 reasoning.defaultEffort ?? 兜底 "high"（configure thinking 无来源时 warn fallback to 'high'）。
//
// 行为对齐官方客户端（两路组合）：
//   - thinking.type 已显式 enabled / disabled → 客户端显式控制，绝不覆盖；disabled 时照抄 case 行为
//     删 reasoning_effort（snake/camel 双字段）。enabled 但缺 effort → 补默认档（官方 configure 行为）。
//   - 无 thinking / thinking.type 空 / 已有 reasoning_effort → 注入 {type:"enabled"} + 补默认档。
//   - 显式 reasoning_effort 一律不覆盖、不降级（降级交给 payload.go normalizeReasoningEffort）。
//   - 非 deepseek 模型（glm/kimi/qwen 等）→ 零改动。
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort 官方客户端默认档兜底（configure thinking 无来源时 warn fallback to 'high'，
// REASONING_SUPPLEMENTS.defaultEffort 亦为 "high"）。补入后走 normalizeReasoningEffort 降级管线，
// 模型不支持 high 时自动落到 ≤high 的最高支持档。
const defaultDeepSeekEffort = "high"

// lookupDefaultEffort 从 FetchModels 缓存的 defaultEfforts 表按模型名查默认档。
// nil map 或模型未缓存 → 空串（thinking.go 回退硬编码 high）。
// 键为模型 ID 原样（与 efforts 缓存对齐：normalizeReasoningEffort 精确匹配 model）。
func lookupDefaultEffort(defaultEfforts map[string]string, model string) string {
	if len(defaultEfforts) == 0 || model == "" {
		return ""
	}
	return defaultEfforts[model]
}

// isDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体；
// 前缀匹配对齐官方 thinkingFormat:"deepseek" 的判定口径，避免漏注。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// thinkingEnabled 半边：读注入后的 thinking.type（与官方 el.thinkingEnabled 对应）。
	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok {
		if typ, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			thinkingEnabled = true
		}
	}
	// hasTrace 半边：检测是否有任何 reasoning 痕迹（非空 reasoning 或已有 reasoning_content）。
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}
	// 第二遍：所有 assistant 消息补/复制 reasoning_content，并镜像保证
	// reasoning 字段存在且非空（issue #165 追评：部分租户校验 len(reasoning)>0）。
	//
	// 跳过条件只认 string（官方 "string"!=typeof 才动手）：null/数字归一化。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		// 第一段：rc 归一化（已有 string → 不覆盖；否则复制 reasoning；
		// 两者皆无 → 补空串）。本次用于 rc 的来源文本记进局部
		// 变量 rc，只作镜像来源用——两字段同源（不各自独立判定，
		// 避免「rc 补空串、reasoning 却复制出旧值」这类错配）。
		rc, hasRC := msg["reasoning_content"].(string)
		if !hasRC {
			if r, ok := msg["reasoning"].(string); ok {
				rc = r
			} else {
				rc = ""
			}
			msg["reasoning_content"] = rc
		}
		// 第二段：镜像写 reasoning。已非空 string → 不覆盖（客户端显式给的
		// reasoning 优先，与 rc 的「不覆盖」同口径）；否则用上面确定的 rc 来源补齐，
		// 两者皆空时补单个空格 " "——上游 len>0 不 trim（空白串过闸、
		// 空串 400），该字段是透传校验位非内容消费位。
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			continue // 已非空 → 不覆盖
		}
		if rc != "" {
			msg["reasoning"] = rc
		} else {
			msg["reasoning"] = " "
		}
	}
}
func injectThinking(obj map[string]any, defaultEffort string) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	// 显式控制分支：type 非空（enabled/disabled 均为明确意图）→ 不改 type。
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return // disabled：关思考且不带任何 effort（照抄客户端 case 行为）
		}
		ensureDeepSeekEffort(obj, defaultEffort) // 显式 enabled 缺 effort → 补默认档
		return
	}
	// 无 thinking（或 thinking 非法非对象值）或 thinking 对象 type 缺失/为空：
	// 注入 enabled（客户端 case "deepseek" 行为）。有 reasoning_effort 也走此分支
	// （effort 保留给既有降级逻辑，开关照开）。
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj, defaultEffort)
}

// ensureDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底）。
// 已有任一 effort → 不覆盖（显式档位不做任何改写，降级交给 normalizeReasoningEffort）。
// defaultEffort 空串 → 回退 defaultDeepSeekEffort（硬编码 "high"）。
func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	_, hasSnake := obj["reasoning_effort"]
	if hasSnake {
		return
	}
	_, hasCamel := obj["reasoningEffort"]
	if hasCamel {
		return
	}
	if defaultEffort == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}
