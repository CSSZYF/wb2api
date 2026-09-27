// sticky_order_wiring_test.go 顺序模式（pool.pick_mode=sequential）在**粘性分配路径**上
// 的接线与热生效（cmd/server 层，走 saveConfig 全链路）。
//
// 缺陷背景（v1.9.26）：顺序填充式选号只改了 pool 侧 Pool.Pick 的挑选方式，而"带会话键的
// 新会话绑到哪个号"走的是另一条路径——session.Router 的慢路径分配（此前一律双段 +
// FNV-1a 哈希取模）。于是开了 sequential 也照样把新会话散到各个号，「绝大头在第一个号」
// 不成立。修复分两层：
//   - 池侧：AvailableUIDs 系列按 Pool.Order() 排序（无顺序时 UID 升序，零回归）；
//   - 粘性侧：Router.assign 在 sequential 下取候选列表第一个，且**跳过** idle 双段策略。
//
// 本文件锁的是**接线**（main.go 装配 + saveConfig 热应用），与 internal/session、
// internal/pool 的算法单测互补：算法对了但接线漏了，配置照样落盘、面板照样提示"已保存"，
// 行为却不变——正是 issue #17 对 max_body_mb 描述过的「静默不生效」失效模式。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stickyOrderPool 造「UID 升序与自定义顺序相反」的池：UID 升序 = [a b]，顺序 = [b a]。
// 带会话键的新会话若绑到 a 即说明顺序没生效（哈希/UID 升序口径）。
func stickyOrderPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New("")
	for _, uid := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 0)
	}
	p.SetOrder([]string{"b", "a"})
	return p
}

// chatHitUID 发一次带会话键的非流式 chat 请求，返回上游收到的 Authorization 后缀
// （= 实际被分配的账号 uid）。上游恒 200，故请求内不会换号。
func chatHitUID(t *testing.T, h *server.Handler, up *upstream.Client, convID string) string {
	t.Helper()
	var seen string
	up.HTTP.Transport = rotateTripFunc(func(r *http.Request) (*http.Response, error) {
		if seen == "" {
			seen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}, nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"`+convID+`"}}`)))
	if rec.Code != 200 {
		t.Fatalf("conv=%s code=%d body=%s", convID, rec.Code, rec.Body)
	}
	return seen
}

// TestNewSessionRouterWiresSequential 装配接线：newSessionRouter 必须把
// cfg.Pool.PickMode 镜像进 session.Config.Sequential（缺了它，顺序模式在带会话键的
// 客户端上完全失效——正是本次缺陷）。
func TestNewSessionRouterWiresSequential(t *testing.T) {
	p := stickyOrderPool(t)
	seqCfg := Default()
	seqCfg.Pool.PickMode = PoolPickModeSequential
	if r := newSessionRouter(seqCfg, p, redisstore.Noop{}); !r.Sequential() {
		t.Errorf("pool.pick_mode=sequential 时 session Router 必须也开 sequential（粘性分配是另一条路径）")
	}
	// weighted（缺省）：粘性侧保持改动前行为（哈希分散）。
	wCfg := Default()
	wCfg.Pool.PickMode = PoolPickModeWeighted
	if r := newSessionRouter(wCfg, p, redisstore.Noop{}); r.Sequential() {
		t.Errorf("pool.pick_mode=weighted 时 session Router 不得开 sequential（零回归）")
	}
	// 非法值经 normalize 后回落 weighted（与 pool 侧同口径）。
	badCfg, err := ParseConfig([]byte(`{"pool":{"pick_mode":"bogus"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := newSessionRouter(badCfg, p, redisstore.Noop{}); r.Sequential() {
		t.Errorf("非法 pick_mode 归一化后必须回落 weighted，粘性侧同样不得开顺序")
	}
}

// TestNewSessionRouterSequentialAssignsFirstInOrder 装配接线的**行为**证明（不读开关值，
// 只看分配结果）：顺序模式装配出的 Router，新会话必须落到顺序第一的 b。
//
// 这条同时覆盖池侧与粘性侧两层：候选列表由 realmAwareAvailableForModel →
// pool.AvailableUIDsForModelRealm 给出（已按 order 排序），Router 取第一个。
//
// key 选取：用哈希下标为 1 的 key（2 元素池里 weighted 会落到列表第二个 a）——
// 若接线缺失（Router 仍按 weighted 走哈希），这些 key 会全部落到 a，断言即失败。
func TestNewSessionRouterSequentialAssignsFirstInOrder(t *testing.T) {
	p := stickyOrderPool(t)
	cfg := Default()
	cfg.Pool.PickMode = PoolPickModeSequential
	r := newSessionRouter(cfg, p, redisstore.Noop{})

	for _, key := range []string{"k1", "k5", "c1", "c3", "x", "z"} {
		uid, ok := r.ResolveForModel(key, "glm-5.2")
		if !ok {
			t.Fatalf("key=%s resolve failed", key)
		}
		if uid != "b" {
			t.Fatalf("顺序模式新会话 key=%s 分配=%q want b（顺序第一；哈希口径会落 a）", key, uid)
		}
	}
}

// TestSaveConfigAppliesPickModeToStickyHot 面板保存 pool.pick_mode 必须让**粘性分配**
// 也即时生效（两处热应用：pool.SetPickMode + sess.SetSequential）。
//
// 与 TestSaveConfigAppliesPickModeHot 的分工：那条盯的是"无会话键 → Pool.Pick"，
// 本用例盯的是"带会话键 → 新会话分配"。只做前者会让缺陷以"配置生效了但一半流量照旧"
// 的形态存活——比完全没生效更难排查。
//
// 判别构造（对"漏热应用"敏感，不依赖随机源）：
//   - 池顺序 = [b a]（AvailableUIDs 也按此序），**两个号都已被既有会话绑定** →
//     双段策略的 idle 段为空、回落全池哈希 [b a]；
//   - 取哈希下标为 1 的 key（k1/k3/k5…）：weighted 下落到列表第二个 a，
//     sequential 下取列表第一个 b——两种模式结果相反，任何一侧漏热应用都会被断言抓到。
func TestSaveConfigAppliesPickModeToStickyHot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"pool":{"pick_mode":"weighted"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := stickyOrderPool(t)

	up := &upstream.Client{
		HTTP: &http.Client{Transport: rotateTripFunc(func(r *http.Request) (*http.Response, error) {
			return badParamsResponse(), nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	// 装配期：与 main.go 同一形状（模式取自启动时的配置）。
	cfg := Default()
	cfg.Pool.PickMode = PoolPickModeWeighted
	p.SetPickMode(PickMode(cfg.Pool.PickMode))
	sess := newSessionRouter(cfg, p, redisstore.Noop{})
	h := server.NewHandler(server.Config{Pool: p, Upstream: up, Session: sess})
	live := livecfg.New(livecfg.Snapshot{})

	// 预绑定两个号：把 idle 段清空，让 weighted 恒走"全池哈希"这一段（确定性）。
	sess.Bind("pre-1", "a")
	sess.Bind("pre-2", "b")

	// 装配期基线：weighted → 哈希下标 1 → 列表第二个 a。
	if got := chatHitUID(t, h, up, "k1"); got != "a" {
		t.Fatalf("装配期 weighted 新会话分配=%q want a（哈希口径）", got)
	}
	// 保存 sequential：不重启、不重建 handler/session。k3 也是哈希下标 1 的 key，
	// 若粘性侧热应用缺失（仍按 weighted 哈希）它会落到 a，断言即失败。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"sequential"}}`),
		cfgPath, live, p, up, sch, h, sess); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := chatHitUID(t, h, up, "k3"); got != "b" {
		t.Errorf("保存 pick_mode=sequential 后新会话分配=%q want b（粘性侧热应用未接线？）", got)
	}
	// 落盘值也要对（面板 GET 回显依赖它）。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["pool"]["pick_mode"]; !ok || v != "sequential" {
		t.Errorf("落盘 pool.pick_mode=%v want sequential (raw=%s)", v, raw)
	}
	// 切回 weighted（反方向：证明不是碰巧读到静态值）。k5 同为哈希下标 1 的 key。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"weighted"}}`),
		cfgPath, live, p, up, sch, h, sess); err != nil {
		t.Fatalf("saveConfig(weighted): %v", err)
	}
	if got := chatHitUID(t, h, up, "k5"); got != "a" {
		t.Errorf("切回 weighted 后新会话分配=%q want a（哈希口径）", got)
	}
	// 非法值：normalize 回落 weighted 并热应用（不是把非法值写进运行态）。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"bogus"}}`),
		cfgPath, live, p, up, sch, h, sess); err != nil {
		t.Fatalf("saveConfig(bogus): %v", err)
	}
	if got := chatHitUID(t, h, up, "k7"); got != "a" {
		t.Errorf("非法 pick_mode 应回落 weighted 并热应用，新会话分配=%q want a", got)
	}
	// 再切 sequential（第二次正向，确认反复切换都生效）。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"sequential"}}`),
		cfgPath, live, p, up, sch, h, sess); err != nil {
		t.Fatalf("saveConfig(sequential): %v", err)
	}
	if got := chatHitUID(t, h, up, "k9"); got != "b" {
		t.Errorf("再次保存 sequential 后新会话分配=%q want b", got)
	}
	// sess=nil（粘性关闭）时 saveConfig 不得 panic（跳过热应用，与 SetTTL 同口径）。
	if _, err := saveConfig([]byte(`{"pool":{"pick_mode":"weighted"}}`),
		cfgPath, live, p, up, sch, h, nil); err != nil {
		t.Fatalf("saveConfig(sess=nil): %v", err)
	}
}

// TestSaveConfigSequentialStickyOverflowHot 顺序模式 + 在途占满的端到端：
// 顺序第一的号在途占满时，新会话必须落到顺序里的下一个号（粘性分配的"溢出"），
// 释放后回到顺序第一。覆盖 AvailableUIDs 的 inFlightFull 过滤 + assign 取第一个的组合。
//
// 判别构造：池顺序 = [c b a]（**第三个**才是 UID 升序的第一位），c 在途占满 →
// sequential 必须给出 b（顺序里的下一个）；哈希/UID 升序口径会给 a，可直接区分。
// 每个断言点的 key 都用「2/3 元素池里哈希下标指向另一个号」的取值（见注释），
// 使断言对"漏改 sequential 分支/漏接线"敏感。
func TestSaveConfigSequentialStickyOverflowHot(t *testing.T) {
	p := pool.New("")
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 0)
	}
	p.SetOrder([]string{"c", "b", "a"})
	p.SetMaxInFlight(1)
	p.SetPickMode(pool.PickSequential)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: rotateTripFunc(func(r *http.Request) (*http.Response, error) {
			return badParamsResponse(), nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	cfg := Default()
	cfg.Pool.PickMode = PoolPickModeSequential
	sess := newSessionRouter(cfg, p, redisstore.Noop{})
	h := server.NewHandler(server.Config{Pool: p, Upstream: up, Session: sess})

	// 未占满：新会话取顺序第一 c。k1 在 3 元素池里哈希下标 0（= 列表首个 c），
	// 故这一条对"顺序/哈希"不敏感，仅作基线。
	if got := chatHitUID(t, h, up, "k1"); got != "c" {
		t.Fatalf("顺序第一未满时新会话分配=%q want c", got)
	}
	// c 占满唯一在途名额 → 候选列表 = [b a]，新会话必须取 b（顺序下一个）。
	// k5 在 2 元素池里哈希下标 1（= 列表第二个 a）：若 sequential 分支缺失，
	// 这里会落 a，断言即失败。
	if !p.Acquire("c") {
		t.Fatal("precondition: c 应能占到唯一在途名额")
	}
	if got := chatHitUID(t, h, up, "k5"); got != "b" {
		t.Fatalf("c 在途占满时新会话分配=%q want b（顺序下一个，而非哈希指向的 a）", got)
	}
	// b 也占满 → 继续沿顺序溢出到 a（候选只剩 a，两种口径一致）。
	if !p.Acquire("b") {
		t.Fatal("precondition: b 应能占到在途名额")
	}
	if got := chatHitUID(t, h, up, "k3"); got != "a" {
		t.Fatalf("c/b 都占满时新会话分配=%q want a（继续沿顺序溢出）", got)
	}
	// 并发降回来 → 顺序第一 c 重新首选（用户明确要求的语义）。
	p.Release("c")
	p.Release("b")
	if got := chatHitUID(t, h, up, "k7"); got != "c" {
		t.Fatalf("c 释放后新会话分配=%q want c（顺序第一重新首选）", got)
	}
	// 顺序持久化不受影响。
	if got := p.Order(); len(got) != 3 || got[0] != "c" || got[1] != "b" || got[2] != "a" {
		t.Errorf("Order()=%v want [c b a]", got)
	}
}
