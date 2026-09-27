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

// sortByOrderLocked 把 uids（池内账号的一个子集，如"当前可用账号"）就地排成
// **选号顺序**——即 Pool.Order() 的口径（effectiveOrderLocked 是顺序的单一权威，
// 本函数只借它的输出取 rank）。调用方必须已持 p.mu（读锁即可）。
//
// 两条路径（与 effectiveOrderLocked 的收敛规则一一对应）：
//   - **无自定义顺序**（p.order 为空 = 旧 state.json / 用户清除顺序）：走 sort.Strings
//     ——改动前的原路径。此时 effectiveOrderLocked 本就把全部账号按 UID 升序输出
//     （seen 为空 → 尾部补全 = 全量 UID 升序），两条路径输出逐元素相同
//     （等价性由 TestAvailableUIDsOrderEmptyEqualsUIDSort 锁定）。之所以短路，
//     是因为本函数在**请求路径**上被调用（internal/session 的 availableSet 每请求一次）：
//     order 为空时构造完整顺序列表 + seen 映射纯属浪费（46 账号实测 1 alloc/2.4µs
//     vs 6 allocs/3.8µs），默认部署应保持改动前的分配特征。
//   - **有自定义顺序**：按 rank 升序排。rank 唯一（effectiveOrderLocked 对 p.byUID
//     每个 uid 恰好输出一次，uids ⊆ p.byUID），故排序结果就是"顺序里该子集的原序"，
//     与逐项遍历顺序等价；不依赖排序稳定性。
//
// 成本（实测 46 账号）：无顺序路径与原实现同量级（1 alloc / 768B / ~2.4µs）；有顺序
// 路径因每次构造完整顺序视图 + rank 映射为 10 allocs / ~10µs。取舍是**刻意**的：
// 本函数在请求路径上每次调用一次（session.availableSet），10µs 相对上游秒级响应可忽略
// （千 RPS 下约 0.75% 单核），而"顺序的收敛规则只实现一处"（effectiveOrderLocked）换来
// 的是不会与新账号追加/已删除跳过等规则漂移——收益大于这点开销。
func (p *Pool) sortByOrderLocked(uids []string) {
	if len(p.order) == 0 {
		sort.Strings(uids)
		return
	}
	rank := make(map[string]int, len(uids))
	for i, uid := range p.effectiveOrderLocked() {
		rank[uid] = i
	}
	sort.Slice(uids, func(i, j int) bool { return rank[uids[i]] < rank[uids[j]] })
}
