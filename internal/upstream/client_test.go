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
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
		// 11102「该后端无此模型」：确定性答复，归 ErrModelBlocked（(账号,模型) 负缓存避让）。
		{404, `{"code":11102,"msg":"model [deepseek-v3-2-volc] service info not found"}`, ErrModelBlocked},
		{400, `{"error":{"code":"11102","message":"model service info not found"}}`, ErrModelBlocked},
		{400, `{"msg":"service info not found"}`, ErrModelBlocked},
		// 11102 撞在 requestId 上不算（不得误避让可用模型）。
		{404, `{"requestId":"11102","msg":"ok"}`, ErrNotFound},
		// 429 + 11102 → 限流语义（ErrSoftRate），不是模型不存在。
		{429, `{"code":11102,"msg":"service info not found"}`, ErrSoftRate},
		// WAF 403（P0-1）：403 + 无业务信封（无 "code":/"msg": 字段）→ ErrWafBlock。
		// 空体 / HTML 拦截页 / 纯文本 / 非信封 JSON 均命中。
		{403, ``, ErrWafBlock},
		{403, `<html><body>403 Forbidden</body></html>`, ErrWafBlock},
		{403, `Forbidden`, ErrWafBlock},
		{403, `{"message":"blocked by waf"}`, ErrWafBlock},
		{403, `<head><script>...</script></head><body>blocked</body>`, ErrWafBlock},
		// 403 带业务信封的仍走既有分类（P0-1 约束：不劫持业务 403）。
		{403, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{403, `{"code":60001,"msg":"quota exceeded"}`, ErrHardCredit},
		{403, `{"code":1,"msg":"unknown business error"}`, ErrClient},
		// 非 403 的无信封错误体不进 WAF 分类（WAF 判定绑定 403 形态）。
		{400, `bad request`, ErrClient},
		{429, ``, ErrSoftRate},
		// 上游英文 6004 原句（"usage exceeds frequency limit, …"）：非 429 状态码下
		// 既无 "rate limit" 也无 "usage limit" 子串，靠 "frequency limit" 词条命中，
		// 须归 ErrSoftRate（否则只换号不冷却，坏号留在池内反复被选中）。
		{400, `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-18 09:31:32 UTC+8"}`, ErrSoftRate},
		{200, `usage exceeds frequency limit`, ErrSoftRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsModelBlocked 11102「该后端无此模型」判定：只认 code 精确等于 11102 或 msg 命中
// 窄短语 "service info not found"，且仅在 400/404 下判。覆盖「11102 撞在 requestId 上」
// 的坑——requestId 里的 11102 不得误判。
func TestIsModelBlocked(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		// 顶层 code 字段。
		{404, `{"code":11102,"msg":"model [x] service info not found"}`, true},
		// error 子对象 code 字段（OpenAI 信封形态）。
		{400, `{"error":{"code":"11102","message":"model service info not found"}}`, true},
		// msg 短语命中（无 code 字段）。
		{400, `{"msg":"model service info not found"}`, true},
		// 11102 撞在 requestId 上不算（防误避让可用模型）。
		{404, `{"requestId":"11102","code":0,"msg":"ok"}`, false},
		{400, `{"requestId":"11102","msg":"boom"}`, false},
		// 429 带 11102 属限流语义，不算模型不存在。
		{429, `{"code":11102,"msg":"service info not found"}`, false},
		// 非 400/404 不算。
		{500, `{"code":11102,"msg":"service info not found"}`, false},
		// code 非 11102 且无短语 → 不算。
		{404, `{"code":11103,"msg":"x"}`, false},
		// 空 body 不算。
		{404, ``, false},
	}
	for _, c := range cases {
		if got := IsModelBlocked(c.status, c.body); got != c.want {
			t.Errorf("IsModelBlocked(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsWafBlocked WAF 403 形态判定的直接回归（Classify 的 WAF 层）：
// 只认 403 + 无业务信封；带信封/其他状态码一律 false。
func TestIsWafBlocked(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{403, "", true},
		{403, "<html>blocked</html>", true},
		{403, `{"code":1}`, false},                // 有 "code": 字段
		{403, `{"msg":"request illegal"}`, false}, // 有 "msg": 字段（且该文案本就该走 accountFault）
		{402, "", false},                          // 非 403
		{429, "", false},
		{500, "<html>gateway</html>", false},
	}
	for _, c := range cases {
		if got := IsWafBlocked(c.status, c.body); got != c.want {
			t.Errorf("IsWafBlocked(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestParseRetryAfter Retry-After / retry-after-ms / x-ratelimit-reset 头解析
// （有效/缺失/非法三形态 + 2h 封顶）。语义对齐 intl CLI parseRetryAfterMs /
// parseRateLimitResetMs（头族与数字口径）。
func TestParseRetryAfter(t *testing.T) {
	t.Run("retry-after seconds", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "30")
		if d := ParseRetryAfter(h); d != 30*time.Second {
			t.Fatalf("ParseRetryAfter(30)=%v want 30s", d)
		}
	})
	t.Run("retry-after-ms", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After-Ms", "1500")
		if d := ParseRetryAfter(h); d != 1500*time.Millisecond {
			t.Fatalf("ParseRetryAfter(1500ms)=%v want 1.5s", d)
		}
	})
	t.Run("x-ratelimit-reset epoch seconds", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(90*time.Second).Unix()))
		d := ParseRetryAfter(h)
		if d < 80*time.Second || d > 100*time.Second {
			t.Fatalf("ParseRetryAfter(epoch+90s)=%v want ~90s", d)
		}
	})
	t.Run("x-ratelimit-reset epoch millis", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(45*time.Second).UnixMilli()))
		d := ParseRetryAfter(h)
		if d < 35*time.Second || d > 55*time.Second {
			t.Fatalf("ParseRetryAfter(epochMilli+45s)=%v want ~45s", d)
		}
	})
	t.Run("missing headers", func(t *testing.T) {
		if d := ParseRetryAfter(http.Header{}); d != 0 {
			t.Fatalf("missing headers must return 0, got %v", d)
		}
	})
	t.Run("invalid values", func(t *testing.T) {
		for _, v := range []string{"abc", "", "-5", "1.5", "0"} {
			h := http.Header{}
			h.Set("Retry-After", v)
			if d := ParseRetryAfter(h); d != 0 {
				t.Errorf("Retry-After=%q must be rejected, got %v", v, d)
			}
		}
	})
	t.Run("oversized sanity cap", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "999999") // > retryAfterSanity(2h)
		if d := ParseRetryAfter(h); d != 0 {
			t.Errorf("oversized Retry-After must fall back, got %v", d)
		}
	})
	t.Run("expired reset epoch", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(-time.Minute).Unix()))
		if d := ParseRetryAfter(h); d != 0 {
			t.Errorf("expired reset must be rejected, got %v", d)
		}
	})
	t.Run("priority retry-after first", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "10")
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(300*time.Second).Unix()))
		if d := ParseRetryAfter(h); d != 10*time.Second {
			t.Fatalf("Retry-After must take priority, got %v", d)
		}
	})
}

// TestChatStreamErrorCarriesRetryAfter 端到端：上游 429 带 Retry-After 头时，
// ChatStreamContext 返回的 *Error 信封携带解析后的 RetryAfter（P1-2 挂载点验收）。
func TestChatStreamErrorCarriesRetryAfter(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		resp := jsonResp(429, `{"code":1,"msg":"rate limit"}`)
		resp.Header.Set("Retry-After", "77")
		return resp, nil
	})
	_, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %v", err)
	}
	if ue.Kind != ErrSoftRate {
		t.Fatalf("kind=%v want soft_rate", ue.Kind)
	}
	if ue.RetryAfter != 77*time.Second {
		t.Fatalf("RetryAfter=%v want 77s", ue.RetryAfter)
	}
}

// TestChatStreamErrorRetryAfterAbsent 头缺失时 RetryAfter 零值（回落调用方计算）。
func TestChatStreamErrorRetryAfterAbsent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `{"code":1,"msg":"rate limit"}`), nil
	})
	_, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrSoftRate {
		t.Fatalf("want *Error{soft_rate}, got %v", err)
	}
	if ue.RetryAfter != 0 {
		t.Fatalf("RetryAfter=%v want 0 (absent header)", ue.RetryAfter)
	}
}

// TestChatStreamWafBlockErrorCarriesKind 端到端：上游 403 空体（WAF 拦截形态）
// 经 ChatStreamContext 返回 ErrWafBlock 分类信封（P0-1 分类一次成型）。
func TestChatStreamWafBlockErrorCarriesKind(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(403, ``), nil
	})
	_, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), "", ChatMeta{})
	if status != 403 {
		t.Errorf("status=%d want 403", status)
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrWafBlock {
		t.Fatalf("403 empty body should return *Error{waf_block}, got %v", err)
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
		// 国际版英文 body：code 仍是数字 6004，判定与文案语言无关（不得因改英文漏判，
		// 漏判会把模型级限流按账号级冷却，切模型也无法豁免）。
		{`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-18 09:31:32 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"abc"}`, true},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestParseRateReset 统一解析任意限流响应（6004 **和** 非 6004，如 11140 rate-limiting）
// msg 里的重置时间（UTC+8）。旧语义（非 6004 带时间 → false）是有意推翻的：
// 11140 的 rate-limiting 变体带重置时间时同样应被精确对齐到上游重置墙钟。
//
// 双形态（CN 中文 / 国际版英文）都要能解析：英文文案漏解析 → 6004 走「无重置时间」
// 退避分支（600s 基数 + 账号级冷却），模型级豁免丢失、冷却时长也不对齐上游。
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
		// 国际版英文形态（上游实测原文）：时间后有 " UTC+8," 逗号 + alternatively 说明文本。
		{"英文形态 带 UTC+8 后缀与尾随逗号", `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at ` + ts + ` UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"x"}`, true},
		// 英文形态无 UTC+8 后缀（时间后直接逗号或句号结尾）。
		{"英文形态 无 UTC+8 后缀(逗号分隔)", `{"code":6004,"msg":"your usage will reset at ` + ts + `, please wait."}`, true},
		{"英文形态 无 UTC+8 后缀(句号结尾)", `{"code":6004,"msg":"your usage will reset at ` + ts + `."}`, true},
		// 非法英文：正则命中但时间串不可解析 → false（绝不臆造时间，退回有界退避）。
		{"英文形态 非法时间", `{"code":6004,"msg":"your usage will reset at tomorrow"}`, false},
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

// TestParseRateResetEnglishRealBody 回归真实英文 body（一字不改的上游原文）：
// 修复前纯中文正则 MatchString=false → 6004 落到「无重置时间」退避分支（600s 基数
// + 账号级冷却），模型级豁免丢失。这里锁定：解析出的墙钟精确等于文案中的时间
// （2026-09-18 09:31:32 UTC+8），且 IsModelRateLimit 同时命中（走模型级冷却）。
func TestParseRateResetEnglishRealBody(t *testing.T) {
	body := `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-18 09:31:32 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"req-1"}`
	got, ok := ParseRateReset(body)
	if !ok {
		t.Fatalf("英文文案必须解析出重置时间（修复前纯中文正则漏判）: body=%s", body)
	}
	want := time.Date(2026, 9, 18, 9, 31, 32, 0, softRateResetLoc)
	if !got.Equal(want) {
		t.Errorf("parsed=%v want %v", got, want)
	}
	if _, off := got.Zone(); off != 8*60*60 {
		t.Errorf("zone offset=%d want +08:00（固定 UTC+8，与容器时区无关）", off)
	}
	if !IsModelRateLimit(body) {
		t.Error("英文 body 的 code 6004 必须命中 IsModelRateLimit（否则走账号级冷却）")
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
	// 错误路径返回已分类的 *Error 信封（WAF 403 修复后的新契约：分类一次成型，
	// 消除 upstream/handler 双次 Classify 的漂移面），同时 respBody 原样返回
	// （错误透传语义不变，调用方仍可读原文）。
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrHardCredit {
		t.Fatalf("hard credit should return classified *Error, got %v", err)
	}
	if ue.Status != 402 {
		t.Errorf("envelope status=%d want 402", ue.Status)
	}
	// caller can still classify via returned body（respBody 原样保留）
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

// resourceStub 返回一个 get-user-resource 响应，body 为 Accounts 数组内容。
func resourceStub(accounts string) *Client {
	return testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[`+accounts+`]}}}}`), nil
	})
}

// TestUserResourceDetailedExpiringFromCycleEndTime 锁定快过期分桶的判据字段是
// CycleEndTime（上游 get-user-resource 响应字段全集实测无 PackageEndTime——旧实现
// 读 PackageEndTime 恒 miss，expiring 恒 0，选号第四因子自上线从未生效）。
func TestUserResourceDetailedExpiringFromCycleEndTime(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := resourceStub(
		`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
			`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`)
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if expiring != 1200 {
		t.Errorf("expiring=%d want 1200（CycleEndTime 在 7 天窗内的奖励包）", expiring)
	}
}

// TestUserResourceDetailedNoWindowAllStable soon ≤ 0 时禁用分桶：即便到期时间就在
// 眼前也全部归 Stable（与引入分桶前行为一致，expiring 恒 0）。
func TestUserResourceDetailedNoWindowAllStable(t *testing.T) {
	in1h := time.Now().Add(time.Hour).Format(packageEndLayout)
	c := resourceStub(`{"PackageName":"p","CycleEndTime":"` + in1h + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`)
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 80 || expiring != 0 {
		t.Errorf("remain=%d expiring=%d, want 80/0（soon<=0 禁用分桶）", remain, expiring)
	}
}

// TestUserResourceDetailedIgnoresPackageEndTime 反向断言：只喂旧字段 PackageEndTime
// 时 expiring 必须为 0。这正是 bug 的本质——上游从不下发该字段，任何"兼容旧字段"
// 的兜底都会让判据重新变成永远 miss（或引入上游不存在的行为），故锁死此事实。
func TestUserResourceDetailedIgnoresPackageEndTime(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := resourceStub(
		`{"PackageName":"奖励包","PackageEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300}`)
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1200 {
		t.Errorf("remain=%d want 1200", remain)
	}
	if expiring != 0 {
		t.Errorf("expiring=%d want 0（PackageEndTime 不是上游下发字段，不得被读取）", expiring)
	}
}

// TestUserResourceDetailedMissingEndTimeStable 响应不含 CycleEndTime：保守归 Stable，
// 不 panic、不误算为快过期（避免插队）。
func TestUserResourceDetailedMissingEndTimeStable(t *testing.T) {
	c := resourceStub(`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`)
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 80 || expiring != 0 {
		t.Errorf("remain=%d expiring=%d, want 80/0（无到期字段归 Stable）", remain, expiring)
	}
}

// TestUserResourceDetailedInvalidEndTimeStable 字段值格式非法（非墙钟串）：解析失败
// 保守归 Stable，不 panic、不误标快过期。
func TestUserResourceDetailedInvalidEndTimeStable(t *testing.T) {
	c := resourceStub(`{"PackageName":"p","CycleEndTime":"not-a-time","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`)
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 80 || expiring != 0 {
		t.Errorf("remain=%d expiring=%d, want 80/0（解析失败归 Stable）", remain, expiring)
	}
}

// TestUserResourceDetailedRequestKeepsPackageEndTimeRange 请求侧过滤串是
// PackageEndTimeRangeBegin/End（与响应侧到期字段同名但无关），改响应侧判据时
// 不得被顺手改名——上游按该参数过滤套餐列表，改错会直接查不到包。
func TestUserResourceDetailedRequestKeepsPackageEndTimeRange(t *testing.T) {
	var got []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		got, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})
	if _, _, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, time.Hour); err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if !bytes.Contains(got, []byte(`"PackageEndTimeRangeBegin"`)) || !bytes.Contains(got, []byte(`"PackageEndTimeRangeEnd"`)) {
		t.Errorf("请求体缺 PackageEndTimeRange* 过滤串: %s", got)
	}
}

// ---------------------------------------------------------------------------
// 分桶诊断（UserResourceDetailedDiag）：让调用方区分「窗口内确实没有快过期积分」
// 与「根本没解析到任何 CycleEndTime」（运维诉求：到期积分不显示时能自证是哪种情况）。
//
// 为什么新增变体而不是改 UserResourceDetailed 签名：上面 6 个既有用例逐字锁定分桶
// 语义（含「忽略 PackageEndTime」「缺/坏 EndTime 归长期」），签名一变它们全部要改；
// 变体把「语义」与「诊断」分开——UserResourceDetailed 委托变体实现，返回值不变。
// ---------------------------------------------------------------------------

// TestUserResourceDetailedDiagNearestEnd 诊断字段：包裹数 / 可解析到期数 / 坏字段数 /
// 最近到期时刻（最早的可解析到期，含已过期——上游按 PackageEndTimeRangeBegin=now 过滤，
// 正常不该出现，出现即数据异常，日志里直接可见）。
func TestUserResourceDetailedDiagNearestEnd(t *testing.T) {
	in3d := time.Now().In(softRateResetLoc).Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := time.Now().In(softRateResetLoc).Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := resourceStub(
		`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
			`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200},` +
			`{"PackageName":"无到期","CycleCapacitySize":100,"CycleCapacityRemain":90,"CycleCapacityUsed":10},` +
			`{"PackageName":"坏字段","CycleEndTime":"not-a-time","CycleCapacitySize":10,"CycleCapacityRemain":5,"CycleCapacityUsed":5}`)
	remain, _, expiring, diag, err := c.UserResourceDetailedDiag(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed diag: %v", err)
	}
	// 分桶口径与 UserResourceDetailed 完全一致（变体只多返回诊断）。
	if remain != 1595 || expiring != 1200 {
		t.Errorf("remain=%d expiring=%d want 1595/1200（分桶口径不得与 UserResourceDetailed 分叉）", remain, expiring)
	}
	if diag.Packages != 4 {
		t.Errorf("diag.Packages=%d want 4（上游返回的套餐条目数）", diag.Packages)
	}
	if diag.WithEnd != 2 {
		t.Errorf("diag.WithEnd=%d want 2（可解析 CycleEndTime 的条目数）", diag.WithEnd)
	}
	if diag.BadEnd != 1 {
		t.Errorf("diag.BadEnd=%d want 1（非空但解析失败的条目数）", diag.BadEnd)
	}
	if diag.Soon != 7*24*time.Hour {
		t.Errorf("diag.Soon=%v want 168h（本次分桶实际使用的窗口）", diag.Soon)
	}
	want, _ := time.ParseInLocation(packageEndLayout, in3d, softRateResetLoc)
	if !diag.NearestEnd.Equal(want) {
		t.Errorf("diag.NearestEnd=%v want %v（最早的到期时刻）", diag.NearestEnd, want)
	}
	// 最近到期的可读文案：上游墙钟格式（UTC+8），供日志行直接拼用。
	if got := diag.NearestEndText(); got != in3d {
		t.Errorf("NearestEndText()=%q want %q", got, in3d)
	}
}

// TestUserResourceDetailedDiagNoEndTime 响应里没有任何可解析 CycleEndTime 时：
// WithEnd=0、NearestEnd 零值、NearestEndText 空串——调用方据此把
// 「窗口内没有」与「根本没解析到」区分开（后者是上游字段变化/解析失败）。
func TestUserResourceDetailedDiagNoEndTime(t *testing.T) {
	c := resourceStub(`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`)
	_, _, expiring, diag, err := c.UserResourceDetailedDiag(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed diag: %v", err)
	}
	if expiring != 0 || diag.WithEnd != 0 || diag.BadEnd != 0 || diag.Packages != 1 {
		t.Errorf("expiring=%d diag=%+v want 0/WithEnd=0/BadEnd=0/Packages=1", expiring, diag)
	}
	if !diag.NearestEnd.IsZero() || diag.NearestEndText() != "" {
		t.Errorf("无可解析到期时间时 NearestEnd 应为零值、文案为空：%+v %q", diag.NearestEnd, diag.NearestEndText())
	}
}

// TestUserResourceDetailedDiagDisabledWindow soon<=0（禁用分桶）时诊断照样给出窗口与
// 到期信息：运维据日志判断"分桶被关掉了"（而不是"没有快过期积分"）。
func TestUserResourceDetailedDiagDisabledWindow(t *testing.T) {
	in1h := time.Now().In(softRateResetLoc).Add(time.Hour).Format(packageEndLayout)
	c := resourceStub(`{"PackageName":"p","CycleEndTime":"` + in1h + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`)
	_, _, expiring, diag, err := c.UserResourceDetailedDiag(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil {
		t.Fatalf("detailed diag: %v", err)
	}
	if expiring != 0 {
		t.Errorf("expiring=%d want 0（soon<=0 禁用分桶）", expiring)
	}
	if diag.Soon != 0 || diag.WithEnd != 1 {
		t.Errorf("diag=%+v want Soon=0/WithEnd=1（窗口禁用也要能看出到期信息）", diag)
	}
	// 日志正文：窗口禁用必须显式写出（否则「快过期=0」会被读成"确实没有"）。
	line := BalanceLine(80, 100, expiring, diag)
	if !strings.Contains(line, "禁用分桶") {
		t.Errorf("窗口禁用时日志正文应显式标注：%q", line)
	}
}

// TestBalanceLineShape 日志正文形态（调度器/面板共用同一口径，便于 grep 与对照）：
// 剩余/快过期在括号外，窗口/包裹数/可解析到期在括号内，最近到期仅在可解析时出现。
func TestBalanceLineShape(t *testing.T) {
	c := resourceStub(`{"PackageName":"p","CycleEndTime":"` +
		time.Now().In(softRateResetLoc).Add(3*24*time.Hour).Format(packageEndLayout) +
		`","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`)
	_, _, expiring, diag, err := c.UserResourceDetailedDiag(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed diag: %v", err)
	}
	line := BalanceLine(80, 100, expiring, diag)
	t.Logf("日志正文样例：%s", line)
	for _, want := range []string{"剩余=80/100", "快过期=80", "窗口=168h", "包裹数=1", "可解析到期=1", "最近到期="} {
		if !strings.Contains(line, want) {
			t.Errorf("日志正文缺 %q：%q", want, line)
		}
	}
}

// ---------------------------------------------------------------------------
// 面板「积分构成」的逐包到期时间（CreditPackages.EndTime）
//
// 面板每一行包的「到期」列全是「-」：CreditPackages 的匿名响应结构体只声明了
// ExpiredTime / PackageEndTime 两个到期字段，而上游（与 Expiring 分桶同一个端点
// get-user-resource，CN/global 两域实测）这两个字段**都不下发**（恒空串）→
// cp.EndTime 恒空 → 前端 esc((p.end_time||'').slice(0,10)||'—') 渲染成「-」。
// 真实到期字段是 CycleEndTime——与本文件 UserResourceDetailed 的 Expiring 分桶
// 判据同源（同端点、同墙钟格式 packageEndLayout，见 cfa10cf）。
// ---------------------------------------------------------------------------

// TestCreditPackagesEndTimeFromCycleEndTime 核心回归：上游只下发 CycleEndTime
// （不下发 ExpiredTime/PackageEndTime）时，逐包到期时间必须取到该值。
// 修复前此用例必红（EndTime 恒空 → 面板「到期」列恒为「-」）。
func TestCreditPackagesEndTimeFromCycleEndTime(t *testing.T) {
	c := resourceStub(`{"PackageName":"国内运营裂变包","CycleEndTime":"2026-10-18 05:24:02",` +
		`"CapacitySize":1500,"CapacityRemain":900,"CapacityUsed":600}`)
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	if packs[0].EndTime != "2026-10-18 05:24:02" {
		t.Errorf("EndTime=%q want %q（上游只下发 CycleEndTime 时不得为空——面板「到期」列恒为「-」的根因）",
			packs[0].EndTime, "2026-10-18 05:24:02")
	}
}

// TestCreditPackagesEndTimeFallbackOrder 三级兜底优先级
// ExpiredTime → PackageEndTime → CycleEndTime，且「有值就用」：空串不得覆盖真值。
//
// 顺序理由：前两个字段是修复前就在读的（旧行为对真下发它们的域零回归——取值与修复前
// 逐字一致）；但它们上游实测从不下发，所以真正补上缺口的是末位的 CycleEndTime。
// 若哪天实测发现某个域真的下发了前两个字段且语义不同，才需要重新评估这个顺序。
func TestCreditPackagesEndTimeFallbackOrder(t *testing.T) {
	cases := []struct {
		name string
		acct string
		want string
	}{
		{"三个都给取 ExpiredTime",
			`{"PackageName":"p","ExpiredTime":"2026-11-01 00:00:00","PackageEndTime":"2026-12-01 00:00:00","CycleEndTime":"2026-10-18 05:24:02"}`,
			"2026-11-01 00:00:00"},
		{"只给后两个取 PackageEndTime",
			`{"PackageName":"p","PackageEndTime":"2026-12-01 00:00:00","CycleEndTime":"2026-10-18 05:24:02"}`,
			"2026-12-01 00:00:00"},
		{"只给 CycleEndTime（上游实际形态）",
			`{"PackageName":"p","CycleEndTime":"2026-10-18 05:24:02"}`,
			"2026-10-18 05:24:02"},
		{"ExpiredTime 空串不得覆盖 PackageEndTime",
			`{"PackageName":"p","ExpiredTime":"","PackageEndTime":"2026-12-01 00:00:00","CycleEndTime":"2026-10-18 05:24:02"}`,
			"2026-12-01 00:00:00"},
		{"前两个空串不得覆盖 CycleEndTime",
			`{"PackageName":"p","ExpiredTime":"","PackageEndTime":"","CycleEndTime":"2026-10-18 05:24:02"}`,
			"2026-10-18 05:24:02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := resourceStub(tc.acct)
			packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
			if err != nil {
				t.Fatalf("packages: %v", err)
			}
			if len(packs) != 1 {
				t.Fatalf("packs=%d want 1", len(packs))
			}
			if packs[0].EndTime != tc.want {
				t.Errorf("EndTime=%q want %q", packs[0].EndTime, tc.want)
			}
		})
	}
}

// TestCreditPackagesEndTimeEmptyWhenAllMissing 三个到期字段全缺 → EndTime 保持空串
// （前端渲染「-」）。**不得**回落成 "0"/零时刻/"1970-01-01"：那会让面板把「无到期」
// 显示成「已过期」，比空值更坏。
func TestCreditPackagesEndTimeEmptyWhenAllMissing(t *testing.T) {
	c := resourceStub(`{"PackageName":"p","CapacitySize":10,"CapacityRemain":5,"CapacityUsed":5}`)
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	if packs[0].EndTime != "" {
		t.Errorf("EndTime=%q want 空串（无到期字段 = 无到期，前端渲染「-」）", packs[0].EndTime)
	}
}

// TestCreditPackagesEndTimePassThrough 到期值**原样透传**，不做格式归一：
// CycleEndTime 上游形态就是 "2006-01-02 15:04:05"（packageEndLayout），前端「到期」
// 列按 slice(0,10) 取日期，原样即已满足；归一成 RFC3339 会多一次格式转换（且要同步
// 改前端），零收益而引入格式风险。
func TestCreditPackagesEndTimePassThrough(t *testing.T) {
	const end = "2027-03-12 22:03:50"
	c := resourceStub(`{"PackageName":"p","CycleEndTime":"` + end + `"}`)
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(packs) != 1 || packs[0].EndTime != end {
		t.Fatalf("EndTime 必须原样透传 %q，实际 %+v", end, packs)
	}
	// 与 UserResourceDetailed 的解析口径同源：同串必须能被 packageEndLayout +
	// softRateResetLoc 解析（前端三态判定也按这个口径算剩余天数）。
	if _, perr := time.ParseInLocation(packageEndLayout, packs[0].EndTime, softRateResetLoc); perr != nil {
		t.Errorf("透传值 %q 必须可被 packageEndLayout 解析：%v", packs[0].EndTime, perr)
	}
}

// TestCreditPackagesSumUnchanged 求和口径锁定（本次只加到期字段，求和逐字不变）：
// CycleCapacitySize > 0 时按周期字段算，否则按 Capacity 字段算——两条路径不能混，
// 否则同一个包会被算两次（与 UserResourceDetailed 的聚合口径一致）。
func TestCreditPackagesSumUnchanged(t *testing.T) {
	c := resourceStub(
		`{"PackageName":"周期包","CycleEndTime":"2026-10-18 05:24:02",` +
			`"CapacitySize":9999,"CapacityRemain":9999,"CapacityUsed":0,` +
			`"CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
			`{"PackageName":"普通包","CycleEndTime":"2027-03-12 22:03:50",` +
			`"CapacitySize":800,"CapacityRemain":500,"CapacityUsed":300}`)
	packs, remain, size, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("packs=%d want 2", len(packs))
	}
	// 周期包的 CapacitySize=9999 不得参与求和（否则同一个包被算两次）。
	if remain != 1700 || size != 2300 {
		t.Errorf("remain/size=%d/%d want 1700/2300（求和口径：周期包只走周期字段）", remain, size)
	}
	if !packs[0].Cycle || packs[1].Cycle {
		t.Errorf("Cycle 标记错误：%+v", packs)
	}
	if packs[0].Remain != 1200 || packs[0].Size != 1500 || packs[0].Used != 300 {
		t.Errorf("周期包明细错误：%+v", packs[0])
	}
	if packs[1].Remain != 500 || packs[1].Size != 800 || packs[1].Used != 300 {
		t.Errorf("普通包明细错误：%+v", packs[1])
	}
	// 到期字段与求和互不影响：两个包各自带自己的到期时刻。
	if packs[0].EndTime != "2026-10-18 05:24:02" || packs[1].EndTime != "2027-03-12 22:03:50" {
		t.Errorf("到期字段错误：%+v", packs)
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
	// 连接层加固后本字段保持 config 驱动（构造默认仍 120s，main.go 会覆盖）；
	// 其余加固项断言见 transport_test.go 的 TestNewTransportHardening。
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

// failReader 读即失败（模拟连接中断/截断），用于验证读 body 错误被显式处理。
type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom: connection reset") }

// TestDoJSONReadBodyErrorNotClassified doJSON 的 body 读失败必须返回普通错误
// （非 *Error）：半截 body 不进 Classify、不参与账号惩罚（传输层故障不误罚号）。
//
// 原实现 `raw, _ := io.ReadAll(...)` 吞掉读错误，把半截 body 交给 Classify——
// 实证误罚链：500 + 半截 credit 文案曾被判 ErrHardCredit（长冷却罚号）。
func TestDoJSONReadBodyErrorNotClassified(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 500,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       nopCloserBody{failReader{}},
		}, nil
	})
	req, _ := http.NewRequest(http.MethodGet, "https://billing.example/x", nil)
	_, err := c.doJSON(req)
	if err == nil {
		t.Fatal("read body failure must return an error, got nil")
	}
	var ue *Error
	if errors.As(err, &ue) {
		t.Fatalf("read body failure must NOT be *Error (would feed breaker/cooldown): %+v", ue)
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("error should wrap read body: %v", err)
	}
}

// TestChatStreamErrorStatusReadBodyError 流式 ≥400 分支的 body 读失败同样返回
// 传输层错误（非 status+半截 body）：调用方 applyErrorPolicy 不会按误判分类罚号。
func TestChatStreamErrorStatusReadBodyError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 500,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       nopCloserBody{failReader{}},
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("chat stream read body failure must return an error")
	}
	if status != 0 || respBody != nil {
		t.Errorf("must not hand half body to caller: status=%d body=%q", status, respBody)
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("error should wrap read body: %v", err)
	}
}

// TestFetchModelsReadBodyError FetchModels 的 body 读失败返回传输层错误
// （该路径不 NoteError，正确行为是换号而非罚号）。
func TestFetchModelsReadBodyError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       nopCloserBody{failReader{}},
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, err := c.FetchModels(a)
	if err == nil {
		t.Fatal("fetch models read body failure must return an error")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("error should wrap read body: %v", err)
	}
}

// TestChatStreamSuccessThenNoShadowedCancelNilPanic 成功分支不再触碰外层 shadowed
// cancel：删掉外层 `var cancel context.CancelFunc` 后，成功路径只依赖内层 := 的
// cancel（已移交 monitorBody），不得 panic。同时覆盖 404 换路径重试后成功的情形
// （循环尾兜底代码已删，靠各出口 return）。
func TestChatStreamSuccessThenNoShadowedCancelNilPanic(t *testing.T) {
	paths := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths++
		if strings.Contains(r.URL.Path, "console") {
			// 兜底路径：直接成功
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
		return jsonResp(404, `{}`), nil
	})
	c.ChatBaseCN = "https://global.example"
	c.GlobalEnabled = true
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatal(err)
	}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("global 404 → fallback 成功路径不应报错: %v", err)
	}
	if status != 200 {
		t.Fatalf("status=%d want 200", status)
	}
	if rc == nil {
		t.Fatal("rc must not be nil on success")
	}
	rc.Close()
	if paths < 2 {
		t.Errorf("expected 404 fallback retry, paths hit=%d", paths)
	}
}

// TestRateRegexesPrecompiledConcurrent 正则预编译为包级 var 后，两个限流判定
// 函数在高并发下结果恒定。旧实现（函数体内 MustCompile）在此测试下同样通过
// （纯只读），该测试锁的是「预编译不改变语义」+ 并发安全，防止未来有人把包级
// var 改回带状态的调用侧编译。
func TestRateRegexesPrecompiledConcurrent(t *testing.T) {
	const bodies = 50
	const workers = 8
	rlBody := `{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`
	resetBody := `{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < bodies; i++ {
				if !IsModelRateLimit(rlBody) {
					errs <- fmt.Errorf("IsModelRateLimit concurrent miss")
					return
				}
				if _, ok := ParseRateReset(resetBody); !ok {
					errs <- fmt.Errorf("ParseRateReset concurrent miss")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestTruncateRuneBoundaryOnCJKErrorBody 三份截断实现合一后的回归：上游错误 body
// 多为中文（"将在 … 重置"），旧实现按字节切会产出半截 UTF-8 序列（乱码）。这里
// 直接断言包内 truncate 转发到 logfmt.Truncate 后输出合法 UTF-8 且带省略标记。
func TestTruncateRuneBoundaryOnCJKErrorBody(t *testing.T) {
	// Arrange：120 字节上限恰好切在某个汉字的中间字节上。
	s := strings.Repeat("中", 50) // 150 字节

	// Act
	got := truncate(s, 120)

	// Assert
	if !utf8.ValidString(got) {
		t.Fatalf("truncate 输出不是合法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("超长截断应补省略标记，got %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "\uFFFD") {
		t.Errorf("截断不得留下替换字符: %q", got)
	}
	// 120 是 3 的倍数 → 恰好落在 rune 边界，保留 40 个汉字。
	if want := strings.Repeat("中", 40) + "…"; got != want {
		t.Errorf("truncate(中×50, 120)=%q want %q", got, want)
	}
}

// TestTruncateShortBodyUnchanged 短 body（未触发截断）不补省略标记：否则每个正常
// 错误消息尾巴都会多一个 "…"，反而让「是否被截断」失去信息量。
func TestTruncateShortBodyUnchanged(t *testing.T) {
	// Arrange
	s := `{"code":6004,"msg":"short"}`

	// Act + Assert
	if got := truncate(s, 200); got != s {
		t.Errorf("truncate(short, 200)=%q want 原样 %q", got, s)
	}
}
