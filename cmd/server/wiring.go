package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// newSessionRouter 按配置装配会话粘性路由（main 与 cmd/server 的接线测试共用，
// 避免"测试里另写一份装配"导致接线回归测不到）。
//
// 两处依赖值得单独说明：
//   - AvailableForModel：realm 感知闭包（见 realmAwareAvailableForModel）——粘性候选
//     必须与请求路由同一套域归属，否则 global 号会被分给 CN 请求；
//   - Sequential：pool.pick_mode 的粘性侧镜像。**这条接线是 v1.9.26 的缺陷所在**：
//     顺序模式只改了 Pool.Pick 的挑选方式，而"新会话绑到哪个号"走的是
//     session.Router.assign（另一条分配路径），缺了这条接线时带会话键的客户端
//     （绝大多数）仍被哈希分散到各号，顺序模式形同未开。
func newSessionRouter(cfg *Config, p *pool.Pool, store redisstore.Store) *session.Router {
	return session.New(session.Config{
		TTL:               cfg.SessionTTL,
		GCInterval:        cfg.SessionGCInterval,
		Store:             store,
		Available:         p.AvailableUIDs,
		AvailableForModel: realmAwareAvailableForModel(p, cfg.realmRouter(p)),
		Sequential:        PickMode(cfg.Pool.PickMode) == pool.PickSequential,
	})
}

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
