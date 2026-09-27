package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// growthFake 假上游：只服务任务列表 + accept，供 startGrowthQueue 的扫描/建队路径
// 使用。growPending 控制是否有一条待办（chat_5，autoActions 里有动作）。
type growthFake struct {
	lists  atomic.Int64
	accept atomic.Int64
}

func (f *growthFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			f.accept.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[{"task_code":"chat_5","status":"accepted"}]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tasks"):
			f.lists.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[` +
				`{"task_code":"chat_5","accept_status":"accepted","target":5,"current":0}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// growthTestPanel 造一个带 scheduler 的面板（school 分支需要非 nil Scheduler）。
// 全部节流/轮询间隔压到毫秒级：队列路径会走 acceptVerified（acceptBatchGap ×
// 重试）与 taskByCodeWaiting（claimPollGap × claimPollAttempts），生产口径合计
// 十余秒，用例等不起。
func growthTestPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	oldGap, oldBatch, oldPoll, oldAttempts, oldMpGap := reportGap, acceptBatchGap, claimPollGap, claimPollAttempts, mpActionGap
	reportGap, acceptBatchGap = time.Millisecond, time.Millisecond
	claimPollGap, claimPollAttempts = time.Millisecond, 2
	mpActionGap = time.Millisecond // accept 回读验证的写动作间隔（生产 2s）
	t.Cleanup(func() {
		reportGap, acceptBatchGap = oldGap, oldBatch
		claimPollGap, claimPollAttempts = oldPoll, oldAttempts
		mpActionGap = oldMpGap
	})

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})
}

// TestRunGrowthQueueOnceStartsQueue 调度器 growth 回调与「执行全部待办」同管线：
// 有 mp 口径待办时必须真建队（started=true、队列 running、items 非空），
// 而不是静默跳过——这是「每日自动推进 Sequential 链」的落点。
func TestRunGrowthQueueOnceStartsQueue(t *testing.T) {
	f := &growthFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := growthTestPanel(t, srv)

	started, total, seq, msg := pn.startGrowthQueue(1, true, false)
	if !started {
		t.Fatalf("应建队成功：msg=%q（上游列表有 chat_5 待办）", msg)
	}
	if total != 1 {
		t.Errorf("total=%d want 1", total)
	}
	if seq <= 0 {
		t.Errorf("seq=%d want >0（前端按 seq 认轮次）", seq)
	}

	q := pn.queue()
	q.mu.Lock()
	running, items := q.running, len(q.items)
	q.mu.Unlock()
	if !running {
		t.Error("建队后 q.running 必须为 true（否则重复触发会并发重跑）")
	}
	if items != 1 {
		t.Errorf("q.items=%d want 1", items)
	}
	// 等队列收尾，避免 goroutine 泄漏到后续用例（per-account 锁是包级）。
	waitQueueIdle(t, pn)
}

// TestRunGrowthQueueOnceConcurrentSkipped 队列已在跑时第二次触发必须安全跳过
// （返回 seq==-1 的冲突语义），不能并发起第二轮。
func TestRunGrowthQueueOnceConcurrentSkipped(t *testing.T) {
	f := &growthFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := growthTestPanel(t, srv)

	if started, _, _, msg := pn.startGrowthQueue(1, true, false); !started {
		t.Fatalf("首轮应建队成功：%q", msg)
	}
	started, _, seq, msg := pn.startGrowthQueue(1, true, false)
	if started || seq != -1 {
		t.Errorf("并发第二轮应被挡：started=%v seq=%d（want false/-1）", started, seq)
	}
	if !strings.Contains(msg, "正在执行") {
		t.Errorf("冲突提示应说明队列在跑：%q", msg)
	}
	waitQueueIdle(t, pn)
}

// TestRunGrowthQueueOnceNoPending 无待办时 started=false 且不置 running
// （调度器到点不该把面板搞成「执行中」）。
func TestRunGrowthQueueOnceNoPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 空任务列表：既无待办也无 mp 任务。
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	defer srv.Close()
	pn := growthTestPanel(t, srv)

	started, total, seq, msg := pn.startGrowthQueue(1, true, false)
	if started || total != 0 || seq != 0 {
		t.Errorf("无待办应 (false,0,0)：started=%v total=%d seq=%d", started, total, seq)
	}
	if msg == "" {
		t.Error("无待办必须给出说明文案（面板 toast 用它）")
	}
	q := pn.queue()
	q.mu.Lock()
	running := q.running
	q.mu.Unlock()
	if running {
		t.Error("无待办不得置 running")
	}
}

// TestRunGrowthQueueOnceConcurrencyClamped 并发入参夹取到 [1,4]：0/负数 → 1，
// 超限 → 4（调度器恒传 1；HTTP 入参由前端约束，这里守住后端）。
func TestRunGrowthQueueOnceConcurrencyClamped(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 1}, {-3, 1}, {2, 2}, {9, 4}} {
		f := &growthFake{}
		srv := httptest.NewServer(f.handler())
		pn := growthTestPanel(t, srv)
		started, _, _, msg := pn.startGrowthQueue(tc.in, true, false)
		if !started {
			t.Fatalf("in=%d 应建队成功：%q", tc.in, msg)
		}
		q := pn.queue()
		q.mu.Lock()
		got := q.conc
		q.mu.Unlock()
		if got != tc.want {
			t.Errorf("conc(in=%d)=%d want %d", tc.in, got, tc.want)
		}
		waitQueueIdle(t, pn)
		srv.Close()
	}
}

// TestTasksRunQueueHTTPConflictStatus 队列在跑时 HTTP 入口返回 409（与调度器回调
// 共用同一核心，语义必须一致——前端按 409 提示「队列正在执行中」）。
func TestTasksRunQueueHTTPConflictStatus(t *testing.T) {
	f := &growthFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := growthTestPanel(t, srv)

	if started, _, _, msg := pn.startGrowthQueue(1, true, false); !started {
		t.Fatalf("首轮应建队成功：%q", msg)
	}
	req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	waitQueueIdle(t, pn)
}

// mpOnlyFake 假上游：**默认口径列表里没有任何待办**（只有已领取项），待办只在
// mp 口径列表里下发（school_season / Sequential_Tasks_1 的真实形态——这些码
// 只在 X-Client-Platform: miniprogram 口径出现）。
//
// 存在的理由（吸收上游 08752df）：run_queue 的内嵌扫描若只拉默认口径，扫描接口
// 显示「有 N 项待办」而点「执行全部待办」却报「无可执行待办（全部账号任务已
// 完成）」——两条路径的口径必须一致（都走 listAllTasks 的 mp 并集）。
type mpOnlyFake struct {
	mpLists      atomic.Int64
	defaultLists atomic.Int64
}

func (f *mpOnlyFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			if r.Header.Get("X-Client-Platform") == "miniprogram" {
				f.mpLists.Add(1)
				// mp 口径：Sequential_Tasks_1 待办（未接受）。
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
					{"task_code":"Sequential_Tasks_1","title":"小程序首对话","accept_status":"not_accepted",
					 "progress":{"current":0,"target":1},"reward_credit":100,"reward_energy":5}]}}`))
				return
			}
			f.defaultLists.Add(1)
			// 默认口径：只有一条已领取的常规任务——无待办（真实形态）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"chat_5","accept_status":"claimed","progress":{"current":5,"target":5}}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks/accept"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[{"task_code":"Sequential_Tasks_1","status":"accepted"}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/claim"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestStartGrowthQueueMergesMPPending 吸收上游 08752df：建队扫描必须与
// scan_all 同口径（listAllTasks 的默认 + mp 并集）。只有 mp 待办时也必须真建队
// ——否则面板「扫描显示待办、执行报无可执行」自相矛盾。
func TestStartGrowthQueueMergesMPPending(t *testing.T) {
	f := &mpOnlyFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := growthTestPanel(t, srv)

	// 先确认这条待办在扫描路径可见（两条路径的口径必须一致）。
	a := pn.cfg.Pool.AuthByUID("u1")
	tasks, err := pn.listAllTasks(a)
	if err != nil {
		t.Fatalf("listAllTasks: %v", err)
	}
	found := false
	for _, tk := range tasks {
		if tk.TaskCode == "Sequential_Tasks_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("listAllTasks 未合并 mp 口径待办（扫描会看不到）: %v", tasks)
	}

	started, total, _, msg := pn.startGrowthQueue(1, true, false)
	if !started {
		t.Fatalf("只有 mp 口径待办时必须建队（否则报「无可执行待办」自相矛盾）：msg=%q", msg)
	}
	if total != 1 {
		t.Errorf("total=%d want 1（mp 待办 1 项）", total)
	}
	if f.mpLists.Load() == 0 {
		t.Error("建队路径未走 mp 口径列表（listAllTasks 未生效）")
	}
	waitQueueIdle(t, pn)
}

// waitQueueIdle 等队列跑完（队列项跑完后 q.running 落回 false）。
// 超时即失败：队列 goroutine 若卡住，后续用例会因 per-account 锁未释放而假失败。
func waitQueueIdle(t *testing.T, pn *Panel) {
	t.Helper()
	q := pn.queue()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		running := q.running
		q.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("队列在 5s 内未收尾（goroutine 卡住或 running 未复位）")
}
