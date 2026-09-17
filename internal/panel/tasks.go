// tasks.go 面板「积分任务」接口：查询任务进度、接受任务、领取奖励。
//
// 上游能力（internal/upstream/tasks.go）的三层薄封装；前端表格展示
// current/target 进度与可领取状态，运维点按钮即可，无需外部 Python 脚本。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// acceptBatchGap 批量接受的批间节流（对齐脚本 1.05s 口径，避免上游风控）。
var acceptBatchGap = 1050 * time.Millisecond

// accountByUID 取账号凭证；不存在时写 404 并返回 nil。
func (p *Panel) accountByUID(w http.ResponseWriter, uid string) *auth.Auth {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return nil
	}
	return a
}

// acceptVerified 接受一批任务码并**验证登记生效**，返回确认登记（accepted）与
// 重试后仍未登记（failed）两组码。
//
// 为什么不能只看 HTTP 200：实测上游存在 200 + msg=OK 但 results[].status 非
// accepted、服务端未落账的形态（对照上游 scripts/task_runner.py 的 accept 登记
// 验证）。此时后续行为事件全部不归账——这是"上报成功却不点亮"的根因，旧日志
// 只打 msg 把失败掩盖了。
//
// 判定顺序：响应 results[].status 初筛（明确非 accepted 的直接判待重试）→
// 回读 ListTasks 的 accept_status（**以回读为准**，非 not_accepted 才算登记）。
// 未通过的码重试一次（批间节流同款 gap），仍未通过即进 failed 由调用方下次重试
// ——回读本身失败同样按"未确认"处理（与上游 Python 侧一致：宁可下轮重试，
// 不谎报登记成功）。
func (p *Panel) acceptVerified(a *auth.Auth, codes []string) (accepted, failed []string) {
	pending := append([]string(nil), codes...)
	for attempt := 1; attempt <= 2 && len(pending) > 0; attempt++ {
		results, err := p.cfg.Upstream.AcceptTasks(a, pending)
		if err != nil {
			log.Printf("panel: accept uid=%s codes=%v 第 %d 次请求失败: %v", a.UID, pending, attempt, err)
			if attempt < 2 {
				time.Sleep(acceptBatchGap)
			}
			continue
		}
		// 初筛：逐码默认通过，响应里明确给了非 accepted 状态的码改判待重试。
		respOK := make(map[string]bool, len(pending))
		for _, code := range pending {
			respOK[code] = true
		}
		for _, r := range results {
			if r.Status != "" && r.Status != "accepted" {
				respOK[r.TaskCode] = false
			}
		}
		// 回读确认（响应 200 不等于服务端落账，以 accept_status 为准）。
		// 单码走 upstream.VerifyAccepted（与外部脚本同源的判定原语）；
		// 批量一次 ListTasks 后本地扫描，避免每码一次回读。
		registered := make(map[string]bool, len(pending))
		if len(pending) == 1 {
			registered[pending[0]] = p.cfg.Upstream.VerifyAccepted(a, pending[0])
		} else {
			tasks, err := p.cfg.Upstream.ListTasks(a)
			if err != nil {
				log.Printf("panel: accept 回读失败 uid=%s: %v（按未确认处理，可重试）", a.UID, err)
			}
			for _, code := range pending {
				registered[code] = err == nil && taskRegistered(tasks, code)
			}
		}
		var retry []string
		for _, code := range pending {
			if respOK[code] && registered[code] {
				accepted = append(accepted, code)
			} else {
				retry = append(retry, code)
			}
		}
		pending = retry
		if len(pending) > 0 && attempt < 2 {
			time.Sleep(acceptBatchGap) // 重试前节流（对齐脚本 gap 口径）
		}
	}
	failed = append(failed, pending...)
	return accepted, failed
}

// taskRegistered 判定任务码是否已登记（accept_status 非空且非 not_accepted）。
// 列表里没有该码时无法确认登记，按未登记处理（上游 Python 侧同口径）。
func taskRegistered(tasks []upstream.Task, code string) bool {
	for _, t := range tasks {
		if t.TaskCode == code {
			return t.AcceptStatus != "" && t.AcceptStatus != "not_accepted"
		}
	}
	return false
}

// accountTasks 查询单账号全量任务（进度/状态/可领取）。
func (p *Panel) accountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks})
}

// accountTaskAccept 接受任务（报名；幂等）。
func (p *Panel) accountTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TaskCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes required")
		return
	}
	// 单码接受也走登记验证：上游存在 200+OK 但未落账的形态，未生效时重试一次。
	accepted, failed := p.acceptVerified(a, body.TaskCodes)
	if len(accepted) == 0 {
		writeErr(w, http.StatusBadGateway, "accept: 上游未登记（重试后仍未生效），可稍后再试")
		return
	}
	log.Printf("panel: 接受任务 uid=%s codes=%v 登记=%d 未登记=%v", uid, body.TaskCodes, len(accepted), failed)
	resp := map[string]any{"ok": true, "accepted": len(accepted)}
	if len(failed) > 0 {
		resp["failed"] = failed
		resp["message"] = "部分任务上游未登记（可重试）"
	}
	writeJSON(w, http.StatusOK, resp)
}

// taskAcceptAll 接受该账号全部尚未接受的任务（跳过已 accepted/claimed 的）。
//
// 为什么值得做：accept 不产生进度（进度靠行为事件点亮），但让状态机规范
// （not_accepted → accepted → completed），也便于后续筛选"我报过名的任务"。
// 上游 scripts 的注释同样建议"先 accept"。
// 批量提交会分片（上游对 task_codes 数组长度无公开上限，保守每批 20 个）。
func (p *Panel) taskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	var codes []string
	for _, t := range tasks {
		// 跳过已完成/已领取/已接受的；locked 的也不碰（上游未开放）。
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	if len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": "所有任务均已接受"})
		return
	}
	const batch = 20
	var accepted, failed []string
	for i := 0; i < len(codes); i += batch {
		end := i + batch
		if end > len(codes) {
			end = len(codes)
		}
		// 验证通过才计数：上游 200+OK 但未登记时改判 failed（旧口径只数请求成功）。
		ok, bad := p.acceptVerified(a, codes[i:end])
		accepted = append(accepted, ok...)
		failed = append(failed, bad...)
		time.Sleep(acceptBatchGap) // 批间节流（对齐脚本 1.05s 口径）
	}
	log.Printf("panel: 全部接受 uid=%s 登记=%d 未登记=%d", uid, len(accepted), len(failed))
	resp := map[string]any{"ok": true, "accepted": len(accepted), "failed": failed}
	if len(failed) > 0 {
		resp["message"] = "部分任务上游未登记（200 但未落账），可重试"
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountTaskClaim 领取任务奖励（未达标时上游返回业务错误，原样透出给前端提示）。
func (p *Panel) accountTaskClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	credit, energy, err := p.cfg.Upstream.ClaimReward(a, body.TaskCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claim: "+err.Error())
		return
	}
	if credit == 0 && energy == 0 {
		log.Printf("panel: 领取任务奖励 uid=%s code=%s（已领取过，无新增）", uid, body.TaskCode)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "该奖励此前已领取"})
		return
	}
	log.Printf("panel: 领取任务奖励 uid=%s code=%s +%d分 +%d能", uid, body.TaskCode, credit, energy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}
