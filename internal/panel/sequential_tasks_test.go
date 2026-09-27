package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// seqFake 假上游：复现 Sequential 族 mp 链的真实形态——
//   - 默认口径列表里**没有**任何 Sequential_* 码（mp 限定下发）；
//   - mp 口径列表按 state 渲染当前环（Sequential_Tasks_1..7）；
//   - accept 只在 mp 口径生效；判据上报走 /v2/report，按事件形状点亮对应环；
//   - claim 走 mp 口径（chat 域），缺头回 task not found。
//
// state: 0=未接受，1=已接受，2=已达标，3=已领取。
type seqFake struct {
	mu     sync.Mutex
	states map[string]int
	target map[string]int

	reportEvents []map[string]any // 收到的全部上报事件（判据形状断言用）
	acceptHits   int32
	claimHits    int32
	claimNoMP    int32
	acceptNoMP   int32
}

func newSeqFake() *seqFake {
	f := &seqFake{states: map[string]int{}, target: map[string]int{
		"Sequential_Tasks_1": 1, "Sequential_Tasks_2": 1, "Sequential_Tasks_3": 5,
		"Sequential_Tasks_4": 1, "Sequential_Tasks_5": 1, "Sequential_Tasks_6": 10,
		"Sequential_Tasks_7": 1,
	}}
	for _, c := range []string{"Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
		"Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6", "Sequential_Tasks_7"} {
		f.states[c] = 0
	}
	return f
}

func (f *seqFake) state(code string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[code]
}

func (f *seqFake) setState(code string, st int) {
	f.mu.Lock()
	f.states[code] = st
	f.mu.Unlock()
}

// seqTaskJSON 渲染单个 Sequential 任务（按 state）。
func (f *seqFake) seqTaskJSON(code string) string {
	st := f.state(code)
	ast := "not_accepted"
	cur, tgt := 0, f.target[code]
	switch st {
	case 1:
		ast = "accepted"
	case 2:
		ast, cur = "completed", tgt
	case 3:
		ast, cur = "claimed", tgt
	}
	return `{"task_code":"` + code + `","title":"` + code + `","accept_status":"` + ast + `",` +
		`"progress":{"current":` + itoaN(cur) + `,"target":` + itoaN(tgt) + `},` +
		`"reward_credit":200,"reward_energy":5}`
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return "10"
}

func (f *seqFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isMP := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			if !isMP {
				// 默认口径：无任何 Sequential 码（真实形态）。
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
					{"task_code":"chat_5","accept_status":"not_accepted","progress":{"current":0,"target":5}}]}}`))
				return
			}
			var parts []string
			for _, c := range []string{"Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
				"Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6", "Sequential_Tasks_7"} {
				parts = append(parts, f.seqTaskJSON(c))
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` + strings.Join(parts, ",") + `]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks/accept"):
			atomic.AddInt32(&f.acceptHits, 1)
			if !isMP {
				atomic.AddInt32(&f.acceptNoMP, 1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found","data":null}`))
				return
			}
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var results []string
			for _, c := range body.TaskCodes {
				if f.state(c) == 0 {
					f.setState(c, 1)
				}
				results = append(results, `{"task_code":"`+c+`","status":"accepted"}`)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[` + strings.Join(results, ",") + `]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/market/expert/list"):
			// 专家市场：Sequential_Tasks_2 的判据载体前置（真实 ex_ id）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[
				{"expert_id":"ex_test01","expert_type":"agent","display_name_zh":"论文写作导师",
				 "profession_zh":"学术写作","version":"1.0.2"}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			var events []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&events)
			f.mu.Lock()
			f.reportEvents = append(f.reportEvents, events...)
			f.mu.Unlock()
			// 按事件形状点亮：本 fake 不区分环，任何判据事件都推进 Tasks_2（供形状断言）。
			for _, ev := range events {
				if ev["eventCode"] == "expert_actual_use" {
					f.setState("Sequential_Tasks_2", 2)
				}
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/claim"):
			atomic.AddInt32(&f.claimHits, 1)
			if r.Header.Get("x-client-platform") != "miniprogram" {
				atomic.AddInt32(&f.claimNoMP, 1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found","data":null}`))
				return
			}
			// 路径形如 .../tasks/<code>/claim —— 取倒数第二段。
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
			code := parts[len(parts)-2]
			f.setState(code, 3)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":200,"energy":5}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newSeqPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	oldGap, oldPoll, oldAttempts, oldSettle, oldMpGap := acceptBatchGap, claimPollGap, claimPollAttempts, schoolSeasonSettle, mpActionGap
	acceptBatchGap, claimPollGap, claimPollAttempts, schoolSeasonSettle, mpActionGap = time.Millisecond, time.Millisecond, 3, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		acceptBatchGap, claimPollGap, claimPollAttempts, schoolSeasonSettle, mpActionGap = oldGap, oldPoll, oldAttempts, oldSettle, oldMpGap
	})
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})
}

// TestSequentialTasksRegistered 吸收上游 4f08798 + 341f919：Sequential_Tasks_1..7
// 全部登记为 mp 口径可自动化任务（缺一个就是「面板上看得见、点不了」）。
func TestSequentialTasksRegistered(t *testing.T) {
	for i := 1; i <= 7; i++ {
		code := "Sequential_Tasks_" + itoaN(i)
		act := autoActionFor(code)
		if act == nil {
			t.Errorf("%s 未登记 autoActions（不可自动化）", code)
			continue
		}
		if act.run == nil {
			t.Errorf("%s.run 未接线（点一键完成会 panic）", code)
		}
		if act.Desc == "" {
			t.Errorf("%s.Desc 为空（前端 title 会空白）", code)
		}
		if !act.MP {
			t.Errorf("%s.MP 应为 true（mp 口径限定下发）", code)
		}
		if !mpTaskCode(code) {
			t.Errorf("mpTaskCode(%s) 应为 true（否则回读/accept/claim 走错口径）", code)
		}
		if idx := autoActionIndex(code); idx >= 1<<20 {
			t.Errorf("autoActionIndex(%s)=%d（应在表内）", code, idx)
		}
	}
	// Tasks_8 是链条封顶（上游实测 not found），不得登记。
	if autoActionFor("Sequential_Tasks_8") != nil {
		t.Error("Sequential_Tasks_8 不该登记（上游 not found 封顶）")
	}
}

// TestSequentialTask2ExpertUseFlow Sequential_Tasks_2 全链路：mp 口径查任务 →
// 市场真实专家 id → accept → expert_actual_use 上报（mp 指纹）→ 回读达标 → claim。
func TestSequentialTask2ExpertUseFlow(t *testing.T) {
	f := newSeqFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "Sequential_Tasks_2")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if f.state("Sequential_Tasks_2") != 3 {
		t.Errorf("state=%d want 3（accept→上报→达标→claim 全链路）", f.state("Sequential_Tasks_2"))
	}
	if n := atomic.LoadInt32(&f.acceptNoMP); n != 0 {
		t.Errorf("有 %d 次 accept 缺 mp 头", n)
	}
	if n := atomic.LoadInt32(&f.claimNoMP); n != 0 {
		t.Errorf("有 %d 次 claim 缺 mp 头", n)
	}
	// 判据形状：必须上报 expert_actual_use（带专家 id、不带 activityId）。
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, ev := range f.reportEvents {
		if ev["eventCode"] != "expert_actual_use" {
			continue
		}
		found = true
		if _, ok := ev["activityId"]; ok {
			t.Errorf("Sequential_Tasks_2 不得带 activityId（那是 school_season 的关联键）: %+v", ev)
		}
		if id, _ := ev["id"].(string); !strings.HasPrefix(id, "ex_") {
			t.Errorf("专家 id 必须是市场真实 ex_ id（空/编造服务端不入账）: %q", id)
		}
	}
	if !found {
		t.Error("未上报 expert_actual_use（判据事件）")
	}
}

// TestSequentialTasks2SkipsWhenMarketUnavailable 专家市场不可用时**整任务不动作**
// （含不 accept）：否则会留下「已登记未上报」的半程态，下轮又得从头补。
func TestSequentialTasks2SkipsWhenMarketUnavailable(t *testing.T) {
	f := newSeqFake()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "market/expert/list") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
			return
		}
		f.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	code, got := postTaskAuto(t, pn, "u1", "Sequential_Tasks_2")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200（市场不可用是软失败，不是 HTTP 错误）body=%v", code, got)
	}
	if st := f.state("Sequential_Tasks_2"); st != 0 {
		t.Errorf("state=%d want 0（市场不可用应整任务不动作，避免半程态）", st)
	}
	if n := atomic.LoadInt32(&f.acceptHits); n != 0 {
		t.Errorf("accept 命中 %d 次（判据载体不可得时不得先 accept）", n)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "专家市场") {
		t.Errorf("message=%q 应说明专家市场不可用", msg)
	}
}

// TestSequentialTask1UsesMiniChatWithoutActivityId Sequential_Tasks_1「小程序首
// 对话」判据 = 裸 mini chat_request_send（**不带** activityId，服务端按
// source=mini_program 指纹关联）。
func TestSequentialTask1UsesMiniChatWithoutActivityId(t *testing.T) {
	f := newSeqFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	// 该 fake 的 report 分支只认 expert_actual_use，故 Tasks_1 不会点亮——
	// 这里只断言**事件形状**（判据正确性由上游 task_runner 实测背书）。
	if _, got := postTaskAuto(t, pn, "u1", "Sequential_Tasks_1"); got == nil {
		t.Fatal("端点未返回 JSON")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reportEvents) == 0 {
		t.Fatal("未上报任何事件")
	}
	ev := f.reportEvents[0]
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v want chat_request_send", ev["eventCode"])
	}
	if _, ok := ev["activityId"]; ok {
		t.Errorf("Sequential_Tasks_1 不得带 activityId: %+v", ev)
	}
	if _, ok := ev["requestModelId"]; ok {
		t.Errorf("Tasks_1 是裸对话事件，不带模型字段（那是 Tasks_5）: %+v", ev)
	}
}

// TestSequentialTask3ReportsByTargetDelta Sequential_Tasks_3「5 次对话」按差额补报：
// target=5 时上报条数 = 5 - current（判据与 Tasks_1 同形状，仅计数）。
func TestSequentialTask3ReportsByTargetDelta(t *testing.T) {
	f := newSeqFake()
	f.setState("Sequential_Tasks_3", 1) // 已接受，进度 0/5
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	postTaskAuto(t, pn, "u1", "Sequential_Tasks_3")
	f.mu.Lock()
	n := len(f.reportEvents)
	f.mu.Unlock()
	if n != 5 {
		t.Errorf("上报条数=%d want 5（按 target 差额补足）", n)
	}
}

// TestSequentialTask5CarriesModelFields Sequential_Tasks_5「使用 GLM5.2」判据 =
// 带 requestModelId/requestModelName 的 mini 对话事件。
func TestSequentialTask5CarriesModelFields(t *testing.T) {
	f := newSeqFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	postTaskAuto(t, pn, "u1", "Sequential_Tasks_5")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reportEvents) == 0 {
		t.Fatal("未上报任何事件")
	}
	ev := f.reportEvents[0]
	if ev["requestModelId"] != "glm-5.2" {
		t.Errorf("requestModelId=%v want glm-5.2（模型任务必须带模型字段）", ev["requestModelId"])
	}
	if ev["requestModelName"] == nil || ev["requestModelName"] == "" {
		t.Errorf("requestModelName 缺失: %+v", ev)
	}
}

// TestSequentialReservedTasksSoftFail 预留任务（Tasks_4/6/7）在判据尚未验证时
// **必须软失败**：任务在 mp 列表里（前置已完成，上游已下发）但判据不点亮时，
// 返回 200 + 可读说明、不 panic、不误报成功——这正是「每日零点解锁一环」的
// 常态（今天这一环的判据形态可能还没实测校正）。
func TestSequentialReservedTasksSoftFail(t *testing.T) {
	f := newSeqFake()
	// 该 fake 的 report 分支只认 expert_actual_use，故 Tasks_4/6/7 上报后不会点亮。
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	for _, code := range []string{"Sequential_Tasks_4", "Sequential_Tasks_6", "Sequential_Tasks_7"} {
		httpCode, got := postTaskAuto(t, pn, "u1", code)
		if httpCode != http.StatusOK {
			t.Errorf("%s: code=%d want 200（判据未点亮是软失败，不是 HTTP 错误）body=%v", code, httpCode, got)
			continue
		}
		if ok, _ := got["ok"].(bool); !ok {
			t.Errorf("%s: ok 应为 true（软失败仍算本轮处理完）: %v", code, got)
		}
		if msg, _ := got["message"].(string); msg == "" {
			t.Errorf("%s: 必须有可读说明（面板 toast 用它）", code)
		}
	}
	// Tasks_4/6/7 的 accept 必须已登记（前置动作照做，只是判据未点亮）。
	if f.state("Sequential_Tasks_4") != 1 {
		t.Errorf("Tasks_4 state=%d want 1（accept 应已登记）", f.state("Sequential_Tasks_4"))
	}
	if n := atomic.LoadInt32(&f.acceptNoMP); n != 0 {
		t.Errorf("有 %d 次 accept 缺 mp 头", n)
	}
}

// TestSequentialTasksAbsentIs404 任务在 mp 列表里不存在（前置未完成 / 活动未开始）
// 时，与其余任务同款语义：404 + 可读说明（既有契约，不因 mp 而特例化）。
func TestSequentialTasksAbsentIs404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	pn := newSeqPanel(t, srv)

	httpCode, got := postTaskAuto(t, pn, "u1", "Sequential_Tasks_4")
	if httpCode != http.StatusNotFound {
		t.Fatalf("code=%d want 404（任务不存在，与既有任务同语义）body=%v", httpCode, got)
	}
	if msg, _ := got["error"].(string); msg == "" {
		t.Error("404 必须带可读说明")
	}
}

// TestGrowthPendingFiltersLocked 吸收上游 09fd96e：上游锁定的任务不出待办。
// Sequential 族每日零点解锁一环，刚做完上一环时下一环以 locked 形态出现在列表里
// ——扫进队列只会 accept 不落账报失败（每日锁定窗口），零点解锁后自然回到待办。
func TestGrowthPendingFiltersLocked(t *testing.T) {
	locked := upstream.Task{TaskCode: "Sequential_Tasks_4", Target: 1, Current: 0, Locked: true}
	if growthPending(locked) {
		t.Error("locked 任务不得进待办（上游每日锁定窗口，accept 不落账）")
	}
	unlocked := upstream.Task{TaskCode: "Sequential_Tasks_4", Target: 1, Current: 0}
	if !growthPending(unlocked) {
		t.Error("解锁后应回到待办（同一判据，只差 locked 位）")
	}
	// 既有语义零回归：未 locked 的普通待办照旧入队。
	if !growthPending(upstream.Task{TaskCode: "chat_5", Target: 5, Current: 0}) {
		t.Error("普通待办不得被 locked 过滤影响")
	}
	if growthPending(upstream.Task{TaskCode: "chat_5", Target: 5, Current: 0, Claimed: true}) {
		t.Error("已领取仍应排除（既有语义）")
	}
}

// TestSequentialTasksFrontendWiring 前端接线：AUTO_TASKS 必须有 1..7 的说明文案 +
// MP_TASKS 标出小程序口径（缺一处用户就在任务表里看不到「一键完成」按钮或
// 不知道它是 mp 限定任务）。
func TestSequentialTasksFrontendWiring(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for i := 1; i <= 7; i++ {
		code := "Sequential_Tasks_" + itoaN(i)
		if !strings.Contains(s, "'"+code+"'") {
			t.Errorf("app.js AUTO_TASKS/MP_TASKS 缺 %s", code)
		}
	}
	// MP_TASKS 必须把 Sequential 族标成小程序口径（任务表/队列打 tag）。
	if !strings.Contains(s, "MP_TASKS") {
		t.Fatal("app.js 缺 MP_TASKS")
	}
	mpBlock := s[strings.Index(s, "const MP_TASKS"):]
	mpBlock = mpBlock[:strings.Index(mpBlock, "}")]
	for i := 1; i <= 7; i++ {
		code := "Sequential_Tasks_" + itoaN(i)
		if !strings.Contains(mpBlock, code) {
			t.Errorf("MP_TASKS 缺 %s（前端不会标「小程序」tag）", code)
		}
	}
}
