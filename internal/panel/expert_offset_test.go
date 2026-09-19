package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 专家/团队 id 按进度偏移（hub PR #21 同款修复）
//
// 根因：上游对 expert_actual_use 按 (eventCode, id) 去重（hub 2026-09 实测：
// 重复发同一个专家 id 进度永远不动；桌面端 appendGrowthEvent 同款去重）。
// 我们改动前的 runExpertBatch 恒从 MarketExpertList 返回的**同一列表**头部
// 迭代（该接口按 reco_rank 排序，顺序稳定）——半途失败重跑、或任务已部分
// 完成（如 3/5）时，已计过数的 id 被重放，进度卡住不推进。
//
// 本文件锁死修复语义：偏移量 = 任务当前进度 cur。
// ---------------------------------------------------------------------------

// expertPool 构造 n 个 id 互异的假专家（ex_1..ex_n），顺序即市场列表原序。
func expertPool(n int) []upstream.MarketExpert {
	out := make([]upstream.MarketExpert, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, upstream.MarketExpert{
			ExpertID:      fmt.Sprintf("ex_%d", i),
			ExpertType:    "agent",
			DisplayNameZH: fmt.Sprintf("专家%d", i),
			ProfessionZH:  "测试专家",
			Version:       "1.0.0",
		})
	}
	return out
}

// expertIDs 取候选里的 id 序列（断言用）。
func expertIDs(es []upstream.MarketExpert) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ExpertID)
	}
	return out
}

// TestPlanExpertBatchOffsetByProgress 进度偏移核心语义：进度 3/5 选出的 id
// 必须与进度 0 不同——这正是"半途重跑重放已计 id"的修复点。
func TestPlanExpertBatchOffsetByProgress(t *testing.T) {
	experts := expertPool(6) // ex_1..ex_6

	// 进度 0：从列表头部开始（不回归口径）。
	at0 := planExpertBatch(experts, 0, 5, 5)
	if got, want := expertIDs(at0.candidates)[:at0.need], []string{"ex_1", "ex_2", "ex_3", "ex_4", "ex_5"}; !equalStrs(got, want) {
		t.Fatalf("进度 0 候选 = %v want %v", got, want)
	}

	// 进度 3/5：本轮只需 2 次，且必须跳过前 3 个已计 id。
	at3 := planExpertBatch(experts, 3, 5, 5)
	if at3.need != 2 {
		t.Fatalf("进度 3/5 时 need = %d want 2", at3.need)
	}
	got3 := expertIDs(at3.candidates)[:at3.need]
	if want := []string{"ex_4", "ex_5"}; !equalStrs(got3, want) {
		t.Fatalf("进度 3/5 候选 = %v want %v（偏移量应取当前进度）", got3, want)
	}
	// 与"产生这 3 点进度的那批 id"必须不相交——这才是"不重放已计 id"的正解。
	// 注意不能拿进度 0 的**整轮**候选（5 个）比：列表仅 6 个时 5+2 必然有交集，
	// 那不是重放而是列表长度所限。已计窗口 = 从进度 0 起的前 cur 个。
	credited := expertIDs(at0.candidates)[:at3.cur]
	for _, id := range got3 {
		for _, old := range credited {
			if id == old {
				t.Fatalf("进度 3/5 选出的 %s 属于已计窗口 %v——已计 id 被重放", id, credited)
			}
		}
	}
}

// TestPlanExpertBatchOffsetWrapsAround 进度超过列表长度时偏移按取模回绕
// （列表比进度短时仍能选出候选，不越界）。
func TestPlanExpertBatchOffsetWrapsAround(t *testing.T) {
	experts := expertPool(3)                 // 比进度短
	pl := planExpertBatch(experts, 7, 10, 5) // 7 % 3 = 1
	if pl.need != 3 {
		t.Fatalf("need = %d want 3", pl.need)
	}
	got := expertIDs(pl.candidates)[:pl.need]
	if want := []string{"ex_2", "ex_3", "ex_1"}; !equalStrs(got, want) {
		t.Fatalf("回绕候选 = %v want %v", got, want)
	}
}

// TestPlanExpertBatchDistinctWithinRound 同一轮内选出的 id 必须互异：
// 市场列表含重复条目时只计一次（重复 id 只会白跑一次召唤 + 真实对话）。
func TestPlanExpertBatchDistinctWithinRound(t *testing.T) {
	experts := []upstream.MarketExpert{
		{ExpertID: "ex_a"}, {ExpertID: "ex_b"}, {ExpertID: "ex_a"}, // 重复
		{ExpertID: ""},                         // 空 id 脏数据
		{ExpertID: "ex_c"}, {ExpertID: "ex_b"}, // 又重复
	}
	pl := planExpertBatch(experts, 0, 5, 5)
	seen := map[string]bool{}
	for _, e := range pl.candidates {
		if e.ExpertID == "" {
			t.Fatal("候选里不得含空 id")
		}
		if seen[e.ExpertID] {
			t.Fatalf("同一轮候选出现重复 id %s", e.ExpertID)
		}
		seen[e.ExpertID] = true
	}
	if want := []string{"ex_a", "ex_b", "ex_c"}; !equalStrs(expertIDs(pl.candidates), want) {
		t.Fatalf("去重后候选 = %v want %v", expertIDs(pl.candidates), want)
	}
}

// TestPlanExpertBatchCompletedSelectsNothing 边界：进度 = 目标（已完成）
// 时不再选任何专家（不浪费真实对话配额；领奖走 claim 路径）。
func TestPlanExpertBatchCompletedSelectsNothing(t *testing.T) {
	experts := expertPool(6)
	for _, tc := range []struct{ cur, target int }{
		{5, 5}, {6, 5}, {3, 3},
	} {
		pl := planExpertBatch(experts, tc.cur, tc.target, 5)
		if pl.need != 0 || len(pl.candidates) != 0 {
			t.Errorf("进度 %d/%d 应不选候选，got need=%d candidates=%v",
				tc.cur, tc.target, pl.need, expertIDs(pl.candidates))
		}
	}
	// 空列表同样不选。
	if pl := planExpertBatch(nil, 0, 5, 5); pl.need != 0 || len(pl.candidates) != 0 {
		t.Errorf("空列表应不选候选，got need=%d candidates=%v", pl.need, expertIDs(pl.candidates))
	}
}

// TestPlanExpertBatchNoRegressionAtZero 不回归：进度 0（或读不到进度时的
// 兜底口径）候选顺序与市场列表原序逐字一致——改动前就是恒从头部迭代。
func TestPlanExpertBatchNoRegressionAtZero(t *testing.T) {
	experts := expertPool(6)
	for _, count := range []int{3, 5} {
		pl := planExpertBatch(experts, 0, count, count)
		if pl.need != count {
			t.Fatalf("count=%d 时 need = %d want %d", count, pl.need, count)
		}
		got := expertIDs(pl.candidates)[:pl.need]
		want := expertIDs(experts)[:count]
		if !equalStrs(got, want) {
			t.Fatalf("count=%d 进度 0 候选 = %v want %v（必须与改动前一致）", count, got, want)
		}
	}
	// 负进度防御：不得因取模落负下标而 panic / 选空。
	if pl := planExpertBatch(experts, -3, 5, 5); pl.need != 5 {
		t.Fatalf("负进度应夹到 0，need = %d want 5", pl.need)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 端到端：偏移量真的作用到出站事件上
// ---------------------------------------------------------------------------

// expertCapture 假上游收集到的出站事实。
type expertCapture struct {
	mu       sync.Mutex
	useIDs   []string // expert_actual_use 的 id（去重键！）
	reqIDs   []string // expert_actual_use 的 requestId（服务端签发）
	summonID []string // expert_summon_click 的 id
}

func (c *expertCapture) addUse(id, reqID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.useIDs = append(c.useIDs, id)
	c.reqIDs = append(c.reqIDs, reqID)
}

func (c *expertCapture) addSummon(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.summonID = append(c.summonID, id)
}

func (c *expertCapture) snapshot() (useIDs, reqIDs, summonIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.useIDs...), append([]string(nil), c.reqIDs...),
		append([]string(nil), c.summonID...)
}

// expertFakePanel 假上游：任务列表按 current/target 作答，市场列表返回 ids，
// /v2/report 收集事件，chat 返回带服务端 requestId 的 SSE。
func expertFakePanel(t *testing.T, ids []string, current, target int64) (*Panel, *expertCapture) {
	t.Helper()
	cap := &expertCapture{}
	var chatSeq int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks"):
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"expert_5","target":%d,"current":%d}]}}`,
				target, current)))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/market/expert/list"):
			var experts []string
			for _, id := range ids {
				experts = append(experts, fmt.Sprintf(
					`{"expert_id":"%s","expert_type":"agent","display_name_zh":"%s","version":"1.0.0"}`,
					id, id))
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[` + strings.Join(experts, ",") + `]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			var events []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&events)
			for _, ev := range events {
				code, _ := ev["eventCode"].(string)
				id, _ := ev["id"].(string)
				switch code {
				case "expert_actual_use":
					reqID, _ := ev["requestId"].(string)
					cap.addUse(id, reqID)
				case "expert_summon_click":
					cap.addSummon(id)
				}
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/chat/completions"):
			// 服务端签发 requestId（32 hex）——不是自造 UUID。
			chatSeq++
			reqID := fmt.Sprintf("%032x", chatSeq)
			_, _ = w.Write([]byte(`data: {"id":"` + reqID + `","object":"chat.completion.chunk"}` + "\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	oldGap := expertSummonGap
	expertSummonGap = time.Millisecond
	t.Cleanup(func() { expertSummonGap = oldGap })

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "tok"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up}), cap
}

// TestRunExpertBatchSkipsAlreadyCredited 核心回归：任务进度 3/5 时重跑，
// 上报的 expert_actual_use id 必须是 ex_4/ex_5——**不得**重放已计数的
// ex_1..ex_3（改动前恒从列表头部迭代，会重放这三个 id 而进度不推进）。
func TestRunExpertBatchSkipsAlreadyCredited(t *testing.T) {
	p, cap := expertFakePanel(t, []string{"ex_1", "ex_2", "ex_3", "ex_4", "ex_5", "ex_6"}, 3, 5)
	a := p.cfg.Pool.AuthByUID("u1")

	msg, err := runExpertBatch(p, a, "agent", "expert_5", 5)
	if err != nil {
		t.Fatalf("runExpertBatch: %v", err)
	}
	useIDs, _, summonIDs := cap.snapshot()
	if want := []string{"ex_4", "ex_5"}; !equalStrs(useIDs, want) {
		t.Fatalf("进度 3/5 上报的专家 id = %v want %v（msg=%s）", useIDs, want, msg)
	}
	// 召唤链必须与使用事件同一批专家（否则事件链自相矛盾）。
	if !equalStrs(summonIDs, useIDs) {
		t.Fatalf("召唤链 id = %v 与使用事件 %v 不一致", summonIDs, useIDs)
	}
}

// TestRunExpertBatchAtZeroUnchanged 不回归：进度 0 时上报列表头部 5 个专家
// ——与改动前逐字一致。
func TestRunExpertBatchAtZeroUnchanged(t *testing.T) {
	p, cap := expertFakePanel(t, []string{"ex_1", "ex_2", "ex_3", "ex_4", "ex_5", "ex_6"}, 0, 5)
	a := p.cfg.Pool.AuthByUID("u1")

	if _, err := runExpertBatch(p, a, "agent", "expert_5", 5); err != nil {
		t.Fatalf("runExpertBatch: %v", err)
	}
	useIDs, _, _ := cap.snapshot()
	if want := []string{"ex_1", "ex_2", "ex_3", "ex_4", "ex_5"}; !equalStrs(useIDs, want) {
		t.Fatalf("进度 0 上报的专家 id = %v want %v", useIDs, want)
	}
}

// TestRunExpertBatchCompletedNoCall 边界：进度已达标时不再发任何真实对话
// （省配额；领奖由调用方的 claim 路径覆盖）。
func TestRunExpertBatchCompletedNoCall(t *testing.T) {
	p, cap := expertFakePanel(t, []string{"ex_1", "ex_2", "ex_3", "ex_4", "ex_5"}, 5, 5)
	a := p.cfg.Pool.AuthByUID("u1")

	msg, err := runExpertBatch(p, a, "agent", "expert_5", 5)
	if err != nil {
		t.Fatalf("runExpertBatch: %v", err)
	}
	if useIDs, _, summonIDs := cap.snapshot(); len(useIDs) != 0 || len(summonIDs) != 0 {
		t.Fatalf("已达标仍发出调用: use=%v summon=%v", useIDs, summonIDs)
	}
	if !strings.Contains(msg, "已达标") {
		t.Errorf("已达标应如实提示，got %q", msg)
	}
}

// TestRunExpertBatchRequestIDServerIssuedDistinct 负结论（根因定位证据）：
// 去重键里的 requestId **不是**我们的问题面——它取自 chat SSE 的服务端签发值，
// 天然互异；真正会被重放的稳定字段是 id（专家 id，来自市场列表）。
// 本测试同时锁死"id 是去重键"这一修复前提，防止后续有人误改成自造 id。
func TestRunExpertBatchRequestIDServerIssuedDistinct(t *testing.T) {
	p, cap := expertFakePanel(t, []string{"ex_1", "ex_2", "ex_3"}, 0, 3)
	a := p.cfg.Pool.AuthByUID("u1")

	for round := 0; round < 2; round++ {
		if _, err := runExpertBatch(p, a, "agent", "expert_5", 3); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	useIDs, reqIDs, _ := cap.snapshot()
	if len(reqIDs) != 6 {
		t.Fatalf("两轮共应有 6 次使用事件，got %d", len(reqIDs))
	}
	// requestId 每轮互异（服务端签发）。
	seen := map[string]bool{}
	for _, id := range reqIDs {
		if id == "" {
			t.Fatal("requestId 不得为空（必须取自 chat SSE 的服务端 id）")
		}
		if seen[id] {
			t.Fatalf("requestId %s 重复——应逐次由服务端签发", id)
		}
		seen[id] = true
	}
	// 而 id 字段是稳定专家 id：进度 0 两轮重跑必然重放同一批——
	// 这正是需要按进度偏移的那一半（本轮测试用同一进度重跑以显式暴露）。
	if want := []string{"ex_1", "ex_2", "ex_3", "ex_1", "ex_2", "ex_3"}; !equalStrs(useIDs, want) {
		t.Fatalf("同进度重跑应重放同一批专家 id（去重键的一半），got %v want %v", useIDs, want)
	}
}
