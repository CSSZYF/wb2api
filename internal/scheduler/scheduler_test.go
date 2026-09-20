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
// 热改为 30d 后同一账号立即被计入，再缩回 1h 又复位为 0。
//
// 两个方向都要断言：放大窗口必然经过一次真实写入，能区分"读到新窗口"与"仍读启动初值"
// （若读取点仍读 cfg.ExpiringSoonWindow 这个启动初值，放大后的断言必失败）；缩回窗口
// 则验证 expiring==0 的复位（缺陷 A：旧实现在 expiring>0 才写分桶，缩小窗口后陈旧值
// 永久留存——热改与重启都不该让旧分桶残留）。
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

	// 再缩回 1h：本轮上游仍报"3 天后到期"，但已不在窗口内 → 必须复位为 0
	// （缺陷 A：旧实现只在 expiring>0 时写分桶，此处会残留 100）；新账号同样为 0。
	s.SetExpiringSoonWindow(time.Hour)
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s.RunBalanceRefreshNow()
	p.Flush()
	if got := creditsExpiringFromState(t, fp, "u1"); got != 0 {
		t.Fatalf("缩回 1h 后 u1 creditsExpiring=%d want 0（陈旧分桶必须复位）", got)
	}
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

// TestCheckinAndKeepaliveIncludeManualDisabled 临时停用号必须仍参与签到与 token 保活
// （上游 a20d06f / issue #138 用户硬约束：停用只是「对话流量摘除」，签到/保活/排程照常）。
//
// scheduler 的跳过判据只看 st.Disabled（scheduler.go:412/469/536/570 等），本锚防止
// 未来有人把判据改成「Disabled || ManualDisabled」时无声破坏停用号的积分与 token 活性
// ——那会让「临时停用」退化成「账号冻结」，正是本特性刻意区分的两件事。
func TestCheckinAndKeepaliveIncludeManualDisabled(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	p.SetManualDisabled("u1", true, "观察几天")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})

	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("临时停用号应照常签到, calls=%d", f.checkinCalls.Load())
	}
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("临时停用号应照常保活刷 token, calls=%d", f.refreshCalls.Load())
	}

	st, _ := p.Status("u1")
	if !st.ManualDisabled {
		t.Fatalf("签到/保活不得解除临时停用: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("签到应照常回填余额, credits=%d want 500", st.Credits)
	}
	if st.Disabled {
		t.Error("临时停用不得被签到/保活路径升格成永久禁用")
	}
	// 仍不参与选号（活性照常 ≠ 可用）。
	if got := p.Pick(); got != nil {
		t.Fatalf("停用号仍不应被选中, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 快过期积分（缺陷 A：expiring==0 时陈旧值永不复位）
//
// 旧形态（runCheckin / RunBalanceRefreshNow 两处同形）：
//
//	ReenableIfCredits(uid, remain, total)
//	if expiring > 0 { SetCreditsDetailed(uid, remain, total, expiring) }
//
// expiring==0 时分支不进入 → creditsExpiring 保持上一轮的非零值（积分到期/窗口缩小/
// 上游不再下发到期字段后，选号权重与面板都还在按陈旧值行事）。
// ---------------------------------------------------------------------------

// mutableExpiringStub 到期时间可运行期改口的 get-user-resource 桩（余额固定 100）：
// 用于构造「先分桶为非零、再刷新为 0」的序列（缺陷 A 的核心回归形态）。
// end 为空串 = 响应**不带** CycleEndTime 字段（模拟上游不给到期时间）。
type mutableExpiringStub struct{ end atomic.Value }

func newMutableExpiringStub(end string) *mutableExpiringStub {
	s := &mutableExpiringStub{}
	s.end.Store(end)
	return s
}

func (m *mutableExpiringStub) set(end string) { m.end.Store(end) }

func (m *mutableExpiringStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			http.Error(w, "not found", 404)
			return
		}
		acct := `{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0}`
		if end, _ := m.end.Load().(string); end != "" {
			acct = `{"PackageName":"p","CycleEndTime":"` + end + `","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0}`
		}
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` + acct + `]}}}}`))
	}))
}

// cstWallClock 把时刻格式化成上游 CycleEndTime 的墙钟串（UTC+8，与 softRateResetLoc 同口径）。
func cstWallClock(tt time.Time) string {
	return tt.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
}

// TestRunBalanceRefreshResetsExpiringToZero 余额刷新路径的核心回归：expiring==0 时
// 必须把 creditsExpiring 复位为 0（旧实现只在 expiring>0 时写分桶）。
func TestRunBalanceRefreshResetsExpiringToZero(t *testing.T) {
	stub := newMutableExpiringStub(cstWallClock(time.Now().Add(72 * time.Hour)))
	srv := stub.server()
	defer srv.Close()

	fp := filepath.Join(t.TempDir(), "state.json")
	p := pool.New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})

	s.RunBalanceRefreshNow()
	if st, _ := p.Status("u1"); st.CreditsExpiring != 100 {
		t.Fatalf("前置：3 天后到期在 7 天窗内，credits_expiring=%d want 100", st.CreditsExpiring)
	}

	// 上游改口：到期时间推到 30 天后（7 天窗外）→ 本轮必须把分桶复位为 0。
	stub.set(cstWallClock(time.Now().Add(30 * 24 * time.Hour)))
	s.RunBalanceRefreshNow()
	st, _ := p.Status("u1")
	if st.CreditsExpiring != 0 {
		t.Errorf("窗口外刷新后 credits_expiring=%d want 0（expiring==0 必须复位，陈旧值永久留存即缺陷 A）", st.CreditsExpiring)
	}
	if st.Credits != 100 {
		t.Errorf("余额本身照常更新：credits=%d want 100", st.Credits)
	}
	// 落盘口径同步：state.json 不再残留旧分桶（重启后也不会失忆地沿用）。
	p.Flush()
	if got := creditsExpiringFromState(t, fp, "u1"); got != 0 {
		t.Errorf("state.json credits_expiring=%d want 0（陈旧分桶已复位）", got)
	}
}

// TestRunCheckinResetsExpiringToZero 签到路径同形缺陷的回归（两处必须一起修，
// 否则「签到 09:00/21:00」这条更常跑的路径仍会把陈旧值写回去）。
func TestRunCheckinResetsExpiringToZero(t *testing.T) {
	stub := newMutableExpiringStub(cstWallClock(time.Now().Add(72 * time.Hour)))
	srv := stub.server()
	defer srv.Close()

	fp := filepath.Join(t.TempDir(), "state.json")
	p := pool.New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})

	s.RunCheckinNow()
	if st, _ := p.Status("u1"); st.CreditsExpiring != 100 {
		t.Fatalf("前置：签到后 3 天后到期的余额应计入，credits_expiring=%d want 100", st.CreditsExpiring)
	}

	stub.set(cstWallClock(time.Now().Add(30 * 24 * time.Hour)))
	s.RunCheckinNow()
	if st, _ := p.Status("u1"); st.CreditsExpiring != 0 {
		t.Errorf("签到刷新后 credits_expiring=%d want 0（签到路径同样必须复位）", st.CreditsExpiring)
	}
}

// TestRunCheckinExpiringKeepsReviveSemantics 分桶同步与解冻是两件事，都不得回归：
//   - 硬冷却（余额耗尽）账号余额恢复 → 照常解冻（ReenableIfCredits 既有语义），分桶写入；
//   - 软冷却账号 → **不**被签到解冻（issue #199 收窄），但分桶照常写入
//     （SetCreditsExpiring 无解冻语义，只同步观测量）。
func TestRunCheckinExpiringKeepsReviveSemantics(t *testing.T) {
	stub := newMutableExpiringStub(cstWallClock(time.Now().Add(72 * time.Hour)))
	srv := stub.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "hard", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "soft", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Cooldown("hard", pool.CoolHard, time.Hour, "余额不足")
	p.CooldownSoftRate("soft", time.Hour, time.Time{}, "429 rate limit")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	s.RunCheckinNow()

	hard, _ := p.Status("hard")
	if hard.Cooling {
		t.Errorf("硬冷却账号余额恢复应照常解冻：%+v", hard)
	}
	if hard.CreditsExpiring != 100 {
		t.Errorf("硬冷却解冻路径也必须写入分桶：credits_expiring=%d want 100", hard.CreditsExpiring)
	}
	soft, _ := p.Status("soft")
	if !soft.Cooling {
		t.Errorf("软冷却账号不得被签到解冻（issue #199）：%+v", soft)
	}
	if soft.CreditsExpiring != 100 {
		t.Errorf("软冷却账号的分桶照常写入（分桶 ≠ 解冻）：credits_expiring=%d want 100", soft.CreditsExpiring)
	}
}

// TestBalanceDiagLogDistinguishesNoExpiryFromNoParse 诊断日志（本任务关键交付）：
// 每个账号每轮一条，让运维一眼区分
//   - 「窗口内确实没有快过期积分」（有可解析到期时间，但都在窗口外）；
//   - 「根本没解析到任何 CycleEndTime」（上游字段变化/解析失败，不是"没有"）。
//
// 日志形如：
//
//	balance uid=01234567 剩余=100/100 快过期=0（窗口=168h，包裹数=1，可解析到期=1，最近到期=2026-09-24 05:00:00）
func TestBalanceDiagLogDistinguishesNoExpiryFromNoParse(t *testing.T) {
	const uid = "0123456789abcdef"
	sink := captureLog(t)

	// ① 到期时间在窗口外（窗口 1h，到期在 3 天后）→ 快过期=0，但可解析到期=1 且有最近到期。
	stub := newMutableExpiringStub(cstWallClock(time.Now().Add(72 * time.Hour)))
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: time.Hour})
	s.RunBalanceRefreshNow()

	got := sink.String()
	if n := strings.Count(got, "balance uid=01234567"); n != 1 {
		t.Fatalf("每账号每轮应恰好一条分桶日志，got %d 条：\n%s", n, got)
	}
	if strings.Contains(got, uid) {
		t.Errorf("日志不得泄漏全量 uid（只打 uid8）：\n%s", got)
	}
	for _, want := range []string{"剩余=100/100", "快过期=0", "窗口=1h", "包裹数=1", "可解析到期=1", "最近到期="} {
		if !strings.Contains(got, want) {
			t.Errorf("分桶日志缺 %q（窗口内没有 vs 没解析到 必须可分）：\n%s", want, got)
		}
	}

	// ② 响应不带 CycleEndTime → 可解析到期=0，且**不带**最近到期段（区分「没解析到」）。
	sink.Reset()
	stub.set("")
	s.RunBalanceRefreshNow()
	got = sink.String()
	if !strings.Contains(got, "可解析到期=0") {
		t.Errorf("无到期字段时应报 可解析到期=0：\n%s", got)
	}
	if strings.Contains(got, "最近到期=") {
		t.Errorf("没有任何可解析到期时间时不应出现最近到期段：\n%s", got)
	}
}
