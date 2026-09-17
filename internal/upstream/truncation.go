// truncation.go 工具调用的残缺参数检测 + 空占位剔除（吸收参考仓库 sse.ts:158-167
// isTruncatedArguments 与 hub wb_proxy.py aggregate_stream 的占位过滤语义）。
//
// 背景：SSE 流被截断（连接中断 / finish_reason==length）时，工具调用的 arguments
// 会只剩半截 JSON。此时网关若把脏参数原样交给客户端，客户端解析会报非法 JSON 并卡死会话。
// 参考仓库的处置是丢弃残缺调用（报告 max-tokens），而非补成 {} 伪造合法外观。
//
// 关键区分：只把「非空但无法解析」视为截断。空串是合法的无参数工具；能解析但类型不对
// （标量 / 数组）属于模型输出错误，交给客户端 schema 校验回传即可，不在此判定。
package upstream

import (
	"encoding/json"
	"strings"
)

// isTruncatedArguments 判定工具参数字符串是否因分片丢失而残缺（区别于「该工具本就无参数」）。
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice）。
// 只依据 isTruncatedArguments 判定，不改动任何保留的调用（正例零改动、顺序保持）。
func dropTruncatedToolCalls(calls []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			kept = append(kept, call)
			continue
		}
		args, _ := fn["arguments"].(string)
		if isTruncatedArguments(args) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}

// isPlaceholderToolCall 判定 tool_call 是否为上游流末尾的空占位（function.name 与
// function.arguments **双空**）。只有双空才算占位：单空（有 name 无参 / 有参无 name）
// 是真实调用的残缺形态，交客户端 schema 校验判定，网关不替客户端做决定。
func isPlaceholderToolCall(call map[string]any) bool {
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return false // 无 function 键的形态不在本判定范围（不因缺键误删）
	}
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	return strings.TrimSpace(name) == "" && strings.TrimSpace(args) == ""
}

// dropPlaceholderToolCalls 过滤掉双空的 tool_call 占位（返回新 slice）。上游会在流
// 末尾发 function_call:{"name":"","arguments":""} 占位，聚合后变成 finish_reason=
// "tool_calls" 却无实际调用的响应，严格客户端会死等终止事件。
func dropPlaceholderToolCalls(calls []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		if isPlaceholderToolCall(call) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}
