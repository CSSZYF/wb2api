// Package logfmt 统一网关日志的 uid 截断与模块前缀约定。
//
// 约定：
//   - uid 统一截 8 位：与 chat 流水行（internal/server/logging.go uidPrefix）对齐，
//     日志行只留 uid 前 8 位。全量 uid 可从 data/state.json 查（54 个号无 8 位前缀碰撞）。
//   - 账号标识统一走 Label：有昵称打 "昵称(uid8)"（昵称认人、uid8 供 grep），无昵称
//     退回 uid8——同一账号在流水行与调度日志里同一写法。
//   - 模块前缀：调度四类已有天然前缀（travel/activity/checkin/keepalive）保持；
//     其他补 [pool]/[auth]/[server] 等 [mod] 方括号前缀，redisstore/session 已有保持。
//   - 级别语义：正常流转不打级别字样（保持简洁）；可疑/降级/失败行加 WARN:/ERR: 前缀。
//
// 本包不引入日志库，只提供 UID8 / Label 账号标签 / DisplayWidth+Pad 显示宽对齐 /
// Truncate 按 rune 边界截断几个纯字符串 helper，供各包替代裸写 [:8]、按字节切 [:n]
// 与按字节数补表格空格（防 uid 短于 8 越界、防多字节字符被切半出乱码、防中文昵称
// 按字节补齐导致整列错位）。
package logfmt

import (
	"strings"
	"unicode/utf8"
)

// Truncate 截断字符串到 n 字节上限（先 TrimSpace，与旧 upstream/内部实现口径
// 一致），切点落在多字节字符中间时回退到 UTF-8 rune 边界——错误 body 多为中文
// （"将在 … 重置"），按字节切会出半截序列乱码。短于 n 原样返回；n<=0 返回空串；
// 超长（截断发生时）在末尾补 "…" 省略标记，让「内容不完整」这件事自身可见。
//
// 契约（与 Pad 的 width<=0 守卫风格对齐）：
//   - n<=0 → 空串（负数直接 s[:n] 会 panic: slice bounds out of range，入口守卫掉）；
//   - 先 TrimSpace 再判长度（沿用旧实现口径，首尾空白不算内容）；
//   - 超长时逐字节回退到 rune 边界（该多字节字符整个让出）后补 "…"：n 是**保留
//     前缀的字节上限**，省略标记是额外 3 字节（与 panel.truncateStr 的 s[:n]+"…"
//     口径一致，便于阅读时区分「原文就这么长」与「被截断了」）。
//
// 与上游（sliver）同名函数的差异：上游 Truncate 只回退 rune 边界、不补 "…"。
// 本仓按自身日志/错误片段口径补标记，故合并上游 diff 时此处必然出现差异，
// 属有意为之（不是漏同步）。
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) > n {
		// s[n] 是切点后的首字节：是 rune 的后续字节（continuation）说明切点落在
		// 多字节字符中间，逐字节回退到 rune 边界（该字符整个让出）。
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n] + "…"
	}
	return s
}

// UID8 返回 uid 的前 8 位；空 uid 返回 "-"（与 server.uidPrefix 对齐）。
//
// 用于调度类与非调度类日志行，把 <task> <full-uid>: ... 改为 <task> <uid8>: ...
// 全量 uid 留在 state.json 供排查，日志里 8 位足够唯一定位。
//
// 契约（不可放松）：返回值恒不长于 8 字符——全量 uid 绝不进日志（见
// TestLabelNeverLeaksFullUID 与 server 侧 logging_test 的 "full uid leaked" 断言）。
func UID8(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// Label 返回日志里的账号标签，形如 "示例昵称甲(a1b2c3d4)"；昵称为空时退回 "a1b2c3d4"。
//
// 为什么需要：uid8 是机器标识，排障时人眼无法直接判断「刚才那个 429/6004 是哪个号」，
// 必须再拿 uid8 去 auths/ 或 data/state.json 反查昵称，一条日志要多跳一步。昵称随
// 登录落在 auths/<uid>.json 的 account.nickname，这里把它与 uid8 拼成可直接辨认的
// 标签——昵称认人、uid8 供 grep，两者都保留。
//
// 规则（与上游 sliver f736008 逐条对齐）：
//   - uid 一律经 UID8 截断 → 全量 uid 绝不进日志（这是硬契约，不是风格问题）；
//   - 昵称先 TrimSpace：仅空白（旧 auth 文件没落 account.nickname）视同无昵称，退回 uid8；
//   - uid 短于 8 位原样使用、不补零（"abc" → "sample(abc)"）；
//   - uid 与昵称皆空返回 "-"（与 UID8 口径一致，避免打出 "(-)"）；
//   - 昵称内的括号不转义（"猫(测试)" → "猫(测试)(a1b2c3d4)"）：日志只供人眼与 grep，
//     转义反而让昵称与账号表里的写法对不上。
func Label(uid, nick string) string {
	short := UID8(uid)
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return short
	}
	return nick + "(" + short + ")"
}

// DisplayWidth 返回 s 的终端显示列宽：CJK / 全角 / emoji 记 2 列，其余记 1 列。
//
// 存在意义：账号昵称是用户自定的中文（"猫" 是 3 字节但占 2 列，"sample" 是 6 字节占
// 6 列），用 len()（字节数）做表格对齐会导致列宽忽宽忽窄——中文昵称列比英文昵称列少补
// 一半空格，整张表往上缩。Go 标准库没有显示宽度函数，本仓库不引 go-runewidth
// （保持零第三方依赖），故内置这份覆盖常见宽字符区段的判定。
//
// 已知近似（与上游同口径，不打算「修正」）：
//   - 组合字符（如 U+0301 重音）本身按 1 列计、不归零，"e\u0301" 算 2 列而终端显示
//     1 列（要做对需 NFC 归一化，代价远超收益）；
//   - 零宽连接符序列（emoji ZWJ 家庭/职业）逐 rune 计，会多于实际列宽；
//   - 只覆盖 runeWidth 列出的区段，未收录的宽字符（如 U+2764 心形）按 1 列计。
//
// 这些情形在账号昵称里罕见，而「中文/全角按 2 列」这一条覆盖了实际会遇到的全部场景。
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// Pad 把 s 右补空格到 width 显示列宽；已超宽或 width<=0 时原样返回（不截断）。
//
// 只补不截：截断会丢信息（昵称是排查主线索），超宽时让该行自然变宽，保持内容完整。
// width<=0 守卫与 Truncate 的 n<=0 风格一致——入口拦掉无意义输入，不制造空串。
func Pad(s string, width int) string {
	if width <= 0 {
		return s
	}
	if d := width - DisplayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// runeWidth 单个 rune 的显示列宽。区段判定取自 Unicode East Asian Width 的
// Wide/Fullwidth 集合（与 go-runewidth 的默认表口径一致），只保留实际会用到的段；
// 不引第三方库是刻意的（零依赖优先于完备性，见 DisplayWidth 的已知近似）。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		// 控制字符（含 DEL/C1）不占位：日志里若混入 \t \r 不破坏列宽计算。
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f: // Hangul Jamo 初声
		return 2
	case r == 0x2329 || r == 0x232a: // 〈 〉 数学尖括号（Wide）
		return 2
	case r >= 0x2e80 && r <= 0xa4cf && r != 0x303f: // CJK 部首…Yi（303f 是窄字符）
		return 2
	case r >= 0xac00 && r <= 0xd7a3: // Hangul 音节
		return 2
	case r >= 0xf900 && r <= 0xfaff: // CJK 兼容表意
		return 2
	case r >= 0xfe30 && r <= 0xfe6f: // CJK 兼容形式（含全角标点）
		return 2
	case r >= 0xff00 && r <= 0xff60: // 全角 ASCII
		return 2
	case r >= 0xffe0 && r <= 0xffe6: // 全角符号（￠￡￥…）
		return 2
	case r >= 0x1f300 && r <= 0x1f9ff: // emoji
		return 2
	case r >= 0x20000 && r <= 0x3fffd: // CJK 扩展 B 及以后
		return 2
	}
	return 1
}
