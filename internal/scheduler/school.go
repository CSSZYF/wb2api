// school.go 开学季管家：签到排程末尾自动「分享上报 → 领奖 → 抽奖」。
//
// 活动期 2026-09-13 ~ 09-24（每日刷新）：share_invite 判据为纯前端上报
// （POST /tasks/share-complete，实测三账号即点亮），+100c + 1 次抽奖/天/号。
// chat_3_times / expert_use 判据绑定小程序原生沙箱会话，纯 API 不做（需人工）。
// 活动结束后 in_period=false 自动跳过，无需下线代码。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// schoolPollLoops/LGap share-complete 后的异步计分轮询（实测 2.5s 内点亮）。
const (
	schoolPollLoops = 3
	schoolPollGap   = 2500 * time.Millisecond
)

// RunSchoolNow 对所有可用账号执行开学季活动闭环（幂等：不在期/已领静默跳过）。
// 由签到排程（runCheckin）末尾调用；也可面板手动触发。无 ctx 的外部入口，
// 内部走 runSchool 取背景 ctx（语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunSchoolNow() {
	s.runSchool(context.Background())
}

// runSchool 开学季遍历，随 ctx 取消立即退出（账号间限速与轮询等待改用 sleepCtx）。
func (s *Scheduler) runSchool(ctx context.Context) {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		s.schoolAccount(ctx, a)
		if !sleepCtx(ctx, activityAccountDelay) {
			return // 优雅停机：不等限速睡满，剩余账号下轮再跑
		}
	}
}

// RunSchoolAccountNow 单账号开学季闭环（面板任务中心逐账号执行用）：
// 四任务独立处理 + 抽完抽奖次数，与每日排程同语义。
func (s *Scheduler) RunSchoolAccountNow(a *auth.Auth) {
	s.schoolAccount(context.Background(), a)
}

// schoolAccount 单账号闭环：四个任务独立处理（已领/不在期静默跳过），最后抽完次数。
// 判据（三账号实测 2026-09-13/14，protocol.md §7.11/§8）：
//   - share_invite（每日 +100c+1抽）：POST share-complete 即点亮。
//   - desktop_chat_1_time（单次 +100c+1抽）：viewed + 真实 chat + 桌面六事件链。
//   - chat_3_times（每日 +50c+1抽）：viewed + 3 条 chat_request_send 埋点
//     （conversationId 任意，无需真实会话）。
//   - expert_use（每日 +50c+1抽）：viewed + mp 事件链（专家召唤 ×3 + 对话）。
func (s *Scheduler) schoolAccount(ctx context.Context, a *auth.Auth) {
	tasks, inPeriod, err := s.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		log.Printf("school %s: tasks: %v", a.UID, err)
		return
	}
	if !inPeriod {
		return // 活动已结束，静默
	}
	_ = tasks
	s.schoolShareTask(ctx, a)
	s.schoolDesktopTask(ctx, a)
	s.schoolChatTimesTask(ctx, a)
	s.schoolExpertTask(ctx, a)
	// 抽奖：把余额全抽完（含本次活动新领的次数）。
	chances, err := s.cfg.Upstream.SchoolChances(a)
	if err != nil {
		return
	}
	for i := 0; i < chances; i++ {
		prize, err := s.cfg.Upstream.SchoolDraw(a)
		if err != nil {
			log.Printf("school %s: draw: %v", a.UID, err)
			return
		}
		log.Printf("school %s: 🎲 %s", a.UID, prize)
		if !sleepCtx(ctx, 2*time.Second) {
			return // 取消即放弃本号剩余抽奖
		}
	}
}

// schoolShareTask 完成 share_invite：share-complete 上报 → 轮询 → 领奖。
func (s *Scheduler) schoolShareTask(ctx context.Context, a *auth.Auth) {
	tasks, _, err := s.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return
	}
	share := findSchoolTask(tasks, "share_invite")
	if share == nil || share.Status == "claimed" {
		return
	}
	if err := s.cfg.Upstream.SchoolShareComplete(a); err != nil {
		log.Printf("school %s: share-complete: %v", a.UID, err)
		return
	}
	if !s.schoolPollDone(ctx, a, "share_invite") {
		log.Printf("school %s: share-complete 上报后未点亮（明日重试）", a.UID)
		return
	}
	granted, err := s.cfg.Upstream.SchoolClaimTask(a, "share_invite")
	if err != nil {
		log.Printf("school %s: share claim: %v", a.UID, err)
		return
	}
	log.Printf("school %s: ★ 分享任务完成，+100c +%d 抽奖次数", a.UID, granted)
}

// schoolPollDone 轮询任务是否达标（异步计分，最多 schoolPollLoops 次）。
// ctx 取消立即返回 false（放弃本项轮询，下轮再试）。
func (s *Scheduler) schoolPollDone(ctx context.Context, a *auth.Auth, code string) bool {
	for i := 0; i < schoolPollLoops; i++ {
		if !sleepCtx(ctx, schoolPollGap) {
			return false
		}
		tasks2, _, err := s.cfg.Upstream.SchoolTasks(a)
		if err != nil {
			continue
		}
		if t := findSchoolTask(tasks2, code); t != nil && t.TargetCount > 0 && t.Progress >= t.TargetCount {
			return true
		}
	}
	return false
}

// schoolChatTimesTask 完成 chat_3_times：viewed → 3 条埋点 → 轮询 → 领奖。
func (s *Scheduler) schoolChatTimesTask(ctx context.Context, a *auth.Auth) {
	tasks, _, err := s.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return
	}
	t := findSchoolTask(tasks, "chat_3_times")
	if t == nil || t.Status == "claimed" || (t.TargetCount > 0 && t.Progress >= t.TargetCount && t.Status == "completed") {
		return
	}
	if t.Status == "pending" {
		if err := s.cfg.Upstream.SchoolTaskViewed(a, "chat_3_times"); err != nil {
			log.Printf("school %s: chat viewed: %v", a.UID, err)
			return
		}
	}
	for i := 0; i < t.TargetCount && i < 5; i++ {
		if err := s.cfg.Upstream.ReportMPEvent(a, upstream.SchoolChatTimesEvents(fmt.Sprintf("wb2api-chat-%d-%d", time.Now().Unix(), i))); err != nil {
			log.Printf("school %s: chat events: %v", a.UID, err)
			return
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return // 取消即放弃本项剩余埋点
		}
	}
	if !s.schoolPollDone(ctx, a, "chat_3_times") {
		log.Printf("school %s: chat_3_times 未点亮（明日重试）", a.UID)
		return
	}
	granted, err := s.cfg.Upstream.SchoolClaimTask(a, "chat_3_times")
	if err != nil {
		log.Printf("school %s: chat claim: %v", a.UID, err)
		return
	}
	log.Printf("school %s: ★ 对话任务完成，+50c +%d 抽奖次数", a.UID, granted)
}

// schoolExpertTask 完成 expert_use：viewed → 专家事件链 → 轮询 → 领奖。
// 开学季专家（16-BackToSchool 分类）：论文写作导师。
func (s *Scheduler) schoolExpertTask(ctx context.Context, a *auth.Auth) {
	tasks, _, err := s.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return
	}
	t := findSchoolTask(tasks, "expert_use")
	if t == nil || t.Status == "claimed" || (t.TargetCount > 0 && t.Progress >= t.TargetCount) {
		return
	}
	if t.Status == "pending" {
		if err := s.cfg.Upstream.SchoolTaskViewed(a, "expert_use"); err != nil {
			log.Printf("school %s: expert viewed: %v", a.UID, err)
			return
		}
	}
	events := upstream.SchoolExpertUseEvents("ex_jB0dyFIQJEWa", "论文写作导师",
		fmt.Sprintf("wb2api-exp-%d", time.Now().Unix()))
	if err := s.cfg.Upstream.ReportMPEvent(a, events...); err != nil {
		log.Printf("school %s: expert events: %v", a.UID, err)
		return
	}
	if !s.schoolPollDone(ctx, a, "expert_use") {
		log.Printf("school %s: expert_use 未点亮（明日重试）", a.UID)
		return
	}
	granted, err := s.cfg.Upstream.SchoolClaimTask(a, "expert_use")
	if err != nil {
		log.Printf("school %s: expert claim: %v", a.UID, err)
		return
	}
	log.Printf("school %s: ★ 专家任务完成，+50c +%d 抽奖次数", a.UID, granted)
}

// schoolDesktopTask 完成 desktop_chat_1_time：viewed 激活 → 真实 chat → 六事件链。
func (s *Scheduler) schoolDesktopTask(ctx context.Context, a *auth.Auth) {
	tasks, _, err := s.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return
	}
	t := findSchoolTask(tasks, "desktop_chat_1_time")
	if t == nil || t.Status == "claimed" || t.Progress >= t.TargetCount {
		return
	}
	if t.Status == "pending" {
		if err := s.cfg.Upstream.SchoolTaskViewed(a, "desktop_chat_1_time"); err != nil {
			log.Printf("school %s: desktop viewed: %v", a.UID, err)
			return
		}
	}
	conv, req, err := s.cfg.Upstream.DesktopChatWithExpert(a, "")
	if err != nil {
		log.Printf("school %s: desktop chat: %v", a.UID, err)
		return
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	if err := s.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		log.Printf("school %s: desktop events: %v", a.UID, err)
		return
	}
	// 异步计分轮询后领奖（失败不阻塞 share 主流程）。
	for i := 0; i < schoolPollLoops; i++ {
		if !sleepCtx(ctx, schoolPollGap) {
			return // 取消即放弃本项轮询
		}
		tasks2, _, err := s.cfg.Upstream.SchoolTasks(a)
		if err != nil {
			continue
		}
		if t2 := findSchoolTask(tasks2, "desktop_chat_1_time"); t2 != nil && t2.Progress >= t2.TargetCount {
			if granted, err := s.cfg.Upstream.SchoolClaimTask(a, "desktop_chat_1_time"); err == nil {
				log.Printf("school %s: ★ 桌面端体验任务完成 +100c +%d 抽奖", a.UID, granted)
			}
			return
		}
	}
	log.Printf("school %s: desktop_chat_1_time 未点亮（明日重试）", a.UID)
}

// findSchoolTask 按任务码查条目。
func findSchoolTask(tasks []upstream.SchoolTask, code string) *upstream.SchoolTask {
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i]
		}
	}
	return nil
}
