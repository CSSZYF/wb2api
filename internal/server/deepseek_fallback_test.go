package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

type fallbackCall struct{ model, token string }

func fallbackFixture(t *testing.T, ids ...string) (*Handler, *[]fallbackCall) {
	t.Helper()
	withGlobalEnabled(t)
	p := pool.New("")
	p.SetPickMode(pool.PickSequential)
	p.SetReserveCredits(50)
	for _, id := range ids {
		p.Add(&auth.Auth{UID: id, Domain: "www.workbuddy.ai", AccessToken: id, ExpiresAt: 9999999999})
		p.SetCredits(id, 100, 1000)
	}
	calls := []fallbackCall{}
	up := &upstream.Client{ChatBaseGlobal: "https://fake.example", HTTP: &http.Client{}}
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		m, _ := body["model"].(string)
		calls = append(calls, fallbackCall{m, r.Header.Get("Authorization")})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.ReplaceAll(sseOK, "glm-5.2", m)))}, nil
	})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmPrecedence: "global", StripRealmPrefix: true, DeepseekSGFallback: true, MaxRotate: 1, Usage: usage.New("")})
	return h, &calls
}
func exhaustFree(p *pool.Pool, ids ...string) {
	for _, id := range ids {
		p.CooldownSoftForModel(id, time.Minute, time.Now().Add(time.Hour), pool.DeepseekFreeModel, "6004 model rate limit")
	}
}
func fallbackRequest(h *Handler, model string, stream bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw))))
	return rec
}
func TestDeepseekFallbackFreeFirstRecoveryAndAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			h, calls := fallbackFixture(t, "a", "b")
			exhaustFree(h.cfg.Pool, "a")
			if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, stream); r.Code != 200 {
				t.Fatal(r.Code, r.Body)
			}
			if (*calls)[0].model != pool.DeepseekFreeModel || (*calls)[0].token != "Bearer b" {
				t.Fatal(*calls)
			}
			exhaustFree(h.cfg.Pool, "b")
			if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, stream); r.Code != 200 || !strings.Contains(r.Body.String(), pool.DeepseekPaidModel) {
				t.Fatal(r.Code, r.Body)
			}
			if (*calls)[1].model != pool.DeepseekPaidModel {
				t.Fatal(*calls)
			}
			snap := h.cfg.Usage.Snapshot(24, nil)
			found := false
			for _, row := range snap.ByModel {
				if row.Key == "global:"+pool.DeepseekPaidModel && row.Requests == 1 {
					found = true
				}
			}
			if !found {
				t.Fatalf("paid usage not attributed to actual model: %+v", snap.ByModel)
			}
			h.cfg.Pool.Revive("b")
			h.cfg.Pool.SetCredits("b", 1, 1000) // even below reserve, restored free quota wins
			if r := fallbackRequest(h, pool.DeepseekFreeModel, stream); r.Code != 200 {
				t.Fatal(r.Code, r.Body)
			}
			if (*calls)[2].model != pool.DeepseekFreeModel || (*calls)[2].token != "Bearer b" {
				t.Fatal(*calls)
			}
		})
	}
}
func TestDeepseekFallbackReserveAndUnknownBalance(t *testing.T) {
	for _, tc := range []struct {
		name           string
		credits, total int64
		want           bool
	}{
		{"above", 51, 1000, true}, {"at", 50, 1000, false}, {"below", 49, 1000, false}, {"unknown", 100, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := fallbackFixture(t, "a")
			h.cfg.Pool.SetCredits("a", tc.credits, tc.total)
			h.cfg.Pool.SetFreeModelLookup(func(string) bool { return true }) // catalog must not bypass paid reserve
			exhaustFree(h.cfg.Pool, "a")
			r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false)
			if (r.Code == 200) != tc.want || (len(*calls) > 0) != tc.want {
				t.Fatalf("code=%d calls=%v body=%s", r.Code, *calls, r.Body)
			}
		})
	}
}
func TestDeepseekFallbackOnlyQuotaExhaustion(t *testing.T) {
	for _, state := range []string{"empty", "account_cooldown", "model_missing", "busy", "disabled", "manual", "cn", "other", "off"} {
		t.Run(state, func(t *testing.T) {
			h, calls := fallbackFixture(t, "a")
			model := "global:" + pool.DeepseekFreeModel
			switch state {
			case "empty":
				h.cfg.Pool = pool.New("")
			case "account_cooldown":
				h.cfg.Pool.Cooldown("a", pool.CoolSoft, time.Hour, "WAF")
			case "model_missing":
				h.cfg.Pool.BlockModelBackoff("a", pool.DeepseekFreeModel, "11102")
			case "busy":
				h.cfg.Pool.SetMaxInFlightGlobal(1)
				h.cfg.Pool.Acquire("a")
				defer h.cfg.Pool.Release("a")
			case "disabled":
				h.cfg.Pool.Disable("a", "test")
			case "manual":
				h.cfg.Pool.SetManualDisabled("a", true, "test")
			case "cn":
				model = "cn:" + pool.DeepseekFreeModel
				exhaustFree(h.cfg.Pool, "a")
			case "other":
				model = "global:glm-5.3"
				exhaustFree(h.cfg.Pool, "a")
			case "off":
				h.cfg.Live = livecfg.New(livecfg.Snapshot{})
				exhaustFree(h.cfg.Pool, "a")
			}
			fallbackRequest(h, model, false)
			for _, c := range *calls {
				if c.model == pool.DeepseekPaidModel {
					t.Fatalf("unexpected paid call for %s", state)
				}
			}
		})
	}
}
func TestDeepseekFallbackAfterLastFreeAttempt(t *testing.T) {
	h, calls := fallbackFixture(t, "a")
	base := h.cfg.Upstream.HTTP.Transport
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var b map[string]any
		json.Unmarshal(raw, &b)
		if b["model"] == pool.DeepseekFreeModel {
			*calls = append(*calls, fallbackCall{pool.DeepseekFreeModel, r.Header.Get("Authorization")})
			until := time.Now().Add(time.Hour).In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
			return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":6004,"msg":"将在 ` + until + ` UTC+8 重置"}`))}, nil
		}
		return base.RoundTrip(r)
	})
	r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false)
	if r.Code != 200 || len(*calls) != 2 || (*calls)[1].model != pool.DeepseekPaidModel {
		t.Fatalf("last-free fallback: code=%d calls=%v body=%s", r.Code, *calls, r.Body)
	}
}
func TestDeepseekFallbackPaidUsageStopsAtReserve(t *testing.T) {
	h, calls := fallbackFixture(t, "a")
	h.cfg.Pool.SetCredits("a", 55, 1000)
	exhaustFree(h.cfg.Pool, "a")
	base := h.cfg.Upstream.HTTP.Transport
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		res, err := base.RoundTrip(r)
		if err != nil {
			return res, err
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		res.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(raw), `"total_tokens":2`, `"total_tokens":2,"credit":5`)))
		return res, nil
	})
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false); r.Code == 200 {
		t.Fatal("spent below reserve")
	}
	if len(*calls) != 1 {
		t.Fatal(*calls)
	}
}

func TestDeepseekFallbackHotSwitch(t *testing.T) {
	h, calls := fallbackFixture(t, "a")
	exhaustFree(h.cfg.Pool, "a")
	live := livecfg.New(livecfg.Snapshot{})
	h.cfg.Live = live
	fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false)
	if len(*calls) != 0 {
		t.Fatal("disabled flag spent credits")
	}
	live.Store(livecfg.Snapshot{DeepseekSGFallback: true})
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	live.Store(livecfg.Snapshot{})
	fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false)
	if len(*calls) != 1 {
		t.Fatal("hot disable did not stop paid fallback")
	}
}
