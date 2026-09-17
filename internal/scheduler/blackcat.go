// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
// 无 ctx 的外部入口，内部走 runBlackcat 取背景 ctx（语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunBlackcatNow() {
	if !s.beginRun("blackcat") {
		return
	}
	defer s.endRun()
	s.runBlackcat(context.Background())
}

// runBlackcat 夜猫子遍历，随 ctx 取消立即退出（账号间限速改用 sleepCtx）。
func (s *Scheduler) runBlackcat(ctx context.Context) {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
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
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", a.UID, err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", a.UID, ok, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", a.UID, ok)
		if !sleepCtx(ctx, activityAccountDelay) {
			return // 优雅停机：不等限速睡满，剩余账号下轮再补
		}
	}
}
