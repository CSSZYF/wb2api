package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoAllFakeTask 假上游里单个任务的状态（列表 JSON 由它渲染）。
type autoAllFakeTask struct {
	accept   string // accept_status：not_accepted / accepted / completed / claimed
	cur, tgt int64  // progress
	credit   int64  // reward_credit
	energy   int64  // reward_energy
	failMsg  string // 非空：claim 回 HTTP 502 + 该原因（模拟领奖失败）
}

// render 按当前状态渲染列表里的任务 JSON（口径同上游：progress 对象）。
func (t autoAllFakeTask) render(code string) string {
	return fmt.Sprintf(`{"task_code":%q,"title":"用例任务","accept_status":%q,`+
		`"progress":{"current":%d,"target":%d},"reward_credit":%d,"reward_energy":%d}`,
		code, t.accept, t.cur, t.tgt, t.credit, t.energy)
}

// autoAllFake 假上游：一张 task_code → 任务状态表驱动列表 / accept / claim，
// 复现「一键完成全部」遇到「达标未领 / 已领过 / 未达标 / 达标但暂不可领」四类任务
// 时的行为。领奖成功后把该项翻成 accept_status=claimed（真实上游口径），幂等用例据此
// 断言第二轮不会重复领。
type autoAllFake struct {
	mu    sync.Mutex
	tasks map[string]autoAllFakeTask
	// claims 每个 code 的 claim 命中次数（**所有** code 都记，误领会体现在这里）。
	claims map[string]int
	// reports /v2/report 命中数（未达标用例的动作判据）。
	reports int32
}

func (f *autoAllFake) set(code string, t autoAllFakeTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tasks == nil {
		f.tasks = map[string]autoAllFakeTask{}
	}
	f.tasks[code] = t
}

// claimCount code 被领奖的次数（含失败尝试）。
func (f *autoAllFake) claimCount(code string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[code]
}

// listJSON 渲染当前任务列表（保证输出顺序稳定）。
func (f *autoAllFake) listJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := make([]string, 0, len(f.tasks))
	for _, code := range sortedKeys(f.tasks) {
		parts = append(parts, f.tasks[code].render(code))
	}
	return `{"code":0,"msg":"OK","data":{"tasks":[` + strings.Join(parts, ",") + `]}}`
}

// sortedKeys map 键排序（列表输出稳定，避免用例间抖动）。
func sortedKeys(m map[string]autoAllFakeTask) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ { // 插入排序：键数量极少，省一个 import
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func (f *autoAllFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/activity/growth/tasks"):
			_, _ = w.Write([]byte(f.listJSON()))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			var body struct {
				Codes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			parts := make([]string, 0, len(body.Codes))
			for _, code := range body.Codes {
				f.mu.Lock()
				if t, ok := f.tasks[code]; ok {
					t.accept = "accepted"
					f.tasks[code] = t
				}
				f.mu.Unlock()
				parts = append(parts, `{"task_code":"`+code+`","status":"accepted"}`)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[` + strings.Join(parts, ",") + `]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			code := claimCodeFromPath(r.URL.Path)
			f.mu.Lock()
			if f.claims == nil {
				f.claims = map[string]int{}
			}
			f.claims[code]++
			t, known := f.tasks[code]
			f.mu.Unlock()
			credit, energy := int64(100), int64(5)
			if known {
				credit, energy = t.credit, t.energy
			}
			if known && t.failMsg != "" {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"code":502,"msg":"` + t.failMsg + `","data":null}`))
				return
			}
			if known { // 领奖成功：上游把该项翻成已领（幂等用例依赖此翻转）
				f.mu.Lock()
				t.accept = "claimed"
				f.tasks[code] = t
				f.mu.Unlock()
			}
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":%d,"energy":%d}}`,
				credit, energy)))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			atomic.AddInt32(&f.reports, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// claimCodeFromPath 从 .../tasks/{code}/claim 取任务码。
func claimCodeFromPath(p string) string {
	parts := strings.Split(strings.TrimSuffix(p, "/claim"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

// newAutoAllPanel 构造带假上游的面板，并把四处节流压到毫秒级
// （否则单项动作与有界轮询要白等数秒）。
func newAutoAllPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	oldGap, oldPoll, oldAttempts, oldAccept := reportGap, claimPollGap, claimPollAttempts, acceptBatchGap
	reportGap, claimPollGap, claimPollAttempts, acceptBatchGap = time.Millisecond, time.Millisecond, 2, time.Millisecond
	t.Cleanup(func() {
		reportGap, claimPollGap, claimPollAttempts, acceptBatchGap = oldGap, oldPoll, oldAttempts, oldAccept
	})
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})
}

// runAutoAllRound 走 HTTP 端点跑一轮「一键完成全部」，把结果按 task_code 索引。
func runAutoAllRound(t *testing.T, pn *Panel) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/tasks/auto_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto_all code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body.String())
	}
	byCode := make(map[string]map[string]any, len(got.Results))
	for _, it := range got.Results {
		if code, _ := it["task_code"].(string); code != "" {
			byCode[code] = it
		}
	}
	return byCode
}

// reachedButUnclaimed chat_5「已达标但没领奖」的形态（单任务路径领奖失败后留下的状态）。
func reachedButUnclaimed() autoAllFakeTask {
	return autoAllFakeTask{accept: "accepted", cur: 5, tgt: 5, credit: 100, energy: 5}
}

// TestAutoAllSettlesReachedTaskBeforeSkip 核心回归：**已达标但没领奖**的任务，下一轮
// 「一键完成全部」必须补领一次，而不是被静默跳过。
//
// 触发链路（bug）：单任务路径领奖失败时记 done + claim_error（提示"可在列表手动重试"）；
// 下一轮批量跑到该项时 Current>=Target 成立 → 直接 skipped，`continue` 让下方的
// claimRewardFor 分支永远不可达 → 补领永远不会发生，积分只能靠用户手动点「领取」。
func TestAutoAllSettlesReachedTaskBeforeSkip(t *testing.T) {
	f := &autoAllFake{}
	f.set("chat_5", reachedButUnclaimed())
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newAutoAllPanel(t, srv)

	it := runAutoAllRound(t, pn)["chat_5"]
	if it == nil {
		t.Fatal("结果里没有 chat_5（批量结果必须逐项返回）")
	}
	if n := f.claimCount("chat_5"); n != 1 {
		t.Fatalf("claim 命中 %d 次 want 1——达标未领必须补一次结算（这是静默丢分的根因）", n)
	}
	if st, _ := it["status"].(string); st != "done" {
		t.Errorf("status=%v want done（要反映「已达标并补领」；skipped 语义是「本轮无事可做」）", it["status"])
	}
	if claimed, _ := it["claimed"].(bool); !claimed {
		t.Errorf("claimed=%v want true（前端据此提示奖励已到账）", it["claimed"])
	}
	if c, _ := it["credit"].(float64); c != 100 {
		t.Errorf("credit=%v want 100（补领到的积分必须透出）", it["credit"])
	}
	if e, _ := it["energy"].(float64); e != 5 {
		t.Errorf("energy=%v want 5", it["energy"])
	}
	msg, _ := it["message"].(string)
	if !strings.Contains(msg, "+100 分") || !strings.Contains(msg, "+5 能") {
		t.Errorf("message=%q 应带上补领的 +分/+能（照单任务路径文案口径）", msg)
	}
}

// TestAutoAllClaimedTaskStaysSkipped 已领过（Claimed==true）的项保持原短路语义：
// status=skipped、message 口径不变，且**不再**发起领奖调用。
func TestAutoAllClaimedTaskStaysSkipped(t *testing.T) {
	f := &autoAllFake{}
	f.set("chat_5", autoAllFakeTask{accept: "claimed", cur: 5, tgt: 5, credit: 100, energy: 5})
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newAutoAllPanel(t, srv)

	it := runAutoAllRound(t, pn)["chat_5"]
	if it == nil {
		t.Fatal("结果里没有 chat_5")
	}
	if n := f.claimCount("chat_5"); n != 0 {
		t.Errorf("已领过的项不应再调 claimRewardFor（命中 %d 次）", n)
	}
	if st, _ := it["status"].(string); st != "skipped" {
		t.Errorf("status=%v want skipped（原短路语义保持不变）", it["status"])
	}
	if msg, _ := it["message"].(string); msg != "已完成（5/5）" {
		t.Errorf("message=%q want 原口径「已完成（5/5）」", msg)
	}
}

// TestAutoAllNotReachedRunsActionUnchanged 未达标的项走原执行路径（accept → 动作 → 回读），
// 行为不变：动作真的执行、未达标时不领奖、无 claim_error。
func TestAutoAllNotReachedRunsActionUnchanged(t *testing.T) {
	f := &autoAllFake{}
	f.set("chat_5", autoAllFakeTask{accept: "not_accepted", cur: 0, tgt: 5, credit: 100, energy: 5})
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newAutoAllPanel(t, srv)

	it := runAutoAllRound(t, pn)["chat_5"]
	if it == nil {
		t.Fatal("结果里没有 chat_5")
	}
	if st, _ := it["status"].(string); st != "done" {
		t.Fatalf("status=%v want done body=%v", it["status"], it)
	}
	if msg, _ := it["message"].(string); msg != "已补报 5 条对话事件" {
		t.Errorf("message=%q want runChat5 原文（未达标路径行为不变）", msg)
	}
	if n := atomic.LoadInt32(&f.reports); n != 5 {
		t.Errorf("/v2/report 命中 %d 次 want 5（动作按差额补报，未达标才执行）", n)
	}
	if n := f.claimCount("chat_5"); n != 0 {
		t.Errorf("未达标不应领奖，实际 claim %d 次", n)
	}
	if _, ok := it["claim_error"]; ok {
		t.Errorf("未达标不该有 claim_error: %v", it["claim_error"])
	}
	if claimable, _ := it["claimable"].(bool); claimable {
		t.Errorf("claimable=%v want false（回读仍是 0/5）", it["claimable"])
	}
}

// TestAutoAllClaimFailureReported 补领失败不静默吞掉：带失败原因，status/字段口径与
// 单任务路径的领奖失败分支一致（done + claim_error）。
func TestAutoAllClaimFailureReported(t *testing.T) {
	ft := reachedButUnclaimed()
	ft.failMsg = "task not completed"
	f := &autoAllFake{}
	f.set("chat_5", ft)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newAutoAllPanel(t, srv)

	it := runAutoAllRound(t, pn)["chat_5"]
	if it == nil {
		t.Fatal("结果里没有 chat_5")
	}
	if n := f.claimCount("chat_5"); n != 1 {
		t.Fatalf("应尝试补领一次，实际 claim %d 次", n)
	}
	ce, _ := it["claim_error"].(string)
	if ce == "" {
		t.Fatalf("补领失败必须带 claim_error（不得静默吞掉）: %v", it)
	}
	msg, _ := it["message"].(string)
	if !strings.Contains(msg, ce) {
		t.Errorf("message=%q 应带上失败原因 %q", msg, ce)
	}
	if st, _ := it["status"].(string); st != "done" {
		t.Errorf("status=%v want done（与单任务路径 claim_error 同口径；skipped 会被前端当成"+
			"「无事可做」而把失败藏起来）", it["status"])
	}
}

// TestAutoAllBackfillIdempotent 幂等：补领成功后上游把该项翻成已领，第二轮批量因
// Claimed 短路而跳过，不会重复领。
func TestAutoAllBackfillIdempotent(t *testing.T) {
	f := &autoAllFake{}
	f.set("chat_5", reachedButUnclaimed())
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newAutoAllPanel(t, srv)

	first := runAutoAllRound(t, pn)["chat_5"]
	if claimed, _ := first["claimed"].(bool); !claimed {
		t.Fatalf("首轮应补领成功: %v", first)
	}
	second := runAutoAllRound(t, pn)["chat_5"]
	if second == nil {
		t.Fatal("第二轮结果里没有 chat_5")
	}
	if st, _ := second["status"].(string); st != "skipped" {
		t.Errorf("第二轮 status=%v want skipped（已领过 → 原短路语义）", second["status"])
	}
	if msg, _ := second["message"].(string); msg != "已完成（5/5）" {
		t.Errorf("第二轮 message=%q want 原口径「已完成（5/5）」", msg)
	}
	if n := f.claimCount("chat_5"); n != 1 {
		t.Errorf("两轮共 claim %d 次 want 1（幂等：不重复领奖）", n)
	}
}
