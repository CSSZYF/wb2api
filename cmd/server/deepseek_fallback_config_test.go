package main

import (
	"encoding/json"
	"testing"
)

func TestDeepseekSGFallbackDefaultAndDisable(t *testing.T) {
	c := Default()
	if !c.Pool.DeepseekSGFallback {
		t.Fatal("fallback default must be enabled")
	}
	if err := json.Unmarshal([]byte(`{"pool":{"deepseek_sg_fallback":false,"reserve_credits":75}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Pool.DeepseekSGFallback || c.Pool.ReserveCredits != 75 {
		t.Fatal("explicit config not respected")
	}
}
