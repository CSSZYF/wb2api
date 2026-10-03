// global_models_desktop_ua_test.go global 目录探测的 **UA 家族三路覆盖**回归
// （吸收上游 PR #103）。
//
// 关键数据（PR #103 实测）：/v3/config 按 **UA 家族**分档下发不同目录：
//
//	CN：IDE 19 / CLI 32 / 桌面端 54
//	global：IDE 13 / CLI 22 / 桌面端 29
//
// 版本号升降**不改**结果，只有家族变。`gpt-6-luna` 只在桌面端那档出现，实测可调用
// 200——只探 IDE + CLI 两路时它永远进不了 /v1/models。
//
// 我们的缺口证据：global_models.go 的 probeGlobalV3Capabilities 只有
// probe(codeBuddyIDEUA) / probe(codeBuddyCLIUA) 两路，grep desktopProbeUA 零命中。
package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDesktopProbeUAIsDesktopFamily desktopProbeUA 必须返回桌面端家族的 UA
// （WorkBuddy 前缀三段式），而不是 IDE / CLI 家族——第三路的意义全在家族不同。
func TestDesktopProbeUAIsDesktopFamily(t *testing.T) {
	c := &Client{}
	ua := c.desktopProbeUA()
	if !strings.HasPrefix(ua, "WorkBuddy/") {
		t.Fatalf("desktopProbeUA() = %q want WorkBuddy/ 前缀（桌面端家族）", ua)
	}
	if ua == codeBuddyIDEUA || ua == codeBuddyCLIUA {
		t.Fatalf("desktopProbeUA() 与 IDE/CLI 家族重合（%q）——三路探测失去意义", ua)
	}
	// 平台段固定 CN 形态（`WorkBuddy`）：PR #103 本次实测即该形态、两个 base 都返回
	// 最大目录；defaultWorkBuddyUAFor 的 global 形态（`WorkBuddy AI`）未实测，不臆造。
	if !strings.Contains(ua, "WorkBuddy/") || strings.Contains(ua, "WorkBuddy AI") {
		t.Errorf("desktopProbeUA() = %q want CN 形态平台段（WorkBuddy，非 WorkBuddy AI）", ua)
	}
}

// TestGlobalCatalogProbesDesktopUA 锁定「/v3/config 目录按 UA 家族分档」这一上游事实：
// 仅桌面端 UA 下发的模型必须出现在并集里。
//
// 回归目标：gpt-6-luna 只在桌面端目录下发、实际可调用，只探 IDE + CLI 两路时它
// 永远进不了 /v1/models。
func TestGlobalCatalogProbesDesktopUA(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	const desktopOnly = "gpt-6-luna"
	uas := new(pathRecorder)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/v3/config":
				ua := r.Header.Get("User-Agent")
				uas.add(ua)
				ids := []string{"shared-model"}
				switch {
				case strings.HasPrefix(ua, "CodeBuddyIDE/"):
					ids = append(ids, "ide-only")
				case strings.HasPrefix(ua, "CLI/"):
					ids = append(ids, "cli-only")
				case strings.HasPrefix(ua, "WorkBuddy/"):
					ids = append(ids, desktopOnly)
				default:
					t.Errorf("v3/config 探测带了非预期 UA: %q", ua)
				}
				return jsonResp(200, catalogModelsJSON(ids)), nil
			default:
				// 企业端点家族一律 500：本用例只验证 v3 多路并集，同时覆盖
				// 「家族路失败不拖累 v3 结果」的降级语义。
				return jsonResp(500, "<html>500</html>"), nil
			}
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"
	a := globalAuth()

	// 企业端点全失败 → v3 兜底路径：目录 = v3 并集（三路独有 id 都要在）。
	names := c.FetchGlobalModels(a)
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"shared-model", "ide-only", "cli-only", desktopOnly} {
		if !got[want] {
			t.Errorf("FetchGlobalModels 缺少 %q（并集=%v）", want, names)
		}
	}
	if n := len(uas.all()); n != 3 {
		t.Errorf("v3/config 应探 3 种 UA 家族，实际 %d 种: %v", n, uas.all())
	}
}

// TestGlobalCatalogDesktopUAFailureIsSoft 第三路失败不得拖累前两路（fail-soft）：
// 桌面端 UA 400（如上游不认该家族）时目录仍含 IDE + CLI 的独有 id。
func TestGlobalCatalogDesktopUAFailureIsSoft(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/v3/config":
				ua := r.Header.Get("User-Agent")
				switch {
				case strings.HasPrefix(ua, "CodeBuddyIDE/"):
					return jsonResp(200, catalogModelsJSON([]string{"ide-only"})), nil
				case strings.HasPrefix(ua, "CLI/"):
					return jsonResp(200, catalogModelsJSON([]string{"cli-only"})), nil
				default: // 桌面端家族：模拟上游拒绝
					return jsonResp(400, `{"code":12403,"msg":"bad ua"}`), nil
				}
			default:
				return jsonResp(500, "<html>500</html>"), nil
			}
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"

	names := c.FetchGlobalModels(globalAuth())
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"ide-only", "cli-only"} {
		if !got[want] {
			t.Errorf("第三路失败不得拖累前两路：缺 %q（并集=%v）", want, names)
		}
	}
}

// TestGlobalCatalogThreeRoutesAllFailNegativeCaches 三路全失败仍走既有降级链：
// 静态名单 + 负缓存（不因新增第三路而改变「全失败」的判定）。
func TestGlobalCatalogThreeRoutesAllFailNegativeCaches(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	c, calls := globalTestClient(t, func(string) (int, string) { return 500, "<html>500</html>" })
	infos := c.FetchGlobalModelInfos(globalAuth())
	if len(infos) != len(GlobalModelNames) {
		t.Fatalf("三路全失败应回落静态名单：%d want %d", len(infos), len(GlobalModelNames))
	}
	first := calls.Load()
	_ = c.FetchGlobalModelInfos(globalAuth())
	if calls.Load() != first {
		t.Errorf("负缓存期内不应再打上游：calls %d → %d", first, calls.Load())
	}
}

// catalogModelsJSON 构造 v3/config 信封（只给 id + 窗口，够解析即可）。
func catalogModelsJSON(ids []string) string {
	var b strings.Builder
	b.WriteString(`{"code":0,"data":{"models":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"` + id + `","maxInputTokens":100000,"maxOutputTokens":32000}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}
