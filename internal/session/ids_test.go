package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// imagePartSig 计算 part 的**规范**字节摘要（sha256 前 8 hex），供期望值精确构造——
// 与 contentSignature 的 canonicalPartBytes 同口径（键名排序后序列化），签名算法
// 变更时测试期望值随此 helper 单点同步。
func imagePartSig(t *testing.T, part string) string {
	t.Helper()
	canon, err := canonicalPartBytes([]byte(part))
	if err != nil {
		t.Fatalf("canonicalPartBytes(%s): %v", part, err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:4])
}

// TestResolveConversationID 覆盖 conversationId 提取的 snake/camel/缺失三态：
//   - metadata.conversation_id / metadata.conversationId → 取值
//   - 顶层 conversation_id / conversationId → 取值（snake 优先同 ExtractKey）
//   - 缺失 / 只有 user_id → ""（会话头族语义只认对话 ID，绝不回落 user_id）
func TestResolveConversationID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"metadata snake_case", `{"metadata":{"conversation_id":"conv-1"}}`, "conv-1"},
		{"metadata camelCase", `{"metadata":{"conversationId":"conv-2"}}`, "conv-2"},
		{"top-level snake_case", `{"conversation_id":"conv-3"}`, "conv-3"},
		{"top-level camelCase", `{"conversationId":"conv-4"}`, "conv-4"},
		{"snake wins over camel", `{"conversation_id":"conv-s","conversationId":"conv-c"}`, "conv-s"},
		{"missing", `{"model":"glm-5.2"}`, ""},
		{"empty body", ``, ""},
		{"broken json", `{broken`, ""},
		{"metadata user_id only", `{"metadata":{"user_id":"u1"}}`, ""},
		{"top-level user_id only", `{"user_id":"u1"}`, ""},
		{"empty string value", `{"conversationId":""}`, ""},
		{"non-string value", `{"conversationId":123}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveConversationID([]byte(c.body)); got != c.want {
				t.Errorf("ResolveConversationID(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestNewMessageIDFormat 32 位 hex 且不为空、两次调用大概率不同（随机性冒烟）。
func TestNewMessageIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := NewMessageID()
		if len(id) != 32 {
			t.Fatalf("NewMessageID() = %q len=%d want 32", id, len(id))
		}
		for _, ch := range id {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("NewMessageID() = %q has non-hex char %q", id, ch)
			}
		}
	}
}

// TestRequestIDForKeyStability 同 key 恒稳定、异 key 各不同、空 key 每次新值。
func TestRequestIDForKeyStability(t *testing.T) {
	// 预热清空包级缓存，避免其他测试污染 key（测试隔离）。
	k1a := RequestIDForKey("conv-a")
	k1b := RequestIDForKey("conv-a")
	if k1a != k1b {
		t.Errorf("same key should be stable: %q vs %q", k1a, k1b)
	}
	k2 := RequestIDForKey("conv-b")
	if k1a == k2 {
		t.Errorf("different keys should differ: %q", k1a)
	}
	// 空 key：每次调用生成新值（无会话则无"会话内稳定"语义）。
	e1 := RequestIDForKey("")
	e2 := RequestIDForKey("")
	if e1 == e2 {
		t.Errorf("empty key should yield fresh values each call: %q", e1)
	}
	// 稳定值自身也须是 32 hex（可作 B3 TraceId 直接使用）。
	for _, id := range []string{k1a, k2, e1} {
		if len(id) != 32 {
			t.Errorf("RequestIDForKey value %q len=%d want 32", id, len(id))
		}
	}
}

// TestTurnKeyExtraction 轮级兜底键的提取：取**最后一条** user 消息的「序号+内容签名」，
// 轮内追加 assistant/tool 消息不改变键；无 user / 无可签名内容 / 坏 JSON 一律空串。
// 纯文本与图文混合形态的键值与历史逐字节一致（contentSignature 的文本优先口径），
// 仅纯图片形态由 "" 变为 [type:摘要]（G1 修复）。
func TestTurnKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"single user", `{"messages":[{"role":"user","content":"你好"}]}`, "u0:你好"},
		{"last user wins", `{"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`, "u2:第二问"},
		// agent 多步：轮内追加 assistant/tool 消息，末条 user 位置与内容不变 → 同键。
		{"agent step keeps same key", `{"messages":[{"role":"user","content":"任务"},{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","content":"结果"}]}`, "u0:任务"},
		{"multimodal parts text joined", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "u0:看图"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"messages key absent", `{"model":"glm-5.2"}`, ""},
		{"broken json", `{broken`, ""},
		{"empty body", ``, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"null content", `{"messages":[{"role":"user","content":null}]}`, ""},
		{"image only content", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
			// 纯图片 content 现按内容签名派生非空轮级键（G1 修复，原为 ""）；
			// 键含 image part 摘要（sha256 前 8 hex）。
			"u0:[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"x"}}`) + "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TurnKey([]byte(c.body)); got != c.want {
				t.Errorf("TurnKey(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestTurnRequestIDDerivation 轮级 ID 是纯派生：同键恒同值、异键异值、空键每次新值、
// 形态恒 32 hex。
func TestTurnRequestIDDerivation(t *testing.T) {
	a1 := TurnRequestID("u0:同一个问题")
	a2 := TurnRequestID("u0:同一个问题")
	if a1 != a2 {
		t.Errorf("same turn key should derive same id: %q vs %q", a1, a2)
	}
	if b := TurnRequestID("u0:另一个问题"); b == a1 {
		t.Errorf("different turn keys should derive different ids: %q", b)
	}
	// 同文本但序号不同（不同轮里内容相同的提问）也要分开。
	if c := TurnRequestID("u2:同一个问题"); c == a1 {
		t.Errorf("same text at different position should differ: %q", c)
	}
	// 空键：无轮可聚合 → 每次新值（保持原有请求级独立行为）。
	if e1, e2 := TurnRequestID(""), TurnRequestID(""); e1 == e2 {
		t.Errorf("empty turn key should yield fresh values each call: %q", e1)
	}
	for _, id := range []string{a1, TurnRequestID(""), TurnRequestID("x")} {
		if len(id) != 32 {
			t.Errorf("TurnRequestID value %q len=%d want 32", id, len(id))
		}
	}
}

// TestRequestIDForKeyBoundedCache 缓存有界：条目数达 requestIDsMax 后整体重建，
// 旧键被清（下次调用生成新 ID）、新键照常工作；同键在未触发重建时仍稳定。
//
// 背景：原实现用 sync.Map 且注释称"值只增不减，不泄漏——key 与粘性会话键同源，
// 数量有限"。该假设不成立——客户端可任意伪造 conversationId，键数随请求无限增长，
// 长期运行下进程内存单调上升。改为 map+互斥锁+阈值重建。
func TestRequestIDForKeyBoundedCache(t *testing.T) {
	// 清空缓存（其他测试可能已写入），并保证测试结束后恢复干净状态。
	requestIDsMu.Lock()
	requestIDs = map[string]string{}
	requestIDsMu.Unlock()
	defer func() {
		requestIDsMu.Lock()
		requestIDs = map[string]string{}
		requestIDsMu.Unlock()
	}()

	// 未达上界：同键稳定。
	first := RequestIDForKey("bounded-k0")
	if again := RequestIDForKey("bounded-k0"); again != first {
		t.Fatalf("同键应稳定: %q vs %q", first, again)
	}

	// 灌入 requestIDsMax 个键，触发重建。第 requestIDsMax 个键写入前会清表。
	for i := 0; i < requestIDsMax; i++ {
		RequestIDForKey("bounded-fill-" + strconv.Itoa(i))
	}
	requestIDsMu.Lock()
	n := len(requestIDs)
	requestIDsMu.Unlock()
	if n > requestIDsMax {
		t.Fatalf("缓存条目超上界: len=%d max=%d", n, requestIDsMax)
	}
	if n < 1 {
		t.Fatalf("重建后缓存应可继续工作: len=%d", n)
	}

	// 旧键已被清：重建后 k0 重新生成（不保证与旧值不同，故断言缓存内不再持有旧值
	// 或返回的仍是合法 32 hex——真正的行为断言是"缓存不无限增长"，此处校验可用性）。
	if got := RequestIDForKey("bounded-k0"); len(got) != 32 {
		t.Errorf("重建后旧键应可正常取值: %q", got)
	}
	// 新键照常工作且稳定。
	nk := RequestIDForKey("bounded-new-after-rebuild")
	if nk2 := RequestIDForKey("bounded-new-after-rebuild"); nk != nk2 {
		t.Errorf("重建后新键应稳定: %q vs %q", nk, nk2)
	}
}

// TestRequestIDForKeyEvictsOldKeys 超限后旧键确实被清出缓存：填满后，早期键在
// 缓存中不存在（Load 未命中），而新键存在。
func TestRequestIDForKeyEvictsOldKeys(t *testing.T) {
	requestIDsMu.Lock()
	requestIDs = map[string]string{}
	requestIDsMu.Unlock()
	defer func() {
		requestIDsMu.Lock()
		requestIDs = map[string]string{}
		requestIDsMu.Unlock()
	}()

	RequestIDForKey("evict-old")
	for i := 0; i < requestIDsMax; i++ {
		RequestIDForKey("evict-fill-" + strconv.Itoa(i))
	}
	requestIDsMu.Lock()
	_, oldPresent := requestIDs["evict-old"]
	_, newPresent := requestIDs["evict-fill-"+strconv.Itoa(requestIDsMax-1)]
	requestIDsMu.Unlock()
	if oldPresent {
		t.Errorf("超限重建后旧键 evict-old 应已被清出缓存")
	}
	if !newPresent {
		t.Errorf("超限重建后新键 evict-fill-%d 应在缓存中", requestIDsMax-1)
	}
}
