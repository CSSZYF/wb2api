package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSaveConfigAppliesCooldownProbeHot 面板保存 schedule.cooldown_probe_* 必须即时生效。
//
// 锁的是 main.go 的热应用接线（sch.SetCooldownProbeInterval）：缺了它，配置照样落盘、
// 校验照样通过、面板照样提示"已保存"，但探活仍按启动时的旧间隔（或仍开着）跑——
// 正是 issue #17 对 max_body_mb 描述过的「静默不生效」失效模式。本用例走 saveConfig
// 全链路（校验→落盘→热应用），而非只测 scheduler 的 setter。
func TestSaveConfigAppliesCooldownProbeHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"schedule":{"cooldown_probe_minutes":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	up := &upstream.Client{}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	live := livecfg.New(livecfg.Snapshot{})

	// 装配期与 main.go 同形：后台循环按启动配置跑起来（长间隔，避免干扰本用例）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sch.StartCooldownProbe(ctx, 10*time.Minute)

	// 保存一个短间隔：热改后应立即读到新值（未接线时仍是装配期的 10m）。
	if _, err := saveConfig([]byte(`{"schedule":{"cooldown_probe_enabled":true,"cooldown_probe_minutes":2}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := sch.CooldownProbeInterval(); got != 2*time.Minute {
		t.Errorf("保存 cooldown_probe_minutes=2 后 interval=%v want 2m（热应用未接线？）", got)
	}

	// 关闭开关：interval 归 0（循环暂停，热启用后立即恢复）。
	if _, err := saveConfig([]byte(`{"schedule":{"cooldown_probe_enabled":false}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig(off): %v", err)
	}
	if got := sch.CooldownProbeInterval(); got != 0 {
		t.Errorf("关闭开关后 interval=%v want 0", got)
	}

	// 重新打开：立即恢复（证明关闭只是暂停，不是"再也起不来"）。
	if _, err := saveConfig([]byte(`{"schedule":{"cooldown_probe_enabled":true,"cooldown_probe_minutes":1}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig(on): %v", err)
	}
	if got := sch.CooldownProbeInterval(); got != time.Minute {
		t.Errorf("重新启用后 interval=%v want 1m", got)
	}
}

// TestCooldownProbeEndToEndClearsExpiredCooldown 端到端：配置驱动的探活循环真的会把
// 「已到期的模型级冷却」探掉——上游提前恢复（200 + SSE）时账号立刻回到可选池。
//
// 这条把三段串起来验：config 解析出的 interval → StartCooldownProbe 循环 →
// pool.CooldownProbeTargets 选择 → 成功解冻。前两段在单元测试里各自覆盖，
// 这里验"装配起来真的管用"（用户视角：撞 6004 后不必干等声明的重置墙钟）。
func TestCooldownProbeEndToEndClearsExpiredCooldown(t *testing.T) {
	// calls 用 atomic：写侧是 httptest 的 handler goroutine，读侧是本测试 goroutine
	// （-race 会抓到裸 int 的跨 goroutine 读写）。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	// 模拟 6004 撞上后上游声明的重置墙钟**已过**（上游提前恢复 / 文案保守）。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(-time.Hour), "glm-5.3", "6004 model rate limit")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sch.StartCooldownProbe(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("探活循环未发起任何上游调用（装配未接线？）")
	}
	// 解冻生效：该模型不再被拦截。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Errorf("探活成功后该模型应可选，got %+v", got)
	}
}

// TestSaveConfigAppliesDegradeHot 面板保存 pool.degrade_* 必须即时生效（issue #114）。
//
// 锁的是 main.go 的热应用接线（p.SetDegrade）：缺了它，配置照样落盘、校验照样通过、
// 面板照样提示"已保存"，但连败阈值仍是启动时的旧值——正是 issue #17 对 max_body_mb
// 描述过的「静默不生效」失效模式。本用例走 saveConfig 全链路（校验→落盘→热应用），
// 而非只测 pool 的 setter。
//
// 用「阈值 1 + 一次 NoteFailures 即降权」作为可观测判据：接线缺失时默认阈值 5 不触发。
func TestSaveConfigAppliesDegradeHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"pool":{"degrade_threshold":5,"degrade_cooldown":"10m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	up := &upstream.Client{}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	live := livecfg.New(livecfg.Snapshot{})
	p.Add(&auth.Auth{UID: "u1"})

	// 保存阈值 1 + 时长 3m：热应用后一次连败即降权，且时长按新值。
	if _, err := saveConfig([]byte(`{"pool":{"degrade_threshold":1,"degrade_cooldown":"3m","degrade_cooldown_max":"2h"}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	p.NoteFailures("u1")
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("保存 degrade_threshold=1 后一次连败即应降权（热应用未接线？）: %+v", st)
	}
	if st.CoolRemaining > int64((3*time.Minute).Seconds()) || st.CoolRemaining < int64((2*time.Minute).Seconds()) {
		t.Errorf("降权时长应约为热改后的 3m, got %ds", st.CoolRemaining)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Errorf("降权后账号应出池, got %v", uids)
	}

	// 再保存一个极大阈值（= 关闭）：重启语义下不再降权（新账号验证新阈值生效）。
	if _, err := saveConfig([]byte(`{"pool":{"degrade_threshold":1000000}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig(off): %v", err)
	}
	p.Add(&auth.Auth{UID: "u2"})
	for n := 0; n < 50; n++ {
		p.NoteFailures("u2")
	}
	st2, _ := p.Status("u2")
	if st2.Cooling {
		t.Errorf("阈值改为极大后连败不应降权（热应用未生效？）: %+v", st2)
	}
}
