package amount

import (
	"math/big"
	"testing"
)

// TestParseWeiPreservesLargeIntegerPrecision 验证超大 Wei 整数不会丢失精度。
func TestParseWeiPreservesLargeIntegerPrecision(t *testing.T) {
	want := "999999999999999999999999999999999999999999999999999999999999999999999999999999"
	got, err := ParseWei(want)
	if err != nil {
		t.Fatalf("ParseWei() error = %v", err)
	}
	if got.String() != want {
		t.Fatalf("ParseWei() = %s, want %s", got, want)
	}
}

// TestParseWeiRejectsNonIntegerFormats 验证金额解析拒绝浮点数和科学计数法。
func TestParseWeiRejectsNonIntegerFormats(t *testing.T) {
	for _, value := range []string{"", "-1", "+1", "1.0", "01", "1e18", "NaN"} {
		if _, err := ParseWei(value); err == nil {
			t.Fatalf("ParseWei(%q) error = nil", value)
		}
	}
}

// TestRequirePositive 验证正数金额约束。
func TestRequirePositive(t *testing.T) {
	for _, value := range []*big.Int{nil, big.NewInt(-1), big.NewInt(0)} {
		if err := RequirePositive(value); err == nil {
			t.Fatalf("RequirePositive(%v) error = nil", value)
		}
	}
	if err := RequirePositive(big.NewInt(1)); err != nil {
		t.Fatalf("RequirePositive(1) error = %v", err)
	}
}

// TestParseETHConvertsDecimalExactly 验证 ETH 小数金额可以无精度损失地转换为 Wei。
func TestParseETHConvertsDecimalExactly(t *testing.T) {
	tests := map[string]string{
		"0":                    "0",
		"1":                    "1000000000000000000",
		"0.002":                "2000000000000000",
		"1.000000000000000001": "1000000000000000001",
	}
	for input, want := range tests {
		got, err := ParseETH(input)
		if err != nil {
			t.Fatalf("ParseETH(%q) error = %v", input, err)
		}
		if got.String() != want {
			t.Fatalf("ParseETH(%q) = %s, want %s", input, got, want)
		}
	}
}

// TestParseETHRejectsAmbiguousFormats 验证 ETH 解析拒绝符号、科学计数法和超精度输入。
func TestParseETHRejectsAmbiguousFormats(t *testing.T) {
	for _, input := range []string{"", "-1", "+1", ".1", "1.", "01", "1e-3", "1.0000000000000000001"} {
		if _, err := ParseETH(input); err == nil {
			t.Fatalf("ParseETH(%q) error = nil", input)
		}
	}
}

// TestFormatETHProducesCanonicalDecimal 验证 Wei 格式化不会产生多余小数尾零。
func TestFormatETHProducesCanonicalDecimal(t *testing.T) {
	tests := map[string]string{
		"0":                   "0",
		"2000000000000000":    "0.002",
		"1000000000000000001": "1.000000000000000001",
	}
	for input, want := range tests {
		value, _ := new(big.Int).SetString(input, 10)
		got, err := FormatETH(value)
		if err != nil {
			t.Fatalf("FormatETH(%s) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("FormatETH(%s) = %s, want %s", input, got, want)
		}
	}
}
