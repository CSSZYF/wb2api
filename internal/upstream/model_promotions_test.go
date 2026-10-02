package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// promoEnvJSON 构造 /v3/config 响应：一个模型 + 一组 modelPromotions。
func promoEnvJSON(models, promos string) string {
	return `{"code":0,"data":{"models":[` + models + `],"modelPromotions":[` + promos + `]}}`
}

// TestPromoClockParsesHHMM promoClock 解析 "HH:MM"：正常值 → 当日分钟数；
// 坏值（缺冒号/非数字/越界）→ (-1,false)，绝不 panic 也不静默当 0 点。
func TestPromoClockParsesHHMM(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"00:00", 0, true},
		{"23:00", 23 * 60, true},
		{"7:50", 7*60 + 50, true},
		{" 08:30 ", 8*60 + 30, true},
		{"24:00", 24 * 60, true}, // 上界容差（上游可能用 24:00 表午夜）
		{"", -1, false},
		{"2300", -1, false},
		{"aa:bb", -1, false},
		{"25:00", -1, false},
		{"12:60", -1, false},
	} {
		got, ok := promoClock(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("promoClock(%q)=(%d,%v) want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPromoActiveDailyWindow 时段窗口判定：窗口内 → true，窗口外 → false；
// 跨午夜窗口（23:00→7:50）两个分支（当日 23:30 / 次日 03:00）都要算命中。
func TestPromoActiveDailyWindow(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 9, 23, h, m, 0, 0, promoZone)
	}
	night := &v3ModelPromotion{Enabled: true, Schedule: &struct {
		Daily []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"daily"`
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	}{Daily: []struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}{{Start: "23:00", End: "7:50"}}, Timezone: "Asia/Shanghai"}}

	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"窗口内（当日 23:30）", at(23, 30), true},
		{"窗口内（跨午夜后 03:00）", at(3, 0), true},
		{"边界起点 23:00", at(23, 0), true},
		{"边界终点 07:50 不含", at(7, 50), false},
		{"窗口外 12:00", at(12, 0), false},
		{"窗口外 22:59", at(22, 59), false},
	} {
		if got := promoActive(night, tc.now); got != tc.want {
			t.Errorf("%s: promoActive=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestPromoActiveDisabledAndDateRange enabled=false 一律不生效；
// validFrom/validUntil 是硬边界（未到/已过都不算）。
func TestPromoActiveDisabledAndDateRange(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, promoZone)
	off := &v3ModelPromotion{Enabled: false}
	if promoActive(off, now) {
		t.Error("enabled=false 不得生效")
	}
	// schedule 为 nil 视为全天生效（只有 enabled 把关）。
	on := &v3ModelPromotion{Enabled: true}
	if !promoActive(on, now) {
		t.Error("schedule 缺省应视为全天生效")
	}
	// 日期范围：validFrom 在未来 → 不生效；validUntil 已过 → 不生效。
	sched := func(from, until string) *v3ModelPromotion {
		p := &v3ModelPromotion{Enabled: true}
		p.Schedule = &struct {
			Daily []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"daily"`
			Timezone   string `json:"timezone"`
			ValidFrom  string `json:"validFrom"`
			ValidUntil string `json:"validUntil"`
		}{ValidFrom: from, ValidUntil: until}
		return p
	}
	if promoActive(sched("2026-10-01T00:00:00+08:00", ""), now) {
		t.Error("validFrom 未到不得生效")
	}
	if promoActive(sched("", "2026-09-01T00:00:00+08:00"), now) {
		t.Error("validUntil 已过不得生效")
	}
	if !promoActive(sched("2026-09-01T00:00:00+08:00", "2026-10-01T00:00:00+08:00"), now) {
		t.Error("日期范围内应生效")
	}
}

// TestApplyModelPromotionsPriority 同模型多条命中取 priority 最高（上游实测
// glm-5.2 白天 badge-only(50) 与夜间五折(100) 靠 priority + daily 双轨切换）；
// 目录外模型不挂（避免把别的域的同名条目点亮）。
func TestApplyModelPromotionsPriority(t *testing.T) {
	out := map[string]ModelInfo{
		"glm-5.2":    {ID: "glm-5.2", Credits: "x0.29"},
		"other":      {ID: "other", Credits: "x1"},
		"out-dir":    {ID: "out-dir"},
		"hy4-prev-f": {ID: "hy4-prev-f", Credits: "x0.29"},
	}
	promos := []v3ModelPromotion{
		{
			Enabled: true, Priority: 50, ModelIDs: []string{"glm-5.2", "not-in-dir"},
			Badge: &struct {
				Label string `json:"label"`
			}{Label: "错峰使用"},
		},
		{
			Enabled: true, Priority: 100, ModelIDs: []string{"glm-5.2"},
			Badge: &struct {
				Label string `json:"label"`
			}{Label: "夜间折扣"},
			Discount: &struct {
				DiscountedCredits string  `json:"discountedCredits"`
				Factor            float64 `json:"factor"`
			}{DiscountedCredits: "0.50x", Factor: 0.5},
			Hover: &struct {
				TextZh string `json:"textZh"`
			}{TextZh: "每日 23:00–07:50 五折"},
		},
		{
			// 已禁用：即使 priority 更高也不得生效。
			Enabled: false, Priority: 999, ModelIDs: []string{"other"},
			Badge: &struct {
				Label string `json:"label"`
			}{Label: "不该出现"},
		},
	}
	applyModelPromotions(out, promos)

	glm := out["glm-5.2"]
	if glm.PromoFactor == nil || *glm.PromoFactor != 0.5 {
		t.Errorf("glm-5.2 应取 priority=100 的折扣：factor=%v", glm.PromoFactor)
	}
	if glm.PromoCredits != "0.50x" || glm.PromoLabel != "夜间折扣" {
		t.Errorf("glm-5.2 折扣字段: credits=%q label=%q", glm.PromoCredits, glm.PromoLabel)
	}
	if glm.PromoNote != "每日 23:00–07:50 五折" {
		t.Errorf("hover 说明未透出: %q", glm.PromoNote)
	}
	if glm.Credits != "x0.29" {
		t.Errorf("牌价不得被折扣覆盖: %q", glm.Credits)
	}
	if out["other"].PromoLabel != "" {
		t.Errorf("enabled=false 的条目不得生效: %+v", out["other"])
	}
	if _, ok := out["not-in-dir"]; ok {
		t.Error("目录外模型不应被凭空创建")
	}
	// badge-only（无 discount）：只挂标签，PromoFactor 保持 nil（前端据此只显示标签）。
	applyModelPromotions(out, []v3ModelPromotion{{
		Enabled: true, Priority: 10, ModelIDs: []string{"hy4-prev-f"},
		Badge: &struct {
			Label string `json:"label"`
		}{Label: "限时免费"},
	}})
	hy := out["hy4-prev-f"]
	if hy.PromoLabel != "限时免费" || hy.PromoFactor != nil {
		t.Errorf("badge-only 条目: label=%q factor=%v（factor 必须 nil）", hy.PromoLabel, hy.PromoFactor)
	}
}

// TestFetchV3ConfigPromotionsParsed 端到端：/v3/config 的 modelPromotions 必须被
// 解析并挂到对应模型条目上（credits 是牌价，promo_* 是生效折扣——上游 2b0eedd）。
func TestFetchV3ConfigPromotionsParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(promoEnvJSON(
			`{"id":"hy4-preview-f","name":"Hy4 Preview F","maxInputTokens":200000,"credits":"x0.29"},`+
				`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"credits":"x1"}`,
			`{"enabled":true,"priority":100,"modelIds":["hy4-preview-f"],`+
				`"badge":{"label":"限时免费"},`+
				`"discount":{"discountedCredits":"0x","factor":0},`+
				`"hover":{"textZh":"限时免费至 09-30"},`+
				`"schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}`)))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	out, err := c.fetchV3ConfigModelMap(&auth.Auth{AccessToken: "at", UID: "u1"}, codeBuddyIDEUA)
	if err != nil {
		t.Fatalf("fetchV3ConfigModelMap: %v", err)
	}
	hy := out["hy4-preview-f"]
	if hy.PromoFactor == nil || *hy.PromoFactor != 0 {
		t.Fatalf("限时免费 factor 必须为 0（前端据此显示 0x 生效价）: %v", hy.PromoFactor)
	}
	if hy.PromoCredits != "0x" || hy.PromoLabel != "限时免费" || hy.PromoNote != "限时免费至 09-30" {
		t.Errorf("优惠字段: %+v", hy)
	}
	if hy.Credits != "x0.29" {
		t.Errorf("牌价必须原样保留: %q", hy.Credits)
	}
	if g := out["glm-5.2"]; g.PromoLabel != "" || g.PromoFactor != nil {
		t.Errorf("未命中优惠的模型不得挂 promo: %+v", g)
	}
}

// TestMergeModelCapabilitiesCarriesPromo v3/config 的优惠必须能穿过能力覆盖
// 合并（CN/global 目录以企业端点结果为基底、v3/config 为 overlay；不搬运
// promo_* 的话面板倍率列永远看不到生效价）。
func TestMergeModelCapabilitiesCarriesPromo(t *testing.T) {
	f := 0.5
	base := []ModelInfo{{ID: "glm-5.2", Credits: "x1", ContextWindow: 100}}
	overlay := map[string]ModelInfo{
		"glm-5.2": {ID: "glm-5.2", ContextWindow: 200, PromoFactor: &f,
			PromoCredits: "0.50x", PromoLabel: "夜间折扣", PromoNote: "23:00–07:50"},
	}
	got := MergeCatalogOverlay(base, overlay, nil)
	if got[0].PromoFactor == nil || *got[0].PromoFactor != 0.5 {
		t.Fatalf("promo_factor 未搬运: %+v", got[0])
	}
	if got[0].PromoCredits != "0.50x" || got[0].PromoLabel != "夜间折扣" || got[0].PromoNote != "23:00–07:50" {
		t.Errorf("promo 字段未搬运: %+v", got[0])
	}
	if got[0].ContextWindow != 200 {
		t.Errorf("既有能力覆盖行为被破坏: %+v", got[0])
	}
	// overlay 无 promo 时不得把基底已有的 promo 抹掉（只填有值字段的语义）。
	base2 := []ModelInfo{{ID: "m", PromoLabel: "保留"}}
	got2 := MergeCatalogOverlay(base2, map[string]ModelInfo{"m": {ID: "m", ContextWindow: 5}}, nil)
	if got2[0].PromoLabel != "保留" {
		t.Errorf("overlay 无 promo 时不应清空基底 promo: %+v", got2[0])
	}
}

// TestPromoFieldsJSONShape promo_* 的 JSON 形状必须稳定（面板 app.js 按这些键渲染）：
// promo_factor 是数值（含 0），promo_credits/label/note 是字符串。
func TestPromoFieldsJSONShape(t *testing.T) {
	f := 0.0
	mi := ModelInfo{ID: "m", PromoFactor: &f, PromoCredits: "0x", PromoLabel: "限时免费", PromoNote: "至 09-30"}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"PromoFactor": 0.0, "PromoCredits": "0x", "PromoLabel": "限时免费", "PromoNote": "至 09-30"} {
		if got[k] != want {
			t.Errorf("%s=%v want %v", k, got[k], want)
		}
	}
}
