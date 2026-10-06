package upstream

import "encoding/json"

// normalizeToolContentParts translates Anthropic-style history blocks carried by
// OpenAI-compatible clients. Run before pairing cleanup so results are not lost.
// Text and tool arguments are moved structurally, never rewritten.
func normalizeToolContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, msg)
			continue
		}
		switch msg["role"] {
		case "assistant":
			calls, _ := msg["tool_calls"].([]any)
			kept := make([]any, 0, len(parts))
			changed := false
			for _, rawPart := range parts {
				p, _ := rawPart.(map[string]any)
				id, _ := p["id"].(string)
				name, _ := p["name"].(string)
				input, validInput := p["input"].(map[string]any)
				if p["type"] != "tool_use" || id == "" || name == "" || !validInput {
					kept = append(kept, rawPart)
					continue
				}
				args, err := json.Marshal(input)
				if err != nil {
					kept = append(kept, rawPart)
					continue
				}
				// Some adapters supply both representations of the same call.
				duplicate := false
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					var existing map[string]any
					argString, _ := fn["arguments"].(string)
					if call["id"] == id && fn["name"] == name && json.Unmarshal([]byte(argString), &existing) == nil {
						canonical, _ := json.Marshal(existing)
						duplicate = string(canonical) == string(args)
						if duplicate {
							break
						}
					}
				}
				if !duplicate {
					calls = append(calls, map[string]any{"id": id, "type": "function",
						"function": map[string]any{"name": name, "arguments": string(args)}})
				}
				changed = true
			}
			if changed {
				msg["tool_calls"] = calls
				msg["content"] = toolPartsContent(kept)
			}
			out = append(out, msg)
		case "user":
			kept := make([]any, 0, len(parts))
			changed := false
			flush := func() {
				if len(kept) == 0 {
					return
				}
				copyMsg := make(map[string]any, len(msg))
				for k, v := range msg {
					copyMsg[k] = v
				}
				copyMsg["content"] = kept
				out = append(out, copyMsg)
				kept = nil
			}
			for _, rawPart := range parts {
				p, _ := rawPart.(map[string]any)
				id, _ := p["tool_use_id"].(string)
				if p["type"] != "tool_result" || id == "" {
					kept = append(kept, rawPart)
					continue
				}
				content := p["content"]
				if content == nil {
					content = ""
				}
				flush()
				out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
				changed = true
			}
			if changed {
				flush()
			} else {
				out = append(out, msg)
			}
		default:
			out = append(out, msg)
		}
	}
	obj["messages"] = out
}

func toolPartsContent(parts []any) any {
	if len(parts) == 0 {
		return ""
	}
	return parts
}
