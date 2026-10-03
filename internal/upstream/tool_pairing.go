// tool_pairing.go 出站请求体的孤儿 tool_call↔tool 配对清理 + tool 结果块重排
// （吸收参考仓库 sse.ts:91-123 resolveToolPairing 语义，适配网关的 OpenAI wire 消息形态）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每一个 tool_call id
// 都必须有对应的一条 role:tool 结果消息；反之 role:tool 消息也必须有对应的前置
// tool_call。缺任一侧，上游都会以 HTTP 400 拒绝整个请求。
//
// 工具执行失败时（参数非法、超时、工具不存在……）客户端会把 assistant 的 tool_calls
// 持久化进会话历史，却写不回结果消息。这条坏历史随后被每次请求原样重放——上游对之后
// 每一条用户消息都返回 400，整条会话报废。网关是最后一道防线：发出请求前剔除无法配对
// 的条目让会话自愈，宁可丢一轮工具上下文，也好过整条会话死亡。
package upstream

import "fmt"

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool 消息
// 挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// 背景：Codex 的 image_resize_notice 特性会把 <image_resize_notice> 作为一条
// developer/system 消息插在 tool 输出后面。并行调用时它插在两份 tool 结果中间：
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <image_resize_notice>   <- 插在中间
//	tool c01
//
// OpenAI 兼容协议要求 tool 结果紧跟 assistant，中间插任何消息都算配对断裂，上游判
// 11148（tool_call_sequence_broken）并顶死整条会话。这里只调顺序、不改内容：
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 结果顺序保持不变（同批 tool_call 的原相对顺序 = 结果顺序），不引入新的顺序敏感
// 问题。无插入消息时零改动零分配（返回原 slice）。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序。
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交由 cleanupOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：一旦被收进
			// between，它永远不再被外层循环当作组头处理，它自己那批结果也就永远得不
			// 到重排（真实会话 msg[181] 正是这样漏掉的）。必须 break 交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// 同批结果尚未收齐时，中间消息视为插入物，暂存待后移。
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// dedupeToolCallIDs 出站前把重复的 tool_call id 唯一化（#81 兜底加固）。
//
// 背景（#81 的最终结论，作者两度更正）：会话历史里的 tool_call id 本应唯一，但
// **客户端在发送前把 id 截断到 40 字符**——库里 `toolu_call-…-0` / `-1` / `-2`
// 三条唯一 49 字符 id，截断后变成同一个 40 字符 id。上游按唯一性/序列校验 →
// HTTP 400 / code 11148（tool_call_sequence_broken）；单号池时重试全败 →
// 客户端收 503 自动重试 5 次 → **会话彻底无法继续**。
//
// 根因在客户端截断，网关拦不住；本函数是**兜底加固**：按消息顺序遍历，同一 id
// 在**同一侧**第 2..n 次出现时改名为 `<id>_d<n>`，调用侧（assistant.tool_calls[].id）
// 与结果侧（role:tool 的 tool_call_id）各自按出现序对齐。纯改名——不动顺序、
// 不动内容、不删任何消息。
//
// 为什么两侧各自计数而非共用一个计数器：正常载荷里一个 id 天然出现两次（一次调用
// + 一次结果），共用计数器会把所有正常配对的结果都改成 `_d2`；分侧计数才能做到
// 「只对真重复动刀」，正常载荷零改动。
//
// 与下游两步的衔接（必须同步覆盖两处口径，否则改名反而制造孤儿）：
//   - repackToolResultBlocks 的 want map；
//   - cleanupOrphanToolCalls 的 callIDs / resultIDs / keepCalls 集合。
//
// 两者都从改写后的消息读取，本函数**同时**改写调用侧 id 与结果侧 tool_call_id，
// 两处看到的是同一套新 id。故本函数必须排在两步之前（先唯一化身份，再重排，再剪枝）。
//
// 幂等：对已唯一化的载荷零改动；改名后缀避开载荷里已存在的字面 id（见 dedupName）。
func dedupeToolCallIDs(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	// 第一遍：统计各 id 在两侧的出现次数，并收集载荷里全部字面 id（用于避碰——
	// 若 `x_d2` 已是另一条调用的真 id，`x` 的第 2 次出现就不得改成 `x_d2`，
	// 否则刚消掉的重复又撞回来）。
	callCount := map[string]int{}
	resultCount := map[string]int{}
	taken := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, _ := msg["tool_call_id"].(string); id != "" {
				resultCount[id]++
				taken[id] = true
				hasTraffic = true
			}
		case "assistant":
			tcs, ok := msg["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				if id, _ := tc["id"].(string); id != "" {
					callCount[id]++
					taken[id] = true
					hasTraffic = true
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	dup := false
	for _, n := range callCount {
		if n > 1 {
			dup = true
			break
		}
	}
	if !dup {
		for _, n := range resultCount {
			if n > 1 {
				dup = true
				break
			}
		}
	}
	if !dup {
		return messages, false // 无重复：零改动，原 slice 原样返回
	}
	// 第二遍：按消息顺序改写，两侧各持独立出现计数。同一 (原 id, 出现序号) 的别名
	// 必须两侧一致——别名经 alias 记忆表统一生成，两侧只查表不各自造名，否则调用侧
	// 拿到 `_d2`、结果侧拿到 `_d3`，配对反而被拆散（改名必须双侧同口径）。
	seenCalls := map[string]int{}
	seenResults := map[string]int{}
	alias := map[string]string{} // 键：原 id + "#" + 出现序号
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			if id == "" {
				continue
			}
			seenResults[id]++
			if seenResults[id] == 1 {
				continue // 首次出现保持原名
			}
			msg["tool_call_id"] = aliasFor(id, seenResults[id], alias, taken)
			changed = true
		case "assistant":
			tcs, ok := msg["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				id, _ := tc["id"].(string)
				if id == "" {
					continue
				}
				seenCalls[id]++
				if seenCalls[id] == 1 {
					continue // 首次出现保持原名
				}
				tc["id"] = aliasFor(id, seenCalls[id], alias, taken)
				changed = true
			}
		}
	}
	if !changed {
		return messages, false
	}
	return messages, true
}

// aliasFor 返回原 id 第 n 次出现（n>=2）的别名，同一 (id, n) 恒返回同一个名字
// （两侧共用，见 dedupeToolCallIDs 第二遍）。名字优先 `<id>_d<n>`，被载荷里既有
// 字面 id 或已分配别名占用则递增后缀。分配即登记，保证同轮内不撞名。
func aliasFor(id string, n int, alias map[string]string, taken map[string]bool) string {
	key := fmt.Sprintf("%s#%d", id, n)
	if name, ok := alias[key]; ok {
		return name
	}
	name := ""
	for k := n; ; k++ {
		cand := fmt.Sprintf("%s_d%d", id, k)
		if !taken[cand] {
			name = cand
			break
		}
	}
	alias[key] = name
	taken[name] = true
	return name
}

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，独立于
// deepseek-only 的 sanitize 开关）。语义对齐参考仓库 resolveToolPairing：
//
//   - 收集全线 role:tool 消息的 tool_call_id（结果集）与 assistant.tool_calls[].id（调用集）；
//   - 一批 assistant.tool_calls 按 keepCalls 对称裁剪：只留有结果配对的调用（部分保留
//     不会留下无结果的 tool_call），过滤后为空才删掉整个 tool_calls 键；
//   - role:tool 只在对应 tool_call 被保留时才保留，否则删除整条消息；
//   - 无任何工具流量 → 原 slice 原样返回，changed=false（零分配零改动）。
//
// 这是「让请求通过」的安全网：只要存在合法配对就整段保留这些字段，绝不吞掉正确配对。
// 返回清理后的 slice（无改动时等于原 slice，勿依赖其是否新分配）及是否发生删除。
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	// keepCalls：调用 id 是否双侧齐全（调用存在且结果存在）。重复 id 与乱序均按集合处理。
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：按 keepCalls 对称裁剪——只留有结果的调用，过滤后为空则删键。
	//
	// 历史实现是「批内每个 id 都齐才整批保留，否则删掉整个 tool_calls 键」。那会留下
	// 无主结果：批 [c1,c2] 只回了 c1 时，调用侧整批被删，而 tool{c1} 仍按 id 命中
	// keepCalls 得以保留 —— 出站载荷于是变成「无 tool_calls 的 assistant + 孤儿 tool」，
	// 上游判 11148（tool calls and tool results do not match）并顶死整条会话。
	// 现在两侧共用同一份 keepCalls 按 id 对称裁剪（与 2) 的删除侧同口径），
	// 任何输入都不会再产生半截配对。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); keepCalls[id] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(tcs) {
			continue // 整批齐全：零改动
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) role:tool 结果：只有对应 tool_call 被保留才保留；孤儿结果整条删除。
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}
