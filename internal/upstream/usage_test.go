package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeUsageCacheAliasesMirrorsNestedHit 上游把真实命中量放在
// prompt_tokens_details.cached_tokens，同时把 cache_read_input_tokens / cached_tokens
// / prompt_cache_hit_tokens 三个兼容别名留为 0——下游严格解析器会优先读那些 0，
// 于是「缓存命中」被判成未命中。归一后四个位置同值（对齐上游 25016de）。
func TestNormalizeUsageCacheAliasesMirrorsNestedHit(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            21041.0,
		"completion_tokens":        8.0,
		"total_tokens":             21049.0,
		"cache_read_input_tokens":  0.0,
		"cached_tokens":            0.0,
		"prompt_cache_hit_tokens":  0.0,
		"prompt_cache_miss_tokens": 177.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 20864.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	for _, key := range []string{
		"cache_read_input_tokens",
		"cached_tokens",
		"prompt_cache_hit_tokens",
	} {
		if got[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, got[key])
		}
	}
	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 20864.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 20864", details["cached_tokens"])
	}
	// 不得污染其他字段（miss 数、prompt/completion 原样）。
	if got["prompt_cache_miss_tokens"] != 177.0 || got["prompt_tokens"] != 21041.0 {
		t.Fatalf("其他 usage 字段被改动: %v", got)
	}
}

// TestNormalizeUsageCacheAliasesPreservesZeroResult 全 0（真未命中）时零改动：
// 不编造命中量，也不凭空建 prompt_tokens_details 子表。
func TestNormalizeUsageCacheAliasesPreservesZeroResult(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           35.0,
		"completion_tokens":       2.0,
		"total_tokens":            37.0,
		"cache_read_input_tokens": 0.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 0.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 0", details["cached_tokens"])
	}
	if _, has := got["cached_tokens"]; has {
		t.Fatalf("全 0 时不应新建 cached_tokens 别名: %v", got)
	}
}

// TestNormalizeUsageCacheAliasesPreservesInputDetails Responses API 形态的
// input_tokens_details：上游已下发时同步镜像，未下发时不臆造（Chat-only 客户端
// 不该凭空多出一个 Responses 专属字段）。
func TestNormalizeUsageCacheAliasesPreservesInputDetails(t *testing.T) {
	withDetails := map[string]any{
		"prompt_tokens_details": map[string]any{"cached_tokens": 900.0},
		"input_tokens_details":  map[string]any{"cached_tokens": 0.0},
	}
	got := normalizeUsageCacheAliases(withDetails)
	if d := got["input_tokens_details"].(map[string]any); d["cached_tokens"] != 900.0 {
		t.Fatalf("input_tokens_details.cached_tokens=%v want 900（已下发则镜像）", d["cached_tokens"])
	}

	withoutDetails := map[string]any{
		"prompt_tokens_details": map[string]any{"cached_tokens": 900.0},
	}
	got = normalizeUsageCacheAliases(withoutDetails)
	if _, has := got["input_tokens_details"]; has {
		t.Fatalf("未下发 input_tokens_details 时不得臆造: %v", got)
	}
}

// TestNormalizeUsageCacheAliasesFlatFallback 上游只给扁平别名（无嵌套明细）时
// 取该值并镜像到其余位置——单一事实来源，避免下游读到不一致的两个数。
func TestNormalizeUsageCacheAliasesFlatFallback(t *testing.T) {
	usage := map[string]any{
		"cache_read_input_tokens": 1234.0,
		"cached_tokens":           0.0,
	}
	got := normalizeUsageCacheAliases(usage)
	if got["cached_tokens"] != 1234.0 || got["prompt_cache_hit_tokens"] != 1234.0 {
		t.Fatalf("扁平别名应互相同步: %v", got)
	}
	if d := got["prompt_tokens_details"].(map[string]any); d["cached_tokens"] != 1234.0 {
		t.Fatalf("嵌套明细应同步: %v", d)
	}
}

// TestAggregateNormalizesUsageCacheAliases 非流式聚合路径（Aggregate）同样归一：
// 客户端拿到的 usage 三个别名与嵌套明细一致。
func TestAggregateNormalizesUsageCacheAliases(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21041,\"completion_tokens\":8,\"total_tokens\":21049,\"cache_read_input_tokens\":0,\"cached_tokens\":0,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":20864}}}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	usage := resp["usage"].(map[string]any)
	for _, key := range []string{"cache_read_input_tokens", "cached_tokens", "prompt_cache_hit_tokens"} {
		if usage[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, usage[key])
		}
	}
}

// TestStreamNormalizesUsageCacheAliases 流式透传路径（Stream → normalizeFrame）
// 逐帧归一：末帧 usage 的三个别名同样与嵌套明细一致。
func TestStreamNormalizesUsageCacheAliases(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21041,\"completion_tokens\":8,\"total_tokens\":21049,\"cache_read_input_tokens\":0,\"cached_tokens\":0,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":20864}}}\n\n" +
		"data: [DONE]\n\n"

	recorder := httptest.NewRecorder()
	if err := Stream(recorder, strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}

	var usage map[string]any
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if raw, ok := frame["usage"].(map[string]any); ok && raw != nil {
			usage = raw
		}
	}
	if usage == nil {
		t.Fatal("stream response missing usage")
	}
	for _, key := range []string{"cache_read_input_tokens", "cached_tokens", "prompt_cache_hit_tokens"} {
		if usage[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, usage[key])
		}
	}
}

// TestNormalizeFrameUsageNullStaysNull usage 缺失 → null 的既有帧契约不因归一改变
// （非 map 形态的 usage 原样透传，不 panic、不臆造）。
func TestNormalizeFrameUsageNullStaysNull(t *testing.T) {
	out := normalizeFrame(map[string]any{"id": "x", "choices": []any{}})
	if v, has := out["usage"]; !has || v != nil {
		t.Fatalf("usage 缺失应写 null, got %#v has=%v", v, has)
	}
	// 非 map（脏数据）原样透传。
	out = normalizeFrame(map[string]any{"id": "x", "usage": "weird"})
	if out["usage"] != "weird" {
		t.Fatalf("非 map usage 应原样透传, got %#v", out["usage"])
	}
}
