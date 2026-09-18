package main

import (
	"encoding/json"
	"io"
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

// rotateTripFunc 测试用 RoundTripper。
type rotateTripFunc func(*http.Request) (*http.Response, error)

func (f rotateTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// badParamsResponse 上游 400 + 11101（ErrBadParams）：不罚账号但**仍然轮转**，
// 每轮必换新号——上游调用次数因此恰好等于本轮生效的换号上限，是量上限的干净探针。
func badParamsResponse() *http.Response {
	return &http.Response{
		StatusCode: 400,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`)),
	}
}

// TestSaveConfigAppliesMaxRotateHot 面板保存 server.max_rotate 必须即时生效。
//
// 锁的是 main.go 的热应用接线（srv.SetMaxRotate）：缺了它，配置照样落盘、校验照样
// 通过、面板照样提示"已保存"，但轮转仍按启动时的旧值跑——正是 issue #17 对
// max_body_mb 描述过的「静默不生效」失效模式（改了配置，行为不变，还不提示重启）。
// 因此这里不复用 handler 单元测试，而是走 saveConfig 全链路（校验→落盘→热应用）。
func TestSaveConfigAppliesMaxRotateHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"server":{"max_body_mb":8}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 池内 4 个号（> 默认上限 3，才能区分 3 与 4）。
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 0)
	}
	attempts := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: rotateTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return badParamsResponse(), nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	// 装配期 handler：与 main.go 同一形状（注入 cfg.Server.MaxRotate）。
	// 装配置 2 而非默认 3：断言走「调小到 1 / 调大到 2」两个方向即可证伪接线缺失
	// （未接线时恒为装配期的 2），且两条路径的轮转退避为 0ms/500ms——本用例要覆盖
	// main.go 的热应用接线，但没必要为退避付出十几秒。
	h := server.NewHandler(server.Config{Pool: p, Upstream: up, MaxRotate: 2})
	live := livecfg.New(livecfg.Snapshot{})

	// rotateAttempts 发一次请求，返回上游被调用次数（= 本轮生效的换号上限）。
	rotateAttempts := func() int {
		attempts = 0
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		return attempts
	}

	// 调小到 1：不重启、不重建 handler。未接线时仍按装配期的 2 跑（断言即失败）。
	// 末参 sess=nil = 粘性路由未建（本用例不覆盖 session_sticky.ttl 热改）。
	if _, err := saveConfig([]byte(`{"server":{"max_body_mb":8,"max_rotate":1}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if n := rotateAttempts(); n != 1 {
		t.Errorf("保存 max_rotate=1 后上游调用数=%d want 1（热应用未接线？）", n)
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
	if v, ok := m["server"]["max_rotate"]; !ok || v.(float64) != 1 {
		t.Errorf("落盘 server.max_rotate=%v want 1 (raw=%s)", v, raw)
	}
	// 调大到 2（反方向：证明不是碰巧读到了静态值 2）。
	if _, err := saveConfig([]byte(`{"server":{"max_body_mb":8,"max_rotate":2}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(2): %v", err)
	}
	if n := rotateAttempts(); n != 2 {
		t.Errorf("保存 max_rotate=2 后上游调用数=%d want 2", n)
	}
}
