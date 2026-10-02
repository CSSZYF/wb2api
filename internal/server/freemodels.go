// freemodels.go 「免费 / 低价模型」判定闭包（保留积分 pool.reserve_credits 的数据源）。
//
// 保留积分的语义是"余额触底时只放行免费/低价模型"，判定必须**模型感知**——池层不认识
// 模型目录（架构约束），故由本包把两个目录缓存折算成一个纯函数回调，经
// pool.SetFreeModelLookup 注入。这与 SetMaxInFlight 那批依赖注入同形，也避免了
// pool → upstream/server 的反向依赖。
//
// 为什么放 server 而不是 upstream：判定要同时看**两个**目录出口——CN 侧
// cachedCatalogSnapshot（本包内的包级缓存）与 global 侧
// upstream.GlobalModelInfosStaleSnapshot。只有 server 包同时够得着这两个（main 装配时
// 也能拼，但那会把"哪些目录算数"的知识摊到 main；放这里与 hintContext 同层）。
//
// 纪律（与 /v1/stats 的倍率透出完全一致）：
//   - **只读快照，零上游调用**：本回调在**选号热路径**上被调用，任何上游请求都会被
//     放大成对上游的额外压力；
//   - **不回调 pool**：它在 p.mu 内被调用（见 pool.SetFreeModelLookup 的契约注释）。
//     cachedCatalogSnapshot 与 GlobalModelInfosStaleSnapshot 各自只取本包/upstream 的锁，
//     与 p.mu 之间不存在反向获取（没有路径持目录锁再去取 p.mu），故无环。
package server

import (
	"context"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// freeModelLookup 构造「该模型此刻是否免费/低价」的判定闭包。
//
// 数据源（两域的目录快照取**并集**）：CN 侧 cachedCatalogSnapshot + global 侧
// upstream.GlobalModelInfosStaleSnapshot。并集而非"按 realm 查对应域"的取舍：
// 同名模型在两域可能价差（如活动价只挂一域），而本闸门的用户意图是"免费的别被底线拦住"
// ——任一域说它免费就放行，宁可多花一点点底线，也不要让用户点名要保的免费模型用不了。
// 反方向的代价是有界的：并集只会让**更少**的号被排除，即"更宽松"，不会凭空掐死服务。
//
// 为什么用**陈旧容错**的快照（cachedCatalogSnapshot / GlobalModelInfosStaleSnapshot）
// 而不是 TTL 版（cachedModelsSnapshot / GlobalModelInfosSnapshot）：
// 后两者的 TTL（10min / 1h）是给 /v1/models 与 /v1/stats 的展示口径定的（面板实时、
// API 缓存），过期即"没有目录"、调用方回落静态表——那对展示是合理的。但本判定的
// 问题是"这个模型此刻要不要花钱"，判错的代价是**误拦免费模型**（用户的免费模型用不了
// = 本功能要修的病灶）。而 /v1/models 的唯一调用方是客户端，绝大多数只在启动时拉一次：
// 启动 10 分钟后 CN 快照恒为 nil → 免费判定把 CN 免费模型全误拦，且**每 10 分钟复发**。
// 上游的对应取舍（a4557dc / ModelRate）是倍率表跨刷新持久、读时不判过期，本函数对齐。
//
// 每次调用现算（不额外缓存）：一次选号至多调一次（pool 侧 reserveGate 按 reqModel 现算
// 一次后全候选共用），两个快照都是只读缓存命中，成本是遍历约数十条条目。刻意**不加**
// 第三层缓存——那会引入一个新的失效面（"折扣结束了但免费集合还留着旧值"），而目录
// 缓存本身已是分钟级新鲜度。
func freeModelLookup(up *upstream.Client) func(model string) bool {
	return func(model string) bool {
		if model == "" {
			return false // 空模型名不是模型（缺失 ≠ 免费），保守口径由 pool 侧兜底
		}
		if upstream.IsFreeModel(cachedCatalogSnapshot(), model) {
			return true
		}
		if up == nil {
			return false
		}
		return upstream.IsFreeModel(up.GlobalModelInfosStaleSnapshot(), model)
	}
}

// FreeModelLookup 导出形态（main 装配时注入 pool.SetFreeModelLookup）。
//
// 与 freeModelLookup 同体，仅命名对外：main 需要它作为**装配参数**传给池，而
// freeModelLookup 是包内实现细节（改名/改数据源不影响 pool 装配）。
func FreeModelLookup(up *upstream.Client) func(model string) bool {
	return freeModelLookup(up)
}

// WarmModelCatalog 启动预热两个域的模型目录缓存（保留积分的"免费判定"数据源）。
//
// 为什么必须有它（对齐上游 a4557dc 的同一问题）：本判定读的是**只读快照**
// （cachedCatalogSnapshot + GlobalModelInfosStaleSnapshot，两者都陈旧容错），而快照
// 只在**懒触发**路径上填充——CN 侧是 /v1/models 或面板「模型与档位」，global 侧是
// /v1/models（global 分支）或面板按 global 域查询。于是重启后到首次触发之间有一段
// **空窗期**：快照**从未成功过**（nil，没有"最近一次成功目录"可回落）→ 判定按既有
// 契约「缺失 ≠ 免费」答"非免费" → 余额触底的号把免费模型也一并拦掉（只剩内置白名单
// 那三个可用）。
//
// 上游对同一问题的实测（a4557dc 的 body）：「重启后 2 分钟，97 分的账号打收费模型
// 归零；倍率表当时尚未建立」。我们的对应症状是"重启后免费模型不可用"，同样是本功能
// 要修的病灶，故必须预热。
//
// 三条纪律（与上游 warmModelRates 逐条对齐）：
//   - **异步**：由调用方 go 出去（本函数自身同步执行、不阻塞监听启动）；调用方若需
//     严格串行（测试）可直接调用；
//   - **失败不致命**：单域失败只记 WARN——后续懒触发仍会补上，不得因预热失败而
//     拒绝启动（目录端点是上游的次要 RPC，偶发 5xx 不该影响网关可用性）；
//   - **逃生门**：GlobalEnabled=false（global realm 关锁）时不探 global 域——此时
//     该域账号按 CN 处理，探 global 目录是无意义的上游调用。
//
// 每域取一个**可用账号**发探测（与面板 models 同口径）：不额外放大上游调用——每次
// 预热至多两次探测（CN 一次 + global 一次），且两者本就会被首次 /v1/models 触发。
func WarmModelCatalog(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	if up == nil || p == nil || ctx.Err() != nil {
		return // 进程正在退出（SIGINT/SIGTERM）时不发起新的上游调用
	}
	// CN：有可用 CN 账号才拉（无账号即无凭证，与面板 models 同口径）。走
	// fetchCNCatalogAndCache——与懒触发路径共用同一份"怎么拉、怎么写缓存"的知识
	// （含 5min 失败负缓存），避免两处各写一遍必然漂移。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if infos := fetchCNCatalogAndCache(up, p); len(infos) == 0 {
			log.Printf("WARN: [server] warm model catalog (cn): empty model list")
		}
	}
	// global：独立目录端点，仅在其路由开关开启时探测（逃生门关锁时该域按 CN 处理）。
	// 必须放在 CN 之后并重新检查 ctx：进程退出时不再发起第二个域的上游调用。
	if !up.GlobalEnabled || ctx.Err() != nil {
		return
	}
	if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			// FetchGlobalModelInfos 无错误返回（内部 5min 负缓存自行节流），
			// 仅按结果条数判断是否拿到目录。
			if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
				log.Printf("WARN: [server] warm model catalog (global): empty model list")
			}
		}
	}
}
