package scheduler

// cooldownprobe_test.go 后台冷却探活（scheduler 侧）：请求构造 / 成功解冻 /
// **失败零惩罚**（本特性的第一原则）/ 在途租约 / 串行 / 重入锁 / 间隔热改 / 日志形状。
//
// 零惩罚为什么必须单独测：探活是后台自发流量，一旦失败路径挂了惩罚（推进
// softStreak、延长冷却、喂熔断），探活就把账号越探越死——旧版软冷却的
// "越重试越冷、全池被推到 2h 封顶"正是这个失效模式。断言口径取「冷却截止 /
// softStreak / 熔断计数 / 熔断截止在探活前后逐字段不变」，比"看日志有没有报错"强得多。
//
// 所有"已到期"的构造都走生产入口 + 过去的重置墙钟：CooldownSoftRate /
// CooldownSoftForModel 对已过去的重置时刻落 until = now+1ms（cappedSoftUntilLocked），
// 故 sleep 一小段即自然到期——不新增任何 test-only 的池内直改入口。

import (
	"context"
	"encoding/json"
	"io"
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

// probeExpirySettle 等「过去的重置墙钟」写法落到 until 已过（until = now+1ms）。
// 5ms 远大于 1ms，且不引入真实长 sleep。
const probeExpirySettle = 5 * time.Millisecond

// probeStub 假上游：按配置的状态码/行为回应 chat 请求，并记录收到的出站 body
// 与请求次数（供"探活请求是不是最小请求"与"串行/次数"断言）。
type probeStub struct {
	calls atomic.Int32
	// body 最后一次收到的出站 chat body（解析为 map 后断言）。
	body atomic.Value
	// handler 可注入自定义行为（nil 时按 status 回 SSE 流或错误体）。
	handler func(w http.ResponseWriter, r *http.Request)
	status  int
}

// sseOK 写一条最小合法 SSE 流（含 [DONE]，Aggregate 能正常收尾）。
func (s *probeStub) sseOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	frame, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-probe", "object": "chat.completion.chunk", "model": "glm-5.2",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "2"}}},
	})
	_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (s *probeStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.calls.Add(1)
		if raw, err := io.ReadAll(r.Body); err == nil {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				s.body.Store(m)
			}
		}
		if s.handler != nil {
			s.handler(w, r)
			return
		}
		if s.status >= 400 {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(`{"code":429,"msg":"rate limited"}`))
			return
		}
		s.sseOK(w)
	}))
}

// probeScheduler 组装探活测试用调度器：单账号 u1（CN 域，chatPaths 只有
// /v2/chat/completions 一条），上游指向假服务器，账号间保险间隔压到最小。
func probeScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.cooldownProbeGap = time.Millisecond // 测试不必为账号间保险间隔白等
	return s, p
}

// expireModelCooldown 给账号写一条**已到期**的模型级冷却条目（走生产入口：
// 过去的重置墙钟 → until = now+1ms，settle 后即到期）。
func expireModelCooldown(t *testing.T, p *pool.Pool, uid, model string) {
	t.Helper()
	p.CooldownSoftForModel(uid, time.Minute, time.Now().Add(-time.Hour), model, "6004 model rate limit")
	time.Sleep(probeExpirySettle)
}

// expireAccountCooldown 给账号写一条**已到期**的账号级软冷却（同上路径）。
func expireAccountCooldown(t *testing.T, p *pool.Pool, uid string) {
	t.Helper()
	p.CooldownSoftRate(uid, time.Hour, time.Now().Add(-time.Hour), "429 rate limit")
	time.Sleep(probeExpirySettle)
}

// probeState 账号惩罚态快照（零惩罚断言用）。只经 Status 的公开字段取值，
// 不依赖任何 test-only 出口。
type probeState struct {
	until        time.Time
	coolKind     string
	reason       string
	softStreak   int
	fails        int
	breakerUntil time.Time
	errTotal     int64
	// modelUntil 指定模型的冷却截止（未到期时才出现在 Status.RateLimitedModels
	// 台账里；已到期条目本就不在台账中，故对"已到期"场景恒为零值——那边改用
	// probeTargetCount 断言条目存在）。
	modelUntil time.Time
}

// probeSnapshot 读账号状态（model 非空时一并取该模型的冷却截止台账）。
func probeSnapshot(t *testing.T, p *pool.Pool, uid, model string) probeState {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("账号 %s 不在池内", uid)
	}
	snap := probeState{
		until:        st.Until,
		coolKind:     st.CoolKind,
		reason:       st.Reason,
		softStreak:   st.SoftStreak,
		fails:        st.BreakerFails,
		breakerUntil: st.BreakerUntil,
		errTotal:     st.ErrTotal,
	}
	for _, m := range st.RateLimitedModels {
		if m.Model == model {
			snap.modelUntil = m.Until
		}
	}
	return snap
}

// probeTargetCount 返回当前待探目标数（生产入口 CooldownProbeTargets 的读值）。
// 已到期的条目不在 Status.RateLimitedModels 台账里（台账只列未到期），故"条目确实
// 存在"这件事必须用它来断言。
func probeTargetCount(t *testing.T, p *pool.Pool) int {
	t.Helper()
	return len(p.CooldownProbeTargets(time.Now()))
}

// assertZeroPenalty 逐字段断言"探活前后惩罚态不变"（本特性的第一原则）。
// 三件套：冷却截止 / softStreak（软退避）/ 熔断计数与截止（fails/breakerUntil）。
func assertZeroPenalty(t *testing.T, before, after probeState) {
	t.Helper()
	if !after.until.Equal(before.until) {
		t.Errorf("冷却截止被改动: %v -> %v（探活失败必须零惩罚）", before.until, after.until)
	}
	if after.softStreak != before.softStreak {
		t.Errorf("softStreak 被推进: %d -> %d（探活失败不得推进软退避）", before.softStreak, after.softStreak)
	}
	if after.fails != before.fails {
		t.Errorf("熔断失败计数被喂入: %d -> %d（探活失败不得喂熔断）", before.fails, after.fails)
	}
	if !after.breakerUntil.Equal(before.breakerUntil) {
		t.Errorf("熔断截止被改动: %v -> %v", before.breakerUntil, after.breakerUntil)
	}
	if after.coolKind != before.coolKind || after.reason != before.reason {
		t.Errorf("冷却类别/原因被改动: %v/%q -> %v/%q",
			before.coolKind, before.reason, after.coolKind, after.reason)
	}
	if !after.modelUntil.Equal(before.modelUntil) {
		t.Errorf("模型级冷却截止被改动: %v -> %v（不得惩罚性延长）", before.modelUntil, after.modelUntil)
	}
	if after.errTotal != before.errTotal {
		t.Errorf("errTotal 被累加: %d -> %d（探活不是用户流量，不记账）", before.errTotal, after.errTotal)
	}
}

// TestCooldownProbeSendsMinimalRequest 探活请求必须是最小 chat 请求：单条 user 消息、
// max_tokens 极小、model 为目标模型。出站 body 经 ChatStreamContext（prepareBody：
// 脱敏/指纹/prompt_cache_key 全套）——本用例同时钉住"走既有出站路径"。
func TestCooldownProbeSendsMinimalRequest(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	expireModelCooldown(t, p, "u1", "glm-5.3")

	res := s.RunCooldownProbeNow()
	if res.Probed != 1 {
		t.Fatalf("Probed=%d want 1（res=%+v）", res.Probed, res)
	}
	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("上游调用数=%d want 1", n)
	}
	raw, _ := stub.body.Load().(map[string]any)
	if raw == nil {
		t.Fatal("未捕获到出站 body")
	}
	if got, _ := raw["model"].(string); got != "glm-5.3" {
		t.Errorf("出站 model=%q want glm-5.3（探活必须打目标模型）", got)
	}
	msgs, _ := raw["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v want 单条（最小请求）", raw["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	if role, _ := m0["role"].(string); role != "user" {
		t.Errorf("messages[0].role=%q want user", role)
	}
	mt, ok := raw["max_tokens"].(float64)
	if !ok {
		t.Fatalf("max_tokens 缺失或非数字: %v", raw["max_tokens"])
	}
	if int(mt) != cooldownProbeMaxTokens {
		t.Errorf("max_tokens=%v want %d（极小值，探活不要实际输出）", mt, cooldownProbeMaxTokens)
	}
	if int(mt) > 8 {
		t.Errorf("max_tokens=%v 过大（want <= 8，积分消耗须可忽略）", mt)
	}
	// prompt_cache_key 是 prepareBody 的产物：它出现即证明请求确实走了既有出站路径
	// （未经 prepareBody 的裸请求不会有该字段）。
	if _, ok := raw["prompt_cache_key"]; !ok {
		t.Error("出站 body 缺 prompt_cache_key：探活可能绕过了 prepareBody（既有出站路径）")
	}
}

// TestCooldownProbeZeroPenaltyOnUpstreamRejection **本特性的第一原则**：探活失败
// （上游明确拒绝）绝不罚号——冷却截止、softStreak、熔断三件套逐字段不变。
//
// 四种拒绝形态一并覆盖：429 软限流（分类信封）、5xx（ErrServer，唯一的熔断入口
// 候选）、403 空体（WAF 拦截形态）、11102（模型级负缓存语义）。
func TestCooldownProbeZeroPenaltyOnUpstreamRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"429 仍限流", 429, `{"code":6004,"msg":"The model provider is rate-limiting requests."}`},
		{"500 上游故障", 500, `{"code":500,"msg":"internal error"}`},
		{"403 空体 WAF", 403, ""},
		{"404 模型不存在", 404, `{"code":11102,"msg":"service info not found"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &probeStub{status: c.status}
			if c.body == "" {
				stub.handler = func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(c.status) // 空体 = WAF 拦截形态
				}
			}
			srv := stub.server(t)
			defer srv.Close()
			s, p := probeScheduler(t, srv)

			// 账号级软冷却已到期 + 一个模型级到期条目（两类目标都会被探）。
			expireAccountCooldown(t, p, "u1")
			expireModelCooldown(t, p, "u1", "glm-5.3")

			before := probeSnapshot(t, p, "u1", "glm-5.3")
			res := s.RunCooldownProbeNow()
			after := probeSnapshot(t, p, "u1", "glm-5.3")

			if res.Probed == 0 {
				t.Fatal("Probed=0：前置条件失败（应有到期目标被探）")
			}
			assertZeroPenalty(t, before, after)
			if res.Cleared != 0 || res.Accounts != 0 {
				t.Errorf("失败轮不应有解冻: cleared=%d accounts=%d", res.Cleared, res.Accounts)
			}
		})
	}
}

// TestCooldownProbeZeroPenaltyOnTransportFailure 传输层失败（上游连接被拒，
// 无任何可判读响应）同样零惩罚。与上表的区别：上表是"上游明确拒绝"，这里是
// "没拿到响应"——两条路径在 probeOne 里是不同分支，必须各自覆盖。
func TestCooldownProbeZeroPenaltyOnTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s, p := probeScheduler(t, srv)
	srv.Close() // 立刻关闭：后续请求必然传输层失败

	expireAccountCooldown(t, p, "u1")
	before := probeSnapshot(t, p, "u1", "")
	res := s.RunCooldownProbeNow()
	after := probeSnapshot(t, p, "u1", "")

	if res.Probed == 0 {
		t.Fatal("Probed=0：应有到期目标被探（前置条件失败）")
	}
	if res.Failed == 0 {
		t.Errorf("Failed=0 want >0（传输层失败应计入 Failed；res=%+v）", res)
	}
	assertZeroPenalty(t, before, after)
}

// TestCooldownProbeSuccessClearsCooldowns 探活成功 → 模型级条目与账号级软冷却一并
// 清（上游提前恢复，账号立刻回到可选池）。
func TestCooldownProbeSuccessClearsCooldowns(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	expireAccountCooldown(t, p, "u1")
	expireModelCooldown(t, p, "u1", "glm-5.3")
	if n := probeTargetCount(t, p); n == 0 {
		t.Fatal("前置条件失败：应有到期的探活目标")
	}

	res := s.RunCooldownProbeNow()
	if res.Cleared != 1 || res.Accounts != 1 {
		t.Fatalf("res=%+v want Cleared=1 Accounts=1", res)
	}
	after := probeSnapshot(t, p, "u1", "glm-5.3")
	if !after.until.IsZero() || after.coolKind != "" || after.softStreak != 0 {
		t.Errorf("账号级软冷却未清: until=%v coolKind=%q softStreak=%d", after.until, after.coolKind, after.softStreak)
	}
	if !after.modelUntil.IsZero() {
		t.Errorf("模型级条目未清: %v", after.modelUntil)
	}
	// 解冻后账号立即可选（模型豁免与账号级冷却都不再拦截）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Errorf("解冻后账号应可被选中，got %+v", got)
	}
}

// TestCooldownProbeSuccessModelOnly 只有模型级条目到期（账号级未冷却）时，成功只清
// 模型级条目，账号级字段不被牵连；探活模型也确是该条目对应的模型。
func TestCooldownProbeSuccessModelOnly(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	expireModelCooldown(t, p, "u1", "deepseek-v4.1-flash")
	res := s.RunCooldownProbeNow()
	if res.Cleared != 1 || res.Accounts != 0 {
		t.Fatalf("res=%+v want Cleared=1 Accounts=0（账号级未冷却）", res)
	}
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("模型级台账应已清空: %+v", st.RateLimitedModels)
	}
	raw, _ := stub.body.Load().(map[string]any)
	if got, _ := raw["model"].(string); got != "deepseek-v4.1-flash" {
		t.Errorf("出站 model=%q want deepseek-v4.1-flash（目标模型必须原样出站）", got)
	}
}

// TestCooldownProbeNoTargetsNoUpstreamCall 无到期目标时整轮零出站流量
// （这是"默认开启"的成本前提：无到期冷却的池不该被探活打出任何请求）。
func TestCooldownProbeNoTargetsNoUpstreamCall(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	// 三种"不该被探"的形态全占：
	//  1) 账号级软冷却**未到期**（未来重置墙钟）；
	//  2) 模型级条目**未到期**；
	//  3) 硬冷却（CoolHard：积分耗尽，恢复条件是签到到账）。
	p.CooldownSoftRate("u1", time.Hour, time.Now().Add(time.Hour), "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	p.CooldownUntilTomorrow4AM("u1", "余额不足")

	res := s.RunCooldownProbeNow()
	if res.Probed != 0 {
		t.Errorf("Probed=%d want 0（未到期/硬冷却都不该探；res=%+v）", res.Probed, res)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上游调用数=%d want 0（无到期目标时必须零出站流量）", n)
	}
}

// TestCooldownProbeNeverProbesHardCooldown 硬冷却（CoolHard）单独一测：即便 until
// 已过（签到还没跑到），也不得探——积分耗尽的恢复条件是签到到账，探了必失败还
// 白花一次上游配额。这是目标选择里唯一"按类别排除"的分支，必须显式钉住。
func TestCooldownProbeNeverProbesHardCooldown(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	// 硬冷却：Cooldown(CoolHard, 负时长) 让 until 落在过去，模拟"到期但等签到"。
	p.Cooldown("u1", pool.CoolHard, -time.Hour, "余额不足")

	res := s.RunCooldownProbeNow()
	if res.Probed != 0 {
		t.Errorf("Probed=%d want 0（硬冷却绝不探，即便 until 已过）", res.Probed)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上游调用数=%d want 0（硬冷却不得被探活）", n)
	}
	// 也不得因探活而被"顺手解冻"：硬冷却的恢复条件是签到到账，探活不构成解冻依据。
	// 注意 Status.CoolKind 只在 Cooling=true 时透出（until 已过 → 不透出），故这里
	// 用"目标数仍为 0"作判据——硬冷却账号在探活眼里始终不可探。
	if n := probeTargetCount(t, p); n != 0 {
		t.Errorf("硬冷却账号出现在探活目标里（目标数=%d want 0）", n)
	}
}

// TestCooldownProbeReconfigurePokes 已删除：Reconfigure 刻意**不** poke 探活循环。
// 原因（设计决定）：rearm 的语义是"重算下一次唤醒"（continue → 按新间隔重开 timer），
// 不是"立刻跑一轮"。若让每次保存配置都 poke，长间隔（默认 10 分钟）的探活会被
// 反复推后——面板保存一次配置就白等一个周期。间隔/开关热改由
// SetCooldownProbeInterval 自己 poke（saveConfig 紧随 Reconfigure 调用它），
// 链路完整且不引入该副作用。热改生效路径见 TestCooldownProbeIntervalHotReload。保留此注释防后人"顺手"补上。

// TestCooldownProbeHoldsInflightLease 探活请求必须持账号在途租约：在途上限=1 且
// 名额已被占满时，本轮探活跳过（不与用户请求抢配额），不发起任何上游调用。
func TestCooldownProbeHoldsInflightLease(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	p.SetMaxInFlight(1)
	expireModelCooldown(t, p, "u1", "glm-5.3")
	// 占满在途名额（模拟一个在途用户请求）。
	if !p.Acquire("u1") {
		t.Fatal("前置条件失败：应能拿到名额")
	}

	res := s.RunCooldownProbeNow()
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("在途占满时上游调用数=%d want 0（探活不得与用户请求抢配额）", n)
	}
	if res.Probed != 0 {
		t.Errorf("Probed=%d want 0（未发请求就不该计入探活数）", res.Probed)
	}
	if res.SkippedInflight != 1 {
		t.Errorf("SkippedInflight=%d want 1（在途占满应单独计数，便于运维区分「没探」与「探了没好」）",
			res.SkippedInflight)
	}

	// 释放名额后探活照常（证明跳过是"名额满"而非"永久失效"）。
	p.Release("u1")
	res = s.RunCooldownProbeNow()
	if res.Probed != 1 {
		t.Errorf("释放名额后 Probed=%d want 1", res.Probed)
	}
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("释放名额后上游调用数=%d want 1", n)
	}
}

// TestCooldownProbeSerialAcrossTargets 池内多个待探账号**串行**探活（逐个发请求），
// 避免瞬间打满上游频控。断言：三个账号共 3 次调用，且并发峰值恒为 1。
func TestCooldownProbeSerialAcrossTargets(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // 放大重叠窗口：并发会被 maxInFlight 抓到
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, Domain: "www.codebuddy.cn", AccessToken: "at", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.cooldownProbeGap = 0

	for _, uid := range []string{"u1", "u2", "u3"} {
		p.CooldownSoftForModel(uid, time.Minute, time.Now().Add(-time.Hour), "glm-5.3", "6004 model rate limit")
	}
	time.Sleep(probeExpirySettle)

	res := s.RunCooldownProbeNow()
	if res.Probed != 3 {
		t.Fatalf("Probed=%d want 3（res=%+v）", res.Probed, res)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("并发峰值=%d want 1（探活必须串行，避免瞬间打满上游频控）", got)
	}
	if res.Cleared != 3 {
		t.Errorf("Cleared=%d want 3（三个账号的模型级条目都该被清）", res.Cleared)
	}
}

// TestCooldownProbeSkipsWhenBusy 重入锁：已有巡检在执行时立即返回 Skipped=true，
// 不发任何上游调用、不阻塞调用方（与其余巡检入口同语义）。
func TestCooldownProbeSkipsWhenBusy(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	expireModelCooldown(t, p, "u1", "glm-5.3")

	if !s.beginRun("test") {
		t.Fatal("前置条件失败：测试未拿到重入锁")
	}
	defer s.endRun()

	done := make(chan CooldownProbeResult, 1)
	go func() { done <- s.RunCooldownProbeNow() }()
	select {
	case res := <-done:
		if !res.Skipped {
			t.Errorf("res=%+v want Skipped=true", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("锁被占用时入口应立即返回（不阻塞调用方）")
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上游调用数=%d want 0（已有巡检在执行时不得发起探活）", n)
	}
}

// TestCooldownProbeIntervalHotReload 间隔热改与开关：StartCooldownProbe 把装配期
// interval 落到循环的 atomic 字段；SetCooldownProbeInterval 立即改变读值（面板保存
// 配置后走这条路径），<=0 表示暂停、负值钳到 0（与 SetBalanceInterval 同口径）。
func TestCooldownProbeIntervalHotReload(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	s, _ := probeScheduler(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 装配期给 10 分钟（长间隔，测试期间不会自发跑）。
	s.StartCooldownProbe(ctx, 10*time.Minute)
	if got := s.CooldownProbeInterval(); got != 10*time.Minute {
		t.Fatalf("StartCooldownProbe 后 interval=%v want 10m", got)
	}
	// 面板改小：立即读到新值。
	s.SetCooldownProbeInterval(30 * time.Second)
	if got := s.CooldownProbeInterval(); got != 30*time.Second {
		t.Errorf("热改后 interval=%v want 30s（热应用未接线？）", got)
	}
	// 面板关开关：interval=0 = 暂停（循环空转等重排通知，热启用后立即恢复）。
	s.SetCooldownProbeInterval(0)
	if got := s.CooldownProbeInterval(); got != 0 {
		t.Errorf("关闭后 interval=%v want 0", got)
	}
	// 负值钳到 0。
	s.SetCooldownProbeInterval(-time.Minute)
	if got := s.CooldownProbeInterval(); got != 0 {
		t.Errorf("负值后 interval=%v want 0", got)
	}
}

// TestCooldownProbeDisabledDoesNotScanThenHotEnables 开关关闭（interval=0）时循环
// 不扫描：即便池内有到期目标也不发任何上游调用；热启用后立即生效（不等旧 timer
// 睡满）——这条覆盖 authwatch 同款的"空转等 rearm"设计。
func TestCooldownProbeDisabledDoesNotScanThenHotEnables(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	expireModelCooldown(t, p, "u1", "glm-5.3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartCooldownProbe(ctx, 0) // enabled=false
	time.Sleep(150 * time.Millisecond)

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("关闭状态下上游调用数=%d want 0（interval=0 必须不扫描）", n)
	}
	// 热启用：给个极短间隔，等它跑一轮。
	s.SetCooldownProbeInterval(10 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for stub.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := stub.calls.Load(); n == 0 {
		t.Error("热启用后仍无上游调用：开关从 false 改 true 应立即生效（rearm 未接线？）")
	}
}

// TestCooldownProbeLoopRunsOnTicker 循环按间隔自发跑（不只靠手动触发）：
// 极短间隔下应在预算内看到探活请求。
func TestCooldownProbeLoopRunsOnTicker(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	expireModelCooldown(t, p, "u1", "glm-5.3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartCooldownProbe(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for stub.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := stub.calls.Load(); n == 0 {
		t.Error("ticker 循环未发起探活（StartCooldownProbe 未启动或未接线）")
	}
}

// TestCooldownProbeCtxCancelStops 优雅停机：ctx 取消后循环停止，不再有新的探活请求。
func TestCooldownProbeCtxCancelStops(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	s.StartCooldownProbe(ctx, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(80 * time.Millisecond) // 等在途轮收尾

	// 取消后再放入到期目标：不应再有任何新调用。
	expireModelCooldown(t, p, "u1", "glm-5.3")
	baseline := stub.calls.Load()
	time.Sleep(200 * time.Millisecond)
	if got := stub.calls.Load(); got != baseline {
		t.Errorf("ctx 取消后仍有 %d 次新调用（want 0）", got-baseline)
	}
}

// TestCooldownProbeSetIntervalRearms 热改间隔必须 rearm（立刻按新值重开 timer，
// 不等旧 timer 睡满）：装配期给 1 小时，热改到 20ms 后应在预算内看到自发探活。
// 这条覆盖"面板把探活间隔改小"这条真实路径（saveConfig → SetCooldownProbeInterval）。
func TestCooldownProbeSetIntervalRearms(t *testing.T) {
	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	expireModelCooldown(t, p, "u1", "glm-5.3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartCooldownProbe(ctx, time.Hour)
	time.Sleep(30 * time.Millisecond)
	if n := stub.calls.Load(); n != 0 {
		t.Fatalf("前置条件失败：1 小时间隔下不应有自发调用（got %d）", n)
	}

	// 热改到 20ms：rearm 后循环立刻按新值重开 timer → 应在预算内跑一轮。
	s.SetCooldownProbeInterval(20 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for stub.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := stub.calls.Load(); n == 0 {
		t.Error("热改间隔后未在预算内探活（SetCooldownProbeInterval 未 poke/rearm？）")
	}
}

// TestProbeLogLineShape 日志行形状（可观测性契约）：成功与仍冷却各打一行，
// 前缀固定 `pool: cooldown probe uid=<8位> model=<m>`，便于面板日志区 grep。
//
// 用 captureLog 捕获（日志是运维唯一的探活观测面，字段漂移会让既有排查手册失效）。
func TestProbeLogLineShape(t *testing.T) {
	buf := captureLog(t)

	stub := &probeStub{}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	expireModelCooldown(t, p, "u1", "glm-5.3")
	s.RunCooldownProbeNow()

	out := buf.String()
	if !strings.Contains(out, "pool: cooldown probe uid=u1 model=glm-5.3 ok") {
		t.Errorf("缺成功日志行（want 含 `pool: cooldown probe uid=u1 model=glm-5.3 ok`）：\n%s", out)
	}
	if !strings.Contains(out, "scheduler: 冷却探活开始") {
		t.Errorf("缺本轮开始日志：\n%s", out)
	}
}

// TestProbeStillCoolingLogLine 仍冷却的日志行同样固定（`still cooling`），
// 与成功行配对，供运维一眼区分"探了但没好"与"探好了"。
func TestProbeStillCoolingLogLine(t *testing.T) {
	buf := captureLog(t)

	stub := &probeStub{status: 429}
	srv := stub.server(t)
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	expireModelCooldown(t, p, "u1", "glm-5.3")
	s.RunCooldownProbeNow()

	out := buf.String()
	if !strings.Contains(out, "pool: cooldown probe uid=u1 model=glm-5.3 still cooling") {
		t.Errorf("缺仍冷却日志行（want 含 `... still cooling`）：\n%s", out)
	}
}
