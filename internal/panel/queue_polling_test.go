package panel

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（吸收上游 410309c）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ /
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
//
// 为什么必须逐视图跑：go() 在 app.js 顶层被调用一次（按 location.hash 落视图），
// 而各视图的加载函数引用的 let/const 声明在文件下方——顶层同步读它们即 TDZ
// ReferenceError，整个脚本中断（面板白屏，而 go test 全绿）。#taskscenter 正是
// 上游 v1.11.3/1.11.4 两连炸的入口。
//
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// TestAppJSQueuePollingNoResidualOverwrite 吸收上游 1d7c97b：显式扫描 = 切到待办
// 视图，必须停掉在途队列轮询——否则队列的下一 tick 会把刚拿到的扫描结果冲掉
// 重渲染回队列视图（服务端执行不受影响，只是本视图不再实时回写）。
func TestAppJSQueuePollingNoResidualOverwrite(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	// 扫描按钮处理器必须以「清掉 queueTimer」开头（在 await 之前——await 之后清
	// 就已经晚了一个 tick）。
	i := strings.Index(s, "$('btnScanAll').onclick")
	if i < 0 {
		t.Fatal("app.js 缺 btnScanAll 处理器")
	}
	head := s[i : i+400]
	if !strings.Contains(head, "clearInterval(queueTimer)") {
		t.Errorf("扫描必须停掉队列轮询（否则扫描结果被残留 tick 冲掉）：\n%s", head)
	}
	// 终态只渲染一次即停表：残留 items（running=false）不得继续回写视图。
	j := strings.Index(s, "function startQueuePolling")
	if j < 0 {
		t.Fatal("app.js 缺 startQueuePolling")
	}
	body := s[j : j+1400]
	if !strings.Contains(body, "clearInterval(queueTimer)") {
		t.Error("队列结束时必须停表（否则残留 items 反复冲掉视图）")
	}
	// 轮询 tick 内只取一次队列状态（旧实现 tick 内取两次：一次渲染、一次判结束）。
	if n := strings.Count(body, "api('tasks/queue')"); n != 1 {
		t.Errorf("startQueuePolling 内 api('tasks/queue') 出现 %d 次，want 1（tick 内单次取状态）", n)
	}
}

// TestAppJSReattachQueueViewTDZSafe 吸收上游 c564e90 + 410309c：
//   - 视图切换/定时刷新必须走 reattachQueueView（只接管本页在跑的队列，不渲染残留）；
//   - reattachQueueView **全程异步**：go() 在顶层被调用时，本文件下方声明的
//     queueTimer/lastQueueSeq 尚未初始化，同步读取即 TDZ ReferenceError 使整个
//     脚本中断（面板白屏）。故 queueTimer 的判定必须在 await 之后。
func TestAppJSReattachQueueViewTDZSafe(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	// 旧的一次性渲染函数必须退场（它的调用点正是崩溃源）。
	if strings.Contains(s, "pollQueueOnce") {
		t.Error("app.js 仍引用 pollQueueOnce（应已由 reattachQueueView 取代）")
	}
	for _, must := range []string{"function reattachQueueView", "reattachQueueView()"} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺 %s", must)
		}
	}
	// go() 与 refreshVisible() 两个调用点都要走新函数。
	if n := strings.Count(s, "reattachQueueView()"); n < 3 {
		t.Errorf("reattachQueueView() 出现 %d 次，want ≥3（定义 + go() + refreshVisible()）", n)
	}
	// TDZ 安全：函数体内 await 必须早于任何 queueTimer 读取（注释先剥掉——
	// 说明文字里必然提到 queueTimer，那不是代码）。
	i := strings.Index(s, "function reattachQueueView")
	if i < 0 {
		t.Fatal("app.js 缺 reattachQueueView 定义")
	}
	body := s[i:]
	if j := strings.Index(body, "}"); j > 0 {
		body = body[:j+2]
	}
	code := stripJSComments(body)
	aw := strings.Index(code, "await ")
	qt := strings.Index(code, "queueTimer")
	if aw < 0 {
		t.Fatal("reattachQueueView 必须异步（await 后碰顶层 let/const，否则 TDZ 崩）")
	}
	if qt >= 0 && qt < aw {
		t.Errorf("reattachQueueView 在 await 之前读 queueTimer（TDZ ReferenceError）：\n%s", code)
	}
}

// stripJSComments 去掉 // 行注释与 /* */ 块注释（逐行扫描，不处理字符串字面量里的
// "//"——本用例的函数体里没有那种形态，够用即可）。
func stripJSComments(s string) string {
	var out strings.Builder
	inBlock := false
	for _, line := range strings.Split(s, "\n") {
		t := line
		if inBlock {
			if j := strings.Index(t, "*/"); j >= 0 {
				t = t[j+2:]
				inBlock = false
			} else {
				continue
			}
		}
		if j := strings.Index(t, "/*"); j >= 0 {
			if k := strings.Index(t[j:], "*/"); k >= 0 {
				t = t[:j] + t[j+k+2:]
			} else {
				t = t[:j]
				inBlock = true
			}
		}
		if j := strings.Index(t, "//"); j >= 0 {
			t = t[:j]
		}
		out.WriteString(t)
		out.WriteString("\n")
	}
	return out.String()
}
