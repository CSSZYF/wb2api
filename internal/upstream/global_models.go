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
	"log"
	"net/http"
	"sort"
	"strconv"
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
	// v3/config 双 UA 能力表（吸收上游 9dce68a）：既作**能力补全 / 独有 id 补充**源
	// （企业端点成功时），也作**目录兜底**源（企业端点全失败时）——企业端点挂了不等于
	// 「上游没模型」，v3/config 是同一批账号可用的另一条目录来源，此时回落静态名单会
	// 让客户端在上游其实可用的情况下少看到模型。两路全失败才回落静态 + 负缓存。
	v3Cap, v3OK := c.probeGlobalV3Capabilities(a)

	if err != nil || len(probed) == 0 {
		if v3OK {
			// 企业端点失败但 v3 可用：以 v3 目录兜底（补静态独有 id），**不算失败**
			// （不写负缓存——上游确实返回了可用目录，只是走了另一条端点）。
			merged := mergeGlobalModelInfos(v3CatalogInfos(v3Cap))
			return c.storeGlobalModels(merged, nil)
		}
		// 两路全失败：负缓存 + 回落静态名单。倍率旁表一并清空（宁可省略，不可留旧值：
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
	// 能力补全 + 独有 id 补充（含试用横幅模型，b498416）：企业端点只给裸 id 时，
	// v3 是唯一的能力来源。v3 两路全失败 → 保持目录不变（能力字段由 context_catalog
	// 知识表兜底，fail-soft：v3 挂了不得把目录整体打空）。
	if v3OK {
		merged = applyGlobalV3Catalog(merged, v3Cap)
	}
	// 倍率旁表从**探测结果**建（静态独有条目无倍率：它们上游没返回，倍率未知——
	// 不编造；v3 追加的条目同样无倍率——v3 的 credits 是展示口径的优惠信息，
	// 与探测端点的计费倍率不是同一事实）。探测结果里的 Credits 字段只在此处被读取，
	// 落旁表后即从 models 抹掉。
	credits := make(map[string]string, len(probed))
	for i := range merged {
		if cr := merged[i].Credits; cr != "" {
			credits[merged[i].ID] = cr
			merged[i].Credits = "" // D2：路由/列表口径恒无倍率
		}
	}
	return c.storeGlobalModels(merged, credits)
}

// storeGlobalModels 落缓存并返回目录（成功路径的公共尾巴：写 models/credits/fetched、
// 清负缓存）。credits 为 nil 时清空旁表（v3 兜底路径无探测倍率）。
func (c *Client) storeGlobalModels(merged []ModelInfo, credits map[string]string) []ModelInfo {
	c.globalModels.Lock()
	c.globalModels.models = merged
	c.globalModels.credits = credits
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
	return merged
}

// v3CatalogInfos 把 v3 能力表转成目录基底（企业端点不可用时的兜底源）：
// 按 id 排序保证输出稳定（map 迭代序随机），并过 nonChatModel 过滤（v3 目录含
// 嵌入/补全/图片生成类条目，选了会报 11102）。Credits 恒清空（PLAN §3.D2）。
func v3CatalogInfos(cap map[string]ModelInfo) []ModelInfo {
	ids := make([]string, 0, len(cap))
	for id, mi := range cap {
		if id == "" {
			continue
		}
		if nonChatModel(mi.ID, mi.MaxTokens, nil) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		mi := cap[id]
		mi.Credits = "" // D2：倍率不进路由/列表口径
		out = append(out, mi)
	}
	return out
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

// probeGlobalV3Capabilities 并发两路 /v3/config（IDE UA + CLI UA）取并集
// （吸收上游 9dce68a）。
//
// /v3/config 对不同 User-Agent 下发的模型集合不同（上游 2026-09-22 实测）：
//   - CodeBuddyIDE/4.12.0 → 14 条，含 o4-mini / enhance-1.0 / auto-chat，**无 deepseek 系列**；
//   - CLI/2.63.2 CodeBuddy/2.63.2 → 22 条，含 deepseek-v4.1-flash / gpt-6-astra 等。
//
// 两路各有独有模型（IDE 响应体积更大 = 单条字段更全，CLI 模型数更多），单纯换 UA
// 只会引入新的缺失，故并发两路 + 并集。单路失败降级到另一路，两路全失败 → ok=false
// （调用方保持既有目录，能力字段由 context_catalog 知识表兜底——fail-soft，探测失败
// 不得把目录整体打空）。
//
// 本仓与上游的架构差异：上游把 v3 目录当 global 目录的**主源**（企业端点补缺），
// 本仓的目录主源是企业端点（见 probeGlobalModels），v3 只作**能力覆盖源 + 独有 id
// 补充源**（applyGlobalV3Catalog）。效果一致（v3 独有的可调用模型同样进目录），
// 但不改动「探测失败回落静态名单」的既有降级链。
func (c *Client) probeGlobalV3Capabilities(a *auth.Auth) (map[string]ModelInfo, bool) {
	type v3Result struct {
		models map[string]ModelInfo
		err    error
	}
	probe := func(ua string) chan v3Result {
		ch := make(chan v3Result, 1)
		go func() {
			m, err := c.fetchV3ConfigModelMap(a, ua)
			ch <- v3Result{models: m, err: err}
		}()
		return ch
	}
	ideCh := probe(codeBuddyIDEUA)
	cliCh := probe(codeBuddyCLIUA)
	ide, cli := <-ideCh, <-cliCh

	switch {
	case ide.err != nil && cli.err != nil:
		return nil, false
	case ide.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config IDE-UA probe failed (CLI-UA only): %v", ide.err)
		return cli.models, len(cli.models) > 0
	case cli.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config CLI-UA probe failed (IDE-UA only): %v", cli.err)
		return ide.models, len(ide.models) > 0
	default:
		return mergeV3CapabilityMaps(ide.models, cli.models), true
	}
}

// mergeV3CapabilityMaps 两路 v3 能力表合并：primary（IDE 路）字段权威——响应更大、
// 单条字段更全；secondary（CLI 路）只补 primary 没有的 id，不覆盖已有条目。
func mergeV3CapabilityMaps(primary, secondary map[string]ModelInfo) map[string]ModelInfo {
	out := make(map[string]ModelInfo, len(primary)+len(secondary))
	for id, mi := range primary {
		out[id] = mi
	}
	for id, mi := range secondary {
		if _, exists := out[id]; !exists {
			out[id] = mi
		}
	}
	return out
}

// applyGlobalV3Catalog 把 v3 能力表并入 global 目录（吸收上游 9dce68a + b498416）：
//
//  1. **能力补全**：目录里已有的 id，用 v3 条目填补其零值能力字段（窗口 / 输出上限 /
//     档位 / 能力旗标）——企业端点只给裸 id 时，v3 是唯一的能力来源（与 CN 侧
//     mergeModelCapabilities 同哲学：只填有值的字段，不抹掉目录已解析出的结果）。
//  2. **独有 id 补充**：v3 有而目录没有的 id（如 deepseek-v4.1-flash-sg、kimi-k2.8-preview、
//     o4-mini，以及 ModelTrialBanner 的试用模型）追加进目录——它们**实际可调用**，
//     不补则客户端选不到。追加前过 nonChatModel 过滤（v3 目录含嵌入/图片类条目），
//     并按 id 排序保证输出稳定（map 迭代序随机）。
//
// Credits 不进本路径（PLAN §3.D2）：调用方在建倍率旁表时从**探测结果**取，
// 本函数追加的条目 Credits 恒空（试用模型更是刻意清空，见 fetchV3ConfigModelMap）。
func applyGlobalV3Catalog(base []ModelInfo, cap map[string]ModelInfo) []ModelInfo {
	if len(cap) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for i := range base {
		seen[base[i].ID] = true
		ov, ok := cap[base[i].ID]
		if !ok {
			continue
		}
		// 只填零值（fill-only）：目录已有真值时不覆盖，避免把企业端点的权威窗口
		// 换成 v3 的另一个数（两者实测同源，但保守取值口径不变）。
		if base[i].ContextWindow == 0 {
			base[i].ContextWindow = ov.ContextWindow
		}
		if base[i].MaxTokens == 0 {
			base[i].MaxTokens = ov.MaxTokens
		}
		if base[i].MaxAllowedSize == 0 {
			base[i].MaxAllowedSize = ov.MaxAllowedSize
		}
		if len(base[i].Efforts) == 0 {
			base[i].Efforts = ov.Efforts
		}
		if base[i].DefaultEffort == "" {
			base[i].DefaultEffort = ov.DefaultEffort
		}
		if !base[i].CanDisableThinking {
			base[i].CanDisableThinking = ov.CanDisableThinking
		}
		if !base[i].SupportsReasoning {
			base[i].SupportsReasoning = ov.SupportsReasoning
		}
		if !base[i].SupportsImages {
			base[i].SupportsImages = ov.SupportsImages
		}
		if base[i].Name == "" {
			base[i].Name = ov.Name
		}
	}

	extra := make([]string, 0, len(cap))
	for id, mi := range cap {
		if id == "" || seen[id] {
			continue
		}
		if nonChatModel(mi.ID, mi.MaxTokens, nil) {
			continue
		}
		extra = append(extra, id)
	}
	if len(extra) == 0 {
		return base
	}
	sort.Strings(extra)
	for _, id := range extra {
		mi := cap[id]
		mi.Credits = "" // D2：倍率不进路由/列表口径（旁表只从探测结果建）
		base = append(base, mi)
	}
	return base
}

// parseGlobalModelInfos 多信封兼容解析模型目录（吸收上游 c3cc888）。
//
// 国际站曾在多种 envelope 之间切换，只认单一形态会把登录成功的账号误判为"无模型"：
//   - 信封：code 存在且非 0 才拒（**无 code 字段也放行**——兼容无信封直出的目录端点）；
//     payload = data（非空非 null 时）否则整包；
//   - 模型数组定位（resolveGlobalModelsArray）：payload 本身是数组，或是 map 下
//     models/items/list/data/result 任一键（递归下钻，取第一个能解析出数组的分支）——
//     覆盖 data.models / data.items / data.list / 顶层 models 等变体；
//   - 数组主形态：对象数组按全字段解析（与 CN 目录同构）：maxInputTokens→ContextWindow、
//     maxOutputTokens→MaxTokens、maxAllowedSize、supportsReasoning / supportsImages /
//     reasoning.*；id 缺省时回退 name；disabled 剔除；
//   - 窄表：字符串数组 → 仅 ID，元数据留空（窗口由调用方兜底）；
//   - 动态兜底（parseGlobalModelLoose）：主形态解析不出任何可用条目时（上游换了键名），
//     逐对象宽松解析——id 依次回退 id/modelId/model/name，窗口键回退
//     contextWindow/maxTokens，reasoning 档位新老双键。
//
// credits（倍率）**照常解析**，但只作旁表来源：调用方 FetchGlobalModelInfos 在落缓存时
// 把 Credits 摘进旁表并从返回条目里抹掉（PLAN §3.D2：倍率不进 /v1/models 与路由口径），
// 只有 GlobalModelInfosSnapshot 会把它填回（展示侧）。
// 解析成功但名单为空 → 返回错误（调用方回落静态，等价"该端点没给全"）。
func parseGlobalModelInfos(raw []byte) ([]ModelInfo, error) {
	fail := func(e error) ([]ModelInfo, error) { return nil, e }
	// 顶层裸数组（无信封）：直接进数组定位。上游只认信封形态，本仓多容忍一层——
	// 该形态一旦出现，"目录端点直出数组"就是唯一解释，拒掉等于把登录成功的账号
	// 误判成"无模型"（正是本提交要消灭的形态）。
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return parseGlobalModelArray(raw)
	}
	// 信封层：code 拒绝非 0 业务码（字段缺失 = 放行）。用 RawMessage 逐键取，
	// 容忍 code 为字符串（"0"）与数字两种形态。
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fail(fmt.Errorf("global models parse: %w", err))
	}
	if codeRaw, ok := envelope["code"]; ok {
		if code, ok := parseBusinessCode(codeRaw); ok && code != 0 {
			return fail(fmt.Errorf("global models code=%d", code))
		}
	}
	payload := json.RawMessage(raw)
	if data, ok := envelope["data"]; ok {
		if t := strings.TrimSpace(string(data)); t != "" && t != "null" {
			payload = data
		}
	}
	arr, ok := resolveGlobalModelsArray(payload)
	if !ok {
		return fail(fmt.Errorf("global models empty list"))
	}
	return parseGlobalModelArray(arr)
}

// parseGlobalModelArray 解析已定位到的模型数组（主形态 → 窄表 → 宽松兜底）。
// 解析不出任何可用条目 → 错误（调用方回落静态名单，等价"该端点没给全"）。
func parseGlobalModelArray(arr json.RawMessage) ([]ModelInfo, error) {
	fail := func(e error) ([]ModelInfo, error) { return nil, e }
	// 主形态：对象数组 → 全字段解析（与 CN FetchModels 共用字段口径）。
	var entries []dynModelEntry
	if json.Unmarshal(arr, &entries) == nil {
		out := parseGlobalModelEntries(entries)
		if len(out) > 0 {
			return fillGlobalMetadataFromLoose(out, arr), nil
		}
		// 可用条目为 0（键名全对不上）：落入下方宽松兜底，不在这里报空。
	}

	// 窄表：字符串数组 → 仅 ID（无对象字段 → 无倍率）。
	var strs []string
	if json.Unmarshal(arr, &strs) == nil {
		out := make([]ModelInfo, 0, len(strs))
		for _, id := range strs {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, ModelInfo{ID: id})
			}
		}
		if len(out) > 0 {
			return out, nil
		}
		return fail(fmt.Errorf("global models empty list"))
	}

	// 动态兜底：逐对象宽松解析（上游换键名时不至于整域空列表）。
	var items []any
	if json.Unmarshal(arr, &items) != nil {
		return fail(fmt.Errorf("global models parse: unsupported payload shape"))
	}
	out := make([]ModelInfo, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case string: // 混合数组里的裸 ID：按窄表条目处理
			if id := strings.TrimSpace(v); id != "" {
				out = append(out, ModelInfo{ID: id})
			}
		case map[string]any:
			mi, ok := parseGlobalModelLoose(v)
			if !ok {
				continue
			}
			out = append(out, mi)
		}
	}
	if len(out) == 0 {
		return fail(fmt.Errorf("global models empty list"))
	}
	return out, nil
}

// fillGlobalMetadataFromLoose 主形态解析成功后，再用宽松解析补齐**零值能力字段**。
//
// 为什么需要这一步：主形态（dynModelEntry）只认 maxInputTokens/maxOutputTokens 等
// 标准键，上游换用别名键（contextWindow/maxTokens）时字段会静默留空——条目仍在
// （id 认得出），但窗口丢失，客户端据此回退自身小默认值 → 提前触发上下文压缩。
// 宽松解析覆盖别名键与数字字符串形态，故用它补零值（fill-only，不覆盖主形态已解析
// 出的真值，与 CN 侧 mergeModelCapabilities 同口径）。
//
// 只在主形态成功时调用（失败时整批走宽松路径，见 parseGlobalModelArray）。
func fillGlobalMetadataFromLoose(out []ModelInfo, arr json.RawMessage) []ModelInfo {
	var items []map[string]any
	if json.Unmarshal(arr, &items) != nil {
		return out
	}
	byID := make(map[string]ModelInfo, len(items))
	for _, obj := range items {
		mi, ok := parseGlobalModelLoose(obj)
		if !ok {
			continue
		}
		byID[mi.ID] = mi
	}
	for i := range out {
		loose, ok := byID[out[i].ID]
		if !ok {
			continue
		}
		if out[i].ContextWindow == 0 {
			out[i].ContextWindow = loose.ContextWindow
		}
		if out[i].MaxTokens == 0 {
			out[i].MaxTokens = loose.MaxTokens
		}
		if out[i].MaxAllowedSize == 0 {
			out[i].MaxAllowedSize = loose.MaxAllowedSize
		}
		if len(out[i].Efforts) == 0 {
			out[i].Efforts = loose.Efforts
		}
		if out[i].DefaultEffort == "" {
			out[i].DefaultEffort = loose.DefaultEffort
		}
		if !out[i].CanDisableThinking {
			out[i].CanDisableThinking = loose.CanDisableThinking
		}
		if !out[i].SupportsReasoning {
			out[i].SupportsReasoning = loose.SupportsReasoning
		}
		if !out[i].SupportsImages {
			out[i].SupportsImages = loose.SupportsImages
		}
		if out[i].Name == "" {
			out[i].Name = loose.Name
		}
		if out[i].Credits == "" {
			out[i].Credits = loose.Credits
		}
	}
	return out
}

// dynModelEntry global 目录对象条目的解析结构（字段名与 CN /console 目录一致）。
// ModelID / Model 是宽松回退键（仅 global 目录的多信封变体下发，CN 目录不下发）。
type dynModelEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ModelID         string `json:"modelId"`
	Model           string `json:"model"`
	MaxInputTokens  int64  `json:"maxInputTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	MaxAllowedSize  int64  `json:"maxAllowedSize"`
	Disabled        bool   `json:"disabled"`
	Credits         string `json:"credits"`
	// SupportsReasoning / SupportsImages 是能力旗标。
	SupportsReasoning bool `json:"supportsReasoning"`
	SupportsImages    bool `json:"supportsImages"`
	Reasoning         struct {
		Effort             string   `json:"effort"`        // 老模型键
		DefaultEffort      string   `json:"defaultEffort"` // 新模型键
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// parseBusinessCode 解析信封 code 字段（数字或字符串形态）。非数字/缺失 → ok=false
// （调用方按"无 code 字段"放行——宁放行不误拒，解析失败由下游空列表兜底）。
func parseBusinessCode(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	s = strings.Trim(strings.TrimSpace(strings.Trim(s, `"`)), " ")
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	// 字符串形态里可能带小数尾巴（"0.0"）：按 float 再试一次。
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f), true
	}
	return 0, false
}

// resolveGlobalModelsArray 从 payload 定位模型数组：payload 本身是数组直接用；
// 是 map 则依次尝试 models/items/list/data/result 键（递归下钻，取第一个能解析出
// 数组的分支）。找不到数组 → false。
func resolveGlobalModelsArray(payload json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(payload))
	if strings.HasPrefix(trimmed, "[") {
		return payload, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return nil, false
	}
	for _, key := range []string{"models", "items", "list", "data", "result"} {
		if nested, ok := obj[key]; ok {
			if arr, ok2 := resolveGlobalModelsArray(nested); ok2 {
				return arr, true
			}
		}
	}
	return nil, false
}

// parseGlobalModelEntries 主形态逐条解析（全字段，与 CN 目录字段口径一致）。
// id 回退链 id→modelId→model→name（宽松键仅出现在 global 目录变体里）；disabled 剔除。
func parseGlobalModelEntries(entries []dynModelEntry) []ModelInfo {
	out := make([]ModelInfo, 0, len(entries))
	for _, m := range entries {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.ModelID)
		}
		if id == "" {
			id = strings.TrimSpace(m.Model)
		}
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" || m.Disabled {
			continue
		}
		def := m.Reasoning.DefaultEffort
		if def == "" {
			def = m.Reasoning.Effort // 新旧双键兼容，与 CN 侧同款
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
	return out
}

// parseGlobalModelLoose 单模型对象的宽松解析（多信封兜底路径）：id 依次回退
// id/modelId/model/name；窗口/上限键回退 contextWindow/maxTokens；reasoning 档位
// defaultEffort 新键优先、effort 老键兜底。disabled 剔除。Credits 照常解析（旁表来源）。
func parseGlobalModelLoose(obj map[string]any) (ModelInfo, bool) {
	str := func(key string) string {
		s, _ := obj[key].(string)
		return strings.TrimSpace(s)
	}
	id := str("id")
	for _, k := range []string{"modelId", "model", "name"} {
		if id != "" {
			break
		}
		id = str(k)
	}
	if id == "" {
		return ModelInfo{}, false
	}
	if disabled, ok := obj["disabled"].(bool); ok && disabled {
		return ModelInfo{}, false
	}
	// num 取数值键：float64（json 常规）与**数字字符串**（上游偶发把数字发成字符串，
	// 那会让主形态的整体反序列化失败、整批落到本兜底路径——不认字符串等于白兜底）。
	num := func(keys ...string) int64 {
		for _, k := range keys {
			switch v := obj[k].(type) {
			case float64:
				if v > 0 {
					return int64(v)
				}
			case string:
				if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && n > 0 {
					return int64(n)
				}
			}
		}
		return 0
	}
	mi := ModelInfo{
		ID:             id,
		Name:           str("name"),
		ContextWindow:  num("maxInputTokens", "contextWindow"),
		MaxTokens:      num("maxOutputTokens", "maxTokens"),
		MaxAllowedSize: num("maxAllowedSize"),
		Credits:        str("credits"),
	}
	mi.SupportsReasoning, _ = obj["supportsReasoning"].(bool)
	mi.SupportsImages, _ = obj["supportsImages"].(bool)
	if r, ok := obj["reasoning"].(map[string]any); ok {
		reasonStr := func(key string) string {
			s, _ := r[key].(string)
			return strings.TrimSpace(s)
		}
		mi.DefaultEffort = reasonStr("defaultEffort")
		if mi.DefaultEffort == "" {
			mi.DefaultEffort = reasonStr("effort") // 老模型键兜底，与 CN 侧同款
		}
		mi.CanDisableThinking, _ = r["canDisableThinking"].(bool)
		if arr, ok := r["supportedEfforts"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						mi.Efforts = append(mi.Efforts, s)
					}
				}
			}
		}
	}
	return mi, true
}
