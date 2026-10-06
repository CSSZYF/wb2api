package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareToolContentParts(t *testing.T) {
	const src = `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"check"},
		{"role":"assistant","content":[{"type":"text","text":"  11128 111-28\n"},{"type":"reasoning","text":"reason"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"query":"  11128 111-28\n","count":2}},{"type":"tool_use","id":"call_2","name":"lookup","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"  found\n"},{"type":"text","text":"keep going"},{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"second"}]}]}]}`
	for _, realm := range []string{"cn", "global"} {
		t.Run(realm, func(t *testing.T) {
			out := PrepareBodyOptRealm([]byte(src), realm, false, false, nil, nil)
			msgs := msgsOf(t, out)
			if len(msgs) != 5 {
				t.Fatalf("lost messages: %s", out)
			}
			a := msgAt(t, out, 1)
			calls, _ := a["tool_calls"].([]any)
			if len(calls) != 2 || a["reasoning_content"] != "reason" {
				t.Fatalf("calls or reasoning lost: %s", out)
			}
			fn := calls[0].(map[string]any)["function"].(map[string]any)
			var args map[string]any
			if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
				t.Fatal(err)
			}
			if args["query"] != "  11128 111-28\n" || args["count"] != float64(2) {
				t.Fatalf("arguments changed: %#v", args)
			}
			if msgAt(t, out, 2)["content"] != "  found\n" || msgAt(t, out, 2)["tool_call_id"] != "call_1" || msgAt(t, out, 3)["tool_call_id"] != "call_2" {
				t.Fatalf("tool results lost/reordered: %s", out)
			}
			if msgAt(t, out, 4)["role"] != "user" || strings.Contains(string(out), `"type":"tool_use"`) || strings.Contains(string(out), `"type":"tool_result"`) {
				t.Fatalf("untranslated blocks: %s", out)
			}
		})
	}
}

func TestToolContentPartsPreserveAndDedupe(t *testing.T) {
	for _, src := range []string{
		`{"messages":[{"role":"user","content":[{"type":"text","text":"11128"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"f","input":{}}]}]}`,
		`{"messages":[{"role":"system","content":[{"type":"tool_use","id":"x","name":"f","input":{}}]}]}`,
		`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"x","content":"ok"}]}`,
	} {
		var obj, before map[string]any
		json.Unmarshal([]byte(src), &obj)
		json.Unmarshal([]byte(src), &before)
		normalizeToolContentParts(obj)
		if !reflect.DeepEqual(obj, before) {
			t.Fatalf("unrelated/invalid content changed: %#v", obj)
		}
	}
	var obj map[string]any
	json.Unmarshal([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{\"b\":2, \"a\":1}"}}],"content":[{"type":"tool_use","id":"x","name":"f","input":{"a":1,"b":2}}]}]}`), &obj)
	normalizeToolContentParts(obj)
	m := obj["messages"].([]any)[0].(map[string]any)
	if len(m["tool_calls"].([]any)) != 1 || m["content"] != "" {
		t.Fatalf("duplicate representation became two calls: %#v", m)
	}
	first, _ := json.Marshal(obj)
	normalizeToolContentParts(obj)
	second, _ := json.Marshal(obj)
	if string(first) != string(second) {
		t.Fatalf("conversion is not idempotent: %s -> %s", first, second)
	}
}

func TestWAF503Classification(t *testing.T) {
	const html = `<!DOCTYPE html><html><head><title>WAF Block Page</title></head><body>Tencent Cloud WAF Access blocked Request UUID: 0a462856</body></html>`
	for _, tc := range []struct {
		status int
		body   string
		want   ErrKind
	}{
		{503, html, ErrWafBlock},
		{403, html, ErrWafBlock},
		{503, `<html><title>Service Unavailable</title></html>`, ErrServer},
		{503, `{"code":500,"msg":"WAF Block Page"}`, ErrServer},
		{503, "", ErrServer},
		{400, `{"code":11101,"msg":"Parse message failed: unsupported content type at index 1: tool_use"}`, ErrBadParams},
	} {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d, %q)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}
