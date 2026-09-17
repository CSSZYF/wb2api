// context_catalog.go context_length / max_output_tokens 字段级静态兜底知识表。
//
// 为什么需要：上游 /v2 与 /console 目录都会出现 maxInputTokens/maxOutputTokens 为零值
// （窄表形态、老账号、灰度模型），旧实现按 131072 兜底——那是**编造值**，会让
// Codex/ZCode/Claude Code 等按 context_length 决策的客户端提前截断、白白丢上下文；
// global 分支更是一度完全不输出 context_length，客户端只能回退自身小默认值。
//
// 数据来源两级（与 hidden.go / pinned.go 的"一处定义、两处投影共用"同原则）：
//   - 上游动态值（ModelInfo.ContextWindow/MaxTokens，即 maxInputTokens/maxOutputTokens）权威，优先；
//   - 本文件按模型的知识表（远端零值时补齐）。
//
// **未知不编造**：两级都查不到 → ok=false，调用方省略字段（context_length 与
// max_output_tokens 同口径）。上游 sliver 版对未知 context_length 兜底 1M
// （"宁可高估不低估"），本仓不采纳：1M 同样是编造值，只是量级更大——客户端拿到
// 一个不存在的窗口会在真正触顶时才被上游报错，代价从"提前截断"换成"中途失败"。
// 省略字段则把决策权交回客户端（各客户端对缺失字段有自己的保守默认）。
//
// 知识表**一处定义、CN/global 两域共用**：context_length 是模型固有属性——fork
// 706412584 实测结论「两区是同一套 API 的两次部署」，同 id 上下文一致，无按 realm
// 分表必要（与 effort 档位的 realm 分表刻意不同）。
//
// 值来源三类，逐条注释标注：
//   - 实测：fork 706412584 直连上游 /console/enterprises/personal/models 的
//     maxInputTokens（上游 sliver 仓 context_catalog.go 收录，2026-09-13 实测；
//     global 侧无法直测、按同 id 外推）；
//   - models.dev：https://models.dev/ 收录值（2026-09-16 查询，取多 provider 共识值；
//     官方源如 moonshotai/zai 优先）。未收录或歧义大者不编造；
//   - 本仓目录：fork 静态目录 wb_catalog.py 收录值（本仓在售、上游表未覆盖的 id）。
//
// 路由别名（default-model / fast-model / balanced-model / primary-model / deep-model，
// 即 DefaultHiddenModels）**刻意不入表**：它们由上游按当时的策略转派到别的真实模型，
// 窗口随策略漂移（hidden.go 的既有结论），写死窗口就是编造——宁可省略字段。
// （"auto" 不属此类：它是 CN 目录实际下发的模型 id，见 client_test.go 的目录样本。）
package upstream

import "strings"

// contextCap 一个模型的上下文能力（字段级兜底条目）。
// context 必为正（否则条目无意义，按未收录处理）；
// maxOutput 为 0 表示输出上限未知 → max_output_tokens 字段省略（不编造）。
type contextCap struct {
	context   int64
	maxOutput int64
}

// contextCapFallback context_length / max_output_tokens 知识表（CN/global 共用）。
// 每条注释标注来源：实测 = 直连 CN /console 实测 maxInputTokens（global 侧为同 id 外推）；
// models.dev = 2026-09-16 收录共识值；估算 = 同族外推；本仓目录 = wb_catalog.py 收录值。
var contextCapFallback = map[string]contextCap{
	// ---- GLM 家族（z-ai）----
	"glm-5.2":       {context: 1000000, maxOutput: 131072}, // 实测（CN 1M；models.dev 共识 1M/131072）
	"glm-5.1":       {context: 200000, maxOutput: 131072},  // 实测（CN 200K；models.dev 共识 200K/131072）
	"glm-5.3":       {context: 1000000, maxOutput: 131072}, // 实测外推 + models.dev 共识 1M/131072
	"glm-5.3-flash": {context: 1000000, maxOutput: 131072}, // models.dev 共识 1M/131072
	"glm-5v-turbo":  {context: 200000, maxOutput: 131072},  // 实测（CN 200K；models.dev 共识 200K/131072）

	// ---- Kimi 家族（moonshot）----
	"kimi-k2.7":         {context: 256000, maxOutput: 65536},   // 实测（CN 256K）；输出 65536 为 models.dev kimi-k2.7-code 同族估算
	"kimi-k2.6":         {context: 256000, maxOutput: 262144},  // 实测（CN 256K）；输出 models.dev 官方 262144
	"kimi-k2.5":         {context: 164000, maxOutput: 262144},  // 实测（global 侧同 id 外推 164K）；输出 models.dev 共识 262144
	"kimi-k3":           {context: 1048576, maxOutput: 131072}, // models.dev 官方（moonshotai 1M/128K）
	"kimi-k2.8-preview": {context: 1048576, maxOutput: 0},      // models.dev（Kimi K2.8 Preview 1M；输出上限未收录，省略）

	// ---- MiniMax / 混元（tencent）----
	"minimax-m3":        {context: 512000, maxOutput: 512000}, // 实测（CN 512K）；输出 models.dev 共识 512000
	"hy3":               {context: 192000, maxOutput: 64000},  // 实测（CN 192K/64K，repo hy3 抓取样本同值）
	"hy3-preview":       {context: 262144, maxOutput: 64000},  // models.dev（共识 262144/64000）
	"hy3-preview-agent": {context: 262144, maxOutput: 64000},  // 估算（hy3-preview 的 agent 变体，同族外推）
	"hy4-preview":       {context: 1000000, maxOutput: 64000}, // 实测外推 + models.dev（~1M/64000）
	"hy4-preview-f":     {context: 1000000, maxOutput: 64000}, // 本仓目录（wb_catalog.py 1M/64000）
	"hy4-preview-x":     {context: 1000000, maxOutput: 64000}, // 实测外推（1M）；输出同族 hy4-preview 估算

	// ---- DeepSeek 家族 ----
	"deepseek-v4-pro":     {context: 1000000, maxOutput: 384000}, // 实测（CN 1M；models.dev 共识 1M/384000）
	"deepseek-v4-flash":   {context: 1000000, maxOutput: 384000}, // 实测（CN 1M；models.dev 共识 1M/384000）
	"deepseek-v4.1-flash": {context: 1000000, maxOutput: 384000}, // 实测外推 + models.dev 共识 1M/384000

	// ---- OpenAI / Google（global 域家族）----
	"gpt-6-astra":      {context: 1050000, maxOutput: 128000}, // models.dev（全 provider 一致 1050000/128000）
	"gpt-5.6-sol":      {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.6-terra":    {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.6-luna":     {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.5":          {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.4":          {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.3-codex":    {context: 400000, maxOutput: 128000},  // models.dev（全 provider 一致 400000/128000）
	"gemini-3.5-flash": {context: 1048576, maxOutput: 65536},  // models.dev 共识

	// ---- CN 目录的 auto 档 ----
	"auto": {context: 168000, maxOutput: 0}, // 实测外推（fork global 静态表 168K）；输出上限未知，省略
}

// lookupContextCap 按模型 id 查知识表（去空白；id 大小写敏感，与上游目录一致）。
func lookupContextCap(model string) (contextCap, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return contextCap{}, false
	}
	cap, ok := contextCapFallback[model]
	if !ok || cap.context <= 0 {
		return contextCap{}, false
	}
	return cap, true
}

// ContextWindowListing 模型在 /v1/models 的 context_length（两级查找）：
// remote（上游 maxInputTokens）>0 时权威；否则查知识表；仍未收录 → ok=false，
// 调用方**省略 context_length 字段**（不编造、不落 131072/1M）。
// 绝不再透出假 131072。
func ContextWindowListing(model string, remote int64) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if cap, ok := lookupContextCap(model); ok {
		return cap.context, true
	}
	return 0, false
}

// MaxOutputTokensListing 模型在 /v1/models 的 max_output_tokens（两级查找，与
// ContextWindowListing 同口径）：remote（上游 maxOutputTokens）>0 时权威；否则查
// 知识表；仍未收录（或表内 maxOutput 为 0 = 未知）→ ok=false，调用方省略字段。
// 输出上限无「宁可高估」的安全侧，不编造。
func MaxOutputTokensListing(model string, remote int64) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if cap, ok := lookupContextCap(model); ok && cap.maxOutput > 0 {
		return cap.maxOutput, true
	}
	return 0, false
}
