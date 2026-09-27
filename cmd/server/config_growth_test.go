package main

import "testing"

// 本文件只覆盖「成长任务自动执行」排程（吸收上游 74c324a）在配置层的语义，
// 独立成文件是为了不与 config_test.go 的既有用例混在一起（该文件另有其他
// 改动在飞，避免改同一文件的冲突面）。
//
// 上游 74c324a 把 growth 的回落误嵌在 blackcat 的 if 块内：
//
//	if len(c.Schedule.BlackcatHours) == 0 {
//	    c.Schedule.BlackcatHours = []int{23}
//	    c.Schedule.GrowthHours = []int{1}   // ← 嵌在这里
//	}
//
// 于是 blackcat_hours 非空（**常态**，缺省就是 [23]）而 growth_hours 缺席时，
// growth 不回落到 [1] → 排程拿零值 hours，nextFire 静默跳过（永不触发）。
// 我们把它拆成独立的 if 块，并由本用例锁死。

// TestNormalizeGrowthHoursIndependentFallback growth_hours 空值必须**独立**回落
// [1]，不受 blackcat_hours 是否为空影响——上游把两者写在一个 if 块里，
// blackcat_hours 非空（缺省形态）时 growth 不回落，每日排程静默失效。
func TestNormalizeGrowthHoursIndependentFallback(t *testing.T) {
	c := Default()
	c.Schedule.BlackcatHours = []int{23} // 显式非空：上游 bug 的触发条件
	c.Schedule.GrowthHours = nil         // 用户配置里没写 growth_hours
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.Schedule.GrowthHours) != 1 || c.Schedule.GrowthHours[0] != 1 {
		t.Errorf("growth_hours 未独立回落：%v（want [1]；否则每日排程拿零值静默不触发）", c.Schedule.GrowthHours)
	}
	// 反向：显式配置的值不得被回落覆盖。
	c2 := Default()
	c2.Schedule.GrowthHours = []int{3, 9}
	if err := c2.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c2.Schedule.GrowthHours) != 2 || c2.Schedule.GrowthHours[0] != 3 || c2.Schedule.GrowthHours[1] != 9 {
		t.Errorf("显式 growth_hours 被覆盖：%v want [3 9]", c2.Schedule.GrowthHours)
	}
}

// TestGrowthEnabledDefaultsTrue 开关缺省 true：键缺席时保持 true（只有显式
// false 才关）。与其余 *_enabled 同口径——若默认 false，升级后老配置静默
// 失去自动执行。
func TestGrowthEnabledDefaultsTrue(t *testing.T) {
	c := Default()
	if !c.Schedule.GrowthEnabled {
		t.Error("Default() 的 growth_enabled 应为 true（缺省开启，仅显式 false 才关）")
	}
	c.Schedule.GrowthEnabled = true
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.GrowthEnabled {
		t.Error("normalize 不得改动 growth_enabled（用户显式 false 也不该被翻回）")
	}
}

// TestValidateGrowthHoursRange growth_hours 的取值校验与其余 *_hours 同口径：
// 启用时越界（<0 / >23）必须报错，而不是让调度器拿到非法时点。
func TestValidateGrowthHoursRange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hours []int
		bad   bool
	}{
		{"合法 [1]", []int{1}, false},
		{"合法多值 [0 23]", []int{0, 23}, false},
		{"越界 [24]", []int{24}, true},
		{"负数 [-1]", []int{-1}, true},
	} {
		c := Default()
		c.Schedule.GrowthEnabled = true
		c.Schedule.GrowthHours = tc.hours
		err := c.validateScheduleHours()
		if tc.bad && err == nil {
			t.Errorf("%s: 越界 growth_hours=%v 应报错", tc.name, tc.hours)
		}
		if !tc.bad && err != nil {
			t.Errorf("%s: 合法 growth_hours=%v 报错: %v", tc.name, tc.hours, err)
		}
	}
	// 关闭时同样校验（与其余 *_hours 的既有口径一致：checkHourRange 不读开关，
	// 开关只进错误文案——越界值一律拦在启动前，不留给调度器）。
	c := Default()
	c.Schedule.GrowthEnabled = false
	c.Schedule.GrowthHours = []int{99}
	if err := c.validateScheduleHours(); err == nil {
		t.Error("越界 growth_hours 一律报错（与既有 *_hours 同口径，不看开关）")
	}
}
