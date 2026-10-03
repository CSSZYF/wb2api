// client_deduction_end_test.go CreditPackages 到期时间的**第四级兜底**
// （DeductionEndTime，吸收上游 PR #93 的字段，但只作兜底）。
//
// 我们的既有实证（sliver 75c15e8 + 本仓 cfa10cf）：上游 get-user-resource 的
// CN/global 两域字段全集**没有** PackageEndTime，真实到期字段是 CycleEndTime
// （墙钟串 packageEndLayout）。PR #93 声称 DeductionEndTime（epoch 毫秒）才是真
// 失效时刻——两者**可能都对**（不同 realm / 时间点），故本仓只把它加为**第四级
// 兜底**：有值才用、缺失回落既有三级，这样即使 PR 的判断在本域不成立也不回归。
//
// 单位差异是关键：CycleEndTime 是墙钟串（"2006-01-02 15:04:05"，UTC+8），
// DeductionEndTime 是 epoch 毫秒——**不能**让解析路径分叉（前端 pkgEndMs 只认墙钟
// 形态），故本仓把 epoch 毫秒格式化成同一墙钟串出站，而不是照搬 PR 的 RFC3339。
package upstream

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestCreditPackagesDeductionEndTimeFallback 第四级兜底：三个墙钟字段全空、
// DeductionEndTime 有值 → 用它的日期，且格式与 CycleEndTime 同口径（墙钟串，
// 前端解析路径不分叉）。
func TestCreditPackagesDeductionEndTimeFallback(t *testing.T) {
	// 2027-03-12 22:03:50 UTC+8 的 epoch 毫秒。
	ms := time.Date(2027, 3, 12, 22, 3, 50, 0, softRateResetLoc).UnixMilli()
	c := resourceStub(`{"PackageName":"p","DeductionEndTime":` +
		itoa64(ms) + `,"CapacitySize":10,"CapacityRemain":5,"CapacityUsed":5}`)
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	if packs[0].EndTime != "2027-03-12 22:03:50" {
		t.Errorf("EndTime=%q want %q（DeductionEndTime 应格式化成与 CycleEndTime 同口径的墙钟串）",
			packs[0].EndTime, "2027-03-12 22:03:50")
	}
}

// TestCreditPackagesCycleEndTimeBeatsDeductionEndTime 既有三级**优先于**第四级：
// CycleEndTime 有值时不得被 DeductionEndTime 覆盖（PR 的判断在本域未必成立，
// 实证过的字段必须优先——这是「只做兜底」的语义本身）。
func TestCreditPackagesCycleEndTimeBeatsDeductionEndTime(t *testing.T) {
	ms := time.Date(2027, 3, 12, 22, 3, 50, 0, softRateResetLoc).UnixMilli()
	c := resourceStub(`{"PackageName":"p","CycleEndTime":"2026-10-18 05:24:02","DeductionEndTime":` +
		itoa64(ms) + `}`)
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(packs) != 1 || packs[0].EndTime != "2026-10-18 05:24:02" {
		t.Fatalf("CycleEndTime 必须优先于第四级兜底，实际 %+v", packs)
	}
}

// TestCreditPackagesDeductionEndTimeMissingNoRegression 缺失（0 / 负数 / 非数）时
// 回落既有三级，行为与加字段前逐字一致——尤其「全缺 → 空串」（前端渲染「-」，
// 不得伪造 1970 或已过期）。
func TestCreditPackagesDeductionEndTimeMissingNoRegression(t *testing.T) {
	cases := []struct {
		name string
		acct string
		want string
	}{
		{"第四级为 0 且三级全缺 → 空串",
			`{"PackageName":"p","DeductionEndTime":0,"CapacitySize":10,"CapacityRemain":5,"CapacityUsed":5}`,
			""},
		{"第四级缺失且 CycleEndTime 有值 → 用 CycleEndTime",
			`{"PackageName":"p","CycleEndTime":"2026-10-18 05:24:02"}`,
			"2026-10-18 05:24:02"},
		{"第四级为负且三级全缺 → 空串",
			`{"PackageName":"p","DeductionEndTime":-1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := resourceStub(tc.acct)
			packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
			if err != nil {
				t.Fatalf("packages: %v", err)
			}
			if len(packs) != 1 {
				t.Fatalf("packs=%d want 1", len(packs))
			}
			if packs[0].EndTime != tc.want {
				t.Errorf("EndTime=%q want %q", packs[0].EndTime, tc.want)
			}
		})
	}
}

// itoa64 十进制定点格式化（避免额外 import strconv 的测试专用小工具）。
func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
