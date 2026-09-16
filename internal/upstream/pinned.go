// pinned.go 强制内置的模型条目。
//
// 为什么需要：有些模型上游的 /v2 目录**不给**（账号差异 / 灰度 / 区域），但它们实际
// 可调用。这类模型在探测结果里"没有"不等于"不可用"——写死一份能力快照，让它稳定
// 出现在面板「模型与档位」与 /v1/models 里。
//
// 代价必须写清楚：快照里的倍率 / 窗口 / 档位是人工跟进的，上游改了不会自动跟随。
// 所以合并策略是"上游数据优先"——只要探测结果里出现了同名模型，就用上游那份，
// 写死条目只在**缺失**时兜底（MergePinned）。
package upstream

// PinnedModel 强制内置的模型条目。字段与 /v1/models 的输出一一对应，
// 同时充当 config 里 models.pinned_models 的元素类型。
type PinnedModel struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Credits            string   `json:"credits"`              // 积分倍率，如 "x0.03"
	ContextLength      int64    `json:"context_length"`       // 上下文窗口（token）
	MaxOutputTokens    int64    `json:"max_output_tokens"`    // 最大输出（token）
	MaxAllowedSize     int64    `json:"max_allowed_size"`     // 单请求体上限；0 = 不输出
	DefaultEffort      string   `json:"default_effort"`       // 默认思考档
	SupportedEfforts   []string `json:"supported_efforts"`    // 空 = 固定档（不可选，只有 DefaultEffort）
	SupportsReasoning  bool     `json:"supports_reasoning"`   // false = 面板显示"不支持思考"
	CanDisableThinking bool     `json:"can_disable_thinking"` // 思考可关（off 档可用）
	SupportsImages     bool     `json:"supports_images"`
}

// DefaultPinnedModels 出厂写死条目。
//
// deepseek-v4.1-flash：国际版 /v2 目录不返回它（实测账号的 cli agents 名单里没有），
// 但模型可调用，故写死。数值来自官方客户端实测：固定档 high、窗口 1000K、输出 128K、
// 倍率 x0.03。上游若开始返回它，此处自动让位（MergePinned 不覆盖已有条目）。
var DefaultPinnedModels = []PinnedModel{
	{
		ID:                "deepseek-v4.1-flash",
		Name:              "Deepseek-V4.1-Flash",
		Credits:           "x0.03",
		ContextLength:     1000000,
		MaxOutputTokens:   128000,
		DefaultEffort:     "high",
		SupportsReasoning: true,
	},
}

// ResolvePinnedModels 归一化写死名单：
//   - nil（配置键缺席）→ DefaultPinnedModels；
//   - 非 nil（含显式空数组）→ 原样采用，空数组表示不强行内置任何模型。
func ResolvePinnedModels(cfg []PinnedModel) []PinnedModel {
	if cfg == nil {
		return DefaultPinnedModels
	}
	out := make([]PinnedModel, 0, len(cfg))
	for _, p := range cfg {
		if p.ID != "" {
			out = append(out, p)
		}
	}
	return out
}

// Info 转成上游探测结果的同构形态（供面板与 /v1/models 复用同一条渲染路径）。
func (p PinnedModel) Info() ModelInfo {
	return ModelInfo{
		ID:                 p.ID,
		Name:               normalizeModelName(p.Name),
		ContextWindow:      p.ContextLength,
		MaxTokens:          p.MaxOutputTokens,
		MaxAllowedSize:     p.MaxAllowedSize,
		Efforts:            p.SupportedEfforts,
		DefaultEffort:      p.DefaultEffort,
		CanDisableThinking: p.CanDisableThinking,
		SupportsReasoning:  p.SupportsReasoning,
		SupportsImages:     p.SupportsImages,
		Credits:            p.Credits,
	}
}

// Entry 转成 /v1/models 的条目形态。
//
// 关键取舍：SupportedEfforts 为空时**不输出该键**，面板据此渲染成"固定档 · 默认 X"
// （空数组与"不可选"是同一语义，输出空数组会让前端多一次无意义的判断）。
func (p PinnedModel) Entry() map[string]any {
	e := map[string]any{
		"id":       p.ID,
		"object":   "model",
		"created":  1753600000,
		"owned_by": "workbuddy",
	}
	if p.ContextLength > 0 {
		e["context_length"] = p.ContextLength
	}
	if p.MaxOutputTokens > 0 {
		e["max_output_tokens"] = p.MaxOutputTokens
	}
	if p.MaxAllowedSize > 0 {
		e["max_allowed_size"] = p.MaxAllowedSize
	}
	if p.Credits != "" {
		e["credits"] = p.Credits
	}
	if p.DefaultEffort != "" {
		e["default_effort"] = p.DefaultEffort
	}
	if len(p.SupportedEfforts) > 0 {
		e["supported_efforts"] = p.SupportedEfforts
	}
	if p.SupportsReasoning {
		e["supports_reasoning"] = true
		e["can_disable_thinking"] = p.CanDisableThinking
	}
	if p.SupportsImages {
		e["supports_images"] = true
	}
	return e
}

// MergePinned 把写死条目并入探测结果：**上游已返回的同名模型原样优先**，
// 只追加缺失的。顺序上追加在末尾，列表整体保持"上游目录在前、兜底在后"。
func MergePinned(in []ModelInfo, pinned []PinnedModel) []ModelInfo {
	if len(pinned) == 0 {
		return in
	}
	have := make(map[string]bool, len(in)+len(pinned))
	for _, mi := range in {
		have[mi.ID] = true
	}
	out := in
	for _, p := range pinned {
		if p.ID == "" || have[p.ID] {
			continue
		}
		have[p.ID] = true
		out = append(out, p.Info())
	}
	return out
}
