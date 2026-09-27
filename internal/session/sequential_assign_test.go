// sequential_assign_test.go 顺序模式（pool.pick_mode=sequential）下**新会话分配**的
// 粘性侧契约。
//
// 缺陷（v1.9.26 已上线顺序填充式选号，但粘性分配路径没跟上）：
//   - 粘性命中走 PickByUIDForModel（已正确：校验 healthyForModel + inFlightFull，
//     满了返回 nil → 回落普通轮换 → 顺序模式生效）；
//   - **新会话绑定到哪个号**走 session.Router 的慢路径分配：此前一律「双段策略 +
//     FNV-1a 哈希取模」。于是即使开了 sequential，新会话也被哈希随机分散到各账号，
//     「绝大头在第一个号」不成立——顺序模式只在「无会话键 → Pool.Pick」那条路上生效。
//
// 本文件锁定修复后的语义（两种模式的分野只在「怎么挑候选」这一件事）：
//   - sequential：候选列表（pool.AvailableUIDs，已按 Pool.Order() 排序且已过滤
//     不健康/在途占满）的**第一个**，且**跳过**"空闲账号优先"的双段策略
//     （该策略与集中填充的诉求相反，见 session.go 分配处的注释）；
//   - weighted（缺省）：双段策略 + 哈希取模，逐字节不变（golden 见本文件末）。
package session

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// assignPool 构造一个「加入次序与 UID 字典序刻意相反」的真实池，并把自定义顺序设为
// order（nil = 不设顺序，即默认部署的旧行为）。
//
// 为什么要真实 pool 而不是注入固定切片：本缺陷的根因横跨两层——pool 侧
// AvailableUIDs 的排序口径 + session 侧的挑选方式。用真实 pool 才能证明两层**合起来**
// 满足「顺序第一优先」（只测其中一层会让另一半回退而不被发现）。
func assignPool(t *testing.T, order []string, uids ...string) *pool.Pool {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
	}
	if order != nil {
		p.SetOrder(order)
	}
	return p
}

// assignRouter 构造与 cmd/server/main.go 同形的粘性路由：Available 直接接
// pool.AvailableUIDs（生产装配即如此），并注入顺序模式开关。
func assignRouter(t *testing.T, p *pool.Pool, sequential bool) *Router {
	t.Helper()
	return New(Config{
		TTL:        time.Hour,
		Store:      newCountingStore(),
		Available:  p.AvailableUIDs,
		Sequential: sequential,
	})
}

// TestSequentialAssignTakesFirstInOrder 验收点 1：顺序模式新会话绑**顺序第一**的号。
//
// 用多个 key 覆盖两种哈希下标（k1 旧口径落 a2、k2 旧口径落 a1）：无论哈希把 key
// 散到哪里，顺序模式都必须给出同一个答案（顺序第一）——「哈希可能给 A」正是本缺陷
// 的形态，单 key 断言可能恰好命中顺序第一而漏判。
func TestSequentialAssignTakesFirstInOrder(t *testing.T) {
	p := assignPool(t, []string{"b", "a"}, "a", "b") // 自定义顺序 [b a]，UID 升序为 [a b]
	r := assignRouter(t, p, true)

	for _, key := range []string{"k1", "k2", "k3", "c1", "c2", "sess-1"} {
		uid, ok := r.Resolve(key)
		if !ok {
			t.Fatalf("key=%s resolve failed", key)
		}
		if uid != "b" {
			t.Fatalf("key=%s sequential 分配=%q want b（顺序第一；哈希分散即缺陷）", key, uid)
		}
	}
	// 分配结果必须落在可用候选列表的第一个上（与 Pool.Order() 同源）。
	if got := p.AvailableUIDs(); got[0] != "b" {
		t.Fatalf("AvailableUIDs()[0]=%q want b（顺序第一）", got[0])
	}
}

// TestSequentialAssignSkipsIdleSegment 验收点 1 的判别用例（双段策略的处置）：
// 顺序第一的号**已被别的会话绑定**（不再 idle）时，顺序模式仍必须把它分配给新会话。
//
// 这一条把「(a) 跳过 idle 段直接取全池顺序第一个」与「(b) 保留 idle 段只改取法」
// 区分开：若保留 idle 段，idle=[a] 会把新会话推到第二个号上，与用户诉求
// （「保证绝大头在第一个号就行了，也就是填充模式」）相反。
func TestSequentialAssignSkipsIdleSegment(t *testing.T) {
	p := assignPool(t, []string{"b", "a"}, "a", "b")
	r := assignRouter(t, p, true)

	// 先把 b 绑给另一个会话（b 从此不再是"空闲账号"，a 仍是）。
	r.Bind("s1", "b")
	if uid, ok := r.Resolve("s1"); !ok || uid != "b" {
		t.Fatalf("已绑定会话应粘住 b, got %q ok=%v", uid, ok)
	}
	// 新会话：顺序第一仍是 b —— 集中填充（跳过 idle 段）。
	for _, key := range []string{"n1", "n2"} {
		uid, ok := r.Resolve(key)
		if !ok {
			t.Fatalf("key=%s resolve failed", key)
		}
		if uid != "b" {
			t.Fatalf("key=%s sequential 分配=%q want b（idle 段必须跳过：集中填充优先于摊开）", key, uid)
		}
	}
}

// TestSequentialAssignOverflowToNext 验收点 2：顺序第一在途占满 → 新会话取顺序第二。
// 「满」的判定与选号同一口径（pool.inFlightFull，上限 0 = 不限）。
//
// 判别构造：每个断言点都用"哈希口径会给出另一个号"的 key（2 元素池里哈希下标 1 的
// key 会落到列表第二个），因此断言对"漏改 sequential 分支"敏感。
func TestSequentialAssignOverflowToNext(t *testing.T) {
	p := assignPool(t, []string{"b", "a"}, "a", "b")
	p.SetMaxInFlight(1)
	r := assignRouter(t, p, true)

	// 未占满：顺序第一 b（哈希口径会落 a）。
	if uid, ok := r.Resolve("k1"); !ok || uid != "b" {
		t.Fatalf("未占满时分配=%q want b（顺序第一）ok=%v", uid, ok)
	}
	// b 占满唯一在途名额 → 候选列表只剩 a（顺序溢出，此时两种口径一致）。
	if !p.Acquire("b") {
		t.Fatal("precondition: b 应能占到唯一在途名额")
	}
	if got := p.AvailableUIDs(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("b 占满后 AvailableUIDs=%v want [a]", got)
	}
	for _, key := range []string{"k2", "n1"} {
		uid, ok := r.Resolve(key)
		if !ok {
			t.Fatalf("key=%s resolve failed", key)
		}
		if uid != "a" {
			t.Fatalf("key=%s 分配=%q want a（顺序第一在途占满 → 溢出到顺序第二）", key, uid)
		}
	}
	// 并发降回来 → 顺序第一重新成为首选（用户明确要求的语义）。
	p.Release("b")
	if uid, ok := r.Resolve("n2"); !ok || uid != "b" {
		t.Fatalf("b 释放后新会话应回到 b, got %q ok=%v", uid, ok)
	}
}

// TestSequentialAssignStickyHitUnaffected 验收点 6：粘性命中路径**不受顺序模式影响**。
//   - 已绑定会话继续用它的号（即使它不是顺序第一）；
//   - 该号在途占满/不健康 → 绑定失效，重新分配走顺序（取顺序第一的可用号）。
//   - 已绑定号不构成"不可用"：新会话仍优先集中到顺序第一（跳过 idle 段）。
func TestSequentialAssignStickyHitUnaffected(t *testing.T) {
	p := assignPool(t, []string{"b", "a"}, "a", "b")
	p.SetMaxInFlight(1)
	r := assignRouter(t, p, true)

	// 会话 s1 预绑顺序第二的 a：命中必须保持 a（粘性优先于顺序）。
	r.Bind("s1", "a")
	for i := 0; i < 3; i++ {
		if uid, ok := r.Resolve("s1"); !ok || uid != "a" {
			t.Fatalf("iter %d: 粘性命中应保持 a（非顺序第一也粘住）, got %q ok=%v", i, uid, ok)
		}
	}
	// 顺序第一的 b 已被 s2 绑定（不再是"空闲号"）：新会话仍必须取 b——哈希口径此时
	// 会因 idle=[a] 落到 a（k1 在 2 元素池里下标 1），断言因此对回退敏感。
	r.Bind("s2", "b")
	if uid, ok := r.Resolve("k1"); !ok || uid != "b" {
		t.Fatalf("已绑定号不算不可用：新会话应集中到顺序第一 b, got %q ok=%v", uid, ok)
	}
	// a 在途占满 → s1 的绑定失效 → 重新分配：候选只剩 b（顺序第一的可用号）。
	if !p.Acquire("a") {
		t.Fatal("precondition: a 应能占到唯一在途名额")
	}
	if uid, ok := r.Resolve("s1"); !ok || uid != "b" {
		t.Fatalf("a 占满后重新分配=%q want b（顺序第一的可用号）ok=%v", uid, ok)
	}
}

// TestWeightedAssignKeepsDoubleStage 验收点 3 的前半：weighted（缺省）保留"空闲优先"
// 双段策略——只要还有空闲号，新会话就**不会**落到已被绑定的号上（改动前语义，零回归）。
//
// 判别构造（顺序刻意让哈希指向被绑定的那个号）：avail=[a1,a2]、a2 已被别的会话绑定，
// key=k1 的哈希下标在 2 元素池里是 1（= a2）。若 idle 段被跳过（或整段删除），k1 会
// 落到**已绑定**的 a2；保留 idle 段则落到唯一空闲号 a1——两种实现可直接区分。
func TestWeightedAssignKeepsDoubleStage(t *testing.T) {
	// 缺省模式（Sequential 零值）：a2 被占用 → 新会话必须摊到空闲的 a1。
	p := assignPool(t, []string{"a1", "a2"}, "a1", "a2")
	r := assignRouter(t, p, false)
	r.Bind("s1", "a2")
	if uid, ok := r.Resolve("k1"); !ok || uid != "a1" {
		t.Fatalf("weighted 分配=%q want a1（idle 段优先：a2 已被占用，哈希本会指向 a2）ok=%v", uid, ok)
	}
	// 反向构造（哈希本就指向空闲号）：a1 被占用 → 新会话落到空闲的 a2。
	p2 := assignPool(t, []string{"a1", "a2"}, "a1", "a2")
	r2 := assignRouter(t, p2, false)
	r2.Bind("s1", "a1")
	if uid, ok := r2.Resolve("k1"); !ok || uid != "a2" {
		t.Fatalf("weighted 分配=%q want a2（idle 段优先）ok=%v", uid, ok)
	}
	// idle 耗尽后回落全池哈希（双段的第二段）：a1/a2 都被占用 → k1 回到哈希口径
	// （2 元素池下标 1 → a2），即"没有空闲号时照旧哈希分散"，不是退化为固定取第一个。
	p3 := assignPool(t, []string{"a1", "a2"}, "a1", "a2")
	r3 := assignRouter(t, p3, false)
	r3.Bind("s1", "a1")
	r3.Bind("s2", "a2")
	if uid, ok := r3.Resolve("k1"); !ok || uid != "a2" {
		t.Fatalf("idle 耗尽后应回落全池哈希（k1→a2），got %q ok=%v", uid, ok)
	}
}

// goldenStep 一条 golden 断言：key → 期望 uid。
type goldenStep struct{ key, want string }

// goldenBind 预置绑定（模拟"已有会话占着某个号"）。
type goldenBind struct{ key, uid string }

// runGolden 在 fresh router 上按 steps 顺序 Resolve 并逐条比对（weighted 模式）。
func runGolden(t *testing.T, avail []string, preBinds []goldenBind, steps []goldenStep) {
	t.Helper()
	r := routerWith(newCountingStore(), avail, time.Hour)
	for _, pb := range preBinds {
		r.Bind(pb.key, pb.uid)
	}
	for _, s := range steps {
		uid, ok := r.Resolve(s.key)
		if !ok {
			t.Fatalf("avail=%v key=%s resolve failed", avail, s.key)
		}
		if uid != s.want {
			t.Fatalf("weighted golden 漂移：avail=%v preBinds=%v key=%s got=%s want=%s",
				avail, preBinds, s.key, uid, s.want)
		}
	}
}

// TestWeightedAssignGoldenHashUnchanged 验收点 3 的基线与 golden 对比：weighted 模式下
// 「同一组输入 → 同一批哈希分配结果」逐字节不变。
//
// 基线采集方式：在 main（改动前，AvailableUIDs 按 UID 升序）上跑同一段代码把
// key→uid 全量打印出来，再把这些字面量钉进本用例。**池侧另有等价性锚点**
// （pool.TestAvailableUIDsOrderEmptyEqualsUIDSort：无自定义顺序时 AvailableUIDs 与
// sort.Strings 逐元素相同），两条合起来覆盖「默认部署（无 account_order）下 weighted
// 粘性分配逐字节不变」。
//
// 注意：本用例的 Available 用**固定切片**（不接 pool），锁的是 Router 侧的挑选算法；
// 池侧排序等价性由 internal/pool 的用例负责——两层各自锚定，任一层回退都会红。
func TestWeightedAssignGoldenHashUnchanged(t *testing.T) {
	// 形态 1：2 个号、无既有绑定（idle = 全池）。
	runGolden(t, []string{"a1", "a2"}, nil, []goldenStep{
		{"k1", "a2"}, {"k2", "a1"}, {"k3", "a2"}, {"k4", "a1"}, {"k5", "a2"}, {"k6", "a1"},
		{"k7", "a2"}, {"k8", "a1"}, {"c1", "a2"}, {"c2", "a1"}, {"c3", "a2"}, {"x", "a2"},
		{"y", "a1"}, {"z", "a2"}, {"sess-1", "a2"}, {"sess-2", "a1"},
	})
	// 形态 2：3 个号、无既有绑定。
	runGolden(t, []string{"a1", "a2", "a3"}, nil, []goldenStep{
		{"k1", "a1"}, {"k2", "a2"}, {"k3", "a3"}, {"k4", "a1"}, {"k5", "a3"}, {"k6", "a3"},
		{"k7", "a2"}, {"k8", "a1"}, {"c1", "a3"}, {"c2", "a3"}, {"c3", "a2"}, {"x", "a1"},
		{"y", "a2"}, {"z", "a2"}, {"sess-1", "a3"}, {"sess-2", "a3"},
	})
	// 形态 3：2 个号且**两个都已被别的会话绑定**（idle 为空 → 回落全池哈希）。
	runGolden(t, []string{"a1", "a2"}, []goldenBind{{"b1", "a1"}, {"b2", "a2"}}, []goldenStep{
		{"k1", "a2"}, {"k2", "a1"}, {"k3", "a2"}, {"k4", "a1"}, {"k5", "a2"}, {"k6", "a1"},
		{"k7", "a2"}, {"k8", "a1"}, {"c1", "a2"}, {"c2", "a1"}, {"c3", "a2"}, {"x", "a2"},
		{"y", "a1"}, {"z", "a2"}, {"sess-1", "a2"}, {"sess-2", "a1"},
	})
	// 形态 4：a1 已被占用（idle = [a2]）→ 第一个新会话落到唯一空闲号 a2；此后两个号
	// 都被占用，idle 为空 → 回落全池哈希（序列与形态 1 相同——哈希口径完全接管）。
	runGolden(t, []string{"a1", "a2"}, []goldenBind{{"b1", "a1"}}, []goldenStep{
		{"k1", "a2"}, {"k2", "a1"}, {"k3", "a2"}, {"k4", "a1"}, {"k5", "a2"}, {"k6", "a1"},
		{"k7", "a2"}, {"k8", "a1"}, {"c1", "a2"}, {"c2", "a1"}, {"c3", "a2"}, {"x", "a2"},
		{"y", "a1"}, {"z", "a2"}, {"sess-1", "a2"}, {"sess-2", "a1"},
	})
	// 形态 5：4 个号、a1/a2 已占用（idle = [a3,a4]；idle 耗尽后回落全池哈希，
	// 序列里能看到两种口径的交替——正是"双段"的完整形态）。
	runGolden(t, []string{"a1", "a2", "a3", "a4"}, []goldenBind{{"b1", "a1"}, {"b2", "a2"}}, []goldenStep{
		{"k1", "a4"}, {"k2", "a3"}, {"k3", "a4"}, {"k4", "a3"}, {"k5", "a2"}, {"k6", "a1"},
		{"k7", "a4"}, {"k8", "a3"}, {"c1", "a2"}, {"c2", "a1"}, {"c3", "a4"}, {"x", "a4"},
		{"y", "a1"}, {"z", "a2"}, {"sess-1", "a2"}, {"sess-2", "a1"},
	})
}

// TestSequentialSwitchIsLive 验收点 7：模式开关可热切换——SetSequential 后**新会话**
// 立即按新模式分配（已建立的绑定不追溯改：粘性优先于模式）。
func TestSequentialSwitchIsLive(t *testing.T) {
	p := assignPool(t, []string{"b", "a"}, "a", "b")
	r := assignRouter(t, p, false) // 启动值 weighted

	if got := r.Sequential(); got {
		t.Fatalf("启动值 want weighted(false), got %v", got)
	}
	// weighted：k1 哈希落 a2 位（= a，顺序第二）。
	first, _ := r.Resolve("k1")
	if first != "a" {
		t.Fatalf("weighted 下 k1=%q want a（golden：哈希下标 1）", first)
	}
	// 热切到 sequential：新会话立刻取顺序第一 b。
	r.SetSequential(true)
	if !r.Sequential() {
		t.Fatal("SetSequential(true) 未生效")
	}
	for _, key := range []string{"k1b", "k2", "n1"} {
		if uid, ok := r.Resolve(key); !ok || uid != "b" {
			t.Fatalf("热切后 key=%s 分配=%q want b ok=%v", key, uid, ok)
		}
	}
	// 既有绑定（k1→a）保持粘性，不被模式切换追溯改写。
	if uid, ok := r.Resolve("k1"); !ok || uid != "a" {
		t.Fatalf("既有绑定不应被模式切换改写: k1=%q ok=%v", uid, ok)
	}
	// 切回 weighted：新会话恢复哈希分散（c1 哈希下标 1 → 列表第二个 a）。
	r.SetSequential(false)
	if r.Sequential() {
		t.Fatal("SetSequential(false) 未生效")
	}
	if uid, ok := r.Resolve("c1"); !ok || uid != "a" {
		t.Fatalf("切回 weighted 后新会话应哈希分配（c1→a）, got %q ok=%v", uid, ok)
	}
}

// TestSequentialAssignConcurrent 验收点 8：并发安全（-race 跑）。
// 多 goroutine 同时 assign + 并发 SetSequential/SetTTL 不得有数据竞争；顺序模式下
// 所有新会话都必须落在顺序第一的可用号上（集中填充的确定性）。
func TestSequentialAssignConcurrent(t *testing.T) {
	p := assignPool(t, []string{"b", "a"}, "a", "b")
	r := assignRouter(t, p, true)

	const goroutines = 16
	const perG = 50
	var wg sync.WaitGroup
	// 写侧：并发热切换模式（原子写，与分配读侧零耦合）。
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				r.SetSequential(true)
			} else {
				r.SetSequential(true) // 恒 true：本用例断言顺序模式的确定性
			}
			r.SetTTL(time.Hour)
		}
	}()
	// 读侧：并发 assign（不同 key）+ 命中复读。
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				key := "g" + strconv.Itoa(n) + "-" + strconv.Itoa(j)
				uid, ok := r.Resolve(key)
				if !ok {
					t.Errorf("resolve %s failed", key)
					return
				}
				if uid != "b" {
					t.Errorf("顺序模式下 %s 分配=%q want b", key, uid)
					return
				}
				if uid2, ok2 := r.Resolve(key); !ok2 || uid2 != uid {
					t.Errorf("同 key 二次 resolve 不一致: %q vs %q", uid, uid2)
					return
				}
			}
		}(g)
	}
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()

	if r.Count() != goroutines*perG {
		t.Errorf("绑定数=%d want %d", r.Count(), goroutines*perG)
	}
}
