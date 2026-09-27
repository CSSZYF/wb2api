// account_order_test.go 面板账号顺序端点（POST /panel/api/accounts/order）契约锁定。
//
// 这是顺序填充式选号（pool.pick_mode=sequential）的**唯一顺序写入口**，面板拖拽排序
// 落到它上面；另一代理的前端按此契约对接（请求 {"uids":[...]}、响应 {"ok":true,...}）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// orderPool 三个账号的面板（u1/u2/u3，UID 升序与期望顺序不同以区分）。
func orderPool(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
	}
	return New(Config{Version: "test", APIKey: "test-key", Pool: p}), p
}

// TestAccountOrderSetsOrder 正向：提交顺序 → 池顺序与 List() 同步改变。
func TestAccountOrderSetsOrder(t *testing.T) {
	pn, p := orderPool(t)
	rec := postPanel(t, pn, "/panel/api/accounts/order", `{"uids":["u3","u1","u2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Errorf("ok=%v want true", resp["ok"])
	}
	// 响应回显有效顺序（前端据此立即重排，不必等下一次 overview）。
	order, _ := resp["order"].([]any)
	if len(order) != 3 || order[0] != "u3" || order[1] != "u1" || order[2] != "u2" {
		t.Errorf("响应 order=%v want [u3 u1 u2]", resp["order"])
	}
	// 池侧生效：Order() 与 List()（面板列表渲染源）同序。
	if got := p.Order(); len(got) != 3 || got[0] != "u3" {
		t.Errorf("Pool.Order()=%v want [u3 u1 u2]", got)
	}
	list := p.List()
	if list[0].UID != "u3" || list[1].UID != "u1" || list[2].UID != "u2" {
		t.Errorf("List() 应按新顺序输出，got [%s %s %s]", list[0].UID, list[1].UID, list[2].UID)
	}
}

// TestAccountOrderEmptyClears 空数组 = 清除自定义顺序（回落 UID 排序）。
func TestAccountOrderEmptyClears(t *testing.T) {
	pn, p := orderPool(t)
	postPanel(t, pn, "/panel/api/accounts/order", `{"uids":["u3","u1","u2"]}`)
	rec := postPanel(t, pn, "/panel/api/accounts/order", `{"uids":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := p.Order(); len(got) != 3 || got[0] != "u1" || got[2] != "u3" {
		t.Errorf("清除顺序后 Order()=%v want UID 排序 [u1 u2 u3]", got)
	}
	// 空体（无 uids 字段）同样按清除处理——前端"重置顺序"按钮的最简形态。
	postPanel(t, pn, "/panel/api/accounts/order", `{"uids":["u3","u1","u2"]}`)
	rec = postPanel(t, pn, "/panel/api/accounts/order", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("空体 code=%d body=%s（空体应视为清除顺序）", rec.Code, rec.Body.String())
	}
	if got := p.Order(); got[0] != "u1" {
		t.Errorf("空体后 Order()=%v want UID 排序（首个 u1）", got)
	}
}

// TestAccountOrderFiltersUnknownUIDs 未知 uid：过滤 + 警告回显（不整单拒绝）。
// 取舍见 panel.go accountOrder 注释：拖拽是一次性 UI 动作，整单 400 会让用户
// "拖了没反应"且无法自愈；过滤后写入有效次序 + 明确回显被忽略的 uid，数据不静默丢失。
func TestAccountOrderFiltersUnknownUIDs(t *testing.T) {
	pn, p := orderPool(t)
	rec := postPanel(t, pn, "/panel/api/accounts/order", `{"uids":["u3","ghost","u1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（未知 uid 不应整单拒绝）", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["warning"] == nil {
		t.Errorf("未知 uid 必须回 warning，got %v", resp)
	}
	unknown, _ := resp["unknown_uids"].([]any)
	if len(unknown) != 1 || unknown[0] != "ghost" {
		t.Errorf("unknown_uids=%v want [ghost]", resp["unknown_uids"])
	}
	// 有效 uid 按原相对次序写入；池内未列出的 u2 追加末尾。
	if got := p.Order(); len(got) != 3 || got[0] != "u3" || got[1] != "u1" || got[2] != "u2" {
		t.Errorf("Order()=%v want [u3 u1 u2]（过滤 ghost + u2 追加末尾）", got)
	}
}

// TestAccountOrderRejectsMalformedBody 非法 JSON（非空体）→ 400（与空体区分）。
func TestAccountOrderRejectsMalformedBody(t *testing.T) {
	pn, _ := orderPool(t)
	rec := postPanel(t, pn, "/panel/api/accounts/order", `{"uids":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

// TestAccountOrderPersistsToStateFile 顺序立即落盘（重启后仍在）。
func TestAccountOrderPersistsToStateFile(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := pool.New(fp)
	for _, uid := range []string{"u1", "u2"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
	}
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p})
	rec := postPanel(t, pn, "/panel/api/accounts/order", `{"uids":["u2","u1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	p.Close() // 停 flusher + 最后落盘

	p2 := pool.New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p2.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	if got := p2.Order(); len(got) != 2 || got[0] != "u2" {
		t.Errorf("重启后 Order()=%v want [u2 u1]（顺序未落盘）", got)
	}
}

// TestAccountOrderRequiresAuth 端点走 withAuth（与其余面板写操作同口径）：
// 无 Authorization 头 → 401，且**不改动池内顺序**。
func TestAccountOrderRequiresAuth(t *testing.T) {
	pn, p := orderPool(t)
	postPanel(t, pn, "/panel/api/accounts/order", `{"uids":["u3","u1","u2"]}`)
	req := httptest.NewRequest("POST", "/panel/api/accounts/order", strings.NewReader(`{"uids":["u1","u2","u3"]}`))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req) // 不带 Authorization
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401, code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := p.Order(); got[0] != "u3" {
		t.Errorf("401 请求不得改动顺序, Order()=%v want [u3 u1 u2]", got)
	}
}
