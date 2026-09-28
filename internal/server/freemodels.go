// freemodels.go 「免费 / 低价模型」判定闭包（保留积分 pool.reserve_credits 的数据源）。
//
// 保留积分的语义是"余额触底时只放行免费/低价模型"，判定必须**模型感知**——池层不认识
// 模型目录（架构约束），故由本包把两个目录缓存折算成一个纯函数回调，经
// pool.SetFreeModelLookup 注入。这与 SetMaxInFlight 那批依赖注入同形，也避免了
// pool → upstream/server 的反向依赖。
//
// 为什么放 server 而不是 upstream：判定要同时看**两个**目录出口——CN 侧
// cachedModelsSnapshot（本包内的包级缓存）与 global 侧
// upstream.GlobalModelInfosSnapshot。只有 server 包同时够得着这两个（main 装配时
// 也能拼，但那会把"哪些目录算数"的知识摊到 main；放这里与 hintContext 同层）。
//
// 纪律（与 /v1/stats 的倍率透出完全一致）：
//   - **只读快照，零上游调用**：本回调在**选号热路径**上被调用，任何上游请求都会被
//     放大成对上游的额外压力；
//   - **不回调 pool**：它在 p.mu 内被调用（见 pool.SetFreeModelLookup 的契约注释）。
//     cachedModelsSnapshot 与 GlobalModelInfosSnapshot 各自只取本包/upstream 的锁，
//     与 p.mu 之间不存在反向获取（没有路径持目录锁再去取 p.mu），故无环。
package server

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// freeModelLookup 构造「该模型此刻是否免费/低价」的判定闭包。
//
// 数据源（两域的目录快照取**并集**）：CN 侧 cachedModelsSnapshot（10min 缓存）+ global
// 侧 upstream.GlobalModelInfosSnapshot（1h 缓存）。并集而非"按 realm 查对应域"的取舍：
// 同名模型在两域可能价差（如活动价只挂一域），而本闸门的用户意图是"免费的别被底线拦住"
// ——任一域说它免费就放行，宁可多花一点点底线，也不要让用户点名要保的免费模型用不了。
// 反方向的代价是有界的：并集只会让**更少**的号被排除，即"更宽松"，不会凭空掐死服务。
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
		if upstream.IsFreeModel(cachedModelsSnapshot(), model) {
			return true
		}
		if up == nil {
			return false
		}
		return upstream.IsFreeModel(up.GlobalModelInfosSnapshot(), model)
	}
}

// FreeModelLookup 导出形态（main 装配时注入 pool.SetFreeModelLookup）。
//
// 与 freeModelLookup 同体，仅命名对外：main 需要它作为**装配参数**传给池，而
// freeModelLookup 是包内实现细节（改名/改数据源不影响 main）。
func FreeModelLookup(up *upstream.Client) func(model string) bool {
	return freeModelLookup(up)
}
