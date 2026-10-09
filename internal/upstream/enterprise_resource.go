package upstream

import (
	"encoding/json"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"net/http"
	"time"
)

// enterpriseUnlimitedRemain / enterpriseUnlimitedTotal 企业版「不限量」在池内的等价表示
// （上游 limitNum == -1；桌面端同判为 unlimited）。
//
// 池内没有"不限"状态位，而 credits 同时参与两处判定——路由权重（credits/maxCredits）与
// credit_floor（余额低于阈值则不接收费模型）——不限量必须两处都"不构成限制"，故剩余量
// 取一个足够大的值；total 记 -1 供面板显示「不限」。total 仅用于展示与快照，
// 不参与任何判定（已核对全仓库引用面）。
const (
	enterpriseUnlimitedRemain int64 = 1 << 40
	enterpriseUnlimitedTotal  int64 = -1
)

// enterpriseResource 查询**企业版账号**的分配额度，并归一到个人口径的
// (remain, total, expiring, earliestAt, earliestRemaining)。
//
// 为什么必须分流：企业额度不在「个人资源包」体系内——/billing/meter/get-user-resource*
// 对 enterpriseId 非空的账号恒返回空 Accounts（实测 code 0 且 Accounts null），
// 面板因此长期显示 0/未知（额度其实是有的）。
//
// 上游口径（实测 2026-10-07，企业号 a6239ec8）：
//
//	POST /v2/billing/meter/get-enterprise-user-usage   （X-Enterprise-Id 由 BillingHeaders 注入）
//	{"credit":812.45,"limitNum":2000,
//	 "cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"2026-10-26 23:59:59",
//	 "cycleResetTime":"2026-10-27 00:00:00"}
//
// credit = **本周期已用**（与个人口径 CapacityRemain「剩余」语义相反），
// limitNum = **分配给本账号的额度**。故 remain = limitNum - credit、total = limitNum。
// 注意：企业**池**总额度是另一回事，成员账号无权查询（实测所有池端点 403 not_authorized），
// 本函数只反映该账号被分配的额度。
//
// 分桶：企业配额按周期重置、未用完即作废，与个人「奖励积分到期作废」同性质，故把
// cycleEndTime 作为唯一到期批次——周期末企业号会被 prefer_expiring 优先选中（期望行为）。
// limitNum < 0（不限量）无作废语义，不参与分桶。
func (c *Client) enterpriseResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	// 与个人口径一致：余额是签到后的紧邻调用，偶发 500 值得有界重试。
	var data json.RawMessage
	err = c.retryBillingTransient(func() error {
		var e error
		data, e = c.billingJSON(a, http.MethodPost, "/v2/billing/meter/get-enterprise-user-usage", map[string]any{})
		return e
	})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	// 兼容上游两套字段命名（桌面端两条解析路径分别读 camelCase 与 snake_case）。
	var resp struct {
		Credit         float64 `json:"credit"`
		LimitNum       int64   `json:"limitNum"`
		LimitNumSnake  int64   `json:"limit_num"`
		UsedNum        float64 `json:"used_num"`
		CycleEndTime   string  `json:"cycleEndTime"`
		CycleResetTime string  `json:"cycleResetTime"`
	}
	if uerr := json.Unmarshal(data, &resp); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("enterprise resource parse: %w", uerr)
	}
	limit := resp.LimitNum
	if limit == 0 && resp.LimitNumSnake != 0 {
		limit = resp.LimitNumSnake
	}
	used := resp.Credit
	if used == 0 && resp.UsedNum != 0 {
		used = resp.UsedNum
	}
	// 不限量：池内无对应状态位，用大剩余量让路由权重与 credit_floor 都不构成限制。
	if limit < 0 {
		return enterpriseUnlimitedRemain, enterpriseUnlimitedTotal, 0, time.Time{}, 0, nil
	}
	// credit 是浮点（如 812.45），池内 credits 是整数：四舍五入到最近整数
	// （向下取整会低报剩余额度，与"额度还有多少"的展示意图相悖）。
	usedInt := int64(used)
	if used-float64(usedInt) >= 0.5 {
		usedInt++
	}
	remain = limit - usedInt
	if remain < 0 {
		remain = 0
	}
	total = limit
	// 周期到期批次：cycleEndTime 优先，缺失回落 cycleResetTime。
	now := time.Now()
	end, ok := parsePackageEndTime(resp.CycleEndTime)
	if !ok {
		end, ok = parsePackageEndTime(resp.CycleResetTime)
	}
	if ok && remain > 0 && end.After(now) {
		earliestAt, earliestRemaining = end, remain
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring = remain
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
