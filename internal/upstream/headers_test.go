// headers_test.go 账号级设备指纹头（X-Machine-ID / X-Session-ID）的派生与注入单测。
//
// 头名与盐格式的断言全部走**原始字节**（byte 字面量 / hex），不用源码里的可见字符串
// 做期望值——源码可见字符可能被工具链/显示层改写，字节才是上游真正收到的东西。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 出站设备头名与固定盐的原始字节（ASCII）：
//   - "X-Machine-ID"  = 58 2d 4d 61 63 68 69 6e 65 2d 49 44
//   - "X-Session-ID"  = 58 2d 53 65 73 73 69 6f 6e 2d 49 44
//   - "wb2a:"         = 77 62 32 61 3a
var (
	rawMachineHeader  = []byte{0x58, 0x2d, 0x4d, 0x61, 0x63, 0x68, 0x69, 0x6e, 0x65, 0x2d, 0x49, 0x44}
	rawSessionHeader  = []byte{0x58, 0x2d, 0x53, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x2d, 0x49, 0x44}
	rawSaltPrefix     = []byte{0x77, 0x62, 0x32, 0x61, 0x3a}
	rawPurposeMachine = []byte{0x6d, 0x61, 0x63, 0x68, 0x69, 0x6e, 0x65}
	rawPurposeSession = []byte{0x73, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e}
	rawColon          = []byte{0x3a}
)

// headerKey 返回头名写入 net/http 头表后的形态（Go 对头名做 canonical MIME 化，
// 与上游同语言的实现一致；HTTP 头名大小写不敏感，语义等价）。
// 源码字面量仍是原始 ASCII（下面另有源码字节断言钉死）。
func headerKey(raw []byte) string { return textproto.CanonicalMIMEHeaderKey(string(raw)) }

// wantStableID 按原始字节独立复算期望值：sha256("wb2a:" + purpose + ":" + uid)[:18] hex。
// 盐/分隔符/purpose 全用 byte 字面量拼接，不共享实现里的任何字符串字面量。
func wantStableID(uid string, purpose []byte) string {
	var in []byte
	in = append(in, rawSaltPrefix...)
	in = append(in, purpose...)
	in = append(in, rawColon...)
	in = append(in, uid...)
	sum := sha256.Sum256(in)
	return hex.EncodeToString(sum[:18])
}

var hex36Re = regexp.MustCompile(`^[0-9a-f]{36}$`)

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestHeaderNamesRawBytesInSource 源码里的头名字面量与固定盐必须是这些原始字节
// （显示层改写会在这里被抓住：bytes.Contains 对字节精确匹配）。
func TestHeaderNamesRawBytesInSource(t *testing.T) {
	src, err := os.ReadFile("headers.go")
	if err != nil {
		t.Fatal(err)
	}
	// 头名出现在 req.Header.Set 的第一个实参里（带引号）。
	for _, raw := range [][]byte{rawMachineHeader, rawSessionHeader} {
		lit := append(append([]byte{0x22}, raw...), 0x22) // "X-..."
		if !strings.Contains(string(src), string(lit)) {
			t.Errorf("headers.go 缺头名字面量（原始字节 % x）", raw)
		}
	}
	if !strings.Contains(string(src), string(rawSaltPrefix)) {
		t.Errorf("headers.go 缺固定盐前缀（原始字节 % x）", rawSaltPrefix)
	}
	// 两个 purpose 盐段也必须逐字面存在。
	for _, raw := range [][]byte{rawPurposeMachine, rawPurposeSession} {
		if !strings.Contains(string(src), string(raw)) {
			t.Errorf("headers.go 缺 purpose 盐段（原始字节 % x）", raw)
		}
	}
}

// TestStableIDStable 同 uid + 同用途两次派生恒同值（固定盐、无随机源 → 跨重启稳定）。
func TestStableIDStable(t *testing.T) {
	if a, b := deriveAccountStableID("u1", "machine"), deriveAccountStableID("u1", "machine"); a != b {
		t.Errorf("machine ID 不稳定：%q vs %q", a, b)
	}
	if a, b := deriveAccountStableID("u1", "session"), deriveAccountStableID("u1", "session"); a != b {
		t.Errorf("session ID 不稳定：%q vs %q", a, b)
	}
}

// TestStableIDDistinctByUID 不同 uid → 不同值（账号间互异，多号不再共用同一设备指纹）。
func TestStableIDDistinctByUID(t *testing.T) {
	for _, purpose := range []string{"machine", "session"} {
		if deriveAccountStableID("u1", purpose) == deriveAccountStableID("u2", purpose) {
			t.Errorf("purpose=%s：不同 uid 派生出相同 ID", purpose)
		}
	}
}

// TestStableIDDistinctByPurpose 同 uid 的 machine / session 用途互异（盐含 purpose 段）。
func TestStableIDDistinctByPurpose(t *testing.T) {
	if deriveAccountStableID("u1", "machine") == deriveAccountStableID("u1", "session") {
		t.Error("machine 与 session 用途派生出相同 ID（purpose 未参与盐）")
	}
}

// TestStableIDFormatAndValue 形态恒 36 位小写 hex，且值与按原始字节的独立复算一致
// （同时钉死盐格式——改一个字节即改全量账号指纹）。
func TestStableIDFormatAndValue(t *testing.T) {
	for _, tc := range []struct {
		purpose string
		raw     []byte
	}{
		{"machine", rawPurposeMachine},
		{"session", rawPurposeSession},
	} {
		got := deriveAccountStableID("uid-abc", tc.purpose)
		if !hex36Re.MatchString(got) {
			t.Errorf("derive(%q)=%q，want 36 位小写 hex", tc.purpose, got)
		}
		if want := wantStableID("uid-abc", tc.raw); got != want {
			t.Errorf("derive(%q)=%q want %q（盐格式漂移）", tc.purpose, got, want)
		}
	}
}

// TestAccountStableHeadersInjectedOnChat 主聊天路径注入两头：头名按原始字节校验
// （经 canonical 形态读回），取值与独立复算一致，两头互异。
func TestAccountStableHeadersInjectedOnChat(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u-200a"}
	req := newReq(t)
	c := &Client{MachineIDHeaders: true}
	c.ChatHeaders(req, a, "", ChatMeta{})

	for _, raw := range [][]byte{rawMachineHeader, rawSessionHeader} {
		if !headerPresent(req.Header, raw) {
			t.Errorf("chat 请求缺头（原始字节 % x）", raw)
		}
	}
	if got, want := req.Header.Get(headerKey(rawMachineHeader)), wantStableID("u-200a", rawPurposeMachine); got != want {
		t.Errorf("machine=%q want %q", got, want)
	}
	if got, want := req.Header.Get(headerKey(rawSessionHeader)), wantStableID("u-200a", rawPurposeSession); got != want {
		t.Errorf("session=%q want %q", got, want)
	}
	if req.Header.Get(headerKey(rawMachineHeader)) == req.Header.Get(headerKey(rawSessionHeader)) {
		t.Error("machine 与 session 头取值相同")
	}
}

// TestAccountStableHeadersInjectedOnBilling billing 域（report/travel/balance 等）
// 不走 CommonHeaders，必须单独注入（业务路径全覆盖，与 X-Device-Token 同口径）。
func TestAccountStableHeadersInjectedOnBilling(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u-200a"}
	req := newReq(t)
	c := &Client{MachineIDHeaders: true}
	c.BillingHeaders(req, a)

	if got, want := req.Header.Get(headerKey(rawMachineHeader)), wantStableID("u-200a", rawPurposeMachine); got != want {
		t.Errorf("billing machine=%q want %q", got, want)
	}
	if got, want := req.Header.Get(headerKey(rawSessionHeader)), wantStableID("u-200a", rawPurposeSession); got != want {
		t.Errorf("billing session=%q want %q", got, want)
	}
}

// TestAccountStableHeadersAbsentOnRefresh refresh/auth 类路径**不得**携带设备指纹：
// RefreshHeaders 复用 CommonHeaders，故注入点刻意不放在 CommonHeaders
// （与上游"公共 headers 全覆盖含 refresh"的口径刻意收敛，见 headers.go 注释）。
func TestAccountStableHeadersAbsentOnRefresh(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "u-200a"}
	req := newReq(t)
	c := &Client{MachineIDHeaders: true}
	c.RefreshHeaders(req, a)

	for _, raw := range [][]byte{rawMachineHeader, rawSessionHeader} {
		if headerPresent(req.Header, raw) {
			t.Errorf("refresh 请求不应携带设备头（原始字节 % x）", raw)
		}
	}
}

// TestAccountStableHeadersSkippedWhenEmptyUID uid 为空/无 auth 时不注入（不伪造）、不 panic。
// 直接调用注入函数：ChatHeaders 对 nil auth 的既有行为与本改动无关（其 a.UID 分支前置存在）。
func TestAccountStableHeadersSkippedWhenEmptyUID(t *testing.T) {
	c := &Client{MachineIDHeaders: true}
	for _, tc := range []struct {
		name string
		a    *auth.Auth
	}{
		{"nil auth", nil},
		{"empty uid", &auth.Auth{AccessToken: "at"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newReq(t)
			c.injectAccountStableHeaders(req, tc.a)
			for _, raw := range [][]byte{rawMachineHeader, rawSessionHeader} {
				if headerPresent(req.Header, raw) {
					t.Errorf("uid 为空时不应注入设备头（原始字节 % x）", raw)
				}
			}
		})
	}
}

// TestAccountStableHeadersDisabledBySwitch 开关 false → 完全不注入（逃生门语义），
// 其余既有头不受影响；New() 的生产默认必须是 true。
func TestAccountStableHeadersDisabledBySwitch(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u-200a"}
	for _, tc := range []struct {
		name string
		c    *Client
	}{
		{"显式 false", &Client{MachineIDHeaders: false}},
		{"零值 Client（未经 New 装配）", &Client{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newReq(t)
			tc.c.ChatHeaders(req, a, "", ChatMeta{})
			for _, raw := range [][]byte{rawMachineHeader, rawSessionHeader} {
				if headerPresent(req.Header, raw) {
					t.Errorf("开关关闭时不应注入设备头（原始字节 % x）", raw)
				}
			}
			// 关开关不得连带影响既有头（chat 必备头仍在）。
			for _, must := range []string{"Authorization", "X-User-Id", "X-Conversation-Request-ID"} {
				if req.Header.Get(must) == "" {
					t.Errorf("关设备头开关后 %s 缺失（应只影响设备指纹头）", must)
				}
			}
		})
	}
	if !New().MachineIDHeaders {
		t.Error("upstream.New() 必须默认打开 MachineIDHeaders（缺省 true，与上游一致）")
	}
}

// TestAccountStableHeadersOnTheWire 真出网验证（httptest 服务端读回）：chat 与 billing
// 两条业务路径收到的头名（读回原文）与取值均正确，且同 uid 两次请求值恒定。
func TestAccountStableHeadersOnTheWire(t *testing.T) {
	type got struct{ machine, session string }
	var seen []got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, got{
			machine: r.Header.Get(headerKey(rawMachineHeader)),
			session: r.Header.Get(headerKey(rawSessionHeader)),
		})
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	c := &Client{
		HTTP:             srv.Client(),
		ChatHTTP:         srv.Client(),
		ChatBaseCN:       srv.URL,
		BillingBaseCN:    srv.URL,
		MachineIDHeaders: true,
	}
	a := &auth.Auth{AccessToken: "at", UID: "u-wire"}
	do := func(path string, apply func(*http.Request)) {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		apply(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	do("/v2/chat/completions", func(req *http.Request) { c.ChatHeaders(req, a, "", ChatMeta{}) })
	do("/v2/report", func(req *http.Request) { c.BillingHeaders(req, a) })

	if len(seen) != 2 {
		t.Fatalf("服务端收到 %d 条请求 want 2", len(seen))
	}
	wantM, wantS := wantStableID("u-wire", rawPurposeMachine), wantStableID("u-wire", rawPurposeSession)
	for i, g := range seen {
		if g.machine != wantM || g.session != wantS {
			t.Errorf("请求#%d machine=%q session=%q want %q / %q", i, g.machine, g.session, wantM, wantS)
		}
	}
	if seen[0] != seen[1] {
		t.Errorf("同 uid 两次出站设备头应恒定：%+v vs %+v", seen[0], seen[1])
	}
}

// TestAccountStableHeadersInjectedOnModelCatalog 模型目录（FetchModels：CLI 目录端点
// + /v3/config 能力覆盖）与 global 目录探测同为账号级业务路径，也带设备指纹头；
// 反向：refresh 同批请求不带（两条路径必须可区分）。
func TestAccountStableHeadersInjectedOnModelCatalog(t *testing.T) {
	var modelHit, v3Hit bool
	var refreshHit bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			modelHit = true
			if r.Header.Get(headerKey(rawMachineHeader)) == "" || r.Header.Get(headerKey(rawSessionHeader)) == "" {
				t.Errorf("模型目录请求缺设备头：machine=%q session=%q",
					r.Header.Get(headerKey(rawMachineHeader)), r.Header.Get(headerKey(rawSessionHeader)))
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			v3Hit = true
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh"):
			refreshHit = true
			if r.Header.Get(headerKey(rawMachineHeader)) != "" || r.Header.Get(headerKey(rawSessionHeader)) != "" {
				t.Error("refresh 请求不应携带设备头（凭证域）")
			}
			return jsonResp(200, `{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`), nil
		default:
			return jsonResp(404, `{"code":1}`), nil
		}
	})
	// 开关走显式 true（testClient 是零值 Client；生产经 New() 默认开）。
	c.MachineIDHeaders = true
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "u-cat", ExpiresAt: 1}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if !modelHit {
		t.Fatal("未走到模型目录端点（用例失效）")
	}
	_ = v3Hit // /v3/config 覆盖是否命中取决于解析结果，不强制
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if !refreshHit {
		t.Fatal("未走到 refresh 端点（用例失效）")
	}
}

// headerPresent 判断 http.Header 里是否存在该原始头名（经 canonical 形态查表）。
func headerPresent(h http.Header, raw []byte) bool {
	_, ok := h[headerKey(raw)]
	return ok
}
