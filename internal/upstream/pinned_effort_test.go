package upstream

import (
	"encoding/json"
	"testing"
)

// TestPinnedDeepSeekEffortPassthrough 写死条目（上游目录不返回、故不在任何能力缓存里）
// 的模型，请求档位必须**原样透传**，不能被改成默认档、也不能被降级。
//
// 这是对「请求 4.1 是 max 会传 max 吗」的直接回答，链路：
//  1. injectThinking：已有 reasoning_effort → 不覆盖（ensureDeepSeekEffort 直接返回）
//  2. normalizeReasoningEffort：模型不在 supportedEfforts 缓存里 → 透传
func TestPinnedDeepSeekEffortPassthrough(t *testing.T) {
	const model = "deepseek-v4.1-flash"

	// ① 客户端显式要 max：出站应仍是 max（外加被强开的 thinking.type=enabled）。
	body, _ := json.Marshal(map[string]any{
		"model":            model,
		"reasoning_effort": "max",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
	})
	// 能力缓存为空：模拟"上游目录里没有这个模型"的真实状态。
	out := PrepareBodyOptWithEffortsAndDefault(body, false, false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["reasoning_effort"] != "max" {
		t.Errorf("reasoning_effort=%v want max（未知模型不得降级/改档）", got["reasoning_effort"])
	}
	if th, ok := got["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Errorf("deepseek 前缀模型应被注入 thinking.type=enabled，got %v", got["thinking"])
	}

	// ② 客户端不带档位：回退硬编码 defaultDeepSeekEffort=high（不是写死条目的值——
	//    写死条目目前只用于展示，不参与出站链路）。
	body2, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	out2 := PrepareBodyOptWithEffortsAndDefault(body2, false, false, nil, nil)
	var got2 map[string]any
	if err := json.Unmarshal(out2, &got2); err != nil {
		t.Fatal(err)
	}
	if got2["reasoning_effort"] != defaultDeepSeekEffort {
		t.Errorf("缺档应补 %q，got %v", defaultDeepSeekEffort, got2["reasoning_effort"])
	}
	if defaultDeepSeekEffort != "high" {
		t.Errorf("硬编码默认档变了（%q）——面板写死的 default_effort 需同步", defaultDeepSeekEffort)
	}

	// ③ 客户端显式关思考：删掉 effort，且不注入 enabled。
	body3, _ := json.Marshal(map[string]any{
		"model":            model,
		"reasoning_effort": "max",
		"thinking":         map[string]any{"type": "disabled"},
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
	})
	out3 := PrepareBodyOptWithEffortsAndDefault(body3, false, false, nil, nil)
	var got3 map[string]any
	if err := json.Unmarshal(out3, &got3); err != nil {
		t.Fatal(err)
	}
	if _, has := got3["reasoning_effort"]; has {
		t.Errorf("thinking.type=disabled 应删除 reasoning_effort，got %v", got3["reasoning_effort"])
	}
	if th, ok := got3["thinking"].(map[string]any); !ok || th["type"] != "disabled" {
		t.Errorf("显式 disabled 应被尊重，got %v", got3["thinking"])
	}

	// ④ 反证：**缓存里有的**模型才会被降级。构造一个只支持 low/high 的模型，请求 max → 落到 high。
	supported := map[string][]string{"some-model": {"low", "high"}}
	body4, _ := json.Marshal(map[string]any{
		"model":            "some-model",
		"reasoning_effort": "max",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
	})
	out4 := PrepareBodyOptWithEffortsAndDefault(body4, false, false, supported, nil)
	var got4 map[string]any
	if err := json.Unmarshal(out4, &got4); err != nil {
		t.Fatal(err)
	}
	if got4["reasoning_effort"] != "high" {
		t.Errorf("已知模型 max→high 降级应生效，got %v（说明降级机制本身是活的）", got4["reasoning_effort"])
	}
}

// TestDeepSeekThinkingForcedOn deepseek 前缀模型"不传 thinking 就被强制开思考"，
// 且唯一的"关"入口是显式 thinking.type=disabled —— reasoning_effort:"off" 关不掉
// （injectThinking 只看 thinking.type，不看 effort；effort:"off" 只影响降级判定）。
//
// 非 deepseek 模型零改动（对照）。
func TestDeepSeekThinkingForcedOn(t *testing.T) {
	run := func(model string, extra map[string]any) map[string]any {
		t.Helper()
		obj := map[string]any{
			"model":    model,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		for k, v := range extra {
			obj[k] = v
		}
		raw, _ := json.Marshal(obj)
		out := PrepareBodyOptWithEffortsAndDefault(raw, false, false, nil, nil)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// thinkingType 读回出站 thinking.type（无该键返回 ""）。
	thinkingType := func(m map[string]any) string {
		th, ok := m["thinking"].(map[string]any)
		if !ok {
			return ""
		}
		s, _ := th["type"].(string)
		return s
	}

	// ① deepseek 裸请求（不传 thinking）→ 被强制开，并补上默认档。
	got := run("deepseek-v4.1-flash", nil)
	if thinkingType(got) != "enabled" {
		t.Errorf("deepseek 不传 thinking 应被强制 enabled，got %q", thinkingType(got))
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("应补默认档 high，got %v", got["reasoning_effort"])
	}

	// ② 只传 effort:"off" 关不掉——thinking 仍被打开。
	got = run("deepseek-v4.1-flash", map[string]any{"reasoning_effort": "off"})
	if thinkingType(got) != "enabled" {
		t.Errorf("effort=off 不应关掉思考，got thinking.type=%q", thinkingType(got))
	}
	if got["reasoning_effort"] != "off" {
		t.Errorf("显式档位不得被改写，got %v", got["reasoning_effort"])
	}

	// ③ 唯一关入口：显式 thinking.type=disabled → 保留 disabled 并删掉 effort。
	got = run("deepseek-v4.1-flash", map[string]any{
		"thinking": map[string]any{"type": "disabled"}, "reasoning_effort": "high",
	})
	if thinkingType(got) != "disabled" {
		t.Errorf("显式 disabled 应被尊重，got %q", thinkingType(got))
	}
	if _, has := got["reasoning_effort"]; has {
		t.Errorf("disabled 应删除 reasoning_effort，got %v", got["reasoning_effort"])
	}

	// ④ thinking:{} （type 空）等同"没传" → 仍被强制开。
	got = run("deepseek-v4.1-flash", map[string]any{"thinking": map[string]any{}})
	if thinkingType(got) != "enabled" {
		t.Errorf("thinking.type 为空应被强制 enabled，got %q", thinkingType(got))
	}

	// ⑤ 对照：非 deepseek 模型零改动（不注入 thinking、不补 effort）。
	got = run("glm-5.3", nil)
	if _, has := got["thinking"]; has {
		t.Errorf("非 deepseek 不应注入 thinking，got %v", got["thinking"])
	}
	if _, has := got["reasoning_effort"]; has {
		t.Errorf("非 deepseek 不应补 reasoning_effort，got %v", got["reasoning_effort"])
	}
}
