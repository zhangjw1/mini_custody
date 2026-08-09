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
