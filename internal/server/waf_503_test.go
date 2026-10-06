package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestChatWAF503DoesNotTripAccountBreaker(t *testing.T) {
	const page = `<html><head><title>WAF Block Page</title></head><body>Tencent Cloud WAF Access blocked Request UUID: test-waf-id</body></html>`
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 503, page, false })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`)))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "test-waf-id") {
		t.Fatalf("lost WAF response/UUID: %d %s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.Disabled || st.BreakerFails != 0 || !strings.Contains(st.Reason, "waf") {
		t.Fatalf("WAF 503 incorrectly penalized account: %+v", st)
	}
}
