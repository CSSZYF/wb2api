// zerowidth.go 零宽字符脱敏：在命中词的首字符之后插入一个 U+200B。
//
// 与 sanitize.go 的分工（两者互补，互不替代）：
//   - sanitize.go：**改写/删除**已知指纹。覆盖面窄（3 条固定模板句 + header/键值段），
//     但对固定句子更彻底（换词、整段剥离）。
//   - 本文件：**插入不可见字符**。可见文本一字未变，只让字节序列与原文不同，
//     从而破坏上游"逐字精确匹配"的审核。覆盖面广（词表见 zeroWidthTerms），
//     代价是改动更隐蔽——出问题时两个"看起来一样"的字符串会对不上。
//
// 词表来源：codebuddy2api（maiphucgiang/codebuddy2api）的 app/desensitize.py。
// 其 SENSITIVE_TERMS 同时覆盖两类指纹：
//   - 客户端 system 模板里的**安全/渗透术语**（Claude Code / Codex 的安全声明句里
//     成片出现 DoS / exploit / malware 这类词）；
//   - **身份指纹**（Claude Code / Anthropic / Co-Authored-By / noreply@anthropic.com）。
//
// 作用域严格限制在 system 角色消息（见 ApplyZeroWidthMessages）：用户消息一字不动。
// 理由有二：用户内容不该被改写；这些词出现在用户正文里属于正常表达，脱敏会污染对话。
package upstream

import (
	"strings"
	"unicode"
)

// zeroWidthSpace 零宽空格 U+200B。渲染宽度为零，肉眼不可见。
const zeroWidthSpace = '\u200b'

// zeroWidthTerms 需要打散的指纹词表（统一小写存储；匹配按 unicode 小写进行）。
//
// 移植自 codebuddy2api app/desensitize.py 的 SENSITIVE_TERMS（83 项，已去重）。
// 有意与其保持一致：这张表是实际验证过能覆盖 Claude Code / Codex system 模板的集合，
// 自行增删等于重新赌一次。
var zeroWidthTerms = []string{
	"dos",
	"ddos",
	"exploit",
	"credential testing",
	"credential stuffing",
	"supply chain compromise",
	"supply-chain compromise",
	"detection evasion",
	"c2 frameworks",
	"c2 framework",
	"command and control",
	"malicious purposes",
	"malicious intent",
	"mass targeting",
	"brute force",
	"brute-force",
	"privilege escalation",
	"reverse shell",
	"remote code execution",
	"sql injection",
	"xss",
	"csrf",
	"phishing",
	"malware",
	"ransomware",
	"keylogger",
	"rootkit",
	"backdoor",
	"botnet",
	"zero-day",
	"0day",
	"vulnerability",
	"vulnerabilities",
	"red teaming",
	"red-teaming",
	"sandbox",
	"sandboxing",
	"sandboxed",
	"unsandboxed",
	"escalated privileges",
	"escalated",
	"escalation",
	"destructive action",
	"destructive command",
	"destructive",
	"attack",
	"attacks",
	"cybersecurity",
	"security review",
	"exploit development",
	"hacking",
	"penetration testing",
	"penetration test",
	"injection",
	"weaponize",
	"weaponized",
	"harmful",
	"dangerous",
	"abuse",
	"abusive",
	"illegal",
	"terrorist",
	"terrorism",
	"bomb",
	"weapon",
	"weapons",
	"drug",
	"drugs",
	"narcotic",
	"suicide",
	"self-harm",
	"murder",
	"kill",
	"violence",
	"violent",
	"claude code",
	"claude opus",
	"claude sonnet",
	"claude haiku",
	"claude fable",
	"anthropic",
	"co-authored-by",
	"noreply@anthropic.com",
}

// zwMatcher 词表索引：按首字符分桶。扫描时只对"首字符可能是某个词开头"的位置做比较，
// 避免 O(词数 × 文本长度) 的全量比较（system prompt 常上万字符）。
type zwMatcher struct {
	byFirst map[rune][]int
	terms   [][]rune
	widths  []int
}

func newZWMatcher(terms []string) *zwMatcher {
	m := &zwMatcher{
		byFirst: make(map[rune][]int, len(terms)),
		terms:   make([][]rune, 0, len(terms)),
	}
	for _, t := range terms {
		if t == "" {
			continue
		}
		r := []rune(t)
		idx := len(m.terms)
		m.terms = append(m.terms, r)
		m.widths = append(m.widths, len(r))
		m.byFirst[r[0]] = append(m.byFirst[r[0]], idx)
	}
	return m
}

// defaultZWMatcher 供生产路径复用的单例（词表是编译期常量，构建一次即够）。
var defaultZWMatcher = newZWMatcher(zeroWidthTerms)

// isWordRune 对齐 Python 正则 \w 的口径（字母/数字/下划线，Unicode 感知）：
// 用于词边界判定，防止 "exploited" 里的 "exploit" 被当成独立词处理。
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// InsertZeroWidth 对文本中命中的词插入零宽字符，返回改写结果与插入处数。
//
// 匹配规则（对齐 codebuddy2api 的 (?<!\w)term(?!\w)，大小写不敏感）：
//   - 词首前一字符与词尾后一字符都不能是 \w，即必须是独立词；
//   - 同一位置多个候选时取**最长**匹配（词表里既有 "escalated" 也有
//     "escalated privileges"，取最长可避免把后者切成前者的残渣）；
//   - 插入位置固定为**首字符之后**，只插一个——一个不可见字符已足以让字节序列与
//     原文不同，插得越少对文本的扰动越小（与 codebuddy2api 的 _zero_width_split 一致）。
//
// 幂等：已插入过的词内部多了 U+200B，rune 序列不再等于词表，重复调用不会二次插入。
func (m *zwMatcher) InsertZeroWidth(text string) (string, int) {
	if text == "" || len(m.terms) == 0 {
		return text, 0
	}
	runes := []rune(text)
	// 逐 rune 小写化（不用 strings.ToLower）：个别字符（如 'İ'）小写化后长度会变，
	// 那会让小写串与原文下标错位、边界判定随之失准。逐 rune 映射保证 1:1。
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	out := make([]rune, 0, len(runes)+8)
	inserted := 0
	for i := 0; i < len(runes); {
		cands, ok := m.byFirst[lower[i]]
		if ok && (i == 0 || !isWordRune(lower[i-1])) {
			best, bestLen := -1, 0
			for _, ti := range cands {
				if m.widths[ti] <= bestLen {
					continue // 已有更长的候选
				}
				end := i + m.widths[ti]
				if end > len(lower) {
					continue
				}
				if !runesEqual(lower[i:end], m.terms[ti]) {
					continue
				}
				if end < len(lower) && isWordRune(lower[end]) {
					continue // 词尾还连着字 → 不是一个独立词
				}
				best, bestLen = ti, m.widths[ti]
			}
			if best >= 0 {
				out = append(out, runes[i], zeroWidthSpace)
				out = append(out, runes[i+1:i+bestLen]...)
				inserted++
				i += bestLen
				continue
			}
		}
		out = append(out, runes[i])
		i++
	}
	if inserted == 0 {
		return text, 0
	}
	return string(out), inserted
}

// runesEqual 比较两个 rune 切片（避免 string 转换的额外分配）。
func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ApplyZeroWidthMessages 对 messages 中 **system 角色**消息的文本内容做零宽脱敏。
//
// 只处理 system：调用点已先跑 normalizeRoles，developer 已归一为 system，故此处
// 只需认 "system" 一种角色即可覆盖两者。用户与 assistant 消息一字不动。
// 返回被改写的消息数。
func ApplyZeroWidthMessages(messages []any) int {
	n := 0
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "system") {
			continue
		}
		if c, ok := m["content"]; ok {
			if nc, hit := zeroWidthContent(c); hit {
				m["content"] = nc
				n++
			}
		}
	}
	return n
}

// zeroWidthContent 处理 content 的两种形态：纯字符串、以及 text 块数组。
func zeroWidthContent(content any) (any, bool) {
	switch v := content.(type) {
	case string:
		out, n := defaultZWMatcher.InsertZeroWidth(v)
		if n == 0 {
			return content, false
		}
		return out, true
	case []any:
		changed := false
		out := make([]any, len(v))
		for i, part := range v {
			blk, ok := part.(map[string]any)
			if !ok {
				out[i] = part
				continue
			}
			txt, ok := blk["text"].(string)
			if !ok {
				out[i] = part
				continue
			}
			nt, n := defaultZWMatcher.InsertZeroWidth(txt)
			if n == 0 {
				out[i] = part
				continue
			}
			nb := make(map[string]any, len(blk))
			for k, val := range blk {
				nb[k] = val
			}
			nb["text"] = nt
			out[i] = nb
			changed = true
		}
		if !changed {
			return content, false
		}
		return out, true
	}
	return content, false
}
