package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// usagePanelFixture 造一个挂了用量记录器的面板（纯内存记录器，不落盘）。
func usagePanelFixture(t *testing.T) (*Panel, *usage.Recorder) {
	t.Helper()
	rec := usage.New("")
	return New(Config{
		Version: "test",
		Pool:    pool.New(""),
		Usage:   rec,
	}), rec
}

// getUsage 打一次 /panel/api/usage 并解析响应。
func getUsage(t *testing.T, p *Panel, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/usage"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /panel/api/usage%s code=%d body=%s", query, rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body)
	}
	return body
}

// 面板用量端点：窗口参数必须**端到端**生效，且响应里的 hours/granularity 是实际
// 生效值。
//
// 现象 A 的现场是「切换范围后顶部六个数字与两张表纹丝不动」，所以这里直接断言
// HTTP 响应里的 totals 与三张表都只含窗口内那一份；hours/granularity 则是给前端
// 显示「窗口：近 N 天」与选横轴格式用的——没有这两个字段，用户无从判断窗口是否
// 真的生效（可见性补偿）。
func TestPanelUsageWindowEndToEnd(t *testing.T) {
	p, rec := usagePanelFixture(t)
	now := time.Now()
	// 窗口内：cn / uid-in / model-in，数值小。
	rec.Add(now, "cn", "uid-in", "model-in", usage.Delta{
		PromptTokens: 10, HasPromptTokens: true, LatencyMs: 100, HasLatency: true,
	}, true)
	// 窗口外（48 小时前）：另一个域/账号/模型，数值大——混进来一眼可辨。
	rec.Add(now.Add(-48*time.Hour), "global", "uid-out", "model-out", usage.Delta{
		PromptTokens: 9999, HasPromptTokens: true, LatencyMs: 9000, HasLatency: true,
	}, true)

	body := getUsage(t, p, "?hours=24")
	if got := body["totals"].(map[string]any)["prompt_tokens"]; got != float64(10) {
		t.Errorf("hours=24 totals.prompt=%v want 10（窗口必须作用到汇总）", got)
	}
	if body["hours"] != float64(24) || body["granularity"] != "hour" {
		t.Errorf("hours=%v granularity=%v want 24/hour", body["hours"], body["granularity"])
	}
	for _, k := range []string{"by_account", "by_model", "by_realm"} {
		rows, ok := body[k].([]any)
		if !ok {
			t.Fatalf("%s 不是数组：%T", k, body[k])
		}
		if len(rows) != 1 {
			t.Errorf("%s = %d 行 want 1（窗口外的行必须消失）: %v", k, len(rows), rows)
		}
	}

	// 30 天：越过小时粒度阈值 → 整条序列改用日点；两个桶都回到窗口内。
	body = getUsage(t, p, "?hours=720")
	if got := body["totals"].(map[string]any)["prompt_tokens"]; got != float64(10009) {
		t.Errorf("hours=720 totals.prompt=%v want 10009", got)
	}
	if body["hours"] != float64(720) || body["granularity"] != "day" {
		t.Errorf("hours=%v granularity=%v want 720/day", body["hours"], body["granularity"])
	}
	series := body["series"].([]any)
	if len(series) == 0 {
		t.Fatal("hours=720 series 为空（48 小时内的两个桶都该在）")
	}
	for _, raw := range series {
		if s := raw.(map[string]any)["scope"]; s != "day" {
			t.Errorf("hours=720 出现 scope=%v 的点（一条序列只能有一种粒度）", s)
		}
	}
}

// 非法/缺失的 hours 一律回退缺省窗口（72 = 近 3 天），且响应里的 hours 必须是
// 回退后的**实际生效值**——前端据此显示窗口文案，不能让用户以为 999999 生效了。
//
// 面板下拉框只会送 24/72/168/720，但 URL 是手改得到的：非法值必须是「回退 + 如实
// 报告」，不是「静默钳制」（钳制会让 hours=999999 这类笔误得到一个看似生效的结果，
// 而这正是现象 A 那种「切了没反应」的观感来源）。
func TestPanelUsageInvalidHoursReportsEffectiveWindow(t *testing.T) {
	p, rec := usagePanelFixture(t)
	now := time.Now()
	// 24 小时内一个桶、48 小时前一个桶：缺省窗口（72h）两个都该在。
	rec.Add(now, "cn", "u1", "m1", usage.Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	rec.Add(now.Add(-48*time.Hour), "cn", "u1", "m1", usage.Delta{PromptTokens: 20, HasPromptTokens: true}, true)

	base := getUsage(t, p, "")
	basePT := base["totals"].(map[string]any)["prompt_tokens"]
	if basePT != float64(30) {
		t.Fatalf("缺省窗口 prompt=%v want 30（用例前提不成立）", basePT)
	}
	for _, q := range []string{
		"", "?hours=72", "?hours=abc", "?hours=", "?hours=0", "?hours=-5",
		"?hours=999999999", "?hours=1.5", "?hours=+72x",
	} {
		body := getUsage(t, p, q)
		if body["hours"] != float64(72) {
			t.Errorf("query %q 的 hours=%v want 72（非法值回退缺省窗口）", q, body["hours"])
		}
		if body["granularity"] != "hour" {
			t.Errorf("query %q 的 granularity=%v want hour（回退到 72 后的粒度）", q, body["granularity"])
		}
		// 回退不是「换个窗口算」，而是真的按 72 小时算：数值必须与显式 72 一致。
		if got := body["totals"].(map[string]any)["prompt_tokens"]; got != basePT {
			t.Errorf("query %q 的 totals.prompt=%v 与缺省 %v 不一致", q, got, basePT)
		}
	}
	// 上限本身（1440 = 60 天）是合法窗口，不得被当成非法值回退。
	if body := getUsage(t, p, "?hours=1440"); body["hours"] != float64(1440) {
		t.Errorf("hours=1440（上限）应原样生效，得到 hours=%v", body["hours"])
	}
}
