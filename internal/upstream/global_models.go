// global 模型目录探测：产出模型名及其窗口 / 能力元数据；倍率只在**展示只读快照**
// 出口透出，不进路由 / 列表口径（PLAN §3.D2「模型名目录 ≠ 倍率表」）。
//
// 倍率的口径（2026-09-27，对齐 sliver 1dfe750 的只读快照语义）：
//   - 探测端点返回的 credits 字段**照常解析**，但不进 FetchGlobalModelInfos 的返回值
//     ——那个出口服务 /v1/models 与路由，PLAN §3.D2 明确倍率不进该路径；
//   - 倍率单独落在缓存的 credits 旁表里，只经 GlobalModelInfosSnapshot 透出，
//     供 /v1/stats 的展示侧合入。
//
// 为什么用旁表而不是直接填进 ModelInfo：同一个 []ModelInfo 类型若"经 Fetch 取到就没有
// 倍率、经 Snapshot 取到就有"，字段含义随读取口变化，是个认知陷阱（后来者会以为
// Fetch 也能拿到倍率）。旁表把两种口径的边界摆在类型层面：路由/列表口径的条目恒无
// 倍率，展示口径只能从快照拿。
//
// 2026-09-17 修复（保留）：本包原先只产模型名（[]string），导致 handler 的 global 分支
// 拿不到窗口大小、只能输出裸名单，客户端回退到自身小默认值后**提前触发上下文压缩**。
// 现改为产出 []ModelInfo（与 CN 侧同构，上游两端点返回的 JSON 形状一致）。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// GlobalModelNames 国际版（global realm）模型名静态名单兜底（PLAN §7.2 附录 21 名）。
// 只含模型名、不含倍率与元数据。无 global 账号 / 探测失败（5min 负缓存内）/
// GlobalEnabled=false 时以此名单兜底（元数据留空，窗口由 context_catalog 知识表补齐）；
// 探测成功时**以探测结果为基底**（纯动态），本名单中探测未返回的 id 才按 id 补齐。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 handler.dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
// Mutex 内嵌，与 modelList 无并发读路径竞争（唯一读写点本文件内）。
type fetchGlobalModelsCache struct {
	sync.Mutex
	models   []ModelInfo // 成功缓存：探测基底 ∪ 静态独有（已去重）；nil = 未探测
	fetched  time.Time
	lastFail time.Time

	// credits 倍率旁表（裸 id → 上游原文，如 "x0.79"）。**与 models 同生命周期**：
	// 同一次探测一起落、一起清。只经 GlobalModelInfosSnapshot 透出，不进 models
	// （PLAN §3.D2：倍率不进 /v1/models 与路由口径）。
	credits map[string]string
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL          = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global 模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// /v2 家族优先（PR #20 实测 /v2/enterprises/personal/models 200 含完整模型表），
// /console 作 fallback（同域旧路径，或 500）。参考 PLAN v1 §2.2 分歧③ 与
// rockswang/wild-work PR #20 实测结论：console 路径在 global 上非 200 → 先 /v2。
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回**模型名列表**（无元数据）。
//
// 兼容入口：等价于 FetchGlobalModelInfos 后取 ID。新代码请直接用
// FetchGlobalModelInfos（需要窗口 / 能力元数据时）。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	infos := c.FetchGlobalModelInfos(a)
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			out = append(out, mi.ID)
		}
	}
	return out
}

// FetchGlobalModelInfos 探测 global 账号的模型目录并返回**带窗口 / 能力元数据**的条目列表。
//
// 成功：探测结果为基底（纯动态，保上游返回序），静态名单中探测未返回的 id 按 id 补齐
// （如 deepseek-v4.1-flash 上游目录不给、但可调用），缓存 1h。
// 失败（家族端点全非 2xx / 解析失败 / 空列表）：记 5min 负缓存，回落静态名单（无元数据）。
// 缓存/负缓存命中：直接返回，零上游调用。
//
// 调用方负责：① 仅在有 global 账号时调用（无则不探测）；
// ② GlobalEnabled 关闭时（逃生门）不得调用——本方法由 globalOn(a) 内部兜底，若账号
// 因开关回落 cn 则返回静态名单（handler 侧仍零探测）。
//
// 返回的 Credits 恒为空（PLAN §3.D2：倍率不进 global 路由/列表口径）。倍率落在
// 缓存旁表里，只经 GlobalModelInfosSnapshot 透出（/v1/stats 展示侧用）。
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	if !c.globalOn(a) {
		// 逃生门兜底：账号不路由 global 上游 → 不探测，回落静态名单（零上游调用）。
		return staticGlobalModelInfos()
	}

	c.globalModels.Lock()
	if len(c.globalModels.models) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		out := c.globalModels.models
		c.globalModels.Unlock()
		return out
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（回落静态）。
		c.globalModels.Unlock()
		return staticGlobalModelInfos()
	}
	c.globalModels.Unlock()

	probed, err := c.probeGlobalModels(a)
	if err != nil || len(probed) == 0 {
		// 探测失败：负缓存 + 回落静态名单。倍率旁表一并清空（宁可省略，不可留旧值：
		// 留下上一轮倍率会让 /v1/stats 展示与当前目录脱节的过期数字）。
		c.globalModels.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.models = nil
		c.globalModels.credits = nil
		c.globalModels.Unlock()
		return staticGlobalModelInfos()
	}

	// 成功：探测结果为基底（纯动态），静态独有条目仅按 id 补齐（元数据留空）。
	merged := mergeGlobalModelInfos(probed)
	// 倍率旁表从**探测结果**建（静态独有条目无倍率：它们上游没返回，倍率未知——
	// 不编造）。探测结果里的 Credits 字段只在此处被读取，落旁表后即从 models 抹掉。
	credits := make(map[string]string, len(probed))
	for i := range merged {
		if cr := merged[i].Credits; cr != "" {
			credits[merged[i].ID] = cr
			merged[i].Credits = "" // D2：路由/列表口径恒无倍率
		}
	}

	c.globalModels.Lock()
	c.globalModels.models = merged
	c.globalModels.credits = credits
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
	return merged
}

// GlobalModelInfosSnapshot 只读 global 模型目录快照（TTL 内；含倍率原文）。
//
// 语义（对齐 sliver 1dfe750）：**只读、不触发探测**。缓存冷 / 过期 / 未探测 → nil，
// 绝不发起上游请求——与 FetchGlobalModelInfos 的差异点（那个 miss 即探测，服务
// /v1/models）。本方法服务 /v1/stats 的倍率透出：统计端点被面板高频轮询，任何
// 上游调用都会变成对上游的额外压力。
//
// 返回的条目是 models 的副本，且把旁表里的倍率填回 Credits——**仅本出口**带倍率
// （见包注释：字段含义不随读取口漂移的做法是旁表 + 单一展示出口，而不是让 Fetch
// 也带倍率）。未下发的模型 Credits 保持空串：**缺失 ≠ 免费**，调用方必须整体省略
// 该字段，不得回填 "x0.00"。
func (c *Client) GlobalModelInfosSnapshot() []ModelInfo {
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	if len(c.globalModels.models) == 0 || time.Since(c.globalModels.fetched) >= globalModelsTTL {
		return nil
	}
	out := make([]ModelInfo, len(c.globalModels.models))
	copy(out, c.globalModels.models)
	for i := range out {
		out[i].Credits = c.globalModels.credits[out[i].ID]
	}
	return out
}

// staticGlobalModelInfos 静态名单 → []ModelInfo（仅 ID，元数据留空，由调用方经
// context_catalog 知识表补齐窗口 / 输出上限）。
func staticGlobalModelInfos() []ModelInfo {
	out := make([]ModelInfo, 0, len(GlobalModelNames))
	for _, id := range GlobalModelNames {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, ModelInfo{ID: id})
		}
	}
	return out
}

// mergeGlobalModelInfos 合并探测结果与静态名单：**探测为基底**（纯动态，保上游返回序，
// 探测内部重复 id 后者覆盖前者），静态名单中探测未返回的 id 追加在尾部（元数据留空）。
// 静态独有条目只补 id 不编造能力——真值由 context_catalog 知识表按 id 提供。
func mergeGlobalModelInfos(probed []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(probed)+len(GlobalModelNames))
	seen := make(map[string]bool, cap(out))
	for _, mi := range probed {
		id := strings.TrimSpace(mi.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		mi.ID = id
		out = append(out, mi)
	}
	for _, id := range GlobalModelNames {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id}) // 静态独有：上游未返回，元数据留空
	}
	return out
}

// probeGlobalModels 按候选路径序列发起一次探测，返回条目列表（未去重、已滤 disabled）。
// 家族端点全部非 2xx（等幂探活）才返回错误。
func (c *Client) probeGlobalModels(a *auth.Auth) ([]ModelInfo, error) {
	var lastErr error
	for _, path := range globalModelsProbePaths {
		infos, err := c.globalModelsOnce(a, path)
		if err != nil {
			lastErr = err
			continue
		}
		return infos, nil
	}
	return nil, lastErr
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (infos, nil)；否则 (nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]ModelInfo, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	// global 模型目录探测同 FetchModels：账号级业务路径，注入设备指纹头同口径。
	c.injectAccountStableHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（探测负缓存走 lastFail，不罚号）。
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	return parseGlobalModelInfos(raw)
}

// parseGlobalModelInfos 容忍两种形态解析模型目录，产出带窗口 / 能力元数据的条目：
//   - 对象数组（主形态，与 CN /console/enterprises/personal/models 同构）：data.models[]，
//     maxInputTokens→ContextWindow、maxOutputTokens→MaxTokens、maxAllowedSize、
//     supportsReasoning / supportsImages / reasoning.*；id 缺省时回退 name；disabled 剔除；
//   - 窄表：data 为字符串数组 → 仅 ID，元数据留空（窗口由调用方兜底）。
//
// credits（倍率）**照常解析**，但只作旁表来源：调用方 FetchGlobalModelInfos 在落缓存时
// 把 Credits 摘进旁表并从返回条目里抹掉（PLAN §3.D2：倍率不进 /v1/models 与路由口径），
// 只有 GlobalModelInfosSnapshot 会把它填回（展示侧）。
// 解析成功但名单为空 → 返回错误（调用方回落静态，等价"该端点没给全"）。
func parseGlobalModelInfos(raw []byte) ([]ModelInfo, error) {
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("global models code=%d", env.Code)
	}
	trimmed := strings.TrimSpace(string(env.Data))
	if strings.HasPrefix(trimmed, "[") {
		// 窄表形态：data 为字符串数组（无对象字段 → 无倍率）。
		var arr []string
		if err := json.Unmarshal(env.Data, &arr); err != nil {
			return nil, fmt.Errorf("global models parse (narrow): %w", err)
		}
		out := make([]ModelInfo, 0, len(arr))
		for _, id := range arr {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, ModelInfo{ID: id})
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("global models empty list")
		}
		return out, nil
	}
	// 对象形态：data.models[]，字段名与 CN 目录一致（maxInputTokens/maxOutputTokens/…）。
	var obj struct {
		Models []struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			MaxInputTokens    int64  `json:"maxInputTokens"`
			MaxOutputTokens   int64  `json:"maxOutputTokens"`
			MaxAllowedSize    int64  `json:"maxAllowedSize"`
			Disabled          bool   `json:"disabled"`
			Credits           string `json:"credits"`
			SupportsReasoning bool   `json:"supportsReasoning"`
			SupportsImages    bool   `json:"supportsImages"`
			Reasoning         struct {
				Effort             string   `json:"effort"`        // 老模型键
				DefaultEffort      string   `json:"defaultEffort"` // 新模型键
				CanDisableThinking bool     `json:"canDisableThinking"`
				SupportedEfforts   []string `json:"supportedEfforts"`
			} `json:"reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(env.Data, &obj); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(obj.Models))
	for _, m := range obj.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" || m.Disabled {
			continue
		}
		def := m.Reasoning.Effort
		if def == "" {
			def = m.Reasoning.DefaultEffort // 新旧双键兼容，与 CN 侧同款
		}
		out = append(out, ModelInfo{
			ID:                 id,
			Name:               m.Name,
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			MaxAllowedSize:     m.MaxAllowedSize,
			Efforts:            m.Reasoning.SupportedEfforts,
			DefaultEffort:      def,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsImages:     m.SupportsImages,
			// Credits 在此解析，但只作旁表来源：调用方落缓存时摘进 credits 旁表
			// 并从返回条目抹掉（PLAN §3.D2：倍率不进 /v1/models 与路由口径）。
			Credits: strings.TrimSpace(m.Credits),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("global models empty list")
	}
	return out, nil
}
