package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// schoolPeriodFake 假上游：开学季活动**已结束**（in_period=false），但任务列表仍
// 返回未完成的 pending 条目——这正是活动刚下线那几天的真实形态（上游返回历史任务
// 而 in_period 已翻假）。
//
// 上游 729247b 的处理是**删掉整个开学季功能**（因为活动 9/24 结束）。我们的原则
// 是保留代码 + 门控（活动可能复办）：本用例锁死「活动结束 ⇒ 不出待办、不入队」，
// 而不是「删代码」。
type schoolPeriodFake struct {
	inPeriod bool
	drawHits int32
}

func (f *schoolPeriodFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/portal/activity/school/tasks"):
			in := "false"
			if f.inPeriod {
				in = "true"
			}
			// 活动结束但任务列表仍给 pending 条目（真实形态）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"in_period":` + in + `,"tasks":[
				{"task_code":"share_invite","status":"pending","progress":0,"target_count":1},
				{"task_code":"chat_3_times","status":"pending","progress":0,"target_count":3}
			]}}`))
		case strings.HasSuffix(r.URL.Path, "/portal/activity/school/chance"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"chance":{"balance":0}}}`))
		case strings.HasSuffix(r.URL.Path, "/portal/activity/school/draw"):
			f.drawHits++
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"prize_name":"积分"}}`))
		case strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/portal/activity/school/vouchers"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"items":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newSchoolPeriodPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})
}

// TestScanAllNoSchoolPendingOutOfPeriod 活动已结束（in_period=false）时，扫描
// **不得**把开学季 pending 条目当待办——否则面板永远显示「有 N 项待办」而点执行
// 什么也做不成（scheduler 的 schoolAccount 一进门就因 !inPeriod 返回）。
func TestScanAllNoSchoolPendingOutOfPeriod(t *testing.T) {
	f := &schoolPeriodFake{inPeriod: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSchoolPeriodPanel(t, srv)

	req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Accounts []struct {
			UID      string `json:"uid"`
			School   []any  `json:"school"`
			InPeriod bool   `json:"in_period"`
		} `json:"accounts"`
		PendingCount int `json:"pending_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(got.Accounts))
	}
	if got.Accounts[0].InPeriod {
		t.Error("in_period 应为 false（假上游已结束）")
	}
	if n := len(got.Accounts[0].School); n != 0 {
		t.Errorf("活动结束仍扫出 %d 项开学季待办（面板会永远显示待办）", n)
	}
	if got.PendingCount != 0 {
		t.Errorf("pending_count=%d want 0", got.PendingCount)
	}
}

// TestScanAllSchoolPendingInPeriod 反向：活动**在期**时开学季待办照旧出现
// （门控只掐活动结束，不误伤在期——零回归）。
func TestScanAllSchoolPendingInPeriod(t *testing.T) {
	f := &schoolPeriodFake{inPeriod: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSchoolPeriodPanel(t, srv)

	req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var got struct {
		Accounts []struct {
			School []any `json:"school"`
		} `json:"accounts"`
		PendingCount int `json:"pending_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if n := len(got.Accounts[0].School); n == 0 {
		t.Error("活动在期时开学季待办必须照旧出现（门控不得误伤在期）")
	}
	if got.PendingCount == 0 {
		t.Error("活动在期时 pending_count 必须 > 0")
	}
}

// TestRunQueueNoSchoolItemOutOfPeriod 活动已结束时不入队开学季项：否则队列里会
// 出现一条「school_daily」永远完不成（scheduler 一进门就返回），用户以为队列卡住。
func TestRunQueueNoSchoolItemOutOfPeriod(t *testing.T) {
	f := &schoolPeriodFake{inPeriod: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSchoolPeriodPanel(t, srv)

	started, total, _, msg := pn.startGrowthQueue(1, true, true)
	if started {
		q := pn.queue()
		q.mu.Lock()
		items := append([]queueItem(nil), q.items...)
		q.mu.Unlock()
		for _, it := range items {
			if it.Kind == "school" {
				t.Errorf("活动结束仍入队开学季项: %+v", it)
			}
		}
		waitQueueIdle(t, pn)
		return
	}
	// 无待办也是正确结果（活动结束 + 无成长待办）。
	if !strings.Contains(msg, "没有待办") {
		t.Errorf("started=false 的说明应指明无可执行待办: %q (total=%d)", msg, total)
	}
}

// TestSchoolStatusStillReportsInPeriod 活动结束后**状态视图照旧可用**（只读展示
// in_period=false）——门控只掐「自动化」，不掐「看得见」：运维要能自己确认
// 「活动确实结束了」，而不是看到一个空白页以为坏了。
func TestSchoolStatusStillReportsInPeriod(t *testing.T) {
	f := &schoolPeriodFake{inPeriod: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSchoolPeriodPanel(t, srv)

	req := httptest.NewRequest("GET", "/panel/api/school/status", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Accounts []struct {
			UID      string `json:"uid"`
			InPeriod bool   `json:"in_period"`
			Err      string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(got.Accounts))
	}
	if got.Accounts[0].Err != "" {
		t.Errorf("活动结束不该报错（是正常状态）: %q", got.Accounts[0].Err)
	}
	if got.Accounts[0].InPeriod {
		t.Error("in_period 必须如实透出 false（运维据此确认活动状态）")
	}
}

// TestSchoolVouchersKeptOutOfPeriod 活动结束后**券码查询照旧可用**（上游 729247b
// 也明确保留券码查询）：历史抽中的券在活动结束后仍要能查——这是删功能与留功能的
// 分界线。
func TestSchoolVouchersKeptOutOfPeriod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/portal/activity/school/vouchers") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"items":[
				{"grant_id":1,"sku_code":"kfc_ice_cream","prize_name":"肯德基冰淇淋","code":"KFC1","valid_to":"2026-10-24"}]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	pn := newSchoolPeriodPanel(t, srv)

	req := httptest.NewRequest("GET", "/panel/api/school/vouchers", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "KFC1") {
		t.Errorf("活动结束后券码查询必须保留（历史券仍要能查）: %s", rec.Body.String())
	}
}

// TestSchoolOutOfPeriodNoDraws 活动结束后**不得发起抽奖**（无谓的上游写请求）：
// scheduler 的 inPeriod 门控已经覆盖，这里锁死「面板侧也不绕过它」。
func TestSchoolOutOfPeriodNoDraws(t *testing.T) {
	f := &schoolPeriodFake{inPeriod: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newSchoolPeriodPanel(t, srv)

	pn.cfg.Scheduler.RunSchoolAccountNow(pn.cfg.Pool.AuthByUID("u1"))
	if f.drawHits != 0 {
		t.Errorf("活动结束仍发起 %d 次抽奖（in_period 门控失效）", f.drawHits)
	}
}

// TestSchedulerSchoolAccountGatesOnInPeriod scheduler 侧门控的单点回归：
// schoolAccount 是三个入口（每日排程 / 面板手动 / 队列）的唯一汇合点，
// in_period=false 必须在**发任何上游写请求之前**返回。
func TestSchedulerSchoolAccountGatesOnInPeriod(t *testing.T) {
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes++
		}
		if strings.HasSuffix(r.URL.Path, "/portal/activity/school/tasks") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"in_period":false,"tasks":[
				{"task_code":"share_invite","status":"pending","progress":0,"target_count":1}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	done := make(chan struct{})
	go func() { defer close(done); sch.RunSchoolNow() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunSchoolNow 未在 3s 内返回")
	}
	if writes != 0 {
		t.Errorf("in_period=false 时发起了 %d 次上游写请求（门控必须在最前面）", writes)
	}
}
