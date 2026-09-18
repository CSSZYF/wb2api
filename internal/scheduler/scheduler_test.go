package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// TestNextWakeKeepaliveOnly 签到已过点时按保活整点唤醒。
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll 签到与保活配到同一整点时两类任务都要执行。
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	s := New(Config{
		CheckinHours:     []int{9, 22},
		TravelHours:      []int{}, // 禁用旅行时点干扰（仅测签到+保活同整点）
		ActivityHours:    []int{}, // 禁用活跃时点干扰
		KeepaliveHours:   []int{22},
		TravelDisabled:   true,
		ActivityDisabled: true,
		BlackcatDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive（同一时刻两任务）", kinds)
	}

	// 22 点过后下一次是次日 09:00，且只含签到（旅行/活跃已禁用）。
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeNothingScheduled 两类任务全空时返回零值，Run 只等退出信号。
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled 显式禁用签到后，排程里不再有签到时点（保活照常）。
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 21 点签到）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled 显式禁用保活后，排程里不再有保活时点（签到照常）。
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 22 点保活）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled 五类任务都显式禁用 → 无可唤醒时点。
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		CheckinHours:      []int{9, 21},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls 四类任务全禁用：Run 不空转（只等退出信号），
// 且不能触发任何上游请求。
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:              p,
		Upstream:          up,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // 阻塞到 ctx 取消为止（无时点可等，不构造 timer）
	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0（四类全禁用）", calls.Load())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（不应提前返回）", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（不应空转/忙等）", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

// expiringStub 返回一个套餐到期时间可控的 get-user-resource 响应，用于验证
// 快过期窗口（ExpiringSoonWindow）的分桶行为；余额固定 100。
// 到期字段必须是 CycleEndTime（上游响应字段全集实测无 PackageEndTime，旧桩喂
// PackageEndTime 会把「读到错误字段」的 bug 掩盖过去）。
func expiringStub(endTime string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleEndTime":"` + endTime +
				`","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

// creditsExpiringFromState 从 state.json 读出账号的快过期积分子集（0 = 未分桶）。
//
// 为什么不直接断言 pool 字段：creditsExpiring 是 pool 的私有运行态，对外唯一可观测
// 口径就是 state.json 的 credits_expiring（与 pool 侧 TestCreditsExpiringPersistRoundTrip
// 同法，且不为此新增导出 API）。
func creditsExpiringFromState(t *testing.T, fp, uid string) int64 {
	t.Helper()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			CreditsExpiring int64 `json:"credits_expiring"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v\n%s", err, raw)
	}
	return sf.Accounts[uid].CreditsExpiring
}

// TestSetExpiringSoonWindowHotApplies 热改快过期窗口（面板 pool.expiring_soon）后，
// 下一轮余额刷新立即按**新窗口**分桶：窗口 1h 时"3 天后到期"的余额不计入，
// 热改为 30d 后同一账号立即被计入。
//
// 为什么以"放大窗口"为判据：pool 侧只在 expiring>0 时写入（SetCreditsDetailed），
// 所以缩小窗口不会主动清掉陈旧分桶——但那是**既有语义**，重启也一样（creditsExpiring
// 持久化在 state.json，恢复后同样只在下次分桶时被覆盖），故热改与重启严格等价。
// 放大窗口则必然经过一次真实写入，能区分"读到新窗口"与"仍读启动初值"：
// 若读取点仍读 cfg.ExpiringSoonWindow（本用例注入的 1h），第二次断言必失败。
func TestSetExpiringSoonWindowHotApplies(t *testing.T) {
	// 到期时间 = UTC+8 的 3 天后（30d 窗口内、1h 窗口外）。
	cst := time.FixedZone("CST", 8*3600)
	end := time.Now().In(cst).Add(72 * time.Hour).Format("2006-01-02 15:04:05")
	srv := expiringStub(end)
	defer srv.Close()

	fp := filepath.Join(t.TempDir(), "state.json")
	p := pool.New(fp)
	defer p.Close() // 停后台落盘 goroutine（本用例传了 state 路径，New 会起 flusher）
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	// 启动初值 1h：3 天后到期的余额不在窗口内 → 不分桶。
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: time.Hour})

	s.RunBalanceRefreshNow()
	p.Flush()
	if got := creditsExpiringFromState(t, fp, "u1"); got != 0 {
		t.Fatalf("1h 窗口下 creditsExpiring=%d want 0（3 天后到期不在窗口内）", got)
	}
	// credits 本身照常更新（窗口只影响分桶，不影响余额口径）。
	if st, _ := p.Status("u1"); st.Credits != 100 {
		t.Fatalf("credits=%d want 100（窗口不影响余额本身）", st.Credits)
	}

	// 热改窗口到 30d（不经重启、不重建 Scheduler）→ 下一轮刷新即分桶。
	s.SetExpiringSoonWindow(30 * 24 * time.Hour)
	s.RunBalanceRefreshNow()
	p.Flush()
	if got := creditsExpiringFromState(t, fp, "u1"); got != 100 {
		t.Fatalf("热改为 30d 窗口后 creditsExpiring=%d want 100（应在窗口内）", got)
	}

	// 再缩回 1h：不主动清旧分桶（与重启行为一致——见用例注释），但新账号不再分桶。
	s.SetExpiringSoonWindow(time.Hour)
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s.RunBalanceRefreshNow()
	p.Flush()
	if got := creditsExpiringFromState(t, fp, "u2"); got != 0 {
		t.Fatalf("缩回 1h 后新账号 u2 creditsExpiring=%d want 0", got)
	}
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// P0-1：12153 连续 N 次才禁用。前 2 次刷新失败不应杀号（误判防护）。
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	// 第 3 次连续 12153 → 禁用。
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("第 3 次连续 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess 两次 12153 后刷新成功 → 计数清零，
// 再来的 12153 从第 1 次重新计（不会因历史失败被继续追杀）。
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // 第 3 次（本次调度循环的第二轮）刷新成功
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow() // 12153 #1
	s.RunKeepaliveNow() // 12153 #2
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("precondition: 前 2 次不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 刷新成功 → 清计数
	// 接下来连续 2 次 12153：从新计数重新算，仍不应禁用（历史计数已清）。
	s.RunKeepaliveNow() // 12153 #1（新计数）
	s.RunKeepaliveNow() // 12153 #2（新计数）
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("刷新成功清计数后连续 2 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 12153 #3（新计数）→ 禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("新计数第 3 次 12153 应禁用: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

// TestRunBalanceRefreshNowUpdatesCreditsAndRevives 只查余额（不签到）即可更新 credits
// 并解冻余额恢复的**硬冷却**账号——面板手动刷新与后台周期任务（StartBalanceRefresh，
// 默认每 5 分钟）共用该语义。issue #199 收窄后仅 CoolHard 走解冻路径（见下一条用例）。
func TestRunBalanceRefreshNowUpdatesCreditsAndRevives(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 777}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	p.Disable("u2", "manual")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBalanceRefreshNow()

	st, _ := p.Status("u1")
	if st.Cooling || st.Credits != 777 {
		t.Errorf("u1 want revived with credits=777: cooling=%v credits=%d", st.Cooling, st.Credits)
	}
	if f.checkinCalls.Load() != 0 {
		t.Errorf("balance refresh must not checkin, got %d calls", f.checkinCalls.Load())
	}
	// 禁用账号不参与：其 credits 保持 0（未被 UserResource 覆盖解冻）。
	if st2, _ := p.Status("u2"); !st2.Disabled {
		t.Errorf("u2 must stay disabled")
	}
}

// TestRunBalanceRefreshNowKeepsSoftCooling issue #199 回归：余额刷新**不得**解冻软冷却
// 账号。本用例覆盖后台 5 分钟周期路径（StartBalanceRefresh → RunBalanceRefreshNow）
// 与面板「刷新」按钮（balanceAll → 同一函数）共用的语义：旧实现 remain > 0 无条件
// 解冻 → 全池软冷却/6004 账号每 5 分钟被自动解冻 → 选号重新选中 → 又撞 429 循环。
// 收窄后 credits 照常更新（观测量新鲜），冷却状态原样保留。
func TestRunBalanceRefreshNowKeepsSoftCooling(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 888}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "soft", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "model", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	// 账号级软冷却（429 无重置时间 → 有界退避）。
	p.CooldownSoftRate("soft", time.Hour, time.Time{}, "429 rate limit")
	// 仅 6004 模型级冷却（coolKind 未设 = 零值 CoolHard，正是零值陷阱的回归点）。
	p.CooldownSoftForModel("model", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBalanceRefreshNow()

	stSoft, _ := p.Status("soft")
	if stSoft.Credits != 888 {
		t.Errorf("soft credits=%d want 888（余额照常更新）", stSoft.Credits)
	}
	if !stSoft.Cooling || stSoft.CoolKind != "soft_rate" {
		t.Errorf("软冷却不得被余额刷新解冻（issue #199）: %+v", stSoft)
	}
	stModel, _ := p.Status("model")
	if stModel.Credits != 888 {
		t.Errorf("model credits=%d want 888（余额照常更新）", stModel.Credits)
	}
	if len(stModel.RateLimitedModels) != 1 || stModel.RateLimitedModels[0].Model != "glm-5.3" {
		t.Errorf("6004 模型级冷却台账应保留: %+v", stModel.RateLimitedModels)
	}
	if stModel.Cooling {
		t.Errorf("模型级冷却不写账号级 until，不应 Cooling: %+v", stModel)
	}
	// 模型豁免仍生效：同模型不可用、切模型可用（用 AvailableUIDsForModel 直接断言
	// 健康口径，避免受全冷却兜底选号影响）。
	if got := p.AvailableUIDsForModel("glm-5.3"); len(got) != 0 {
		t.Errorf("同模型可用集应排除仍被 6004 冷却的 model 号，got %v", got)
	}
	if got := p.AvailableUIDsForModel("hy3-x"); len(got) != 1 || got[0] != "model" {
		t.Errorf("切模型应豁免选中 model 号，got %v", got)
	}
}
