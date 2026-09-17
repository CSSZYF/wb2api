package upstream

import "testing"

// TestNormalizeModelName 上游部分国际版模型名带冗余厂商前缀，展示前必须剥掉。
func TestNormalizeModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"DeepSeek: DeepSeek V4.1 Flash", "DeepSeek V4.1 Flash"},
		{"deepseek: DeepSeek V4.1 Flash", "DeepSeek V4.1 Flash"}, // 厂商大小写不同也算
		{"DeepSeek:DeepSeek V4.1 Flash", "DeepSeek V4.1 Flash"},  // 冒号后无空格
		{"  GLM: GLM-5.3  ", "GLM-5.3"},
		{"Auto", "Auto"},                         // 无冒号 → 原样
		{"GPT 5.4", "GPT 5.4"},                   // 无冒号 → 原样
		{"GLM: GLMX-1", "GLM: GLMX-1"},           // 同前缀但不是同一个词 → 不剥
		{"DeepSeek: Qwen 3", "DeepSeek: Qwen 3"}, // 厂商不重复 → 不剥
		{"", ""},
		{": X", ": X"}, // 冒号在首 → 原样
		{"X:", "X:"},   // 冒号在尾 → 原样
	}
	for _, c := range cases {
		if got := normalizeModelName(c.in); got != c.want {
			t.Errorf("normalizeModelName(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestResolveHiddenModels 键缺席用内置默认（隐藏路由别名），显式空数组 = 全不隐藏。
func TestResolveHiddenModels(t *testing.T) {
	def := ResolveHiddenModels(nil)
	for _, id := range DefaultHiddenModels {
		if !def.Has(id) {
			t.Errorf("默认名单应含 %q", id)
		}
	}
	if !def.Has("Default-Model") {
		t.Error("匹配应大小写不敏感")
	}
	if def.Has("gpt-5.4") {
		t.Error("真实模型不应被默认隐藏")
	}

	empty := ResolveHiddenModels([]string{})
	if len(empty) != 0 || empty.Has("default-model") {
		t.Error("显式空数组应表示全部展示")
	}

	custom := ResolveHiddenModels([]string{"gemini-3.5-flash", " "})
	if !custom.Has("gemini-3.5-flash") {
		t.Error("自定义名单应生效")
	}
	if custom.Has("default-model") {
		t.Error("自定义名单应完全覆盖默认，不再隐藏 default-model")
	}
	if len(custom) != 1 {
		t.Errorf("空白项应被丢弃，实际 %d 项", len(custom))
	}
}

// TestHiddenSetFilters 两个投影用的过滤器都要保序、不误伤。
func TestHiddenSetFilters(t *testing.T) {
	h := ResolveHiddenModels(nil)
	infos := []ModelInfo{{ID: "gpt-5.4"}, {ID: "default-model"}, {ID: "glm-5.2"}}
	got := h.FilterInfo(infos)
	if len(got) != 2 || got[0].ID != "gpt-5.4" || got[1].ID != "glm-5.2" {
		t.Fatalf("FilterInfo=%v want [gpt-5.4 glm-5.2]", got)
	}
	names := h.FilterNames([]string{"default-model", "gpt-5.4", "deep-model", "kimi-k3"})
	if len(names) != 2 || names[0] != "gpt-5.4" || names[1] != "kimi-k3" {
		t.Fatalf("FilterNames=%v want [gpt-5.4 kimi-k3]", names)
	}
	// nil 集合 = 不隐藏（且不复制切片）
	var nilSet HiddenSet
	if nilSet.Has("default-model") {
		t.Fatal("nil 集合不应隐藏任何模型")
	}
	if got := nilSet.FilterNames(names); len(got) != len(names) {
		t.Fatalf("nil 集合应原样返回，got=%v", got)
	}
}
