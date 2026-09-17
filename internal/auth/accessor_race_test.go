package auth

// accessor_race_test.go 加锁访问器（AccessTokenValue/DomainValue/RefreshTokenValue）
// 与 Realm 系列加锁读的并发回归：出站读取侧与 RefreshToken 写回侧的竞争。
//
// 背景：auth.Auth.mu 的既有契约只覆盖「RefreshToken 写 ↔ SaveAtomic 读」（见 auth.go
// 注释）。出站请求头构造（ChatHeaders/BillingHeaders/各活动接口）与调度器守卫都是
// 锁外直读 a.AccessToken / a.Domain / a.RefreshToken，而 upstream.RefreshToken 在
// a.mu 内改写这四个字段——生产上 Scheduler.RunKeepaliveNow 定时对每个非禁用账号
// 刷新（与是否有在途请求无关），而 handler 正基于**同一个** *auth.Auth 指针构造请求头
// （Pool.AuthByUID/List 返回池内同一个对象）。无同步即数据竞争，本测试用 -race 锁定。
//
// 本测试直接驱动字段写（模拟 upstream.RefreshToken 的「第 2 段（锁内）」写回），
// 读侧全部走访问器——修复前（访问器不存在/内部不加锁）由 -race 报竞争。

import (
	"sync"
	"testing"
	"time"
)

// TestAccessorsNilSafe 访问器 nil 安全：nil 接收者不得 panic（出站路径存在
// a == nil 的降级分支，如 CommonHeaders 的 originRefererFor）。
func TestAccessorsNilSafe(t *testing.T) {
	var a *Auth
	if got := a.AccessTokenValue(); got != "" {
		t.Errorf("nil AccessTokenValue()=%q want empty", got)
	}
	if got := a.DomainValue(); got != "" {
		t.Errorf("nil DomainValue()=%q want empty", got)
	}
	if got := a.RefreshTokenValue(); got != "" {
		t.Errorf("nil RefreshTokenValue()=%q want empty", got)
	}
}

// TestAccessorsReadLatestValue 访问器读到最新写入值（加锁不改变可见性语义）。
func TestAccessorsReadLatestValue(t *testing.T) {
	a := &Auth{AccessToken: "at1", RefreshToken: "rt1", Domain: "d1.example.com"}
	if got := a.AccessTokenValue(); got != "at1" {
		t.Errorf("AccessTokenValue()=%q want at1", got)
	}
	if got := a.DomainValue(); got != "d1.example.com" {
		t.Errorf("DomainValue()=%q want d1.example.com", got)
	}
	if got := a.RefreshTokenValue(); got != "rt1" {
		t.Errorf("RefreshTokenValue()=%q want rt1", got)
	}
	a.Lock()
	a.AccessToken = "at2"
	a.RefreshToken = "rt2"
	a.Domain = "d2.example.com"
	a.ExpiresAt = time.Now().Add(time.Hour).Unix()
	a.Unlock()
	if got := a.AccessTokenValue(); got != "at2" {
		t.Errorf("AccessTokenValue()=%q want at2", got)
	}
	if got := a.DomainValue(); got != "d2.example.com" {
		t.Errorf("DomainValue()=%q want d2.example.com", got)
	}
	if got := a.RefreshTokenValue(); got != "rt2" {
		t.Errorf("RefreshTokenValue()=%q want rt2", got)
	}
	if a.NeedsRefresh(30 * time.Minute) {
		t.Error("NeedsRefresh(30m) 在 1h 后过期时应为 false")
	}
	if !a.NeedsRefresh(2 * time.Hour) {
		t.Error("NeedsRefresh(2h) 在 1h 后过期时应为 true（窗口覆盖过期点）")
	}
}

// TestAccessorsRaceRefreshWrite 读侧访问器与写侧锁内写回并发（-race 回归）：
// 覆盖 AccessToken/Domain/RefreshToken 三字段与 Realm/NeedsRefresh 两个加锁派生读。
func TestAccessorsRaceRefreshWrite(t *testing.T) {
	withGlobalEnabled(t)
	a := &Auth{
		AccessToken:  "at",
		RefreshToken: "rt",
		Domain:       "chat.example.com",
		ExpiresAt:    1,
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写侧：模拟 upstream.RefreshToken 的「第 2 段（锁内）：校验快照一致后写回」。
	// domain 每次换值，使 Realm() 的 domain 回落分支与 X-Domain 读取都进入竞争面。
	wg.Add(1)
	go func() {
		defer wg.Done()
		domains := []string{"www.workbuddy.ai", "chat.example.com"}
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			a.Lock()
			a.AccessToken = "at-new"
			a.RefreshToken = "rt-new"
			a.Domain = domains[i%len(domains)]
			a.ExpiresAt = time.Now().Add(time.Hour).Unix()
			a.Unlock()
			i++
		}
	}()

	// 读侧：出站头构造 + 调度器守卫的全部取值口径。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = a.AccessTokenValue()
			_ = a.DomainValue()
			_ = a.RefreshTokenValue()
			_ = a.Realm()
			_ = a.RealmStored()
			_ = a.IsGlobal()
			_ = a.NeedsRefresh(time.Minute)
		}
		close(stop)
	}()

	wg.Wait()
}

// TestRealmAndBackfillRace 写侧 Realm 系列（BackfillRealm/BackfillRealmFor）与
// 读侧 Realm()/RealmStored() 并发：a.realm 字段的读写必须同锁。
func TestRealmAndBackfillRace(t *testing.T) {
	withGlobalEnabled(t)
	a := &Auth{AccessToken: "at", Domain: "www.workbuddy.ai", ExpiresAt: time.Now().Add(time.Hour).Unix()}

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
			// 交替写空/写值，覆盖 backfill 的「有变更」与「幂等」两分支。
			a.Lock()
			a.realm = ""
			a.Unlock()
			_, _ = a.BackfillRealm()
			_, _ = BackfillRealmFor(a, "global")
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = a.Realm()
			_ = a.RealmStored()
			_ = a.IsGlobal()
		}
		close(stop)
	}()

	wg.Wait()
}
