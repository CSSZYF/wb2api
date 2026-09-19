package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSchoolSeasonChatEvents 校园日判据事件形状：复用 SchoolChatTimesEvents
// （同一 mini 指纹 chat_request_send），**必须**叠加 activityId=school_open_day_2026。
//
// 为什么单测锁死：activityId 是 school_season 唯一的判据关联键——上游实证
// "无 activityId 的事件不点亮"（0ceb9c7c/f8657995）。字段名写错（activityID /
// activity_id）或值写错都会让整条链路静默失效：上报恒 200，进度恒 0。
func TestSchoolSeasonChatEvents(t *testing.T) {
	events := SchoolSeasonChatEvents("wb-run-1-0")
	if len(events) != 1 {
		t.Fatalf("events=%d want 1（target=1，一条即点亮）", len(events))
	}
	ev := events[0]
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v want chat_request_send", ev["eventCode"])
	}
	// 字节级核对 activityId 的键名与取值（显示层可能改写字符串，见环境说明）。
	if got, ok := ev["activityId"]; !ok {
		t.Fatal("缺 activityId（校园日判据关联键，缺失即不点亮）")
	} else if got != "school_open_day_2026" {
		t.Errorf("activityId=%v want school_open_day_2026", got)
	}
	// 复用 SchoolChatTimesEvents 的单一事实源：**键集合**必须等于 school 域形状
	// + activityId（多/少都说明形状漂移，手抄了第二份）。
	//
	// 只比键集合与稳定值，不比逐字段值——SchoolChatTimesEvents 每次调用生成新的
	// requestId（clientToken），且 mentionContexts 等是切片（不可比较）。
	base := SchoolChatTimesEvents("wb-run-1-0")
	for k := range base {
		if _, ok := ev[k]; !ok {
			t.Errorf("校园日事件缺字段 %s（应与 school 域 chat_3_times 同形）", k)
		}
	}
	for k := range ev {
		if _, ok := base[k]; !ok && k != "activityId" {
			t.Errorf("校园日事件多出字段 %s（只应比 school 域形状多 activityId 一个）", k)
		}
	}
	if len(ev) != len(base)+1 {
		t.Errorf("事件字段数=%d want %d（= school 域形状 + activityId）", len(ev), len(base)+1)
	}
	// 稳定值抽样（这些字段不该随调用变化）。
	//
	// 注意：本仓 SchoolChatTimesEvents 的形状与上游 Python 的 mini_chat_event
	// **不完全相同**（本仓 29 键：用 traceId/rootRequestId 而非 requestId，
	// inputLength=14 而非 12，无 mode；source/ideName 等指纹字段由 mpEventBase
	// 在 ReportMPEvent 时合并，不在事件本身）。此处只钉住本仓既有形状，
	// 不为对齐 Python 而改动 school 域 chat_3_times 的线上口径。
	for k, want := range map[string]any{
		"eventCode": "chat_request_send", "inputLength": 14,
		"agentName": "mp", "agentType": "main",
		"conversationId":       "wb-run-1-0",
		"parentConversationId": "wb-run-1-0",
		"codebuddy.session_id": "wb-run-1-0",
		"activityId":           "school_open_day_2026",
	} {
		if got, ok := ev[k]; !ok {
			t.Errorf("缺字段 %s（want %v）", k, want)
		} else if got != want {
			t.Errorf("%s=%v want %v", k, got, want)
		}
	}
	// traceId / rootRequestId / codebuddy.conversation_request_id 三者必须同值
	// （服务端按它们做事件 JOIN，不同值会被当成三条独立事件）。
	rid, _ := ev["traceId"].(string)
	if rid == "" {
		t.Error("traceId 为空")
	}
	for _, k := range []string{"rootRequestId", "codebuddy.conversation_request_id"} {
		if ev[k] != rid {
			t.Errorf("%s=%v 应与 traceId(%s) 同值（JOIN 键）", k, ev[k], rid)
		}
	}
	if ev["conversationId"] != "wb-run-1-0" {
		t.Errorf("conversationId=%v 应透传入参", ev["conversationId"])
	}
	// 序列化后再核对一次键名（防止 map 键被写成非常量/带空格）。
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"activityId":"school_open_day_2026"`) {
		t.Errorf("序列化后 activityId 键值不对：%s", b)
	}
}

// TestReportMPEventMergesSchoolSeasonFingerprint 判据事件经 ReportMPEvent 上报时
// 必须与小程序公共指纹（mpEventBase）**合并**后发送，且 activityId 一并落地。
//
// 为什么关键：事件构造器只给事件级字段，指纹（source/ideName/ideType/userId 等）
// 由上报函数逐事件合并。合并写漏（比如覆盖而非叠加）会让事件缺指纹 → 上游
// 不按 mini_program 口径归账 → 上报恒 200 但进度恒 0（静默失效）。
func TestReportMPEventMergesSchoolSeasonFingerprint(t *testing.T) {
	var body []map[string]any
	var gotPlatform, gotProduct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("X-Client-Platform")
		gotProduct = r.Header.Get("X-Client-Product")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	err := c.ReportMPEvent(&auth.Auth{AccessToken: "at", UID: "u-9", Nickname: "测试"},
		SchoolSeasonChatEvents("conv-9")...)
	if err != nil {
		t.Fatalf("ReportMPEvent: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("上报事件数=%d want 1", len(body))
	}
	ev := body[0]
	// 指纹（mpEventBase 合并面）。
	if ev["platform"] != "mini_program" || ev["ideType"] != "WorkBuddy_MP" {
		t.Errorf("指纹未合并：platform=%v ideType=%v", ev["platform"], ev["ideType"])
	}
	if ev["ideName"] != "wx_app_cloud" || ev["extName"] != "workbuddy-mp" {
		t.Errorf("指纹未合并：ideName=%v extName=%v", ev["ideName"], ev["extName"])
	}
	if ev["userId"] != "u-9" {
		t.Errorf("userId=%v want u-9（账号维度归账）", ev["userId"])
	}
	// 判据面。
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v want chat_request_send", ev["eventCode"])
	}
	if ev["activityId"] != schoolActivityID {
		t.Errorf("activityId=%v want %s（合并后仍须在）", ev["activityId"], schoolActivityID)
	}
	if ev["conversationId"] != "conv-9" {
		t.Errorf("conversationId=%v 被合并覆盖", ev["conversationId"])
	}
	// 上报口径头：/v2/report 族用 mp-weixin（与 growth 域的 miniprogram **不同值**，
	// 勿"统一"）。
	if gotPlatform != "mp-weixin" {
		t.Errorf("埋点头 X-Client-Platform=%q want mp-weixin（与 growth 域口径不同值）", gotPlatform)
	}
	if gotProduct != "workbuddy-mp" {
		t.Errorf("X-Client-Product=%q want workbuddy-mp", gotProduct)
	}
}

// TestMPPlatformHeaderValuesDiffer growth 域与埋点域的 mp 头**取值不同**，
// 是上游两处实测原值，不能统一：统一成任一个都会让另一半链路失效。
func TestMPPlatformHeaderValuesDiffer(t *testing.T) {
	if mpPlatformValue != "miniprogram" {
		t.Errorf("growth 域 X-Client-Platform=%q want miniprogram", mpPlatformValue)
	}
	if mpReportPlatformValue != "mp-weixin" {
		t.Errorf("埋点域 X-Client-Platform=%q want mp-weixin", mpReportPlatformValue)
	}
	if mpPlatformValue == mpReportPlatformValue {
		t.Error("两域取值相同——但上游实测它们是不同原值，此处相等说明有一处被误改")
	}
}

// TestSchoolSeasonActivityIDConstant 活动 code 是**硬编码常量**（上游同口径）：
// /config 响应里不返回该值，无法从接口获取。本用例把它钉住——值改了必须是有意为之。
func TestSchoolSeasonActivityIDConstant(t *testing.T) {
	if schoolActivityID != "school_open_day_2026" {
		t.Errorf("schoolActivityID=%q want school_open_day_2026（与 school 域开学季同一活动）", schoolActivityID)
	}
}

// TestListTasksMPHeader 小程序口径列表请求必须带 X-Client-Platform: miniprogram。
// 缺该头时上游不下发 mp 限定任务（school_season）——列表里看不到就等于扫不到待办。
func TestListTasksMPHeader(t *testing.T) {
	var gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("X-Client-Platform")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"school_season","accept_status":"not_accepted",
			 "progress":{"current":0,"target":1},"reward_credit":100,"reward_energy":5}
		]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	tasks, err := c.ListTasksMP(a)
	if err != nil {
		t.Fatalf("ListTasksMP: %v", err)
	}
	if gotPlatform != mpPlatformValue {
		t.Errorf("X-Client-Platform=%q want %q", gotPlatform, mpPlatformValue)
	}
	if len(tasks) != 1 || tasks[0].TaskCode != "school_season" {
		t.Fatalf("tasks=%+v want [school_season]", tasks)
	}
	if tasks[0].Credit != 100 || tasks[0].Energy != 5 {
		t.Errorf("奖励解析：credit=%d energy=%d want 100/5", tasks[0].Credit, tasks[0].Energy)
	}
}

// TestListTasksDefaultHasNoMPHeader 默认口径**不得**带 mp 头（零回归：既有 18 项
// 任务的列表口径逐字不变，避免给全部既有调用引入新的上游特征面）。
func TestListTasksDefaultHasNoMPHeader(t *testing.T) {
	var gotPlatform string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotPlatform = r.Header.Get("X-Client-Platform")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.ListTasks(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if hits != 1 {
		t.Fatalf("请求数=%d want 1", hits)
	}
	if gotPlatform != "" {
		t.Errorf("默认口径不应带 X-Client-Platform，got %q", gotPlatform)
	}
}

// TestAcceptTasksMPHeader mp 口径 accept 必须带 mp 头：缺头时上游对 mp 限定任务
// 返回 task not found（实测），accept 恒失败 → 后续上报全部不归账。
func TestAcceptTasksMPHeader(t *testing.T) {
	var gotPlatform, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform, gotPath = r.Header.Get("X-Client-Platform"), r.URL.Path
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[
			{"task_code":"school_season","status":"accepted"}]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	res, err := c.AcceptTasksMP(&auth.Auth{AccessToken: "at", UID: "u1"}, []string{"school_season"})
	if err != nil {
		t.Fatalf("AcceptTasksMP: %v", err)
	}
	if gotPlatform != mpPlatformValue {
		t.Errorf("X-Client-Platform=%q want %q", gotPlatform, mpPlatformValue)
	}
	if !strings.HasSuffix(gotPath, "/v2/activity/growth/tasks/accept") {
		t.Errorf("path=%q 应为 growth accept 端点", gotPath)
	}
	if len(res) != 1 || res[0].Status != "accepted" {
		t.Errorf("results=%+v want accepted", res)
	}
}

// TestClaimRewardMPChatFirstWithHeader mp 口径 claim 的首选路径是 **chat 域**
// 带 X-Client-Platform: miniprogram（上游 claim_one(mp=True) 实测形态）。
//
// 为什么关键：上游两个域的 claim 头族是二选一的实测原值（chat 域带 mp 头 /
// web 域带 web 头）。写成"web 域 + mp 头"是未经验证的第三种组合。
func TestClaimRewardMPChatFirstWithHeader(t *testing.T) {
	var gotPlatform, gotPath, gotHost string
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform, gotPath, gotHost = r.Header.Get("X-Client-Platform"), r.URL.Path, r.Host
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer chatSrv.Close()
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不应降级到 web 域（chat 域已 200）：%s", r.URL.Path)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":0,"energy":0}}`))
	}))
	defer webSrv.Close()

	c := &Client{HTTP: chatSrv.Client(), ChatBaseCN: chatSrv.URL, BillingBaseCN: chatSrv.URL, WebBaseCN: webSrv.URL}
	credit, energy, err := c.ClaimRewardMP(&auth.Auth{AccessToken: "at", UID: "u1"}, "school_season")
	if err != nil {
		t.Fatalf("ClaimRewardMP: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy=%d/%d want 100/5", credit, energy)
	}
	if gotHost != strings.TrimPrefix(chatSrv.URL, "http://") {
		t.Errorf("首选应走 chat 域（chatBase），实际 host=%s", gotHost)
	}
	if gotPlatform != mpPlatformValue {
		t.Errorf("X-Client-Platform=%q want %q（mp 任务 claim 必须带小程序口径头）", gotPlatform, mpPlatformValue)
	}
	if want := "/activity/growth/tasks/school_season/claim"; gotPath != want {
		t.Errorf("path=%q want %q", gotPath, want)
	}
}

// TestClaimRewardMPFallsBackToWebOn400 chat 域 400 时降级 web 域（上游
// _claim_via_web 同口径），且降级请求带 **web** 头族（x-client-platform: web）——
// 不是把 mp 头搬到 web 域。
func TestClaimRewardMPFallsBackToWebOn400(t *testing.T) {
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"task not completed","data":null}`))
	}))
	defer chatSrv.Close()

	var webPlatform, webPath, webReferer string
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webPlatform, webPath = r.Header.Get("x-client-platform"), r.URL.Path
		webReferer = r.Header.Get("Referer")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer webSrv.Close()

	c := &Client{HTTP: chatSrv.Client(), ChatBaseCN: chatSrv.URL, BillingBaseCN: chatSrv.URL, WebBaseCN: webSrv.URL}
	credit, energy, err := c.ClaimRewardMP(&auth.Auth{AccessToken: "at", UID: "u1"}, "school_season")
	if err != nil {
		t.Fatalf("400 应降级 web 域而非报错: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("降级后应拿到奖励：credit/energy=%d/%d", credit, energy)
	}
	if webPlatform != "web" {
		t.Errorf("降级请求 x-client-platform=%q want web（web 域头族，不是 mp 头）", webPlatform)
	}
	if want := "/activity/growth/tasks/school_season/claim"; webPath != want {
		t.Errorf("降级 path=%q want %q", webPath, want)
	}
	if !strings.Contains(webReferer, "workbuddy.cn") {
		t.Errorf("降级 Referer=%q 应指向 workbuddy.cn", webReferer)
	}
}

// TestClaimRewardMPNon400NotDowngraded 非 400（如 401/403/5xx）**不降级**：
// 盲目降级会把鉴权/风控错误掩盖成"上游不支持领奖"，并多打一次无谓的写请求。
func TestClaimRewardMPNon400NotDowngraded(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":40100,"msg":"token invalid","data":null}`))
		}))
		webHit := 0
		webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			webHit++
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":0,"energy":0}}`))
		}))
		c := &Client{HTTP: chatSrv.Client(), ChatBaseCN: chatSrv.URL, BillingBaseCN: chatSrv.URL, WebBaseCN: webSrv.URL}
		if _, _, err := c.ClaimRewardMP(&auth.Auth{AccessToken: "at", UID: "u1"}, "school_season"); err == nil {
			t.Errorf("http=%d 应返回错误（不掩盖）", status)
		}
		if webHit != 0 {
			t.Errorf("http=%d 不应降级到 web 域（命中 %d 次）", status, webHit)
		}
		webSrv.Close()
		chatSrv.Close()
	}
}

// TestClaimRewardDefaultKeepsWebPlatform 默认口径 claim 的来源端标记必须仍是 web
// （零回归：既有任务领奖口径逐字不变）。
func TestClaimRewardDefaultKeepsWebPlatform(t *testing.T) {
	var gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("x-client-platform")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":1,"energy":1}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err != nil {
		t.Fatalf("ClaimReward: %v", err)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web（默认口径零回归）", gotPlatform)
	}
}

// TestClaimRewardMPAlreadyClaimed mp 任务重复领奖幂等：上游 already_claimed=true
// 时返回 (0,0,nil)——不算失败，避免"已领"被当成错误反复重试。
func TestClaimRewardMPAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true,"credit":0,"energy":0}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	credit, energy, err := c.ClaimRewardMP(&auth.Auth{AccessToken: "at", UID: "u1"}, "school_season")
	if err != nil {
		t.Fatalf("already_claimed 不应报错: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("credit/energy=%d/%d want 0/0（幂等无新增）", credit, energy)
	}
}

// TestVerifyAcceptedMPReadsMPList 登记回读必须走 mp 口径：school_season 在默认
// 口径列表里**不存在**，用默认口径回读会恒判"未登记"——面板据此空转重试，
// 表现为"accept 永远失败"而实际已落账。
func TestVerifyAcceptedMPReadsMPList(t *testing.T) {
	var mpHits, defaultHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Platform") == mpPlatformValue {
			mpHits++
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"school_season","accept_status":"accepted"}]}}`))
			return
		}
		defaultHits++
		// 默认口径里没有 school_season（真实形态）。
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"chat_5","accept_status":"accepted"}]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	if !c.VerifyAcceptedMP(a, "school_season") {
		t.Error("VerifyAcceptedMP 应判已登记（mp 列表里有 accepted）")
	}
	if mpHits != 1 || defaultHits != 0 {
		t.Errorf("请求口径不对：mp=%d default=%d want 1/0", mpHits, defaultHits)
	}
	// 对照：默认口径回读判未登记（这正是必须分口径的原因）。
	if c.VerifyAccepted(a, "school_season") {
		t.Error("默认口径回读应判未登记（列表里没有该码）——本用例的动机断言")
	}
}
