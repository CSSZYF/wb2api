// available_order_test.go AvailableUIDs 系列按**选号顺序**（Pool.Order() 口径）输出。
//
// 为什么必须改：会话粘性的「新会话绑定到哪个号」走 AvailableUIDs(ForModel[Realm])
// 的第一个候选（internal/session 的 assign），而此前这两个函数内部是 sort.Strings
// ——UID 升序。于是即使开了 pool.pick_mode=sequential，顺序第一的号也排不到候选列表
// 首位，粘性分配永远绕开顺序模式（「绝大头在第一个号」不成立）。
//
// 零回归约束（本文件逐条锁定）：无自定义顺序（旧 state.json 无 account_order 键，
// 或用户清除顺序）时输出必须与改动前的 sort.Strings **逐元素相同**——因为
// effectiveOrderLocked 在 order 为空时退化为「按 UID 升序」。
package pool

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// orderPool 构造一个「加入次序与 UID 字典序刻意相反」的池：UID 升序 = [a1 a2 a3]，
// 自定义顺序 = [a3 a1 a2]。两种排序口径的输出可直接区分（这正是本缺陷的判别手段）。
func orderPool(t *testing.T, order []string, uids ...string) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
	}
	if order != nil {
		p.SetOrder(order)
	}
	return p
}

// TestAvailableUIDsFollowsOrder 核心：自定义顺序生效时按 order 输出，而不是 UID 升序。
// 实现前必红（现状 sort.Strings → [a1 a2 a3]，want [a3 a1 a2]）。
func TestAvailableUIDsFollowsOrder(t *testing.T) {
	p := orderPool(t, []string{"a3", "a1", "a2"}, "a1", "a2", "a3")
	got := p.AvailableUIDs()
	want := []string{"a3", "a1", "a2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDs()=%v want %v（必须与 Pool.Order() 同口径）", got, want)
	}
	if !reflect.DeepEqual(got, p.Order()) {
		t.Errorf("AvailableUIDs() 与 Order() 必须同源: %v vs %v", got, p.Order())
	}
}

// TestAvailableUIDsForModelFollowsOrder 模型口径同源；被过滤掉的账号不得打乱其余账号
// 的相对次序（过滤保序，不是重新排序）。
func TestAvailableUIDsForModelFollowsOrder(t *testing.T) {
	p := orderPool(t, []string{"a3", "a1", "a2"}, "a1", "a2", "a3")
	if got, want := p.AvailableUIDsForModel(""), []string{"a3", "a1", "a2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDsForModel(\"\")=%v want %v（model 空必须等价 AvailableUIDs）", got, want)
	}
	// a3 在该模型被 6004 限流 → 从列表消失，a1/a2 相对次序不变。
	p.CooldownSoftForModel("a3", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")
	if got, want := p.AvailableUIDsForModel("glm-5.2"), []string{"a1", "a2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDsForModel(glm-5.2)=%v want %v（过滤保序）", got, want)
	}
	// 其他模型：模型豁免照常（a3 只对 glm-5.2 不可用）。
	if got, want := p.AvailableUIDsForModel("glm-5.3"), []string{"a3", "a1", "a2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDsForModel(glm-5.3)=%v want %v（模型豁免 + 顺序）", got, want)
	}
}

// TestAvailableUIDsOrderEmptyEqualsUIDSort 零回归锚点：无自定义顺序（order 为空，
// 即旧 state.json 无 account_order 键）时输出与改动前的 sort.Strings 逐元素相同。
func TestAvailableUIDsOrderEmptyEqualsUIDSort(t *testing.T) {
	p := orderPool(t, nil, "a3", "a1", "a2") // 未 SetOrder：order 为空
	want := []string{"a1", "a2", "a3"}
	if got := p.AvailableUIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("无顺序时 AvailableUIDs()=%v want %v（= sort.Strings，零回归）", got, want)
	}
	if got := p.AvailableUIDsForModel("glm-5.2"); !reflect.DeepEqual(got, want) {
		t.Fatalf("无顺序时 AvailableUIDsForModel()=%v want %v（零回归）", got, want)
	}
	// 清除自定义顺序（空数组）后回落 UID 排序。
	p.SetOrder([]string{"a3", "a1", "a2"})
	if got := p.AvailableUIDs(); !reflect.DeepEqual(got, []string{"a3", "a1", "a2"}) {
		t.Fatalf("SetOrder 后 AvailableUIDs()=%v want [a3 a1 a2]", got)
	}
	p.SetOrder(nil)
	if got := p.AvailableUIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("清除顺序后 AvailableUIDs()=%v want %v（回落 UID 排序）", got, want)
	}
}

// TestAvailableUIDsOrderUnlistedAppended 顺序未列出的新账号追加到末尾（尾部按 UID
// 升序），且顺序里的已删除/未知 uid 被跳过——与 effectiveOrderLocked 的收敛规则一致。
func TestAvailableUIDsOrderUnlistedAppended(t *testing.T) {
	p := orderPool(t, []string{"a3", "ghost", "a1"}, "a1", "a2", "a3", "a4")
	want := []string{"a3", "a1", "a2", "a4"}
	if got := p.AvailableUIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDs()=%v want %v（未知 uid 跳过 + 未列出账号按 UID 升序追加）", got, want)
	}
}

// TestAvailableUIDsRealmFollowsOrder realm 分池口径同源：生产路径
// （cmd/server/wiring.go 的 AvailableUIDsForModelRealm）必须按 order 过滤，而不是
// 重新按 UID 排序——否则混合池下粘性新会话仍拿不到顺序第一的本域账号。
func TestAvailableUIDsRealmFollowsOrder(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})
	// 顺序刻意让 global 号夹在中间，且 cn 域内次序 cn2 → cn1（与 UID 升序相反）。
	p.SetOrder([]string{"cn2", "g1", "cn1"})

	if got, want := p.AvailableUIDsForRealm("cn"), []string{"cn2", "cn1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(cn)=%v want %v（域过滤保序）", got, want)
	}
	if got, want := p.AvailableUIDsForRealm("global"), []string{"g1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(global)=%v want %v", got, want)
	}
	// realm=="" 退化为 AvailableUIDs（现状语义），同样按 order。
	if got, want := p.AvailableUIDsForRealm(""), []string{"cn2", "g1", "cn1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(\"\")=%v want %v（退化为 AvailableUIDs）", got, want)
	}
	if got, want := p.AvailableUIDsForModelRealm("", "cn"), []string{"cn2", "cn1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(\"\",cn)=%v want %v", got, want)
	}
	if got, want := p.AvailableUIDsForModelRealm("", "global"), []string{"g1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(\"\",global)=%v want %v", got, want)
	}
}

// TestAvailableUIDsRealmOrderEmptyEqualsUIDSort realm 口径的零回归：无自定义顺序时
// 仍是「UID 升序 + 域过滤」（与既有 realm_test.go 的断言逐元素一致）。
func TestAvailableUIDsRealmOrderEmptyEqualsUIDSort(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := realmPool(t) // 既有 helper：cn1/cn2/g1/g2，无自定义顺序
	if got, want := p.AvailableUIDsForRealm("cn"), []string{"cn1", "cn2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("无顺序时 AvailableUIDsForRealm(cn)=%v want %v（零回归）", got, want)
	}
	if got, want := p.AvailableUIDsForRealm(""), []string{"cn1", "cn2", "g1", "g2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("无顺序时 AvailableUIDsForRealm(\"\")=%v want %v（零回归）", got, want)
	}
}

// TestAvailableUIDsUnhealthyKeepsOrder 健康过滤保序：中间账号不健康时，其余账号的
// 相对次序仍是 order 里的次序（不是过滤后重排）。
func TestAvailableUIDsUnhealthyKeepsOrder(t *testing.T) {
	p := orderPool(t, []string{"a3", "a2", "a1"}, "a1", "a2", "a3")
	p.Cooldown("a2", CoolSoft, 10*time.Minute, "x") // 中间那个不健康
	if got, want := p.AvailableUIDs(), []string{"a3", "a1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDs()=%v want %v（健康过滤保序）", got, want)
	}
	// 在途占满同样保序（顺序模式的「溢出」正是靠这一条：第一个满了，列表第一个
	// 变成下一个号）。
	p2 := orderPool(t, []string{"a3", "a1", "a2"}, "a1", "a2", "a3")
	p2.SetMaxInFlight(1)
	p2.Acquire("a3")
	if got, want := p2.AvailableUIDs(), []string{"a1", "a2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDs()=%v want %v（在途占满过滤保序）", got, want)
	}
}

// TestSortByOrderLockedMatchesEffectiveOrder 排序辅助（sortByOrderLocked，顺序模式的
// 排序落点）必须与顺序权威（effectiveOrderLocked / Order()）**逐元素同序**——它是
// AvailableUIDs 系列与 Pool.Order() 同口径的机制保证（顺序的收敛规则只实现一处：
// 新账号追加末尾、已删除跳过、重复去重、无顺序回落 UID 升序）。
//
// 覆盖各种收敛形态：全量、子集（部分不可用）、乱序自定义顺序、未列出新账号、
// 已删除 uid、重复 uid、空顺序回落。
func TestSortByOrderLockedMatchesEffectiveOrder(t *testing.T) {
	cases := []struct {
		name  string
		order []string
		uids  []string // 池内账号（加入次序刻意与 UID 升序不同）
		sub   []string // 待排序子集（模拟"过滤后的可用账号"）
	}{
		{"无自定义顺序", nil, []string{"b", "a", "c"}, []string{"c", "a"}},
		{"空顺序显式清除", []string{}, []string{"b", "a"}, []string{"a", "b"}},
		{"全量自定义顺序", []string{"c", "a", "b"}, []string{"a", "b", "c"}, []string{"b", "c", "a"}},
		{"子集（部分不可用）", []string{"c", "a", "b"}, []string{"a", "b", "c"}, []string{"b", "c"}},
		{"顺序含已删除 uid", []string{"ghost", "c", "b"}, []string{"a", "b", "c"}, []string{"a", "c", "b"}},
		{"顺序含重复 uid", []string{"c", "c", "a"}, []string{"a", "b", "c"}, []string{"b", "a", "c"}},
		{"未列出的新账号在末尾", []string{"c"}, []string{"b", "a", "c"}, []string{"a", "b", "c"}},
		{"子集单元素", []string{"c", "a", "b"}, []string{"a", "b", "c"}, []string{"b"}},
		{"子集为空", []string{"c", "a"}, []string{"a", "b", "c"}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := orderPool(t, c.order, c.uids...)
			// 权威顺序里过滤出子集（保序）——这是 AvailableUIDs 的真实形态。
			want := make([]string, 0, len(c.sub))
			inSub := map[string]bool{}
			for _, u := range c.sub {
				inSub[u] = true
			}
			for _, uid := range p.Order() {
				if inSub[uid] {
					want = append(want, uid)
				}
			}
			got := append([]string(nil), c.sub...)
			p.sortByOrderLocked(got)
			if len(want) != len(got) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
				t.Fatalf("sortByOrderLocked(%v)=%v want %v（必须与 Order() 同序）", c.sub, got, want)
			}
		})
	}
}

// TestSortByOrderLockedEmptyOrderEqualsSortStrings 零回归机制锚点：无自定义顺序时
// sortByOrderLocked 走的就是改动前的 sort.Strings 原路径（含边界：nil / 空 / 单元素 /
// 已含重复的输入），保证默认部署的 AvailableUIDs 分配特征与改动前逐字节相同。
func TestSortByOrderLockedEmptyOrderEqualsSortStrings(t *testing.T) {
	p := orderPool(t, nil, "a1", "a2", "a3")
	cases := [][]string{
		nil,
		{},
		{"a2"},
		{"a3", "a1", "a2"},
		{"a2", "a2", "a1"}, // 重复元素：sort.Strings 原样保留重复（不静默去重）
		{"b", "a"},
	}
	for _, in := range cases {
		got := append([]string(nil), in...)
		p.sortByOrderLocked(got)
		want := append([]string(nil), in...)
		sort.Strings(want)
		if len(want) != len(got) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("无顺序时 sortByOrderLocked(%v)=%v want %v（= sort.Strings，零回归）", in, got, want)
		}
	}
}
