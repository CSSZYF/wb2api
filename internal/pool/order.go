// order.go 顺序填充式选号的「账号顺序」权威（pool.order / state.json 顶层
// account_order）：写入（SetOrder）、读取（Order）与内部的有效顺序视图
// （effectiveOrderLocked，List 与顺序选号共用）。
package pool

import "sort"

// SetOrder 设置用户指定的选号顺序（面板拖拽排序的落点；空数组 = 清除自定义顺序）。
//
// 语义（三条健壮性都刻意做成「读时收敛」而不是「写时校验」，见 effectiveOrderLocked）：
//   - 只原样记录 uid 列表，**不在这里**剔除未知 uid：顺序是运维意图的忠实记录，
//     账号因 auths 目录半截写入/临时移出池（SyncToDirExcept）而后又回来时，顺序仍在；
//   - 顺序里的已删除 uid 在读取时跳过，不影响其他账号的相对次序；
//   - 池内不在顺序里的账号（新加入）在读取时追加到**末尾**（尾部按 UID 升序保证稳定）；
//   - 重复 uid 只取首次出现。
//
// 立即落盘（同 Remove 口径）：顺序是运维动作，重启后必须还在（state.json 顶层键
// account_order，不塞进每个 account 对象）。
func (p *Pool) SetOrder(uids []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(uids) == 0 {
		if len(p.order) == 0 {
			return // 已是「无自定义顺序」：不产生无意义的落盘
		}
		p.order = nil // 空数组 = 清除自定义顺序 → 回落「按 UID 排序」（改动前行为）
	} else {
		p.order = append([]string(nil), uids...)
	}
	p.dirty.Store(true)
	p.saveLocked()
}

// Order 返回**有效顺序**（与 List() 和 sequential 选号同一口径）：
// 自定义顺序中仍在池内的 uid（跳过已删除、去重），后接池内未列出的账号（按 UID 升序）。
// 无自定义顺序时退化为「全部账号按 UID 升序」——即改动前的行为（零回归）。
//
// 返回副本：调用方可自由修改，不会影响池内状态。
func (p *Pool) Order() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.effectiveOrderLocked()
}

// effectiveOrderLocked 是顺序的单一权威实现（Order / List / 顺序选号共用）。
// 调用方必须已持 p.mu（读锁即可：只读 p.order 与 p.byUID）。
//
// 收敛规则见 SetOrder 注释；`order` 为空（旧 state.json 无 account_order 键，或
// 用户清除顺序）时输出等价于 sort.Strings(byUID keys)——保证「无顺序」与改动前
// 逐字节相同。
func (p *Pool) effectiveOrderLocked() []string {
	out := make([]string, 0, len(p.byUID))
	seen := make(map[string]bool, len(p.byUID))
	for _, uid := range p.order {
		if seen[uid] {
			continue // 重复 uid：只取首次出现
		}
		if _, ok := p.byUID[uid]; !ok {
			continue // 已删除 uid：跳过（其余账号相对次序不变）
		}
		seen[uid] = true
		out = append(out, uid)
	}
	if len(seen) == len(p.byUID) {
		return out // 池内账号全在顺序里：无需补尾部
	}
	rest := make([]string, 0, len(p.byUID)-len(seen))
	for uid := range p.byUID {
		if !seen[uid] {
			rest = append(rest, uid)
		}
	}
	sort.Strings(rest) // 尾部按 UID 升序：新账号的次序稳定可预期
	return append(out, rest...)
}

// expiringFirstOrderLocked 把一条账号顺序临时改排成「快过期账号优先」：
// creditsExpiring>0 的账号整体提前，内部按 creditsExpiringAt（该账号快过架子集
// 的最早到期时刻）升序；没有时间戳的旧状态排在有明确时间的快过期账号之后。
// 其余账号保持原相对顺序。它不改写 p.order（面板拖拽/运维指定顺序仍是权威），
// 只在 sequential 选号与粘性分配候选排序时现算。
func (p *Pool) expiringFirstOrderLocked(order []string) []string {
	if len(order) < 2 {
		return order
	}
	expiring := make([]string, 0, len(order))
	rest := make([]string, 0, len(order))
	for _, uid := range order {
		if e := p.byUID[uid]; e != nil && e.creditsExpiring > 0 {
			expiring = append(expiring, uid)
			continue
		}
		rest = append(rest, uid)
	}
	if len(expiring) < 2 {
		return append(expiring, rest...)
	}
	sort.SliceStable(expiring, func(i, j int) bool {
		ti := p.byUID[expiring[i]].creditsExpiringAt
		tj := p.byUID[expiring[j]].creditsExpiringAt
		if ti.IsZero() != tj.IsZero() {
			return !ti.IsZero() // 有明确到期时间的快过期账号排在时间未知者之前
		}
		return ti.Before(tj)
	})
	return append(expiring, rest...)
}

// sequentialPickOrderLocked sequential 模式实际使用的账号顺序：
// effectiveOrderLocked（运维/默认顺序）+ 快过期账号临时前置排序。
func (p *Pool) sequentialPickOrderLocked() []string {
	return p.expiringFirstOrderLocked(p.effectiveOrderLocked())
}

// sortByOrderLocked 把 uids（池内账号的一个子集，如"当前可用账号"）就地排成
// 选号顺序。weighted/无顺序时走原 sort.Strings / Pool.Order() 口径；sequential 时
// 改按 sequentialPickOrderLocked（快过期账号前置），让粘性路由给新会话分配的
// 首元素与 pickSequentialLocked 的首选保持同一口径。
func (p *Pool) sortByOrderLocked(uids []string) {
	if len(p.order) == 0 && p.pickMode != PickSequential {
		sort.Strings(uids)
		return
	}
	var order []string
	if p.pickMode == PickSequential {
		order = p.sequentialPickOrderLocked()
	} else {
		order = p.effectiveOrderLocked()
	}
	rank := make(map[string]int, len(order))
	for i, uid := range order {
		rank[uid] = i
	}
	sort.Slice(uids, func(i, j int) bool { return rank[uids[i]] < rank[uids[j]] })
}
