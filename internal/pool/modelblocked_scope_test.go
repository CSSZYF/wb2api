package pool

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"testing"
)

func TestModelBlockedRespectsRealmAndManualDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.BlockModelBackoff("cn", "model", "11102 model not available")
	if !p.ModelBlocked("model", "cn").Blocked {
		t.Fatal("explicit CN scope must report model lock")
	}
	if p.ModelBlocked("model", "").Blocked {
		t.Fatal("healthy global fallback must remain available")
	}
	p.SetManualDisabled("global", true, "test")
	if !p.ModelBlocked("model", "").Blocked {
		t.Fatal("manually disabled account must not hide the model lock")
	}
	p.SetManualDisabled("cn", true, "test")
	if p.ModelBlocked("model", "").Blocked {
		t.Fatal("empty eligible pool is not a model lock")
	}
}
