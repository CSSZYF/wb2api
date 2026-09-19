// stats.go GET /v1/stats 请求统计端点（按模型聚合 token / 缓存 / 延迟）。
//
// 背景：网关此前只有逐请求的表格日志（每行一条），没有结构化累加——运维要回答
// 「某模型烧了多少 token、缓存命中多少、延迟多少」只能翻日志 grep，社区面板
// （287775856/workbuddy2api-gui）也依赖该端点，此前打过来是 404。
//
// 数据来源与设计取舍：
//
//   - **复用既有 usage 记录器**，不新建内存计数器。上游 733d348 的实现是纯内存
//     累加（重启清零、无落盘），因为它那边没有现成的用量账；我们已有
//     internal/usage（按 (时间片, 域, 账号, 模型) 分桶 + 落盘 + 面板在用），再建
//     一份就是第二本账——两处埋点迟早漂移，且会出现「面板看得见、/v1/stats 看
//     不见」这类极难发现的不一致。故本端点只做投影：读一次桶快照 → 按模型求和
//     折算，零新增写入路径。
//
//   - **只读缓存快照，不触发上游请求**。与 /v1/models 的差异点：那里缓存冷时会
//     FetchModels（打上游），这里永远只读本地桶——统计端点被面板高频轮询，任何
//     上游调用都会变成对上游的额外压力（与 cachedModelsSnapshot 同一纪律）。
//
//   - **字段名逐字对齐上游 schema**（见 internal/usage/stats.go 的 StatsModel），
//     消费方按上游约定解析即可，无需为本仓特判。
//
//   - 鉴权与其余 /v1/* 同口径（Bearer，api_key 为空则放行）。
//
// 与上游的口径差异（实现时逐条确认，写在这里便于对账）：
//   - success/failed 按「这次尝试是否拿到上游 usage」判定（上游按 HTTP 状态码）；
//   - since/uptime_sec 是**数据覆盖区间**（跨重启），不是进程运行时长；
//   - 不提供 POST /v1/stats/reset：我们的桶是长期落盘账，清空等于永久删历史，
//     与「用量视图」共用同一份数据——不能为了统计口径把面板的历史一起抹掉。
//     需要观察增量用 ?hours= 取窗口（见下）。
package server

import (
	"net/http"
	"strconv"
	"time"
)

// statsDefaultHours 缺省窗口：0 = 不限（全量累计，与上游 /v1/stats 同口径）。
// 刻意不设默认窗口——统计端点的语义是「至今累计」，设默认窗口会让数值随查询
// 时间漂移，对账时误以为流量掉了。
const statsDefaultHours = 0

// statsMaxHours 窗口上限（60 天）：再往前都是日桶，精度不再变化，限幅只为
// 挡住 hours=999999999 这类无意义入参（不改变任何结果，只避免无谓遍历）。
const statsMaxHours = 24 * 60

// stats 处理 GET /v1/stats：按模型聚合的请求统计。
//
// 查询参数 hours：可选，只统计最近 N 小时（1..1440）。缺省 0 = 全量累计。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	hours := statsDefaultHours
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > statsMaxHours {
		hours = statsMaxHours
	}
	// 记录器未装配（裸 handler / 测试）时直接调 nil 接收者：Stats() 内部判 nil，
	// 返回 enabled=false 的完整结构（含非 nil 的 models 数组），仍 200——消费方
	// 不必为「未启用」单独写解析分支，也不会把 404 误读成「旧版网关不支持该端点」。
	snap := h.cfg.Usage.Stats(hours)
	snap.Now = time.Now()
	writeJSON(w, http.StatusOK, snap)
}
