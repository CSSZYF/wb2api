package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.MaxInFlightGlobal != 2 {
		t.Errorf("max_in_flight_global=%d want 2 (WAF P1-1 global 档默认)", c.Pool.MaxInFlightGlobal)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h", c.SoftRateMaxDur)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"max_in_flight_global":4,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.Pool.MaxInFlightGlobal != 4 {
		t.Errorf("max_in_flight_global=%d want 4 (config 覆盖默认)", c.Pool.MaxInFlightGlobal)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", c.SoftRateDur)
	}
	if c.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", c.SoftRateMaxDur)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", c.SoftRateMaxDur)
	}
}

func TestBadSoftRateMax(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
	}
}

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true：
// 老 config 不写这些键，行为必须与从前完全一致。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("travel/activity enabled defaults want true/true, got %v/%v",
			c.Schedule.TravelEnabled, c.Schedule.ActivityEnabled)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写签到/保活小时数组，无新键）加载后仍是启用态，
// 新开关缺省 true、新 hours 回落默认——对老配置零影响。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("new switches must default true on legacy config: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
	// 新 hours 缺省 → 回落默认（非空）。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_enabled":false,"activity_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.TravelEnabled || c.Schedule.ActivityEnabled {
		t.Errorf("want travel/activity disabled: %+v", c.Schedule)
	}
	// 签到/保活开关缺省 true（互不干扰）。
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("checkin/keepalive should stay enabled: %+v", c.Schedule)
	}
	// hours 仍回落默认。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21] even when disabled", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10] even when disabled", c.Schedule.ActivityHours)
	}
}

// TestScheduleTravelActivityInvalidHoursRejected 旅行/活跃非法小时报错并指向正确开关。
func TestScheduleTravelActivityInvalidHoursRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"activity_hours":[-1]}}`, "activity_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleTravelActivityExplicitHours 显式配置旅行/活跃小时。
func TestScheduleTravelActivityExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_hours":[9,21],"activity_hours":[11]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 11 {
		t.Errorf("activity_hours=%v want [11]", c.Schedule.ActivityHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null,"travel_hours":null,"activity_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[],"travel_hours":[],"activity_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
				t.Errorf("travel_hours=%v want default [9 21]", c.Schedule.TravelHours)
			}
			if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
				t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
			if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

func TestWriteDefault(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "sub", "config.json") // 顺带验证父目录自动创建
	key, err := WriteDefault(fp)
	if err != nil {
		t.Fatal(err)
	}
	// key 形如 sk-<24字符随机串>，两次生成不重复
	if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
		t.Errorf("key=%q want sk-<random>", key)
	}
	if key2, _ := WriteDefault(filepath.Join(dir, "another.json")); key2 == key {
		t.Errorf("two generated keys identical: %q", key)
	}
	// 落盘文件可被 Load 正常加载，推荐值齐备且 api_key 生效
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if c.APIKey != key {
		t.Errorf("api_key=%q want %q", c.APIKey, key)
	}
	if c.Listen != ":7863" || c.AuthDir != "./auths" || c.StateFile != "./data/state.json" {
		t.Errorf("generated defaults off: %+v", c)
	}
	if len(c.Schedule.CheckinHours) == 0 || !c.Schedule.CheckinEnabled {
		t.Errorf("generated schedule off: %+v", c.Schedule)
	}
	// 已存在的文件不覆盖：二次写入同一路径必须报错
	if _, err := WriteDefault(fp); err == nil {
		t.Error("WriteDefault must refuse to overwrite existing file")
	}
}

func TestBalanceRefreshDefaults(t *testing.T) {
	// 缺省：启用 + 30 分钟
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.BalanceRefreshEnabled || c.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("default balance refresh: enabled=%v interval=%v", c.Schedule.BalanceRefreshEnabled, c.BalanceRefreshInterval)
	}
	// 显式配置 10 分钟
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":10}}`), 0o600)
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.BalanceRefreshInterval != 10*time.Minute {
		t.Errorf("interval=%v want 10m", c2.BalanceRefreshInterval)
	}
	// 显式关闭：interval 归零（不启动）
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_enabled":false}}`), 0o600)
	c3, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c3.BalanceRefreshInterval != 0 {
		t.Errorf("disabled interval=%v want 0", c3.BalanceRefreshInterval)
	}
	// 启用但 minutes<=0 → 回落默认 30
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":-5}}`), 0o600)
	c4, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c4.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("fallback interval=%v want 30m", c4.BalanceRefreshInterval)
	}
}

// TestMaxBodyDefault 默认 max_body_mb=8。
func TestMaxBodyDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Server.MaxBodyMB != 8 {
		t.Errorf("max_body_mb=%d want 8", c.Server.MaxBodyMB)
	}
}

// TestMaxBodyExplicit 显式设置 max_body_mb。
func TestMaxBodyExplicit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"server":{"max_body_mb":16}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxBodyMB != 16 {
		t.Errorf("max_body_mb=%d want 16", c.Server.MaxBodyMB)
	}
}

// TestMaxBodyInvalid 非法值（0/负数）normalize 报错：0 想表达"不限"会被静默当成 8MB，
// 与其误导不如 fail fast 提示显式配大上限。
func TestMaxBodyInvalid(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"server":{"max_body_mb":`+v+`}}`), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for max_body_mb=%s", v)
		}
		if !strings.Contains(err.Error(), "server.max_body_mb") {
			t.Errorf("error should name config key server.max_body_mb: %v", err)
		}
	}
}

// TestMaxRotateDefault 默认 max_rotate=3（与 handler 侧 NewHandler 兜底口径一致，
// 零行为变更：暴露前的写死值就是 3）。
func TestMaxRotateDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Server.MaxRotate != 3 {
		t.Errorf("max_rotate=%d want 3", c.Server.MaxRotate)
	}
}

// TestMaxRotateExplicit 文件覆盖 max_rotate（池内账号多时调大）。
func TestMaxRotateExplicit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"server":{"max_rotate":8}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxRotate != 8 {
		t.Errorf("max_rotate=%d want 8", c.Server.MaxRotate)
	}
}

// TestMaxRotateInvalidFallsBack 非法值（0/负数）回落默认 3 而非报错——与
// max_body_mb 的 fail fast 相反，走 pool.max_in_flight_global 的「非正回落」先例：
// max_rotate 的 0 没有"不限"之类的合理语义，无从误导用户，报错只会让手写配置起不来。
func TestMaxRotateInvalidFallsBack(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"server":{"max_rotate":`+v+`}}`), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("max_rotate=%s must fall back, not error: %v", v, err)
		}
		if c.Server.MaxRotate != 3 {
			t.Errorf("max_rotate=%s normalize 后=%d want 3（回落默认）", v, c.Server.MaxRotate)
		}
	}
	// 键缺席同样保持默认 3（零行为变更的回归锁定）。
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxRotate != 3 {
		t.Errorf("max_rotate 键缺席=%d want 3", c.Server.MaxRotate)
	}
}

// TestMaxRotateJSONKeyRoundTrip JSON 键名双向核对：面板 GET 回显（结构体序列化）
// 与 POST 保存（反序列化）必须用同一个键名——两边漂移时表单值读不到也存不进。
// 键名字面量用 hex 构造（显示的字符串未必等于真实字节，字面量断言可能假绿）。
func TestMaxRotateJSONKeyRoundTrip(t *testing.T) {
	key := string([]byte{0x6d, 0x61, 0x78, 0x5f, 0x72, 0x6f, 0x74, 0x61, 0x74, 0x65}) // max_rotate
	if key != "max_rotate" {
		t.Fatalf("hex decode mismatch: %q", key)
	}
	// 入站：用 hex 键走「面板保存」的解析路径。
	c, err := ParseConfig([]byte(`{"server":{"` + key + `":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxRotate != 7 {
		t.Errorf("hex-key max_rotate=%d want 7", c.Server.MaxRotate)
	}
	// 出站：序列化出来的键名必须逐字节等于 max_rotate（面板回显依赖它）。
	out, err := json.Marshal(c.Server)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m[key]; !ok {
		t.Errorf("server 序列化缺 key %q: %s", key, out)
	}
}

// TestMaxInFlightGlobalNormalize max_in_flight_global 的 normalize 语义：
// 0/负数视为「未设置」回落默认 2（WAF 403 修复 P1-1）。与 max_in_flight 的
// 0=不限不同——分档键的 0 没有合理语义，回退分档默认最稳。
func TestMaxInFlightGlobalNormalize(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		c := Default()
		c.Pool.MaxInFlightGlobal = 0
		if v == "-1" {
			c.Pool.MaxInFlightGlobal = -1
		}
		if err := c.normalize(); err != nil {
			t.Fatalf("normalize(%s): %v", v, err)
		}
		if c.Pool.MaxInFlightGlobal != 2 {
			t.Errorf("max_in_flight_global=%s normalize 后=%d want 2（回落默认）", v, c.Pool.MaxInFlightGlobal)
		}
	}
	// 显式正值保持不动（分档可调）。
	c := Default()
	c.Pool.MaxInFlightGlobal = 5
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlightGlobal != 5 {
		t.Errorf("max_in_flight_global=5 normalize 后=%d want 5", c.Pool.MaxInFlightGlobal)
	}
}

// TestMaxBodyEnvOverride env WB2A_MAX_BODY_MB 非空覆盖 JSON 值。
func TestMaxBodyEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"server":{"max_body_mb":4}}`), 0o600)
	t.Setenv("WB2A_MAX_BODY_MB", "12")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxBodyMB != 12 {
		t.Errorf("max_body_mb=%d want env 12", c.Server.MaxBodyMB)
	}
}

// TestPromptDefaultPassthrough 默认 prompt.mode=passthrough（对齐上游：透传客户端
// 原始 system 是更保守的缺省）；custom 由用户显式选择，此时 PromptText 为内置默认（非空）。
func TestPromptDefaultPassthrough(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("prompt.mode=%q want passthrough", c.Prompt.Mode)
	}
	// passthrough 不加载提示词文本（透传客户端 system）；切 custom 时 normalize 会加载。
}

// TestPromptExplicitPassthrough passthrough 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitPassthrough(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough should not load PromptText, got len=%d", len(c.PromptText))
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	// 用 json.Marshal 拼路径：Windows 反斜杠必须转义，手工字符串拼接会产出非法 JSON。
	cfgJSON, err := json.Marshal(map[string]any{"prompt": map[string]any{"mode": "custom", "file": pf}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cf, cfgJSON, 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "passthrough")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
}

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 缺省 passthrough。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("legacy config should default to passthrough, got %q", c.Prompt.Mode)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q", c.Listen)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
func TestUpstreamUserAgentConfig(t *testing.T) {
	// JSON 配置
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"user_agent":"WorkBuddy/1.2.3"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", c.Upstream.UserAgent)
	}
	// 缺省为空
	if c2, err := Load(""); err != nil || c2.Upstream.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty (err=%v)", c2.Upstream.UserAgent, err)
	}
	// env 覆盖
	t.Setenv("WB2A_USER_AGENT", "EnvAgent/9")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.UserAgent != "EnvAgent/9" {
		t.Errorf("env user_agent=%q want EnvAgent/9", c3.Upstream.UserAgent)
	}
}

// TestLoadConfigPathIsDirectory config 路径是目录时给出可操作提示（Docker bind mount 陷阱）。
// 复现：compose 挂载 ./config.json 但宿主机缺该文件 → Docker 创建同名目录 → 启动失败。
// 旧行为只报 "read config: ... Incorrect function" 之类晦涩错误，无从排查。
func TestLoadConfigPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Load(asDir)
	if err == nil {
		t.Fatal("want error when config path is a directory")
	}
	msg := err.Error()
	if !strings.Contains(msg, "是目录") {
		t.Errorf("error should explain it is a directory: %v", err)
	}
	if !strings.Contains(msg, "config.example.json") {
		t.Errorf("error should suggest the fix (cp config.example.json): %v", err)
	}
}

// TestZeroWidthSanitizeDefaultOffAndEnvOverride 零宽脱敏的默认值与开关路径。
//
// 默认必须是关：这个功能改动的是"看不见的字节"，出问题时表现为两个看起来一样的
// 字符串对不上，排查成本高，所以由使用者显式开启（与上游 codebuddy2api 口径一致）。
func TestZeroWidthSanitizeDefaultOffAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	key, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if key == "" {
		t.Error("应生成随机 api_key")
	}
	// 生成的配置文件必须显式含该键且为 false —— 让用户能看见并直接改。
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
	v, ok := gen.Features["zerowidth_sanitize"]
	if !ok {
		t.Errorf("生成的配置缺少 features.zerowidth_sanitize 键：%v", gen.Features)
	} else if v != false {
		t.Errorf("zerowidth_sanitize 默认应为 false，实际 %v", v)
	}

	// 不设环境变量 → 关闭。
	if c, err := Load(path); err != nil {
		t.Fatal(err)
	} else if c.Features.ZeroWidthSanitize {
		t.Error("缺省应为关闭")
	}

	// 环境变量打开。注意用 ParseBool 口径：只有 true/false/1/0 等合法值生效。
	t.Setenv("WB2A_ZEROWIDTH_SANITIZE", "true")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Features.ZeroWidthSanitize {
		t.Error("WB2A_ZEROWIDTH_SANITIZE=true 应打开零宽脱敏")
	}

	// 与指纹脱敏是两个独立开关：开零宽不得连带改动另一个。
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("零宽开关不应影响 sanitize_blacklist_fingerprints（默认仍应为 true）")
	}
}

// TestRestartRequiredFieldsSessionTTLHotApplied 面板「需重启」清单的边界：
// session_sticky.ttl 已改为热生效（Router.SetTTL）→ 不得再出现在清单里，
// 否则面板会提示用户"改了要重启"，而实际已即时生效（误导性提示）。
// gc_interval 反向：GC ticker 不热重建 → 必须仍在清单里。
func TestRestartRequiredFieldsSessionTTLHotApplied(t *testing.T) {
	got := restartRequiredFields(Default())
	var hasTTL, hasGC bool
	for _, f := range got {
		switch f {
		case "session_sticky.ttl":
			hasTTL = true
		case "session_sticky.gc_interval":
			hasGC = true
		}
	}
	if hasTTL {
		t.Errorf("session_sticky.ttl 已热生效，不应在需重启清单里: %v", got)
	}
	if !hasGC {
		t.Errorf("session_sticky.gc_interval 仍应需重启，清单里缺失: %v", got)
	}
}

// TestMachineIDHeadersDefaultOnAndEnvOverride 账号级设备指纹头的配置语义：
// 缺省 true（键缺席零影响，与上游三仓默认一致），显式 false 才关，
// WB2A_MACHINE_ID_HEADERS 可覆盖。config.example.json 必须含该键（面板/文档同一来源）。
func TestMachineIDHeadersDefaultOnAndEnvOverride(t *testing.T) {
	if !Default().Upstream.MachineIDHeaders {
		t.Error("Default() 的 upstream.machine_id_headers 必须缺省 true")
	}
	// 键缺席（老配置）→ 保持 true：Load 先取 Default 再 Unmarshal 覆盖。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"listen":":9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Upstream.MachineIDHeaders {
		t.Error("配置文件缺 machine_id_headers 键时应保持缺省 true（老配置零影响）")
	}
	// 显式 false → 关。
	if err := os.WriteFile(fp, []byte(`{"upstream":{"machine_id_headers":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.MachineIDHeaders {
		t.Error("显式 machine_id_headers=false 应关闭")
	}
	// env 覆盖。
	t.Setenv("WB2A_MACHINE_ID_HEADERS", "false")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.MachineIDHeaders {
		t.Error("WB2A_MACHINE_ID_HEADERS=false 应关闭")
	}
	t.Setenv("WB2A_MACHINE_ID_HEADERS", "true")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Upstream.MachineIDHeaders {
		t.Error("WB2A_MACHINE_ID_HEADERS=true 应打开（覆盖文件里的 false）")
	}
}

// TestMachineIDHeadersInExampleConfig config.example.json 是配置项最完整参考
// （README 明示）——新增键必须同步落进去，否则跟随示例的用户拿着"最全样例"却看不到该开关。
func TestMachineIDHeadersInExampleConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Upstream map[string]any `json:"upstream"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json 非法 JSON: %v", err)
	}
	v, ok := doc.Upstream["machine_id_headers"]
	if !ok {
		t.Fatal("config.example.json upstream 段缺 machine_id_headers 键")
	}
	if b, isBool := v.(bool); !isBool || !b {
		t.Errorf("config.example.json 的 machine_id_headers = %v，want true（与 Default() 一致）", v)
	}
	// 示例文件必须能被本进程解析（新键不引入校验失败）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp); err != nil {
		t.Errorf("config.example.json 应能被 Load 解析: %v", err)
	}
}

// TestUpstreamConnLayerDefaults 连接层四项缺省：h2 **启用**（DisableHTTP2 零值
// false）+ 30/30/90。h2 默认启用是本任务的核心约定（CN 上游走 TUN 代理实测：
// 允许 h2 复用 90%，禁 h2 复用率 0%）——这条锁住默认值不被回退成 HTTP/1.1。
func TestUpstreamConnLayerDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.DisableHTTP2 {
		t.Error("disable_http2 缺省必须为 false（= 启用 h2）")
	}
	if c.Upstream.TLSHandshakeTimeoutSeconds != 30 {
		t.Errorf("tls_handshake_timeout_seconds=%d want 30", c.Upstream.TLSHandshakeTimeoutSeconds)
	}
	if c.Upstream.DialTimeoutSeconds != 30 {
		t.Errorf("dial_timeout_seconds=%d want 30", c.Upstream.DialTimeoutSeconds)
	}
	if c.Upstream.IdleConnTimeoutSeconds != 90 {
		t.Errorf("idle_conn_timeout_seconds=%d want 90（30s 太激进，v1.9.6 的 90s 实测顺滑）", c.Upstream.IdleConnTimeoutSeconds)
	}
}

// TestUpstreamConnLayerJSONRoundTrip JSON 键名往返：四个键按任务书约定的
// snake_case 落盘，且 Default() 序列化后含这四个键（面板/样例文件同口径）。

// TestUpstreamConnLayerJSONRoundTrip JSON 键名往返：四个键按任务书约定的
// snake_case 落盘，且 Default() 序列化后含这四个键（面板/样例文件同口径）。
func TestUpstreamConnLayerJSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"disable_http2"`, `"tls_handshake_timeout_seconds"`,
		`"dial_timeout_seconds"`, `"idle_conn_timeout_seconds"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("Default() 序列化缺 JSON 键 %s", key)
		}
	}
}

// TestUpstreamConnLayerFileOverride 文件覆盖：四个键各自独立生效（含 h2 显式关闭）。

// TestUpstreamConnLayerFileOverride 文件覆盖：四个键各自独立生效（含 h2 显式关闭）。
func TestUpstreamConnLayerFileOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"disable_http2":true,"tls_handshake_timeout_seconds":15,`+
		`"dial_timeout_seconds":7,"idle_conn_timeout_seconds":45}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Upstream.DisableHTTP2 {
		t.Error("disable_http2=true 应生效（显式关闭 h2）")
	}
	if c.Upstream.TLSHandshakeTimeoutSeconds != 15 {
		t.Errorf("tls_handshake_timeout_seconds=%d want 15", c.Upstream.TLSHandshakeTimeoutSeconds)
	}
	if c.Upstream.DialTimeoutSeconds != 7 {
		t.Errorf("dial_timeout_seconds=%d want 7", c.Upstream.DialTimeoutSeconds)
	}
	if c.Upstream.IdleConnTimeoutSeconds != 45 {
		t.Errorf("idle_conn_timeout_seconds=%d want 45", c.Upstream.IdleConnTimeoutSeconds)
	}
}

// TestUpstreamConnLayerInvalidFallsBack 非法值回落：三个超时 0 / 负数一律回落
// 推荐值（30/30/90），不报错——与 max_rotate / max_in_flight_global 同风格，
// 「0 没有合理语义」时回落默认而非让部署起不来。显式 0 的 disable_http2 无回落
// 语义（bool false 就是「启用 h2」）。

// TestUpstreamConnLayerInvalidFallsBack 非法值回落：三个超时 0 / 负数一律回落
// 推荐值（30/30/90），不报错——与 max_rotate / max_in_flight_global 同风格，
// 「0 没有合理语义」时回落默认而非让部署起不来。显式 0 的 disable_http2 无回落
// 语义（bool false 就是「启用 h2」）。
func TestUpstreamConnLayerInvalidFallsBack(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"disable_http2":false,"tls_handshake_timeout_seconds":0,`+
		`"dial_timeout_seconds":-5,"idle_conn_timeout_seconds":-1}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("非法值应回落而非报错: %v", err)
	}
	if c.Upstream.DisableHTTP2 {
		t.Error("disable_http2=false 应保持 false（启用 h2）")
	}
	if c.Upstream.TLSHandshakeTimeoutSeconds != 30 {
		t.Errorf("tls_handshake_timeout_seconds=%d want 回落 30", c.Upstream.TLSHandshakeTimeoutSeconds)
	}
	if c.Upstream.DialTimeoutSeconds != 30 {
		t.Errorf("dial_timeout_seconds=%d want 回落 30", c.Upstream.DialTimeoutSeconds)
	}
	if c.Upstream.IdleConnTimeoutSeconds != 90 {
		t.Errorf("idle_conn_timeout_seconds=%d want 回落 90", c.Upstream.IdleConnTimeoutSeconds)
	}
}

// TestUpstreamConnLayerEnvOverride env 覆盖（与 JSON/面板同口径）。

// TestUpstreamConnLayerEnvOverride env 覆盖（与 JSON/面板同口径）。
func TestUpstreamConnLayerEnvOverride(t *testing.T) {
	t.Setenv("WB2A_DISABLE_HTTP2", "true")
	t.Setenv("WB2A_TLS_HANDSHAKE_TIMEOUT_SECONDS", "25")
	t.Setenv("WB2A_DIAL_TIMEOUT_SECONDS", "26")
	t.Setenv("WB2A_IDLE_CONN_TIMEOUT_SECONDS", "27")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Upstream.DisableHTTP2 {
		t.Error("WB2A_DISABLE_HTTP2=true 应生效")
	}
	if c.Upstream.TLSHandshakeTimeoutSeconds != 25 || c.Upstream.DialTimeoutSeconds != 26 || c.Upstream.IdleConnTimeoutSeconds != 27 {
		t.Errorf("env 覆盖失败: %+v", c.Upstream)
	}
}

// TestUpstreamConnLayerEnvInvalidIgnored env 非法值忽略（保持文件/默认值），
// 与既有 WB2A_PASSTHROUGH_IP 的 ParseBool 口径一致。

// TestUpstreamConnLayerEnvInvalidIgnored env 非法值忽略（保持文件/默认值），
// 与既有 WB2A_PASSTHROUGH_IP 的 ParseBool 口径一致。
func TestUpstreamConnLayerEnvInvalidIgnored(t *testing.T) {
	t.Setenv("WB2A_DISABLE_HTTP2", "notabool")
	t.Setenv("WB2A_TLS_HANDSHAKE_TIMEOUT_SECONDS", "abc")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.DisableHTTP2 {
		t.Error("非法 bool 应忽略（保持默认 false = 启用 h2）")
	}
	if c.Upstream.TLSHandshakeTimeoutSeconds != 30 {
		t.Errorf("非法 int 应忽略（保持默认 30），got %d", c.Upstream.TLSHandshakeTimeoutSeconds)
	}
}

// TestRestartRequiredFieldsConnLayer 连接层四项必须在「需重启」清单里：
// Transport 是装配期对象（HTTP/ChatHTTP 共享），运行期重建会换掉在途请求脚下的
// Transport，刻意不做热改。清单缺项 → 面板提示"已立即生效"，而实际要重启才生效。

// TestRestartRequiredFieldsConnLayer 连接层四项必须在「需重启」清单里：
// Transport 是装配期对象（HTTP/ChatHTTP 共享），运行期重建会换掉在途请求脚下的
// Transport，刻意不做热改。清单缺项 → 面板提示"已立即生效"，而实际要重启才生效。
func TestRestartRequiredFieldsConnLayer(t *testing.T) {
	got := restartRequiredFields(Default())
	want := []string{
		"upstream.disable_http2",
		"upstream.tls_handshake_timeout_seconds",
		"upstream.dial_timeout_seconds",
		"upstream.idle_conn_timeout_seconds",
	}
	set := map[string]bool{}
	for _, f := range got {
		set[f] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("需重启清单缺 %s: %v", w, got)
		}
	}
}

// TestAuthWatchDefaults auths 目录热加载的缺省口径：开启 + 30 秒。
// 缺省必须是「开着」——这个特性的存在意义就是让手工上传的账号文件免重启生效，
// 默认关闭等于把它退化成「用户得先知道有这个东西并去打开」。
func TestAuthWatchDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.AuthWatchEnabled {
		t.Error("auth_watch_enabled 缺省应为 true")
	}
	if c.Schedule.AuthWatchSeconds != 30 {
		t.Errorf("auth_watch_seconds 缺省=%d want 30", c.Schedule.AuthWatchSeconds)
	}
	if c.AuthWatchInterval != 30*time.Second {
		t.Errorf("AuthWatchInterval=%v want 30s", c.AuthWatchInterval)
	}
}

// TestAuthWatchNormalize 三种配置形态：显式改间隔 / 显式关闭 / <=0 回落 30。
// 回落口径与 balance_refresh 一致：0 是「开关关闭」的哨兵值，故 <=0 先回落再判
// （否则「配了 0 秒」会被当成关闭开关，而不是回落默认）。

// TestAuthWatchNormalize 三种配置形态：显式改间隔 / 显式关闭 / <=0 回落 30。
// 回落口径与 balance_refresh 一致：0 是「开关关闭」的哨兵值，故 <=0 先回落再判
// （否则「配了 0 秒」会被当成关闭开关，而不是回落默认）。
func TestAuthWatchNormalize(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	write := func(body string) *Config {
		t.Helper()
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("Load(%s): %v", body, err)
		}
		return c
	}

	// 显式间隔 120 秒。
	c := write(`{"schedule":{"auth_watch_seconds":120}}`)
	if c.AuthWatchInterval != 2*time.Minute {
		t.Errorf("interval=%v want 2m", c.AuthWatchInterval)
	}
	if !c.Schedule.AuthWatchEnabled {
		t.Error("只配间隔不应关掉开关")
	}

	// 显式关闭：interval 归零（main 侧据此不扫描）。
	c = write(`{"schedule":{"auth_watch_enabled":false}}`)
	if c.AuthWatchInterval != 0 {
		t.Errorf("关闭时 interval=%v want 0", c.AuthWatchInterval)
	}

	// 启用但秒数 <=0 → 回落默认 30。
	c = write(`{"schedule":{"auth_watch_seconds":-5}}`)
	if c.Schedule.AuthWatchSeconds != 30 || c.AuthWatchInterval != 30*time.Second {
		t.Errorf("秒数<=0 应回落 30：sec=%d interval=%v", c.Schedule.AuthWatchSeconds, c.AuthWatchInterval)
	}

	// 关闭且秒数 <=0：不回落（开关关了就没有生效间隔可言）。
	c = write(`{"schedule":{"auth_watch_enabled":false,"auth_watch_seconds":-5}}`)
	if c.AuthWatchInterval != 0 {
		t.Errorf("关闭时不应回落出间隔: %v", c.AuthWatchInterval)
	}
}

// TestAuthWatchNotRestartRequired auth_dir 本身仍是重启项（watcher 在启动时捕获目录），
// 但 interval/开关必须能热改——面板保存后不应把它列进「需重启」提示。

// TestAuthWatchNotRestartRequired auth_dir 本身仍是重启项（watcher 在启动时捕获目录），
// 但 interval/开关必须能热改——面板保存后不应把它列进「需重启」提示。
func TestAuthWatchNotRestartRequired(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, f := range restartRequiredFields(c) {
		if strings.Contains(f, "auth_watch") {
			t.Errorf("auth_watch 可热改，不应出现在重启项清单: %q", f)
		}
	}
	// auth_dir 仍在清单里（目录是装配期捕获的）。
	found := false
	for _, f := range restartRequiredFields(c) {
		if f == "auth_dir" {
			found = true
		}
	}
	if !found {
		t.Error("auth_dir 应仍是重启项（watcher 启动时捕获目录，运行期不跟随热改）")
	}
}

// TestConfigExampleHasAuthWatchKeys config.example.json 必须带上新键：
// 它是用户复制起步的模板（也是「程序自动生成推荐配置」的形状参考），缺键会让新特性
// 在用户眼里不存在——面板能改，但手写配置的人不会知道有这项。

// TestConfigExampleHasAuthWatchKeys config.example.json 必须带上新键：
// 它是用户复制起步的模板（也是「程序自动生成推荐配置」的形状参考），缺键会让新特性
// 在用户眼里不存在——面板能改，但手写配置的人不会知道有这项。
func TestConfigExampleHasAuthWatchKeys(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 模板必须能被启动路径原样解析（含所有新键）。
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("config.example.json 无法解析: %v", err)
	}
	if !c.Schedule.AuthWatchEnabled || c.Schedule.AuthWatchSeconds != 30 {
		t.Errorf("example 的 auth_watch 口径=%v/%d want true/30",
			c.Schedule.AuthWatchEnabled, c.Schedule.AuthWatchSeconds)
	}
}
