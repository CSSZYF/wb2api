package upstream

import "testing"

// TestContextWindowListingRemoteWins 上游动态值（maxInputTokens>0）为权威：
// 无视知识表直接透出（包括与知识表不同的值，不「纠正」上游）。
func TestContextWindowListingRemoteWins(t *testing.T) {
	if got, ok := ContextWindowListing("glm-5.2", 262144); !ok || got != 262144 {
		t.Errorf("remote wins: context_length=%d,%v want 262144,true (remote authoritative)", got, ok)
	}
	// 远端小值也权威——不按知识表「纠正」。
	if got, ok := ContextWindowListing("glm-5.2", 32000); !ok || got != 32000 {
		t.Errorf("small remote: context_length=%d,%v want 32000,true (remote authoritative)", got, ok)
	}
	// 知识表未收录的 id + 远端有值 → 仍权威。
	if got, ok := ContextWindowListing("dyn-zero-ctx", 65536); !ok || got != 65536 {
		t.Errorf("unknown id remote: context_length=%d,%v want 65536,true", got, ok)
	}
}

// TestContextWindowListingKnowledgeTable 知识表命中：远端零值 → 按模型补齐真实量级，
// 不再透出假 131072。
func TestContextWindowListingKnowledgeTable(t *testing.T) {
	cases := []struct {
		model, source string
		want          int64
	}{
		{"glm-5.2", "fork 实测 CN 1M", 1000000},
		{"glm-5.1", "fork 实测 200K", 200000},
		{"glm-5v-turbo", "fork 实测 200K", 200000},
		{"kimi-k2.7", "fork 实测 256K", 256000},
		{"kimi-k2.6", "fork 实测 256K", 256000},
		{"minimax-m3", "fork 实测 512K", 512000},
		{"deepseek-v4-pro", "fork 实测 1M", 1000000},
		{"deepseek-v4-flash", "fork 实测 1M", 1000000},
		{"deepseek-v4.1-flash", "实测外推 + models.dev", 1000000},
		{"hy3", "fork 实测 192K", 192000},
		{"hy3-preview", "models.dev", 262144},
		{"hy3-preview-agent", "同族外推", 262144},
		{"hy4-preview-f", "本仓目录", 1000000},
		{"gpt-6-astra", "models.dev", 1050000},
		{"gpt-5.3-codex", "models.dev", 400000},
		{"gemini-3.5-flash", "models.dev", 1048576},
		{"auto", "fork global 外推", 168000},
	}
	for _, c := range cases {
		if got, ok := ContextWindowListing(c.model, 0); !ok || got != c.want {
			t.Errorf("%s (%s): context_length=%d,%v want %d,true", c.model, c.source, got, ok, c.want)
		}
	}
}

// TestContextWindowListingUnknownOmitted 知识表也未收录 → ok=false，
// 调用方省略 context_length 字段（**不编造、不落 1M**）。
//
// 与上游 sliver 版刻意不同：那边未知兜底 1M（"宁可高估不低估"），本仓认为 1M 同样是
// 编造值——客户端拿到不存在的窗口会在真正触顶时才被上游报错，代价只是换了个形态。
func TestContextWindowListingUnknownOmitted(t *testing.T) {
	if got, ok := ContextWindowListing("totally-unknown-model", 0); ok || got != 0 {
		t.Errorf("unknown model: context_length=%d,%v want 0,false (omit, not 1M)", got, ok)
	}
	if got, ok := ContextWindowListing("", 0); ok || got != 0 {
		t.Errorf("empty model: context_length=%d,%v want 0,false", got, ok)
	}
	if got, ok := ContextWindowListing("  ", 0); ok || got != 0 {
		t.Errorf("blank model: context_length=%d,%v want 0,false", got, ok)
	}
}

// TestMaxOutputTokensListing max_output_tokens 与 context_length 同口径：
// remote 权威 → 知识表 → 未知省略（ok=false），不编造。
func TestMaxOutputTokensListing(t *testing.T) {
	// remote 权威。
	if got, ok := MaxOutputTokensListing("glm-5.2", 64000); !ok || got != 64000 {
		t.Errorf("remote: max_output_tokens=%d,%v want 64000,true", got, ok)
	}
	// 知识表命中。
	if got, ok := MaxOutputTokensListing("deepseek-v4-pro", 0); !ok || got != 384000 {
		t.Errorf("table: max_output_tokens=%d,%v want 384000,true", got, ok)
	}
	// 知识表条目但输出上限未知（kimi-k2.8-preview/auto）→ 省略。
	if got, ok := MaxOutputTokensListing("auto", 0); ok || got != 0 {
		t.Errorf("auto: max_output_tokens=%d,%v want 0,false (unknown → omit)", got, ok)
	}
	if got, ok := MaxOutputTokensListing("kimi-k2.8-preview", 0); ok || got != 0 {
		t.Errorf("kimi-k2.8-preview: max_output_tokens=%d,%v want 0,false", got, ok)
	}
	// 完全未知 → 省略。
	if _, ok := MaxOutputTokensListing("totally-unknown-model", 0); ok {
		t.Error("unknown model max_output_tokens should be omitted")
	}
}

// TestContextCatalogCoversGlobalStaticNames global 静态名单的每个**真实模型** id 都必须在
// 知识表里命中（无 global 账号 / 探测失败时该名单是唯一输出，元数据只能来自知识表）。
//
// 路由别名（DefaultHiddenModels：default-model 等 5 个）刻意**不入表**：它们由上游按
// 当时的策略转派到别的真实模型，窗口/倍率随策略漂移（hidden.go 的既有结论），
// 给它们写死窗口就是编造——宁可省略字段。
//
// 静态 CN 表（server 包）的同类断言在 internal/server 侧（那里能读到 staticCNModelIDs）。
func TestContextCatalogCoversGlobalStaticNames(t *testing.T) {
	aliases := ResolveHiddenModels(nil)
	for _, id := range GlobalModelNames {
		if aliases.Has(id) {
			if _, ok := lookupContextCap(id); ok {
				t.Errorf("路由别名 %s 不应入知识表（窗口随转派策略漂移，写死即编造）", id)
			}
			continue
		}
		if _, ok := lookupContextCap(id); !ok {
			t.Errorf("global 静态名单模型 %s 不在知识表内（探测失败时会缺 context_length）", id)
		}
	}
}

// TestContextCatalogNoLegacy131072 回归锚点：131072 假兜底已退役——知识表任意条目
// 不得再以 131072 作为 context_length（除非真值恰为 128K，本表当前无此值）。
func TestContextCatalogNoLegacy131072(t *testing.T) {
	for model, cap := range contextCapFallback {
		if cap.context == 131072 {
			t.Errorf("%s: knowledge table context=131072 (legacy fake fallback leaked)", model)
		}
	}
}

// TestContextCatalogValuesPositive 表完整性：每条 context 必为正（0/负条目无意义，
// 会静默落到"未知省略"）；maxOutput 为 0 语义是「未知省略」，不得为负。
func TestContextCatalogValuesPositive(t *testing.T) {
	for model, cap := range contextCapFallback {
		if cap.context <= 0 {
			t.Errorf("%s: context=%d must be positive", model, cap.context)
		}
		if cap.maxOutput < 0 {
			t.Errorf("%s: maxOutput=%d must be >=0", model, cap.maxOutput)
		}
	}
}
