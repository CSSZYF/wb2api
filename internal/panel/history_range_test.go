package panel

import (
	"fmt"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func TestPanelAllHistoryAndExplicitRange(t *testing.T) {
	p, rec := usagePanelFixture(t)
	now := time.Now().Truncate(time.Hour)
	rec.Add(now, "cn", "recent", "m", usage.Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	rec.Add(now.Add(-100*24*time.Hour), "global", "old", "m", usage.Delta{PromptTokens: 40, HasPromptTokens: true}, true)
	rec.Rollup(now)
	for _, tc := range []struct {
		query string
		want  float64
	}{
		{"", 5}, {"?hours=0", 45},
		{fmt.Sprintf("?from=%d&to=%d", now.Add(-time.Minute).Unix(), now.Add(time.Hour).Unix()), 5},
		{fmt.Sprintf("?from=%d&to=%d", now.Add(time.Hour).Unix(), now.Add(2*time.Hour).Unix()), 0},
	} {
		s := getUsage(t, p, tc.query)
		if got := s["totals"].(map[string]any)["prompt_tokens"]; got != tc.want {
			t.Fatalf("%s: tokens=%v, want %v", tc.query, got, tc.want)
		}
		if tc.query == "?hours=0" && (s["all_history"] != true || s["granularity"] != "day") {
			t.Fatalf("all history metadata: %v", s)
		}
	}
}
