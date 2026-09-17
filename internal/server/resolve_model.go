package server

import "strings"

// splitRealmPrefix 解析显式 realm 前缀协议（PLAN D6）："[realm:]model"。
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离（大小写敏感）；否则 ok=false 并原串返回。
// 非 "cn"/"global" 的前段（含冒号的模型名）整体视为裸名，不会被误切成域前缀。
func splitRealmPrefix(model string) (realm, bare string, ok bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "", model, false
	}
	return prefix, model[idx+1:], true
}

// RealmRouter 模型名 → realm 归属决策的唯一真相（handler 路由与 cmd 粘性闭包共用）。
//
// 规则：
//  1. 显式 "cn:"/"global:" 前缀优先——老客户端配置里写死的带前缀模型名零改动。
//  2. 裸名按池内可用域决定：只有一域有账号 → 该域；两域都有 → Precedence；
//     两域都无 → Precedence（随后选号必然 503，与历史行为一致）。
//
// 单域部署（例如只登国际版账号）下裸名直接落到该域；配合 strip_realm_prefix
// 即得到"输出无前缀、且裸名路由正确"的语义。
type RealmRouter struct {
	// GlobalEnabled 与 handler 第三道闸同源（config global.enabled）。
	// false 时 global 域视为不存在（纯 CN 锁定逃生门）。
	GlobalEnabled bool
	// Precedence 两域都有账号时裸名的默认域："global"（缺省）或 "cn"。
	Precedence string
	// HasRealm 池内该域是否有账号；nil 视为无账号（测试可只给 Precedence）。
	HasRealm func(realm string) bool
}

// Resolve 解析模型名为 (realm, bareModel)。bare 用于选号/粘性/出站 body 重写
// （前缀是网关侧路由协议，上游只认裸名）。
func (r RealmRouter) Resolve(model string) (realm, bare string) {
	realm, bare, _ = r.ResolveWithSource(model)
	return realm, bare
}

// ResolveWithSource 同 Resolve，并额外报告 realm 是否来自显式前缀（用户强指定）。
//
// 显式性与跨域回落的关系（issue #199c）：显式 "cn:"/"global:" 前缀是客户端**写明**
// 的意图，本域全不可用时也不跨域——换域可能违反用户意图（例如 CN 前缀是国际号受限
// 时的显式规避，回落 global 等于把他送回去）；裸名归属只是网关的默认倾向
// （realm_precedence），本域不可用时回落另一域是"尽量别 503"的合理默认。
// 故 handler 据此对两种来源传不同的选号入口（硬过滤 vs 软优先）。
func (r RealmRouter) ResolveWithSource(model string) (realm, bare string, explicit bool) {
	if realm, bare, ok := splitRealmPrefix(model); ok {
		return realm, bare, true
	}
	return r.BareRealm(), model, false
}

// BareRealm 返回裸模型名的归属域。
func (r RealmRouter) BareRealm() string {
	has := func(realm string) bool { return r.HasRealm != nil && r.HasRealm(realm) }
	cn, gl := has("cn"), r.GlobalEnabled && has("global")
	switch {
	case gl && !cn:
		return "global" // 单域：只登国际版账号 → 裸名全走国际版
	case cn && !gl:
		return "cn"
	}
	if r.Precedence == "cn" {
		return "cn"
	}
	return "global"
}
