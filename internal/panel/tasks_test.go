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
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// acceptFakePanel 假上游：accept 恒回 200 + code=0 + msg=OK，但 results[].status
// 与回读 accept_status 由 registered 决定——复现"200+OK 但服务端未落账"形态。
//
// 首次 GET（accept-all 的待办枚举）恒回 not_accepted，保证列表里确实有待接受的
// 码；之后的 GET（acceptVerified 的回读）才按 registered 作答。
// 返回的 *int64 记录 accept 请求次数（用于断言"失败重试一次"）。
func acceptFakePanel(t *testing.T, registered *atomic.Bool) (*httptest.Server, *int64) {
	t.Helper()
	var attempts, lists int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := "not_accepted"
		if registered.Load() {
			st = "accepted"
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			atomic.AddInt64(&attempts, 1)
			// 无论真假都回 200 + OK（旧口径下会被判成"接受成功"）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[` +
				`{"task_code":"chat_5","status":"` + st + `"}]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tasks"):
			if atomic.AddInt64(&lists, 1) == 1 {
				st = "not_accepted" // 首次枚举：必须有待接受的码
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[` +
				`{"task_code":"chat_5","accept_status":"` + st + `","target":5,"current":0}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &attempts
}

func newAcceptTestPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	oldGap := acceptBatchGap
	acceptBatchGap = time.Millisecond
	t.Cleanup(func() { acceptBatchGap = oldGap })

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})
}

func postAcceptAll(t *testing.T, pn *Panel) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/tasks/accept_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body.String())
	}
	return rec.Code, got
}

// TestTaskAcceptAllNotRegisteredNotCounted 上游 200+OK 但 results[].status 非
// accepted / 回读仍 not_accepted 时，accept-all **不得计数为成功**，该码必须进
// failed（这是"上报成功却不点亮"的根因形态，旧口径只数请求成功）。
// 同时断言失败后重试了一次（accept 请求共 2 次）。
func TestTaskAcceptAllNotRegisteredNotCounted(t *testing.T) {
	var registered atomic.Bool // 恒 false：模拟服务端始终未落账
	srv, attempts := acceptFakePanel(t, &registered)
	pn := newAcceptTestPanel(t, srv)

	code, got := postAcceptAll(t, pn)
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if n, _ := got["accepted"].(float64); n != 0 {
		t.Errorf("accepted=%v want 0（200+OK 但未登记，不能计数为成功）", got["accepted"])
	}
	failed, _ := got["failed"].([]any)
	if len(failed) != 1 || failed[0] != "chat_5" {
		t.Errorf("failed=%v want [chat_5]（未登记的码必须进 failed）", got["failed"])
	}
	if n := atomic.LoadInt64(attempts); n != 2 {
		t.Errorf("accept 请求次数=%d want 2（失败重试一次）", n)
	}
}

// TestTaskAcceptAllRegisteredCounted 登记生效（响应 accepted + 回读 accepted）
// 才计数成功：accepted=1、failed 为空、只请求一次（不触发重试）。
func TestTaskAcceptAllRegisteredCounted(t *testing.T) {
	var registered atomic.Bool
	registered.Store(true)
	srv, attempts := acceptFakePanel(t, &registered)
	pn := newAcceptTestPanel(t, srv)

	code, got := postAcceptAll(t, pn)
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if n, _ := got["accepted"].(float64); n != 1 {
		t.Errorf("accepted=%v want 1", got["accepted"])
	}
	if failed, _ := got["failed"].([]any); len(failed) != 0 {
		t.Errorf("failed=%v want 空", got["failed"])
	}
	if n := atomic.LoadInt64(attempts); n != 1 {
		t.Errorf("accept 请求次数=%d want 1（登记生效不重试）", n)
	}
}

// TestAcceptVerifiedRetryThenSucceed 首次未登记、重试后生效：第一次 accept 返回
// not_accepted（服务端未落账），第二次回 accepted 且回读 accepted → 判成功。
// 锁死"重试一次"的语义（上游 Python 侧同口径）。
func TestAcceptVerifiedRetryThenSucceed(t *testing.T) {
	oldGap := acceptBatchGap
	acceptBatchGap = time.Millisecond
	t.Cleanup(func() { acceptBatchGap = oldGap })

	var registered atomic.Bool
	srv, attempts := acceptFakePanel(t, &registered)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})

	a := p.AuthByUID("u1")
	// 第 2 次请求前"上游落账"：模拟服务端延迟生效。
	go func() {
		for atomic.LoadInt64(attempts) < 1 {
			time.Sleep(time.Millisecond)
		}
		registered.Store(true)
	}()
	accepted, failed := pn.acceptVerified(a, []string{"chat_5"})
	if len(accepted) != 1 || accepted[0] != "chat_5" {
		t.Errorf("accepted=%v want [chat_5]（重试后生效）", accepted)
	}
	if len(failed) != 0 {
		t.Errorf("failed=%v want 空", failed)
	}
	if n := atomic.LoadInt64(attempts); n != 2 {
		t.Errorf("accept 请求次数=%d want 2", n)
	}
}

// TestSchoolVouchersGlobalGatedNoUpstreamCall global 账号不发上游调用（与
// school/status 同款 D4 门控）：面板必须直接标 error，vouchers 恒为空，
// 且上游 /vouchers 端点命中次数为 0。
func TestSchoolVouchersGlobalGatedNoUpstreamCall(t *testing.T) {
	var voucherHits, taskHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/vouchers") {
			voucherHits++
		}
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			taskHits++
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"}) // global 域
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN = srv.URL, srv.URL
	up.ChatBaseGlobal, up.BillingBaseGlobal = srv.URL, srv.URL
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})

	req := httptest.NewRequest("GET", "/panel/api/school/vouchers", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Vouchers []struct {
				Code string `json:"code"`
			} `json:"vouchers"`
			Err string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1（global 账号也要占一行，标 error 而非消失）", len(got.Accounts))
	}
	if got.Accounts[0].Err == "" {
		t.Error("global 账号应有 error 说明（无开学季活动）")
	}
	if len(got.Accounts[0].Vouchers) != 0 {
		t.Errorf("global 账号 vouchers=%v want 空", got.Accounts[0].Vouchers)
	}
	if voucherHits != 0 {
		t.Errorf("global 账号不应发起上游 /vouchers 调用，实际命中 %d 次", voucherHits)
	}
}

// TestSchoolVouchersCNQueriesAndParses CN 账号走真实查询：3 并发拉取上游
// /vouchers，逐账号返回券列表；空券列表（未抽中）不算错误。
func TestSchoolVouchersCNQueriesAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/vouchers") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-User-Id") == "c2" { // 二号账号未抽中
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"grant_id":1,` +
			`"sku_code":"kfc_ice_cream","prize_name":"肯德基冰淇淋","code":"KFC-XYZ",` +
			`"valid_to":"2026-10-24","granted_at":"2026-09-16T11:20:00+08:00"}]}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	p.Add(&auth.Auth{UID: "c2", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN = srv.URL, srv.URL
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})

	req := httptest.NewRequest("GET", "/panel/api/school/vouchers", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Vouchers []struct {
				GrantID   int64  `json:"grant_id"`
				PrizeName string `json:"prize_name"`
				Code      string `json:"code"`
				ValidTo   string `json:"valid_to"`
			} `json:"vouchers"`
			Err string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(got.Accounts))
	}
	byUID := map[string]int{}
	for i, a := range got.Accounts {
		byUID[a.UID] = i
		if a.Err != "" {
			t.Errorf("CN 账号 %s 不应有 error: %s", a.UID, a.Err)
		}
	}
	c1 := got.Accounts[byUID["c1"]]
	if len(c1.Vouchers) != 1 || c1.Vouchers[0].Code != "KFC-XYZ" {
		t.Fatalf("c1 vouchers=%+v want 1 张 KFC-XYZ", c1.Vouchers)
	}
	if c1.Vouchers[0].PrizeName != "肯德基冰淇淋" || c1.Vouchers[0].ValidTo != "2026-10-24" {
		t.Errorf("c1 voucher 字段: %+v", c1.Vouchers[0])
	}
	if len(got.Accounts[byUID["c2"]].Vouchers) != 0 {
		t.Errorf("c2 未抽中，vouchers 应为空: %+v", got.Accounts[byUID["c2"]].Vouchers)
	}
}
