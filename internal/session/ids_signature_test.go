package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// contentSignature 的 TDD 锚点（G1：纯图片轮聚合碎片化 + 首图会话粘性盲区）。
//
// 上游 a767465 引入 contentSignature 修同款问题，本仓吸收时**刻意偏离**一处：
// 上游把"文本 part 拼接 + 非文本 part 摘要"混入同一签名，本仓改为**文本优先**
// （有文本则只返回文本拼接，与旧 contentText/messageText 逐字节一致）——因为本仓
// 有既有契约「图片 URL 变化不得破坏派生键稳定性」（TestExtractKeyMultimodalContent），
// 带签名/会过期的图片 URL 每轮都变，混入摘要会让粘性键逐轮漂移。纯图片形态历史上
// 恒为空串（无键可漂移），只有它获得新的 [type:摘要] 键。

// 纯图 body 构造 helper（末条 user 在 index=turnIdx 处）。
func imgOnlyBody(url string) string {
	return `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + url + `"}}]}]}`
}

// TestTurnKeyImageOnlyStable 锚点1：纯图末条 user 同 body ×2 同键（原返回 ""，
// 聚合链退化请求级随机碎片化）。
func TestTurnKeyImageOnlyStable(t *testing.T) {
	body := []byte(imgOnlyBody("https://img.example/cat.png"))
	a := TurnKey(body)
	b := TurnKey(body)
	if a == "" {
		t.Fatal("纯图末条 user 应派生非空轮级键（G1：原为空串碎片化）")
	}
	if a != b {
		t.Fatalf("同 body 纯图应同键: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "u0:[image_url:") {
		t.Errorf("纯图键应为 u<idx>:[type:摘要] 形态: %q", a)
	}
}

// TestTurnKeyImageTextCombined 锚点2：图文混合键 == 纯文本键（文本优先，向后兼容），
// 且两者都区别于纯图键。
//
// 这是与上游 a767465 的**有意分歧**：上游让图文混合键 != 纯文本键（摘要混入），
// 本仓保持图文混合 == 纯文本——图片 URL 常带签名且会过期，混入会让多模态会话的
// 粘性/聚合键逐轮漂移（本仓 TestExtractKeyMultimodalContent 已钉此契约）。
func TestTurnKeyImageTextCombined(t *testing.T) {
	mixed := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`)
	textOnly := []byte(`{"messages":[{"role":"user","content":"看图"}]}`)
	imageOnly := []byte(imgOnlyBody("https://img.example/cat.png"))
	m := TurnKey(mixed)
	tx := TurnKey(textOnly)
	im := TurnKey(imageOnly)
	if m == "" || tx == "" || im == "" {
		t.Fatalf("三形态均应非空: mixed=%q text=%q image=%q", m, tx, im)
	}
	if tx != "u0:看图" {
		t.Errorf("纯文本路径签名应与旧 contentText 完全一致（向后兼容）: got %q", tx)
	}
	if m != tx {
		t.Errorf("图文混合应只取文本（图片 URL 不入键，防签名 URL 漂移）: mixed=%q text=%q", m, tx)
	}
	if m == im {
		t.Errorf("图文混合键应区别于纯图键: %q", m)
	}
}

// TestTurnKeyImageDifferentURLDifferentKey 锚点3：不同图不同键。
func TestTurnKeyImageDifferentURLDifferentKey(t *testing.T) {
	a := TurnKey([]byte(imgOnlyBody("https://img.example/cat.png")))
	b := TurnKey([]byte(imgOnlyBody("https://img.example/dog.png")))
	if a == "" || b == "" {
		t.Fatalf("均应非空: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("不同图应不同键: %q", a)
	}
}

// TestTurnKeyIndexStillSeparatesTurns 锚点4：同图不同序号（跨轮）不同键
// （序号入键防"继续"类跨轮混并的既有设计不得回退）。
func TestTurnKeyIndexStillSeparatesTurns(t *testing.T) {
	a := TurnKey([]byte(imgOnlyBody("https://img.example/cat.png")))
	b := TurnKey([]byte(`{"messages":[{"role":"user","content":"第一问"},` +
		`{"role":"assistant","content":"答"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`))
	if a == "" || b == "" {
		t.Fatalf("均应非空: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("同图不同序号应不同键（跨轮不混并）: %q", a)
	}
}

// TestTurnKeyImageCountMatters 多图轮：张数不同 → 摘要序列不同 → 键不同
// （纯图轮靠"part 序列"区分，无需额外计数占位）。
func TestTurnKeyImageCountMatters(t *testing.T) {
	one := TurnKey([]byte(imgOnlyBody("https://img.example/cat.png")))
	two := TurnKey([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}},` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`))
	if one == "" || two == "" {
		t.Fatalf("均应非空: %q %q", one, two)
	}
	if one == two {
		t.Errorf("同图一张 vs 两张应不同键（part 序列不同）: %q", one)
	}
}

// TestContentSignatureBounds 签名边界：data: 超长 base64 只入摘要（键长有界）；
// 空/null/空数组不伪造。
func TestContentSignatureBounds(t *testing.T) {
	longDataURL := "data:image/png;base64," + strings.Repeat("QUFBQQ", 4096)
	k := TurnKey([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"` + longDataURL + `"}}]}]}`))
	if k == "" {
		t.Fatal("data: 超长图应仍派生键")
	}
	if len(k) > 256 {
		t.Errorf("签名键长度应有界（摘要防超长）: len=%d", len(k))
	}
	// content 为空 / null / 空数组 → ""（不伪造，保留空键语义）。
	for _, body := range []string{
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":[]}]}`,
		`{"messages":[{"role":"user","content":""}]}`,
		// 文本 part 全为空串（无文本、无非文本 part）→ ""（与旧 contentText 同口径）。
		`{"messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`,
	} {
		if got := TurnKey([]byte(body)); got != "" {
			t.Errorf("无可签名内容应空串: %s got %q", body, got)
		}
	}
}

// TestContentSignatureAnyMatchesRawSignature 两条签名路径（RawMessage 版与已解码
// any 版）对同一 content 必须给出同一签名——否则轮级键与粘性键会在同一条消息上
// 分裂成两套算法。
func TestContentSignatureAnyMatchesRawSignature(t *testing.T) {
	// 与 body 内 content 逐字节对应的原文（map 序列化键序：encoding/json 对
	// map[string]any 按键名排序，故 raw 形态用排序后键序构造）。
	cases := []struct {
		name    string
		content string // 原始 JSON content
	}{
		{"纯文本", `"你好"`},
		{"纯图", `{"type":"image_url","image_url":{"url":"https://x/cat.png"}}`},
		{"图文", `{"text":"看图","type":"text"}`},
		{"空文本 part", `{"text":"","type":"text"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := []byte(c.content)
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// 数组形态：把单 part 包成数组，两条路径都必须一致。
			rawArr := []byte("[" + c.content + "]")
			decArr := []any{decoded}
			got1 := contentSignature(rawArr)
			got2 := contentSignatureAny(decArr)
			if got1 != got2 {
				t.Errorf("两路径签名应一致: raw=%q any=%q", got1, got2)
			}
			// 字符串形态。
			if s := contentSignature(raw); s != contentSignatureAny(decoded) {
				t.Errorf("字符串形态两路径应一致: raw=%q any=%q", s, contentSignatureAny(decoded))
			}
		})
	}
}
