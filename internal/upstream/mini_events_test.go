package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestMiniExpertUseEventShape Sequential_Tasks_2「小程序内选中专家并完成有效对话」
// 的判据事件：mp 指纹 expert_actual_use。
//
// 上游 task_runner 实测（2026-09-23）：上报即 completed，claim +200c+5e。
// 与 school 域的 SchoolExpertUseEvents 是**两套口径**，勿混：
//   - 不带 conversationId / activityId——真实事件就是这两个字段都不带；
//   - extVersion 用小程序自身版本 2.2.8（覆盖 mpEventBase 的 2.4.0）；
//   - source=mini_program + type 固定 "send_message"。
func TestMiniExpertUseEventShape(t *testing.T) {
	ev := MiniExpertUseEvent("ex_abc", "论文写作导师", "agent")
	if ev["eventCode"] != "expert_actual_use" {
		t.Errorf("eventCode=%v want expert_actual_use", ev["eventCode"])
	}
	if ev["id"] != "ex_abc" {
		t.Errorf("id=%v want ex_abc（服务端按真实 ex_ id 校验，编造 id 不入账）", ev["id"])
	}
	if ev["expertTitle"] != "论文写作导师" || ev["expertType"] != "agent" {
		t.Errorf("专家字段: %+v", ev)
	}
	if ev["type"] != "send_message" {
		t.Errorf("type=%v want send_message（小程序恒发此值）", ev["type"])
	}
	if ev["source"] != "mini_program" {
		t.Errorf("source=%v want mini_program", ev["source"])
	}
	if ev["extVersion"] != "2.2.8" {
		t.Errorf("extVersion=%v want 2.2.8（小程序自身版本，非 mpEventBase 的 2.4.0）", ev["extVersion"])
	}
	for _, forbidden := range []string{"conversationId", "activityId"} {
		if _, ok := ev[forbidden]; ok {
			t.Errorf("不得带 %s（真实事件不带这两个字段）: %+v", forbidden, ev)
		}
	}
	// 缺省兜底：expertType 空 → agent；name 空 → 用 id。
	ev2 := MiniExpertUseEvent("ex_x", "", "")
	if ev2["expertType"] != "agent" || ev2["expertTitle"] != "ex_x" {
		t.Errorf("缺省兜底: %+v", ev2)
	}
}

// TestMiniChatModelEventShape Sequential_Tasks_5「使用 GLM5.2」判据载体：mp 对话
// 事件 + 模型字段。Tasks_1/3 的裸对话事件不带模型，模型任务须用本形态。
func TestMiniChatModelEventShape(t *testing.T) {
	ev := MiniChatModelEvent("conv-1", "glm-5.2", "GLM-5.2")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v（与 Tasks_1/3 同形状）", ev["eventCode"])
	}
	if ev["requestModelId"] != "glm-5.2" || ev["requestModelName"] != "GLM-5.2" {
		t.Errorf("模型字段缺失: %+v", ev)
	}
	if ev["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v", ev["conversationId"])
	}
	if _, ok := ev["activityId"]; ok {
		t.Errorf("Sequential 族不带 activityId（那是 school_season 的关联键）: %+v", ev)
	}
}

// TestMiniPlaybookEventsShape Sequential_Tasks_7「体验灵感功能」判据载体（mp 形态）：
// playbook_cta_click → playbook_prompt_send 两连事件，形状对齐 mpsrc 发射点。
func TestMiniPlaybookEventsShape(t *testing.T) {
	evs := MiniPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if len(evs) != 2 {
		t.Fatalf("events=%d want 2（CTA 点击 + prompt 发送）", len(evs))
	}
	if evs[0]["eventCode"] != "playbook_cta_click" {
		t.Errorf("events[0]=%v want playbook_cta_click", evs[0]["eventCode"])
	}
	if evs[1]["eventCode"] != "playbook_prompt_send" {
		t.Errorf("events[1]=%v want playbook_prompt_send", evs[1]["eventCode"])
	}
	for i, ev := range evs {
		if ev["id"] != "pm-gtm-launch-plan" || ev["name"] != "新产品上市 GTM 发布计划一页纸" {
			t.Errorf("events[%d] 案例字段缺失: %+v", i, ev)
		}
		if ev["extVersion"] != "2.2.8" {
			t.Errorf("events[%d].extVersion=%v want 2.2.8", i, ev["extVersion"])
		}
	}
	// prompt 发送必须带 conversationId（服务端关联会话），CTA 不需要。
	if cid, _ := evs[1]["conversationId"].(string); !strings.HasPrefix(cid, "wb2api-mp-pb-") {
		t.Errorf("prompt 事件 conversationId=%q（需带 wb2api-mp-pb- 前缀）", cid)
	}
}

// TestMiniEventsUseMPReportPath 三个构造器都必须能经 ReportMPEvent 出站
// （mp 指纹 + /v2/report + X-Client-Platform: mp-weixin），否则判据根本到不了上游。
func TestMiniEventsUseMPReportPath(t *testing.T) {
	var body []map[string]any
	var gotPath, gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPlatform = r.Header.Get("X-Client-Platform")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u-9", Nickname: "测试"}

	if err := c.ReportMPEvent(a, MiniExpertUseEvent("ex_1", "n", "agent")); err != nil {
		t.Fatalf("MiniExpertUseEvent 上报: %v", err)
	}
	if err := c.ReportMPEvent(a, MiniChatModelEvent("c1", "glm-5.2", "GLM-5.2")); err != nil {
		t.Fatalf("MiniChatModelEvent 上报: %v", err)
	}
	if err := c.ReportMPEvent(a, MiniPlaybookEvents("pm-1", "案例")...); err != nil {
		t.Fatalf("MiniPlaybookEvents 上报: %v", err)
	}
	if gotPath != mpReportPath {
		t.Errorf("path=%q want %q", gotPath, mpReportPath)
	}
	if gotPlatform != mpReportPlatformValue {
		t.Errorf("X-Client-Platform=%q want %q", gotPlatform, mpReportPlatformValue)
	}
	// 最后一次上报是灵感事件组（2 条），指纹必须逐事件合并（合并写漏即静默失效）。
	if len(body) != 2 {
		t.Fatalf("最后一次上报事件数=%d want 2", len(body))
	}
	for i, ev := range body {
		if ev["ideType"] != "WorkBuddy_MP" || ev["userId"] != "u-9" {
			t.Errorf("events[%d] 指纹未合并: %+v", i, ev)
		}
	}
}
