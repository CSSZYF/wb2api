package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ptl_retry_test.go 11115「prompt is too long」按错误里的真实数字下调 max_tokens
// 重试一次的端到端验收（任务 B）。
//
// 背景（实测，**未 100% 证实**）：客户端 max_tokens=128000、输入约 95 万 token 的请求
// 被上游回 `{"code":11115,"msg":"prompt is too long: 1083265 tokens > 1048576 maximum"}`；
// 1083265-1048576=34689；同一会话在另一条（同内容、max_tokens=20000）请求里被上游计为
// 955192，1083265-955192=128073 ≈ max_tokens(128000)。强烈提示上游把 max_tokens 也算进
// 上下文上限检查。另有一次 max_tokens=1048576 + 极小输入返回 200（说明要么不计入、
// 要么 max_tokens 被截到模型上限后再判）——故本项是**安全兜底**：能算就下调重试一次，
// 算不出/不适用一律保持现状（透传原文、不轮转、不罚号）。
//
// 纪律（每条都有对应用例）：
//   - 只重试一次，且必须发生在**还没向客户端写出任何字节**的时刻（11115 是流式开始前
//     的 400，天然满足；守卫见 handler 注释）；
//   - 重试走与首次**完全相同**的准备管线（同一 body 变换、同一账号），只有 max_tokens
//     一个字段不同——不轮转账号、不记惩罚；
//   - 重试仍失败 → 客户端拿到**首次**的原始 11115 原文（报错信息与现在逐字一致）；
//   - 开关 features.ptl_max_tokens_retry 关闭 → 行为与现在完全一致。

// ptlRawUser 用户实测 11115 原文（逐字，含 requestId）。
const ptlRawUser = `{"code":11115,"msg":"prompt is too long: 1083265 tokens > 1048576 maximum","requestId":"2b9f0a4e-1c3d-4e5f-8a7b-6c5d4e3f2a1b"}`

// ptlReqBody 用户实测形态的请求体：max_tokens=128000 + 一条 user 消息。
const ptlReqBody = `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":128000,` +
	`"messages":[{"role":"user","content":"hi"}]}`

// attemptRecord 一次出站尝试的观测（Authorization + 请求体字节）。
type attemptRecord struct {
	authz string
	body  []byte
}

// newRecordingUpstream 假上游：记录每次出站的 Authorization 与请求体（读取顺序 = 调用
// 顺序），答复由 behavior(n, authz, body) 决定（n 从 1 起）。
func newRecordingUpstream(t *testing.T, behavior func(n int, authz string, body []byte) (int, string, bool)) (*upstream.Client, func() []attemptRecord) {
	t.Helper()
	var mu sync.Mutex
	var recs []attemptRecord
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			n := len(recs) + 1
			recs = append(recs, attemptRecord{authz: r.Header.Get("Authorization"), body: raw})
			mu.Unlock()
			status, body, isStream := behavior(n, r.Header.Get("Authorization"), raw)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return c, func() []attemptRecord {
		mu.Lock()
		defer mu.Unlock()
		out := make([]attemptRecord, len(recs))
		copy(out, recs)
		return out
	}
}

// ptlField 取请求体里的数字字段（不存在/非数字 → ok=false）。
func ptlField(t *testing.T, body []byte, key string) (float64, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("outbound body not json: %v (%s)", err, body)
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	f, isNum := v.(float64)
	if !isNum {
		t.Fatalf("field %s not a number: %T", key, v)
	}
	return f, true
}

// TestChatPromptTooLongDowngradesMaxTokensAndRetries 核心用例：可解析数字 + 有
// max_tokens → 下调后**同一账号**重试一次，第二次 200 → 客户端 200（流式照常）。
// 断言第二次请求体 max_tokens 正好等于期望值、其余字段与首次完全一致、账号无惩罚。
func TestChatPromptTooLongDowngradesMaxTokensAndRetries(t *testing.T) {
	const wantNew = 86911 // 128000 - 34689(overshoot) - 6400(max(512, 5%×128000))
	up, attempts := newRecordingUpstream(t, func(n int, authz string, body []byte) (int, string, bool) {
		if n == 1 {
			return 400, ptlRawUser, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ptlReqBody)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（下调后重试成功应照常返回流）body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("流式响应应完整透传: %s", rec.Body)
	}
	recs := attempts()
	if len(recs) != 2 {
		t.Fatalf("upstream calls=%d want 2（首次 + 一次下调重试）", len(recs))
	}
	// 同一账号：不因 11115 轮转（换号同样超限，白扔健康号配额）。
	if recs[0].authz != recs[1].authz {
		t.Errorf("重试必须用同一账号: %q vs %q", recs[0].authz, recs[1].authz)
	}
	if recs[0].authz != "Bearer at1" {
		t.Errorf("首次应打第一个账号: %q", recs[0].authz)
	}
	// 第二次请求体：只有 max_tokens 一个字段不同。
	first, second := recs[0].body, recs[1].body
	if got, _ := ptlField(t, second, "max_tokens"); got != wantNew {
		t.Errorf("第二次 max_tokens=%v want %d", got, wantNew)
	}
	if got, _ := ptlField(t, first, "max_tokens"); got != 128000 {
		t.Errorf("首次 max_tokens=%v want 128000（首次请求体不得被改写）", got)
	}
	var m1, m2 map[string]any
	if err := json.Unmarshal(first, &m1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &m2); err != nil {
		t.Fatal(err)
	}
	m1["max_tokens"] = float64(wantNew)
	if fmt.Sprint(m1) != fmt.Sprint(m2) {
		t.Errorf("除 max_tokens 外第二次请求体必须与首次完全一致:\n got=%v\nwant=%v", m2, m1)
	}
	// 逐字节口径（出站 body 由 json.Marshal 生成、键序确定）：把首次 body 里的
	// max_tokens 字面量替换成下调值后，必须与第二次 body **逐字节相同**——这是
	// 「同一准备管线、只差一个字段」的最强断言（任何其它字段/键序/空格的漂移都会红）。
	wantSecond := bytes.Replace(first, []byte(`"max_tokens":128000`), []byte(fmt.Sprintf(`"max_tokens":%d`, wantNew)), 1)
	if !bytes.Equal(second, wantSecond) {
		t.Errorf("第二次请求体必须等于「首次 body 只换 max_tokens 字面量」:\n got=%s\nwant=%s", second, wantSecond)
	}
	// 无惩罚：成功号清零，另一号未参与。
	for _, uid := range []string{"a1", "a2"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() || st.Cooling || st.Disabled ||
			st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("%s 11115 重试不得有任何惩罚: %+v", uid, st)
		}
	}
	if st, _ := p.Status("a2"); st.SuccessCount != 0 {
		t.Errorf("重试不得轮转账号（a2 不应被调用）: %+v", st)
	}
}

// TestChatPromptTooLongRetryStillTooLongReturnsFirstError 重试仍 11115 → 客户端拿到
// **首次**的原始原文（code/message/requestId 一致），调用次数 = 2，账号无惩罚。
func TestChatPromptTooLongRetryStillTooLongReturnsFirstError(t *testing.T) {
	up, attempts := newRecordingUpstream(t, func(n int, authz string, body []byte) (int, string, bool) {
		if n == 1 {
			return 400, ptlRawUser, false
		}
		// 第二次仍超限，但 requestId 不同（上游每次生成新 ID）——客户端必须看到首次那条。
		return 400, strings.Replace(ptlRawUser, "2b9f0a4e", "second-attempt-", 1), false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ptlReqBody)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "prompt_too_long" {
		t.Errorf("code=%q want prompt_too_long", e.Error.Code)
	}
	if e.Error.Message != ptlRawUser {
		t.Errorf("重试失败必须退回首次原文:\n got=%q\nwant=%q", e.Error.Message, ptlRawUser)
	}
	if !strings.Contains(e.Error.Message, "1083265 tokens > 1048576 maximum") {
		t.Errorf("首次原文的真实数字必须保留: %s", e.Error.Message)
	}
	if len(attempts()) != 2 {
		t.Errorf("calls=%d want 2（只重试一次）", len(attempts()))
	}
	st, _ := p.Status("a1")
	if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() || st.Cooling || st.ErrTotal != 0 {
		t.Errorf("重试失败也不得罚号: %+v", st)
	}
}

// TestChatPromptTooLongNoRetryCases 不重试的三种情形（调用次数必须 = 1，行为与现在
// 完全一致：400 + 原文透传）：
//   - message 解析不出数字；
//   - 解析出数字但请求体没有 max_tokens / maxOutputTokens；
//   - 下调后低于下限（1024）——「算不出/不适用就保持现状」的安全兜底。
func TestChatPromptTooLongNoRetryCases(t *testing.T) {
	cases := []struct {
		name string
		body string
		raw  string
	}{
		{
			name: "message 无数字",
			body: ptlReqBody,
			raw:  `{"code":11115,"msg":"prompt is too long","requestId":"r-1"}`,
		},
		{
			name: "n 未超过上限",
			body: ptlReqBody,
			raw:  `{"code":11115,"msg":"prompt is too long: 1048576 tokens > 1048576 maximum","requestId":"r-2"}`,
		},
		{
			name: "请求体无 max_tokens",
			body: `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			raw:  ptlRawUser,
		},
		{
			name: "下调后低于下限",
			// overshoot = 1050000-1048576 = 1424；max_tokens=2000 → 2000-1424-512 < 1024。
			body: `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":2000,"messages":[{"role":"user","content":"hi"}]}`,
			raw:  `{"code":11115,"msg":"prompt is too long: 1050000 tokens > 1048576 maximum","requestId":"r-3"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, attempts := newRecordingUpstream(t, func(n int, authz string, body []byte) (int, string, bool) {
				return 400, c.raw, false
			})
			p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
			}
			var e struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("resp not json: %v body=%s", err, rec.Body)
			}
			if e.Error.Code != "prompt_too_long" || e.Error.Message != c.raw {
				t.Errorf("行为应与现在完全一致（400 + 原文）: code=%q msg=%q", e.Error.Code, e.Error.Message)
			}
			if got := len(attempts()); got != 1 {
				t.Errorf("calls=%d want 1（不适用则绝不重试）", got)
			}
		})
	}
}

// TestChatPromptTooLongRetryCamelCaseField maxOutputTokens 形态同样支持（上游/客户端
// 两个键名都认）；重试时只改这一个键。
func TestChatPromptTooLongRetryCamelCaseField(t *testing.T) {
	const body = `{"model":"deepseek-v4.1-flash","stream":true,"maxOutputTokens":128000,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	up, attempts := newRecordingUpstream(t, func(n int, authz string, body []byte) (int, string, bool) {
		if n == 1 {
			return 400, ptlRawUser, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	recs := attempts()
	if len(recs) != 2 {
		t.Fatalf("calls=%d want 2", len(recs))
	}
	if got, _ := ptlField(t, recs[1].body, "maxOutputTokens"); got != 86911 {
		t.Errorf("maxOutputTokens=%v want 86911", got)
	}
	if _, has := ptlField(t, recs[1].body, "max_tokens"); has {
		t.Error("不得凭空新增 max_tokens 键")
	}
}

// TestChatPromptTooLongRetryDisabledByConfig 开关关闭（显式 false）→ 行为与现在
// 完全一致：调用次数 = 1、400 + 首次原文透传、不罚号。
func TestChatPromptTooLongRetryDisabledByConfig(t *testing.T) {
	off := false
	up, attempts := newRecordingUpstream(t, func(n int, authz string, body []byte) (int, string, bool) {
		if n == 1 {
			return 400, ptlRawUser, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PTLMaxTokensRetry: &off})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ptlReqBody)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message != ptlRawUser {
		t.Errorf("开关关闭时 message 必须是原文: %q", e.Error.Message)
	}
	if got := len(attempts()); got != 1 {
		t.Errorf("calls=%d want 1（开关关闭不重试）", got)
	}
	st, _ := p.Status("a1")
	if st.ConsecutiveFails != 0 || st.Cooling {
		t.Errorf("11115 无论如何不罚号: %+v", st)
	}
}

// TestPTLRetryGuardNoBytesWritten 写出守卫（单元级）：respWriteTracker 必须在
// WriteHeader/Write 任一发生后报告 wrote()=true——11115 重试要求「还没向客户端写出
// 任何字节」，守卫失效会让「头已发出（如 200）却想改错误响应」成为可能。
// 同时锁住 Flush 透传与 Unwrap 暴露（SSE 逐帧 flush 与 ResponseController 的
// SetReadDeadline 都依赖它们，本包装不得把这两个能力吃掉）。
func TestPTLRetryGuardNoBytesWritten(t *testing.T) {
	rec := httptest.NewRecorder()
	wt := &respWriteTracker{ResponseWriter: rec}
	if wt.wrote() {
		t.Fatal("未写出任何字节时应为 false")
	}
	// Flush 透传：httptest.ResponseRecorder 实现 Flusher，Flush 本身不算「写出 body」。
	wt.Flush()
	if wt.wrote() {
		t.Error("仅 Flush 不应视为已写出（SSE 场景下 header 尚未定型的边界）")
	}
	if wt.Unwrap() == nil {
		t.Error("Unwrap 必须暴露底层 writer（ResponseController 靠它找 SetReadDeadline）")
	}
	// 只写头（无 body）也算已写出：状态码一旦定下就不能再改成错误响应。
	wt.WriteHeader(http.StatusOK)
	if !wt.wrote() {
		t.Error("WriteHeader 后必须报告已写出")
	}
	// 只写 body（隐式 200）同样算。
	rec2 := httptest.NewRecorder()
	wt2 := &respWriteTracker{ResponseWriter: rec2}
	if _, err := wt2.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if !wt2.wrote() {
		t.Error("Write 后必须报告已写出")
	}
	// 空 body 写入（len=0）也走隐式 WriteHeader(200)——标准库语义，视为已写出。
	rec3 := httptest.NewRecorder()
	wt3 := &respWriteTracker{ResponseWriter: rec3}
	if _, err := wt3.Write(nil); err != nil {
		t.Fatal(err)
	}
	if !wt3.wrote() {
		t.Error("空 Write 也会定下状态码（隐式 200），必须报告已写出")
	}
}

// TestSetPTLMaxTokensRetryHot 热改开关（面板保存路径）：SetPTLMaxTokensRetry(false)
// 后下一个请求即不重试；改回 true 即恢复。与 features 的其它开关同口径（保存即生效）。
func TestSetPTLMaxTokensRetryHot(t *testing.T) {
	up, attempts := newRecordingUpstream(t, func(n int, authz string, body []byte) (int, string, bool) {
		// 语义忠实：只要请求体还带着原始 max_tokens=128000 就回 11115，下调后才 200
		// ——这样「关闭时 1 次、开启时 2 次」两个分支各自可断言，不受调用序号影响。
		if bytes.Contains(body, []byte(`"max_tokens":128000`)) {
			return 400, ptlRawUser, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	if !h.ptlRetryEnabled() {
		t.Fatal("默认应为开（Config.PTLMaxTokensRetry=nil → true）")
	}
	h.SetPTLMaxTokensRetry(false)
	if h.ptlRetryEnabled() {
		t.Fatal("SetPTLMaxTokensRetry(false) 后应关")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ptlReqBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（关闭后不重试）body=%s", rec.Code, rec.Body)
	}
	if got := len(attempts()); got != 1 {
		t.Errorf("calls=%d want 1（热关闭后不重试）", got)
	}
	h.SetPTLMaxTokensRetry(true)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ptlReqBody)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（热开启后重试成功）body=%s", rec2.Code, rec2.Body)
	}
	if got := len(attempts()); got != 3 {
		t.Errorf("calls=%d want 3（首次 1 + 下调重试 1）", got)
	}
}
