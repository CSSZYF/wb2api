package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

// reqHeaders 构造带任意头组合的请求（多头优先级 / 兼容头用例的构造口）。
func reqHeaders(h map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		// 原用例名「前缀大小写不符（规范要求精确）」把错误规范钉死了：RFC 7235 §2.1
		// 规定 auth-scheme 是大小写不敏感的 token，"Bearer" 与 "bearer" 等价。
		// 该用例记录的是错误规范，此处翻正为 true（详见 TestVerifyBearerSchemeCaseInsensitive）。
		{"auth-scheme 大小写不敏感（RFC 7235 §2.1；原用例记录的是错误规范）", "sk-abc123", "bearer sk-abc123", true},
		// 同理翻正：RFC 7235 §2.1 的 credentials = auth-scheme [ 1*SP ... ]，分隔是
		// **1*SP（一个或多个空格）**，不是恰好一个空格。原用例按「必须恰好一个空格」
		// 写，记录的也是错误规范。
		{"scheme 后多个空格（RFC 7235 §2.1 的 1*SP；原用例记录的是错误规范）", "sk-abc123", "Bearer  sk-abc123", true},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerSchemeCaseInsensitive auth-scheme 大小写不敏感（RFC 7235 §2.1）：
// bearer / Bearer / BEARER / BeArEr 一律等价；方案与凭据之间允许 1*SP（一个或多个空格）。
func TestVerifyBearerSchemeCaseInsensitive(t *testing.T) {
	const key = "sk-abc123"
	for _, authz := range []string{
		"Bearer " + key,
		"bearer " + key,
		"BEARER " + key,
		"BeArEr " + key,
		"bEaReR " + key,
		"bearer  " + key,    // 2 空格
		"Bearer     " + key, // 5 空格
	} {
		if !VerifyBearer(req(authz), key) {
			t.Errorf("VerifyBearer(authz=%q) = false, want true（scheme 大小写不敏感 + 1*SP）", authz)
		}
	}
	// 反向：scheme 名后**必须**跟至少一个空格才是 Bearer 方案。
	// "Bearerfoo" ≠ "Bearer foo"；tab 不作分隔（RFC 7235 §2.1 写的是 1*SP，只认空格）。
	for _, authz := range []string{
		"Bearersk-abc123",
		"bearer" + key,
		"Bearer\t" + key,
		"Bearer",
		"Bearer ",
	} {
		if VerifyBearer(req(authz), key) {
			t.Errorf("VerifyBearer(authz=%q) = true, want false（scheme 后必须跟 SP 且有凭据）", authz)
		}
	}
}

// TestVerifyBearerCompatHeaders OpenAI 兼容客户端常用的三个凭据头各自可用
// （此前只认 Authorization: Bearer，持有**正确密钥**的这类客户端被回 401）。
func TestVerifyBearerCompatHeaders(t *testing.T) {
	const key = "sk-abc123"
	// 头名大小写不敏感（HTTP 头名本来就大小写不敏感，Go 侧 canonical 化后同名）。
	for _, name := range []string{"x-api-key", "api-key", "x-auth-token", "X-API-Key", "API-KEY", "X-Auth-Token"} {
		t.Run(name, func(t *testing.T) {
			if !VerifyBearer(reqHeaders(map[string]string{name: key}), key) {
				t.Errorf("%s=%q 应通过（正确密钥）", name, key)
			}
			if VerifyBearer(reqHeaders(map[string]string{name: "sk-wrong"}), key) {
				t.Errorf("%s=sk-wrong 不得通过（错密钥）", name)
			}
			if VerifyBearer(reqHeaders(map[string]string{name: ""}), key) {
				t.Errorf("%s 空值不得通过（空 = 未提供，不是「未启用鉴权」）", name)
			}
			if VerifyBearer(reqHeaders(map[string]string{name: "   "}), key) {
				t.Errorf("%s 纯空白不得通过（空白不是密钥）", name)
			}
		})
	}
}

// TestVerifyBearerHeaderPriority 多头同时存在时的优先级：Authorization: Bearer 恒最优先，
// 其余按 x-api-key → api-key → x-auth-token 的顺序取第一个非空值。
//
// 关键性质（两类都断言）：
//   - 高优先头携带**错**值时，低优先头**不能**把它救回来（否则"优先级"没有意义）；
//   - 高优先头**缺/空**时，低优先头的正确值必须生效（这正是本次加固要修的 401）。
func TestVerifyBearerHeaderPriority(t *testing.T) {
	const key = "sk-abc123"
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"Bearer 对 + x-api-key 错 → Bearer 胜", map[string]string{
			"Authorization": "Bearer " + key, "x-api-key": "sk-wrong"}, true},
		{"Bearer 错 + x-api-key 对 → Bearer 胜（低优先不救）", map[string]string{
			"Authorization": "Bearer sk-wrong", "x-api-key": key}, false},
		{"无 Authorization + x-api-key 对 + api-key 错 → x-api-key 胜", map[string]string{
			"x-api-key": key, "api-key": "sk-wrong"}, true},
		{"x-api-key 错 + api-key 对 → x-api-key 胜（低优先不救）", map[string]string{
			"x-api-key": "sk-wrong", "api-key": key}, false},
		{"x-api-key 空 + api-key 对 → 空视为未提供，回落 api-key", map[string]string{
			"x-api-key": "", "api-key": key}, true},
		{"api-key 错 + x-auth-token 对 → api-key 胜（低优先不救）", map[string]string{
			"api-key": "sk-wrong", "x-auth-token": key}, false},
		{"三个兼容头全错 → false", map[string]string{
			"x-api-key": "sk-a", "api-key": "sk-b", "x-auth-token": "sk-c"}, false},
		{"非 Bearer 方案不阻断兼容头回落", map[string]string{
			"Authorization": "Basic Zm9v", "x-api-key": key}, true},
		{"Bearer 无凭据 + x-api-key 对 → 空凭据回落兼容头", map[string]string{
			"Authorization": "Bearer", "x-api-key": key}, true},
		{"Bearer 错 + 三个兼容头全对 → Bearer 胜（false）", map[string]string{
			"Authorization": "Bearer sk-wrong", "x-api-key": key, "api-key": key, "x-auth-token": key}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(reqHeaders(c.headers), key); got != c.want {
				t.Errorf("VerifyBearer(headers=%v) = %v, want %v", c.headers, got, c.want)
			}
		})
	}
}

// TestVerifyBearerQuotedCredential 成对引号剥离：只剥一层、残缺引号不剥
// （引号是某些 SDK 的拼写习惯，不该让持有正确密钥的客户端吃 401；
// 残缺引号说明客户端拼错了，按原样比较必然失败——不猜、不补）。
func TestVerifyBearerQuotedCredential(t *testing.T) {
	const key = "sk-abc123"
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"Bearer + 双引号", map[string]string{"Authorization": `Bearer "` + key + `"`}, true},
		{"Bearer + 单引号", map[string]string{"Authorization": `Bearer '` + key + `'`}, true},
		{"x-api-key 双引号", map[string]string{"x-api-key": `"` + key + `"`}, true},
		{"x-api-key 单引号", map[string]string{"x-api-key": `'` + key + `'`}, true},
		{"api-key 双引号", map[string]string{"api-key": `"` + key + `"`}, true},
		{"x-auth-token 单引号", map[string]string{"x-auth-token": `'` + key + `'`}, true},
		{"残缺：只有前引号", map[string]string{"x-api-key": `"` + key}, false},
		{"残缺：只有后引号", map[string]string{"x-api-key": key + `"`}, false},
		{"残缺：引号不成对（前双后单）", map[string]string{"x-api-key": `"` + key + `'`}, false},
		{"残缺：引号不成对（前单后双）", map[string]string{"x-api-key": `'` + key + `"`}, false},
		{"只剥一层：双层引号不通过", map[string]string{"x-api-key": `""` + key + `""`}, false},
		{"引号内为空 → 空凭据不通过", map[string]string{"x-api-key": `""`}, false},
		{"引号包裹的错 key 不通过", map[string]string{"x-api-key": `"sk-wrong"`}, false},
		{"残缺引号（Bearer 路径）", map[string]string{"Authorization": `Bearer "` + key}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(reqHeaders(c.headers), key); got != c.want {
				t.Errorf("VerifyBearer(headers=%v) = %v, want %v", c.headers, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWrongKeyShapes 错密钥的形状族一律 false：长度不同、前缀相近、
// 大小写不同、多一段少一段、key 是凭据的子串/超串、空白/控制字符包裹——全部走同一个
// 常量时间比较口，任何"提前 return 的短路径比较"都会让其中一类误过。
//
// 两条取值路径的归一化口径不同，故断言分列（两者都必须为 false）：
//   - 兼容头（x-api-key 等）：整值即凭据，**逐字比较**，不做任何裁剪；
//   - Authorization：scheme 后的 1*SP 是结构分隔（合法），凭据本体逐字比较。
//     故"前导空格"形态在 Authorization 路径上与合法多空格写法不可区分，
//     不列入本用例（见 TestVerifyBearerSchemeCaseInsensitive 正向断言）。
func TestVerifyBearerWrongKeyShapes(t *testing.T) {
	const key = "sk-abc123"
	// 兼容头路径：整值即凭据，逐字比较（含空白包裹形态）。
	for _, tok := range []string{
		"", "sk-abc12", "sk-abc1234", "sk-abc123 ", " sk-abc123", "  sk-abc123  ",
		"SK-ABC123", "sk-ABC123", "sk-abc123\n", "sk-abc123x", "xsk-abc123",
		"sk-ab", "sk-", "sk-abc123-sk-abc123", "sk-abc124",
		"Bearer", "bearer", "null", "undefined", "0", "false", "-", "total",
	} {
		if VerifyBearer(reqHeaders(map[string]string{"x-api-key": tok}), key) {
			t.Errorf("x-api-key=%q 不得通过（错密钥形状）", tok)
		}
	}
	// Authorization 路径：scheme 之后的凭据本体逐字比较（尾随空格属于凭据，不裁）。
	// 前导空格形态不在此列：scheme 后的 1*SP 是合法结构分隔，"Bearer   sk-x"
	// 与合法的多空格写法在规范层面不可区分（见 TestVerifyBearerSchemeCaseInsensitive）。
	for _, tok := range []string{
		"", "sk-abc12", "sk-abc1234", "sk-abc123 ",
		"SK-ABC123", "sk-ABC123", "sk-abc123\n", "sk-abc123x", "xsk-abc123",
		"sk-ab", "sk-", "sk-abc123-sk-abc123", "sk-abc124",
		"null", "undefined", "0", "false",
	} {
		if VerifyBearer(req("Bearer "+tok), key) {
			t.Errorf("Authorization=%q 不得通过（错密钥形状）", "Bearer "+tok)
		}
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
	// 全空头集合（含显式空值的兼容头）同样不得通过。
	if VerifyBearer(reqHeaders(map[string]string{"x-api-key": "", "api-key": "", "x-auth-token": ""}), "any-key") {
		t.Error("empty compat headers must not pass")
	}
}

// TestVerifyBearerNilRequest nil 请求不得 panic（防御：调用方理论上恒传非 nil，
// 但鉴权是安全边界，宁可返回 false 也不能崩）。
func TestVerifyBearerNilRequest(t *testing.T) {
	if VerifyBearer(nil, "any-key") {
		t.Error("nil request must not pass")
	}
	if !VerifyBearer(nil, "") {
		t.Error("nil request with empty key must pass（未启用鉴权）")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

// TestCompareCredentialIsTheOnlyComparison 所有取值路径必须收敛到同一个比较口：
// 本用例锁定 compareCredential 的语义（错 → false、对 → true），
// 防止后续有人把某条路径改回 `==` 或 bytes.Equal 的短路径比较。
func TestCompareCredentialIsTheOnlyComparison(t *testing.T) {
	if !compareCredential("sk-x", "sk-x") {
		t.Error("compareCredential(same) = false, want true")
	}
	if compareCredential("sk-x", "sk-y") {
		t.Error("compareCredential(diff) = true, want false")
	}
	if compareCredential("", "sk-x") {
		t.Error("compareCredential(empty, key) = true, want false")
	}
	// 长度不同也必须是"比较后为假"，而不是提前返回的假（形状由 digest 吸收）。
	if compareCredential("sk-x", "sk-x-longer") {
		t.Error("compareCredential(shorter) = true, want false")
	}
	if compareCredential("sk-x-longer", "sk-x") {
		t.Error("compareCredential(longer) = true, want false")
	}
}
