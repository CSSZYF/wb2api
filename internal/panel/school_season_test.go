package panel

import (
	"encoding/json"
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

// seasonFake 假上游：复现 school_season 的真实形态——
//   - 默认口径列表里**没有** school_season（mp 限定下发）；
//   - mp 口径列表（带 X-Client-Platform: miniprogram）才有，accept_status/progress
//     由原子状态驱动；
//   - accept 只在 mp 口径生效（缺头回 task not found）；
//   - /v2/report 收到带 activityId 的事件才把进度置为达标。
type seasonFake struct {
	mu        sync.Mutex
	accepted  bool
	completed bool
	claimed   bool

	mpListHits     int32 // mp 口径列表命中数
	defListHits    int32 // 默认口径列表命中数
	acceptHits     int32 // accept 命中数
	acceptNoMPHits int32 // accept 缺 mp 头命中数
	reportHits     int32 // /v2/report 命中数
	reportNoActID  int32 // 事件缺 activityId 命中数
	claimHits      int32 // claim 命中数
	claimNoMPHits  int32 // claim 缺 mp 头命中数
}

func (f *seasonFake) snapshot() (accepted, completed, claimed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted, f.completed, f.claimed
}

func (f *seasonFake) setCompleted() {
	f.mu.Lock()
	f.completed = true
	f.mu.Unlock()
}

func (f *seasonFake) setAccepted() {
	f.mu.Lock()
	f.accepted = true
	f.mu.Unlock()
}

func (f *seasonFake) setClaimed() {
	f.mu.Lock()
	f.claimed = true
	f.mu.Unlock()
}

// seasonTaskJSON 按当前状态渲染 school_season 任务 JSON。
func (f *seasonFake) seasonTaskJSON() string {
	accepted, completed, claimed := f.snapshot()
	ast := "not_accepted"
	cur, tgt := 0, 1
	switch {
	case claimed:
		ast, cur = "claimed", 1
	case completed:
		ast, cur = "completed", 1
	case accepted:
		ast = "accepted"
	}
	return `{"task_code":"school_season","title":"校园日","accept_status":"` + ast + `",` +
		`"progress":{"current":` + itoa(cur) + `,"target":` + itoa(tgt) + `},` +
		`"reward_credit":100,"reward_energy":5}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

func (f *seasonFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isMP := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			if isMP {
				atomic.AddInt32(&f.mpListHits, 1)
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` + f.seasonTaskJSON() + `]}}`))
				return
			}
			atomic.AddInt32(&f.defListHits, 1)
			// 默认口径：只有普通任务，没有 school_season（真实形态）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"chat_5","accept_status":"not_accepted","progress":{"current":0,"target":5}}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks/accept"):
			atomic.AddInt32(&f.acceptHits, 1)
			if !isMP {
				// 缺 mp 头：上游对 mp 限定任务返回 task not found（实测）。
				atomic.AddInt32(&f.acceptNoMPHits, 1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found","data":null}`))
				return
			}
			f.setAccepted()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[
				{"task_code":"school_season","status":"accepted"}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			atomic.AddInt32(&f.reportHits, 1)
			var events []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&events)
			ok := false
			for _, ev := range events {
				if ev["activityId"] == "school_open_day_2026" {
					ok = true
				}
			}
			if !ok {
				// 无 activityId 的事件不点亮（上游实证）。
				atomic.AddInt32(&f.reportNoActID, 1)
			} else {
				f.setCompleted()
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/activity/growth/tasks/school_season/claim"):
			atomic.AddInt32(&f.claimHits, 1)
			if r.Header.Get("x-client-platform") != "miniprogram" {
				atomic.AddInt32(&f.claimNoMPHits, 1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found","data":null}`))
				return
			}
			f.setClaimed()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// newSeasonPanel 构造带假上游的面板，并把四处节流压到毫秒级（否则单测要跑十几秒）。
func newSeasonPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	oldGap, oldPoll, oldAttempts, oldSettle := acceptBatchGap, claimPollGap, claimPollAttempts, schoolSeasonSettle
	acceptBatchGap, claimPollGap, claimPollAttempts, schoolSeasonSettle = time.Millisecond, time.Millisecond, 3, time.Millisecond
	t.Cleanup(func() {
		acceptBatchGap, claimPollGap, claimPollAttempts, schoolSeasonSettle = oldGap, oldPoll, oldAttempts, oldSettle
	})
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})
}

// postTaskAuto 调「一键完成」单任务端点，返回 (code, body)。
func postTaskAuto(t *testing.T, pn *Panel, uid, code string) (int, map[string]any) {
	t.Helper()
	body := strings.NewReader(`{"task_code":"` + code + `"}`)
	req := httptest.NewRequest("POST", "/panel/api/accounts/"+uid+"/tasks/auto", body)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// TestSchoolSeasonFullFlow 校园日全链路：accept（mp 口径）→ 判据上报（带
// activityId 的 mini chat_request_send）→ 回读达标 → claim（mp 口径）→ 100 分 5 能。
//
// 锁死上游 e45f39f 的判据实证：缺 activityId 不点亮、accept/claim 缺 mp 头不认。
func TestSchoolSeasonFullFlow(t *testing.T) {
	f := &seasonFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "school_season")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if ok, _ := got["ok"].(bool); !ok {
		t.Fatalf("ok=false body=%v", got)
	}
	accepted, completed, claimed := f.snapshot()
	if !accepted {
		t.Error("未 accept（mp 口径 accept 是链路第一步）")
	}
	if !completed {
		t.Error("未点亮：带 activityId 的事件应把进度置达标")
	}
	if !claimed {
		t.Error("达标后应自动 claim（mp 口径）")
	}
	if n := atomic.LoadInt32(&f.reportNoActID); n != 0 {
		t.Errorf("有 %d 条事件缺 activityId（判据关联键，缺失即不点亮）", n)
	}
	if n := atomic.LoadInt32(&f.acceptNoMPHits); n != 0 {
		t.Errorf("有 %d 次 accept 缺 mp 头（上游会回 task not found）", n)
	}
	if n := atomic.LoadInt32(&f.claimNoMPHits); n != 0 {
		t.Errorf("有 %d 次 claim 缺 mp 头（mp 任务上游不认领奖）", n)
	}
	// 领奖结果必须透出（100 分 + 5 能）。
	if c, _ := got["credit"].(float64); c != 100 {
		t.Errorf("credit=%v want 100（校园日奖励）", got["credit"])
	}
	if e, _ := got["energy"].(float64); e != 5 {
		t.Errorf("energy=%v want 5", got["energy"])
	}
}

// TestSchoolSeasonUsesMPListForReadback 回读必须走 mp 口径：school_season 在默认
// 口径列表里不存在，用默认口径回读会恒判"该账号无此任务"——表现为任务永远不出现
// 在面板上（也点不到一键完成）。
func TestSchoolSeasonUsesMPListForReadback(t *testing.T) {
	f := &seasonFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	a := pn.cfg.Pool.AuthByUID("u1")
	tsk, err := pn.taskByCode(a, "school_season")
	if err != nil {
		t.Fatalf("taskByCode: %v", err)
	}
	if tsk == nil {
		t.Fatal("mp 口径应能查到 school_season（否则整条链路不可达）")
	}
	if tsk.TaskCode != "school_season" {
		t.Errorf("task_code=%s", tsk.TaskCode)
	}
	if n := atomic.LoadInt32(&f.mpListHits); n == 0 {
		t.Error("回读未走 mp 口径（mp 列表命中 0）")
	}
	if n := atomic.LoadInt32(&f.defListHits); n != 0 {
		t.Errorf("回读不应走默认口径（默认列表命中 %d）——该口径里没有此码", n)
	}
	// 对照：普通任务仍走默认口径（零回归）。
	if _, err := pn.taskByCode(a, "chat_5"); err != nil {
		t.Fatalf("chat_5 taskByCode: %v", err)
	}
	if n := atomic.LoadInt32(&f.defListHits); n == 0 {
		t.Error("普通任务应走默认口径（零回归）")
	}
}

// TestSchoolSeasonNotClaimWhenJudgeUnsatisfied 判据不满足（上报未点亮）时**不 claim**：
// 上游 claim 未达标会返回业务错误，白跑一次写请求。本用例让 /v2/report 静默丢弃
// 事件（模拟服务端不归账），断言零 claim 且结果为"未达标"提示而非报错。
func TestSchoolSeasonNotClaimWhenJudgeUnsatisfied(t *testing.T) {
	var claimHits int32
	var reportHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			// 恒未达标（无论上报多少次）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"school_season","accept_status":"accepted",
				 "progress":{"current":0,"target":1}}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			atomic.AddInt32(&reportHits, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/claim"):
			atomic.AddInt32(&claimHits, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":40901,"msg":"task not completed","data":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "school_season")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200（未达标不是错误）body=%v", code, got)
	}
	if n := atomic.LoadInt32(&reportHits); n == 0 {
		t.Error("应尝试上报判据事件")
	}
	if n := atomic.LoadInt32(&claimHits); n != 0 {
		t.Errorf("claim 命中 %d 次 want 0（判据不满足时不得 claim）", n)
	}
	// 未完成的表达走框架统一字段：claimable=false + progress_after 未达标
	// （动作消息只说"已上报"，完成与否由回读判定，不谎报）。
	if cl, _ := got["claimed"].(bool); cl {
		t.Error("未达标不得标 claimed")
	}
	if cl, _ := got["claimable"].(bool); cl {
		t.Error("未达标不得标 claimable")
	}
	if after, _ := got["progress_after"].(string); after != "0/1" {
		t.Errorf("progress_after=%q want 0/1（如实反映未达标）", after)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "已上报") {
		t.Errorf("message=%q 应如实说明已上报条数", msg)
	}
}

// TestSchoolSeasonClaimErrorNotFatal 领奖失败（上游业务错误）不掩盖主流程：
// 判据已点亮时 claim 失败应返回 200 + 领奖失败提示（而非整体报错），
// 与 autoAction 框架「领奖失败不掩盖主流程结果」同口径。
func TestSchoolSeasonClaimErrorNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"school_season","accept_status":"completed",
				 "progress":{"current":1,"target":1}}]}}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/claim"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":40901,"msg":"task not claimable","data":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "school_season")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200（领奖失败不该 5xx）body=%v", code, got)
	}
	msg, _ := got["message"].(string)
	if !strings.Contains(msg, "领奖失败") {
		t.Errorf("message=%q 应如实提示领奖失败", msg)
	}
}

// TestSchoolSeasonAlreadyClaimedIdempotent 已领取的校园日任务幂等跳过：
// 不重复 accept、不重复上报、不重复 claim（不浪费上游配额/写请求）。
func TestSchoolSeasonAlreadyClaimedIdempotent(t *testing.T) {
	f := &seasonFake{}
	f.setClaimed()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "school_season")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if skipped, _ := got["skipped"].(bool); !skipped {
		t.Errorf("已领取应 skipped=true，body=%v", got)
	}
	if n := atomic.LoadInt32(&f.reportHits); n != 0 {
		t.Errorf("report 命中 %d want 0（已领不再上报）", n)
	}
	if n := atomic.LoadInt32(&f.claimHits); n != 0 {
		t.Errorf("claim 命中 %d want 0（已领不再领）", n)
	}
}

// TestSchoolSeasonCompletedUnclaimedClaimsDirectly 已达标未领：直接领奖，
// 不重复上报判据事件（上游脚本同口径：completed/cur>=target 时只 claim）。
func TestSchoolSeasonCompletedUnclaimedClaimsDirectly(t *testing.T) {
	f := &seasonFake{}
	f.setCompleted()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "school_season")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if n := atomic.LoadInt32(&f.reportHits); n != 0 {
		t.Errorf("report 命中 %d want 0（已达标不重复上报）", n)
	}
	if n := atomic.LoadInt32(&f.claimHits); n == 0 {
		t.Error("已达标应直接 claim")
	}
	if _, _, claimed := f.snapshot(); !claimed {
		t.Error("应完成领取")
	}
}

// TestSchoolSeasonAcceptNotRegisteredDoesNotBlock 上游 accept 未登记（200+OK 但
// 回读仍 not_accepted）时**不阻塞**判据上报：行为事件才是进度判据（与既有
// runModelChat 口径一致）。accept 失败只记日志，链路继续走完。
func TestSchoolSeasonAcceptNotRegisteredDoesNotBlock(t *testing.T) {
	var reportHits int32
	var claimHits int32
	var mu sync.Mutex
	completed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			// accept_status 恒 not_accepted（未落账），但进度会随上报推进。
			ast, cur := "not_accepted", 0
			mu.Lock()
			if completed {
				ast, cur = "completed", 1
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"school_season","accept_status":"` + ast + `",
				 "progress":{"current":` + itoa(cur) + `,"target":1}}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks/accept"):
			// 恒回 not_accepted：复现"200+OK 但未落账"。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[
				{"task_code":"school_season","status":"not_accepted"}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			atomic.AddInt32(&reportHits, 1)
			mu.Lock()
			completed = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/claim"):
			atomic.AddInt32(&claimHits, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":100,"energy":5}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "school_season")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if n := atomic.LoadInt32(&reportHits); n == 0 {
		t.Error("accept 未登记不应阻塞判据上报（行为事件才是进度判据）")
	}
	if n := atomic.LoadInt32(&claimHits); n == 0 {
		t.Error("判据已点亮后仍应领奖")
	}
}

// TestSchoolSeasonTaskVisibleInScan 扫描必须能看到 mp 限定任务：listAllTasks 把
// 默认口径与 mp 口径并集（去重），否则面板上永远看不到「校园日」待办。
func TestSchoolSeasonTaskVisibleInScan(t *testing.T) {
	f := &seasonFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Accounts []struct {
			UID    string `json:"uid"`
			Growth []struct {
				TaskCode string `json:"task_code"`
			} `json:"growth"`
		} `json:"accounts"`
		PendingCount int `json:"pending_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	var codes []string
	for _, ac := range got.Accounts {
		for _, g := range ac.Growth {
			codes = append(codes, g.TaskCode)
		}
	}
	hasSeason, hasChat5 := false, false
	for _, c := range codes {
		if c == "school_season" {
			hasSeason = true
		}
		if c == "chat_5" {
			hasChat5 = true
		}
	}
	if !hasSeason {
		t.Errorf("扫描待办应含 school_season（mp 口径并集），实际 %v", codes)
	}
	if !hasChat5 {
		t.Errorf("扫描待办应仍含默认口径任务 chat_5（零回归），实际 %v", codes)
	}
}

// TestSchoolSeasonDedupeAcrossCalibers 两口径并集必须**去重**：同码在两口径都出现
// 时只算一条，否则队列里会出现重复条目（执行两次、白跑写请求）。
func TestSchoolSeasonDedupeAcrossCalibers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 两个口径都返回同一个 school_season（人为构造的重叠）。
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"school_season","accept_status":"accepted","progress":{"current":0,"target":1}},
			{"task_code":"chat_5","accept_status":"accepted","progress":{"current":0,"target":5}}]}}`))
	}))
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	a := pn.cfg.Pool.AuthByUID("u1")
	tasks, err := pn.listAllTasks(a)
	if err != nil {
		t.Fatalf("listAllTasks: %v", err)
	}
	seen := map[string]int{}
	for _, t := range tasks {
		seen[t.TaskCode]++
	}
	if seen["school_season"] != 1 {
		t.Errorf("school_season 出现 %d 次 want 1（并集必须去重）", seen["school_season"])
	}
	if seen["chat_5"] != 1 {
		t.Errorf("chat_5 出现 %d 次 want 1", seen["chat_5"])
	}
}

// TestSchoolSeasonMPListFailureDegrades 可选口径请求失败不致命：mp 列表报错时
// 退化为默认口径结果（该账号 mp 任务本轮不可见），不让整账号扫描标红。
func TestSchoolSeasonMPListFailureDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":500,"msg":"upstream boom","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"chat_5","accept_status":"accepted","progress":{"current":0,"target":5}}]}}`))
	}))
	defer srv.Close()
	pn := newSeasonPanel(t, srv)

	a := pn.cfg.Pool.AuthByUID("u1")
	tasks, err := pn.listAllTasks(a)
	if err != nil {
		t.Fatalf("mp 口径失败不应让整体报错（应退化）: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskCode != "chat_5" {
		t.Errorf("应退化为默认口径结果，got %+v", tasks)
	}
}

// TestAutoActionForSchoolSeason autoActionFor/autoActionIndex 必须覆盖 school_season：
// 它是「可自动化」的唯一判据（growthPending 据此入队、任务表据此显示一键完成按钮）。
func TestAutoActionForSchoolSeason(t *testing.T) {
	act := autoActionFor("school_season")
	if act == nil {
		t.Fatal("autoActionFor(school_season) 为 nil——任务不会被识别为可自动化")
	}
	if act.TaskCode != "school_season" {
		t.Errorf("TaskCode=%q", act.TaskCode)
	}
	if act.run == nil {
		t.Error("run 未接线（点一键完成会 panic）")
	}
	if act.Desc == "" {
		t.Error("Desc 为空（前端 title 提示会空白）")
	}
	if !act.MP {
		t.Error("MP 标记应为 true（小程序口径任务）")
	}
	// 排序索引必须是有限值（未知码返回 1<<20）——否则队列排序会把它当"未映射"。
	if idx := autoActionIndex("school_season"); idx >= 1<<20 {
		t.Errorf("autoActionIndex=%d want < 1<<20（应在 autoActions 表内）", idx)
	}
	// 大小写/空白容错与 autoActionFor 既有语义一致。
	if autoActionFor(" school_season ") == nil {
		t.Error("autoActionFor 应容忍首尾空白（既有语义）")
	}
}

// TestAutoActionsMPMarkerConsistent mpTaskCode 与 autoActions 的 MP 标记必须一致：
// 两者刻意分置（避免初始化环，见 mpTaskCode 注释），不一致会让口径判定分叉
// ——比如列表走 mp 但 claim 走默认，链路静默半失效。
func TestAutoActionsMPMarkerConsistent(t *testing.T) {
	for i := range autoActions {
		act := &autoActions[i]
		if got, want := mpTaskCode(act.TaskCode), act.MP; got != want {
			t.Errorf("%s: mpTaskCode=%v 但 autoAction.MP=%v（两处必须一致）", act.TaskCode, got, want)
		}
	}
	// 反向：mpTaskCode 认的码必须真的在 autoActions 表里（否则永不可达）。
	if !mpTaskCode("school_season") {
		t.Fatal("mpTaskCode(school_season) 应为 true")
	}
	if autoActionFor("school_season") == nil {
		t.Error("mpTaskCode 认的码必须同时在 autoActions 里（否则无动作可跑）")
	}
	if mpTaskCode("chat_5") {
		t.Error("普通任务不应被判成 mp 口径")
	}
}

// TestGrowthPendingIncludesSchoolSeason growthPending 必须把 school_season 判为待办
// （未完成 + 可自动化）——它是队列/扫描的入队判据。
//
// 注意「达标未领」的口径：growthPending 现有实现**排除**达标未领（见下），
// 与其行内注释（"达标未领：也入队"）不符——那是既有行为，本批次**刻意不改**
// （改它会让 expert_5 等真实对话类任务在"达标未领"时被重复执行，代价远大于收益）。
// 达标未领的领取路径由任务表的「领取」按钮覆盖（accountTaskAuto 会走
// claimRewardFor → mp 口径 claim），队列路径不是唯一出口。
func TestGrowthPendingIncludesSchoolSeason(t *testing.T) {
	pending := upstream.Task{TaskCode: "school_season", Target: 1, Current: 0}
	if !growthPending(pending) {
		t.Error("未完成的 school_season 应判为待办")
	}
	// 达标未领：按现有实现不入队（见上方注释；领取走任务表按钮）。
	done := upstream.Task{TaskCode: "school_season", Target: 1, Current: 1}
	if growthPending(done) {
		t.Error("达标未领按现有实现不入队——行为变了说明 growthPending 语义被改动，" +
			"需同时评估 expert_5 等真实对话类任务的重复执行风险")
	}
	// 已领：不入队。
	claimed := upstream.Task{TaskCode: "school_season", Target: 1, Current: 1, Claimed: true}
	if growthPending(claimed) {
		t.Error("已领取的 school_season 不应入队")
	}
}

// TestAcceptSplitRoutesMPCodeToMPCaliber 三处 accept 入口共用的拆分必须把 mp 码
// 路由到 mp 口径：混进默认批会让上游回 task not found（整批可能失败）。
func TestAcceptSplitRoutesMPCodeToMPCaliber(t *testing.T) {
	var mu sync.Mutex
	mpAccepted := map[string]bool{}
	defAccepted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isMP := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var results []string
			mu.Lock()
			for _, c := range body.TaskCodes {
				if isMP {
					mpAccepted[c] = true
				} else {
					defAccepted[c] = true
				}
				results = append(results, `{"task_code":"`+c+`","status":"accepted"}`)
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[` + strings.Join(results, ",") + `]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			// 回读：按口径分别作答（两个码都判 accepted）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"school_season","accept_status":"accepted"},
				{"task_code":"chat_5","accept_status":"accepted"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pn := newSeasonPanel(t, srv)
	a := pn.cfg.Pool.AuthByUID("u1")

	tasks := []upstream.Task{
		{TaskCode: "school_season", AcceptStatus: "not_accepted"},
		{TaskCode: "chat_5", AcceptStatus: "not_accepted"},
	}
	accepted, failed := pn.acceptSplit(a, tasks)
	if len(failed) != 0 {
		t.Errorf("failed=%v want 空", failed)
	}
	if len(accepted) != 2 {
		t.Fatalf("accepted=%v want 2 个", accepted)
	}
	mu.Lock()
	defer mu.Unlock()
	if !mpAccepted["school_season"] {
		t.Error("school_season 未走 mp 口径 accept（混进默认批会被上游拒）")
	}
	if defAccepted["school_season"] {
		t.Error("school_season 不应出现在默认口径批里")
	}
	if !defAccepted["chat_5"] {
		t.Error("chat_5 应走默认口径（零回归）")
	}
	if mpAccepted["chat_5"] {
		t.Error("chat_5 不应被带进 mp 口径批（其 mp 口径 accept 语义未经验证）")
	}
}

// TestSplitAcceptCodesSkipsDone 拆分必须跳过已接受/已领/达标/locked 态
// （避免重复 accept 写请求）。
//
// 注意：跳过判据用的是 Task.Claimed **布尔**（由 ListTasks 从 accept_status
// 派生，见 upstream.Task），不是 AcceptStatus 字符串——构造夹具时必须两者一致，
// 否则测的不是生产形态。
func TestSplitAcceptCodesSkipsDone(t *testing.T) {
	codes, mpCodes := splitAcceptCodes([]upstream.Task{
		{TaskCode: "school_season", AcceptStatus: "accepted"},
		{TaskCode: "school_season", AcceptStatus: "claimed", Claimed: true},
		{TaskCode: "chat_5", AcceptStatus: "completed"},
		{TaskCode: "chat_5", Locked: true},
		{TaskCode: "chat_5", Claimed: true},
		{TaskCode: "chat_5", AcceptStatus: "not_accepted"},
		{TaskCode: "school_season", AcceptStatus: "not_accepted"},
	})
	if len(codes) != 1 || codes[0] != "chat_5" {
		t.Errorf("codes=%v want [chat_5]", codes)
	}
	if len(mpCodes) != 1 || mpCodes[0] != "school_season" {
		t.Errorf("mpCodes=%v want [school_season]", mpCodes)
	}
}

// TestSchoolSeasonFrontendWiring 前端接线必须齐全（app.js 是 go:embed 静态资源，
// Go 编译器不校验其内容——少一处映射，面板上就是"看不到这条任务"或"点了没反应"）。
func TestSchoolSeasonFrontendWiring(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()
	for _, want := range []string{
		`'school_season':`, // AUTO_TASKS 条目（任务表「一键完成」按钮的前提）
		"MP_TASKS",         // 小程序口径标记表
		"mpTag",            // 两处渲染的插值变量
		"小程序",              // tag 文案（index.html 无此串，只在 app.js）
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少校园日接线：%s", want)
		}
	}
	// tag 必须在两个渲染函数里都插值（任务表 + 队列行）——只加一处会漏另一半。
	for _, fn := range []string{"function loadTasks(", "function qrowHTML("} {
		body := jsFuncBody(js, fn)
		if body == "" {
			t.Fatalf("app.js 缺少函数 %s", fn)
		}
		if !strings.Contains(body, "MP_TASKS") {
			t.Errorf("%s 未引用 MP_TASKS（小程序任务在该视图里无标记）", fn)
		}
	}
}
