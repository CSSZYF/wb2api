// sequential_test.go 顺序填充式选号（pool.pick_mode=sequential）+ 顺序持久化
// （state.json 顶层 account_order）的行为锁定。
//
// 覆盖任务书的验收点 1-9、11（10 的 429 不退避在 internal/server 侧）：
//  1. 顺序基本行为：顺序第一且未满 → 恒选它；
//  2. 并发溢出：A 满 → B；B 也满 → C；
//  3. 回归：A 在途降回 0 → 再选又回到 A（用户明确要求的语义）；
//  4. 「用完」跳过：A 带恢复时间的冷却 → 选 B；A 冷却到期后重新首选；
//  5. realm 过滤 + 顺序：指定 realm 时只在该域候选里按顺序挑；
//  6. 持久化往返：SetOrder → 落盘 → 重新 Load → 顺序还在；
//  7. 兼容零回归：无 account_order 的旧 state.json → 行为与改动前一致；
//  8. 新账号追加末尾 / 删除账号跳过；
//  9. weighted 零回归：既有 pick 测试全绿（本文件另加确定性黄金序列，见
//     TestPickWeightedGoldenSequenceUnchanged）+ 落盘无 account_order 键；
//
// 11. 并发正确性：多 goroutine 同时选号+占名额，单号在途不超上限（-race 跑）。
package pool

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// seqPool 构造顺序模式池：accounts 按给定 uid 顺序加入并设为自定义顺序。
// credits 递减（第一最高）以证明顺序模式**不看积分**（用户澄清：只按列表顺序）。
func seqPool(t *testing.T, uids ...string) *Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	for i, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, int64(1000-i*10), 0) // 第一最高：若实现按积分挑，顺序错位会暴露
	}
	p.SetOrder(uids)
	p.SetPickMode(PickSequential)
	return p
}

// TestSequentialPicksFirst 验收点 1：顺序 [a,b,c] 且 a 未满 → 恒选 a。
// 实现前该用例必红（现状为三因子加权随机，实测 a 仅被选中 2-3/30 次）。
func TestSequentialPicksFirst(t *testing.T) {
	withNoPickGap(t) // 顺序模式本不受 minPickGap 影响；此处只为与既有用例同环境
	p := seqPool(t, "a", "b", "c")
	for i := 0; i < 30; i++ {
		got := p.Pick()
		if got == nil || got.UID != "a" {
			t.Fatalf("iter %d: pick=%v want a（顺序第一且未满必须恒选它）", i, got)
		}
	}
}

// TestSequentialIgnoresMinPickGap 显式锁定「sequential 分支不做 minPickGap 过滤」这一
// 设计决定（minPickGap 保持**生产值 100ms**，不像其他用例那样置 0）。
//
// 为什么必须跳过（结论写进代码注释，此处锁行为）：minPickGap 是加权模式的防撞号
// 机制——随机抽签可能连续抽中同一个号，靠时间窗把它挤开。顺序模式下「连续选中同一个
// 号」不是缺陷而是需求本身（一个号一个号地用）。若保留该过滤，紧接着的下一次选号
// 会因 `now.Sub(lastUsed) < 100ms` 把顺序第一的号跳过，落到**第二个号**——
// 制造出「明明没满却跳到下一个号」的假溢出，正是用户抱怨的「每次挑不同账号」。
func TestSequentialIgnoresMinPickGap(t *testing.T) {
	if minPickGap <= 0 {
		t.Fatalf("minPickGap 生产值应 > 0，当前 %v（本用例需要非零窗口才有意义）", minPickGap)
	}
	p := seqPool(t, "a", "b")
	// 连续快速选号（远小于 100ms 窗口）：必须恒为 a。
	for i := 0; i < 5; i++ {
		if got := p.Pick(); got == nil || got.UID != "a" {
			t.Fatalf("iter %d: 顺序模式不得被 minPickGap 挤到下一个号, got %v", i, got)
		}
	}
	// 反向对照：同一池切回 weighted 后，同样的连续快速选号会被窗口挤开
	// （证明本用例的探针确实能观测到 minPickGap 的效果，不是"窗口恰好没生效"）。
	p.SetPickMode(PickWeighted)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	first := p.Pick()
	if first == nil {
		t.Fatal("weighted pick nil")
	}
	second := p.Pick()
	if second == nil {
		t.Fatal("weighted pick nil (2nd)")
	}
	if first.UID == second.UID {
		t.Fatalf("weighted 模式在 %v 窗口内应被挤向另一个号（对照失效：探针观测不到 minPickGap）", minPickGap)
	}
}

// TestSequentialInFlightOverflow 验收点 2：A 满 → B；B 也满 → C；全满 → nil。
func TestSequentialInFlightOverflow(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b", "c")
	p.SetMaxInFlight(1)

	if !p.Acquire("a") {
		t.Fatal("warm up a")
	}
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("a 满 → want b, got %v", got)
	}
	if !p.Acquire("b") {
		t.Fatal("warm up b")
	}
	if got := p.Pick(); got == nil || got.UID != "c" {
		t.Fatalf("a+b 满 → want c, got %v", got)
	}
	if !p.Acquire("c") {
		t.Fatal("warm up c")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("全满 → want nil（不得回落选满额号）, got %v", got.UID)
	}
}

// TestSequentialReturnsToFirstAfterRelease 验收点 3（用户明确要求的语义）：
// 并发只是临时分流——A 在途降回 0 后，下一次选号必须**回到 A**（顺序靠前优先，
// 不是轮转、不是 LRU、不是"继续用 B"）。
func TestSequentialReturnsToFirstAfterRelease(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b")
	p.SetMaxInFlight(1)

	if !p.Acquire("a") {
		t.Fatal("warm up a")
	}
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("a 满 → want b, got %v", got)
	}
	// 关键：B 刚被选中（lastUsed=now，若实现里残留 minPickGap 语义，A 之外的号会被挤开）。
	p.Release("a")
	got := p.Pick()
	if got == nil || got.UID != "a" {
		t.Fatalf("a 释放后 → want a（顺序靠前优先，非轮转）, got %v", got)
	}
	// 再来一次：A 仍是首选（确定性，不受上次选中影响）。
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("再次选号 → want a（顺序确定性）, got %v", got)
	}
}

// TestSequentialSkipsCooldownUntilExpiry 验收点 4：「用完」= 带恢复时间的 429
// （applyErrorPolicy 的 ErrSoftRate 分支写成 until / 模型级冷却）→ 直接选下一个；
// 冷却到期后该号重新成为首选。
func TestSequentialSkipsCooldownUntilExpiry(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b")

	// 带恢复时间的账号级软冷却（等价 handler.applyErrorPolicy 的
	// CooldownSoftRate(uid, softCooldown, resetAt, "429 rate limit")）。
	p.CooldownSoftRate("a", time.Hour, time.Now().Add(30*time.Minute), "429 rate limit")
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("a 有带恢复时间的冷却 → want b, got %v", got)
	}

	// 模型级冷却（6004 带恢复时间）同理：仅对触发模型跳过，换模型仍可选（issue #31 语义不变）。
	p2 := seqPool(t, "a", "b")
	p2.CooldownSoftForModel("a", time.Hour, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")
	if got := p2.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "b" {
		t.Fatalf("a 对该模型 6004 冷却 → want b, got %v", got)
	}
	if got := p2.PickExcludingForModel(nil, "glm-4.5"); got == nil || got.UID != "a" {
		t.Fatalf("a 对另一模型应仍可选（模型豁免不变）→ want a, got %v", got)
	}

	// 冷却到期 → 重新首选（用极短冷却，避免睡 30 分钟）。
	p3 := seqPool(t, "a", "b")
	p3.CooldownSoftRate("a", time.Hour, time.Now().Add(2*time.Millisecond), "429 rate limit")
	if got := p3.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("冷却中 → want b, got %v", got)
	}
	time.Sleep(10 * time.Millisecond)
	if got := p3.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("冷却到期后 → want a（重新首选）, got %v", got)
	}
}

// TestSequentialRealmFilterWithOrder 验收点 5：顺序混 cn/global，指定 realm 时
// 只在该域候选里按顺序挑（realm 谓词是硬过滤，顺序只在域内生效）。
func TestSequentialRealmFilterWithOrder(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai"})
	// 顺序：cn1, g1, cn2, g2（混域）。
	p.SetOrder([]string{"cn1", "g1", "cn2", "g2"})
	p.SetPickMode(PickSequential)

	// 本域内按顺序：cn 域首个候选是 cn1（不是 g1），global 域首个是 g1。
	if got := p.PickExcludingForRealm(nil, "", "cn"); got == nil || got.UID != "cn1" {
		t.Fatalf("cn 域 want cn1, got %v", got)
	}
	if got := p.PickExcludingForRealm(nil, "", "global"); got == nil || got.UID != "g1" {
		t.Fatalf("global 域 want g1, got %v", got)
	}
	// cn1 满 → cn 域内落到 cn2（跳过 g1：跨域不参与硬过滤路径）。
	p.SetMaxInFlight(1)
	if !p.Acquire("cn1") {
		t.Fatal("warm up cn1")
	}
	if got := p.PickExcludingForRealm(nil, "", "cn"); got == nil || got.UID != "cn2" {
		t.Fatalf("cn1 满时 cn 域 want cn2（顺序域内前进，不得跨域到 g1）, got %v", got)
	}
	// 硬过滤：本域全满 → nil，不跨域。
	if !p.Acquire("cn2") {
		t.Fatal("warm up cn2")
	}
	if got := p.PickExcludingForRealm(nil, "", "cn"); got != nil {
		t.Fatalf("cn 域全满 → want nil（硬过滤不跨域）, got %v", got.UID)
	}
}

// TestSequentialOrderPersistenceRoundTrip 验收点 6：SetOrder → 落盘 → 重新 Load
// （新 Pool 实例读同一 state.json）→ 顺序还在，且 List()/Pick 同序。
func TestSequentialOrderPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetOrder([]string{"u3", "u1", "u2"})
	p.Close() // 停 flusher + 最后落盘

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// 顶层键（不在 accounts 里）：按字节核对，避免"看起来对"。
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("state.json 非法: %v\n%s", err, raw)
	}
	if _, ok := m["account_order"]; !ok {
		t.Fatalf("state.json 缺顶层 account_order 键:\n%s", raw)
	}
	var accounts map[string]json.RawMessage
	if err := json.Unmarshal(m["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	for uid, acct := range accounts {
		if bytes.Contains(acct, []byte("order")) {
			t.Errorf("顺序不得塞进 account 对象（%s 里出现 order 字段）: %s", uid, acct)
		}
	}

	// 重新 Load：顺序还在，List() 与 Pick 都用它。
	p2 := New(fp)
	defer p2.Close()
	p2.SetPickMode(PickSequential)
	for _, a := range []*auth.Auth{{UID: "u1"}, {UID: "u2"}, {UID: "u3"}} {
		p2.Add(a)
	}
	if got, want := p2.Order(), []string{"u3", "u1", "u2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Order()=%v want %v（持久化往返丢失）", got, want)
	}
	if got := listUIDs(p2); !reflect.DeepEqual(got, []string{"u3", "u1", "u2"}) {
		t.Fatalf("List()=%v want [u3 u1 u2]", got)
	}
	if got := p2.Pick(); got == nil || got.UID != "u3" {
		t.Fatalf("恢复后首选 want u3, got %v", got)
	}
}

// listUIDs 取 List() 的 uid 次序（断言面板顺序用）。
func listUIDs(p *Pool) []string {
	out := []string{}
	for _, s := range p.List() {
		out = append(out, s.UID)
	}
	return out
}

// TestOrderCompatOldStateFileNoOrderKey 验收点 7（兼容零回归）：旧 state.json 无
// account_order 键 → 顺序为空 → Order()/List() 回落「按 UID 排序」（改动前行为），
// 且落盘**不新增** account_order 键（老文件与新文件在该键上等价）。
func TestOrderCompatOldStateFileNoOrderKey(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 手写一份「旧格式」state.json：只有 accounts，无 account_order。
	old := `{"accounts":{"u2":{"credits":7},"u1":{"credits":9}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if got := p.Order(); !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Fatalf("旧 state 无顺序 → Order() 应回落 UID 排序 [u1 u2], got %v", got)
	}
	if got := listUIDs(p); !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Fatalf("旧 state 无顺序 → List() 应回落 UID 排序 [u1 u2], got %v", got)
	}
	// 空顺序下默认模式（weighted）行为不变：credits 高的 u1 被确定性注入源选中。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("weighted 缺省模式 want u1（高积分+注入源）, got %v", got)
	}
	// 落盘后仍无 account_order 键（omitempty + 空顺序不写出）。
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("account_order")) {
		t.Errorf("空顺序不应写出 account_order 键（与旧文件等价）:\n%s", raw)
	}
}

// TestSetOrderEmptyClearsCustomOrder 空数组 = 清除自定义顺序（回落 UID 排序）。
func TestSetOrderEmptyClearsCustomOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetOrder([]string{"u2", "u1"})
	if got := p.Order(); !reflect.DeepEqual(got, []string{"u2", "u1"}) {
		t.Fatalf("Order()=%v want [u2 u1]", got)
	}
	p.SetOrder(nil)
	if got := p.Order(); !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Fatalf("清除顺序后 Order()=%v want UID 排序 [u1 u2]", got)
	}
	if got := listUIDs(p); !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Fatalf("清除顺序后 List()=%v want [u1 u2]", got)
	}
}

// TestOrderNewAccountAppendedDeletedSkipped 验收点 8：
//   - 顺序里有已删除 uid → 跳过（其余相对次序不变）；
//   - 池里有 uid 不在顺序里（新账号）→ 追加到末尾，尾部按 UID 升序（稳定）。
func TestOrderNewAccountAppendedDeletedSkipped(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetOrder([]string{"u3", "ghost", "u1"}) // ghost 不存在；u2/u4 未列出
	if got, want := p.Order(), []string{"u3", "u1", "u2", "u4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Order()=%v want %v（跳过已删除 + 新号按 UID 升序追加末尾）", got, want)
	}
	if got := listUIDs(p); !reflect.DeepEqual(got, []string{"u3", "u1", "u2", "u4"}) {
		t.Fatalf("List()=%v want [u3 u1 u2 u4]", got)
	}
	// 顺序里的账号被删除（Remove）后：跳过它，其余次序不变。
	p.Remove("u1")
	if got, want := p.Order(), []string{"u3", "u2", "u4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("删除 u1 后 Order()=%v want %v", got, want)
	}
	// 重复 uid：只取首次出现（不会把同一号算两次）。
	p.SetOrder([]string{"u4", "u4", "u2"})
	if got, want := p.Order(), []string{"u4", "u2", "u3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("重复 uid 的 Order()=%v want %v", got, want)
	}
}

// TestSequentialOverflowLog 验收点 6（可观测）：发生溢出（顺序靠前的号被跳过）时
// 打一条日志；顺序第一直接命中时**不打**（不刷屏）。
func TestSequentialOverflowLog(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old) // log.SetOutput 是进程级全局：本用例不并行

	withNoPickGap(t)
	p := seqPool(t, "a", "b")
	p.SetMaxInFlight(3)

	// 无溢出：顺序第一直接命中 → 零日志。
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("want a, got %v", got)
	}
	if s := buf.String(); strings.Contains(s, "sequential skip") {
		t.Fatalf("顺序第一命中不该打溢出日志: %q", s)
	}

	// 溢出：a 满 → 落到 b，打一条含原因与下一个号的日志。
	for i := 0; i < 3; i++ {
		if !p.Acquire("a") {
			t.Fatalf("warm up a #%d", i)
		}
	}
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("a 满 → want b, got %v", got)
	}
	s := buf.String()
	if !strings.Contains(s, "pool: sequential skip uid=a reason=inflight_full(3/3) -> next uid=b") {
		t.Fatalf("溢出日志缺失或格式不符:\n%q", s)
	}

	// 冷却型跳过（「用完」）也走同一条日志（原因 unhealthy）。
	buf.Reset()
	p2 := seqPool(t, "a", "b")
	p2.CooldownSoftRate("a", time.Hour, time.Now().Add(time.Hour), "429 rate limit")
	if got := p2.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("want b, got %v", got)
	}
	if s := buf.String(); !strings.Contains(s, "uid=a reason=unhealthy -> next uid=b") {
		t.Fatalf("冷却跳过日志缺失或格式不符:\n%q", s)
	}

	// 纯 realm 跳过**不打**日志（混合池按域过滤是正常形态，照打会每次请求刷一行）。
	buf.Reset()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p3 := New("")
	p3.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p3.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p3.SetOrder([]string{"g1", "cn1"}) // g1 在顺序最前，但本次请求只允许 cn 域
	p3.SetPickMode(PickSequential)
	if got := p3.PickExcludingForRealm(nil, "", "cn"); got == nil || got.UID != "cn1" {
		t.Fatalf("cn 域 want cn1, got %v", got)
	}
	if s := buf.String(); strings.Contains(s, "sequential skip") {
		t.Fatalf("纯 realm 过滤跳过不该打溢出日志: %q", s)
	}
	// 无候选（本域唯一号占满）→ 返回 nil，由 pick 决定回落/兜底，同样不打溢出日志
	// （避免与 "realm fallback"/"fallback_earliest_expiry" 两条既有日志重复叙事）。
	buf.Reset()
	p3.SetMaxInFlight(1)
	if !p3.Acquire("cn1") {
		t.Fatal("warm up cn1")
	}
	if got := p3.PickExcludingForRealm(nil, "", "cn"); got != nil {
		t.Fatalf("cn 域唯一号满 → want nil, got %v", got.UID)
	}
	if s := buf.String(); strings.Contains(s, "sequential skip") {
		t.Fatalf("无候选返回 nil 时不该打溢出日志: %q", s)
	}
}

// TestSequentialConcurrentPickRespectsInFlightLimit 验收点 11（-race 跑）：
// 多 goroutine 同时「选号 + 占名额 + 释放」，单号在途数恒不超上限。
// 顺序模式下选号是确定性的（多 goroutine 全瞄同一个号），正是最容易撞满的形态。
//
// 关于「Pick 返回的号 Acquire 失败」：这是**合法**的既有 CAS 竞态，不是缺陷——
// Pick 的 inFlightFull 判定与 Acquire 的 CAS 之间有窗口（handler 的注释称之为
// "CAS 兜底并发抢名额的竞态"，命中即换号）。本用例因此把它计为 `casLost` 而不报错，
// 只断言真正的租约不变量：**任何时刻单号在途 ≤ 上限**、结束后名额全部归还。
// 顺序模式下一个号被 12 个 goroutine 同时盯上，这个窗口必然会被打到——正是本用例
// 要覆盖的高压形态。
func TestSequentialConcurrentPickRespectsInFlightLimit(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b", "c")
	const limit = 3
	p.SetMaxInFlight(limit)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var casLost atomic.Int64
	maxSeen := map[string]int64{}
	// 每个 goroutine 尽量**同时持满** limit 个名额再一起释放：单号在途因此会真实
	// 顶到上限（而非永远在 0/1 之间摆动），"峰值 ≤ 上限" 这条断言才有区分力——
	// 若实现里的上限判定有任何并发漏洞，12×3 个 goroutine 会把峰值顶破。
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				acct := p.Pick()
				if acct == nil {
					continue // 全满：合法（调用方 503）
				}
				held := make([]string, 0, limit)
				for len(held) < limit {
					if !p.Acquire(acct.UID) {
						// CAS 竞态（Pick 与 Acquire 之间的窗口被别的 goroutine 抢走）
						// 或已顶到上限：合法，计一次 casLost 继续。
						casLost.Add(1)
						break
					}
					held = append(held, acct.UID)
					cur := p.inFlightOf(acct.UID)
					mu.Lock()
					if cur > maxSeen[acct.UID] {
						maxSeen[acct.UID] = cur
					}
					mu.Unlock()
					if cur > limit {
						// 租约不变量被破坏：t.Errorf 是 goroutine 安全的，直接报（不用
						// 通道转发——固定容量通道在失败量大时会阻塞 goroutine 造成死锁）。
						t.Errorf("uid=%s 在途=%d 超过上限 %d", acct.UID, cur, limit)
					}
				}
				for _, uid := range held {
					p.Release(uid)
				}
			}
		}()
	}
	wg.Wait()
	peak := int64(0)
	for uid, n := range maxSeen {
		if n > limit {
			t.Errorf("uid=%s 峰值在途 %d > 上限 %d", uid, n, limit)
		}
		if n > peak {
			peak = n
		}
	}
	// 非空断言：本用例必须真的把某个号顶到上限，否则"峰值 ≤ 上限"是空话
	// （例如实现退化成"每次都新建 entry"或选号恒返回 nil 时也会通过）。
	if peak < limit {
		t.Errorf("峰值在途=%d 未达上限 %d：本用例失去区分力（并发未真正压满）", peak, limit)
	}
	// 结束后所有名额归还（Release 幂等，无泄漏）。
	for _, uid := range []string{"a", "b", "c"} {
		if n := p.inFlightOf(uid); n != 0 {
			t.Errorf("uid=%s 在途残留 %d want 0", uid, n)
		}
	}
	t.Logf("CAS 竞态命中 %d 次（合法：Pick 与 Acquire 之间的既有窗口，handler 以换号兜底）", casLost.Load())
}

// inFlightOf 读单号在途计数（包内测试 helper；不存在返回 -1）。
func (p *Pool) inFlightOf(uid string) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return -1
	}
	return e.inFlight.Load()
}

// TestPickWeightedGoldenSequenceUnchanged 验收点 9（weighted 零回归的强证据）：
// 缺省模式下，固定账号集 + 注入确定性随机源 + minPickGap=0 时的选号序列必须与
// 改动前（base commit fd2d52d）**逐项相同**。黄金序列由基线 worktree 实测产生
// （同场景跑基线代码打印序列），实现改动若碰到 weighted 路径的候选集/权重/截断/
// LRU 任一处，本用例即红。
//
// 序列形状的解释（证明它确实是 weighted 语义而非碰巧）：注入源恒取权重区间最左，
// 首轮从未用过的号吃满 idleWeightMax，a（积分最高）胜出；a 被选中后 idle 归零，
// 权重掉到 b 之下（b 仍未用过）→ 第二轮 b；b 用过之后两者 idle 都≈0，a 的积分项
// 更高 → 之后恒为 a。任何一处权重/候选集/排序改动都会改变这个形状。
func TestPickWeightedGoldenSequenceUnchanged(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"a", "b", "c", "d"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetCredits("a", 400, 0)
	p.SetCredits("b", 300, 0)
	p.SetCredits("c", 200, 0)
	p.SetCredits("d", 100, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 恒取权重区间最左 → 完全确定

	want := []string{"a", "b", "a", "a", "a", "a", "a", "a", "a", "a", "a", "a"}
	for i, w := range want {
		got := p.Pick()
		if got == nil || got.UID != w {
			t.Fatalf("iter %d: pick=%v want %s（weighted 序列漂移）", i, got, w)
		}
	}
}

// TestSequentialIgnoresCreditsAndSuccessRate 顺序模式只认顺序：积分/成功率/闲置
// 都不参与挑选（用户澄清「现在不考虑积分」）。
func TestSequentialIgnoresCreditsAndSuccessRate(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 0, 0) // 顺序第一但零积分
	p.SetCredits("b", 999999, 0)
	// 只喂 2 次错误（熔断阈值默认 3）：足以让 a 的成功率为 0、b 为 1，又不至于
	// 把 a 熔断（熔断会让它变 unhealthy，本用例要证的是「健康但不优」仍首选）。
	for i := 0; i < 2; i++ {
		p.NoteSuccess("b")
		p.NoteError("a")
	}
	p.SetOrder([]string{"a", "b"})
	p.SetPickMode(PickSequential)
	for i := 0; i < 10; i++ {
		if got := p.Pick(); got == nil || got.UID != "a" {
			t.Fatalf("iter %d: 顺序模式必须只认顺序（零积分+高错误率也首选）, got %v", i, got)
		}
	}
}

// TestSequentialModeSwitchIsLive 模式可热切换：SetPickMode 后下一次选号即按新模式
// （面板改 pool.pick_mode 的热生效语义）。
func TestSequentialModeSwitchIsLive(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 1, 0)
	p.SetCredits("b", 1000, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetOrder([]string{"a", "b"})

	if got := p.PickMode(); got != PickWeighted {
		t.Fatalf("缺省模式 want PickWeighted(0), got %v", got)
	}
	// weighted：注入源恒 0 + b 积分高 → 恒选 b。
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("weighted want b, got %v", got)
	}
	p.SetPickMode(PickSequential)
	if got := p.PickMode(); got != PickSequential {
		t.Fatalf("PickMode()=%v want PickSequential", got)
	}
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("切到 sequential 后 want a, got %v", got)
	}
	p.SetPickMode(PickWeighted) // 切回
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("切回 weighted 后 want b, got %v", got)
	}
}

// TestSequentialFallbackSemanticsUnchanged 兜底与跨域回落语义不变（只换 healthy 段
// 的挑选方式）：全池冷却时仍走 pickEarliestExpiryLocked（选最早到期者），
// realm 软优先回落照旧。
func TestSequentialFallbackSemanticsUnchanged(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b")
	// 两号都在冷却 → 顺序模式也必须走全冷却兜底（不得返回 nil）。
	p.Cooldown("a", CoolSoft, 30*time.Minute, "x")
	p.Cooldown("b", CoolSoft, 10*time.Minute, "y")
	got := p.Pick()
	if got == nil || got.UID != "b" {
		t.Fatalf("全冷却兜底应选最早到期者 b, got %v", got)
	}

	// 跨域回落：本域全冷却、另一域健康 → 软优先入口回落（顺序模式不改回落语义）。
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p2 := New("")
	p2.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p2.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p2.SetCredits("cn1", 10, 0)
	p2.SetCredits("g1", 10, 0)
	p2.Cooldown("cn1", CoolSoft, time.Hour, "x")
	p2.SetPickMode(PickSequential)
	if got := p2.PickExcludingForRealmFallback(nil, "", "cn"); got == nil || got.UID != "g1" {
		t.Fatalf("cn 域全冷却 → 应回落 global 的 g1, got %v", got)
	}
}

// TestSequentialExpiringFirstOrdering 顺序模式的「快过期账号优先」：
// creditsExpiring>0 的账号整体提前，组内按 creditsExpiringAt（快过期子集的最早
// 到期时刻）升序；没有时间戳的旧状态排在没有时间戳的快过期账号之后、非快过期
// 账号之前。其余账号保持运维顺序；在途占满照常溢出。
func TestSequentialExpiringFirstOrdering(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b", "c", "d")
	p.SetMaxInFlight(1)
	now := time.Now()
	p.SetCreditsExpiringAt("a", 5, now.Add(5*time.Hour))
	p.SetCreditsExpiringAt("c", 10, now.Add(2*time.Hour))
	p.SetCreditsExpiringAt("d", 7, time.Time{}) // 旧 state：有分桶但无最早到期时刻

	// 粘性新会话走的 AvailableUIDs 必须与顺序选号同一口径（快过期前置）。
	if got, want := p.AvailableUIDs(), []string{"c", "a", "d", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDs()=%v want %v（粘性分配与顺序选号同口径）", got, want)
	}
	for _, uid := range []string{"c", "a", "d", "b"} {
		got := p.Pick()
		if got == nil || got.UID != uid {
			t.Fatalf("顺序选号 want %s, got %v（快过期账号应按最早到期优先）", uid, got)
		}
		if !p.Acquire(uid) {
			t.Fatalf("warm up %s（在途占满后应溢出到下一个）", uid)
		}
	}
}

// TestSequentialExpiringTieKeepsConfiguredOrder 同为快过期且最早到期时刻相同（或
// 都缺时间戳）时，按运维顺序作稳定 tie-breaker——快过期优先不是重排账号池。
func TestSequentialExpiringTieKeepsConfiguredOrder(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "a", "b", "c")
	p.SetMaxInFlight(1)
	at := time.Now().Add(3 * time.Hour)
	p.SetCreditsExpiringAt("a", 5, at)
	p.SetCreditsExpiringAt("c", 5, at)
	for _, uid := range []string{"a", "c", "b"} {
		got := p.Pick()
		if got == nil || got.UID != uid {
			t.Fatalf("相同到期时刻 want %s, got %v（应保持运维顺序）", uid, got)
		}
		if !p.Acquire(uid) {
			t.Fatalf("warm up %s", uid)
		}
	}
}

// TestSequentialExpiringRespectsReserve 快过期优先只在「有资格」的候选里生效：
// 贵模型受 reserve_credits 闸门约束，低余额快过期账号不能靠到期优先绕过底线；
// 免费模型不受底线限制，低余额快过期账号仍按最早到期优先消耗。
func TestSequentialExpiringRespectsReserve(t *testing.T) {
	withNoPickGap(t)
	p := seqPool(t, "low", "high", "normal")
	p.SetReserveCredits(50)
	now := time.Now()
	setKnownCredits(p, "low", 45) // ≤50：贵模型不得选它
	p.SetCreditsExpiringAt("low", 10, now.Add(time.Hour))
	setKnownCredits(p, "high", 60) // >50 且有快过期：贵模型首选
	p.SetCreditsExpiringAt("high", 5, now.Add(10*time.Hour))
	setKnownCredits(p, "normal", 1000)

	if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "high" {
		t.Fatalf("贵模型 want high（low 到期更早但余额触底线）, got %v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3"); got == nil || got.UID != "low" {
		t.Fatalf("免费模型 want low（免费模型不受底线限制，仍按最早到期优先）, got %v", got)
	}
}
