// tasks.go growth 域「任务」接口：列表查询 / 接受 / 领取奖励。
//
// 来源：上游 scripts/task_common.py 实测口径（list_tasks/accept_tasks/claim_reward），
// 集成进主程序后不再需要外部脚本。
//
// 端点（chatBase，BillingHeaders）：
//   - GET  /v2/activity/growth/tasks                全量任务列表（含 progress/accept_status）
//   - POST /v2/activity/growth/tasks/accept         {"task_codes":[...]} not_accepted → accepted
//   - POST /v2/activity/growth/tasks/<code>/claim   完成态领奖（任务码在**路径**里，无 body）
//
// 另有**小程序口径**变体（ListTasksMP / AcceptTasksMP / VerifyAcceptedMP /
// ClaimRewardMP）：多带 X-Client-Platform: miniprogram，供 mp 限定任务
// （school_season「校园日」）使用——该口径下才会下发这些 code，accept 缺头返回
// task not found。默认口径调用逐字不变（零回归）。
//
// 语义要点：
//   - accept 是"报名"，不产生进度；进度由服务端行为事件点亮（如 chat_request_send 上报、
//     真实对话），故 accept 后可幂等重放。
//   - claim 仅在 progress 达标后可领；重复领返回业务错误（安全，不需要前置状态判断）。
package upstream

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growth 域任务路径（与 scripts/task_common.py 对齐）。
const (
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
)

// Task 单个任务的对外视图（字段名与上游 JSON 对齐，多余字段不透出）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // 操作指引（含跳转说明）
	TaskDesc     string `json:"task_desc,omitempty"`   // 达成条件简述
	Credit       int64  `json:"credit,omitempty"`      // 奖励积分（上游 reward_credit）
	Energy       int64  `json:"energy,omitempty"`      // 奖励能量（上游 reward_energy）
	HasReward    bool   `json:"has_reward,omitempty"`  // 是否带奖励
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"` // single（一次性）/ 累计型
	Tag          string `json:"tag,omitempty"`       // 端标记（PC 等）
	JumpURL      string `json:"jump_url,omitempty"`  // 客户端跳转协议（workbuddy://...）
	Locked       bool   `json:"locked,omitempty"`    // 上游标记未解锁
	Target       int64  `json:"target"`              // 目标次数（恒输出：0 是有效进度值）
	Current      int64  `json:"current"`             // 当前进度（恒输出：0 是有效进度值）
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`    // 上游任务状态（complete 等）
	Claimable    bool   `json:"claimable,omitempty"` // 进度达标且未领取（本地推算）
	Claimed      bool   `json:"claimed,omitempty"`   // 已领取（accept_status == claimed）
}

// ListTasks 拉取全量任务列表。
// 响应形如 data.tasks[]，元素字段随任务类型变化（progress 可能是 {current,target} 或平铺），
// 这里做宽松解析：两种形状都尝试。
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	return c.listTasksMP(a, false)
}

// ListTasksMP 以**小程序口径**拉取任务列表（X-Client-Platform: miniprogram）。
//
// 为什么需要单独入口：小程序限定任务（school_season「校园日」/ Sequential_Tasks_1）
// 只在带该头的请求里下发——默认口径列表里根本没有这些 code，于是"扫不到待办"。
// 同时该头也是 accept/claim 的前置（缺头 accept 返回 task not found，实测）。
func (c *Client) ListTasksMP(a *auth.Auth) ([]Task, error) {
	return c.listTasksMP(a, true)
}

// listTasksMP ListTasks 的公共实现；mp 决定是否带小程序口径头。
func (c *Client) listTasksMP(a *auth.Auth, mp bool) ([]Task, error) {
	data, err := c.growthJSONMP(a, http.MethodGet, tasksListPath, nil, mp)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"` // 上游实际字段名（reward_ 前缀）
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		// progress 可能是 {current,target} 对象（实测口径），覆盖平铺字段。
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, Task{
			TaskCode:     t.TaskCode,
			Title:        t.Title,
			Description:  t.Description,
			TaskDesc:     t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			HasReward:    t.HasReward,
			RewardBuddy:  t.RewardBuddy,
			TaskType:     t.TaskType,
			Tag:          t.Tag,
			JumpURL:      t.JumpURL,
			Locked:       t.Locked,
			Target:       tgt,
			Current:      cur,
			AcceptStatus: t.AcceptStatus,
			Status:       t.Status,
			Claimable:    !claimed && tgt > 0 && cur >= tgt,
			Claimed:      claimed,
		})
	}
	return out, nil
}

// AcceptResult 单个任务码的 accept 结果（上游 data.results[] 元素）。
type AcceptResult struct {
	TaskCode string `json:"task_code"`
	Status   string `json:"status"` // accepted / not_accepted / 其他上游口径
}

// AcceptTasks 接受任务（幂等：已 accepted 时上游返回成功或业务提示，均不视为致命错误）。
//
// 返回值语义（对照上游 scripts/task_runner.py 的 accept 登记验证）：
//   - err != nil：请求失败（HTTP/信封错误）；
//   - err == nil 但 results 为空：上游未给逐条结果（老口径），调用方按"接受成功"处理；
//   - err == nil 且 results 非空：逐条 status 由调用方判定，**不能只看 HTTP 200**——
//     实测上游存在 200 + msg=OK 但 results[].status != accepted、服务端未落账的形态，
//     此时后续行为事件全部不归账（任务永远点不亮）。
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) ([]AcceptResult, error) {
	return c.acceptTasksMP(a, taskCodes, false)
}

// AcceptTasksMP 以小程序口径接受任务（X-Client-Platform: miniprogram）。
// 缺该头时上游对 mp 限定任务返回 task not found（实测），故 mp 任务的 accept
// 必须走本入口；返回语义与 AcceptTasks 逐字一致。
func (c *Client) AcceptTasksMP(a *auth.Auth, taskCodes []string) ([]AcceptResult, error) {
	return c.acceptTasksMP(a, taskCodes, true)
}

// acceptTasksMP AcceptTasks 的公共实现；mp 决定是否带小程序口径头。
func (c *Client) acceptTasksMP(a *auth.Auth, taskCodes []string, mp bool) ([]AcceptResult, error) {
	data, err := c.growthJSONMP(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes}, mp)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []AcceptResult `json:"results"`
	}
	// data 为空（老响应/无 body）不算错：返回空结果，调用方走宽松分支。
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, nil // 形状不符时同样走宽松分支，不因解析失败阻断 accept 语义
	}
	return resp.Results, nil
}

// VerifyAccepted 回读任务列表确认 accept 已登记生效。
//
// 为什么不能只看 accept 响应：上游可能回 200 + msg=OK 但服务端未落账
// （accept_status 仍为 not_accepted），此时上报的行为事件全部不归账——
// 这是"上报成功却不点亮"的根因。判定以回读为准（响应 status 只是初筛）。
// 查询失败（网络/上游错误）返回 false，由调用方决定是否重试。
func (c *Client) VerifyAccepted(a *auth.Auth, code string) bool {
	return c.verifyAcceptedMP(a, code, false)
}

// VerifyAcceptedMP 同 VerifyAccepted，但回读走**小程序口径**列表。
// mp 限定任务（school_season）在默认口径列表里不可见，用默认口径回读会恒判
// "未登记"——面板据此会误判 accept 失败并反复重试。
func (c *Client) VerifyAcceptedMP(a *auth.Auth, code string) bool {
	return c.verifyAcceptedMP(a, code, true)
}

// verifyAcceptedMP VerifyAccepted 的公共实现；mp 决定回读口径。
func (c *Client) verifyAcceptedMP(a *auth.Auth, code string, mp bool) bool {
	tasks, err := c.listTasksMP(a, mp)
	if err != nil {
		return false
	}
	for _, t := range tasks {
		if t.TaskCode != code {
			continue
		}
		// accepted/claimed/completed 等任意非 not_accepted 态都视为登记生效。
		return t.AcceptStatus != "" && t.AcceptStatus != "not_accepted"
	}
	return false // 列表里没有该任务码 → 无法确认登记
}

// ClaimReward 领取单个任务奖励。
//
// 端点来源（实测）：Web 成长中心的领奖请求 ——
//
//	POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim
//	（任务码在**路径**里，无 body；带 x-client-platform: web 头，Bearer 鉴权）
//
// 关键区别：此前误用 CLI 域 copilot.tencent.com 的
// /v2/activity/growth/tasks/reward/claim（task_code 放 body），该路径**不存在**，
// 一直返回 400 "task not completed"，是此前领奖失败的真实原因。
// 本实现返回 (credit, energy, err)：credit/energy 为本次到账奖励（已领取过时为 0）。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	return c.claimViaWeb(a, taskCode)
}

// ClaimRewardMP 以小程序口径领取任务奖励（mp 限定任务专用，如 school_season）。
//
// 链路对照上游 e45f39f 的 claim_one(mp=True) 实测（账号 f8657995 e2e
// accept→report→回读→claim 全链 200）：
//  1. **先走 chat 域**（chatBase）：POST /activity/growth/tasks/{code}/claim
//     带 X-Client-Platform: miniprogram（上游 MP_PLATFORM_HEADER）；
//  2. 该请求 HTTP 400 时降级 web 域（ClaimReward 的既有路径，
//     x-client-platform: web——与上游 _claim_via_web 逐字同头族）。
//
// 为什么不直接用「web 域 + mp 头」：上游两个域的 claim 头族是**二选一**的实测
// 原值（chat 域带 mp 头 / web 域带 web 头），混搭组合未经验证，不引入第三种形态。
// 返回 (credit, energy, err) 语义与 ClaimReward 一致（already_claimed → 0,0,nil）。
func (c *Client) ClaimRewardMP(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	credit, energy, err = c.claimViaChatMP(a, taskCode, true)
	if err == nil {
		return credit, energy, nil
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return 0, 0, err // 非 400：原样透出（不盲目降级，避免掩盖鉴权/风控错误）
	}
	log.Printf("upstream: claim %s chat 域 400，降级 web 域（mp 任务）", taskCode)
	return c.ClaimReward(a, taskCode)
}

// claimViaChatMP chat 域领奖（上游 claim_one 的首选路径）：POST
// {chatBase}/activity/growth/tasks/{code}/claim，mp=true 时带小程序口径头。
// body 为空（任务码在路径里，与上游 do_post(..., None, ...) 一致）。
func (c *Client) claimViaChatMP(a *auth.Auth, taskCode string, mp bool) (credit, energy int64, err error) {
	data, err := c.growthJSONMP(a, http.MethodPost,
		"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil, mp)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // 幂等：重复领取不算错误，但无新增奖励
	}
	return resp.Credit, resp.Energy, nil
}

// claimViaWeb web 域领奖实现（webBase + web 头族）：ClaimReward 的唯一路径，
// 也是 ClaimRewardMP 在 chat 域 400 时的降级目标。
func (c *Client) claimViaWeb(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.webBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	// Web 端请求头形状（对照浏览器实际请求）：Origin/Referer 指向 workbuddy.cn 成长中心，
	// 带 x-client-platform: web 标记来源端。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}

	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	// 响应 data：{"already_claimed":bool,"credit":100,"energy":5,...}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // 幂等：重复领取不算错误，但无新增奖励
	}
	return resp.Credit, resp.Energy, nil
}
