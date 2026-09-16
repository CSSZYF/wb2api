// chat_debug.go 思考字段出站诊断（env WB2A_DEBUG_REASONING 开启）。
//
// 存在的理由：思考档位相关的字段有三种形态（扁平 reasoning_effort、camel reasoningEffort、
// 嵌套 reasoning.effort），客户端用哪种、网关改写后变成哪种、模型是否在能力缓存里——
// 这三件事都不在日志里，于是"我调了 low/medium/high 感觉一样"只能靠猜。
//
// 只打思考相关字段，**不打消息内容**：诊断不能变成内容泄漏通道。
package upstream

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// DebugReasoningEnabled 报告思考字段诊断是否开启（每次读取，便于临时排障）。
func DebugReasoningEnabled() bool { return os.Getenv("WB2A_DEBUG_REASONING") != "" }

// reasoningSnapshot 提取请求体里的思考相关字段，格式化为一行诊断串。
//
// 覆盖四种客户端表达方式（任一存在即会出现在诊断里，方便判断客户端用的是哪种）：
//   - thinking.type          官方开关（enabled/disabled）
//   - reasoning_effort       扁平 snake
//   - reasoningEffort        扁平 camel
//   - reasoning.effort       嵌套对象（官方 codebuddy.js 用的是这个形态）
//   - reasoning_summary      官方判定开启的另一个信号
//
// 另附能力缓存命中情况：模型不在缓存 → 网关一律透传不降级，"四档没区别"往往出在这里。
// 解析失败返回空串（调用方不打印）。
func reasoningSnapshot(body []byte, efforts map[string][]string, defaults map[string]string) string {
	var obj map[string]any
	if len(body) == 0 || json.Unmarshal(body, &obj) != nil {
		return ""
	}
	model, _ := obj["model"].(string)

	// thinking.type
	thinkingType := ""
	if th, ok := obj["thinking"].(map[string]any); ok {
		thinkingType, _ = th["type"].(string)
	}
	// 三种 effort 形态
	effSnake, hasSnake := obj["reasoning_effort"]
	effCamel, hasCamel := obj["reasoningEffort"]
	nestedEffort := ""
	nestedSummary := ""
	if r, ok := obj["reasoning"].(map[string]any); ok {
		nestedEffort, _ = r["effort"].(string)
		if v, ok := r["summary"]; ok {
			nestedSummary = fmt.Sprintf("%v", v)
		}
	}

	sup, known := efforts[model]
	def := defaults[model]

	var b strings.Builder
	fmt.Fprintf(&b, "model=%s", model)
	fmt.Fprintf(&b, " thinking.type=%q", thinkingType)
	if hasSnake {
		fmt.Fprintf(&b, " reasoning_effort=%v", effSnake)
	}
	if hasCamel {
		fmt.Fprintf(&b, " reasoningEffort=%v", effCamel)
	}
	if nestedEffort != "" {
		fmt.Fprintf(&b, " reasoning.effort=%q", nestedEffort)
	}
	if nestedSummary != "" {
		fmt.Fprintf(&b, " reasoning.summary=%s", nestedSummary)
	}
	fmt.Fprintf(&b, " | cache_known=%v cache_supported=%v cache_default=%q", known, sup, def)
	return b.String()
}

// logReasoning 打印一行思考诊断（tag 区分改写前 in / 改写后 out）。
func logReasoning(tag string, body []byte, efforts map[string][]string, defaults map[string]string) {
	if !DebugReasoningEnabled() {
		return
	}
	if s := reasoningSnapshot(body, efforts, defaults); s != "" {
		log.Printf("[dbg-reasoning %s] %s", tag, s)
	}
}
