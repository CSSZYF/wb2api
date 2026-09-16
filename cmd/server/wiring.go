package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带显式 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须用
// 与 handler 同一套 router 解析出 realm + bareModel，再交给分池选号域过滤——否则裸名
// 取池子全集，global 号会被粘性分配给 CN 请求（跨 realm 泄漏）。
//
// 裸名的域归属与请求路由同源（RealmRouter：单域部署落唯一可用域，多域按
// realm_precedence）。历史行为「裸名一律 cn」已废除：那会让只登国际版账号的部署
// 在裸名上恒 503。
func realmAwareAvailableForModel(p *pool.Pool, router server.RealmRouter) func(model string) []string {
	return func(model string) []string {
		realm, bare := router.Resolve(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}

// realmRouter 由配置 + 池构造模型名域归属决策器。handler 与粘性闭包共用同一份，
// 避免"列表按一种口径、路由按另一种口径"的漂移。
func (c *Config) realmRouter(p *pool.Pool) server.RealmRouter {
	r := server.RealmRouter{
		GlobalEnabled: c.Global.Enabled,
		Precedence:    c.Models.RealmPrecedence,
	}
	if p != nil {
		r.HasRealm = p.HasRealm
	}
	return r
}
