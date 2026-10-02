package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestProbeDeepSeekPromo 彻底搜索：4.1-flash 的免费折扣到底在哪。
// 手动运行：go test ./internal/upstream/ -run TestProbeDeepSeekPromo -v -timeout 180s
func TestProbeDeepSeekPromo(t *testing.T) {
	dir := os.Getenv("PROBE_AUTHS")
	if dir == "" {
		dir = `D:/wb2tmp/probe/fixed/auths`
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no auths dir: %v", err)
	}
	var ga *auth.Auth
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var f struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
				Domain      string `json:"domain"`
				Realm       string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID string `json:"uid"`
			} `json:"account"`
		}
		if json.Unmarshal(raw, &f) != nil || f.Auth.AccessToken == "" || f.Auth.Realm != "global" {
			continue
		}
		ga = &auth.Auth{UID: f.Account.UID, Domain: f.Auth.Domain, AccessToken: f.Auth.AccessToken}
		t.Logf("账号 uid=%s realm=%s", f.Account.UID[:8], f.Auth.Realm)
		break
	}
	if ga == nil {
		t.Skip("no global auth")
	}

	c := New()
	c.GlobalEnabled = true
	base := c.chatBase(ga)

	fetch := func(label, url string, hdr map[string]string) []byte {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil
		}
		c.CommonHeaders(req, ga)
		c.injectAccountStableHeaders(req, ga)
		req.Header.Set("Authorization", "Bearer "+ga.AccessTokenValue())
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Logf("[%s] err=%v", label, err)
			return nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		t.Logf("[%s] status=%d len=%d", label, resp.StatusCode, len(body))
		return body
	}

	// ============ 1) 企业端点：全文搜 deepseek / 4.1 ============
	ent := fetch("enterprise", base+"/v2/enterprises/personal/models", nil)
	if len(ent) > 0 {
		low := strings.ToLower(string(ent))
		for _, kw := range []string{"deepseek", "4.1", "free", "trial", "promo", "discount"} {
			n := strings.Count(low, kw)
			t.Logf("  enterprise 含 %-10q x%d", kw, n)
		}
		// 所有含 deepseek 的片段
		re := regexp.MustCompile(`(?i).{0,200}deepseek.{0,300}`)
		for i, m := range re.FindAllString(string(ent), 8) {
			t.Logf("  [ent-deepseek#%d] %s", i, strings.ReplaceAll(m, "\n", " "))
		}
	}

	// ============ 2) v3-IDE：全文搜 ============
	ide := fetch("v3-IDE", base+"/v3/config", map[string]string{"User-Agent": codeBuddyIDEUA})
	if len(ide) > 0 {
		low := strings.ToLower(string(ide))
		for _, kw := range []string{"deepseek", "4.1", "free", "trial", "promo", "discount"} {
			t.Logf("  v3-IDE 含 %-10q x%d", kw, n2(low, kw))
		}
		re := regexp.MustCompile(`(?i).{0,200}deepseek.{0,300}`)
		for i, m := range re.FindAllString(string(ide), 8) {
			t.Logf("  [ide-deepseek#%d] %s", i, strings.ReplaceAll(m, "\n", " "))
		}
		// modelPromotions 全文
		var env map[string]json.RawMessage
		if json.Unmarshal(ide, &env) == nil {
			if d, ok := env["data"]; ok {
				var obj map[string]json.RawMessage
				if json.Unmarshal(d, &obj) == nil {
					if pm, ok := obj["modelPromotions"]; ok {
						t.Logf("  v3-IDE modelPromotions FULL: %s", truncate(string(pm), 4000))
					}
				}
			}
		}
	}

	// ============ 3) v3-CLI ============
	cli := fetch("v3-CLI", base+"/v3/config", map[string]string{"User-Agent": codeBuddyCLIUA})
	if len(cli) > 0 {
		low := strings.ToLower(string(cli))
		for _, kw := range []string{"deepseek", "4.1", "free", "trial", "promo", "discount"} {
			t.Logf("  v3-CLI 含 %-10q x%d", kw, n2(low, kw))
		}
	}

	// ============ 4) 企业端点里 4.1 那条模型的**完整字段** ============
	if len(ent) > 0 {
		var env map[string]json.RawMessage
		if json.Unmarshal(ent, &env) == nil {
			if d, ok := env["data"]; ok {
				var obj map[string]json.RawMessage
				if json.Unmarshal(d, &obj) == nil {
					if ms, ok := obj["models"]; ok {
						var arr []map[string]json.RawMessage
						if json.Unmarshal(ms, &arr) == nil {
							for _, m := range arr {
								var id string
								_ = json.Unmarshal(m["id"], &id)
								if strings.Contains(id, "4.1") {
									keys := make([]string, 0, len(m))
									for k := range m {
										keys = append(keys, k)
									}
									sort.Strings(keys)
									t.Logf("  === 企业端点模型 %s 的全部字段 ===", id)
									for _, k := range keys {
										t.Logf("      %-24s %s", k, truncate(string(m[k]), 300))
									}
								}
							}
						}
					}
					// productFeaturesConfig 里有没有 promo
					if pfc, ok := obj["productFeaturesConfig"]; ok {
						t.Logf("  enterprise productFeaturesConfig: %s", truncate(string(pfc), 1500))
					}
				}
			}
		}
	}

	// ============ 4b) v3-CLI 里 4.1 那条的完整字段 ============
	if len(cli) > 0 {
		var env map[string]json.RawMessage
		if json.Unmarshal(cli, &env) == nil {
			if d, ok := env["data"]; ok {
				var obj map[string]json.RawMessage
				if json.Unmarshal(d, &obj) == nil {
					if ms, ok := obj["models"]; ok {
						var arr []map[string]json.RawMessage
						if json.Unmarshal(ms, &arr) == nil {
							for _, m := range arr {
								var id string
								_ = json.Unmarshal(m["id"], &id)
								if strings.Contains(id, "4.1") {
									keys := make([]string, 0, len(m))
									for k := range m {
										keys = append(keys, k)
									}
									sort.Strings(keys)
									t.Logf("  === v3-CLI 模型 %s 的全部字段 ===", id)
									for _, k := range keys {
										t.Logf("      %-28s %s", k, truncate(string(m[k]), 400))
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// ============ 5) 走我们的目录，看最终 4.1 的字段 ============
	infos := c.FetchGlobalModelInfos(ga)
	for _, mi := range infos {
		if strings.Contains(mi.ID, "4.1") {
			t.Logf("  [我们的目录] %s credits=%q promoFactor=%v promoCredits=%q promoLabel=%q promoNote=%q",
				mi.ID, mi.Credits, mi.PromoFactor, mi.PromoCredits, mi.PromoLabel, mi.PromoNote)
		}
	}
	_ = fmt.Sprintf
}

func n2(s, sub string) int { return strings.Count(s, sub) }
