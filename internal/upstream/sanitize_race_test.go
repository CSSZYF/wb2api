package upstream

// sanitize_race_test.go 面板热改脱敏开关 vs 请求路径读的数据竞争回归（199g）。
//
// 现场：cmd/server/main.go 的 saveConfig（面板保存配置的热改段）原先裸赋值
// `up.SanitizeFingerprints = ...` / `up.ZeroWidthSanitize = ...`，而请求路径
// ChatStream → prepareBody 直接读这两个字段——面板保存与在途请求是两条独立
// goroutine，Go 内存模型下是数据竞争（-race 报 DATA RACE，client.go:839）。
//
// 修复：两个开关改私有 atomic.Bool + Set*/On* 读写方法。本文件锁死该契约：
// 写侧走 setter、读侧走 prepareBody，-race 下不得再有报告。

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSanitizeHotSwapRace 面板热改开关与出站请求体组装并发（-race 回归）。
// 采用时间窗写法（同 outbound_race_test.go）：写侧持续跑到读侧结束，确保两侧
// 真并发——定长次数的写循环可能先于读者跑完，漏报竞争。
func TestSanitizeHotSwapRace(t *testing.T) {
	c := New()
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + `"},` +
		`{"role":"user","content":"hi"}]}`)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写者：模拟面板保存配置，持续热改两个开关（True/False 往返）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.SetSanitizeFingerprints(i%2 == 0)
			c.SetZeroWidthSanitize(i%3 == 0)
			i++
		}
	}()
	// 读者：并发请求路径反复组装出站 body（真实读点）。
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if out := c.prepareBody(body, "cn", "u1", "c1"); len(out) == 0 {
					t.Error("prepareBody 返回空 body")
					return
				}
			}
		}()
	}
	time.Sleep(320 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSanitizeHotSwapSemantics 热改开关的取值语义（非竞争）：setter 写读成对、
// 默认值不变（指纹默认开/零宽默认关），关闭后出站保留指纹、开启后净化。
func TestSanitizeHotSwapSemantics(t *testing.T) {
	c := New()
	if !c.SanitizeFingerprintsOn() {
		t.Error("New() 默认应开启指纹脱敏")
	}
	if c.ZeroWidthSanitizeOn() {
		t.Error("New() 默认应关闭零宽脱敏")
	}
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)

	c.SetSanitizeFingerprints(false)
	if c.SanitizeFingerprintsOn() {
		t.Error("SetSanitizeFingerprints(false) 未生效")
	}
	if out := string(c.prepareBody(body, "cn", "u1", "c1")); !strings.Contains(out, ccIdentity) {
		t.Errorf("脱敏关闭后应保留指纹: %s", out)
	}

	c.SetSanitizeFingerprints(true)
	if !c.SanitizeFingerprintsOn() {
		t.Error("SetSanitizeFingerprints(true) 未生效")
	}
	if out := string(c.prepareBody(body, "cn", "u1", "c1")); strings.Contains(out, ccIdentity) {
		t.Errorf("脱敏开启后不应残留指纹: %s", out)
	}

	// 零宽脱敏独立于指纹脱敏：开启后 system 内出现 U+200B。
	c.SetZeroWidthSanitize(true)
	if !c.ZeroWidthSanitizeOn() {
		t.Error("SetZeroWidthSanitize(true) 未生效")
	}
	if out := string(c.prepareBody(body, "cn", "u1", "c1")); !strings.Contains(out, string(zeroWidthSpace)) {
		t.Errorf("零宽脱敏开启后应插入 U+200B: %s", out)
	}
	c.SetZeroWidthSanitize(false)
	if out := string(c.prepareBody(body, "cn", "u1", "c1")); strings.Contains(out, string(zeroWidthSpace)) {
		t.Errorf("零宽脱敏关闭后不应插入 U+200B: %s", out)
	}
}
