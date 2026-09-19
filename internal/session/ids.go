// ids.go 会话头族 ID 的解析与生成（issue #35 后台聚合）。
//
// 官方 CodeBuddy CLI 出站头族（X-Conversation-ID / X-Conversation-Request-ID /
// X-Request-ID / X-B3-*），后台按 X-Conversation-Request-ID（对话轮）聚合请求；
// 本文件提供 conversationId 提取、消息级 32 hex messageID、以及"同一会话键
// 稳定复用"的 conversationRequestID 惰性缓存，供 handler 轮转循环外生成、循环内
// 复用（换号/重试/降级全部同 ID → 后台不再碎片化）。
package session

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
)

// ResolveConversationID 从请求体提取会话头族的 conversationId（snake/camel 双形态，
// 复用 ExtractKey 的识别顺序：metadata 优先、snake 优先于 camel）。
// 与 ExtractKey 的差异：**只认 conversationId，绝不回落 user_id**——X-Conversation-ID
// 语义是"对话 ID"，user_id 回落会污染后台按对话聚合的判据。
// 缺失返回 ""（不伪造：透传客户端原值优先，客户端没给就不发）。
func ResolveConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	return strOrEmpty(obj["conversationId"])
}

// NewMessageID 生成消息级 ID：32 位 hex（UUID v4 去横线的长度形态），对齐官方
// X-Request-ID / X-Conversation-Message-ID。crypto/rand 失败（理论上不可能）时回落
// math/rand/v2 双 uint64 拼 32 hex——恒 32 hex、恒合法，可安全用作 B3 TraceId。
func NewMessageID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	// 熵源故障的极端兜底：仍保证 32 hex（fallbackID 不 panic、不空串）。
	return fmt.Sprintf("%016x%016x", uint64(rand.Uint64())|1, rand.Uint64())
}

// requestIDs 会话键（sticky key）→ conversationRequestID 的进程内惰性缓存。
//
// 有界：会话键**并非**恒定有限——客户端可任意伪造 conversationId，长期运行下键数
// 随请求增长（原注释"值只增不减，不泄漏"只在"键与粘性会话同源、数量有限"的假设下
// 成立，而该假设不成立）。这里用 map + 互斥锁 + 计数阈值整体重建做上界：条目数达到
// requestIDsMax 时丢弃整张表重建（摊还 O(1)，无第三方依赖，无需维护访问序）。
//
// 为什么选"整体重建"而非真 LRU：本缓存只影响聚合 ID 的稳定性（键被清后再取会换新
// ID，上游用量明细多一条记录），不影响正确性；真 LRU 要维护访问序结构（额外内存 +
// 锁竞争），收益仅是让热键更久存活——不值得。清空后热键在下一次调用即重新缓存。
var (
	requestIDsMu sync.Mutex
	requestIDs   = map[string]string{}
)

// requestIDsMax 缓存条目上界。取 4096：单条 32 hex 值 + 键字符串约百字节量级，
// 上限内存占用约几百 KB；同时远大于正常运行的并发会话数，正常场景永不触发重建。
const requestIDsMax = 4096

// RequestIDForKey 返回会话键的稳定 conversationRequestID：
//   - 同 key：首次调用生成并缓存，此后恒返回同值（一次 user send/同会话多轮聚合）；
//   - 异 key：各自独立，互不相同；
//   - 空 key：每次生成新值（无会话则无"会话内稳定"语义——调用方应在请求级
//     捕获复用，handler 在轮转循环外取一次即天然共享）。
//
// 返回值恒为 32 hex（NewMessageID 形态），可直接用作 B3 TraceId（16/32 hex 合法）。
func RequestIDForKey(key string) string {
	if key == "" {
		return NewMessageID()
	}
	requestIDsMu.Lock()
	if v, ok := requestIDs[key]; ok {
		requestIDsMu.Unlock()
		return v
	}
	id := NewMessageID()
	if len(requestIDs) >= requestIDsMax {
		// 达上界：整体重建（旧键下次调用重新生成——只影响聚合 ID 稳定性，不影响正确性）。
		requestIDs = map[string]string{}
	}
	requestIDs[key] = id
	requestIDsMu.Unlock()
	return id
}

// turnSalt 轮级聚合键的派生盐：进程启动时随机生成，让派生 ID 无法按消息内容
// 被外部预计算；重启换新（重启时旧对话轮已结束，不构成断档）。
var turnSalt = NewMessageID()

// TurnKey 派生「对话轮级」聚合键：body 里**最后一条** role=="user" 消息的
// 「序号 + 文本」。
//
// 为什么需要它：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry Studio
// 等的请求体里既无 conversationId 也无 metadata 键）会让 ExtractKey 恒返回空串，
// 会话头族的聚合主键便只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求
// 一条记录。本函数给这类客户端一个**不依赖客户端配合**的轮级键：一次用户发送内的
// 所有上游调用（tool call 多轮 / 换号重试 / 降级重发）body 里最后一条 user 消息
// 恒定 → 同键；用户发下一条消息 → 换键。
//
// 为什么不取第一条 user 消息：首条在整个会话内不变，会把一次会话的所有轮并进
// 同一个聚合键（跨对话轮混并）。取最后一条才对齐官方 X-Conversation-Request-ID
// 的「对话轮」语义。序号一并入键：两次不同轮里内容相同的提问（"继续"）不会被并成
// 一轮。
//
// 无 body / 无 messages / 无 user 消息 / 该消息无可签名内容 → ""（调用方回落
// 请求级随机 ID，不伪造聚合键）。
//
// 内容签名（G1，见 contentSignature）：末条 user 消息**纯图片**（无 text part）时
// 此前返回空串 → 聚合头退化成请求级随机碎片化。现由 contentSignature 取非文本
// part 的 [type:摘要]，纯图片轮也能建立稳定轮级键；纯文本路径键值与历史完全一致
// （存量轮键零漂移）。
func TurnKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for i := len(obj.Messages) - 1; i >= 0; i-- {
		if obj.Messages[i].Role != "user" {
			continue
		}
		sig := contentSignature(obj.Messages[i].Content)
		if sig == "" {
			// 最后一条 user 消息没有可签名内容（空 / null / 空数组）→ 本轮不建立
			// 聚合键。不继续往前找：整轮内该消息位置恒定，往前找反而会让键随 step 漂移。
			return ""
		}
		return fmt.Sprintf("u%d:%s", i, sig)
	}
	return ""
}

// contentPart 内容签名的输入单元。两条解析路径（ids.go 的 RawMessage 路径与
// session.go 的已解码 any 路径）都归一到本结构，组装规则只定义一处
// （contentSignatureCore），避免两套算法各自漂移。
type contentPart struct {
	Type string // part type："" / "text" 视为文本 part，其余（image_url 等）为非文本
	Text string // part 的 text 字段
	Raw  []byte // part 的**规范**字节（见 canonicalPartBytes）：非文本 part 摘要的哈希源
}

// canonicalPartBytes 把 part 原文归一到「键名排序」的规范 JSON 字节，作为摘要输入。
//
// 为什么必须归一：同一 part 在两条路径上来源不同——TurnKey 拿的是客户端原始
// RawMessage（键序/空白由客户端决定），deriveKey 拿的是已解码的 map[string]any
// （Go 序列化时键名自动排序）。不归一的话同一个 image part 会在轮级键与粘性键里
// 得到两个不同摘要（同一消息两套算法），且客户端换个字段顺序就会换键。
// 归一到规范形态后：两路径摘要恒等，且键对空白/键序变化免疫。
func canonicalPartBytes(raw []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// contentSignatureCore 组装内容签名。三条规则：
//
//  1. **文本优先**：只要有任何非空文本（字符串形态内容，或任一带 text 的 part）→
//     只返回文本拼接，结果与旧 contentText 逐字节相同 → 存量键零漂移；
//  2. 全无文本（纯图片轮）→ 返回各非文本 part 的 `[type:sha256前8hex]`（换行分隔）
//     ——修复纯图片轮"空键"盲区（G1）；摘要只取原文哈希，data: base64 超长内联图
//     不会把键撑长（键长有界）；
//  3. 两者皆空 → ""（不伪造，调用方保留原有空键语义）。
//
// 为什么不像上游 a767465 那样"文本 + 非文本摘要"混入：本仓有既有契约——图片 URL
// 变化不得破坏派生键稳定性（TestExtractKeyMultimodalContent）。带签名/会过期的
// 图片 URL 每轮都变，混入摘要会让粘性键逐轮漂移，把粘性反而打散。文本优先让
// "有文本"的形态键值与历史完全一致，只有历史上恒为空串的纯图片形态才获得新键。
func contentSignatureCore(parts []contentPart) string {
	var texts, markers []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
			continue
		}
		if p.Type == "" || p.Type == "text" {
			continue // 无文本的文本 part 不贡献（保持 [{"type":"text","text":""}] → "" 旧口径）
		}
		sum := sha256.Sum256(p.Raw)
		markers = append(markers, "["+p.Type+":"+hex.EncodeToString(sum[:4])+"]")
	}
	if len(texts) > 0 {
		return strings.Join(texts, "")
	}
	return strings.Join(markers, "\n")
}

// contentSignature 取消息 content 的确定性签名（G1 修复——纯图片轮不再碎片化）。
// 入参为原始 content JSON；组装规则见 contentSignatureCore。
//
// 形态口径与旧 contentText 完全一致（字符串形态原样返回、数组形态拼接 text 字段、
// 空 / null / 非字符串非数组 → ""），差异仅在"全无文本但含非文本 part"时由 "" 变为
// 非文本摘要序列——即纯图片轮的修复面。
func contentSignature(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return ""
		}
		return contentSignatureCore([]contentPart{{Text: str}})
	case '[':
		var raws []json.RawMessage
		if err := json.Unmarshal(raw, &raws); err != nil {
			return ""
		}
		parts := make([]contentPart, 0, len(raws))
		for _, pr := range raws {
			var p struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(pr, &p); err != nil {
				// 数组里混入非对象元素（裸字符串/数字等）属未知形态：整体返回 ""
				// 不派生键。与旧 contentText（反序列化到 []struct 直接失败）逐字节
				// 同口径——不因未知形态伪造键，避免脏绑定。
				return ""
			}
			part := contentPart{Type: p.Type, Text: p.Text}
			// 只有"会被计入标记"的 part（非文本类型且无文本）才需要规范字节：
			// 文本 part 只贡献 Text，空文本的文本 part 两者都不贡献。
			if p.Text == "" && p.Type != "" && p.Type != "text" {
				canon, err := canonicalPartBytes(pr)
				if err != nil {
					return ""
				}
				part.Raw = canon
			}
			parts = append(parts, part)
		}
		return contentSignatureCore(parts)
	}
	return ""
}

// contentSignatureAny 是 contentSignature 的「已解码形态」版本，供 session.go 的
// deriveKey 使用（那里 body 已整体 Unmarshal 成 map[string]any，不再持有原始
// RawMessage）。归一到同一 contentPart 列表 + 同一 contentSignatureCore，两条
// 链路的签名算法**只此一处定义**，不会各自漂移；非文本 part 的摘要输入经
// json.Marshal 得到规范字节，与 contentSignature 的 canonicalPartBytes 恒等。
//
// 兼容口径与旧 messageText 一致：
//   - string → 文本原样（不 trim，与旧 messageText 相同）；
//   - []any 的每个 map[string]any part → type/text 字段；
//   - 数组里的非对象元素**跳过**（与旧 messageText 的 `if p, ok := ...` 同口径）；
//   - 其他形态（数字 / 对象 / null）→ ""（旧 messageText 取不到文本即空）。
//
// 与 RawMessage 版（contentSignature）的唯一分歧在"数组含非对象元素"：那边沿用旧
// contentText 的整体失败语义（→ ""）。两版各自保持本键族的存量行为，不引入键漂移。
func contentSignatureAny(content any) string {
	switch v := content.(type) {
	case string:
		return contentSignatureCore([]contentPart{{Text: v}})
	case []any:
		parts := make([]contentPart, 0, len(v))
		for _, item := range v {
			p, ok := item.(map[string]any)
			if !ok {
				continue // 非对象元素：跳过（旧 messageText 同口径）
			}
			part := contentPart{Type: strOrEmpty(p["type"]), Text: strOrEmpty(p["text"])}
			// 同 contentSignature：只有"会被计入标记"的 part 才需要序列化字节。
			if part.Text == "" && part.Type != "" && part.Type != "text" {
				raw, err := json.Marshal(p)
				if err != nil {
					continue // 序列化失败（理论不可达：来自已成功 Unmarshal 的值）
				}
				part.Raw = raw
			}
			parts = append(parts, part)
		}
		return contentSignatureCore(parts)
	}
	return ""
}

// TurnRequestID 返回轮级键对应的聚合 ID：sha256(盐|键) 前 16 字节的 hex（32 位，
// 与 NewMessageID 同形态，可直接作 B3 TraceId）。
//
// 纯派生，无缓存、无 TTL、不随进程内请求数增长内存 —— 这点与会话级的
// RequestIDForKey 相反：会话键数量有限（与粘性会话同源）可以常驻缓存，而轮级键
// 每个对话轮新增一条，缓存必须有界，派生式天然有界。
// 空键返回新随机值（无轮可聚合时保持原有的「每请求独立」行为）。
func TurnRequestID(turnKey string) string {
	if turnKey == "" {
		return NewMessageID()
	}
	sum := sha256.Sum256([]byte(turnSalt + "|" + turnKey))
	return hex.EncodeToString(sum[:16])
}
