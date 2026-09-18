// authwatch.go auths 目录热加载：定时扫描 auth_dir，把用户手工上传/删除的 auth 文件
// 增量对齐进池，免重启生效。
//
// 背景（云服务器场景）：用户在本地完成登录后把 auth 文件**上传**到 auths/ 目录，
// 但进程没有文件监听——旧实现只在启动时调一次 pool.SyncToDir（cmd/server/main.go），
// 手工放文件必须重启才生效。面板「添加账号」走 Pool.Add 本来就是热的，此处补的
// 就是「手工上传」这条路径。
//
// 为什么不引入 fsnotify：本仓定位是单二进制零外部依赖（前端都是 go:embed 内嵌），
// 为一个「账号数是个位数、变更以分钟计」的目录加一个第三方依赖不划算；且 inotify
// 在 Docker bind mount / 网络盘（NFS/SMB）上常丢事件，轮询反而是这类部署下更可靠的
// 形态（也顺手覆盖了「容器外写文件」这一最常见场景）。
//
// 为什么复用 pool.SyncToDir 而不是另写一套增删：对齐语义（新号加入、消失的号剔除、
// 状态保留）必须与启动路径逐字一致，两套 diff 迟早漂移出「重启后才对」的怪现象。
// 这里只做「拿到目录现状」这一件事，对账整体交给 pool。
package scheduler

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// authFileStamp 单个 auth 文件的目录元信息快照（size + mtime）。
// 与上一轮相同即认为内容未变：跳过读取与解析，每轮只付出一次 Glob + stat。
type authFileStamp struct {
	size  int64
	mtime time.Time
}

// authWatchEntry 单个 auth 文件的解析缓存：上次成功解析的账号 + 读取时的元信息。
// 元信息未变的文件直接复用 a 指针（不读盘、不重新 Parse），它同时充当「目录现状」
// 里的一个成员参与 pool 对账——漏掉它会把它误判成「文件已删除」。
type authWatchEntry struct {
	stamp authFileStamp
	a     *auth.Auth
}

// authWatcher auths 目录扫描器。prev 只被扫描 goroutine（或测试的串行调用）读写，
// 无需加锁——热加载循环是单 goroutine 串行执行的。
type authWatcher struct {
	dir string
	// prev 文件路径 → 上一次成功解析的结果。除「本轮的目录现状」外，还包含本轮读不出
	// 来但仍在目录里的坏文件旧条目（keepPrev 结转），保证 keep 保护跨轮不失效。
	prev map[string]authWatchEntry
}

func newAuthWatcher(dir string) *authWatcher {
	return &authWatcher{dir: dir, prev: map[string]authWatchEntry{}}
}

// scan 扫描一轮：Glob 目录 → 逐文件 stat → **仅对新增/变更的文件**读取解析 →
// 交给 pool 与池内账号对账（新增/更新加入，消失的剔除，状态保留）。
//
// 三条防「读到半截文件」的规则（用户通过 scp/docker cp/编辑器上传时，文件会先以
// 零长或半截内容出现在目录里）：
//  1. stat 失败（Glob 与 stat 之间被覆盖/删除）：本轮跳过该文件，并保留上一轮已解析
//     账号在池内（keep），下轮再判——不能把一次中间态当「文件被删除」。
//  2. 解析失败（半截 JSON / 写入中的空文件）：打 WARN 跳过，同样把上一轮已解析账号
//     留在池内（keep），下轮元信息变化后再读——账号不会因一次写入中间态被出池。
//  3. 只有成功解析出的账号才进入本轮「目录现状」，也才有资格参与剔除判定：账号
//     只在文件**确实从目录消失**时才出池。
//
// 规则 1/2 的「上轮结果」必须**跨轮结转**（见 keepPrev）：w.prev 每轮按目录现状整体
// 重建，若不把坏文件的旧条目带进下一轮，keep 就只生效一轮——文件持续半截（上传中断
// 后残留、写入卡住）时第二轮仍会被当成「文件已删除」剔除，与规则 3 的约定相悖。
//
// 返回 (新增 uid, 剔除 uid)，按 UID 升序；供调用方打运维日志与测试断言。
func (w *authWatcher) scan(p *pool.Pool) (added, removed []string) {
	files, err := filepath.Glob(filepath.Join(w.dir, auth.FilePattern))
	if err != nil {
		// 目录不存在时 Glob 返回空集（不报错），故这里只可能是非法模式/IO 错误。
		log.Printf("WARN: auth watch: 扫描 %s 失败: %v", w.dir, err)
		return nil, nil
	}
	sort.Strings(files)

	next := make(map[string]authWatchEntry, len(files))
	auths := make([]*auth.Auth, 0, len(files))
	// keep 保护「本轮读不出来、但上一轮确实存在」的账号：它们是写入中间态/瞬时
	// IO 错误的受害者，不该被当成「文件已删除」而从池中剔除（剔除还会连带丢掉
	// 该号的积分/冷却状态，下次入池是全新条目）。
	keep := map[string]bool{}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			log.Printf("WARN: auth watch: %s 读取元信息失败，本轮跳过（下轮重试）: %v", f, err)
			w.keepPrev(f, next, keep)
			continue
		}
		stamp := authFileStamp{size: st.Size(), mtime: st.ModTime()}
		// 已知文件的快速路径：元信息未变 → 直接复用上次解析结果（不读盘、不重新 Parse）。
		// 每轮的固定开销因此只有一次 Glob + N 次 stat（账号数是个位数，可忽略）。
		if prev, ok := w.prev[f]; ok && prev.a != nil && prev.stamp == stamp {
			next[f] = prev
			auths = append(auths, prev.a)
			continue
		}
		a, err := auth.LoadFile(f)
		if err != nil {
			// 正在写入的半截 JSON 走这里（最常见路径）：WARN 一行便于运维确认
			// 「文件已上传但还没生效」不是 bug，下一轮就会成功。
			log.Printf("WARN: auth watch: %s 解析失败，本轮跳过（文件可能正在写入，下轮重试）: %v", f, err)
			w.keepPrev(f, next, keep)
			continue
		}
		next[f] = authWatchEntry{stamp: stamp, a: a}
		auths = append(auths, a)
	}
	w.prev = next // 缓存随目录现状整体重建：被删除的文件自然从缓存消失（不泄漏、不复活）

	added, removed = p.SyncToDirExcept(auths, keep)
	byUID := make(map[string]*auth.Auth, len(auths))
	for _, a := range auths {
		byUID[a.UID] = a
	}
	for _, uid := range added {
		file := ""
		if a := byUID[uid]; a != nil {
			file = a.FilePath
		}
		log.Printf("auth watch: added uid=%s file=%s realm=%s", logfmt.UID8(uid), file, realmOf(byUID[uid]))
	}
	for _, uid := range removed {
		log.Printf("auth watch: removed uid=%s（auth 文件已删除）", logfmt.UID8(uid))
	}
	return added, removed
}

// keepPrev 把某路径「上一轮成功解析出的 uid」记入 keep 集合：本轮该文件读不出来
// （stat/解析失败），但文件仍在目录中，池内账号必须原样保留到下轮成功读取为止。
// 上一轮也没解析成功（新文件正在上传）时无事可保留，静默跳过即可。
//
// 同时把该旧条目**结转到 next**（而不是让它随本轮目录现状重建而消失）：keep 的判定
// 基准是 w.prev[path]，若坏文件本轮就被从缓存里抹掉，下一轮 keepPrev 找不到旧条目、
// 保护随即失效——文件持续半截（上传中断残留 / 写入卡住）时第二轮仍会被当成
// 「文件已删除」剔除，正是本特性最该防的「上传中间态把在用账号踢出池」。
// 结转的条目元信息仍是上次成功解析时的值，故文件一旦再次变化即走完整读取路径。
func (w *authWatcher) keepPrev(path string, next map[string]authWatchEntry, keep map[string]bool) {
	prev, ok := w.prev[path]
	if !ok || prev.a == nil {
		return
	}
	next[path] = prev
	keep[prev.a.UID] = true
}

// realmOf 日志用域标识（nil 安全：byUID 未命中时给 "-"）。
func realmOf(a *auth.Auth) string {
	if a == nil {
		return "-"
	}
	return a.Realm()
}

// StartAuthWatch 启动 auths 目录后台热加载循环（ctx 取消即停）。
//
// 形态与 StartBalanceRefresh 一致（atomic 间隔 + rearm 通知 + 独立 goroutine），
// 但有一处**刻意不同**：interval<=0（面板关了开关）时不是「不启动」，而是启动后
// 空转等重排通知。原因：本开关要支持面板热启用，若关闭时压根不建 goroutine，
// 之后把 auth_watch_enabled 打开就没有任何路径能把它拉起来（余额刷新没有这个需求，
// 所以那里是直接 return）。空转不扫描、不占 CPU。
//
// dir 为空（未配置 auth_dir）时不启动：没有目录可看，启动只会每轮打 WARN。
func (s *Scheduler) StartAuthWatch(ctx context.Context, dir string, interval time.Duration) {
	if dir == "" || s.cfg.Pool == nil {
		return
	}
	s.authWatchInterval.Store(int64(interval))
	w := newAuthWatcher(dir)
	go func() {
		// logged 初值 -1（非法负值）保证首轮必打一条启动/暂停日志，后续仅在
		// 间隔真变化时打——不与「0 = 暂停」的合法值混淆。
		logged := time.Duration(-1)
		for {
			cur := time.Duration(s.authWatchInterval.Load())
			if cur != logged {
				if cur <= 0 {
					log.Printf("auth watch: 已暂停（auth_watch_enabled=false，改回 true 后立即恢复）")
				} else {
					log.Printf("auth watch: 每 %s 扫描 %s（手工上传/删除 auth 文件免重启生效）", cur, dir)
				}
				logged = cur
			}
			if cur <= 0 {
				// 已暂停：等重排通知（重新启用时唤醒）或退出信号。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmAuthWatch:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmAuthWatch:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				w.scan(s.cfg.Pool)
			}
		}
	}()
}

// SetAuthWatchInterval 热改 auths 目录扫描间隔；<=0 表示暂停扫描（面板关闭该开关时）。
// 下一轮生效（正睡眠的 timer 被 rearm 唤醒后按新值重算）。
func (s *Scheduler) SetAuthWatchInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.authWatchInterval.Store(int64(d))
	poke(s.rearmAuthWatch)
}

// AuthWatchInterval 返回当前生效的 auths 扫描间隔（0 = 暂停/未启动）。
// 供测试与运维观测：热改后立即读到新值，不必等下一轮扫描。
func (s *Scheduler) AuthWatchInterval() time.Duration {
	return time.Duration(s.authWatchInterval.Load())
}
