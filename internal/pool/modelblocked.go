package pool

import "time"

type ModelBlockStatus struct {
	Blocked bool      // true = 每个非禁用账号都对该模型处于冷却中
	Reason  string    // 冷却原因（通常为上游原文，如 "11102 model ... not found"）
	Until   time.Time // 最早解封时间（零值 = 上游未给重置时刻）
	Count   int       // 因此被挡的账号数
}

// ModelBlocked 报告该模型是否在全池范围内被模型级冷却挡住。
//
// 为什么需要它：选号失败时客户端只会拿到 no_healthy_account（"没有可用账号"），
// 但真实原因常常是「号都在、只是都对这个模型关闭」。两者对调用方的处置完全不同
// ——前者该等，后者换个模型才有用——此前却无法区分：首次请求还能看到上游原文
// （lastErr 非空），一旦负缓存写入，后续请求 lastErr 为空，就只剩"池子没号"
// （issue #102 附带发现 1）。上游原文与解封时间在那里被丢掉。
//
// 跨 realm 判定：调用方失败前已依次尝试过各域，所以只要有**任意**账号还能服务该
// 模型，就不能算全池阻塞 —— 此时返回 Blocked=false，让上层继续用原有的
// no_healthy_account 文案（选号失败另有原因：在途占满/积分保底/账号级冷却）。
//
// 口径必须与选号一致：用 modelCooled 而非直接查 map，这样 无有效截止时间的条目
// （只审计不拦截）不会被误报成阻塞。
func (p *Pool) ModelBlocked(model, realm string) ModelBlockStatus {
	if model == "" {
		return ModelBlockStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	var st ModelBlockStatus
	for _, e := range p.byUID {
		// 暂停选号（paused）号与禁用号同口径跳过：它此刻不可选，「能服务」的
		// 证明不成立——否则一个暂停的健康号会掩盖「其余号全被模型级冷却挡住」。
		if e.disabled || e.manualDisabled || (realm != "" && e.a.Realm() != realm) {
			continue // 禁用/暂停号不参与：它们的不可用与模型无关
		}
		if !e.modelCooled(now, model) {
			// 还有账号能服务这个模型 → 不是模型级阻塞。
			return ModelBlockStatus{}
		}
		st.Count++
		if mc, ok := e.modelCooldowns[model]; ok {
			if st.Reason == "" {
				st.Reason = mc.Reason
			}
			// 取最早解封：那才是"再等多久值得重试"的答案。
			if !mc.Until.IsZero() && (st.Until.IsZero() || mc.Until.Before(st.Until)) {
				st.Until = mc.Until
			}
		}
	}
	if st.Count == 0 {
		// 池里压根没有非禁用账号：这是"真的没号"，不是模型问题。
		return ModelBlockStatus{}
	}
	st.Blocked = true
	return st
}
