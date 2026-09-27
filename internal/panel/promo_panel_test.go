package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsExposesPromo 端到端（吸收上游 2b0eedd）：/panel/api/models 必须把
// 生效优惠透出给前端——promo_factor（数值，含 0=限时免费）/promo_credits/
// promo_label/promo_note。无优惠的模型不得带这些键（缺失 ≠ 免费，前端不回填）。
func TestPanelModelsExposesPromo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy4-preview-f","name":"Hy4","maxInputTokens":200000,"credits":"x0.29"},
				{"id":"glm-5.2","name":"GLM","maxInputTokens":200000,"credits":"x1"}
			],"agents":[{"name":"cli","models":["hy4-preview-f","glm-5.2"]}]}}`))
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy4-preview-f","name":"Hy4","maxInputTokens":200000,"credits":"x0.29"}
			],"modelPromotions":[
				{"enabled":true,"priority":100,"modelIds":["hy4-preview-f"],
				 "badge":{"label":"限时免费"},
				 "discount":{"discountedCredits":"0x","factor":0},
				 "hover":{"textZh":"限时免费至 09-30"}}
			]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up})

	req := httptest.NewRequest("GET", "/panel/api/models?realm=cn", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, m := range got.Models {
		id, _ := m["id"].(string)
		byID[id] = m
	}
	hy := byID["hy4-preview-f"]
	if hy == nil {
		t.Fatalf("缺 hy4-preview-f：%v", got.Models)
	}
	if f, ok := hy["promo_factor"].(float64); !ok || f != 0 {
		t.Errorf("promo_factor=%v want 0（限时免费必须下发数值 0，不能被 omitempty 吃掉）", hy["promo_factor"])
	}
	if hy["promo_credits"] != "0x" || hy["promo_label"] != "限时免费" || hy["promo_note"] != "限时免费至 09-30" {
		t.Errorf("promo 字段不全: %+v", hy)
	}
	if hy["credits"] != "x0.29" {
		t.Errorf("牌价必须原样透出（前端要显示划线牌价）: %v", hy["credits"])
	}
	glm := byID["glm-5.2"]
	for _, k := range []string{"promo_factor", "promo_credits", "promo_label", "promo_note"} {
		if _, ok := glm[k]; ok {
			t.Errorf("无优惠的模型不得带 %s: %+v", k, glm)
		}
	}
}

// TestAppJSRateCellPromoRender 倍率列三态渲染（纯 JS，用 node 实跑函数体）：
//
//	有折扣 → 生效价大字 + 标签 + 划线牌价；
//	仅标签（错峰类，无 factor）→ 牌价 + 标签；
//	无优惠 → 原样牌价（既有行为零回归）。
//
// 无 node 环境时跳过（同 frontend_test.go 既有做法）。
func TestAppJSRateCellPromoRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS logic check")
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := jsFuncFull(string(src), "function rateCell")
	if fn == "" {
		t.Fatal("app.js 里找不到 rateCell（倍率列渲染函数被删/改名？）")
	}
	script := `
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
` + fn + `
const out = {
  free: rateCell({ credits: 'x0.29', promo_factor: 0, promo_credits: '0x', promo_label: '限时免费', promo_note: '限时免费至 09-30' }),
  half: rateCell({ credits: 'x1', promo_factor: 0.5, promo_credits: '0.50x', promo_label: '夜间折扣' }),
  badgeOnly: rateCell({ credits: 'x0.79', promo_label: '错峰使用', promo_note: '每日 23:00–07:50' }),
  plain: rateCell({ credits: 'x1' }),
  none: rateCell({}),
};
process.stdout.write(JSON.stringify(out));
`
	fp := filepath.Join(t.TempDir(), "ratecell.js")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp).CombinedOutput()
	if err != nil {
		t.Fatalf("node 运行失败: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出解析失败: %v\n%s", err, out)
	}
	// 有折扣：生效价（promo_credits）在前、划线牌价（<s>）在后、标签存在。
	if !strings.Contains(got["free"], "<b>0x</b>") {
		t.Errorf("限时免费应显示生效价 0x 为正文: %s", got["free"])
	}
	if !strings.Contains(got["free"], "<s style=") || !strings.Contains(got["free"], "x0.29") {
		t.Errorf("有折扣时必须划线显示牌价: %s", got["free"])
	}
	if !strings.Contains(got["free"], "限时免费") {
		t.Errorf("标签缺失: %s", got["free"])
	}
	if !strings.Contains(got["free"], `title="限时免费至 09-30"`) {
		t.Errorf("时段说明应挂 title（悬停看时段）: %s", got["free"])
	}
	if !strings.Contains(got["half"], "<b>0.50x</b>") || !strings.Contains(got["half"], "夜间折扣") {
		t.Errorf("五折应显示生效价 + 标签: %s", got["half"])
	}
	// 仅标签（错峰类）：牌价照旧显示，标签是 warn 色，且**不得**出现划线牌价。
	if !strings.Contains(got["badgeOnly"], "x0.79") || !strings.Contains(got["badgeOnly"], "错峰使用") {
		t.Errorf("badge-only 应显示牌价 + 标签: %s", got["badgeOnly"])
	}
	if strings.Contains(got["badgeOnly"], "<s style=") {
		t.Errorf("badge-only 无 factor，不得渲染划线牌价: %s", got["badgeOnly"])
	}
	if !strings.Contains(got["badgeOnly"], `title="每日 23:00–07:50"`) {
		t.Errorf("badge-only 的时段说明也应挂 title: %s", got["badgeOnly"])
	}
	// 无优惠：原样牌价（零回归）；无 credits：破折号。
	if got["plain"] != "x1" {
		t.Errorf("无优惠应原样显示牌价: %q", got["plain"])
	}
	if got["none"] != "—" {
		t.Errorf("无 credits 应显示 —: %q", got["none"])
	}
}

// TestAppJSRateCellWiring 倍率列接线完整性：loadModels 必须走 rateCell
// （否则 promo_* 白透出——面板仍显示牌价，与客户端不一致）。
func TestAppJSRateCellWiring(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, must := range []string{"function rateCell", "+ rateCell(m) +", "promo_factor", "promo_credits", "promo_label", "promo_note"} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺倍率列优惠接线：%s", must)
		}
	}
}
