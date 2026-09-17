// testchat.go 单账号对话测试（面板诊断用，最小版）：选定模型发一条短消息，
// 走与 /v1/chat/completions 完全相同的出站路径（ChatStream），把 SSE 流聚合出
// 最终 message 后回显回复摘要、耗时与失败原因。
//
// 设计边界（刻意为之，勿在后续迭代里"顺手"补齐）：
//   - 不计入账号统计与惩罚：不调 NoteError/NoteSuccess，不碰熔断与冷却。这是诊断
//     操作，一次手点不该把好号打成冷却态；ChatStreamContext 自身只做出站请求，
//     无任何池副作用（见 upstream/client.go），故本文件也不调用 applyErrorPolicy。
//   - 模型可见性不做门禁：只校验 model 非空，不拿 /v1/models 的目录做硬校验。
//     上游目录没列出的模型 ≠ 不可调用（写死条目 deepseek-v4.1-flash 就是这种
//     情况），硬门禁会把合法诊断挡在门外。前端用 /panel/api/models 的同一份目录
//     填下拉框（看得见 = 选得到），模型真不可用时上游自会报错并原样透出。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// testChatMaxMessage 消息长度上限（按字符计，中文按 1 字算；非字节）。
	testChatMaxMessage = 4000
	// testChatMaxTokens 出站 max_tokens 上限：诊断只要一句回复，防超长输出把
	// 60s 预算耗光（思考与最终回答共享该预算，见 ModelInfo.MaxTokens 注记）。
	testChatMaxTokens = 512
	// testChatReplyLimit 回显回复的截断长度（字符）。
	testChatReplyLimit = 200
	// testChatErrorLimit 上游错误原文回显的截断长度（字符）。
	testChatErrorLimit = 300
	// testChatTimeout 单次测试的整体上限：出站请求与聚合读取都挂在这个 ctx 上
	// （超时取消 → 阻塞中的读随之返回错误，不会吊住面板请求）。
	testChatTimeout = 60 * time.Second
)

// accountTestChat POST /panel/api/account/test_chat：单账号对话测试。
//
// 成功与"上游拒绝"都返回 HTTP 200（ok 字段区分）：本端点的语义是"测试已执行完毕，
// 结论在 body 里"，上游状态码放 status 字段原样透出——面板一次请求就能拿到
// 回复/耗时/错误三者，不必为读错误详情再解析 HTTP 状态。只有请求本身不合法
// （400）与账号不存在（404）才用非 200。
func (p *Panel) accountTestChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID     string `json:"uid"`
		Model   string `json:"model"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	req.UID = strings.TrimSpace(req.UID)
	req.Model = strings.TrimSpace(req.Model)
	if req.UID == "" {
		writeErr(w, http.StatusBadRequest, "uid 必填")
		return
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "model 必填")
		return
	}
	if n := utf8.RuneCountInString(req.Message); n > testChatMaxMessage {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("message 超长：%d 字符（上限 %d）", n, testChatMaxMessage))
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		// 空消息上游只会回一个语义不明的 400，这里提前给出可读结论。
		writeErr(w, http.StatusBadRequest, "message 不能为空")
		return
	}
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "pool/upstream not available")
		return
	}
	a := p.cfg.Pool.AuthByUID(req.UID)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在："+req.UID)
		return
	}

	// 出站 body 与 /v1/chat/completions 同形。stream 显式写 false 只表达意图：
	// 上游拒绝非流式，PrepareBody 会强制改回 true，ChatStream 返回的恒是 SSE 流，
	// 因此下面必须用 upstream.Aggregate 聚合。
	body, err := json.Marshal(map[string]any{
		"model":      req.Model,
		"messages":   []map[string]any{{"role": "user", "content": req.Message}},
		"stream":     false,
		"max_tokens": testChatMaxTokens,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal body: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testChatTimeout)
	defer cancel()
	start := time.Now()
	rc, status, respBody, err := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{})
	// 耗时口径：出站 + 聚合读完整条流的总墙钟——用户等多久就报多久。
	elapsed := func() int64 { return time.Since(start).Milliseconds() }
	account := uidPrefix(req.UID)

	// 所有失败都带 latency_ms 回显：卡了 60s 还是 200ms 就被拒，结论完全不同。
	fail := func(status int, msg string) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": status, "error": msg,
			"latency_ms": elapsed(), "model": req.Model, "account": account,
		})
	}
	// wrapErr 给底层错误加上"是不是超时/客户端断开"的判读。
	// 60s 上限与浏览器关闭都会以"读错误"的形式冒出来（ctx 取消 → 阻塞中的读返回
	// 错误），原文是 "context deadline exceeded" 之类，直接透出对用户没有信息量。
	wrapErr := func(prefix string, err error) string {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			return fmt.Sprintf("%s：超过 %s 上限（上游未在预算内完成）", prefix, testChatTimeout)
		case context.Canceled:
			return prefix + "：请求已取消"
		default:
			return prefix + "：" + err.Error()
		}
	}
	if err != nil {
		// 传输层失败/超时：上游没给出可判读的响应，status 记 0。
		log.Printf("panel: test_chat uid=%s model=%s 失败(transport) %dms: %v",
			req.UID, req.Model, elapsed(), err)
		fail(0, wrapErr("上游请求失败", err))
		return
	}
	if rc != nil {
		defer rc.Close()
	}
	if status >= 400 {
		// 上游明确拒绝（401/403/429/5xx…）：原文透出，便于区分"号不能用"与"模型不可用"。
		msg := fmt.Sprintf("上游 HTTP %d：%s", status, truncateRunes(string(respBody), testChatErrorLimit))
		log.Printf("panel: test_chat uid=%s model=%s 失败(upstream) status=%d %dms",
			req.UID, req.Model, status, elapsed())
		fail(status, msg)
		return
	}
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		log.Printf("panel: test_chat uid=%s model=%s 失败(parse) %dms: %v",
			req.UID, req.Model, elapsed(), err)
		fail(status, wrapErr("解析上游流失败", err))
		return
	}
	reply := chatMessageContent(resp)
	replyRunes := utf8.RuneCountInString(reply)
	// finish_reason/reasoning_chars 是"空回复"的判读依据：思考与最终回答共享
	// max_tokens 预算，思考型模型可能整份预算都花在思考上、content 为空
	// （finish_reason=length）。此时链路其实是通的，报"空回复"会误导诊断。
	finish, reasoningRunes := chatFinishInfo(resp)
	ms := elapsed()
	log.Printf("panel: test_chat uid=%s model=%s ok %dms reply=%d 字符 finish=%s reasoning=%d 字符",
		req.UID, req.Model, ms, replyRunes, finish, reasoningRunes)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"status":     status,
		"latency_ms": ms,
		"reply":      truncateRunes(reply, testChatReplyLimit),
		// reply_chars 是截断前的字符数、reply_truncated 是截断标记：前端不必自己
		// 比对长度（JS 的 length 按 UTF-16 单元数，与 Go 的字符数在 emoji 上不等）。
		"reply_chars":     replyRunes,
		"reply_truncated": replyRunes > testChatReplyLimit,
		"finish_reason":   finish,
		"reasoning_chars": reasoningRunes,
		// max_tokens 回显：空回复时前端据此解释"思考可能吃光了预算"。
		"max_tokens": testChatMaxTokens,
		"model":      req.Model,
		"account":    account,
	})
}

// chatMessageContent 取聚合响应的 choices[0].message.content。
// 结构缺失/类型不符一律返回空串（面板显示"空回复"，不因字段异常炸 handler）。
// reasoning_content 刻意不取：诊断要的是最终答复，思考过程对"能不能用"无判据。
func chatMessageContent(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return ""
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	s, _ := msg["content"].(string)
	return s
}

// chatFinishInfo 取聚合响应的 finish_reason 与 reasoning_content 长度。
// 用途见调用点：content 为空时区分"链路不通"与"预算全被思考吃掉"。
func chatFinishInfo(resp map[string]any) (finish string, reasoningRunes int) {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return "", 0
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return "", 0
	}
	finish, _ = c["finish_reason"].(string)
	if msg, ok := c["message"].(map[string]any); ok {
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoningRunes = utf8.RuneCountInString(rc)
		}
	}
	return finish, reasoningRunes
}

// truncateRunes 按字符截断（超长时补省略号）。不能按字节切：中文被劈成半个字
// 会变成乱码（面板回显与上游错误原文都是中文高频场景）。
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// uidPrefix 展示用 uid 前缀（16 字符，与前端账号行 id 列同口径）。
func uidPrefix(uid string) string {
	r := []rune(uid)
	if len(r) <= 16 {
		return uid
	}
	return string(r[:16]) + "…"
}
