// cooldown_hot_test.go 钉住 account fault（11140/14017）冷却基数取热改优先的
// softCooldown()：面板改 soft_rate 后这两条冷却必须立即生效，而不是永远用启动时
// 注入的静态 SoftCooldown。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestAccountFaultCooldownHotReload 11140 与 14017 两条 account fault 冷却都走
// h.softCooldown()：Live 快照热改后立即按新基数冷却。
func TestAccountFaultCooldownHotReload(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"11140 request illegal", `{"code":11140,"msg":"request illegal"}`},
		{"14017 trial not activated", `{"code":14017,"msg":"trial not activated"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := pool.New("")
			p.Add(&auth.Auth{UID: "u1"})
			// 静态字段 600s（启动值）；Live 快照 90s（面板热改后的值）。
			live := livecfg.New(livecfg.Snapshot{SoftCooldown: 90 * time.Second})
			h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second, Live: live})

			h.applyErrorPolicy("u1", upstream.ErrAccountFault, c.body, "glm-5.3", nil, nil)
			st, _ := p.Status("u1")
			if !st.Cooling {
				t.Fatalf("应进入冷却: %+v", st)
			}
			// 冷却基数取 Live（90s）而非静态（600s）。
			if st.CoolRemaining > 90 || st.CoolRemaining <= 0 {
				t.Errorf("cool_remaining_sec=%d want in (0,90]（热改基数立即生效，非静态 600s）", st.CoolRemaining)
			}

			// 再热改一次：30s，下一号立即按新值。
			live.Store(livecfg.Snapshot{SoftCooldown: 30 * time.Second})
			p.Add(&auth.Auth{UID: "u2"})
			h.applyErrorPolicy("u2", upstream.ErrAccountFault, c.body, "glm-5.3", nil, nil)
			st2, _ := p.Status("u2")
			if !st2.Cooling || st2.CoolRemaining > 30 || st2.CoolRemaining <= 0 {
				t.Errorf("u2 cool_remaining_sec=%d want in (0,30]（第二次热改同样生效）: %+v", st2.CoolRemaining, st2)
			}
		})
	}
}
