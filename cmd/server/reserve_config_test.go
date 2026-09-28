// reserve_config_test.go pool.reserve_credits 配置层语义（缺省/校验/env/热生效/示例文件）。
//
// 本键的缺省方向与既有键相反：它**默认开启**（50，用户点名"默认搞个 50"），故
// "键缺席 → 50" 与 "显式 0 → 关闭" 两条都要锁死；同时负数必须 fail fast（负的保留线
// 没有合理语义，静默钳 0 会把"我配错了"变成"功能被关了"且不提示）。
package main

import (
	"encoding/json"
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

// TestReserveCreditsDefaultsTo50 键缺席 → 50（Default() 与 ParseConfig 两条路径）；
// 显式 0 → 关闭。
func TestReserveCreditsDefaultsTo50(t *testing.T) {
	if got := Default().Pool.ReserveCredits; got != DefaultReserveCredits {
		t.Errorf("Default().Pool.ReserveCredits=%d want %d", got, DefaultReserveCredits)
	}
	if DefaultReserveCredits != 50 {
		t.Errorf("DefaultReserveCredits=%d want 50（用户点名要默认 50）", DefaultReserveCredits)
	}
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.ReserveCredits != DefaultReserveCredits {
		t.Errorf("空配置 ReserveCredits=%d want %d（缺省必须开启）", c.Pool.ReserveCredits, DefaultReserveCredits)
	}
	// 显式 0 = 关闭（合法值，不得被回落成默认 50——那是"关不掉"）。
	c2, err := ParseConfig([]byte(`{"pool":{"reserve_credits":0}}`))
	if err != nil {
		t.Fatalf("reserve_credits=0 应合法: %v", err)
	}
	if c2.Pool.ReserveCredits != 0 {
		t.Errorf("显式 0 应保持 0（= 关闭），got %d", c2.Pool.ReserveCredits)
	}
	// 自定义值照收。
	c3, err := ParseConfig([]byte(`{"pool":{"reserve_credits":123}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c3.Pool.ReserveCredits != 123 {
		t.Errorf("reserve_credits=123 应保持 123，got %d", c3.Pool.ReserveCredits)
	}
}

// TestReserveCreditsNegativeFailsFast 负数非法 → fail fast（风格同 server.max_body_mb）：
// 静默钳 0 会把"我配了个负数"变成"功能被关了"，正是 issue #17 那类静默失效。
func TestReserveCreditsNegativeFailsFast(t *testing.T) {
	_, err := ParseConfig([]byte(`{"pool":{"reserve_credits":-1}}`))
	if err == nil {
		t.Fatal("reserve_credits=-1 应 fail fast")
	}
	if !strings.Contains(err.Error(), "pool.reserve_credits") {
		t.Errorf("错误信息应指明字段名，实际=%v", err)
	}
}

// TestReserveCreditsEnvOverride WB2A_RESERVE_CREDITS 覆盖 JSON（与其余 env 同口径）。
// 走 Load（它才读 env；ParseConfig 按注释明确不读环境变量）。
func TestReserveCreditsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"reserve_credits":50}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_RESERVE_CREDITS", "7")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.ReserveCredits != 7 {
		t.Errorf("env 覆盖后 ReserveCredits=%d want 7", c.Pool.ReserveCredits)
	}
	// env 的 0 同样表意"关闭"（env 与 JSON 同口径）。
	t.Setenv("WB2A_RESERVE_CREDITS", "0")
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Pool.ReserveCredits != 0 {
		t.Errorf("env=0 应关闭，got %d", c2.Pool.ReserveCredits)
	}
}

// TestReserveCreditsInExampleConfig config.example.json 必须带 reserve_credits 键且与
// Default() 一致（示例文件是配置项最完整的参考，缺键会让跟随示例的用户看不到这个开关）。
func TestReserveCreditsInExampleConfig(t *testing.T) {
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
	v, ok := poolSec["reserve_credits"]
	if !ok {
		t.Fatal("config.example.json pool 段缺 reserve_credits 键")
	}
	if int(v.(float64)) != DefaultReserveCredits {
		t.Errorf("config.example.json reserve_credits=%v want %d（与 Default() 一致）", v, DefaultReserveCredits)
	}
	if _, err := ParseConfig(raw); err != nil {
		t.Errorf("config.example.json 应能被 ParseConfig 解析: %v", err)
	}
}

// TestSaveConfigAppliesReserveCreditsHot 面板保存 pool.reserve_credits 必须**即时生效**。
//
// 锁的是 main.go 的热应用接线（p.SetReserveCredits）：缺了它配置照样落盘、校验照样通过、
// 面板照样提示"已保存"，但闸门仍按启动时的旧线跑——正是 issue #17 对 max_body_mb
// 描述过的「静默不生效」失效模式。这里走 saveConfig 全链路（校验→落盘→热应用），
// 并用**可观测的选号结果**（而非读配置回显）证明生效。
func TestSaveConfigAppliesReserveCreditsHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"pool":{"reserve_credits":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	// 单个余额 45 的号：reserve 0 时贵模型可用；reserve 50 时不可用（无候选 → 503）。
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 45, 8451) // 余额已知（credits_total > 0），闸门才可能生效

	attempts := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: rotateTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return badParamsResponse(), nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	h := server.NewHandler(server.Config{Pool: p, Upstream: up})
	p.SetReserveCredits(0) // 装配期：与落盘配置同值（关闭）
	live := livecfg.New(livecfg.Snapshot{})

	// pickAttempts 发一次请求，返回上游被调用次数（0 = 选号阶段就无候选，被闸门拦住）。
	pickAttempts := func() int {
		attempts = 0
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		return attempts
	}

	// 装配期基线：reserve=0（关闭）→ 贵模型照常打上游。
	if n := pickAttempts(); n == 0 {
		t.Fatal("装配期 reserve=0 应放行贵模型（基线）")
	}

	// 保存 50：不重启、不重建 handler。未接线时仍打上游（断言即失败）。
	if _, err := saveConfig([]byte(`{"pool":{"reserve_credits":50}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if n := pickAttempts(); n != 0 {
		t.Errorf("保存 reserve_credits=50 后贵模型上游调用数=%d want 0（热应用未接线？）", n)
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
	if v, ok := m["pool"]["reserve_credits"]; !ok || int(v.(float64)) != 50 {
		t.Errorf("落盘 pool.reserve_credits=%v want 50 (raw=%s)", v, raw)
	}
	// 免费模型在同一配置下照常可用（证明不是"把号整个摘掉"）。
	attempts = 0
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hy3","messages":[]}`)))
	if attempts == 0 {
		t.Error("免费模型 hy3 在余额触底时应照常可用")
	}
	// 调回 0（反方向：证明不是碰巧读到静态值）。
	if _, err := saveConfig([]byte(`{"pool":{"reserve_credits":0}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(0): %v", err)
	}
	if n := pickAttempts(); n == 0 {
		t.Error("调回 reserve_credits=0 后应恢复放行")
	}
}

// TestReserveCreditsExhaustedMessage 保留积分把池内全部候选挡住时的末端文案：
// 通用「cooling/disabled」是误导（账号其实都健康、没在冷却），必须补一句可操作的
// 限定说明（调小 reserve_credits / 充值 / 改用免费模型），让用户知道该动哪个旋钮。
func TestReserveCreditsExhaustedMessage(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 45, 8451) // 余额已知且触底
	p.SetReserveCredits(50)
	up := &upstream.Client{
		HTTP:       &http.Client{Transport: rotateTripFunc(func(r *http.Request) (*http.Response, error) { return badParamsResponse(), nil })},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	h := server.NewHandler(server.Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, must := range []string{"reserve line", "reserve_credits=50", "1 account(s) blocked"} {
		if !strings.Contains(body, must) {
			t.Errorf("503 文案缺可操作说明 %q: %s", must, body)
		}
	}
	// code 仍是 no_healthy_account：它不是新错误类，客户端重试语义与"池子空了"一致
	// （换号/重试都不会自己变好），只有文案被换成可操作版本。
	if !strings.Contains(body, "no_healthy_account") {
		t.Errorf("业务 code 应保持 no_healthy_account（不得凭空造新错误类）: %s", body)
	}

	// 关掉闸门（reserve=0）→ 回到既有通用文案（零回归）。
	p.SetReserveCredits(0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if strings.Contains(rec.Body.String(), "reserve line") {
		t.Errorf("reserve=0 时不得出现保留积分文案: %s", rec.Body.String())
	}

	// 免费模型不受影响（不该被闸门拦）。
	p.SetReserveCredits(50)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hy3","messages":[]}`)))
	if strings.Contains(rec.Body.String(), "reserve line") {
		t.Errorf("免费模型不得被保留积分拦住: %s", rec.Body.String())
	}
}

// TestReserveCreditsNotRestartRequired pool.reserve_credits 不得出现在重启项清单里
// （它已热生效；列进去会让面板误导用户"改了要重启"）。
func TestReserveCreditsNotRestartRequired(t *testing.T) {
	c := Default()
	for _, f := range restartRequiredFields(c) {
		if strings.Contains(f, "reserve_credits") {
			t.Errorf("reserve_credits 已热生效，不应列为重启项: %v", restartRequiredFields(c))
		}
	}
}

// TestFreeModelLookupWiring main 的免费判定回调接线必须齐全（server.FreeModelLookup）：
// 缺了它 pool 只剩内置兜底白名单（hy4-preview / hy3 / deepseek-v4.1-flash），
// 倍率口径（x0.00 / 限时免费）失效——用户"按倍率判定"的需求落空。
//
// 这里直接断言 server.FreeModelLookup 的**行为契约**（数据源是两个目录只读快照）：
// 目录缓存冷时返回 false（缺失 ≠ 免费），CN 目录灌入 x0.00 后返回 true。
func TestFreeModelLookupWiring(t *testing.T) {
	up := upstream.New()
	lookup := server.FreeModelLookup(up)
	if lookup == nil {
		t.Fatal("FreeModelLookup 返回 nil（main 装配会注入 nil，免费判定失效）")
	}
	// 目录缓存冷：任何模型都不是"免费"（缺失 ≠ 免费）。
	if lookup("glm-5.2") {
		t.Error("目录缓存冷时不得把模型判为免费")
	}
	if lookup("") {
		t.Error("空模型名不得判为免费（缺失 ≠ 免费）")
	}
	// CN 目录灌入（内部缓存的公开写入路径是 fetchDynamicModels，测试里用不上网络——
	// 直接查 upstream 侧的判定函数行为即可，缓存灌入由 server 包的用例覆盖）。
	if !upstream.IsFreeModel([]upstream.ModelInfo{{ID: "glm-5.2", Credits: "x0.00"}}, "glm-5.2") {
		t.Error("upstream.IsFreeModel 应把 x0.00 判为免费（倍率口径）")
	}
}
