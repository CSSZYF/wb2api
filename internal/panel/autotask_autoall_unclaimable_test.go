package panel

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestAutoAllReachedButUnclaimableNotForced 达标但本地推算不可领（Claimable==false）：
// 保持 skipped、**不强行领奖**（上游计分异步，强领必失败并污染日志），但 message 要
// 与「已完成」区分开，便于排查。
//
// 为什么直调 autoAllSkipOrSettle：上游列表解析里 Claimable 由 current/target 重算
// （upstream/tasks.go 的 Claimable: !claimed && tgt>0 && cur>=tgt），HTTP 假上游造不出
// 「current>=target 且 claimable=false」的形态；本分支是「不强行领」边界要求的防御分支，
// 用同一入口直调覆盖，且面板仍接真假上游——若实现无视 Claimable，claim 命中数会变成 1。
func TestAutoAllReachedButUnclaimableNotForced(t *testing.T) {
	f := &autoAllFake{}
	f.set("chat_5", reachedButUnclaimed())
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pn := newAutoAllPanel(t, srv)

	item := map[string]any{"task_code": "chat_5", "desc": "用例任务"}
	before := &upstream.Task{TaskCode: "chat_5", Current: 5, Target: 5, Claimable: false}
	if !pn.autoAllSkipOrSettle(pn.cfg.Pool.AuthByUID("u1"), item, before) {
		t.Fatal("达标但不可领的项应就地跳过（返回 true），由调用方 append 后 continue")
	}
	if n := f.claimCount("chat_5"); n != 0 {
		t.Errorf("Claimable==false 时不得调 claimRewardFor（命中 %d 次）", n)
	}
	if st, _ := item["status"].(string); st != "skipped" {
		t.Errorf("status=%v want skipped", item["status"])
	}
	msg, _ := item["message"].(string)
	if !strings.Contains(msg, "暂不可领") {
		t.Errorf("message=%q 要区分出「达标但暂不可领」（与「已完成」可区分，便于排查）", msg)
	}
	if _, ok := item["claimed"]; ok {
		t.Errorf("未领奖不得标 claimed: %v", item["claimed"])
	}
}
