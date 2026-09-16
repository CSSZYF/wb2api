// hidden.go 对外展示口径的模型名治理：
//  1. 路由别名不列为"模型"（DefaultHiddenModels）；
//  2. 上游部分模型名带冗余厂商标注（"DeepSeek: DeepSeek V4.1 Flash"）→ 归一化。
//
// 这两个口径必须由 upstream 提供、由 handler 与 panel 共用：面板「模型与档位」与
// /v1/models 是同一份目录的两种投影，两边各写一份过滤就会出现"面板看得见、客户端
// 调不到"（或反之）的漂移。
package upstream

import (
	"sort"
	"strings"
)

// DefaultHiddenModels 上游目录里的「路由策略别名」，不是真实模型。
//
// 它们由官方客户端的自动选档逻辑使用（Auto/Fast/Balanced/Primary/Deep）：选中后由
// 上游按当时的策略转派到别的真实模型，因此其倍率、窗口、思考档随时会变，被列成独立
// 模型只会误导（用户以为在选一个固定的模型）。
//
// 想展示它们：config 里 models.hidden_models 显式给 [] （空数组 = 全不隐藏）。
var DefaultHiddenModels = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"deep-model",
}

// HiddenSet 对外隐藏的模型名集合（大小写不敏感，nil 集合 = 不隐藏任何模型）。
type HiddenSet map[string]bool

// ResolveHiddenModels 归一化隐藏名单：
//   - nil（配置键缺席）→ DefaultHiddenModels；
//   - 非 nil（含显式空数组）→ 原样采用，空数组表示全部展示。
func ResolveHiddenModels(cfg []string) HiddenSet {
	src := cfg
	if src == nil {
		src = DefaultHiddenModels
	}
	out := make(HiddenSet, len(src))
	for _, id := range src {
		if id = strings.TrimSpace(id); id != "" {
			out[strings.ToLower(id)] = true
		}
	}
	return out
}

// Has 报告该模型名是否被隐藏。
func (h HiddenSet) Has(id string) bool {
	if h == nil {
		return false
	}
	return h[strings.ToLower(strings.TrimSpace(id))]
}

// Names 返回隐藏名单（升序），供面板/诊断展示「到底藏了什么」。
func (h HiddenSet) Names() []string {
	if len(h) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(h))
	for id := range h {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// FilterInfo 过滤 []ModelInfo（保序）。
func (h HiddenSet) FilterInfo(in []ModelInfo) []ModelInfo {
	if len(h) == 0 {
		return in
	}
	out := make([]ModelInfo, 0, len(in))
	for _, mi := range in {
		if h.Has(mi.ID) {
			continue
		}
		out = append(out, mi)
	}
	return out
}

// FilterNames 过滤 []string 模型名（保序）。
func (h HiddenSet) FilterNames(in []string) []string {
	if len(h) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, id := range in {
		if h.Has(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// normalizeModelName 去掉上游名称里冗余的厂商标注。
//
// 上游对部分国际版模型返回形如 "DeepSeek: DeepSeek V4.1 Flash" 的名称：冒号前是厂商、
// 冒号后又重复一遍，作为面板副标题纯属噪音。规则：冒号前段与冒号后段的前缀**不区分
// 大小写相等**、且正好落在词边界（空格/连字符/下划线/斜杠/结尾）时，剥掉 "<厂商>: "。
//
// 其他形态一律原样返回（"Auto"、"GLM 5.2"、"GPT 5.4" 等都不含冒号或不符合规则）。
func normalizeModelName(name string) string {
	name = strings.TrimSpace(name)
	idx := strings.IndexByte(name, ':')
	if idx <= 0 || idx >= len(name)-1 {
		return name // 无冒号 / 冒号在首尾 → 无从剥离
	}
	vendor := strings.TrimSpace(name[:idx])
	rest := strings.TrimSpace(name[idx+1:])
	if vendor == "" || rest == "" || len(rest) < len(vendor) {
		return name
	}
	if !strings.EqualFold(rest[:len(vendor)], vendor) {
		return name
	}
	if len(rest) > len(vendor) {
		switch rest[len(vendor)] {
		case ' ', '-', '_', '/':
		default:
			return name // "GLM: GLMXYZ" 这类同前缀但不同词 → 不剥
		}
	}
	return rest
}
