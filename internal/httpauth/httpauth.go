// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
//
// 取值口径（2026-09-27 加固，对齐 hub 291c76c）：客户端**怎么把密钥递过来**不影响
// 它能不能通过鉴权。具体三处修正：
//
//  1. auth-scheme 大小写不敏感。RFC 7235 §2.1 明确 auth-scheme 是大小写不敏感的
//     token（`credentials = auth-scheme [ 1*SP ... ]`），故 "bearer sk-x" 与
//     "BEARER sk-x" 必须和 "Bearer sk-x" 等价。旧实现用 strings.HasPrefix 逐字比
//     "Bearer "，把 `bearer`/`BEARER` 一律判 401——**这不是加固前"更严格"，而是
//     按错规范实现**：代码注释与测试都写着"规范要求精确"，实际规范要求相反。
//     同时按 1*SP 容忍 scheme 后多个空格（旧实现要求恰好一个）。
//  2. 认 OpenAI 兼容客户端常用的 x-api-key / api-key / x-auth-token。只认
//     Authorization 会让**持有正确密钥**的这类客户端吃 401（误判成"密钥错"）。
//  3. 剥掉凭据两端**成对**的单/双引号（只剥一层）。引号是部分 SDK 的拼写习惯；
//     残缺引号不剥（说明客户端拼错了，按原样比较必然失败——不猜、不补）。
//
// 强度不降：所有取值路径**统一收敛到 compareCredential 这一个比较函数**，
// 仍是 SHA-256 摘要 + subtle.ConstantTimeCompare。选择"用哪个头"的分支发生在
// 比较**之前**且只依赖请求自身的形状（与密钥值无关），故不泄露密钥信息；
// 比较本身恒发生、无 `==`、无按长度/前缀提前返回的短路径。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerScheme auth-scheme 名（小写）。比较时用 strings.EqualFold：
// RFC 7235 §2.1 规定 auth-scheme 大小写不敏感。
const bearerScheme = "bearer"

// compatCredentialHeaders 无 Authorization 时的兼容头，**按此顺序取第一个非空值**。
//
// 顺序即优先级，选定理由：
//   - x-api-key 最优先：OpenAI 官方 SDK / 多数兼容客户端在自定义头里用它，生态最广；
//   - api-key 次之：部分代理与网关（含 hub 的 Python 实现）用这个短名；
//   - x-auth-token 最后：多见于内部工具，语义最弱（"token"不一定是 api key）。
//
// 三者都排在 Authorization 之后（见 credentialFromRequest）：Authorization 是
// RFC 7235 的标准承载头，显式带方案、歧义最小，故恒为第一优先。
// "取第一个非空"是刻意的——高优先头携带**错**值时不得被低优先头"救回"，
// 否则优先级形同虚设，且同一请求可能因头的顺序不同得到不同结果（不可复现）。
var compatCredentialHeaders = []string{"x-api-key", "api-key", "x-auth-token"}

// VerifyBearer 校验请求头是否携带正确的密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 请求为 nil 时按"未提供凭据"处理（返回 false；key 为空仍放行）——鉴权是安全
// 边界，宁可拒绝也不 panic。
//
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	if r == nil {
		return false
	}
	// 唯一比较点：候选凭据为空时同样走一次摘要比较（形状一致），
	// 且因 key != "" 恒为假——不需要额外分支。
	return compareCredential(credentialFromRequest(r), key)
}

// compareCredential 全部取值路径的唯一比较口：SHA-256 摘要 + 常量时间比较。
//
// 必须保持"先摘要、后定长比较"的形状：不写 `got == key`、不按长度提前返回、
// 不做前缀预筛。任何"快路径"都会把密钥的长度与公共前缀暴露给计时侧信道，
// 也会让"前缀相近的错 key"在某个分支上误过（见 TestVerifyBearerWrongKeyShapes）。
func compareCredential(got, key string) bool {
	return subtle.ConstantTimeCompare(digest(got), digest(key)) == 1
}

// credentialFromRequest 按优先级取出请求携带的凭据原文；全部缺失 → 空串。
//
// 优先级：Authorization: Bearer > x-api-key > api-key > x-auth-token。
// 空值/纯空白之外的判定不做裁剪：兼容头的整值即凭据（逐字比较），故带空格包裹的
// 值必然比较失败——这是"不猜客户端意图"的代价，比替它 trim 更安全（trim 会把
// "sk-x " 与 "sk-x" 视为同一凭据，等于悄悄扩大可接受形态）。
//
// 注意本函数只做**选择**，不做比较：分支只依赖请求头的存在性与形状，
// 与 key 无关，因此不构成侧信道。
func credentialFromRequest(r *http.Request) string {
	// Authorization 的凭据按 RFC 7235 解析（scheme 大小写不敏感 + 1*SP）。
	if c := normalizeCredential(bearerCredential(r.Header.Get("Authorization"))); c != "" {
		return c
	}
	for _, name := range compatCredentialHeaders {
		if c := normalizeCredential(r.Header.Get(name)); c != "" {
			return c
		}
	}
	return ""
}

// bearerCredential 从 Authorization 头值解析 Bearer 凭据；非 Bearer 方案 → 空串。
//
// 解析口径（RFC 7235 §2.1 `credentials = auth-scheme [ 1*SP ( token68 / #auth-param ) ]`）：
//   - auth-scheme 大小写不敏感（bearer/Bearer/BEARER/BeArEr 等价）；
//   - scheme 与凭据之间是 1*SP（一个或多个空格），不是恰好一个；
//   - scheme 名后必须跟空格才算分隔（"Bearersk-x" 不是 Bearer 方案）；
//   - 只认 SP 作分隔（规范写的是 SP，不把 tab 当分隔，也不 trim 凭据本体）。
//
// 非 Bearer 方案（如 Basic）返回空串，让调用方继续回落兼容头：方案不匹配说明
// 客户端在用别的鉴权机制，不代表它没带本网关的密钥。
func bearerCredential(authz string) string {
	i := strings.IndexByte(authz, ' ')
	if i < 0 {
		return ""
	}
	if !strings.EqualFold(authz[:i], bearerScheme) {
		return ""
	}
	return strings.TrimLeft(authz[i:], " ") // 吃掉 1*SP
}

// normalizeCredential 剥掉凭据两端**成对**的同种引号，只剥一层。
//
// 成对才剥（前后必须是同一个引号字符）：`"sk-x"` / `'sk-x'` → sk-x；
// `"sk-x` / `sk-x"` / `"sk-x'` 一律原样返回（残缺引号是客户端拼错，按原样比较
// 必然失败，不替它猜）。只剥一层：`""sk-x""` → `"sk-x"`（仍不匹配），
// 不做递归剥离——递归会把"引号本身是密钥字符"的部署变成不可表达。
func normalizeCredential(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q != '"' && q != '\'') || s[len(s)-1] != q {
		return s
	}
	return s[1 : len(s)-1]
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
