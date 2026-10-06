// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101；
//     "none" 保留 tools 声明，见 normalizeToolChoice 的上游 4db4e91 依据）
//  3. max_completion_tokens 别名翻译为 max_tokens（上游只认后者，别名被静默忽略后
//     回落默认输出上限，见 translateMaxCompletionTokens）
//  4. image_url 归一化（上游只认 OpenAI 对象形态 {"url":...}，字符串形态会
//     400 code=11101，见 normalizeImageURL）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, false, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, false, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 完整管线：efforts 降级 + thinking.go 按
// defaultEfforts（模型声明默认档）补档。defaultEfforts 为 nil 时与旧行为一致
// （deepseek 缺档回退硬编码 high）。
//
// zeroWidth 控制零宽脱敏（见 zerowidth.go）——与 sanitize 是**两个独立开关**：
// sanitize 默认开（改写/删除已知指纹），零宽默认关（插入不可见字符，改动更隐蔽，
// 由使用者在面板上显式开启）。
//
// 本入口不带 realm，按包内约定（realmKey）视为 cn——它是 CN 现状路径的旧封装，
// 行为与改动前一致；需要区分 cn/global 的调用方（出站主路径 client.prepareBody）
// 用 PrepareBodyOptRealm 显式传 realm。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize, zeroWidth bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	return PrepareBodyOptRealm(src, "", sanitize, zeroWidth, efforts, defaultEfforts)
}

// PrepareBodyOptRealm 同 PrepareBodyOptWithEffortsAndDefault，但显式指定 realm（供
// efforts 缓存分桶等按域区分的处理使用；reasoning content-part 转换不分域，见
// reasoning_parts.go——实测 CN 与 global 上游都拒绝该 part 类型）。
//
// 为什么用新变体而不是给原函数加参数：原函数有 20+ 处调用点（大量测试直接构造
// 请求体），加参数等于全量改签名、把「realm 感知」扩散到与域无关的用例里；
// 新变体只改出站主路径一个调用点，其余调用点零改动。
func PrepareBodyOptRealm(src []byte, realm string, sanitize, zeroWidth bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	return PrepareBodyOptRealmHistory(src, realm, sanitize, zeroWidth, ReasoningHistoryFull, efforts, defaultEfforts)
}

// PrepareBodyOptRealmHistory 同 PrepareBodyOptRealm，但多一个出站历史推理文本裁剪档位
// （features.reasoning_history，见 reasoning_history.go 的三档语义与实测依据）。
// 空串/未知档位与 full 等价（trimReasoningHistory 入口即 return）——fail-safe 零回归。
//
// 为什么又是新变体而不是给 PrepareBodyOptRealm 加参数：同当年加 PrepareBodyOptRealm
// 的取舍（见上方注释）——既有入口有 20+ 处调用点（含大量直接构造请求体的测试），
// 加参数等于全量改签名；新变体只改出站主路径（client.prepareBody）一个调用点。
func PrepareBodyOptRealmHistory(src []byte, realm string, sanitize, zeroWidth bool, reasoningHistory string, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（上游 sliver edb9e97 吸收 PR #116，
	// Closes #117）：OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens
	// 是新别名（o-series 起引入），DeepSeek Harness 等新客户端只发别名；上游只认
	// max_tokens，别名被忽略后**静默**回落默认输出上限（实测 32000）——用户设
	// 128000 实际只拿到 32000 且无任何报错，属最难排查的静默降级。
	// 位置：与 stream 强制同属「顶层标量字段归一」，紧贴在一起便于审阅；纯字段搬运，
	// 不依赖 messages/model，与后续 stream_options 注入等步骤无顺序耦合。
	translateMaxCompletionTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeToolContentParts(obj)
	normalizeImageURL(obj)
	// tool 配对四步（见 tool_pairing.go）：唯一化 id → 合并背靠背调用 → 重排 → 清理孤儿。
	// 所有模型一律执行（独立于 deepseek-only 的 sanitize 开关）。这是「让请求通过」的
	// 安全网——不完整配对的 tool_calls/tool 结果会让上游对之后每条消息都返 400，必须
	// 先行剔除；插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	//
	// 顺序不能换（四步各有前置依赖）：
	//   1. dedupeToolCallIDs 最先（#81 兜底）：客户端发送前把 tool_call id 截断到 40
	//      字符，三条唯一 id 截成同一个 → 上游判 400/11148，单号池重试全败会话报废。
	//      唯一化同时改写调用侧 id 与结果侧 tool_call_id，故下游看到的都是新 id，
	//      配对不被破坏。**必须在剪枝之前**——剪枝依据的 id 才是唯一且自洽的。
	//   2. mergeAdjacentToolCalls 第二：把「背靠背的两条 assistant.tool_calls」合成
	//      一条（部分 agent 客户端回放并行调用的报文形状），是上游 deepseek 系
	//      tool_call_sequence_broken 的正面修复。**必须在 repack 之前**——先合并，
	//      repack 才看得到完整的一批调用。
	//   3/4. repack 挪后、cleanup 删孤儿。
	//
	// 为什么 dedupe 在 merge 之前：merge 会把两条 assistant 的 tool_calls 拼进一条，
	// 若两条各带同名 id，合并后重复就"焊死"在同一条消息里，dedupe 仍能处理但语义上
	// 更绕；先在原始形状上去重，后续每一步看到的 id 都是干净的。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = dedupeToolCallIDs(msgs)
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时四步都返回原 slice，这里回写等于零操作；任一步改名/合并/重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则前面步骤单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// reasoning content-part 转换（见 reasoning_parts.go）：ZCode 3.11.2 把思考
	// 内容作为 content 数组里的 {"type":"reasoning"} part 发出，**两个域都不认**
	// 该 part 类型（HTTP 400 code=11101 "unsupported content type at index 0:
	// reasoning"），改成顶层 reasoning_content 字符串后两域均 200。
	// （最初的 202c 只在 CN 域转换，假设 global 接受该 part；实测证伪：
	// global 同样 400。故无条件转换——结构搬移不改任何可见文本与语义，
	// 无需分域。）
	//
	// 位置理由（顺序敏感，三处）：
	//  1. 必须早于 backfillReasoningContent：backfill 第一遍只检测顶层 msg["reasoning"] /
	//     msg["reasoning_content"]，数组里的 reasoning part 它看不见——先提升，backfill
	//     才能按既有语义（任一 assistant 有痕迹 → 全部 assistant 补 reasoning_content）
	//     把整段历史补齐。
	//  2. 与 injectThinking 无耦合（一个按 realm、一个按模型名），放在它前面只是顺路；
	//     提升出的 reasoning_content 不参与 thinking/effort 注入判定（那些只看顶层
	//     thinking/reasoning_effort 字段）。
	//  3. 必须早于 sanitize：提升后的 reasoning_content 走 sanitizeMessages 的
	//     reasoning_content 分支净化，覆盖面与提升前 sanitizeContent 对 reasoning part
	//     的 text 字段净化一致（sanitizeContent 只认 part 的 text 键、不看 type）。
	promoteReasoningParts(obj)
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	modelName, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	// 出站历史推理文本裁剪（见 reasoning_history.go）：**必须晚于 sanitizeMessages**——
	// sanitize 会把纯指纹块的 rc 整段删除成空串（sanitize.go 记录的「已知残余」：
	// rc 净化后为空 → 租户 len>0 校验 400），裁剪步在其后即可把空串补成 " "，
	// 顺带修掉这条路径；反过来（先裁剪后净化）占位没有净化需求（无害），但被清空的
	// rc 会永远停在空串、无人补位（有害），且 keeper 会选中「净化前看着有文本」的
	// 指纹块消息、净化后只剩空串（既丢真原文又留空串，最坏组合）。
	// 与 zeroWidth 无耦合（后者只处理 system 消息的 content，见 zerowidth.go），
	// 放在它前面只是顺路。档位默认 full 时本步入口即 return（无任何 map 写入）。
	trimReasoningHistory(obj, reasoningHistory)
	// 零宽脱敏必须在 sanitize 之后：sanitize 依赖**整句子符串相等**来定位并改写模板句，
	// 若先插了零宽字符，句子中间多出 U+200B，字符串匹配随即失效、改写全部落空。
	// 反过来则不冲突：零宽按"独立词"匹配，改写后的新句子（…CLI tool for Claude.）里
	// 仍含 Claude / Anthropic 等词，照样被覆盖。
	if zeroWidth {
		if msgs, ok := obj["messages"].([]any); ok {
			ApplyZeroWidthMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游认的
// max_tokens（上游 sliver edb9e97 吸收 PR #116，Closes #117）。调用点在
// PrepareBodyOptRealm 管线里 stream 强制之后（与上游挂载位置一致：同一 payload 预处理
// 管线，见上游提交说明「任务书 prompt-too-long §3」）。
//
// 为什么必须翻译：OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新
// 字段；DeepSeek Harness 等新客户端只发别名。上游只认 max_tokens，别名被忽略后**静默**
// 回落默认输出上限（实测 32000）——用户设 128000 实际只拿到 32000，无任何报错。
//
// 规则（严格对齐上游边界，不放宽也不收紧）：
//   - 显式 max_tokens 已存在 → 别名只删不译（显式优先，不覆盖用户明确设置的值）；
//   - 别名值为正整数（v > 0 且无小数尾巴）→ 译为 max_tokens；
//   - 别名 0 / null / 负数 / 浮点尾巴 / 非数值 → 不翻译（0/null 语义是「未设置」，
//     走上游默认；把 0 或负数翻进 max_tokens 等于把「未设置」变成「限制为 0」，是
//     反向风险）；
//   - 别名一律删除（无论是否翻译成功）——否则上游可能对未知字段报错，且留着徒增
//     body 体积与排障噪音。
//
// 不分域：CN /v2 与 global /console 是同一套 API 的两次部署（见 context_catalog
// 文件头实测结论），global 域上游同样只认 max_tokens，故翻译对两域同口径执行。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（见上方注释）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 把数字解成 float64（整数去整后回写，避免 1.28e5 科学计数法/小数
	// 尾巴进上游 body）；int 家族分支是防御性兼容——手构造 map 的调用方（测试/内部）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法
// （吸收上游 d47219b）。
//
// OpenAI Chat Completions 规范用对象形态 {"url":"...","detail":"..."}，但部分客户端
// （以及 Responses → Chat 转换器）会发字符串形态 "data:..." / "https://..."。
// WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"——同一个 body 换任何账号都失败，
// 且客户端拿到的报错完全不指向"写法不对"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象（含 url/detail/mime_type 等键）
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值——让上游返回真实错误
// （编造 url 会把"客户端漏字段"变成"网关发了个不存在的图片"，错误方向完全错）。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system（"You are a helpful assistant."）。
// 仅对 global 请求调用（CN 现状不动；即使首条就是 system 也不重复注入）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//
// 上游依据：hub 4db4e91（fix(tools): keep the tools declaration when tool_choice
// is "none"）。**我们此前逐字复刻了上游这个 bug**——旧实现在 "none" 分支里用
// suppress() 把 tools/functions 一起删掉，而 payload_test.go 里 "none" 出现 0 次、
// 零测试覆盖，所以它一直没被暴露（连函数注释都把错误行为写成了规格）。
//
// 上游实测的故障模式（Agent 死循环）：tool_choice="none" 时删掉整个 tools 声明 →
// 模型拿不到函数签名、又没有结构化工具通道，却仍被要求完成任务，于是把「调用」
// 降级成 DSML / 伪 JSON 文本塞进 content（tool_calls 为空、finish_reason=stop）；
// Agent 客户端解析不到调用只能再追问一轮，模型每轮重复 "I'll do it"，上下文每轮
// +2 条消息、约 +500 token 线性膨胀。上游 usage.jsonl 实测（deepseek-v4.1-flash
// + Agent 客户端，用户手动断开）：outcome=client_aborted elapsed_ms=131027
// gen_ms=128262 usage_missing=true n_msgs=602——n_msgs 从 543 两两爬到 622，
// prompt_tokens 涨到 297k。
//
// 修法：保留工具声明，只让 tool_choice 字段本身表达「本轮不许调用工具」。
//
// 为什么保留 tools 是更优取舍：上游实测**并不真正遵守** tool_choice="none"——
// 保留 tools 后它仍可能返回 tool_calls。但两条路对比：删 tools 会让模型输出不可
// 解析的文本、Agent 原地空转；留 tools 则走正常 tool_calls 通道，客户端能正常
// 执行与回填——后者严格更好。确实需要禁止调用时，客户端不传 tools 字段即可。
//
// 上游只认字符串：tool_choice 发对象形态会 400 code=11101，所以 "none" 必须以
// 字符串透传（不得改写成 {"type":"none"}），对象形态则降级为字符串 "none"。
//
// 规则：
//   - "none" / {"type":"none"}（大小写不敏感、容忍前后空格）→ **保留**
//     tools/functions，tool_choice 写回字符串 "none"
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice（tools 原样保留）
func normalizeToolChoice(obj map[string]any) {
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			// 保留 tools/functions（上游 4db4e91）：只由本字段表达「本轮不许调用」。
			obj["tool_choice"] = "none"
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			// 同上：保留 tools 声明；上游只认字符串，对象形式必须降级成 "none"，
			// 否则 11101。
			obj["tool_choice"] = "none"
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			unescapePatternLiteralEscapes(fn["parameters"])
		}
		unescapePatternLiteralEscapes(tool["parameters"])
	}
}

// unescapePatternLiteralEscapes 递归改写 schema 树里 pattern 值与 patternProperties
// 键中的 `\_` → `_`（patternProperties 的键也是正则；map 键不可原地改，命中时重建
// 该层）。
func unescapePatternLiteralEscapes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	case []any:
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	}
}
