package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// payload_max_completion_tokens_test.go max_completion_tokens → max_tokens 翻译
// （对齐上游 sliver edb9e97 / PR #116，Closes #117）。
//
// 背景：OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新别名
// （o-series 起引入），DeepSeek Harness 等新客户端只发别名；上游只认 max_tokens，
// 别名被忽略后**静默**回落默认输出上限（实测 32000）——用户设 128000 实际只拿到
// 32000 且无任何报错，属最难排查的静默降级。
//
// 用例：别名翻译 / 显式优先 / 边界（0/null/负数/浮尾/非数值）不翻译 / 无别名零改动 /
// int 形态防御性兼容 / 不分域 / 与既有管线顺序（stream 仍强制、stream_options 仍注入）/
// 端到端 wire body。

// TestPrepareBodyConvertsMaxCompletionTokens（上游 PR #116 用例 1）：
// 只发别名 128000 → max_tokens=128000，别名删除。
func TestPrepareBodyConvertsMaxCompletionTokens(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"deepseek-v4.1-flash","messages":[],"max_completion_tokens":128000}`), false, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	if got["max_tokens"] != float64(128000) {
		t.Fatalf("max_tokens=%v (%T), want 128000", got["max_tokens"], got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Fatal("alias must be deleted")
	}
}

// TestPrepareBodyPrefersMaxTokens（上游 PR #116 用例 2）：
// 显式 max_tokens 与别名并存 → 取显式值（64000），别名删除，不被别名覆盖。
func TestPrepareBodyPrefersMaxTokens(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"messages":[],"max_tokens":64000,"max_completion_tokens":128000}`), false, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens=%v, want 64000 (explicit wins)", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Fatal("alias must be deleted")
	}
}

// TestMaxCompletionTokensExplicitZeroKept 显式 max_tokens=0 与别名并存：
// 显式字段原样保留（0 语义 = 上游默认），不覆盖成别名值，别名删除。
func TestMaxCompletionTokensExplicitZeroKept(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"messages":[],"max_tokens":0,"max_completion_tokens":128000}`), false, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got["max_tokens"]; !ok || v != float64(0) {
		t.Fatalf("explicit max_tokens=0 must be preserved verbatim, got %v ok=%v", v, ok)
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Error("alias must be deleted")
	}
}

// TestMaxCompletionTokensNonPositiveNotTranslated 边界：
// 0/null/负数/浮点尾巴/非数值别名一律不翻译（0/null 语义是「未设置」，走上游默认；
// 把 0 或负数翻进 max_tokens 等于把「未设置」变成「限制为 0」，是反向风险），
// 但别名仍一律删除。
func TestMaxCompletionTokensNonPositiveNotTranslated(t *testing.T) {
	cases := []struct {
		name      string
		aliasJSON string
	}{
		{"zero", `"max_completion_tokens":0`},
		{"null", `"max_completion_tokens":null`},
		{"negative", `"max_completion_tokens":-1`},
		{"float tail", `"max_completion_tokens":1.5`},          // 非整数：不翻译（不把小数尾巴搬进 max_tokens）
		{"non-numeric", `"max_completion_tokens":"abc"`},       // 字符串畸形：不翻译（上游 11101 自会报）
		{"numeric string", `"max_completion_tokens":"128000"`}, // 数值字符串同样不翻译（不做类型强转）
		{"bool", `"max_completion_tokens":true`},
		{"array", `"max_completion_tokens":[128000]`},
	}
	for _, c := range cases {
		src := []byte(`{"messages":[],` + c.aliasJSON + `}`)
		out := PrepareBodyOptWithEfforts(src, false, nil)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if v, ok := got["max_tokens"]; ok {
			t.Errorf("%s: max_tokens must NOT be set from invalid alias, got %v", c.name, v)
		}
		if _, ok := got["max_completion_tokens"]; ok {
			t.Errorf("%s: alias must be deleted", c.name)
		}
	}
}

// TestMaxCompletionTokensNoAliasZeroChange 无别名 → 零改动：
// 不产生 max_tokens，其余键集合不变（除管线本就注入的 stream / stream_options）。
func TestMaxCompletionTokensNoAliasZeroChange(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[],"temperature":0.7}`), false, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["max_tokens"]; ok {
		t.Error("max_tokens must not appear when alias absent")
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Error("alias must not appear")
	}
	wantKeys := map[string]bool{"model": true, "messages": true, "temperature": true, "stream": true, "stream_options": true}
	for k := range got {
		if !wantKeys[k] {
			t.Errorf("unexpected key %q added (keys=%v)", k, got)
		}
	}
	if len(got) != len(wantKeys) {
		t.Errorf("key set changed: got %d keys %v", len(got), got)
	}
}

// TestTranslateMaxCompletionTokensIntForms 手构造 map 的 int/int64 形态
// （非 JSON 解码路径）防御性兼容：正数翻译，非正不翻译；别名一律删。
func TestTranslateMaxCompletionTokensIntForms(t *testing.T) {
	obj := map[string]any{"max_completion_tokens": int(42)}
	translateMaxCompletionTokens(obj)
	if v, ok := obj["max_tokens"].(int64); !ok || v != 42 {
		t.Errorf("int alias: max_tokens=%v (%T), want int64 42", obj["max_tokens"], obj["max_tokens"])
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Error("int alias: alias must be deleted")
	}
	obj = map[string]any{"max_completion_tokens": int64(-1)}
	translateMaxCompletionTokens(obj)
	if v, ok := obj["max_tokens"]; ok {
		t.Errorf("negative int64 must not translate, got %v", v)
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Error("negative int64: alias must be deleted")
	}
}

// TestTranslateMaxCompletionTokensNoAlias 直接调函数（无别名）：
// 零副作用——不新增任何键。
func TestTranslateMaxCompletionTokensNoAlias(t *testing.T) {
	obj := map[string]any{"model": "glm-5.2"}
	translateMaxCompletionTokens(obj)
	if len(obj) != 1 {
		t.Errorf("no-alias call must be a no-op, got %v", obj)
	}
}

// TestMaxCompletionTokensRealmAgnostic 翻译不分域：
// CN /v2 与 global /console 是同一套 API 的两次部署，两域上游都只认 max_tokens。
func TestMaxCompletionTokensRealmAgnostic(t *testing.T) {
	for _, realm := range []string{"", "cn", "global"} {
		out := PrepareBodyOptRealm([]byte(`{"messages":[],"max_completion_tokens":128000}`), realm, false, false, nil, nil)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("realm %q: unmarshal: %v", realm, err)
		}
		if got["max_tokens"] != float64(128000) {
			t.Errorf("realm %q: max_tokens=%v, want 128000", realm, got["max_tokens"])
		}
		if _, ok := got["max_completion_tokens"]; ok {
			t.Errorf("realm %q: alias must be deleted", realm)
		}
	}
}

// TestMaxCompletionTokensPipelineOrder 与既有管线的顺序：
// 翻译后 stream 仍被强制、stream_options 仍被注入（翻译不打断管线）。
func TestMaxCompletionTokensPipelineOrder(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[],"stream":false,"max_completion_tokens":128000}`), false, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["stream"] != true {
		t.Errorf("stream not forced: %v", got["stream"])
	}
	so, ok := got["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("stream_options not injected: %v", got["stream_options"])
	}
	if got["max_tokens"] != float64(128000) {
		t.Errorf("max_tokens=%v, want 128000", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Error("alias must be deleted")
	}
}

// TestChatStreamWireBodyMaxCompletionTokens 端到端（参考 sanitize_test.go 的
// TestChatStreamWireBodySanitized 写法）：客户端只发别名时，假上游收到的 wire body
// 里必须是 max_tokens（别名消失、stream 仍强制）——这是「静默回落默认上限」修复的
// 出站边界断言，单测直调管线不足以覆盖 prepareBody 链路。
func TestChatStreamWireBodyMaxCompletionTokens(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":128000}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	if len(gotBody) == 0 {
		t.Fatal("upstream received empty body")
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v (body=%s)", err, gotBody)
	}
	if obj["max_tokens"] != float64(128000) {
		t.Errorf("wire body max_tokens=%v, want 128000 (alias must be translated)", obj["max_tokens"])
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Error("wire body still carries alias")
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
}
