package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestMPEventBase(t *testing.T) {
	a := &auth.Auth{UID: "u-1", Nickname: "测试"}
	base := mpEventBase(a)
	for _, k := range []string{"ideType", "extName", "ideName", "platform", "userId"} {
		if _, ok := base[k]; !ok {
			t.Errorf("missing common field %s", k)
		}
	}
	if base["ideType"] != "WorkBuddy_MP" || base["extName"] != "workbuddy-mp" {
		t.Errorf("fingerprint ideType=%v extName=%v", base["ideType"], base["extName"])
	}
}

func TestSchoolChatTimesEvents(t *testing.T) {
	ev := SchoolChatTimesEvents("conv-1")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["conversationId"] != "conv-1" || ev["codebuddy.session_id"] != "conv-1" {
		t.Errorf("conversation join fields missing: %v", ev)
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), "agentName") {
		t.Error("agentName missing")
	}
}

func TestSchoolExpertUseEvents(t *testing.T) {
	events := SchoolExpertUseEvents("ex_x", "论文写作导师", "conv-2")
	if len(events) != 4 {
		t.Fatalf("events=%d want 4", len(events))
	}
	wantCodes := []string{"expert_summon_click", "expert_summoned", "expert_actual_use", "chat_request_send"}
	for i, code := range wantCodes {
		if events[i]["eventCode"] != code {
			t.Errorf("events[%d].eventCode=%v want %s", i, events[i]["eventCode"], code)
		}
	}
	if events[0]["id"] != "ex_x" || events[0]["expertTitle"] != "论文写作导师" {
		t.Errorf("expert fields: %v", events[0])
	}
	if events[3]["expertId"] != "ex_x" {
		t.Errorf("chat expertId=%v", events[3]["expertId"])
	}
}

// TestSchoolVouchers 券码查询：GET /portal/activity/school/vouchers（只读，
// 账号 Bearer + X-User-Id，无需 web cookie），data.items[] 逐字段解析。
// 空券列表（未抽中）必须是"零券无错"，不能误报成查询失败。
func TestSchoolVouchers(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"items":[
			{"grant_id":12345,"draw_uuid":"d-1","sku_code":"kfc_ice_cream",
			 "prize_name":"肯德基冰淇淋","code":"KFC20260916ABC",
			 "valid_to":"2026-10-24","granted_at":"2026-09-16T11:20:00+08:00"},
			{"grant_id":12346,"sku_code":"voucher_luckin","prize_name":"瑞幸咖啡券",
			 "code":"LUCKIN-8888","valid_to":"2026-10-01"}
		]}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	vs, err := c.SchoolVouchers(&auth.Auth{AccessToken: "at", UID: "u-9"})
	if err != nil {
		t.Fatalf("SchoolVouchers: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method=%s want GET（只读查询）", gotMethod)
	}
	if want := "/portal/activity/school/vouchers"; gotPath != want {
		t.Errorf("path=%q want %q", gotPath, want)
	}
	if gotAuth != "Bearer at" || gotUID != "u-9" {
		t.Errorf("auth=%q uid=%q（应与任务族同鉴权：账号 Bearer + X-User-Id）", gotAuth, gotUID)
	}
	if len(vs) != 2 {
		t.Fatalf("vouchers=%d want 2", len(vs))
	}
	v := vs[0]
	if v.GrantID != 12345 || v.SKUCode != "kfc_ice_cream" || v.PrizeName != "肯德基冰淇淋" {
		t.Errorf("voucher[0] 标识字段: %+v", v)
	}
	if v.Code != "KFC20260916ABC" {
		t.Errorf("code=%q（券码本体，前端复制/二维码都依赖它）", v.Code)
	}
	if v.ValidTo != "2026-10-24" || v.GrantedAt != "2026-09-16T11:20:00+08:00" {
		t.Errorf("有效期/抽中时间: %+v", v)
	}
	if vs[1].DrawUUID != "" || vs[1].ValidFrom != "" {
		t.Errorf("缺省字段应为空（omitempty 语义）: %+v", vs[1])
	}
}

// TestSchoolVouchersEmpty 未抽中：items 为空数组 → 返回空列表且无错误
// （面板侧据此汇总"暂无券"，不能当成查询失败标红）。
func TestSchoolVouchersEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"items":[]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	vs, err := c.SchoolVouchers(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("空券列表不应报错: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("vouchers=%d want 0", len(vs))
	}
}

// TestSchoolVouchersBizError 上游业务错误（信封 code≠0）必须透出为 error，
// 面板才能在对应账号行标红而不是静默显示"无券"。
func TestSchoolVouchersBizError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":40001,"msg":"activity not started","data":null}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	if _, err := c.SchoolVouchers(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("业务错误应返回 error")
	} else if !strings.Contains(err.Error(), "activity not started") {
		t.Errorf("error 应带上游 msg，got %v", err)
	}
}
