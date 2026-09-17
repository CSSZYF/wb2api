// Package logfmt 统一网关日志的 uid 截断与模块前缀约定。
//
// 约定：
//   - uid 统一截 8 位：与 chat 流水行（internal/server/logging.go uidPrefix）对齐，
//     日志行只留 uid 前 8 位。全量 uid 可从 data/state.json 查（54 个号无 8 位前缀碰撞）。
//   - 模块前缀：调度四类已有天然前缀（travel/activity/checkin/keepalive）保持；
//     其他补 [pool]/[auth]/[server] 等 [mod] 方括号前缀，redisstore/session 已有保持。
//   - 级别语义：正常流转不打级别字样（保持简洁）；可疑/降级/失败行加 WARN:/ERR: 前缀。
//
// 本包不引入日志库，只提供 UID8 / Truncate 截断 helper，供各包替代裸写 [:8] 与
// 按字节切 [:n]（防 uid 短于 8 越界、防多字节字符被切半出乱码）。
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
func UID8(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
