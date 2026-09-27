package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// global_chat_path_test.go global chat 出站路径收敛（吸收上游 03ce06d 的 #119 项）。
//
// 上游实测：/console 挂腾讯云 WAF body 内容规则（printf/whoami 等命令执行特征确定性
// 403），/v2 同 base 不挂该规则、实测等价端点。本仓 2026-09-17 的独立实测结论一致
// （同一 body 直连 /console 403 WAF Block Page、/v2 200），但当时保留了 console 作
// 404/405 兜底。
//
// 为什么现在把兜底也去掉（本仓新增理由，不只是照抄上游）：兜底会把**路径级 404**
// 升级成**账号级惩罚**——/v2 若 404，同请求改打 /console，后者对命令执行特征文案
// 确定性 403 WAF → Classify 归 ErrWafBlock → 健康账号被软冷却 + 抖动退避。也就是说
// 兜底不仅大概率救不回来（console 本身 WAF 敏感），还会为一个「上游路径变了」的
// 事实惩罚无辜账号。已知取舍：若上游未来关闭 /v2，global chat 整体不可用——届时应
// 重新启用 /console 路径，本注释与 chatPaths 注释即"坏了再说"的锚点。

// TestChatPathsGlobalSingleV2 global 与 cn 都只走 /v2 单路径（无 console 候选）。
func TestChatPathsGlobalSingleV2(t *testing.T) {
	c := &Client{GlobalEnabled: true}
	c.ChatBaseGlobal = "https://global.example"
	c.ChatBaseCN = "https://cn.example"
	g := &auth.Auth{UID: "g", AccessToken: "t", Domain: "www.workbuddy.ai"}
	n := &auth.Auth{UID: "c", AccessToken: "t"}

	for _, tc := range []struct {
		name string
		a    *auth.Auth
	}{{"global", g}, {"cn", n}} {
		paths := c.chatPaths(tc.a)
		if len(paths) != 1 || paths[0] != "/v2/chat/completions" {
			t.Errorf("%s chatPaths=%v want 仅 [/v2/chat/completions]", tc.name, paths)
		}
	}
}

// TestChatStreamGlobalDoesNotFallbackToConsole global 请求遇 404 时**不得**再打
// /console：那是路径级问题，改打 WAF 敏感的 console 只会把 404 变成账号级 WAF 惩罚。
func TestChatStreamGlobalDoesNotFallbackToConsole(t *testing.T) {
	var paths []string
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			return jsonResp(http.StatusNotFound, `{"code":11102,"msg":"not found"}`), nil
		})},
		ChatBaseCN: "https://cn.example", BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true, IdleTimeout: time.Second,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"

	a := &auth.Auth{UID: "g", AccessToken: "t", Domain: "www.workbuddy.ai"}
	_, status, _, _ := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if status != http.StatusNotFound {
		t.Errorf("status=%d want 404（如实返回，不换路径）", status)
	}
	if len(paths) != 1 || paths[0] != "/v2/chat/completions" {
		t.Errorf("探测路径=%v want 仅 /v2（不得回落 console）", paths)
	}
	for _, p := range paths {
		if strings.Contains(p, "/console/") {
			t.Errorf("不得打 console（WAF 敏感，会把 404 升级成账号级惩罚）：%v", paths)
		}
	}
}

// TestEnsureConsoleSystemStillApplied #119 后 global 出站仍固定 /v2，但
// ensureConsoleSystem 兜底 system 注入保留（上游对 /v2 是否需要 system 无实测反证，
// 删了无回滚路径）：首条非 system 时仍补 fallback system。
func TestEnsureConsoleSystemStillApplied(t *testing.T) {
	var got string
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			return jsonResp(200, "data: [DONE]\n\n"), nil
		})},
		ChatBaseCN: "https://cn.example", BillingBaseCN: "https://cn-billing.example",
		GlobalEnabled: true, IdleTimeout: time.Second,
	}
	c.ChatBaseGlobal = "https://global.example"
	c.BillingBaseGlobal = "https://global-billing.example"

	a := &auth.Auth{UID: "g", AccessToken: "t", Domain: "www.workbuddy.ai"}
	rc, _, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	_ = rc.Close()
	if !strings.Contains(got, `"role":"system"`) {
		t.Errorf("global 出站应保留 ensureConsoleSystem 兜底注入：%s", got)
	}
}
