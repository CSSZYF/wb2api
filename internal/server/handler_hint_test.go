// handler_hint_test.go gateway_hint 端到端验收（任务书 gateway-hint）：
// error.message 永远是上游原文透传（逐字）、gateway_hint 并列补充、未覆盖形态
// 不带字段、SSE error 帧同样附加、本地调度错误带 no_healthy hint。
//
// 与本 fork 既有口径的关系（移植上游 fa7b5d9 时的适配点，逐条）：
//   - 本 fork 末端错误出口的 message 是**既有**形态
//     "all accounts unavailable (cooling/disabled): upstream <kind> (http <status>): <上游 body>"
//     —— 上游原文逐字出现在 message 里（本批不改 message 一个字节，见
//     TestChatHintMessageUnchangedAnchor 的锚定断言）；上游 fork 的 message 是裸
//     body，两边 message 形态差异是既有分歧，**不属于本批范围**（本批只加并列字段）。
//   - 本 fork 末端状态码恒 503（上游 fork 把 ErrSoftRate 映射 429）；本批不动状态码，
//     故 hint 用例断言 503 + hint 文案，不照抄上游的 429 断言。
//   - ErrContentBlocked 本 fork 有「不泄露上游 11128」的既有口径
//     （handler_test.go TestContentBlockedCustomModeDoesNotDegrade），故
//     content_blocked 的 message 用网关防火墙文案、hint 措辞不含 "upstream"。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// hintEnvelope 错误响应解形态（含可选 gateway_hint）。
type hintEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		// GatewayHint 用指针：未覆盖形态必须**字段缺席**（JSON 键不存在），
		// 不能是空串（断言「不带字段不编造」）。
		GatewayHint *string `json:"gateway_hint"`
	} `json:"error"`
}

// seedModelsCache 把动态模型目录缓存置为指定条目（hintContext 只读缓存快照，
// 测试隔离：测试前 seed、测试后 reset，见 resetModelsCache）。
func seedModelsCache(infos []upstream.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now() // 热（TTL 内）
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// resetModelsCache 清空动态模型目录缓存（hintContext 只读该缓存，用例之间必须
// 互不残留；测试前后各调一次）。
func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// imgBody11133 携带 image_url 的请求体（11133 场景：不支持图片的模型传图）。
const imgBody11133 = `{"model":"deepseek-v3-0324","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`

// body11133Real 图片回归实测的 11133 原始 body（2026-09-16 真实账号抓取）。
const body11133Real = `{"code":11133,"msg":"Invalid request parameters","requestId":"6376d207cb901455a6851344c5e52769","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider","param":"","type":"invalid_request_error","StatusCode":400,"Request":null,"Response":null},"displayMsg":{"en":"The request parameters do not meet the current model requirements. Please adjust and retry."}}`

// body11135Real 图片回归实测的 11135 原始 body。
const body11135Real = `{"code":11135,"msg":"Please start a new conversation, replace the image, and try again.","requestId":"32ff8c86419322f4a06804365d5ffd88","extError":{"code":"400001","message":"Please start a new conversation, replace the image, and try again.","param":"","type":"invalid_request_error","StatusCode":400,"Request":null,"Response":null},"displayMsg":{"en":"The image cannot be processed. Please use a valid png/jpeg/webp image and retry."}}`

// hintNeutralParams 11133 非「目录证明不支持图片」场景的中性参数 hint。
const hintNeutralParams = "request parameters were rejected by the model provider; check message format and model capabilities"

// TestChat11133HintModelNoImages 11133 + 请求带图 + 目录声明 supports_images=false
// → hint 点名模型并指向 /v1/models；message 逐字含上游原文（不被 hint 污染）。
func TestChat11133HintModelNoImages(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v3-0324", SupportsImages: false},
		{ID: "hy3", SupportsImages: true},
	})
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11133Real, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	// message 逐字含上游原文（本 fork 末端 message 有既有本地前缀，原文必须完整出现）。
	if !strings.HasSuffix(e.Error.Message, body11133Real) {
		t.Errorf("message must carry verbatim upstream body, got %q", e.Error.Message)
	}
	want := "model deepseek-v3-0324 does not support images; pick one with supports_images=true from /v1/models"
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != want {
		t.Errorf("gateway_hint=%v want %q", e.Error.GatewayHint, want)
	}
	// 目录查询零上游调用（cachedModelsSnapshot 只读缓存）：上游只被 chat 打过。
	// 2 账号 MaxRotate=3 但 tried 去重后最多 2 次 chat。
	if calls > 2 {
		t.Errorf("catalog lookup must not trigger upstream calls, calls=%d", calls)
	}
}

// TestChat11133HintCatalogSupportsImages 11133 + 带图 + 目录声明支持（数据非法撞
// 11133）→ 中性参数 hint；目录未收录（缓存冷）→ 同样中性（宁缺勿滥）。
func TestChat11133HintCatalogSupportsImages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seeded []upstream.ModelInfo
	}{
		{"目录声明支持", []upstream.ModelInfo{{ID: "deepseek-v3-0324", SupportsImages: true}}},
		{"目录未收录（缓存冷）", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsCache()
			defer resetModelsCache()
			if tc.seeded != nil {
				seedModelsCache(tc.seeded)
			}
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				return 400, body11133Real, false
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))
			var e hintEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("resp not json: %v", err)
			}
			if !strings.HasSuffix(e.Error.Message, body11133Real) {
				t.Errorf("message must carry verbatim upstream body: %q", e.Error.Message)
			}
			if e.Error.GatewayHint == nil || *e.Error.GatewayHint != hintNeutralParams {
				t.Errorf("gateway_hint=%v want neutral params hint", e.Error.GatewayHint)
			}
			if strings.Contains(*e.Error.GatewayHint, "does not support images") {
				t.Errorf("must NOT claim model-not-supports-images without catalog proof: %q", *e.Error.GatewayHint)
			}
		})
	}
}

// TestChat11133HintNoImage 11133 但请求不带图（纯参数问题）→ 中性 hint，
// 不点名图片。
func TestChat11133HintNoImage(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "deepseek-v3-0324", SupportsImages: false}})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11133Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3-0324","messages":[{"role":"user","content":"hi"}]}`)))
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.GatewayHint == nil || strings.Contains(*e.Error.GatewayHint, "images") {
		t.Errorf("no-image 11133 must be neutral hint, got %v", e.Error.GatewayHint)
	}
}

// TestChat11135Hint 11135 invalid_image_data → 图片数据 hint；message 逐字原文。
func TestChat11135Hint(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11135Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.Error.Message, body11135Real) {
		t.Errorf("message must carry verbatim 11135 body: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "image data rejected") {
		t.Errorf("gateway_hint=%v want image-data hint", e.Error.GatewayHint)
	}
}

// TestChatHintMessageUnchangedAnchor message 逐字不变锚：本批只新增并列字段，
// **message 一个字节都不动**——断言各错误出口的 message 等于改动前既有形态
// （硬编码本批之前的期望串；任何「顺手改写 message」都会让本测试红）。
//
// 11115 走 prompt_too_long 专用出口（message = 上游原文裸串，无本地前缀）；
// 其余走末端出口（message = 既有本地前缀 + "upstream <kind> (http <status>): " + 原文）。
func TestChatHintMessageUnchangedAnchor(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantMsg  string
		wantHint string
	}{
		{
			name:   "11115 prompt too long 专用出口",
			status: 400,
			body:   `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`,
			// 既有形态：上游原文裸串（promptTooLongMessage 透传，无本地前缀）。
			wantCode: "prompt_too_long",
			wantMsg:  `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`,
			wantHint: "request context exceeds the model's limit; reduce history/message size",
		},
		{
			name:     "11102 模型不可用 末端出口",
			status:   400,
			body:     `{"code":11102,"msg":"model [glm-4.6v] service info not found"}`,
			wantCode: "no_healthy_account",
			wantMsg: "all accounts unavailable (cooling/disabled): upstream model_blocked (http 400): " +
				`{"code":11102,"msg":"model [glm-4.6v] service info not found"}`,
			wantHint: "upstream has no such model on this backend; switch model or retry on another account",
		},
		{
			name:     "6004 软限流 末端出口",
			status:   429,
			body:     `{"code":6004,"msg":"您的使用量已超出频率限制","requestId":"r"}`,
			wantCode: "no_healthy_account",
			wantMsg: "all accounts unavailable (cooling/disabled): upstream soft_rate (http 429): " +
				`{"code":6004,"msg":"您的使用量已超出频率限制","requestId":"r"}`,
			wantHint: "rate limited by upstream; retry after reset",
		},
		{
			name:     "11140 账号级故障 末端出口",
			status:   403,
			body:     `{"code":11140,"msg":"request illegal"}`,
			wantCode: "no_healthy_account",
			wantMsg: "all accounts unavailable (cooling/disabled): upstream account_fault (http 403): " +
				`{"code":11140,"msg":"request illegal"}`,
			wantHint: "account-level fault at upstream (auth/quota state); the gateway will rotate or disable this account",
		},
		{
			name:     "12153 session 失效 末端出口",
			status:   401,
			body:     `{"code":12153,"msg":"Offline user session not found"}`,
			wantCode: "no_healthy_account",
			wantMsg: "all accounts unavailable (cooling/disabled): upstream session_dead (http 401): " +
				`{"code":12153,"msg":"Offline user session not found"}`,
			wantHint: "account session expired at upstream; the account is disabled until re-login",
		},
		{
			name:     "402 余额不足 末端出口",
			status:   402,
			body:     `{"code":1,"msg":"余额不足"}`,
			wantCode: "no_healthy_account",
			wantMsg: "all accounts unavailable (cooling/disabled): upstream hard_credit (http 402): " +
				`{"code":1,"msg":"余额不足"}`,
			wantHint: "account credits exhausted at upstream; waiting for daily check-in to restore",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				return c.status, c.body, false
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
			var e hintEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("resp not json: %v body=%s", err, rec.Body)
			}
			if e.Error.Code != c.wantCode {
				t.Errorf("code=%q want %q", e.Error.Code, c.wantCode)
			}
			if e.Error.Message != c.wantMsg {
				t.Errorf("message changed!\n got: %q\nwant: %q", e.Error.Message, c.wantMsg)
			}
			if e.Error.GatewayHint == nil || *e.Error.GatewayHint != c.wantHint {
				t.Errorf("gateway_hint=%v want %q", e.Error.GatewayHint, c.wantHint)
			}
		})
	}
}

// TestChatHintUncoveredNoField 未覆盖形态（ErrServer 5xx / 11101 bad_params）→
// error 对象**无 gateway_hint 键**（指针 nil，非空串）。message/状态码仍是既有形态。
func TestChatHintUncoveredNoField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx server error", 500, `{"code":500,"msg":"internal"}`},
		{"11101 bad params", 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`},
		{"404 not found", 404, `{"code":404,"msg":"not found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				return tc.status, tc.body, false
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
			var e hintEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("resp not json: %v", err)
			}
			if e.Error.GatewayHint != nil {
				t.Errorf("uncovered form must have NO gateway_hint field, got %q", *e.Error.GatewayHint)
			}
			// 未覆盖形态 message 也不得被 hint 逻辑污染：仍逐字含上游原文。
			if !strings.HasSuffix(e.Error.Message, tc.body) {
				t.Errorf("message must carry verbatim body: %q", e.Error.Message)
			}
		})
	}
}

// TestChatNoAccountHint 本地调度错误（空池）→ no_healthy_account hint 固定文案。
func TestChatNoAccountHint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, nil)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "no healthy account available in pool; check /status or retry later" {
		t.Errorf("gateway_hint=%v want no-healthy hint", e.Error.GatewayHint)
	}
	// message 仍是既有本地调度文案（本批不动）。
	if e.Error.Message != "all accounts unavailable (cooling/disabled)" {
		t.Errorf("message=%q want unchanged local scheduling text", e.Error.Message)
	}
}

// TestChatContentBlockedHintNoUpstreamWordNo11128 ErrContentBlocked 的 hint 口径：
// 措辞不含 "upstream"（与本 fork content_blocked 响应「不含上游字样」的既有口径
// 一致），且响应体**不得泄露上游 code 11128**（复用既有测试
// TestContentBlockedCustomModeDoesNotDegrade 的断言口径）。
func TestChatContentBlockedHintNoUpstreamWordNo11128(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptMode: "custom",
		PromptText: "SYS",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 content_blocked", rec.Code)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if e.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", e.Error.Code)
	}
	// 既有不泄露口径（逐字节）：响应体任何位置都不得出现 11128。
	if strings.Contains(rec.Body.String(), "11128") {
		t.Errorf("client body leaks upstream code 11128: %s", rec.Body.String())
	}
	// hint 必须存在，且措辞不含 "upstream"。
	if e.Error.GatewayHint == nil {
		t.Fatalf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}
	if *e.Error.GatewayHint != "request content was rejected by content policy; adjust the prompt and retry" {
		t.Errorf("gateway_hint=%q want content-policy hint", *e.Error.GatewayHint)
	}
	if strings.Contains(*e.Error.GatewayHint, "upstream") {
		t.Errorf("content_blocked hint must NOT contain 'upstream': %q", *e.Error.GatewayHint)
	}
}

// TestChatSoftRateHint 软限流末端（本 fork 恒 503）→ rate limit hint + message
// 逐字含上游原文。
func TestChatSoftRateHint(t *testing.T) {
	const raw = `{"code":6004,"msg":"您的使用量已超出频率限制","requestId":"r"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, raw, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	// 本 fork 末端恒 503（状态码映射是既有分歧，本批不动）。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (fork terminal status)", rec.Code)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.Error.Message, raw) {
		t.Errorf("message must carry verbatim upstream body: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v", e.Error.GatewayHint)
	}
}

// TestChatPromptTooLongHint 11115 → prompt too long hint（既有 code/message 语义
// 不变，只加字段）。
func TestChatPromptTooLongHint(t *testing.T) {
	const raw = `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, raw, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message != raw {
		t.Errorf("message must be verbatim: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "request context exceeds the model's limit; reduce history/message size" {
		t.Errorf("gateway_hint=%v", e.Error.GatewayHint)
	}
}

// TestChatSSErrorFrameHint 流式：上游 error 帧（6004）透传时附加 gateway_hint
// 字段，message 原文/干净帧/恰好一个 [DONE] 均不受影响。
func TestChatSSErrorFrameHint(t *testing.T) {
	const errFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"req-rl-42"}}`
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, raw, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 (mid-stream error frame still 200)", rec.Code)
	}
	body := rec.Body.String()
	// message 原文 + 既有字段 + 新增 hint。
	var frame map[string]any
	found := false
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.TrimPrefix(ln, "data: ") == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(ln[6:]), &obj) == nil {
			if e, ok := obj["error"].(map[string]any); ok {
				frame = e
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("error frame missing: %q", body)
	}
	if frame["message"] != "您的使用量已超出频率限制" || frame["code"] != "6004" || frame["requestId"] != "req-rl-42" {
		t.Errorf("error frame fields must be preserved: %v", frame)
	}
	if frame["gateway_hint"] != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v", frame["gateway_hint"])
	}
	if strings.Count(body, "data: [DONE]") != 1 || !strings.Contains(body, `"content":"hello"`) {
		t.Errorf("clean frames/[DONE] broken: %q", body)
	}
}

// TestChatSSEmptyStreamFrameNoHint 网关本地空流兜底帧是未覆盖形态 → 不附加
// gateway_hint（空流路径的 hintFn 判定恒空，不编造）。
func TestChatSSEmptyStreamFrameNoHint(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "", true // 空流
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "empty upstream stream") {
		t.Fatalf("empty-stream frame missing: %q", body)
	}
	if strings.Contains(body, "gateway_hint") {
		t.Errorf("empty-stream local frame must NOT carry gateway_hint: %q", body)
	}
}

// TestHasImagePart 请求体 image_url 探测的形态正/负例。
func TestHasImagePart(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"多模态 image_url part", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"t"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`, true},
		{"字符串 content", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"畸形 JSON", `not json`, false},
		{"空 body", ``, false},
		{"无 messages", `{"model":"m"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasImagePart([]byte(c.body)); got != c.want {
				t.Errorf("hasImagePart=%v want %v", got, c.want)
			}
		})
	}
}

// TestHintContextOnlyReadsCache hintContext 只读缓存快照、**不触发上游拉取**：
// 用记录请求路径的假上游，断言错误路径只出现 chat 端点、模型目录端点一次都没打。
func TestHintContextOnlyReadsCache(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	// 缓存置为「已过期」（fetched=过去时刻）：即便过期也不得触发 FetchModels
	// （错误路径不放大请求量——与 WAF IP fail-fast 的哲学一致）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "deepseek-v3-0324", SupportsImages: false}}
	dynamicModelsCache.fetched = time.Now().Add(-2 * dynamicModelsTTL)
	dynamicModelsCache.Unlock()

	var mu sync.Mutex
	var paths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11133Real, false
	})
	// 记录路径需要自定义 transport（fake 上游默认 transport 不暴露 URL）。
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body11133Real)),
		}, nil
	})}

	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	// 缓存已过期 → 不做「不支持图片」判定（宁缺勿滥，不编造能力事实）。
	if e.Error.GatewayHint == nil || strings.Contains(*e.Error.GatewayHint, "does not support images") {
		t.Errorf("expired cache must NOT claim supports_images fact: %v", e.Error.GatewayHint)
	}
	// 零模型目录拉取：所有上游请求都是 chat 端点。
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("expected at least one chat upstream call")
	}
	for _, p := range paths {
		if strings.Contains(p, "models") {
			t.Errorf("hint path must NOT fetch model catalog, got path %q (all=%v)", p, paths)
		}
	}
}

// TestHintContextHotCacheNoUpstreamFetch 缓存热（TTL 内）时同样零目录拉取，
// 且支持能力判定生效（正向：证明只读快照能给出真值，不是恒退中性）。
func TestHintContextHotCacheNoUpstreamFetch(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "deepseek-v3-0324", SupportsImages: false}})

	var mu sync.Mutex
	var paths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11133Real, false
	})
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body11133Real)),
		}, nil
	})}

	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "does not support images") {
		t.Errorf("hot cache should yield model-not-supports-images hint, got %v", e.Error.GatewayHint)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.Contains(p, "models") {
			t.Errorf("hint path must NOT fetch model catalog, got path %q (all=%v)", p, paths)
		}
	}
}
