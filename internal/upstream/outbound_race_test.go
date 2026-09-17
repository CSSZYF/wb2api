package upstream

// outbound_race_test.go 出站读取侧与 RefreshToken 写回侧的数据竞争回归（#125 全量版）。
//
// auth.Auth.mu 的既有契约只覆盖「RefreshToken 写 ↔ SaveAtomic 读」（见 auth.go 注释）。
// 写侧全程持锁（RefreshToken「第 2 段（锁内）」写 AccessToken/RefreshToken/Domain/
// ExpiresAt），而**所有出站请求头构造与账号级守卫都是锁外直读字段**：
//   - Client.ChatHeaders / BillingHeaders / RefreshHeaders（headers.go）
//   - Client.fetchModelsOnce / fetchV3ConfigModelMap / v3ConfigDomain（client.go）
//   - globalModelsOnce（global_models.go）
//   - ReportDesktopEvent / SetAppearanceTheme / ReportWebEvent / MarketExpertList /
//     DesktopChatWithExpert（desktop.go）
//   - schoolJSON / ReportMPEvent（school.go）、ClaimReward（tasks.go）
//   - globalRegisterReq 的三个调用点（global_register.go）
//   - Scheduler 的 checkin/keepalive/travel/activity/streak/blackcat/school 守卫
//
// 生产上两侧真会并发：Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号调
// RefreshToken（与该账号是否有在途请求无关），而 handler 正基于同一个 *auth.Auth
// 指针构造出站请求头——Pool.AuthByUID/List 返回的就是池内同一个对象。
//
// 本文件覆盖全部出站入口（不止 ChatHeaders）：每个入口一个并发对，-race 下若有
// 任一入口残留锁外直读即报竞争。

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// raceTestAuth 构造参与并发测试的账号（ExpiresAt=1 使 NeedsRefresh 恒真，覆盖刷新路径）。
func raceTestAuth() *auth.Auth {
	return &auth.Auth{
		AccessToken:  "at",
		RefreshToken: "rt",
		Domain:       "chat.example.com",
		UID:          "u-race",
		EnterpriseID: "e-race",
		ExpiresAt:    1,
	}
}

// runWithRefreshRace 启动一个持续刷新的写侧 goroutine，执行 body（读侧），
// 结束后停写侧并等待收尾。body 内部只做纯读/构造，不依赖上游响应内容。
func runWithRefreshRace(t *testing.T, c *Client, a *auth.Auth, body func()) {
	t.Helper()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.RefreshToken(a) // 写侧：a.mu 内改写四个字段
		}
	}()
	body()
	close(stop)
	wg.Wait()
}

// refreshRaceClient 返回一个把 refresh 端点打到内存 round-tripper 的客户端。
// domain 带非空值让 RefreshToken 的 `a.Domain = tok.Domain` 真正执行
// （否则该写入被 `if tok.Domain != ""` 挡掉，Domain 竞争不可见）。
func refreshRaceClient() *Client {
	return testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/token/refresh") {
			return jsonResp(404, `{"code":1,"msg":"not found"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","domain":"www.workbuddy.ai","expiresIn":3600}}`), nil
	})
}

// TestChatHeadersRacesRefreshToken ChatHeaders 读 AccessToken/Domain/Realm 与刷新写回并发。
// 这是上游 #125 的原始现场（读侧 = 在途请求构造 chat 头）。
func TestChatHeadersRacesRefreshToken(t *testing.T) {
	c := refreshRaceClient()
	a := raceTestAuth()

	req, err := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	runWithRefreshRace(t, c, a, func() {
		for i := 0; i < 300; i++ {
			c.ChatHeaders(req, a, "", ChatMeta{})
			_ = req.Header.Get("Authorization")
			_ = req.Header.Get("X-Domain")
		}
	})
}

// TestBillingHeadersRacesRefreshToken billing 域（report/travel/balance/checkin）头构造
// 与刷新写回并发：BillingHeaders 走的是与 chat 不同的一条出站路径（不经 CommonHeaders）。
func TestBillingHeadersRacesRefreshToken(t *testing.T) {
	c := refreshRaceClient()
	a := raceTestAuth()

	req, err := http.NewRequest(http.MethodPost, "https://billing.example/v2/report", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	runWithRefreshRace(t, c, a, func() {
		for i := 0; i < 300; i++ {
			c.BillingHeaders(req, a)
			_ = req.Header.Get("Authorization")
			_ = req.Header.Get("X-Domain")
		}
	})
}

// TestModelFetchRacesRefreshToken 模型目录/能力探测路径（fetchModelsOnce、
// fetchV3ConfigModelMap→v3ConfigDomain、globalModelsOnce）与刷新写回并发。
func TestModelFetchRacesRefreshToken(t *testing.T) {
	c := refreshRaceClient()
	a := raceTestAuth()

	runWithRefreshRace(t, c, a, func() {
		for i := 0; i < 200; i++ {
			// FetchModelsDiag 串起 fetchModelsOnce + /v3/config 能力覆盖（含 v3ConfigDomain）。
			_, _, _ = c.FetchModelsDiag(a)
			// global 模型探测（globalModelsOnce）。
			_, _ = c.probeGlobalModels(a)
		}
	})
}

// TestDesktopPathsRaceRefreshToken 桌面端/Web 上报族（desktop.go）与刷新写回并发：
// 该文件是出站点最多的一处（5 个独立请求构造点）。
func TestDesktopPathsRaceRefreshToken(t *testing.T) {
	c := refreshRaceClient()
	a := raceTestAuth()

	runWithRefreshRace(t, c, a, func() {
		for i := 0; i < 60; i++ {
			_ = c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "chat_message_send"})
			_ = c.SetAppearanceTheme(a, "theme-tkmw7j")
			_ = c.ReportWebEvent(a, "page_view", "https://www.workbuddy.cn/", "el", "name")
			_, _ = c.MarketExpertList(a, "")
			_, _, _ = c.DesktopChatWithExpert(a, "expert-1")
		}
	})
}

// TestActivityPathsRaceRefreshToken 活动/任务族（school.go / tasks.go /
// global_register.go）与刷新写回并发。
func TestActivityPathsRaceRefreshToken(t *testing.T) {
	c := refreshRaceClient()
	a := raceTestAuth()

	runWithRefreshRace(t, c, a, func() {
		for i := 0; i < 60; i++ {
			_, _, _ = c.SchoolTasks(a)
			_ = c.SchoolShareComplete(a)
			_, _, _ = c.ClaimReward(a, "daily_sign_in")
			_ = c.ReportMPEvent(a, map[string]any{"event_code": "page_view"})
		}
	})
}

// TestGlobalRegisterPathsRaceRefreshToken global 注册激活链路（global_register.go
// 三处 a.AccessTokenValue()）与刷新写回并发。
func TestGlobalRegisterPathsRaceRefreshToken(t *testing.T) {
	c := refreshRaceClient()
	// global 注册链路只服务 global 账号（Realm() != "global" 直接报错返回）。
	a := raceTestAuth()
	a.Domain = "www.workbuddy.ai"
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	runWithRefreshRace(t, c, a, func() {
		for i := 0; i < 60; i++ {
			_, _ = c.GlobalFetchCountries(a, false)
			_, _, _, _ = c.GlobalRegisterStatus(a)
			_ = c.GlobalSubmitRegion(a, GlobalCountry{Code: "HK", EnName: "Hong Kong", IOS2: "HK"})
		}
	})
}

// TestChatHeadersConcurrentWithRefreshValueStable 并发正确性（非竞争）：修复后
// ChatHeaders 读到的 Authorization 必须是「某一个完整 token 值」，不得出现空/撕裂。
// 与 -race 回归配对：前者防竞争，本测试锁死「加锁不改变取值语义」。
func TestChatHeadersConcurrentWithRefreshValueStable(t *testing.T) {
	c := refreshRaceClient()
	a := raceTestAuth()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.RefreshToken(a)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			req, err := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
			if err != nil {
				t.Errorf("new request: %v", err)
				break
			}
			c.ChatHeaders(req, a, "", ChatMeta{})
			got := req.Header.Get("Authorization")
			if got != "Bearer at" && got != "Bearer newat" {
				t.Errorf("Authorization=%q 非完整 token 快照（撕裂/空值）", got)
				break
			}
		}
		close(stop)
	}()

	wg.Wait()
}
