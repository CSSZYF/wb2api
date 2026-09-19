package main

// reasoninghistory_hot_test.go features.reasoning_history 面板热改接线回归。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSaveConfigAppliesReasoningHistoryHot 面板保存 features.reasoning_history 必须即时生效。
//
// 锁的是 main.go 的热应用接线（up.SetReasoningHistory）：缺了它，配置照样落盘、校验照样
// 通过、面板照样提示"已保存"，但出站仍按启动时的档位组装 body——正是 issue #17 对
// max_body_mb 描述过的「静默不生效」失效模式（改了配置，行为不变，还不提示重启）。
// 因此这里不复用上游包的单元测试，而是走 saveConfig 全链路（校验→落盘→热应用）。
func TestSaveConfigAppliesReasoningHistoryHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"features":{"reasoning_history":"full"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	h := server.NewHandler(server.Config{Pool: p, Upstream: up})
	live := livecfg.New(livecfg.Snapshot{})

	if got := up.ReasoningHistoryMode(); got != upstream.ReasoningHistoryFull {
		t.Fatalf("装配期档位 = %q want full", got)
	}
	// sess 传 nil = 本用例不涉会话粘性（saveConfig 已容忍 nil：跳过热应用）。
	if _, err := saveConfig([]byte(`{"features":{"reasoning_history":"blank"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(blank): %v", err)
	}
	if got := up.ReasoningHistoryMode(); got != upstream.ReasoningHistoryBlank {
		t.Errorf("保存 blank 后档位 = %q want blank（热应用未接线？）", got)
	}
	// 落盘值也要对（面板 GET 回显依赖它）。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["features"]["reasoning_history"]; !ok || v != "blank" {
		t.Errorf("落盘 features.reasoning_history=%v want blank (raw=%s)", v, raw)
	}
	// 反方向：切到 last（证明不是碰巧读到静态值）。
	if _, err := saveConfig([]byte(`{"features":{"reasoning_history":"last"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(last): %v", err)
	}
	if got := up.ReasoningHistoryMode(); got != upstream.ReasoningHistoryLast {
		t.Errorf("保存 last 后档位 = %q want last", got)
	}
	// 非法值：不报错（fail-safe），落盘与运行时都回落 full。
	if _, err := saveConfig([]byte(`{"features":{"reasoning_history":"bogus"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(bogus) 不该报错（fail-safe）: %v", err)
	}
	if got := up.ReasoningHistoryMode(); got != upstream.ReasoningHistoryFull {
		t.Errorf("非法档位热改后 = %q want full", got)
	}
}
