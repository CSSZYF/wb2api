// nickname_sync_test.go 钉住面板昵称同步（上游 f1496d0 / issue #94）：
// 仅手动刷新（balance_all）触发；并发 3；单号失败（含 global 路径不存在）
// 静默跳过，不打断余额刷新；手机号等敏感字段不进入内存。
package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// profileFake 假上游：/console/account 返回指定昵称（可带手机号）；/billing/meter
// 族返回余额（balance_all 的主路径）；其余 404。
type profileFake struct {
	profileHits atomic.Int32
	maxInFlight atomic.Int32
	curInFlight atomic.Int32
	nickByUID   map[string]string
	// profileStatus 覆盖 /console/account 的状态码（0 = 200）。
	profileStatus int
	// phone 若非空则塞进响应体（隐私边界测试用）。
	phone string
}

func (f *profileFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/account"):
			n := f.curInFlight.Add(1)
			for {
				old := f.maxInFlight.Load()
				if n <= old || f.maxInFlight.CompareAndSwap(old, n) {
					break
				}
			}
			defer f.curInFlight.Add(-1)
			f.profileHits.Add(1)
			if f.profileStatus != 0 {
				http.Error(w, "not found", f.profileStatus)
				return
			}
			uid := r.Header.Get("X-User-Id")
			nick := f.nickByUID[uid]
			if nick == "" {
				nick = "nick-" + uid
			}
			body := `{"code":0,"msg":"OK","data":{"uid":"` + uid + `","nickname":"` + nick + `"`
			if f.phone != "" {
				body += `,"phoneNumber":"` + f.phone + `"`
			}
			body += `}}`
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
				`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// newProfilePanel 造一个带 scheduler 的面板 + 假上游。
func newProfilePanel(t *testing.T, p *pool.Pool, up *upstream.Client) *Panel {
	t.Helper()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})
}

// TestSyncNicknamesOnManualRefresh 手动全量刷新（balance_all）后池内昵称更新，
// 且 auths 文件同步落盘；未变化不写盘。
func TestSyncNicknamesOnManualRefresh(t *testing.T) {
	dir := t.TempDir()
	fake := &profileFake{nickByUID: map[string]string{"u1": "改后的名字"}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", Nickname: "旧名字", AccessToken: "at", ExpiresAt: 9999999999,
		FilePath: dir + "/workbuddy-u1.json"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pn := newProfilePanel(t, p, up)

	req := httptest.NewRequest("POST", "/panel/api/balance_all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := p.Status("u1"); st.Nickname != "改后的名字" {
		t.Fatalf("刷新后池内昵称=%q want 改后的名字", st.Nickname)
	}
	// 回写 auths 文件（重启不丢）。
	raw, err := os.ReadFile(dir + "/workbuddy-u1.json")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := auth.Parse(raw)
	if err != nil || reloaded.Nickname != "改后的名字" {
		t.Fatalf("落盘昵称=%q err=%v want 改后的名字", reloaded.Nickname, err)
	}
}

// TestSyncNicknamesConcurrency3 并发上限 3：8 个账号时 profile 接口的在途峰值 ≤3
// （与上游「逐账号并发 3」一致，避免打爆上游）。
func TestSyncNicknamesConcurrency3(t *testing.T) {
	fake := &profileFake{nickByUID: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8"} {
		p.Add(&auth.Auth{UID: uid, Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pn := newProfilePanel(t, p, up)

	pn.syncNicknames()

	if n := fake.profileHits.Load(); n != 8 {
		t.Fatalf("profile hits=%d want 8（每号一次）", n)
	}
	if n := fake.maxInFlight.Load(); n > 3 {
		t.Fatalf("profile 在途峰值=%d want ≤3（并发上限 3）", n)
	}
	for _, uid := range []string{"a1", "a8"} {
		if st, _ := p.Status(uid); st.Nickname == "旧" || st.Nickname == "" {
			t.Errorf("%s 昵称未同步: %q", uid, st.Nickname)
		}
	}
}

// TestSyncNicknamesGlobalFailureSilentSkip global 号 /console/account 404（该端点
// 在国际域是否同形未验证）→ 静默跳过，不报错、不影响 CN 号同步、不中断余额刷新。
func TestSyncNicknamesGlobalFailureSilentSkip(t *testing.T) {
	fake := &profileFake{nickByUID: map[string]string{"cn1": "CN新名"}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	old := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(old) })

	p := pool.New("")
	cn := &auth.Auth{UID: "cn1", Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999}
	g := &auth.Auth{UID: "g1", Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(g, "global"); err != nil {
		t.Fatal(err)
	}
	p.Add(cn)
	p.Add(g)
	// global 侧 profile 路径 404：global base 指向一个只回 404 的 server。
	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer globalSrv.Close()
	up := &upstream.Client{
		HTTP:              srv.Client(),
		GlobalEnabled:     true,
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		WebBaseCN:         srv.URL,
		ChatBaseGlobal:    globalSrv.URL,
		BillingBaseGlobal: globalSrv.URL,
	}
	pn := newProfilePanel(t, p, up)

	pn.syncNicknames() // 不应 panic、不应报错

	if st, _ := p.Status("cn1"); st.Nickname != "CN新名" {
		t.Errorf("CN 号昵称=%q want CN新名（global 失败不得影响其他号）", st.Nickname)
	}
	if st, _ := p.Status("g1"); st.Nickname != "旧" {
		t.Errorf("global 号昵称=%q want 旧（失败静默跳过，不写脏）", st.Nickname)
	}
}

// TestSyncNicknamesSkipsDisabledAndNoToken 禁用账号与无 token 账号不发资料请求。
func TestSyncNicknamesSkipsDisabledAndNoToken(t *testing.T) {
	fake := &profileFake{nickByUID: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ok", Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "dis", Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "notoken", Nickname: "旧", AccessToken: "", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pn := newProfilePanel(t, p, up)

	pn.syncNicknames()

	if n := fake.profileHits.Load(); n != 1 {
		t.Fatalf("profile hits=%d want 1（仅 ok 号）", n)
	}
}

// TestSyncNicknamesDropsPhoneFromMemory 隐私边界：响应带手机号时，同步后池内
// 除昵称外**不得**留存任何手机号痕迹（内存里只有 nickname/uid）。
func TestSyncNicknamesDropsPhoneFromMemory(t *testing.T) {
	const phone = "13800000000"
	fake := &profileFake{nickByUID: map[string]string{"u1": "新名"}, phone: phone}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pn := newProfilePanel(t, p, up)

	pn.syncNicknames()

	st, _ := p.Status("u1")
	if st.Nickname != "新名" {
		t.Fatalf("昵称=%q want 新名", st.Nickname)
	}
	// 昵称字段本身不得被污染成含手机号的串。
	if strings.Contains(st.Nickname, phone) {
		t.Fatalf("昵称被手机号污染: %q", st.Nickname)
	}
	// 池内凭证对象上不得出现手机号（auth.Auth 没有该字段，这里防未来误加）。
	a := p.AuthByUID("u1")
	if a == nil {
		t.Fatal("账号丢失")
	}
	if strings.Contains(a.Nickname, phone) {
		t.Fatalf("auth.Nickname 含手机号: %q", a.Nickname)
	}
}

// TestBackgroundBalanceRefreshDoesNotFetchProfile 后台余额定时器（RunBalanceRefreshNow）
// **不**调资料接口：昵称同步仅手动刷新触发（用户明确要求资料接口仅手动触达）。
func TestBackgroundBalanceRefreshDoesNotFetchProfile(t *testing.T) {
	fake := &profileFake{nickByUID: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "旧", AccessToken: "at", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	sch.RunBalanceRefreshNow() // 后台定时器路径

	if n := fake.profileHits.Load(); n != 0 {
		t.Fatalf("后台余额刷新触发了资料接口 %d 次 want 0（资料接口仅手动触发）", n)
	}
	if st, _ := p.Status("u1"); st.Nickname != "旧" {
		t.Fatalf("后台刷新不应改昵称: %q", st.Nickname)
	}
}
