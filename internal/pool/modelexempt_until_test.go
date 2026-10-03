// modelexempt_until_test.go 钉住 modelExempt 的账号级冷却（until）排除：
// 全冷却兜底选中软冷却号 → 撞 6004（带重置）→ CooldownSoftForModel 写
// modelCooldowns 而 until 仍在未来 → /healthz 报 servable 而 chat 选号实际
// 无候选（口径裂缝）。until 非零（冷却被施加过）即不算豁免形态。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelExemptExcludesAccountCooldown until 未到期的账号即便有 6004 模型级冷却
// 条目，也不得被算作「模型豁免」可服务形态——否则探活口径与 chat 选号分裂。
func TestModelExemptExcludesAccountCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 账号级软冷却（until 在未来）+ 模型级 6004 条目并存。
	p.CooldownSoftRate("u1", time.Minute, time.Now().Add(time.Hour), "429 rate limit")
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	if p.ServableNow() {
		t.Error("账号级冷却(until) + 模型级冷却并存时不应报可服务：until 是整体出池")
	}
	if p.ServableForRealm("cn") {
		t.Error("ServableForRealm(cn) 同样应 false")
	}

	// 对照 1：无账号级冷却的模型豁免形态照旧可服务（issue #31 语义不回归）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u2"})
	p2.mu.Lock()
	p2.byUID["u2"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p2.mu.Unlock()
	if !p2.ServableNow() {
		t.Error("仅模型级冷却（无账号级 until）应保持可服务（豁免语义）")
	}

	// 对照 2：until 已过期（冷却自然到期）后恢复可服务（healthy 覆盖，不产生假阴性）。
	p3 := New("")
	p3.Add(&auth.Auth{UID: "u3"})
	p3.mu.Lock()
	e := p3.byUID["u3"]
	e.until = time.Now().Add(-time.Minute)
	e.coolKind = CoolSoft
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p3.mu.Unlock()
	if !p3.ServableNow() {
		t.Error("账号级冷却已过期 + 模型级冷却条目应恢复可服务（形态判定只看字段被设过）")
	}
}
