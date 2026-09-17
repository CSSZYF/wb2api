package logfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateASCII ASCII 截断：按字节上限正常切，截断后补 "…" 省略标记。
func TestTruncateASCII(t *testing.T) {
	// Arrange
	s := "abcdefghijklmnopqrstuvwxyz"

	// Act
	got := Truncate(s, 10)

	// Assert
	if got != "abcdefghij…" {
		t.Errorf("Truncate(ascii, 10)=%q want %q", got, "abcdefghij…")
	}
}

// TestTruncateCJKBoundary 中文截断：字节切点落在多字节字符中间时回退到 rune
// 边界，不出乱码（半截 UTF-8 序列）。
func TestTruncateCJKBoundary(t *testing.T) {
	// Arrange
	s := "将在 24 小时后重置限额" // 每个汉字 3 字节

	// Act
	got := Truncate(s, 4) // 切点落在第 2 个汉字（字节 3..6）中间 → 应回退到 3 字节边界

	// Assert
	if got != "将…" {
		t.Errorf("Truncate(cjk, 4)=%q want %q（回退到 rune 边界 + 省略标记）", got, "将…")
	}
	for _, r := range got {
		if r == 0xFFFD { // utf8.RuneError：半截序列解码失败的替换字符
			t.Fatalf("Truncate 输出含乱码替换字符: %q", got)
		}
	}
}

// TestTruncateShorterThanN 短于 n 的输入原样返回（不补不截，无省略标记）。
func TestTruncateShorterThanN(t *testing.T) {
	// Arrange
	s := "短"

	// Act + Assert
	if got := Truncate(s, 100); got != s {
		t.Errorf("Truncate(短, 100)=%q want 原样 %q", got, s)
	}
	if got := Truncate("", 10); got != "" {
		t.Errorf("Truncate(\"\", 10)=%q want 空串", got)
	}
}

// TestTruncateTrimSpace 与旧 client.go 实现口径对齐：先 TrimSpace 再截断。
func TestTruncateTrimSpace(t *testing.T) {
	// Arrange
	s := "  error body  "

	// Act + Assert
	if got := Truncate(s, 20); got != "error body" {
		t.Errorf("Truncate(%q, 20)=%q want %q（TrimSpace 后原样）", s, got, "error body")
	}
}

// TestTruncateExactRuneBoundary 多字节串在 rune 边界内整段保留：切点恰好落在
// rune 边界时按上限整切（同样补省略标记）。
func TestTruncateExactRuneBoundary(t *testing.T) {
	// Arrange
	s := strings.Repeat("中", 5) // 15 字节

	// Act + Assert
	if got := Truncate(s, 9); got != strings.Repeat("中", 3)+"…" {
		t.Errorf("Truncate(中×5, 9)=%q want %q（9=3 个 rune 的字节边界）", got, strings.Repeat("中", 3)+"…")
	}
}

// TestTruncateNonPositiveReturnsEmpty n<=0 的完整契约：负数与 0 都返回空串，
// 绝不 panic（旧实现 s[:负数] 会 slice bounds out of range）。
func TestTruncateNonPositiveReturnsEmpty(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
	}{
		{"negative n on ascii", "hello", -1},
		{"negative n on cjk", "将在 24 小时后重置限额", -5},
		{"negative n on empty", "", -1},
		{"zero n on ascii", "hello", 0},
		{"zero n on cjk", "重置限额", 0},
		{"zero n on empty", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — tc.s / tc.n
			// Act
			got := Truncate(tc.s, tc.n)

			// Assert
			if got != "" {
				t.Errorf("Truncate(%q, %d)=%q want 空串", tc.s, tc.n, got)
			}
		})
	}
}

// TestTruncateNegativeDoesNotPanic 负数 n 显式防 panic 锚（旧实现 len(s) > n 对
// 负数恒成立、内层 for n>0 不进循环体，落到 s[:n] 即 panic）。
func TestTruncateNegativeDoesNotPanic(t *testing.T) {
	for _, n := range []int{-1, -5} {
		// Arrange
		s := "将在 24 小时后重置限额"

		// Act
		got, panicked := func() (out string, panicked bool) {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			out = Truncate(s, n)
			return
		}()

		// Assert
		if panicked {
			t.Errorf("Truncate(s, %d) panicked (slice bounds out of range), want 空串", n)
		}
		if got != "" {
			t.Errorf("Truncate(s, %d)=%q want 空串（n<=0 返回空串）", n, got)
		}
	}
}

// TestTruncateOutputIsValidUTF8 任意切点下输出都必须是合法 UTF-8（本次修复的
// 核心不变量：错误 body 是中文，按字节切会产出半截序列）。
func TestTruncateOutputIsValidUTF8(t *testing.T) {
	// Arrange
	s := "将在 2026-09-11 18:33:27 UTC+8 重置，请稍后再试"

	// Act + Assert：遍历全部 n，逐一断言输出可被 utf8 完整解码。
	for n := -2; n <= len(s)+2; n++ {
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(s, %d)=%q 不是合法 UTF-8", n, got)
		}
	}
}
