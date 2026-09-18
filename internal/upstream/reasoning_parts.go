// reasoning_parts.go CN 域 reasoning content-part 转换。
//
// 现场（2026-09-18，用户生产环境，已完整复现）：ZCode 3.11.2 把**思考内容**作为
// content 数组里的一个 part 发出：
//
//	{"role":"assistant","content":[{"type":"reasoning","text":"[A]\nChen reports: ..."},
//	                               {"type":"text","text":"..."}]}
//
// global 域（www.workbuddy.ai）接受该格式；CN 域（copilot.tencent.com）直接 HTTP 400：
//
//	{"code":11101,"msg":"Parse message failed: unsupported content type at index 0: reasoning"}
//
// 会话历史里一旦出现第一个 reasoning part，之后每次请求都带它 → 每个账号都被 400
// → 网关侧表现为「对话到一半突然 503，换号也没用」。
//
// 修法（实测两种等价，取第一种）：把 reasoning part 的 text 提升为 message 顶层的
// reasoning_content 字符串字段（即 DeepSeek 多轮一致性用的同一字段），数组里删掉该
// part；删后数组为空则 content 置 ""（避免上游报 content 不能为空类错误）。
// 与零宽脱敏同类：**只做「数组 part → 顶层字符串字段」的结构搬移，text 一字不动**——
// 用户可见内容与模型读到的语义都不变。
//
// 作用域（三条边界，勿越界）：
//   - 只认 CN 域：调用点按 realmKey(realm)=="cn" 把关（见 payload.go），global 域上游
//     接受原格式，改它反而引入风险，一律原样透传。
//   - 不按模型名 gate：reasoning part 不是 deepseek 专属，任何模型的思考内容都可能
//     这么发（thinking.go 的 isDeepSeekModel 只管思维链开关注入，与此无关）。
//   - 只动 assistant 消息的 reasoning part：reasoning_content 是 assistant 侧的字段，
//     把别的角色的 part 提到顶层属于语义错位；其余 part 类型（text/image/tool_use…）
//     一律不动，也不把数组扁平化成字符串（那会改变上游看到的结构）。
package upstream

import (
	"log"
	"strings"
)

// promoteReasoningParts 把 CN 域出站 messages 里 assistant 消息 content 数组中的
// reasoning part 提升为顶层 reasoning_content 字符串字段，并从数组移除该 part。
//
// 逐条 assistant 消息：
//   - 收集全部 reasoning part 的 text（按原顺序拼接）→ 顶层 reasoning_content；
//     已有非空 reasoning_content 时不覆盖、**追加在后**（两段文本均逐字保留，
//     不插入任何分隔符——不发明原文本里没有的字符，也不丢已有思维链）；
//   - 数组里只剩其他 part → 保留数组形态（不扁平化）；
//   - 数组被删空 → content 置 ""（string）；
//   - 无 reasoning part 的消息零改动零分配（hasReasoningPart 快路径）。
//
// 返回被转换的消息数（仅用于日志/测试断言，0 表示整体零改动）。
func promoteReasoningParts(obj map[string]any) int {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return 0
	}
	convertedMsgs, convertedParts := 0, 0
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		// 角色判定与 normalizeRoles 同口径（大小写/空白不敏感）：上游 role 白名单
		// 校验的正是归一化后的值。
		role, _ := msg["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		if !hasReasoningPart(parts) {
			continue // 快路径：无 reasoning part → 不分配、不改写
		}
		var joined strings.Builder
		kept := make([]any, 0, len(parts))
		hit := 0
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				kept = append(kept, p)
				continue
			}
			if typ, _ := part["type"].(string); !strings.EqualFold(strings.TrimSpace(typ), "reasoning") {
				kept = append(kept, p)
				continue
			}
			hit++
			// 只有 string 形态的 text 参与提升；缺 text / text 非 string 的
			// reasoning part 同样是上游不认的 part，一并移除（无内容可丢）。
			if txt, ok := part["text"].(string); ok {
				joined.WriteString(txt)
			}
		}
		text := joined.String()
		if existing, ok := msg["reasoning_content"].(string); ok && existing != "" {
			msg["reasoning_content"] = existing + text
		} else {
			msg["reasoning_content"] = text
		}
		if len(kept) == 0 {
			msg["content"] = ""
		} else {
			msg["content"] = kept
		}
		convertedMsgs++
		convertedParts += hit
	}
	if convertedMsgs > 0 {
		model, _ := obj["model"].(string)
		log.Printf("cn reasoning parts promoted model=%s msgs=%d parts=%d", model, convertedMsgs, convertedParts)
	}
	return convertedMsgs
}

// hasReasoningPart 报告 content 数组里是否含 reasoning part。
// 判定与 promoteReasoningParts 的移除口径一致（type 大小写/空白不敏感）。
func hasReasoningPart(parts []any) bool {
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := part["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "reasoning") {
			return true
		}
	}
	return false
}
