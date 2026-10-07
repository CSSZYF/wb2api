package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// handler_image_terminal_test.go 请求级图片错误（11135 invalid_image_data）端到端验收。
//
// 缺陷（用户实测 503）：上游对**图片数据本身**的拒绝（400 + code 11135）落
// Classify 的通用 4xx 兜底 ErrClient → applyErrorPolicy default 分支喂连败计数
// （NoteFailures）→ 每个账号各记一次连败 → 达阈（默认 5）全体降权 10 分钟 →
// 「一张坏图拖垮整个账号池」。图片是否可识别是**请求的属性**（同一张图换任何账号
// 都会被拒），与账号健康无关——照 ErrContentBlocked/ErrPromptTooLong 的既有范式
// 归「请求级终态」：不轮转、不罚号，400 + 上游原文逐字透传。
//
// 11133 model_param_invalid 的相邻风险：判为「不罚号但**仍轮转**」（ErrBadParams
// 既有语义）——不同账号可能路由到不同后端、模型能力/权限不同（11102 的
// (账号,模型) 负缓存即此事实的既有证据），轮转仍有价值；但绝不能喂连败
// （轮转 N 个账号 = N 次计数，同样是「一个请求拖垮全池」）。

// body11135User 用户实测的 11135 原始 body（报告原文形态，逐字）。
// 含 code/extError.code/displayMsg.zh/actions 全形态——测试断言它被逐字透传。
const body11135User = `{"code":11135,"msg":"Please start a new conversation, replace the image, and try again.",` +
	`"requestId":"bbf3277b-1f4c-4e0a-9d6f-0a1b2c3d4e5f",` +
	`"extError":{"code":"invalid_image_data","message":"Please start a new conversation, replace the image, and try again.",` +
	`"param":"","type":"invalid_request_error","StatusCode":400},` +
	`"displayMsg":{"en":"The image cannot be processed.","zh":"图片无法识别，请更换图片（建议 png/jpeg/webp）后重试。"},` +
	`"actions":["SUBMIT_FEEDBACK","COPY_ERROR","REUPLOAD"]}`

// imgReqBody 携带 image_url part 的聊天请求（用户报错时的请求形态）。
const imgReqBody = `{"model":"glm-5.2","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"describe this"},` +
	`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="}}]}]}`

// TestChatInvalidImageTerminalNoRotateNoPenalty 11135 端到端（核心回归）：
// 上游 400 + 11135 原文 → 客户端拿到 **400**（不是 503）、error.code=invalid_image_data、
// error.message 逐字等于上游原文、带 gateway_hint；且**账号池状态未变**（无连败计数
// 增长、无降权、无冷却/熔断/禁用）。
func TestChatInvalidImageTerminalNoRotateNoPenalty(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11135User, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReqBody)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400（请求级终态，不是 503 全池不可用）", rec.Code, rec.Body)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "invalid_image_data" {
		t.Errorf("code=%q want invalid_image_data", e.Error.Code)
	}
	// message 必须逐字等于上游原文（透传纪律：code/requestId/extError/displayMsg/
	// actions 原样，含上游 zh 文案——客户端据此自行排查）。
	if e.Error.Message != body11135User {
		t.Errorf("message 非逐字原文:\n got=%q\nwant=%q", e.Error.Message, body11135User)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "image request was rejected") {
		t.Errorf("gateway_hint=%v want image-data hint", e.Error.GatewayHint)
	}
	// 不轮转：多账号池也只打第一个号（同一张坏图换任何账号都被拒，轮转纯属浪费）。
	if calls != 1 {
		t.Errorf("upstream calls=%d want exactly 1 (no rotation on 11135)", calls)
	}
	// 不罚号：两个账号均无连败计数/降权/冷却/熔断/禁用/错误累计。
	for _, uid := range []string{"a1", "a2"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("account %s missing", uid)
		}
		if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("%s: 11135 不得喂连败/降权: consecutive_fails=%d degrade_until=%v",
				uid, st.ConsecutiveFails, st.DegradeUntil)
		}
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("%s: 11135 不得冷却/禁用/熔断: %+v", uid, st)
		}
	}
	if uids := p.AvailableUIDs(); len(uids) != 2 {
		t.Errorf("账号池应保持完整可用, got %v", uids)
	}
}

// TestChatInvalidImageThreeAccountsNoPoolPollution 多账号不污染（缺陷 A 的核心自伤面）：
// 池内 3 个账号、同一 11135 请求打过来 → 只尝试 1 个账号，三个账号的
// consecutive_fails 全为 0（修复前：3 个账号各记一次连败，且连续几次坏图请求即全池降权）。
func TestChatInvalidImageThreeAccountsNoPoolPollution(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11135User, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReqBody)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1（11135 不轮转，不得把同一坏图放大到全池）", calls)
	}
	for _, uid := range []string{"a1", "a2", "a3"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 {
			t.Errorf("%s consecutive_fails=%d want 0（请求级错误不指向任何账号）", uid, st.ConsecutiveFails)
		}
		if !st.DegradeUntil.IsZero() || st.Cooling {
			t.Errorf("%s 不应降权/冷却: %+v", uid, st)
		}
	}
	if uids := p.AvailableUIDs(); len(uids) != 3 {
		t.Errorf("账号池应保持完整可用, got %v", uids)
	}
}

// TestChat11133RotatesButNoConsecutiveFails 11133 相邻风险（任务 B 判断 +
// 同指纹收敛升级）：首个 11133 仍换号（不同账号可能路由到不同后端/模型能力，
// 11102 的 (账号,模型) 负缓存是既有证据）；但第二个账号回**同一业务码**即证明
// 确定性拒绝 → 400 终态，第三号不再放大调用。且**绝不喂连败**：修复前 11133 落
// ErrClient → 各号记连败，与 11135 同一自伤面。此处锁定：恰好 2 次上游调用 +
// 400 bad_params + message 逐字含上游原文 + hint 不回归 + 全池连败为 0。
func TestChat11133RotatesButNoConsecutiveFails(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11133Real, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReqBody)))

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	// 同指纹确定性收敛：首号 11133 → 换号；第二号同码 → 400 终态，第三号不打。
	if calls != 2 {
		t.Errorf("calls=%d want 2（第二号同指纹即请求级终态，不放大到满轮转）", calls)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 bad_params（不再 503 误导为池子空）", rec.Code)
	}
	// message 逐字含上游原文（透传纪律不回归）。
	if !strings.HasSuffix(e.Error.Message, body11133Real) {
		t.Errorf("message must carry verbatim 11133 body: %q", e.Error.Message)
	}
	// 11133 的 hint 形态不回归（hint 层自带形态判定）。
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != hintNeutralParams {
		t.Errorf("gateway_hint=%v want neutral params hint", e.Error.GatewayHint)
	}
	for _, uid := range []string{"a1", "a2", "a3"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 {
			t.Errorf("%s consecutive_fails=%d want 0（轮转不得把每个号都记一次连败）", uid, st.ConsecutiveFails)
		}
	}
	if uids := p.AvailableUIDs(); len(uids) != 3 {
		t.Errorf("账号池应保持完整可用, got %v", uids)
	}
}

// TestChatErrClientFingerprintDedupAcrossAccounts ErrClient（未知 4xx）连败喂入的
// 请求级去重：同一请求在 3 个账号上撞**同一错误形态**（同一业务 code，requestId
// 每次不同）→ 只喂首个账号的连败计数。修复前每个账号各记一次 → 一个请求就能把
// N 个账号推向降权阈值。轮转语义不变（仍试满 3 个账号），不同错误形态仍各自计数。
func TestChatErrClientFingerprintDedupAcrossAccounts(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		// 11148 tool_call_sequence_broken：本仓 tool_pairing.go 记录的**请求级**
		// 4xx（工具配对断裂），不在 Classify 任何专用词表里 → 落 ErrClient。
		// requestId 逐次不同（上游真实行为），故去重键必须取业务 code 而非整 body。
		return 400, fmt.Sprintf(`{"code":11148,"msg":"tool calls and tool results do not match","requestId":"rq-%d"}`, calls), false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503（ErrClient 仍轮转，末端既有语义不变）body=%s", rec.Code, rec.Body)
	}
	if calls != 3 {
		t.Fatalf("calls=%d want 3（轮转语义不变）", calls)
	}
	fed := 0
	for _, uid := range []string{"a1", "a2", "a3"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 {
			fed++
			if st.ConsecutiveFails != 1 {
				t.Errorf("%s consecutive_fails=%d want 1", uid, st.ConsecutiveFails)
			}
		}
	}
	if fed != 1 {
		t.Errorf("喂入连败的账号数=%d want 1（同一请求同一错误形态只算一次证据）", fed)
	}
}

// TestHasImagePartForms hasImagePart 形态覆盖：type 判定对 data:/http URL 两种
// url 形态都成立（只看 part type，不看 url 内容），字符串形态 content 不误报。
// 混合形态（部分消息 content 是字符串、另一条是带图数组）必须仍判出带图——
// 整体 json.Unmarshal 到固定结构时，字符串 content 的类型错误会让**整次解析失败**，
// 从而漏判「请求带图」（11133 的换模型 hint 前提丢失）。
func TestHasImagePartForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"data URL", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`, true},
		{"http URL", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"}}]}]}`, true},
		{"image_url 字符串形态", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://cdn.example.com/a.png"}]}]}`, true},
		{"纯文本 part", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false},
		{"字符串 content", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"混合：字符串 content + 带图数组", `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`, true},
		{"畸形 body", `{not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasImagePart([]byte(c.body)); got != c.want {
				t.Errorf("hasImagePart=%v want %v body=%s", got, c.want, c.body)
			}
		})
	}
}
