// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位（绝不整串进日志）
	nick   string // 账号昵称（auth.Auth.Nickname，登录时落盘）；空则只显示 uid8
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// 缓存三段与真实扣费（末帧 usage）。字段名是上游实测值（见 upstream/cache_key.go
	// 的逆向记录：带 prompt_cache_key 时 prompt_cache_hit_tokens=7808、credit≈0.02）。
	// hasCache 区分「上游给了缓存字段」与「上游根本没这个字段」——缺失 ≠ 全 0。
	cacheHit  int
	cacheMiss int
	cacheWr   int
	hasCache  bool
	credit    float64
	hasCredit bool
	pend      []byte // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// CacheTokens 返回末帧 usage 的缓存三段与是否有观测（供 /v1/stats 的命中率聚合）。
// 缺观测时 ok=false，调用方据此把三个 0 记成「没看到」而不是「命中 0」。
func (s *chatStatsReader) CacheTokens() (hit, miss, write int, ok bool) {
	return s.cacheHit, s.cacheMiss, s.cacheWr, s.hasCache
}

// Credit 返回末帧 usage.credit（本次真实扣费）与是否有观测。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// HasTTFB 报告是否观测到首帧。用 seen 而非 ttfb>0 判定：首帧在 1ms 内到达时
// ttfb 截断为 0ms，用 >0 判定会把它误记成「无观测」，把这次样本从均值里漏掉。
func (s *chatStatsReader) HasTTFB() bool { return s.seen }

// attemptObs 一次尝试里「统计端点专用」的观测（/v1/stats 数据源）。
//
// 为什么不并进 pool.TokenUsageDelta：那是账号级累计器的入参，加字段会让 pool 的
// 持久化结构跟着膨胀（那本账只要 token/延迟/吞吐）；本结构只喂 usage 记录器。
// 两者读同一份上游 usage，但目标字段集不同，各自演进反而不会互相牵制。
type attemptObs struct {
	Stream     bool
	TTFB       time.Duration
	HasTTFB    bool
	CacheHit   int64
	CacheMiss  int64
	CacheWrite int64
	HasCache   bool
	Credit     float64
	HasCredit  bool
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
			// 缓存三段与真实扣费：指针区分「缺失」与「显式 0」——上游不返回这些
			// 字段时不能当成「命中 0 个 token」（会把命中率算成 0，看起来像缓存全失效）。
			PromptCacheHitTokens   *int     `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens  *int     `json:"prompt_cache_miss_tokens"`
			PromptCacheWriteTokens *int     `json:"prompt_cache_write_tokens"`
			Credit                 *float64 `json:"credit"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	// 缓存三段按「任一字段出现」标记有观测：上游可能只给 hit（miss/write 省略），
	// 此时未给的字段按 0 计入是对的（上游确实没算那一段的费用）。
	if chunk.Usage.PromptCacheHitTokens != nil {
		s.hasCache = true
		s.cacheHit = *chunk.Usage.PromptCacheHitTokens
	}
	if chunk.Usage.PromptCacheMissTokens != nil {
		s.hasCache = true
		s.cacheMiss = *chunk.Usage.PromptCacheMissTokens
	}
	if chunk.Usage.PromptCacheWriteTokens != nil {
		s.hasCache = true
		s.cacheWr = *chunk.Usage.PromptCacheWriteTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// usageExtraFromResponse 从非流式聚合响应里取缓存三段与真实扣费（供 /v1/stats）。
//
// 与 usageDeltaFromResponse 的分工：那个函数服务 token 三段（pool 与 usage 两处
// 累计器都要），本函数只补统计端点额外需要的缓存/扣费观测。两者读同一份 usage，
// 但目标字段不同，故不复用——强行合并会让账本依赖统计的字段集，反之亦然。
//
// 返回的 ok 表示「上游给了任一缓存字段」：全缺时调用方按「无观测」记账，
// 而不是把三个 0 当命中 0 累加（那会把命中率永久压低）。
func usageExtraFromResponse(resp map[string]any) (hit, miss, write int64, ok bool, credit float64, hasCredit bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, 0, false, 0, false
	}
	read := func(key string) (int64, bool) {
		v, present := u[key]
		if !present {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, has := read("prompt_cache_hit_tokens"); has {
		hit, ok = n, true
	}
	if n, has := read("prompt_cache_miss_tokens"); has {
		miss, ok = n, true
	}
	if n, has := read("prompt_cache_write_tokens"); has {
		write, ok = n, true
	}
	if c, has := u["credit"].(float64); has {
		credit, hasCredit = c, true
	}
	return hit, miss, write, ok, credit, hasCredit
}

// obsFromResponse 由非流式聚合响应派生统计观测（缓存三段 + 扣费）。
// 非流式没有「首帧」概念，HasTTFB 恒 false——计入会把首字均值拉低失真。
func obsFromResponse(resp map[string]any) attemptObs {
	hit, miss, write, ok, credit, hasCredit := usageExtraFromResponse(resp)
	return attemptObs{
		CacheHit:   hit,
		CacheMiss:  miss,
		CacheWrite: write,
		HasCache:   ok,
		Credit:     credit,
		HasCredit:  hasCredit,
	}
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 保留本函数是因为 logging_test.go 直接断言它；实现委托 logfmt.UID8，避免
// "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

// chatAcctWidth 流水行账号列的显示宽度（列宽非字节）。取固定宽度而不是让内容自然
// 长度撑开，是为了让 stdout 里成百上千行能竖着扫：中文昵称按字节补空格会错位
// （"猫" 3 字节 2 列），整张表往上缩，肉眼没法一列列对齐着看。
//
// 22 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列，留
// 2 列余量。只补不截（见 logfmt.Pad）：昵称超宽时让该行自然变宽，不丢信息。
const chatAcctWidth = 22

// modelLogWidth 表格日志中模型名的显示宽度（按 rune 计）。
//
// 取 32 的理由：覆盖现实中最长的形态——带显式域前缀的 "global:deepseek-v4.1-flash"
// 是 26 字符；同时对畸形输入保留上限，防止单行日志被超长模型名撑爆。
const modelLogWidth = 32

// shortModel 按 rune 截断模型名，截断时追加 "…" 标记。
//
// 为什么必须留标记：早期实现是硬切前 11 个字符（model[:11]），而
// "deepseek-v4.1-flash" 的前 11 个字符恰好是 "deepseek-v4" —— 一个看起来
// 完全合理的**另一个模型名**。于是日志显示的模型与客户端实际请求的模型不是同一个，
// 排查时把人往完全相反的方向带。截断成"合法名字"比截断成乱码危险得多；
// 带 "…" 之后，"名字不完整"这件事自身可见，不可能再被误读成另一个模型。
//
// 按 rune 而非字节切：客户端可能塞入非 ASCII 模型名，按字节切会产出非法 UTF-8，
// 在面板/终端里显示成乱码。
func shortModel(model string, width int) string {
	if width <= 0 {
		return model
	}
	r := []rune(model)
	if len(r) <= width {
		return model
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 modelLogWidth 按 rune 截断并补 "…"（见 shortModel）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只打 uid8
//     时人眼无法判断是哪个号，要辨认必须再查 auths/ 或 state.json，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
//
// 账号列按**显示列宽**右补空格（logfmt.Pad）：中文昵称按 2 列/字算，不再因按字节数
// 补齐而错位——账号列定宽后，右侧 TTFB/tok 等列在成百行里自然竖着对齐。账号列只补
// 不截：昵称是排查主线索，超宽时宁可让该行变宽，也不丢昵称。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = shortModel(model, modelLogWidth)
	// 账号列保留 "acct=" 键值前缀（上游 b61d7b4 是裸值列）：本仓日志一律键值写法
	// （uid=/tok=/TTFB=），保留前缀才能 grep 'acct=' 直接定位账号列，不必按列序号数位。
	acct := "acct=" + logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
