package logfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件所有 uid / 昵称均为虚构构造值，与任何真实账号无关。
//
// TestLabel 覆盖账号标签的五种形态。标签用于请求流水行与调度日志，
// 目的是让人眼直接看出「刚才那个 429 / 6004 是哪个号」，因此昵称必须保留、
// uid8 必须保留（供 grep），两者都不能缺。
func TestLabel(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		nick string
		want string
	}{
		{"昵称 + uid8", "a1b2c3d4-0000-4000-8000-000000000001", "示例昵称甲", "示例昵称甲(a1b2c3d4)"},
		{"昵称为空退回 uid8", "e5f60718-0000-4000-8000-000000000002", "", "e5f60718"},
		{"昵称只有空白视为空", "e5f60718-b", "   ", "e5f60718"},
		{"uid 与昵称皆空", "", "", "-"},
		{"短 uid 不补零", "abc", "sample", "sample(abc)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := Label(tc.uid, tc.nick)

			// Assert
			if got != tc.want {
				t.Errorf("Label(%q, %q) = %q, want %q", tc.uid, tc.nick, got, tc.want)
			}
		})
	}
}

// TestLabelExtraShapes 补充形态：昵称自带括号（不转义，保持与账号表里的写法一致）、
// 昵称首尾空白（TrimSpace 后入标签）、超长昵称（不截断——截断在 Pad 层也只补不截）。
func TestLabelExtraShapes(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		nick string
		want string
	}{
		{"昵称自带括号不转义", "a1b2c3d4", "猫(测试)", "猫(测试)(a1b2c3d4)"},
		{"昵称首尾空白被裁掉", "a1b2c3d4", "  示例昵称甲  ", "示例昵称甲(a1b2c3d4)"},
		{"超长昵称原样保留", "a1b2c3d4", strings.Repeat("猫", 20), strings.Repeat("猫", 20) + "(a1b2c3d4)"},
		{"uid 仅 8 位不截", "12345678", "sample", "sample(12345678)"},
		{"空 uid 有昵称", "", "sample", "sample(-)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := Label(tc.uid, tc.nick)

			// Assert
			if got != tc.want {
				t.Errorf("Label(%q, %q) = %q, want %q", tc.uid, tc.nick, got, tc.want)
			}
		})
	}
}

// TestLabelNeverLeaksFullUID 全量 uid 不得出现在日志标签里（与 server 侧
// logging_test 的 "full uid leaked" 断言同一口径：日志只留 8 位，全量去
// state.json 查）。这是硬契约——日志会进面板环形缓冲、可能被运维贴出来，
// 全量 uid 是账号主键，不该出现在日志里。
func TestLabelNeverLeaksFullUID(t *testing.T) {
	const uid = "a1b2c3d4-0000-4000-8000-000000000001"

	// Act
	got := Label(uid, "示例昵称甲")

	// Assert
	if got != "示例昵称甲(a1b2c3d4)" {
		t.Fatalf("Label = %q", got)
	}
	if strings.Contains(got, uid) {
		t.Fatalf("全量 uid 泄露进标签：%q", got)
	}
	// 无昵称分支同样只留 8 位。
	if bare := Label(uid, ""); strings.Contains(bare, uid) {
		t.Fatalf("无昵称分支泄露全量 uid：%q", bare)
	}
}

// TestDisplayWidth 中文/全角按 2 列算——这是表格能对齐的前提。
// 若错算成 1 列，中文昵称列会比英文昵称列少补一半空格，整张表往上缩。
func TestDisplayWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"空串", "", 0},
		{"纯 ASCII", "sample", 6},
		{"单个汉字", "猫", 2},
		{"纯中文", "示例昵称甲", 10},
		{"中文 + ASCII 括号 uid8", "示例昵称甲(a1b2c3d4)", 20},
		{"中英混合", "a猫b", 4},
		{"全角 ASCII", "ｆｕｌｌ", 8},
		{"全角标点", "（全角括号）", 12},
		{"emoji", "🐱🎉", 4},
		{"控制字符不占位", "a\tb\rc", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := DisplayWidth(tc.in)

			// Assert
			if got != tc.want {
				t.Errorf("DisplayWidth(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestDisplayWidthMixedCJKASCII 混排逐字符累加：中文 2 列 + ASCII 1 列，
// 与「按字节数」（中文 3 字节）明确区分——后者正是错位的根因。
func TestDisplayWidthMixedCJKASCII(t *testing.T) {
	// Arrange：中文 2 字（4 列）+ "(u1)" 4 字符（4 列）= 8 列；按字节数是 6+4=10。
	const s = "猫猫(u1)"

	// Act + Assert
	if got := DisplayWidth(s); got != 8 {
		t.Errorf("DisplayWidth(%q) = %d, want 8（按字节数是 %d，二者必须不同才有意义）", s, got, len(s))
	}
	if len(s) == 8 {
		t.Fatalf("测试前提失效：%q 的字节数恰好等于显示宽", s)
	}
}

// TestDisplayWidthCombiningMarkIsApproximate 组合字符按 rune 逐算，是**已知近似**
// 而非正确值：本仓为保持零第三方依赖（不引 go-runewidth），不做组合字符归一化。
// 昵称里组合字符罕见，这里把行为固定下来，避免后来者误当成 bug 去"修"。
func TestDisplayWidthCombiningMarkIsApproximate(t *testing.T) {
	// Arrange：e + U+0301 组合尖音符，终端显示 1 列，本实现按 2 个 rune 记 2 列。
	const s = "e\u0301"

	// Act + Assert
	if got := DisplayWidth(s); got != 2 {
		t.Errorf("DisplayWidth(%q) = %d, want 2（组合字符不归一化的已知近似）", s, got)
	}
}

// TestPad 只补不截：超宽时原样返回，绝不丢信息（昵称是排查主线索）。
func TestPad(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"ascii 右补空格", "sample", 10, "sample    "},
		{"中文按显示宽补", "猫", 6, "猫    "},
		{"等宽不加空格", "sample", 6, "sample"},
		{"超宽原样返回不截断", "示例昵称甲(a1b2c3d4)", 8, "示例昵称甲(a1b2c3d4)"},
		{"width<=0 原样返回", "abc", 0, "abc"},
		{"负 width 原样返回", "abc", -3, "abc"},
		{"空串补满", "", 3, "   "},
		{"空串 width<=0", "", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := Pad(tc.in, tc.width)

			// Assert
			if got != tc.want {
				t.Errorf("Pad(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
			// 不变量：补完后显示宽至少达到 width（超宽输入除外）。
			if w := DisplayWidth(got); w < tc.width && w < DisplayWidth(tc.in) {
				t.Errorf("Pad(%q, %d) width=%d < %d", tc.in, tc.width, w, tc.width)
			}
			// 不变量：只补不截——原内容必须仍是前缀（信息零丢失）。
			if !strings.HasPrefix(got, tc.in) {
				t.Errorf("Pad(%q, %d) = %q 丢失了原内容前缀", tc.in, tc.width, got)
			}
		})
	}
}

// TestPadKeepsValidUTF8 补空格不碰原字节：输出恒为合法 UTF-8（中文昵称不得被切半）。
func TestPadKeepsValidUTF8(t *testing.T) {
	for _, s := range []string{"猫", "示例昵称甲(a1b2c3d4)", "🐱", ""} {
		for _, w := range []int{-1, 0, 1, 6, 22, 64} {
			got := Pad(s, w)
			if !utf8.ValidString(got) {
				t.Fatalf("Pad(%q, %d) = %q 不是合法 UTF-8", s, w, got)
			}
		}
	}
}

// TestPadAlignsCJKAndASCII 本任务的核心价值：**显示宽相同的两个标签补出的空格数相同**，
// 即中文昵称与等宽 ASCII 昵称在日志里能竖着对齐。按字节数补齐时两者空格数不同
// （中文 3 字节/字 → 少补），整列错位。
func TestPadAlignsCJKAndASCII(t *testing.T) {
	// Arrange：两个标签显示宽都是 10 列（ASCII 10 字符 / 中文 3 字 + "(u1)" 4 字符）。
	ascii := Label("u1", "sample") // sample(u1) → 10 列
	cjk := Label("u1", "猫猫猫")      // 猫猫猫(u1) → 6 + 4 = 10 列
	if DisplayWidth(ascii) != DisplayWidth(cjk) {
		t.Fatalf("测试前提失效：%q=%d 列，%q=%d 列", ascii, DisplayWidth(ascii), cjk, DisplayWidth(cjk))
	}

	// Act
	pa, pc := Pad(ascii, 22), Pad(cjk, 22)

	// Assert：补出的空格数一致（显示宽一致），而字节长度必然不同。
	if DisplayWidth(pa) != DisplayWidth(pc) || DisplayWidth(pa) != 22 {
		t.Errorf("补齐后显示宽不一致：%q=%d vs %q=%d", pa, DisplayWidth(pa), pc, DisplayWidth(pc))
	}
	if len(pa) == len(pc) {
		t.Errorf("测试前提失效：两者字节长度相同（%d）说明没走到中文分支", len(pa))
	}
}
