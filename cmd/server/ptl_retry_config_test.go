package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ptl_retry_config_test.go features.ptl_max_tokens_retry 的配置侧验收：
// 默认开（键缺席零影响）、显式 false 才关、env 可覆盖、config.example.json 带该键、
// 生成配置里出现该键。
//
// 为什么默认开是安全的（配置侧口径，与 cmd/server/config.go 的字段注释同源）：
// 本项只在 11115 时生效，且失败路径逐字退回首次的上游原文——最坏情形只是多打一次上游。

// TestPTLMaxTokensRetryDefaultOn 默认开：Default() 与「键缺席」两条路径都必须为 true
// （键缺席时靠 Default()+Unmarshal 覆盖语义保留 true，老配置零影响）。
func TestPTLMaxTokensRetryDefaultOn(t *testing.T) {
	if !Default().Features.PTLMaxTokensRetry {
		t.Error("Default() 的 features.ptl_max_tokens_retry 必须缺省 true")
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 键缺席（老配置）。
	if err := os.WriteFile(fp, []byte(`{"listen":":9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Features.PTLMaxTokensRetry {
		t.Error("配置文件缺 ptl_max_tokens_retry 键时应保持缺省 true（老配置零影响）")
	}
	// 显式 false → 关（逃生门）。
	if err := os.WriteFile(fp, []byte(`{"features":{"ptl_max_tokens_retry":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.PTLMaxTokensRetry {
		t.Error("显式 ptl_max_tokens_retry=false 应关闭")
	}
	// 显式 true → 开。
	if err := os.WriteFile(fp, []byte(`{"features":{"ptl_max_tokens_retry":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(fp); err != nil || !c.Features.PTLMaxTokensRetry {
		t.Errorf("显式 true 应开启: cfg=%+v err=%v", c, err)
	}
}

// TestPTLMaxTokensRetryEnvOverride WB2A_PTL_MAX_TOKENS_RETRY 覆盖文件值
// （ParseBool 口径：只有 true/false/1/0 等合法值生效，非法值静默忽略）。
func TestPTLMaxTokensRetryEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"features":{"ptl_max_tokens_retry":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_PTL_MAX_TOKENS_RETRY", "false")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.PTLMaxTokensRetry {
		t.Error("WB2A_PTL_MAX_TOKENS_RETRY=false 应关闭")
	}
	t.Setenv("WB2A_PTL_MAX_TOKENS_RETRY", "true")
	if c, err = Load(fp); err != nil || !c.Features.PTLMaxTokensRetry {
		t.Errorf("WB2A_PTL_MAX_TOKENS_RETRY=true 应开启: %v %v", c, err)
	}
	// 非法值静默忽略（不报错、不改动已有值）。
	t.Setenv("WB2A_PTL_MAX_TOKENS_RETRY", "maybe")
	if c, err = Load(fp); err != nil || !c.Features.PTLMaxTokensRetry {
		t.Errorf("非法 env 值应静默忽略: %v %v", c, err)
	}
}

// TestPTLMaxTokensRetryInExampleConfig config.example.json 是配置项最完整参考
// （README 明示）——新增键必须同步落进去，否则跟随示例的用户看不到这个开关。
func TestPTLMaxTokensRetryInExampleConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Features map[string]any `json:"features"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json 非法 JSON: %v", err)
	}
	v, ok := doc.Features["ptl_max_tokens_retry"]
	if !ok {
		t.Fatal("config.example.json features 段缺 ptl_max_tokens_retry 键")
	}
	if b, isBool := v.(bool); !isBool || !b {
		t.Errorf("config.example.json 的 ptl_max_tokens_retry = %v，want true（与 Default() 一致）", v)
	}
	// 示例文件必须能被启动路径原样解析（新键不引入校验失败）。
	if _, err := ParseConfig(raw); err != nil {
		t.Errorf("config.example.json 应能被 ParseConfig 解析: %v", err)
	}
}

// TestWriteDefaultIncludesPTLRetryKey 首次运行自动生成的推荐配置必须含该键：
// 它是「程序自动生成推荐配置」的形状来源，缺键会让新特性在用户眼里不存在。
func TestWriteDefaultIncludesPTLRetryKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if _, err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var gen struct {
		Features map[string]any `json:"features"`
	}
	if err := json.Unmarshal(raw, &gen); err != nil {
		t.Fatal(err)
	}
	v, ok := gen.Features["ptl_max_tokens_retry"]
	if !ok {
		t.Errorf("生成的配置缺少 features.ptl_max_tokens_retry 键：%v", gen.Features)
	} else if v != true {
		t.Errorf("ptl_max_tokens_retry 默认应为 true，实际 %v", v)
	}
}
