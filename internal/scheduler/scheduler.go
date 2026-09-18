// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额；余额恢复的**硬冷却**（余额耗尽类）账号自动解冻（issue #199
// 收窄后软冷却/模型级冷却不被余额恢复解冻，按上游重置墙钟或有界退避自行到期）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此优先消耗，见
	// entry.creditsExpiring）。<=0 时禁用分桶（全部归长期，行为与引入前一致）。
	// 默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}
	// rearmAuthWatch 同上，供 auths 目录热加载循环（authwatch.go）消费：间隔/开关热改
	// 时立刻唤醒重算。三条循环各用独立 channel，同 channel 被多个 select 消费会丢信号。
	rearmAuthWatch chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// authWatchInterval auths 目录扫描间隔（纳秒，0=暂停）。形态同 balanceInterval：
	// 循环每轮读当前值，SetAuthWatchInterval 热改（面板保存配置）。
	authWatchInterval atomic.Int64

	// expiringSoonNanos 快过期积分窗口（纳秒，0=禁用分桶）。与 balanceInterval 同一
	// 形态：cfg.ExpiringSoonWindow 只作启动初值，运行期一律经 expiringSoonWindow() /
	// SetExpiringSoonWindow() 读写。用 atomic 而非 schedMu 的原因：读取点在
	// runCheckin / RunBalanceRefreshNow 的并发分支里（不持 schedMu），热改窗口不应
	// 与排程参数的锁耦合。
	expiringSoonNanos atomic.Int64

	// runningMu 巡检重入锁：五类任务的公开入口（定时批量派发与面板手动触发）互斥，
	// TryLock 失败即放弃本次触发并记一行日志——同一时刻两趟全量巡检并发会对上游
	// 重复轰炸（hub 版 wb_scheduler 的 _run_lock 同语义：「已有巡检正在执行」）。
	// 定时批量（runBatch）在整批期间持锁，批内各任务走不带锁的 run* 内部函数，
	// 故同槽多类任务并行派发不会自锁（否则后派发者必被自己跳过）。
	runningMu sync.Mutex

	// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限（<=0 回落 wakeupGraceDefault）。
	// 实例字段而非包级 var：读取点在 Run goroutine（awaitWakeupGrace），包级 var 被
	// 测试直接改写即与 Run 的读构成数据竞争（fix/200e-race）。生产零配置（恒缺省），
	// 仅测试构造后直接改本字段；Run 启动后不再写，无同步负担。
	wakeupGraceDelay time.Duration
}

// beginRun 尝试占用巡检重入锁；返回 false 表示已有巡检在执行（调用方直接返回）。
// name 仅用于日志定位触发来源（scheduler/checkin/travel/activity/keepalive/blackcat）。
func (s *Scheduler) beginRun(name string) bool {
	if !s.runningMu.TryLock() {
		log.Printf("%s: 已有巡检在执行，跳过本次触发", name)
		return false
	}
	return true
}

// endRun 释放巡检重入锁（与 beginRun 成对，defer 调用）。
func (s *Scheduler) endRun() { s.runningMu.Unlock() }

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	s := &Scheduler{
		cfg:            cfg,
		adoptTried:     make(map[string]string),
		rearmSchedule:  make(chan struct{}, 1),
		rearmBalance:   make(chan struct{}, 1),
		rearmAuthWatch: make(chan struct{}, 1),
	}
	// 启动初值写入原子字段；此后 cfg.ExpiringSoonWindow 不再被读取（见字段注释）。
	s.expiringSoonNanos.Store(int64(cfg.ExpiringSoonWindow))
	return s
}

// expiringSoonWindow 返回当前生效的快过期窗口（原子读；<=0 = 禁用分桶）。
func (s *Scheduler) expiringSoonWindow() time.Duration {
	return time.Duration(s.expiringSoonNanos.Load())
}

// SetExpiringSoonWindow 热更新快过期积分窗口（面板保存 pool.expiring_soon 后调用）。
// 下一轮签到/余额刷新即按新窗口分桶（pool 的 creditsExpiring 随之更新），无需重启。
// 窗口 <=0 = 禁用分桶（全部计入总量，行为与引入前一致）。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	s.expiringSoonNanos.Store(int64(d))
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDefault 迟到唤醒补跑的派发前网络宽限缺省值：Windows Modern Standby
// exit 后网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置（分析报告裁定全套配置不成比例）。
const wakeupGraceDefault = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// wakeupGrace 返回当前生效的迟到唤醒宽限：实例字段 <=0（未注入）回落缺省
// wakeupGraceDefault。读取点在 Run goroutine（awaitWakeupGrace），故宽限只能是
// Scheduler 的实例字段（构造期注入、Run 启动后只读）；曾经的包级 var 被测试
// 直接改写，与 Run goroutine 的读构成真实数据竞争（fix/200e-race）。
func (s *Scheduler) wakeupGrace() time.Duration {
	if s.wakeupGraceDelay <= 0 {
		return wakeupGraceDefault
	}
	return s.wakeupGraceDelay
}

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGrace 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限睡满，
// 本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func (s *Scheduler) awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	grace := s.wakeupGrace()
	log.Printf("wakeup grace %s: late catch-up for slot %s", grace, planned.Format("15:04"))
	return sleepCtx(ctx, grace)
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.rearmSchedule:
			timer.Stop() // 排程已变：重算下一次唤醒
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !s.awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
			// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
			// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
// 整批期间持巡检重入锁（beginRun）：批内各任务走不带锁的 run* 内部函数，
// 同槽多类任务并行不会自锁；批外的手动触发（面板/测试）撞上即跳过。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	if !s.beginRun("scheduler") {
		return
	}
	defer s.endRun()
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数。脚本类（school/cat）失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给带账号间限速的遍历（取消时立即放弃剩余账号），纯脚本类任务不感知。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	switch k {
	case taskCheckin:
		s.runCheckin(ctx)
	case taskTravel:
		s.runTravel(ctx)
	case taskActivity:
		s.runActivity(ctx)
	case taskKeepalive:
		s.runKeepalive(ctx)
	case taskBlackcat:
		s.runBlackcat(ctx)
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻（无 ctx 的外部入口：
// 面板手动触发、测试）。内部走 runCheckin，取背景 ctx。
// 重入保护：已有巡检在执行时直接跳过（返回前不阻塞调用方）。
func (s *Scheduler) RunCheckinNow() {
	if !s.beginRun("checkin") {
		return
	}
	defer s.endRun()
	s.runCheckin(context.Background())
}

// runCheckin 签到遍历 + 连登管家 + 开学季，随 ctx 取消立即退出。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) runCheckin(ctx context.Context) {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
			if upstream.IsAlreadyCheckin(err) {
				log.Printf("checkin %s: 今天已签到（幂等）", st.UID)
			} else {
				log.Printf("checkin %s: %v", st.UID, err)
			}
			// 其余业务错误也继续走余额查询
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		remain, total, expiring, err := s.cfg.Upstream.UserResourceDetailed(a, s.expiringSoonWindow())
		if err != nil {
			log.Printf("user-resource %s: %v", st.UID, err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		if expiring > 0 {
			s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring)
		}
	}
	s.RunStreakBonusNow()
	s.runSchool(ctx) // 开学季活动（活动期 9/13-9/24，结束自动跳过）
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报（无 ctx 的外部入口：
// 面板手动触发、测试）。内部走 runActivity，取背景 ctx（不可取消，语义与引入前
// time.Sleep 版一致）。重入保护：已有巡检在执行时直接跳过。
func (s *Scheduler) RunActivityNow() {
	if !s.beginRun("activity") {
		return
	}
	defer s.endRun()
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无任务中心/活跃体系，不发起任何上游调用
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", a.UID, err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", a.UID, err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", a.UID)
		return true
	}
	log.Printf("activity %s: streak days=%d", a.UID, days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用（无 ctx 的
// 外部入口：面板手动触发、测试）。内部走 runKeepalive，取背景 ctx。
// 重入保护：已有巡检在执行时直接跳过。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	if !s.beginRun("keepalive") {
		return
	}
	defer s.endRun()
	s.runKeepalive(context.Background())
}

// runKeepalive token 保活遍历。账号间无 sleep（不参与 sleepCtx 可取消化），
// ctx 取消时在账号边界收尾：剩余账号下轮再刷，避免停机时继续打上游。
func (s *Scheduler) runKeepalive(ctx context.Context) {
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", st.UID, pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义**收窄**（issue #199）：仅余额耗尽的硬冷却（CoolHard）账号余额恢复即解冻；
// 软冷却（CoolSoft 429 / 6004 模型级）**不被余额刷新解冻**——余额恢复不证明限流已
// 解除，旧实现无条件解冻会让软冷却账号每轮刷新（后台默认 5 分钟一次）被解冻 →
// 选号重新选中 → 又撞 429。软冷却按上游重置墙钟/有界退避自行到期，或人工「解冻」。
// 不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			remain, total, expiring, err := s.cfg.Upstream.UserResourceDetailed(a, s.expiringSoonWindow())
			if err != nil {
				log.Printf("balance %s: %v", uid, err)
				return
			}
			// 与 RunCheckinNow 同一形态（两分支语义统一）：解冻判定一律走
			// ReenableIfCredits（内部按收窄规则判定：仅有效硬冷却 CoolHard 解冻，
			// 软冷却/模型级冷却只更新 credits），带分桶时再叠加 SetCreditsDetailed
			// 记录快过架子集。旧实现把两件事塞进 if/else 二选一，导致「有快过期积分
			// 的硬冷却账号」永不解冻（SetCreditsDetailed 不含解冻语义）——分支语义
			// 分裂，同一账号是否解冻取决于上游是否返回 expiring 分桶。
			s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			if expiring > 0 {
				s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring)
			}
		}(a, st.UID)
	}
	wg.Wait()
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}
