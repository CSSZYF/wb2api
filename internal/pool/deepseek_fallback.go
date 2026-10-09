package pool

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"time"
)

const DeepseekFreeModel = "deepseek-v4.1-flash"
const DeepseekPaidModel = "deepseek-v4.1-flash-sg"

// PickDeepseekPaidFallback only selects healthy global accounts with a known
// balance above the reserve. Check the free pool under the same lock so a
// restored free account wins over SG. No cooling-account probe or CN fallback.
func (p *Pool) PickDeepseekPaidFallback(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if _, exhausted := p.modelRateLimitExhaustedLocked(DeepseekFreeModel, "global", now); !exhausted {
		return nil
	}
	gate := reserveGate{p: p, active: true, line: p.reserveCredits, requireKnown: true}
	return p.pickHealthyLocked(tried, now, DeepseekPaidModel, "global", gate)
}
