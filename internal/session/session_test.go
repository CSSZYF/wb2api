package session

import (
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// countingStore 记录镜像调用次数的假 Store（不联网）。
type countingStore struct {
	redisstore.Noop
	mu       sync.Mutex
	setBinds int
	delBinds int
	binds    map[string]string
}

func newCountingStore() *countingStore {
	return &countingStore{binds: map[string]string{}}
}

func (c *countingStore) SetBind(key, uid string, ttl time.Duration) {
	c.mu.Lock()
	c.setBinds++
	c.binds[key] = uid
	c.mu.Unlock()
}
func (c *countingStore) DelBind(key string) {
	c.mu.Lock()
	c.delBinds++
	delete(c.binds, key)
	c.mu.Unlock()
}
func (c *countingStore) LoadBinds() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.binds {
		out[k] = v
	}
	return out
}

func routerWith(store redisstore.Store, avail []string, ttl time.Duration) *Router {
	return routerWithGC(store, avail, ttl, 0) // GCInterval 0 → New 兜底默认（5m）
}

// routerWithGC 同 routerWith，但显式注入 GCInterval。GCInterval 必须在 StartGC
// **之前**注入：StartGC 之后写 r.cfg.GCInterval 会与 GC goroutine 里的
// time.NewTicker(r.cfg.GCInterval) 竞争（cfg 非并发安全），测试自身即犯规。
func routerWithGC(store redisstore.Store, avail []string, ttl, gcInterval time.Duration) *Router {
	return New(Config{
		TTL:        ttl,
		GCInterval: gcInterval,
		Store:      store,
		Available:  func() []string { return avail },
	})
}

func TestSameKeySameAccount(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, ok1 := r.Resolve("c1")
	u2, ok2 := r.Resolve("c1")
	if !ok1 || !ok2 || u1 != u2 {
		t.Fatalf("same key should map to same account: %s vs %s", u1, u2)
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1", r.Count())
	}
}

func TestTTLExpiryReassigns(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, 10*time.Millisecond)
	u1, _ := r.Resolve("c1")
	time.Sleep(20 * time.Millisecond)
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve after expiry should still succeed")
	}
	// 过期后可重新分配（可能巧合同号，但至少返回有效账号）。
	_ = u1
	_ = u2
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (reassigned, not duplicated)", r.Count())
	}
}

func TestBoundAccountCooldownReassigns(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	u1, _ := r.Resolve("c1")
	if u1 != "a1" {
		t.Fatalf("initial bind=%s want a1", u1)
	}
	// a1 冷却 → 可用列表只剩 a2 → 重新分配必须换到 a2。
	r.cfg.Available = func() []string { return []string{"a2"} }
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed with fallback account")
	}
	if u2 == u1 {
		t.Fatalf("bound account %s cooled but still assigned", u1)
	}
	if u2 != "a2" {
		t.Fatalf("reassigned to %s want a2", u2)
	}
}

func TestNoSessionKeyPassthrough(t *testing.T) {
	// ExtractKey 找不到任何会话键 → 空串（调用方据空串走普通 Pick；router 不会被调用）。
	got := ExtractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id 优先
		{`{"conversation_id":"top"}`, "top"},                                                   // 顶层 conversation_id
		// P1-anti-monopoly：user_id 不再是粘性键（对话级粒度修正，键序四键全 conversation 维度）。
		{`{"metadata":{"user_id":"mu"}}`, ""},        // user_id 单独出现 → 空（不生成粘性）
		{`{"user_id":"mu"}`, ""},                     // 顶层 user_id 从未支持，保持空
		{`{"metadata":{"conversation_id":123}}`, ""}, // 非字符串 → 空
		{`not-json`, ""},                             // 非法 JSON → 空
		// issue #35：客户端实际发 camelCase conversationId，ExtractKey 必须识别。
		{`{"conversationId":"abc"}`, "abc"},                                            // 顶层 camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"},                               // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake 优先于 camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"},              // 顶层 snake 优先于 camel
		{`{"conversationId":123}`, ""},                                                 // 数字 conversationId → 空
		{`{"metadata":{"conversationId":456}}`, ""},                                    // metadata 数字 conversationId → 空
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"},                // camel conversationId 生效（user_id 不再抢占）
		// 剔除前 user_id 抢占顶层 conversation_id（旧键序 3 在 4 之前）；剔除后顶层 conversation_id 正常生效。
		{`{"metadata":{"user_id":"mu"},"conversation_id":"top"}`, "top"},
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

func TestConcurrentSameKeyAssignsOnce(t *testing.T) {
	avail := []string{"a1", "a2", "a3", "a4", "a5"}
	r := routerWith(newCountingStore(), avail, time.Minute)

	const N = 100
	uids := make([]string, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			u, ok := r.Resolve("same-key")
			if ok {
				uids[idx] = u
			}
		}(i)
	}
	wg.Wait()

	// 所有 goroutine 必须拿到同一个账号（写锁 re-check 防重复分配）。
	first := ""
	for _, u := range uids {
		if u == "" {
			t.Fatal("some goroutine failed to resolve")
		}
		if first == "" {
			first = u
		}
		if u != first {
			t.Fatalf("concurrent resolve assigned different accounts: %s vs %s", first, u)
		}
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (single binding)", r.Count())
	}
}

// boundUID 直接读绑定 uid（不触发 Resolve 的重分配），供 Bind 系列测试断言用（包内私有 helper）。
func (r *Router) boundUID(key string) (string, bool) {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	return e.uid, ok
}

func TestBindOverridesAndMirrors(t *testing.T) {
	// Bind 幂等覆盖旧值，并异步镜像 SetBind。
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	if u, ok := r.boundUID("c1"); !ok || u != "a1" {
		t.Fatalf("bind c1->a1 then bound=%s ok=%v", u, ok)
	}
	// 覆盖到 a2
	r.Bind("c1", "a2")
	if u, _ := r.boundUID("c1"); u != "a2" {
		t.Fatalf("bind override should map c1->a2, got %s", u)
	}
	if r.Count() != 1 {
		t.Errorf("bind override must not duplicate entries, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	binds := map[string]string{}
	for k, v := range st.binds {
		binds[k] = v
	}
	st.mu.Unlock()
	if n != 2 {
		t.Errorf("SetBind mirror count=%d want 2", n)
	}
	if binds["c1"] != "a2" {
		t.Errorf("mirrored bind should be a2, got %s", binds["c1"])
	}
}

func TestBindIgnoresEmptyKey(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("", "a1")
	r.Bind("c1", "")
	if r.Count() != 0 {
		t.Errorf("Bind with empty key/uid must be no-op, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	st.mu.Unlock()
	if n != 0 {
		t.Errorf("empty-key Bind must not mirror, setBinds=%d", n)
	}
}

func TestBindThenUnbindLifecycle(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	if !r.Unbind("c1") {
		t.Fatal("Unbind should report found")
	}
	if r.Count() != 0 {
		t.Errorf("count after unbind=%d want 0", r.Count())
	}
	st.mu.Lock()
	del := st.delBinds
	st.mu.Unlock()
	if del != 1 {
		t.Errorf("DelBind mirror count=%d want 1", del)
	}
}

func TestRedisMirrorSetBindCount(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")
	r.Resolve("c1") // 快路径 touch → 又镜像一次
	if st.setBinds < 1 {
		t.Errorf("SetBind mirror count=%d want >=1", st.setBinds)
	}
	r.Unbind("c1")
	if st.delBinds != 1 {
		t.Errorf("DelBind mirror count=%d want 1", st.delBinds)
	}
}

func TestGCCleansExpired(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, 10*time.Millisecond)
	r.Resolve("c1")
	r.Resolve("c2")
	time.Sleep(20 * time.Millisecond)
	removed := r.gcOnce(time.Now())
	if removed != 2 {
		t.Errorf("gc removed=%d want 2", removed)
	}
	if r.Count() != 0 {
		t.Errorf("count after gc=%d want 0", r.Count())
	}
}

// TestSetTTLShortenExpiresExisting 面板热改 TTL（缩短）后，既有绑定按**新值**判定过期：
// 同一份 entry（lastActive 未动）在 SetTTL 前有效、SetTTL 后立即失效并被重新分配。
// 判据用「双段分配」的空闲号优先：c1 原绑 a1，过期后 a1 仍是"唯一被占用的号"，
// 故 idle=[a2] 强制改绑 a2——若读取点仍读 cfg.TTL（启动初值 1h），这里必返回 a1。
func TestSetTTLShortenExpiresExisting(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Hour)
	u1, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("initial resolve should succeed")
	}
	// 把 lastActive 往回拨 30m：1h TTL 下未过期（应命中快路径、仍绑 u1）。
	backdateEntry(t, r, "c1", 30*time.Minute)
	if u2, ok := r.Resolve("c1"); !ok || u2 != u1 {
		t.Fatalf("binding should stay valid under 1h TTL: got %q want %q (ok=%v)", u2, u1, ok)
	}
	backdateEntry(t, r, "c1", 30*time.Minute) // 上一步 Resolve 会滚动 lastActive，重新拨回
	r.SetTTL(time.Minute)
	if got := r.TTL(); got != time.Minute {
		t.Fatalf("TTL()=%v want 1m", got)
	}
	u3, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed after TTL change")
	}
	if u3 == u1 {
		t.Fatalf("binding must be re-evaluated under new TTL: still %q (expired entry not detected)", u3)
	}
}

// TestSetTTLLengthenKeepsBinding 反向：TTL 放大后原本已过期的绑定恢复有效（不重分配）。
func TestSetTTLLengthenKeepsBinding(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, _ := r.Resolve("c1")
	backdateEntry(t, r, "c1", 30*time.Minute) // 1m TTL 下已过期
	r.SetTTL(time.Hour)
	u2, ok := r.Resolve("c1")
	if !ok || u2 != u1 {
		t.Fatalf("binding should be revived by longer TTL: got %q want %q (ok=%v)", u2, u1, ok)
	}
}

// TestSetTTLNonPositiveFallsBackDefault 面板把 ttl 清空/填 0 时回落默认 30m，
// 而不是把所有绑定瞬间判为过期（粘性全丢）。
func TestSetTTLNonPositiveFallsBackDefault(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, _ := r.Resolve("c1")
	backdateEntry(t, r, "c1", 5*time.Minute)
	r.SetTTL(0)
	if got := r.TTL(); got != 30*time.Minute {
		t.Fatalf("TTL()=%v want default 30m", got)
	}
	// 5m 老绑定在 30m 默认 TTL 下仍有效 → 快路径原样返回 u1（若被误判过期会改绑 a2）。
	if u2, ok := r.Resolve("c1"); !ok || u2 != u1 {
		t.Fatalf("5m-old binding must survive 30m default TTL: got %q want %q (ok=%v)", u2, u1, ok)
	}
}

// TestSetTTLConcurrentWithResolve 并发 SetTTL + Resolve + gcOnce 无数据竞争
// （-race 下验证原子 TTL 与 entries 锁无耦合；GC goroutine 不启动，只手动触发）。
func TestSetTTLConcurrentWithResolve(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2", "a3"}, time.Minute)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写侧：不停改 TTL（覆盖正常值与非法值回落分支）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 4 {
			case 0:
				r.SetTTL(time.Minute)
			case 1:
				r.SetTTL(2 * time.Hour)
			case 2:
				r.SetTTL(0)
			default:
				r.SetTTL(-time.Second)
			}
		}
	}()
	// 读侧：并发 Resolve（快路径 + 慢路径）与 GC（与写侧 TTL 变化同时发生）
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "c" + strconv.Itoa(n)
			for j := 0; j < 200; j++ {
				if _, ok := r.Resolve(key); !ok {
					t.Errorf("resolve %s failed", key)
					return
				}
				if j%50 == 0 {
					r.gcOnce(time.Now())
				}
			}
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// backdateEntry 把某绑定的 lastActive 往回拨 d（测试专用；与运行时写路径同锁语义）。
func backdateEntry(t *testing.T, r *Router, key string, d time.Duration) {
	t.Helper()
	r.mu.Lock()
	e, ok := r.entries[key]
	if ok {
		e.lastActive = e.lastActive.Add(-d)
		r.entries[key] = e
	}
	r.mu.Unlock()
	if !ok {
		t.Fatalf("entry %s not found for backdating", key)
	}
}

func TestLoadFromStoreRestores(t *testing.T) {
	st := newCountingStore()
	st.binds["c1"] = "a1"
	st.binds["c2"] = "a2"
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.LoadFromStore()
	if r.Count() != 2 {
		t.Fatalf("restored count=%d want 2", r.Count())
	}
	u, ok := r.Resolve("c1")
	if !ok || u != "a1" {
		t.Errorf("restored c1 -> %s want a1", u)
	}
}

// TestExtractKeyDerivedFromContent 客户端不发会话 id 时回退到内容派生键：
// 同一对话多轮（历史追加）→ 键稳定不变；不同对话 → 键不同。
func TestExtractKeyDerivedFromContent(t *testing.T) {
	// 第一轮
	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"}]}`
	// 第二轮：历史追加了 assistant 与新的 user（system 与首条 user 不变）
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"},{"role":"assistant","content":"好的"},{"role":"user","content":"换成快排"}]}`
	k1, k2 := ExtractKey([]byte(turn1)), ExtractKey([]byte(turn2))
	if k1 == "" {
		t.Fatal("derived key should not be empty when messages present")
	}
	if k1 != k2 {
		t.Errorf("derived key must be stable across turns: turn1=%q turn2=%q", k1, k2)
	}
	if !strings.HasPrefix(k1, "d-") {
		t.Errorf("derived key should carry prefix d-: %q", k1)
	}
	// 不同对话（首条 user 不同）→ 不同键
	other := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"翻译这段话"}]}`
	if ExtractKey([]byte(other)) == k1 {
		t.Error("different first user message must yield a different derived key")
	}
	// 显式 id 优先于派生键
	withID := `{"conversation_id":"my-session","messages":[{"role":"user","content":"帮我写个排序算法"}]}`
	if got := ExtractKey([]byte(withID)); got != "my-session" {
		t.Errorf("explicit id must win over derived key, got %q", got)
	}
	// 无 messages / 纯无文本内容 → 空（退回普通轮换，不误粘）
	if got := ExtractKey([]byte(`{"model":"x"}`)); got != "" {
		t.Errorf("no messages should yield empty key, got %q", got)
	}
	if got := ExtractKey([]byte(`{"messages":[{"role":"user","content":[]}]}`)); got != "" {
		t.Errorf("text-less content should yield empty key, got %q", got)
	}
}

// TestExtractKeyPromptCacheKey 第 5 来源 prompt_cache_key：pi-ai 系客户端把会话 ID
// 放在这个 OpenAI 前缀缓存字段里（而非 conversation_id），此前 ExtractKey 恒空 →
// 粘性永不参与、逐请求换号。置于 conversation 维度四键之后、内容派生之前。
func TestExtractKeyPromptCacheKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// 主用例：只有 prompt_cache_key（无任何 conversation 键）→ 取它。
		{"cache key only", `{"model":"m","prompt_cache_key":"pi-ai-sess-7","messages":[{"role":"user","content":"你好"}]}`, "pi-ai-sess-7"},
		// 优先级：绝不抢占 conversation 维度四键。
		{"top-level conversation_id wins", `{"conversation_id":"c1","prompt_cache_key":"pk","messages":[{"role":"user","content":"x"}]}`, "c1"},
		{"metadata conversation_id wins", `{"metadata":{"conversation_id":"mc"},"prompt_cache_key":"pk","messages":[{"role":"user","content":"x"}]}`, "mc"},
		{"camelCase conversationId wins", `{"conversationId":"c2","prompt_cache_key":"pk","messages":[{"role":"user","content":"x"}]}`, "c2"},
		// 空串 / 非字符串不算标识（strOrEmpty 口径）→ 回落内容派生。
		{"empty cache key falls through", `{"model":"m","prompt_cache_key":"","messages":[{"role":"user","content":"你好"}]}`, "d-46d71dd26ed9d7daba1a04dcc72be6d0"},
		{"non-string cache key falls through", `{"model":"m","prompt_cache_key":123,"messages":[{"role":"user","content":"你好"}]}`, "d-46d71dd26ed9d7daba1a04dcc72be6d0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractKey([]byte(c.body)); got != c.want {
				t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
			}
		})
	}
	// user_id 不抑制 prompt_cache_key：客户端显式声明的会话身份优先于
	// P1-anti-monopoly 的内容派生抑制（该抑制只作用于派生路径，见 deriveKey）。
	withUID := `{"prompt_cache_key":"pk","metadata":{"user_id":"u1"},"messages":[{"role":"user","content":"x"}]}`
	if got := ExtractKey([]byte(withUID)); got != "pk" {
		t.Errorf("user_id 不应抑制显式 prompt_cache_key: got %q", got)
	}
}

// TestExtractKeyBackwardCompatGolden 向后兼容 golden：改动**之前**采集的键值
// （v1.9.16 实测）在改动后必须逐字节不变——否则存量会话的粘性绑定会因键漂移而
// 全部失配（旧绑定仍占着 TTL，新键重新分配 → 同会话短时跨号）。
//
// 覆盖：字符串 content（含 system/developer 角色、空白、空串边界）、图文混合
// （文本优先，图片不入键）、以及各类空态。纯图片轮是唯一**有意**变更的形态
// （原为空串、现派生 [type:摘要] 键），由 TestExtractKeyImageOnlyFallback 覆盖。
func TestExtractKeyBackwardCompatGolden(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"system+首条 user", `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"}]}`,
			"d-e5f141cd029bf33ce1cf8fda869bcab4"},
		{"仅首条 user", `{"model":"m","messages":[{"role":"user","content":"你好"}]}`,
			"d-46d71dd26ed9d7daba1a04dcc72be6d0"},
		{"图文混合（文本优先）", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`,
			"d-883bdebc08b173ad8449b1956682a91d"},
		{"图文混合 文本+多图", `{"messages":[{"role":"user","content":[{"type":"text","text":"A"},{"type":"image_url","image_url":{"url":"http://x/y.png"}},{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`,
			"d-c00b4d3c929cb5cc316691ed4636f634"},
		{"developer 角色入键", `{"messages":[{"role":"developer","content":"dev"},{"role":"user","content":"q"}]}`,
			"d-7bc6fe0e72f9e38252cca0827c914e30"},
		// 空白**不**归一：旧实现按原文入哈希，若新版 trim 会让存量键全部漂移。
		{"首条 user 带首尾空白", `{"messages":[{"role":"user","content":"  hi  "}]}`,
			"d-4d36a26596e178427ece95f5dbe3ac85"},
		// 数组含非对象元素：旧 messageText 跳过该元素仍取到文本（不是整体失败）。
		{"数组含非对象元素", `{"messages":["junk",{"role":"user","content":"hi"}]}`,
			"d-e6908025cd50ce380feecfeaedb70ba2"},
		{"role 非字符串 + user", `{"messages":[{"role":123},{"role":"user","content":"hi"}]}`,
			"d-e6908025cd50ce380feecfeaedb70ba2"},
		{"首条 user 空串、第二条有文本", `{"messages":[{"role":"user","content":""},{"role":"assistant","content":"x"},{"role":"user","content":"第二问"}]}`,
			"d-26eb414d21f5b1cfed09a7458fed3648"},
		// 空态恒空串（不伪造键）。
		{"messages 非数组", `{"conversation_id":"c1","messages":"oops"}`, "c1"},
		{"无 messages", `{"model":"x"}`, ""},
		{"空数组 content", `{"messages":[{"role":"user","content":[]}]}`, ""},
		{"content 对象", `{"messages":[{"role":"user","content":{"text":"x"}}]}`, ""},
		{"content 数字", `{"messages":[{"role":"user","content":123}]}`, ""},
		{"文本 part 全空串", `{"messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractKey([]byte(c.body)); got != c.want {
				t.Errorf("键值漂移（存量粘性会失配）: ExtractKey=%q want %q", got, c.want)
			}
		})
	}
}

// TestExtractKeyImageOnlyFallback 纯图片轮的粘性盲区修复（G1 加重形态）：
// 首条 user 纯图片时 deriveKey 原返回空串 → 该类会话完全无粘性（逐请求换号）。
// 现由 contentSignatureAny 取 [type:摘要] 派生非空键，且会话内历史追加不换键。
func TestExtractKeyImageOnlyFallback(t *testing.T) {
	first := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`
	k := ExtractKey([]byte(first))
	if k == "" || !strings.HasPrefix(k, "d-") {
		t.Fatalf("纯图片首条 user 应派生非空粘性键（G1 修复）: got %q", k)
	}
	// 会话推进（历史追加、首条 user 不变）→ 同键。
	longer := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]},` +
		`{"role":"assistant","content":"答"},{"role":"user","content":"继续"}]}`
	if got := ExtractKey([]byte(longer)); got != k {
		t.Errorf("纯图会话历史追加不应换键: %q vs %q", got, k)
	}
	// 换图 → 换键（不同会话）。
	other := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/dog.png"}}]}]}`
	if got := ExtractKey([]byte(other)); got == k {
		t.Errorf("不同首图应不同键: %q", got)
	}
	// 键长有界：data: 超长内联图只入摘要。
	longDataURL := "data:image/png;base64," + strings.Repeat("QUFBQQ", 4096)
	long := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + longDataURL + `"}}]}]}`
	lk := ExtractKey([]byte(long))
	if lk == "" {
		t.Fatal("data: 超长内联图应仍派生键")
	}
	if len(lk) > 128 {
		t.Errorf("派生键长度应有界（摘要防超长）: len=%d", len(lk))
	}
	// 图片 part 键序/空白差异不换键（规范字节摘要）。
	reordered := `{"messages":[{"role":"user","content":[ { "image_url" : { "url" : "https://img.example/cat.png" } , "type" : "image_url" } ]}]}`
	if got := ExtractKey([]byte(reordered)); got != k {
		t.Errorf("part 键序/空白变化不应换键（规范摘要）: %q vs %q", got, k)
	}
}

// TestDeriveKeySuppressedByUserID P1-anti-monopoly 契约在**派生路径**的延伸：
// ExtractKey 有意剔除 user_id 作粘性键（user 维度粒度过粗），但内容派生是另一条
// 会给出非空键的路径——若不设闸，只发 user_id 的客户端会借首条 prompt 重新获得
// 粘性，使该契约失效（上游 10eefa8 修的同款回归；我们此前**没有**此闸门）。
//
// 与 TestUserIdNoLongerSticky 互补：后者只断言 ExtractKey 对"无 messages 的
// user_id body"返回空，覆盖不到"带 messages 时走派生路径"这一形态。
func TestDeriveKeySuppressedByUserID(t *testing.T) {
	// 闸门生效：两种 user_id 形态都恒空。
	for _, body := range []string{
		`{"model":"m","metadata":{"user_id":"u-42"},"messages":[{"role":"user","content":"帮我写代码"}]}`,
		`{"model":"m","user_id":"u-42","messages":[{"role":"user","content":"帮我写代码"}]}`,
		// 带 system 也照常抑制。
		`{"metadata":{"user_id":"u-42"},"messages":[{"role":"system","content":"sys"},{"role":"user","content":"q"}]}`,
	} {
		if got := ExtractKey([]byte(body)); got != "" {
			t.Errorf("带 user_id 的请求不应派生内容键（P1-anti-monopoly）: %s got %q", body, got)
		}
	}
	// 边界：非字符串 / 空串 user_id 不算标识 → 不抑制（与 strOrEmpty 口径一致）。
	for _, body := range []string{
		`{"metadata":{"user_id":123},"messages":[{"role":"user","content":"帮我写代码"}]}`,
		`{"metadata":{"user_id":""},"messages":[{"role":"user","content":"帮我写代码"}]}`,
		`{"metadata":{"user_id":null},"messages":[{"role":"user","content":"帮我写代码"}]}`,
	} {
		if got := ExtractKey([]byte(body)); got == "" {
			t.Errorf("非字符串/空 user_id 不应抑制派生: %s", body)
		}
	}
	// 不误伤真正需要粘性的客户端（无 user_id 的 OpenAI 兼容形态）。
	plain := `{"model":"m","messages":[{"role":"user","content":"帮我写代码"}]}`
	if got := ExtractKey([]byte(plain)); got == "" {
		t.Error("无 user_id 的请求应照常派生键")
	}
	// 显式 conversation 维度键不受抑制影响（user_id 只影响派生路径）。
	explicit := `{"metadata":{"user_id":"u-42","conversation_id":"c1"},"messages":[{"role":"user","content":"q"}]}`
	if got := ExtractKey([]byte(explicit)); got != "c1" {
		t.Errorf("显式 conversation_id 不受 user_id 抑制影响: got %q", got)
	}
}

// TestExtractKeyMultimodalContent 多模态 content 数组取文本部分派生。
func TestExtractKeyMultimodalContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k := ExtractKey([]byte(body))
	if k == "" || !strings.HasPrefix(k, "d-") {
		t.Fatalf("multimodal text should derive a key, got %q", k)
	}
	// 同一文本（图片不同）→ 同键（图片不参与派生，避免签名 URL 变化破坏粘性）
	body2 := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(body2)) != k {
		t.Error("image url changes must not break derived key stability")
	}
}

// TestUserIdNoLongerSticky P1-anti-monopoly：user_id 不再生成粘性——只发
// metadata.user_id 的客户端 ExtractKey 返回空（无粘性键），handler 侧 gate
// （sessKey != ""）不成立，Router 不会被咨询，同一 user 的并行对话不再钉同一
// 账号（回落加权轮换）。键序断言由 TestExtractKeyPriority 覆盖（user_id → ""）。
func TestUserIdNoLongerSticky(t *testing.T) {
	bodies := []string{
		`{"model":"m","metadata":{"user_id":"u-42"},"messages":[]}`,
		`{"model":"m","metadata":{"user_id":"u-42"},"conversation_id":"c1"}`, // user_id 不再抢占顶层键
	}
	for _, body := range bodies {
		if key := ExtractKey([]byte(body)); key == "" && strings.Contains(body, `"conversation_id":"c1"`) {
			// 第二条应取 conversation_id（非空）——防御本测试自身的构造错误。
			t.Fatalf("构造错误：含 conversation_id 的 body 不应返回空: %s", body)
		}
	}
	// 主断言：只发 user_id 的 body 无粘性键。
	if key := ExtractKey([]byte(bodies[0])); key != "" {
		t.Fatalf("user_id 不应再生成粘性键, got %q", key)
	}
	// conversation 变体不受影响：四种键形态照常提取。
	for _, body := range []string{
		`{"conversation_id":"c1"}`,
		`{"conversationId":"c1"}`,
		`{"metadata":{"conversation_id":"c1"}}`,
		`{"metadata":{"conversationId":"c1"}}`,
	} {
		if key := ExtractKey([]byte(body)); key != "c1" {
			t.Errorf("conversation 变体应照常提取: %s got %q", body, key)
		}
	}
}

// TestStopGCStopsGoroutine 关停语义回归：StopGC 后后台 GC goroutine 必须真的退出
// （N 轮「StartGC → StopGC」不泄漏 goroutine）。
//
// 原实现三处缺陷同时存在：① goroutine 在 select 里**每轮无锁重读** r.stop，
// 而 StopGC 持写锁把它置 nil —— 数据竞争（-race 报 Write@StopGC vs Read@StartGC.func1）；
// ② 一旦读到 nil，case <-r.stop 变成「select 里的 nil channel」永不就绪，关停信号
// 彻底丢失，goroutine 只能靠 ticker 无限空转；③ 于是 StopGC 之后 gcOnce 仍在跑，
// 每次 Start/Stop 循环净泄漏一个 goroutine。修复把 stop channel 捕获进局部变量，
// 让 close 与 select 观测同一个 channel。
//
// GCInterval 必须**构造时**注入（不能 StartGC 后再写 r.cfg）：goroutine 会读
// r.cfg.GCInterval 起 ticker，启动后写 cfg 本身就是数据竞争。
func TestStopGCStopsGoroutine(t *testing.T) {
	const rounds = 20
	baseline := runtime.NumGoroutine()

	for i := 0; i < rounds; i++ {
		r := routerWithGC(newCountingStore(), []string{"a1"}, time.Minute, time.Millisecond)
		r.StartGC()
		// 1ms tick：让 ticker 在 goroutine 退出前至少触发数轮，覆盖
		// 「select 重新求值 r.stop」的窗口（原缺陷的触发路径）。
		time.Sleep(2 * time.Millisecond)
		r.StopGC()
	}

	// 轮询等待所有 GC goroutine 退出（有界，防挂死）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("StopGC 后 goroutine 未退出（泄漏）: baseline=%d now=%d rounds=%d",
		baseline, runtime.NumGoroutine(), rounds)
}

// TestStartGCIdempotentAndRestartable StartGC 幂等、StopGC 后可重启：
// 重复 StartGC 不叠加 goroutine，StopGC 后再 StartGC 仍能恢复 GC
// （原实现 StopGC 把 r.stop 置 nil，goroutine 卡在 nil channel 上永不退出；
// 修复后 close 能被观测到，重启也能重新拉起一个可关停的 GC）。
func TestStartGCIdempotentAndRestartable(t *testing.T) {
	r := routerWithGC(newCountingStore(), []string{"a1"}, 10*time.Millisecond, 5*time.Millisecond)
	baseline := runtime.NumGoroutine()

	r.StartGC()
	r.StartGC() // 幂等：不应起第二个
	r.StartGC()
	time.Sleep(20 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > baseline+2 {
		t.Errorf("重复 StartGC 叠加了 goroutine: baseline=%d now=%d", baseline, n)
	}

	r.StopGC()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > baseline+1 {
		time.Sleep(5 * time.Millisecond)
	}

	// 重启：StopGC 已把 r.stop 置 nil，StartGC 应能重新拉起可用的 GC。
	// TTL=10ms、GCInterval=5ms：绑定建立后必然被重启的 GC 清掉。
	r.Resolve("c1")
	r.StartGC()
	r.Resolve("c2")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && r.Count() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.Count(); got != 0 {
		t.Errorf("重启后 GC 未生效: count=%d want 0", got)
	}
	r.StopGC()
}
