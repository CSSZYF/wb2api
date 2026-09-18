package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSaveConfigAppliesAuthWatchHot 面板保存 schedule.auth_watch_* 必须即时生效。
//
// 锁的是 main.go 的热应用接线（sch.SetAuthWatchInterval）：缺了它，配置照样落盘、
// 校验照样通过、面板照样提示"已保存"，但扫描仍按启动时的旧间隔（或仍开着）跑——
// 正是 issue #17 对 max_body_mb 描述过的「静默不生效」失效模式。本用例走
// saveConfig 全链路（校验→落盘→热应用），而非只测 scheduler 的 setter。
func TestSaveConfigAppliesAuthWatchHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"schedule":{"auth_watch_seconds":30}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	up := &upstream.Client{}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	live := livecfg.New(livecfg.Snapshot{})

	// 装配期与 main.go 同形：后台循环按启动配置跑起来（这里给个长间隔，避免干扰）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sch.StartAuthWatch(ctx, authDir, 30*time.Second)

	// 保存一个短间隔：热改后应立即读到新值（未接线时仍是装配期的 30s）。
	if _, err := saveConfig([]byte(`{"schedule":{"auth_watch_enabled":true,"auth_watch_seconds":2}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := sch.AuthWatchInterval(); got != 2*time.Second {
		t.Errorf("保存 auth_watch_seconds=2 后 interval=%v want 2s（热应用未接线？）", got)
	}
	// 间隔热改后循环真的按新值跑：放个账号文件进来，2 秒级间隔内应被发现
	// （未接线时下一轮要等 30 秒，本断言必然超时失败）。
	fp := filepath.Join(authDir, "workbuddy-hot1.json")
	body := `{"auth":{"accessToken":"at-hot1","refreshToken":"rt","expiresAt":9999999999,` +
		`"domain":"","realm":"cn"},"account":{"uid":"hot1"}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { _, ok := p.Status("hot1"); return ok },
		"间隔热改后应在新间隔内发现新账号文件")

	// 关闭开关：interval 归零，循环停扫（删文件也不再同步）。
	if _, err := saveConfig([]byte(`{"schedule":{"auth_watch_enabled":false}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig(false): %v", err)
	}
	if got := sch.AuthWatchInterval(); got != 0 {
		t.Errorf("关闭后 interval=%v want 0", got)
	}
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := p.Status("hot1"); !ok {
		t.Error("关闭扫描后账号不应被剔除（已暂停，不再对账目录）")
	}

	// 重新打开（面板勾回来）：同一循环被 rearm 唤醒，删掉的文件随之出池。
	if _, err := saveConfig([]byte(`{"schedule":{"auth_watch_enabled":true,"auth_watch_seconds":1}}`),
		cfgPath, live, p, up, sch, nil, nil); err != nil {
		t.Fatalf("saveConfig(re-enable): %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { _, ok := p.Status("hot1"); return !ok },
		"重新开启后应发现文件已删除并出池")
}

// TestMainWiresAuthWatchInterval 启动装配必须把 config 解析出的 interval 交给
// StartAuthWatch：这在 main() 里无法单测，故以「保守可测的等价形态」钉住——
// 用 main.go 同一套调用（scheduler.New + StartAuthWatch）验证 config→循环 的传导，
// 任何一侧改了单位（秒/毫秒）或忘了传 interval，本用例都会失败。
func TestMainWiresAuthWatchInterval(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"schedule":{"auth_watch_seconds":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthWatchInterval != 7*time.Second {
		t.Fatalf("AuthWatchInterval=%v want 7s（秒→Duration 换算）", cfg.AuthWatchInterval)
	}

	p := pool.New("")
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: &upstream.Client{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sch.StartAuthWatch(ctx, cfg.AuthDir, cfg.AuthWatchInterval)
	if got := sch.AuthWatchInterval(); got != 7*time.Second {
		t.Errorf("StartAuthWatch 应把 interval 存入循环（%v want 7s）", got)
	}
}

// waitFor 轮询等待 cond 成立，超时则 t.Fatal 并附上 msg。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
