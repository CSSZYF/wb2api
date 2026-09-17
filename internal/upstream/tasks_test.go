package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestClaimRewardWebEndpoint 领奖走 Web 域（workbuddy.cn）、任务码在路径里、无 body。
// 这是与 CLI 域（copilot.tencent.com/v2/.../reward/claim，task_code 在 body）的关键区别——
// 后者路径不存在，曾导致长期 400 "task not completed" 误判为"上游不支持领取"。
func TestClaimRewardWebEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	var gotPlatform, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("x-client-platform")
		gotReferer = r.Header.Get("Referer")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimReward(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy = %d/%d, want 100/5", credit, energy)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}
	if want := "/activity/growth/tasks/Model_chat_GLM5.2/claim"; gotPath != want {
		t.Errorf("path=%q want %q（任务码必须在路径里）", gotPath, want)
	}
	if gotBody != "" {
		t.Errorf("claim 不应携带 body，got %q", gotBody)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web", gotPlatform)
	}
	if !strings.Contains(gotReferer, "workbuddy.cn") {
		t.Errorf("Referer=%q 应指向 workbuddy.cn", gotReferer)
	}
}

// TestClaimRewardAlreadyClaimed 重复领取：上游返回 already_claimed=true，
// 本地应视为"无新增奖励但不报错"（幂等语义）。
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	credit, energy, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed should not error: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("already claimed should yield 0/0, got %d/%d", credit, energy)
	}
}

// TestClaimRewardNotCompleted 未达标：上游 400 + task not completed 应作为错误透出。
func TestClaimRewardNotCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "task not completed"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err == nil {
		t.Fatal("want error for not-completed task")
	}
}

// acceptFakeUpstream 假上游：POST accept 恒回 200 + code=0 + msg=OK（**不看真伪**），
// GET tasks 的 accept_status 由 statusFn 决定——用来复现"200+OK 但未登记"形态。
//
// 这是线上 13 个任务"上报成功却不点亮"的根因形态：只看 HTTP 状态与 msg 会把
// 未落账判成成功，后续行为事件全部不归账。本用例锁死修复后的判定口径。
func acceptFakeUpstream(t *testing.T, statusFn func(attempt int) string) (*httptest.Server, *int) {
	t.Helper()
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			attempts++
			// 恒 200 + OK + results 里 status 明说未接受（上游真实形态之一）。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[` +
				`{"task_code":"chat_5","status":"` + statusFn(attempts) + `"}]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tasks"):
			st := statusFn(attempts)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` +
				`{"task_code":"chat_5","accept_status":"` + st + `","target":5,"current":0}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &attempts
}

// TestAcceptTasksParsesResultsStatus accept 必须解析 data.results[].status，
// 而不是只看 HTTP 200 / msg=OK。
func TestAcceptTasksParsesResultsStatus(t *testing.T) {
	srv, _ := acceptFakeUpstream(t, func(int) string { return "not_accepted" })
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	results, err := c.AcceptTasks(&auth.Auth{AccessToken: "at", UID: "u1"}, []string{"chat_5"})
	if err != nil {
		t.Fatalf("HTTP 200 + code=0 不应报错: %v", err)
	}
	if len(results) != 1 || results[0].TaskCode != "chat_5" || results[0].Status != "not_accepted" {
		t.Fatalf("results=%+v want 1 条 chat_5/not_accepted（status 必须解析出来）", results)
	}
}

// TestAcceptTasksResultsAbsent 老口径响应（data 里没有 results）：返回空结果
// 且无错误，调用方走宽松分支（不因形状变化误判失败）。
func TestAcceptTasksResultsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	results, err := c.AcceptTasks(&auth.Auth{AccessToken: "at", UID: "u1"}, []string{"chat_5"})
	if err != nil {
		t.Fatalf("无 results 不应报错: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results=%+v want 空", results)
	}
}

// TestVerifyAccepted 回读判定：accept_status 非 not_accepted 才算登记生效；
// 列表里没有该码 → 无法确认（false）；查询失败 → false（由调用方决定重试）。
func TestVerifyAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"accepted", `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"accepted"}]}}`, true},
		{"claimed", `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"claimed"}]}}`, true},
		{"completed", `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"completed"}]}}`, true},
		{"not_accepted", `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"not_accepted"}]}}`, false},
		{"空状态", `{"code":0,"data":{"tasks":[{"task_code":"chat_5"}]}}`, false},
		{"码不在列表", `{"code":0,"data":{"tasks":[{"task_code":"other"}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
			if got := c.VerifyAccepted(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); got != tc.want {
				t.Errorf("VerifyAccepted=%v want %v", got, tc.want)
			}
		})
	}
}

// TestVerifyAcceptedQueryError 回读查询失败（HTTP 500）→ false，不 panic 不误判 true。
func TestVerifyAcceptedQueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if c.VerifyAccepted(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5") {
		t.Error("查询失败应返回 false（不能凭空认定已登记）")
	}
}
