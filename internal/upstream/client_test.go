package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{403, `credits exhausted`, ErrHardCredit},
		{200, `{"code":1,"msg":"credits exhausted, please top up"}`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 429 + 余额措辞 → 限流语义（fork-scan-absorb T-3，本次修复点）：限流响应
		// body 高频携带 "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，
		// hardMarkers 在 429 之前会误判 ErrHardCredit 硬冷却到次日 04:00，白扔号约 12h。
		// 状态码是比关键词更权威的信号：真余额耗尽走 402，非 429 的 quota 措辞
		// 仍归 hardMarkers（上方 {200,"quota exceeded"} 语义不变）。
		{429, `quota exceeded`, ErrSoftRate},
		{429, `{"code":1,"msg":"quota exceeded, please wait"}`, ErrSoftRate},
		{429, `insufficient credits`, ErrSoftRate},
		{429, `{"code":1,"msg":"额度不足"}`, ErrSoftRate},
		{429, `积分不足，请充值`, ErrSoftRate},
		// 429 + 账号级故障码防回归（accountFault 仍先于 429 判定）：429+14017 若
		// 落到 status==429 会误归 soft_rate，账号级故障等不来自愈。
		{429, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
		{429, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 内容策略拦截（HTTP 400 + 审核文案）：误报信号，不罚账号，走降级重试。
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// 通用 4xx（非审核文案）：仍判 ErrClient，只换号不罚。
		{400, `bad request`, ErrClient},
		// ErrBadParams：请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）。
		// 这是"发给上游的 body 有问题"（网关截断已由 413 消灭，剩余为客户端畸形 JSON），
		// 换了账号也一样 400，不罚号。具体词优先于通用 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		{200, `quota exceeded`, ErrHardCredit},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestClassifyRateLimitWithQuotaIsSoft 429 + quota/额度措辞 → ErrSoftRate（fork-scan-absorb T-3）。
// 修复前 hardMarkers 先于 status==429 判定，429 body 高频携带的 "quota exceeded"/"额度不足"
// 会被误归 ErrHardCredit → CooldownUntilTomorrow4AM 硬冷却到次日 04:00（白扔号约 12h），
// 多号同因被推即"全池一起熔断"。状态码是比关键词更权威的信号：真余额耗尽走 402，
// 非 429 的 quota 措辞仍归 hardMarkers（见 TestClassify 的 {200,"quota exceeded"}）。
func TestClassifyRateLimitWithQuotaIsSoft(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"纯文本 quota exceeded", `quota exceeded`},
		{"信封 msg quota exceeded", `{"code":1,"msg":"quota exceeded, please wait"}`},
		{"insufficient credits", `insufficient credits`},
		{"中文 额度不足", `{"code":1,"msg":"额度不足"}`},
		{"中文 积分不足", `积分不足，请充值`},
		{"quota exhaust", `{"code":1,"msg":"quota exhaust"}`},
	}
	for _, c := range cases {
		if got := Classify(429, c.body); got != ErrSoftRate {
			t.Errorf("%s: Classify(429,%q)=%v want ErrSoftRate（429 限流语义优先于计费措辞）", c.name, c.body, got)
		}
	}
	// 非 429 的同一措辞仍归 hardMarkers（历史语义不变，只有 429 前置接管）。
	if got := Classify(200, `quota exceeded`); got != ErrHardCredit {
		t.Errorf("Classify(200, quota exceeded)=%v want ErrHardCredit（非 429 语义不变）", got)
	}
	if got := Classify(403, `insufficient credits`); got != ErrHardCredit {
		t.Errorf("Classify(403, insufficient credits)=%v want ErrHardCredit（非 429 语义不变）", got)
	}
}

// TestClassifyCreditsExhaustedPlural 余额不足词表补英文复数 "credits exhausted"
// （对齐上游口径）：漏判会落到 4xx → ErrClient 只换号不硬冷却，坏号留在池内反复刷计费失败。
func TestClassifyCreditsExhaustedPlural(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{403, `credits exhausted`},
		{200, `{"code":1,"msg":"credits exhausted, please top up"}`},
		{400, `Credits Exhausted`},
	} {
		if got := Classify(tc.status, tc.body); got != ErrHardCredit {
			t.Errorf("Classify(%d,%q)=%v want ErrHardCredit（复数 credits exhausted 已收录）", tc.status, tc.body, got)
		}
	}
}

// TestIsModelRateLimit 判断 429 body 是否明确指向模型级限流（code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// 6004：模型级限流（issue #31 的核心场景）。
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// 其他 code（非模型级限流）→ 不算。
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestParseRateReset 统一解析任意限流响应（6004 **和** 非 6004，如 11140 rate-limiting）
// msg 里的「将在 … 重置」时间（UTC+8）。旧语义（非 6004 带时间 → false）是有意推翻的：
// 11140 的 rate-limiting 变体带重置时间时同样应被精确对齐到上游重置墙钟。
func TestParseRateReset(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"6004 带时间+UTC+8 后缀", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 带时间无后缀", `{"code":6004,"msg":"将在 ` + ts + ` 重置"}`, true},
		{"11140 rate-limiting 带时间(账号级也应对齐)", `{"code":11140,"msg":"The model provider is rate-limiting requests. 将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 无时间文案", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"非法时间格式", `{"code":6004,"msg":"将在 明天 重置"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				// 解析结果 = ts 在 UTC+8 解释下的墙钟（截断到分钟），应与 future 相差 ±2 分钟。
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if got.Location() != time.UTC {
					// 不同指针的 FixedZone 实例相等性按 offset 判，这里只断言 offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

// TestParseSoftRateResetAlias 旧函数名保留为兼容别名：与 ParseRateReset 等价
// （含「非 6004 带时间 → true」的新语义，不再被 6004 门禁）。
func TestParseSoftRateResetAlias(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	body := `{"code":11140,"msg":"The model provider is rate-limiting requests. 将在 ` + ts + ` UTC+8 重置"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatalf("alias should parse non-6004 body with reset time (body=%s)", body)
	}
	want, ok2 := ParseRateReset(body)
	if !ok2 || !got.Equal(want) {
		t.Errorf("alias=%v want %v（与 ParseRateReset 等价）", got, want)
	}
}

// TestClassifyPromptTooLong 11115「prompt is too long」三形态分类：
// code 数字 / code 字符串 / msg 文案，均归 ErrPromptTooLong；只认 400/404/413。
func TestClassifyPromptTooLong(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"400 code 数字", 400, `{"code":11115,"msg":"prompt is too long"}`, ErrPromptTooLong},
		{"400 code 字符串", 400, `{"code":"11115","msg":"x"}`, ErrPromptTooLong},
		{"400 msg 文案", 400, `{"code":1,"msg":"prompt is too long: 120000 tokens > 65536 maximum"}`, ErrPromptTooLong},
		{"404 code 数字（不落 ErrNotFound）", 404, `{"code":11115,"msg":"prompt is too long"}`, ErrPromptTooLong},
		{"413 code 数字", 413, `{"code":11115,"msg":"prompt is too long"}`, ErrPromptTooLong},
		{"400 大小写不敏感", 400, `Prompt Is Too Long`, ErrPromptTooLong},
		// 状态码门禁：429 限流语义优先、5xx 服务端故障优先（只认请求级 4xx）。
		{"429 带 11115 仍限流", 429, `{"code":11115,"msg":"prompt is too long"}`, ErrSoftRate},
		{"500 带 11115 仍 5xx", 500, `{"code":11115,"msg":"prompt is too long"}`, ErrServer},
		// 撞 requestId 不算（只认 code 字段形态与 msg 文案）。
		{"400 requestId 含 11115 不算", 400, `{"requestId":"11115","msg":"ok"}`, ErrClient},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d,%q)=%v want %v", c.name, c.status, c.body, got, c.want)
		}
	}
	// IsPromptTooLong 与 Classify 同口径（handler 末端透传分支用）。
	if !IsPromptTooLong(400, `{"code":11115,"msg":"prompt is too long"}`) {
		t.Error("IsPromptTooLong(400, 11115) should be true")
	}
	if IsPromptTooLong(429, `{"code":11115,"msg":"prompt is too long"}`) {
		t.Error("IsPromptTooLong must reject 429 (rate-limit semantics first)")
	}
	if IsPromptTooLong(400, `{"code":1,"msg":"ok"}`) {
		t.Error("IsPromptTooLong must reject bodies without 11115 markers")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

// TestRefreshTokenExpiresInCap expiresIn 量级上限：上游脏值（如 99999999999 秒 ≈ 3170 年）
// 不得把 ExpiresAt 推到荒谬未来（NeedsRefresh 永假 → token 永不刷新反而真过期失效）。
// 上限 10 年（上游实测响应恒 expiresIn=5184000=60d，10 年是纯防御量级）。
// 超限按脏值处理：保留旧 ExpiresAt（与缺省分支同语义），token 本身仍写回。
func TestRefreshTokenExpiresInCap(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":99999999999}}`), nil
	})
	oldExpiry := time.Now().Add(time.Hour).Unix()
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: oldExpiry}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != oldExpiry {
		t.Errorf("脏 expiresIn 应保留旧 ExpiresAt=%d, got %d（被推到荒谬未来）", oldExpiry, a.ExpiresAt)
	}
	// token 本身仍应写回（脏 expiresIn 只否决过期时间，不否决凭证）。
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

// TestRefreshTokenExpiresInWithinCapApplied 正常量级（60d，上游恒 5184000）不受上限
// 影响：ExpiresAt 照常推进。
func TestRefreshTokenExpiresInWithinCapApplied(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	before := time.Now().Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := before + 5184000
	if a.ExpiresAt < want-2 || a.ExpiresAt > want+2 {
		t.Errorf("ExpiresAt=%d want ~%d (60d 正常推进)", a.ExpiresAt, want)
	}
}

// TestRefreshTokenExpiresInOverflowRejected 溢出窗口守卫（本仓加强版，**不**照抄上游
// `time.Duration(tok.ExpiresIn)*time.Second < max` 的写法）：极大 expiresIn 让 Duration
// 乘法溢出成负数，上游写法会判「在上限内」而写回**过去时刻**（NeedsRefresh 恒真 →
// 刷新风暴）。本仓用 int64 秒比较，无溢出路径。
func TestRefreshTokenExpiresInOverflowRejected(t *testing.T) {
	// 1<<63-1 纳秒 ≈ 292 年；乘 time.Second 必溢出。这里取 maxInt64/1e9*2 量级。
	const huge = int64(1) << 62 // 4.6e18 秒，Duration 乘法必溢出为负
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, fmt.Sprintf(`{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":%d}}`, huge)), nil
	})
	oldExpiry := time.Now().Add(time.Hour).Unix()
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: oldExpiry}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != oldExpiry {
		t.Errorf("溢出 expiresIn 必须保留旧 ExpiresAt=%d, got %d（写回过去时刻会引发刷新风暴）", oldExpiry, a.ExpiresAt)
	}
	if a.ExpiresAt < time.Now().Unix() {
		t.Errorf("ExpiresAt=%d 落在过去时刻（刷新风暴）", a.ExpiresAt)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "WorkBuddy" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts，DefaultEffort 应携带 reasoning.effort
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}
	if infos[0].DefaultEffort != "high" {
		t.Errorf("infos[0].DefaultEffort=%q want high", infos[0].DefaultEffort)
	}
	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("hard credit should return body via status, not err: %v", err)
	}
	// caller classifies via returned body
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, total, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if total != 3000 {
		t.Errorf("total=%d want 3000", total)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, total, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
	if total != 100 {
		t.Errorf("total=%d want 100", total)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// 恒 CN：domain 不同不改变上游 host。
	if c.chatBase(other) != c.chatBase(cn) || c.billingBase(other) != c.billingBase(cn) {
		t.Error("bases must be CN regardless of domain")
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 120s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

func TestFetchModelsDefaultEffortDualKeyAndSizes(t *testing.T) {
	// 上游双键：老模型 reasoning.effort（auto），新模型（glm-5.3 系）只有 reasoning.defaultEffort；
	// credits/maxAllowedSize/canDisableThinking/supportsReasoning 等尺寸与能力字段应一并透出。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-5.3","maxInputTokens":1000000,"maxOutputTokens":48000,"maxAllowedSize":1000000,"credits":"x0.79","supportsReasoning":true,"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}},
			{"id":"auto","maxInputTokens":168000,"maxOutputTokens":32000,"supportsReasoning":true,"reasoning":{"effort":"high"}}
		],"agents":[{"name":"cli","models":["glm-5.3","auto"]}]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	g := byID["glm-5.3"]
	if g.DefaultEffort != "high" {
		t.Errorf("glm-5.3 DefaultEffort=%q want high (from defaultEffort key)", g.DefaultEffort)
	}
	if !g.CanDisableThinking || !g.SupportsReasoning {
		t.Errorf("glm-5.3 capability flags: canDisable=%v supportsReasoning=%v want true/true", g.CanDisableThinking, g.SupportsReasoning)
	}
	if g.MaxAllowedSize != 1000000 || g.MaxTokens != 48000 || g.Credits != "x0.79" {
		t.Errorf("glm-5.3 sizes: maxAllowed=%d maxOut=%d credits=%q", g.MaxAllowedSize, g.MaxTokens, g.Credits)
	}
	if au := byID["auto"]; au.DefaultEffort != "high" {
		t.Errorf("auto DefaultEffort=%q want high (from legacy effort key)", au.DefaultEffort)
	}
}

func TestFetchModelsOverlaysV3ConfigCapabilities(t *testing.T) {
	// CLI 目录给 flash 精简字段（128K / 固定 high）；IDE /v3/config 给完整能力。
	var sawIDE bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03 credits","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"effort":"high","summary":"auto"}}
			],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			sawIDE = true
			if r.Header.Get("User-Agent") != codeBuddyIDEUA {
				t.Errorf("v3/config UA=%q want %s", r.Header.Get("User-Agent"), codeBuddyIDEUA)
			}
			if r.Header.Get("X-Product") != "SaaS" {
				t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
			}
			if r.Header.Get("X-User-Id") != "u1" {
				t.Errorf("X-User-Id=%q want u1", r.Header.Get("X-User-Id"))
			}
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":393216,"credits":"x0.03","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"canDisableThinking":true,"defaultEffort":"high","summary":"auto","supportedEfforts":["low","high","max"]}}
			]}}`), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return jsonResp(404, `{}`), nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if !sawIDE {
		t.Fatal("expected /v3/config request")
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	mi := infos[0]
	if mi.MaxTokens != 393216 {
		t.Errorf("MaxTokens=%d want 393216", mi.MaxTokens)
	}
	if mi.ContextWindow != 1000000 {
		t.Errorf("ContextWindow=%d want 1000000", mi.ContextWindow)
	}
	if !mi.CanDisableThinking || !mi.SupportsReasoning {
		t.Errorf("flags canDisable=%v supportsReasoning=%v", mi.CanDisableThinking, mi.SupportsReasoning)
	}
	if mi.DefaultEffort != "high" {
		t.Errorf("DefaultEffort=%q want high", mi.DefaultEffort)
	}
	if got := strings.Join(mi.Efforts, ","); got != "low,high,max" {
		t.Errorf("Efforts=%v want low,high,max", mi.Efforts)
	}
}

func TestMergeModelCapabilitiesKeepsCLIWhenOverlayEmpty(t *testing.T) {
	base := []ModelInfo{{ID: "m", MaxTokens: 128000, DefaultEffort: "high"}}
	got := mergeModelCapabilities(base, map[string]ModelInfo{"m": {ID: "m"}})
	if got[0].MaxTokens != 128000 || got[0].DefaultEffort != "high" {
		t.Errorf("empty overlay wiped CLI fields: %+v", got[0])
	}
}
