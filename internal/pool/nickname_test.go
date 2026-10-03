// nickname_test.go 钉住昵称回写（上游 f1496d0 / issue #94）：内存更新 + SaveAtomic
// 回写 auths 文件；未变化不写盘；空昵称/未知 uid 拒绝。
package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSetNicknamePersists 昵称更新写入 auths 文件并往返无损；未变化不写盘；
// 空昵称/未知 uid 拒绝。
func TestSetNicknamePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", Nickname: "旧名字", AccessToken: "at", FilePath: fp}
	p := New("")
	p.Add(a)
	if !p.SetNickname("u1", "新名字") {
		t.Fatal("昵称变化应返回 true")
	}
	reloaded, err := auth.Parse(mustReadFile(t, fp))
	if err != nil || reloaded.Nickname != "新名字" {
		t.Fatalf("回写后昵称=%q err=%v, want 新名字", reloaded.Nickname, err)
	}
	if p.SetNickname("u1", "新名字") {
		t.Fatal("未变化不应再写盘")
	}
	if p.SetNickname("u1", "") || p.SetNickname("no-such", "x") {
		t.Fatal("空昵称/未知 uid 应拒绝")
	}
	st, _ := p.Status("u1")
	if st.Nickname != "新名字" {
		t.Fatalf("Status 昵称=%q, want 新名字", st.Nickname)
	}
}

// TestSetNicknameSaveFailureKeepsMemoryAndReturnsFalse FilePath 缺失（无法写盘）时
// 返回 false，但内存已更新——面板下次刷新仍能显示新昵称（不因落盘失败而回滚）。
func TestSetNicknameSaveFailureKeepsMemoryAndReturnsFalse(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "旧", AccessToken: "at"}) // 无 FilePath
	if p.SetNickname("u1", "新") {
		t.Fatal("无 FilePath 落盘失败应返回 false")
	}
	if st, _ := p.Status("u1"); st.Nickname != "新" {
		t.Fatalf("内存昵称=%q want 新（落盘失败不回滚内存）", st.Nickname)
	}
}

// TestSetNicknameConcurrentReadersValueStable 并发正确性（非竞争检测）：写侧持续
// 改名时，读侧（Pool.Status/List）取到的昵称必须是「某一个完整值」，不得撕裂/为空。
//
// 注：本测试**不能**当作 -race 护栏——写侧经 p.mu.RLock、读侧经 p.mu.RLock，
// 同锁序已把两侧排好，删掉 a.mu 也不会在这里报竞争。真正的竞争面在**不持 p.mu**
// 的读点（出站请求体/日志行），护栏见 upstream.TestNicknamePathsRaceNicknameSync。
func TestSetNicknameConcurrentReadersValueStable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "n0", AccessToken: "at", ExpiresAt: 9999999999})

	var wg sync.WaitGroup
	stop := make(chan struct{})
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
			// 不写盘（无 FilePath）→ 只覆盖内存改写路径。
			p.SetNickname("u1", fmt.Sprintf("n%d", i))
			i++
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 2000; i++ {
			_ = p.List()
			st, _ := p.Status("u1")
			if st.Nickname == "" {
				t.Errorf("昵称读空（撕裂）")
				return
			}
		}
	}()
	wg.Wait()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
