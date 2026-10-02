package upstream

import (
	"reflect"
	"testing"
)

// TestDefaultPinnedModels 内置兜底条目的数值是硬编码的能力快照，改动必须是有意的。
func TestDefaultPinnedModels(t *testing.T) {
	if len(DefaultPinnedModels) == 0 {
		t.Fatal("默认写死名单不应为空")
	}
	var ds *PinnedModel
	for i := range DefaultPinnedModels {
		if DefaultPinnedModels[i].ID == "deepseek-v4.1-flash" {
			ds = &DefaultPinnedModels[i]
		}
	}
	if ds == nil {
		t.Fatal("默认写死名单应含 deepseek-v4.1-flash")
	}
	if ds.Name != "Deepseek-V4.1-Flash" {
		t.Errorf("name=%q", ds.Name)
	}
	// x0.00（2026-10-02 更正）：v3-CLI（唯一下发该 id 的端点）实测 x0.00；
	// 旧值 x0.03 是新加坡版 deepseek-v4.1-flash-sg 的价，当初跟进时抄混了。
	if ds.Credits != "x0.00" {
		t.Errorf("credits=%q want x0.00（实测 v3-CLI 原值；x0.03 是 -sg 变体的价）", ds.Credits)
	}
	if ds.ContextLength != 1000000 {
		t.Errorf("context_length=%d want 1000000（面板显示 1000K）", ds.ContextLength)
	}
	if ds.MaxOutputTokens != 128000 {
		t.Errorf("max_output_tokens=%d want 128000（面板显示 128K）", ds.MaxOutputTokens)
	}
	if ds.DefaultEffort != "high" {
		t.Errorf("default_effort=%q want high", ds.DefaultEffort)
	}
	if !ds.SupportsReasoning {
		t.Error("supports_reasoning 应为 true（面板显示「固定档 · 默认 high」）")
	}
	if len(ds.SupportedEfforts) != 0 {
		t.Errorf("supported_efforts=%v 应为空——空数组/缺键才是「固定档」语义", ds.SupportedEfforts)
	}
}

// TestResolvePinnedModels 键缺席用内置默认，显式空数组 = 不强行内置。
func TestResolvePinnedModels(t *testing.T) {
	if got := ResolvePinnedModels(nil); len(got) != len(DefaultPinnedModels) {
		t.Errorf("nil 应回落默认，got %d 项", len(got))
	}
	if got := ResolvePinnedModels([]PinnedModel{}); len(got) != 0 {
		t.Errorf("显式空数组应不内置任何模型，got %v", got)
	}
	custom := ResolvePinnedModels([]PinnedModel{{ID: "my-model"}, {ID: ""}})
	if len(custom) != 1 || custom[0].ID != "my-model" {
		t.Errorf("空 ID 应被丢弃，got %v", custom)
	}
}

// TestMergePinnedUpstreamWins 上游已返回同名模型时，写死条目必须让位。
func TestMergePinnedUpstreamWins(t *testing.T) {
	upstreamSaid := []ModelInfo{{ID: "deepseek-v4.1-flash", Credits: "x9.99", ContextWindow: 12345}}
	pinned := []PinnedModel{{ID: "deepseek-v4.1-flash", Credits: "x0.03", ContextLength: 1000000}}

	got := MergePinned(upstreamSaid, pinned)
	if len(got) != 1 {
		t.Fatalf("不应追加重复条目，got %d 项", len(got))
	}
	if got[0].Credits != "x9.99" || got[0].ContextWindow != 12345 {
		t.Fatalf("应保留上游数据，got %+v", got[0])
	}

	// 上游缺失 → 追加（保序，写死在末尾）
	got = MergePinned([]ModelInfo{{ID: "gpt-5.4"}}, pinned)
	if len(got) != 2 || got[0].ID != "gpt-5.4" || got[1].ID != "deepseek-v4.1-flash" {
		t.Fatalf("got %v", []string{got[0].ID, got[1].ID})
	}
	if got[1].Credits != "x0.03" || got[1].ContextWindow != 1000000 {
		t.Fatalf("追加条目应带写死的能力快照，got %+v", got[1])
	}

	// 写死名单为空 → 原样返回（不复制）
	in := []ModelInfo{{ID: "gpt-5.4"}}
	if got := MergePinned(in, nil); !reflect.DeepEqual(got, in) {
		t.Fatalf("空写死名单应原样返回，got %v", got)
	}
}

// TestFillMissingFromPinned 字段级兜底：上游给了条目但**字段缺失**时补写死快照，
// 上游给了的字段一律不动（方向恒为上游优先）。
//
// 场景（2026-10-02 实测）：deepseek-v4.1-flash 只在 v3-CLI 下发（企业端点没有），
// v3 探测半残（5min 负缓存 / schema 变更）时目录里仍有该 id 但 credits 为空——
// 面板显示 "—"，而写死快照里 x0.00 是已知真值。
func TestFillMissingFromPinned(t *testing.T) {
	pinned := []PinnedModel{{
		ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", Credits: "x0.00",
		ContextLength: 1000000, MaxOutputTokens: 128000, DefaultEffort: "high", SupportsReasoning: true,
	}}

	// 上游只给了裸 id（credits/窗口/档位全缺）→ 字段级补齐。
	in := []ModelInfo{{ID: "deepseek-v4.1-flash"}}
	got := FillMissingFromPinned(in, pinned)
	if got[0].Credits != "x0.00" {
		t.Errorf("credits=%q want x0.00（字段级兜底）", got[0].Credits)
	}
	if got[0].ContextWindow != 1000000 || got[0].MaxTokens != 128000 {
		t.Errorf("窗口/输出未补齐：%+v", got[0])
	}
	if got[0].DefaultEffort != "high" || !got[0].SupportsReasoning {
		t.Errorf("档位未补齐：%+v", got[0])
	}

	// 上游给了 credits/窗口 → 上游权威（写死快照让位，哪怕值不同）。
	in = []ModelInfo{{ID: "deepseek-v4.1-flash", Credits: "x0.09", ContextWindow: 500000, MaxTokens: 64000}}
	got = FillMissingFromPinned(in, pinned)
	if got[0].Credits != "x0.09" || got[0].ContextWindow != 500000 || got[0].MaxTokens != 64000 {
		t.Errorf("上游数据被写死快照覆盖：%+v", got[0])
	}
	// 能力字段 fill-only：上游给了 DefaultEffort 时不被写死值改写。
	in = []ModelInfo{{ID: "deepseek-v4.1-flash", DefaultEffort: "low"}}
	got = FillMissingFromPinned(in, pinned)
	if got[0].DefaultEffort != "low" {
		t.Errorf("上游 DefaultEffort 被覆盖：%q", got[0].DefaultEffort)
	}

	// 不在写死名单里的 id 不动；空名单原样返回。
	in = []ModelInfo{{ID: "other-model"}}
	if got := FillMissingFromPinned(in, pinned); got[0].Credits != "" || got[0].ContextWindow != 0 {
		t.Errorf("名单外条目被改写：%+v", got[0])
	}
	if got := FillMissingFromPinned(in, nil); !reflect.DeepEqual(got, in) {
		t.Errorf("空写死名单应原样返回，got %v", got)
	}
}

// TestPinnedEntryShape /v1/models 的条目形态：空 supported_efforts 不输出该键。
func TestPinnedEntryShape(t *testing.T) {
	p := PinnedModel{
		ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", Credits: "x0.03",
		ContextLength: 1000000, MaxOutputTokens: 128000,
		DefaultEffort: "high", SupportsReasoning: true,
	}
	e := p.Entry()
	if _, ok := e["supported_efforts"]; ok {
		t.Error("固定档不应输出 supported_efforts 键（前端据此渲染「固定档 · 默认 X」）")
	}
	if e["default_effort"] != "high" || e["credits"] != "x0.03" {
		t.Errorf("entry=%v", e)
	}
	if e["context_length"] != int64(1000000) || e["max_output_tokens"] != int64(128000) {
		t.Errorf("entry=%v", e)
	}
	if e["supports_reasoning"] != true {
		t.Errorf("entry=%v", e)
	}
	if _, ok := e["max_allowed_size"]; ok {
		t.Error("MaxAllowedSize=0 时不应输出该键")
	}
	// 可选字段：给了才输出
	p.MaxAllowedSize = 262144
	p.SupportedEfforts = []string{"low", "high"}
	e = p.Entry()
	if e["max_allowed_size"] != int64(262144) {
		t.Errorf("给了 MaxAllowedSize 就该输出，entry=%v", e)
	}
	if !reflect.DeepEqual(e["supported_efforts"], []string{"low", "high"}) {
		t.Errorf("entry=%v", e)
	}
}
