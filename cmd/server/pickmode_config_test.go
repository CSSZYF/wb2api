// pickmode_config_test.go pool.pick_mode 配置层语义（缺省/归一化/回落/热生效/示例文件）。
//
// 缺省方向是本键的核心约束：**weighted 必须与加本键前逐字节一致**（顺序模式是用户
// 显式选择的行为变更），故"键缺席 → weighted"、"非法值 → weighted + warn"两条都要锁死。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPickModeDefaultsWeighted 键缺席 → weighted（Default() 与 ParseConfig 两条路径）。
func TestPickModeDefaultsWeighted(t *testing.T) {
	if got := Default().Pool.PickMode; got != "weighted" {
		t.Errorf("Default().Pool.PickMode=%q want weighted", got)
	}
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.PickMode != "weighted" {
		t.Errorf("空配置 Pool.PickMode=%q want weighted（缺省必须与改动前一致）", c.Pool.PickMode)
	}
	if got := PickMode(c.Pool.PickMode); got != pool.PickWeighted {
		t.Errorf("PickMode(weighted)=%v want PickWeighted", got)
	}
}

// TestPickModeNormalize 合法值归一化（大小写/空白不敏感）；非法值回落 weighted。
func TestPickModeNormalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sequential", "sequential"},
		{"Sequential", "sequential"},
		{"  SEQUENTIAL  ", "sequential"},
		{"weighted", "weighted"},
		{"Weighted", "weighted"},
	}
	for _, c := range cases {
		c2, err := ParseConfig([]byte(`{"pool":{"pick_mode":"` + c.in + `"}}`))
		if err != nil {
			t.Fatalf("ParseConfig(%q): %v", c.in, err)
		}
		if c2.Pool.PickMode != c.want {
			t.Errorf("pick_mode=%q 归一化后=%q want %q", c.in, c2.Pool.PickMode, c.want)
		}
	}
	// 非法值：回落 weighted（不 fail fast——与 features.reasoning_history 同风格）。
	for _, bad := range []string{"", "random", "seq", "0"} {
		c2, err := ParseConfig([]byte(`{"pool":{"pick_mode":"` + bad + `"}}`))
		if err != nil {
			t.Fatalf("非法 pick_mode=%q 不应 fail fast: %v", bad, err)
		}
		if c2.Pool.PickMode != "weighted" {
			t.Errorf("非法 pick_mode=%q 应回落 weighted, got %q", bad, c2.Pool.PickMode)
		}
		if _, ok := NormalizePickMode(bad); ok {
			t.Errorf("NormalizePickMode(%q) 应报告非法", bad)
		}
	}
	// 非法值要记 warn（用户能在日志里看到自己的拼写错误）。
	if got := capturePickModeLog(t, func() { _, _ = ParseConfig([]byte(`{"pool":{"pick_mode":"seq"}}`)) }); !strings.Contains(got, "pool.pick_mode") {
		t.Errorf("非法 pick_mode 应记 warn，实际日志=%q", got)
	}
}

// TestPickModeEnvOverride WB2A_PICK_MODE 覆盖 JSON（与其余 env 同口径）。
// 走 Load（它才读 env；ParseConfig 按注释明确不读环境变量）。
func TestPickModeEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"pick_mode":"weighted"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_PICK_MODE", "sequential")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.PickMode != "sequential" {
		t.Errorf("env 覆盖后 Pool.PickMode=%q want sequential", c.Pool.PickMode)
	}
	// 非法 env 值同样回落 weighted（归一化只在一处）。
	t.Setenv("WB2A_PICK_MODE", "bogus")
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Pool.PickMode != "weighted" {
		t.Errorf("非法 env pick_mode 应回落 weighted, got %q", c2.Pool.PickMode)
	}
}

// TestPickModeInExampleConfig config.example.json 必须带 pick_mode 键且与 Default() 一致
// （示例文件是配置项最完整的参考，缺键会让跟随示例的用户看不到这个开关）。
func TestPickModeInExampleConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("config.example.json 非法 JSON: %v", err)
	}
	poolSec, _ := m["pool"].(map[string]any)
	if poolSec == nil {
		t.Fatal("config.example.json 缺 pool 段")
	}
	v, ok := poolSec["pick_mode"]
	if !ok {
		t.Fatal("config.example.json pool 段缺 pick_mode 键")
	}
	if v != "weighted" {
		t.Errorf("config.example.json pick_mode=%v want weighted（与 Default() 一致）", v)
	}
	if _, err := ParseConfig(raw); err != nil {
		t.Errorf("config.example.json 应能被 ParseConfig 解析: %v", err)
	}
}

// capturePickModeLog 捕获一次调用期间的 log 输出（log.SetOutput 是进程级全局：
// 本用例不并行、结束立即还原，与 startup_test.go 同约定）。
func capturePickModeLog(t *testing.T, fn func()) string {
	t.Helper()
	sink := &startupLogSink{}
	old := log.Writer()
	log.SetOutput(sink)
	defer log.SetOutput(old)
	fn()
	return sink.String()
}

// TestSaveConfigAppliesPickModeHot 面板保存 pool.pick_mode 必须**即时生效**。
//
// 锁的是 main.go 的热应用接线（p.SetPickMode）：缺了它配置照样落盘、校验照样通过、
// 面板照样提示"已保存"，但选号仍按启动时的旧模式跑——正是 issue #17 对 max_body_mb
// 描述过的「静默不生效」失效模式。这里走 saveConfig 全链路（校验→落盘→热应用），
// 并用**可观测的选号结果**（而非读配置回显）证明生效。
func TestSaveConfigAppliesPickModeHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"pool":{"pick_mode":"weighted"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	// a 顺序在前但积分最低：weighted 下几乎不会被选中，sequential 下恒被选中——
	// 两种模式的选号结果可直接区分。
	for _, uid := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
	}
	p.SetCredits("a", 1, 0)
	p.SetCredits("b", 100000, 0)
	p.SetOrder([]string{"a", "b"})
	p.SetRandomSource(func(n int64) int64 { return 0 }) // weighted 下恒选高积分号 b

	up := &upstream.Client{
		HTTP: &http.Client{Transport: rotateTripFunc(func(r *http.Request) (*http.Response, error) {
			return rotatingProbeResponse(), nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	// 装配期：与 main.go 同一形状（注入启动时读到的模式）。
	h := server.NewHandler(server.Config{Pool: p, Upstream: up})
	p.SetPickMode(PickMode("weighted"))
	live := livecfg.New(livecfg.Snapshot{})

	// firstPickUID 返回本次请求**首个**被尝试的账号 uid（选号结果的可观测量）。
	firstPickUID := func() string {
		var first string
		up.HTTP.Transport = rotateTripFunc(func(r *http.Request) (*http.Response, error) {
			if first == "" {
				first = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
			}
			return rotatingProbeResponse(), nil
		})
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		return first
	}

	// 装配期基线：weighted → 恒选 b（注入源恒 0 + b 积分高）。
	if got := firstPickUID(); got != "b" {
		t.Fatalf("装配期 weighted 首选=%q want b", got)
	}

	// 保存 sequential：不重启、不重建 handler。未接线时仍选 b（断言即失败）。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"sequential"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := firstPickUID(); got != "a" {
		t.Errorf("保存 pick_mode=sequential 后首选=%q want a（热应用未接线？）", got)
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
	if v, ok := m["pool"]["pick_mode"]; !ok || v != "sequential" {
		t.Errorf("落盘 pool.pick_mode=%v want sequential (raw=%s)", v, raw)
	}
	// 切回 weighted（反方向：证明不是碰巧读到静态值）。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"weighted"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(weighted): %v", err)
	}
	if got := firstPickUID(); got != "b" {
		t.Errorf("切回 weighted 后首选=%q want b", got)
	}
	// 非法值经 normalize 回落 weighted 并热应用（不是把非法值写进运行态）。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"bogus"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(bogus): %v", err)
	}
	if got := firstPickUID(); got != "b" {
		t.Errorf("非法 pick_mode 应回落 weighted 并热应用，首选=%q want b", got)
	}
}

// TestPickModeNotRestartRequired pool.pick_mode 不得出现在重启项清单里
// （它已热生效；列进去会让面板误导用户"改了要重启"）。
func TestPickModeNotRestartRequired(t *testing.T) {
	c := Default()
	for _, f := range restartRequiredFields(c) {
		if strings.Contains(f, "pick_mode") {
			t.Errorf("pick_mode 已热生效，不应列为重启项: %v", restartRequiredFields(c))
		}
	}
}
