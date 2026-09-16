package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReasoningSnapshot 诊断必须把"客户端用了哪种字段形态"和"模型在不在能力缓存里"
// 都摊出来——这两件事是判断"四档没区别"的唯一依据。
func TestReasoningSnapshot(t *testing.T) {
	efforts := map[string][]string{"glm-5.3": {"low", "high", "max"}}
	defaults := map[string]string{"glm-5.3": "high"}
	body := func(m map[string]any) []byte {
		b, _ := json.Marshal(m)
		return b
	}

	// ① 客户端完全没发思考字段（deepseek 会被网关强制开 → 四档当然一样）
	s := reasoningSnapshot(body(map[string]any{
		"model": "deepseek-v4.1-flash", "messages": []any{},
	}), efforts, defaults)
	if !strings.Contains(s, "thinking.type=\"\"") {
		t.Errorf("应显示 thinking.type 为空，got %s", s)
	}
	if strings.Contains(s, "reasoning_effort=") {
		t.Errorf("未携带 effort 时不应输出该字段，got %s", s)
	}
	if !strings.Contains(s, "cache_known=false") {
		t.Errorf("deepseek 不在缓存里应显示 cache_known=false，got %s", s)
	}

	// ② 扁平 snake（多数 OpenAI 兼容客户端）
	s = reasoningSnapshot(body(map[string]any{
		"model": "glm-5.3", "reasoning_effort": "max",
	}), efforts, defaults)
	if !strings.Contains(s, "reasoning_effort=max") || !strings.Contains(s, "cache_known=true") {
		t.Errorf("got %s", s)
	}
	if !strings.Contains(s, "cache_supported=[low high max]") {
		t.Errorf("应连带打出支持档位，got %s", s)
	}

	// ③ camel
	s = reasoningSnapshot(body(map[string]any{
		"model": "glm-5.3", "reasoningEffort": "low",
	}), efforts, defaults)
	if !strings.Contains(s, "reasoningEffort=low") {
		t.Errorf("got %s", s)
	}

	// ④ 嵌套 reasoning.effort（官方 codebuddy.js 形态）+ reasoning.summary
	s = reasoningSnapshot(body(map[string]any{
		"model":     "glm-5.3",
		"reasoning": map[string]any{"effort": "high", "summary": true},
		"thinking":  map[string]any{"type": "enabled"},
	}), efforts, defaults)
	if !strings.Contains(s, `reasoning.effort="high"`) || !strings.Contains(s, "reasoning.summary=true") {
		t.Errorf("嵌套形态必须被识别，got %s", s)
	}
	if !strings.Contains(s, `thinking.type="enabled"`) {
		t.Errorf("got %s", s)
	}

	// ⑤ 非法 body → 空串（调用方不打印，不 panic）
	if s := reasoningSnapshot([]byte("{not json"), efforts, defaults); s != "" {
		t.Errorf("解析失败应返回空串，got %q", s)
	}
	if s := reasoningSnapshot(nil, efforts, defaults); s != "" {
		t.Errorf("空 body 应返回空串，got %q", s)
	}
}

// TestReasoningSnapshotNoContentLeak 诊断串里绝不能出现消息内容。
func TestReasoningSnapshotNoContentLeak(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "SECRET-CANARY-STRING"}},
	})
	if s := reasoningSnapshot(b, nil, nil); strings.Contains(s, "SECRET-CANARY-STRING") {
		t.Fatalf("诊断泄漏了消息内容：%s", s)
	}
}
