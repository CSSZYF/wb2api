// usage.go 出站 usage 的缓存命中别名归一（吸收上游 25016de）。
//
// 问题：部分上游把真实缓存命中量放在 prompt_tokens_details.cached_tokens，同时把
// cache_read_input_tokens / cached_tokens / prompt_cache_hit_tokens 三个兼容别名
// 留为 0。下游严格解析器（按字段存在性优先读扁平别名）会读到 0，把「命中」误判成
// 「未命中」——客户端据此显示的缓存收益全错，按命中量做的成本核算也跟着偏。
//
// 归一策略：先在五个已知位置里取**第一个正值**（唯一事实来源），再把该值镜像到
// 全部别名与嵌套明细。全 0（真未命中）时零改动：不编造、不凭空建子表。
package upstream

// normalizeUsageCacheAliases 把 usage 里的缓存命中别名统一为同一个值。
// usage 为 nil 或取不到正值时原样返回（不新建字段）。
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails

	// Responses API 客户端用这个嵌套形态。上游已下发时同步镜像，**不**为
	// Chat-only 客户端臆造该字段（多一个不存在于上游的字段会被严格校验器拒绝）。
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

// bestUsageCacheHitTokens 按可信度顺序取第一个正的命中量：嵌套明细优先（上游主形态），
// 其次扁平别名。全部非正 → ok=false。
func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}
	return 0, false
}

// positiveUsageNumber 把 JSON number 归一为 float64；非数字或非正值 → ok=false。
// 覆盖上游可能给出的各种数值类型（json.Unmarshal 默认 float64，其余为防御）。
func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, n > 0
	case float32:
		v := float64(n)
		return v, v > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	default:
		return 0, false
	}
}

// cloneUsageMap 浅拷贝 usage（不改上游原 map——同一 map 可能被聚合与非流式两条
// 路径共享，就地改写会互相污染）。
func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

// cloneUsageDetails 浅拷贝 usage[key] 的子表；缺失或非 map 时返回空表（调用方
// 随后写入 cached_tokens 即新建）。
func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	for detailKey, value := range details {
		out[detailKey] = value
	}
	return out
}
