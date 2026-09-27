package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// handler_body_limit_test.go 请求体预拦截语义（吸收上游 73fe1f8，行为变更）。
//
// 上游 e34cfa4/a0aae43（PR #159）的裁定：请求体超限**不再由网关 413 提前拦截**，
// 交由上游自然响应——上游错误信息量更大（能看到上游到底是什么策略），网关提前
// 413 反而挡住上游真实行为；多图/长上下文会话（历史图片每轮 base64 重发）不再撞
// 网关上限。
//
// 本仓的取舍（与上游的差异，刻意）：**保留配置项** server.max_body_mb，但把默认值
// 改为 0 = 不预拦截。理由：
//   - 删除配置键会破坏既有 config.json 兼容（老配置里的键会变成"未知键被忽略"，
//     用户的显式设置静默失效——正是本仓 issue #17 反复强调的"静默不生效"失效模式）；
//   - 用户可能确实想要一道内存护栏（无入站并发闸门时，超大 body 会把进程内存吃爆），
//     保留可显式开启的护栏比删掉它更安全：默认不拦（对齐上游行为），要拦就显式配。
//   - 面板「请求体上限」输入框留空 → 不下发该键 → 用默认（不预拦截）；填数字才拦。
//
// 负值仍 fail fast（语义不可能：负的字节上限）；0 从"非法"变成"不预拦截"。

// TestChatLargeBodyNoGatewayLimit 默认（max_body_mb=0，不预拦截）：远超旧默认
// 32MB 的合法 body 完整读入并照常打上游，网关不再 413（对齐上游 e34cfa4 规约）。
//
// body 用**尾部空白**撑大：JSON 扫描器跳过空白不分配内存，仍能真实覆盖"网关不预拦截"
// 这条行为，同时避免 33MB 的字符串拷贝把 -race -count=2 跑成内存瓶颈。
func TestChatLargeBodyNoGatewayLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 MaxBodyBytes → 默认不预拦截

	if DefaultMaxBodyBytes != 0 {
		t.Fatalf("DefaultMaxBodyBytes=%d want 0（默认不预拦截，吸收上游 73fe1f8）", DefaultMaxBodyBytes)
	}

	body := append([]byte(`{"model":"glm-5.2","messages":[]}`), bytes.Repeat([]byte(" "), 33<<20)...)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed, no gateway pre-block)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatBodyLimitConfiguredStillBlocks 显式配置护栏时 413 路径照旧：
// 「保留配置项」的承诺必须真能拦住（否则等于删掉了护栏能力）。
func TestChatBodyLimitConfiguredStillBlocks(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})
	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	pad := strings.Repeat("a", 100-len(prefix)-len(suffix)+1) // 101 字节 > 100
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(prefix+pad+suffix)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%s want 413", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"code":"request_body_too_large"`) {
		t.Errorf("413 应携带 request_body_too_large code: %s", rec.Body)
	}
	if calls != 0 {
		t.Errorf("预拦截不得打上游, calls=%d", calls)
	}
	// 恰好等于上限仍放行（边界不误伤）。
	exact := prefix + strings.Repeat("a", 100-len(prefix)-len(suffix)) + suffix
	if len(exact) != 100 {
		t.Fatalf("fixture len=%d want 100", len(exact))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(exact)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (exactly-at-limit must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestSetMaxBodyBytesHotApply 面板在线改 server.max_body_mb 必须即时生效（issue #17：
// 改了配置却静默不生效，用户仍被旧上限 413）。同一请求体：调小后 413、调大后放行；
// **置 0 等于关闭预拦截**（吸收上游 73fe1f8 的新语义——0 不再是"非法/回落默认"）。
func TestSetMaxBodyBytesHotApply(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})

	// 尾部空格不影响 JSON 合法性，只把请求体撑过 100 字节。
	body := []byte(`{"model":"glm-5.2","messages":[]}` + strings.Repeat(" ", 128))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 (body 155B > limit 100B)", rec.Code)
	}

	h.SetMaxBodyBytes(4096) // 面板保存路径的热更新（调大）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 after enlarge (limit 4096B)", rec.Code)
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1（放行后应恰好打一次）", calls)
	}

	// 0 = 关闭预拦截（新语义）：同一 body 仍放行，且不再有任何上限。
	h.SetMaxBodyBytes(0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (0 = 不预拦截)", rec.Code)
	}
	huge := append([]byte(`{"model":"glm-5.2","messages":[]}`), bytes.Repeat([]byte(" "), 2<<20)...)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(huge)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (2MB body with no limit)", rec.Code)
	}
}

// TestSetMaxBodyBytesNegativeDisablesLimit 负值（防御性输入）按"不预拦截"处理：
// 面板 number 输入可能提交 0 或负数，负的字节上限无意义，不静默变成"极小上限"
// （那会把所有请求打成 413——最坏的反向风险）。
func TestSetMaxBodyBytesNegativeDisablesLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.SetMaxBodyBytes(-1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（负值不得变成极小上限）", rec.Code)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1", calls)
	}
}

// TestBodyTooLargeMsgWording 413 文案口径：本项是**网关侧可配护栏**（不是上游限制），
// 且必须给出关闭方式（max_body_mb=0）——默认已改为不预拦截，收到 413 只可能是
// 运维自己配了护栏，文案要让人知道怎么关。
func TestBodyTooLargeMsgWording(t *testing.T) {
	msg := bodyTooLargeMsg(32 << 20)
	for _, want := range []string{"网关", "不是上游限制", "server.max_body_mb"} {
		if !strings.Contains(msg, want) {
			t.Errorf("413 文案必须含 %q：%s", want, msg)
		}
	}
	if !strings.Contains(msg, "0") {
		t.Errorf("413 文案须给出关闭方式（max_body_mb=0 = 不预拦截）：%s", msg)
	}
	if !strings.Contains(msg, "32 MB") {
		t.Errorf("413 文案须回显当前生效上限：%s", msg)
	}
	// 限值按实际注入值渲染（不是写死 32——用户调大后文案要跟着变）。
	if m := bodyTooLargeMsg(8 << 20); !strings.Contains(m, "8 MB") {
		t.Errorf("文案的限值应跟随实际值渲染（8MB）: %s", m)
	}
	// 常见成因（多图 base64 重发）。
	if !strings.Contains(msg, "base64") {
		t.Errorf("413 文案应提示多图 base64 重发的成因：%s", msg)
	}
	// 不得残留 fmt 动词渲染残渣：文案里的字面百分号（"37%"）若漏转义成 "%%"，
	// Sprintf 会把它当动词渲染出 "%!)(MISSING)" 之类的垃圾。go build 不报错、
	// go vet 才报（unknown verb），故这里再做一道运行期兜底（本函数上线前踩过一次）。
	for _, bad := range []string{"%!", "(MISSING)", "EXTRA"} {
		if strings.Contains(msg, bad) {
			t.Errorf("413 文案含 fmt 渲染残渣 %q（字面百分号需写成 %%）：%s", bad, msg)
		}
	}
	if !strings.Contains(msg, "37%") {
		t.Errorf("413 文案应保留「膨胀约 37%%」的原始百分号：%s", msg)
	}
}

// TestBodyTooLargeMsgMatchesREADME README 的 FAQ 示例 JSON 必须与代码实际输出一致。
//
// README 里贴的是「用户会照抄去排查」的样本：示例 message 与真实 413 不一致时，
// 用户拿样本去 grep/对比会以为收到的是别的错误（且这类漂移不会有任何编译错误）。
// 断言方式：README 的示例行必须包含本进程渲染出的完整 message。
func TestBodyTooLargeMsgMatchesREADME(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	msg := bodyTooLargeMsg(32 << 20) // FAQ 示例里用的上限值（32MB）
	if !strings.Contains(readme, msg) {
		t.Errorf("README 的 413 示例与代码实际输出不一致。\n代码输出：%s\n（README 中未找到该串；"+
			"FAQ 示例 JSON 需原样同步 bodyTooLargeMsg 的输出）", msg)
	}
}
