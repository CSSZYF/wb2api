// 持久化：本地 state.json 落盘/加载、Redis 快照镜像（StoreSnapshotter）、
// 后台 flusher、择新恢复（RestoreFromSnapshot）。
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool 账号池。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
//
// 本地**可用**时按新旧择一（快照不早于本地 → 采用快照，否则本地优先）；本地**不可用**
// （state.json 缺失或不可读，典型为首次在新卷/新节点启动）时**采用快照**——此时本地根本
// 没有可"优先"的状态，快照是本轮唯一的运行态来源，这正是快照作为「启动恢复备份」的核心
// 场景。分支情形：无快照 / 快照无 savedAt → 本地优先（无判据可比）。
//
// 每种情形都打一条对应的恢复来源日志，便于对账。必须在 SyncToDir 之前调用
// （SyncToDir 只增删不入值：值只能来自本地 load 或本函数采用快照）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr != nil {
		// 本地不可用 → 采用快照（本地没有可"优先"的状态）。
		//
		// 旧实现把该情形与「本地较新」合并成同一个 fall-through：既不改内存、不置 dirty
		// （有效快照被静默丢弃），又打出"本地 state.json（较新于 Redis 快照 …）"——一次
		// 从未发生过的比较，把排障引向根本不存在的本地文件；随后 SyncToDir 只增删不入值，
		// 全池运行态（credits/冷却/熔断计数/usedSeq/lastUsed）被清零。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)（本地 state.json 不可用: %v）",
			snap.SavedAt.Format(time.RFC3339), localErr)
		return
	}
	if !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	// 走到这里必然是「本地存在且严格新于快照」，日志结论属实。
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	// 在启动 goroutine 前同步写入（避免与测试对 flushInterval 的恢复写竞争）；
	// stopCh 同步登记，Close 才能可靠停止（New 与 startFlusher 之间无并发窗口）。
	p.stopCh = stopCh
	interval := flushInterval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				if p.dirty.Swap(false) {
					p.saveLocked()
				}
				p.mu.Unlock()
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	now := time.Now()
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		// 快过期积分钳到 [0, credits]：与 SetCreditsDetailed 同口径。state.json 是
		// 可被手工编辑/旧版本写坏的输入，越界值会经 weightOf 的占比项（×8）放大成
		// 选号偏置，恢复即钳死。
		creditsExpiring := s.CreditsExpiring
		if creditsExpiring < 0 {
			creditsExpiring = 0
		}
		if creditsExpiring > s.Credits {
			creditsExpiring = s.Credits
		}
		e := &entry{
			a:                &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:          s.Credits,
			creditsTotal:     s.CreditsTotal,
			creditsExpiring:  creditsExpiring,
			disabled:         s.Disabled,
			reason:           s.Reason,
			until:            s.Until,
			coolKind:         s.CoolKind,
			successCount:     s.SuccessCount,
			errTotal:         errTotal,
			lastErr:          s.LastErr,
			lastSuccess:      s.LastSuccess,
			tokenUsage:       s.TokenUsage,
			softStreak:       s.SoftStreak,
			sessionDeadFails: s.SessionDeadFails,
			consecutiveFails: s.ConsecutiveFails,
		}
		// 恢复熔断器：breakerUntil 在未来才恢复（惰性过滤过期/零值，与落盘同口径）。
		// retryCount 仅在 breakerUntil 未过期时恢复——已过期则归零（不保留无用退避指数）。
		if s.BreakerUntil != nil && !s.BreakerUntil.IsZero() && now.Before(*s.BreakerUntil) {
			e.breakerUntil = *s.BreakerUntil
			e.retryCount = s.RetryCount
		}
		// 恢复连败降权（issue #114）：degradeUntil 在未来才恢复（惰性过滤，与
		// breakerUntil 同口径）——降权期重启不失忆。consecutiveFails 恒恢复
		// （半开进度：重启归零会让「持续故障 + 频繁重启」的组合重新学满阈值）。
		if s.DegradeUntil != nil && !s.DegradeUntil.IsZero() && now.Before(*s.DegradeUntil) {
			e.degradeUntil = *s.DegradeUntil
		}
		// 恢复 modelCooldowns，惰性过滤已过期条目（Until 在未来才恢复）。
		// 防止重启后残留已过期的模型级冷却条目（与 pick 路径的 pruneExpiredModelCooldowns 同口径）。
		if len(s.ModelCooldowns) > 0 {
			e.modelCooldowns = make(map[string]modelCooldown, len(s.ModelCooldowns))
			for m, smc := range s.ModelCooldowns {
				if smc.Until.IsZero() || !now.Before(smc.Until) {
					continue // 过期/零值丢弃
				}
				e.modelCooldowns[m] = modelCooldown{
					Until:   smc.Until,
					ResetAt: smc.ResetAt,
					Reason:  smc.Reason,
				}
			}
			if len(e.modelCooldowns) == 0 {
				e.modelCooldowns = nil
			}
		}
		p.byUID[uid] = e
	}
}

// adoptSnapshot 采用 Redis 快照为当前池状态，并置 dirty 让下一次落盘把它物化回本地
// state.json（否则快照只在内存生效，下次崩溃恢复又回到旧本地文件）。
func (p *Pool) adoptSnapshot(s snapshot) {
	p.mu.Lock()
	p.applySnapshotLocked(s)
	p.mu.Unlock()
	p.dirty.Store(true)
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}
func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := p.stateOverviewLocked()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.notePersistFail(err)
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		p.notePersistFail(err)
		return
	}
	if p.persistFails > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}
	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打完整错误、每 persistLogEvery 次连续失败打一条提醒、
// 其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
// 恢复成功的日志由 saveLocked 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", p.persistFails, err)
	}
	p.persistFails++
}

// cooledReasonLocked 对非 disabled 账号，若 until 已过期/零值，清空 coolKind/reason
// （惰性清理僵尸 reason）。disabled 账号的 reason 是禁用原因，照常保留。落盘（stateOverviewLocked）
// 与 status（statusOf）共用本判断，避免两条路径口径不一致导致 reason 残留（最多 5s 落盘窗口）。
func cooledReasonLocked(e *entry, now time.Time) (coolKind CoolKind, reason string) {
	if e.disabled || (!e.until.IsZero() && now.Before(e.until)) {
		return e.coolKind, e.reason
	}
	return 0, ""
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	now := time.Now()
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		// 模型级冷却落盘（复用既有落盘循环，不新增遍历）。只写 Until 在未来的条目，
		// 与恢复时过期过滤同口径——落盘即清理，避免 state.json 残留已过期条目。
		var mcs map[string]stateModelCooldown
		if len(e.modelCooldowns) > 0 {
			mcs = make(map[string]stateModelCooldown, len(e.modelCooldowns))
			for m, mc := range e.modelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue // 已过期：不落盘（惰性清理）
				}
				mcs[m] = stateModelCooldown{
					Until:   mc.Until,
					ResetAt: mc.ResetAt,
					Reason:  mc.Reason,
				}
			}
			if len(mcs) == 0 {
				mcs = nil
			}
		}
		// 熔断器 breakerUntil + retryCount 落盘（惰性过滤：仅未过期才写出）。
		// breakerUntil 已过期/零值时不写 breaker_until + retry_count——过期时退避
		// 已无意义，保留 retryCount 是无用退避指数。与恢复侧过期过滤同口径。
		// BreakerUntil 用指针：未过期时取地址写出，过期/零值留 nil（omitempty 省略）。
		var breakerUntil *time.Time
		var retryCount int
		if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
			bu := e.breakerUntil
			breakerUntil = &bu
			retryCount = e.retryCount
		}
		// 连败降权 degradeUntil 落盘（同惰性过滤口径，issue #114）：仅未过期才写出。
		// consecutiveFails 恒写出（含零值，运维口径与 session_dead_fails 的兄弟字段一致）。
		var degradeUntil *time.Time
		if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
			du := e.degradeUntil
			degradeUntil = &du
		}
		// 惰性清理僵尸 reason：until 为零值或已过期时不写出 cool_kind/reason，
		// 避免 state.json 残留「until=0001 零值 + reason=6004 model rate limit」
		// 的不一致快照（模型级冷却不该污染账号级 coolKind/reason 域）。disabled
		// 账号的 reason 是禁用原因，不在冷却语义内，照常保留。
		// 与 statusOf（state.go）共用 cooledReasonLocked，保证落盘与查询同口径。
		coolKind, reason := cooledReasonLocked(e, now)
		sf.Accounts[uid] = stateAccount{
			Credits:          e.credits,
			CreditsTotal:     e.creditsTotal,
			Disabled:         e.disabled,
			Reason:           reason,
			Until:            e.until,
			CoolKind:         coolKind,
			SuccessCount:     e.successCount,
			ErrTotal:         e.errTotal,
			LastSuccess:      e.lastSuccess,
			LastErr:          e.lastErr,
			TokenUsage:       e.tokenUsage,
			SoftStreak:       e.softStreak,
			SessionDeadFails: e.sessionDeadFails,
			ConsecutiveFails: e.consecutiveFails,
			DegradeUntil:     degradeUntil,
			BreakerUntil:     breakerUntil,
			RetryCount:       retryCount,
			CreditsExpiring:  e.creditsExpiring,
			ModelCooldowns:   mcs,
		}
	}
	return sf
}
