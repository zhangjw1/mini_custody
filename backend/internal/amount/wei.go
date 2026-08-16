package amount

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var unsignedDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var ethDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.([0-9]{1,18}))?$`)

var weiPerETH = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// ParseDecimal 将指定精度的非负十进制金额精确转换为最小单位整数。
func ParseDecimal(value string, decimals uint8) (*big.Int, error) {
	if decimals == 0 || decimals > 18 {
		return nil, errors.New("资产精度必须在 1 到 18 之间")
	}
	value = strings.TrimSpace(value)
	pattern := regexp.MustCompile(fmt.Sprintf(`^(0|[1-9][0-9]*)(?:\.([0-9]{1,%d}))?$`, decimals))
	matches := pattern.FindStringSubmatch(value)
	if matches == nil {
		return nil, fmt.Errorf("金额必须是最多 %d 位小数的非负十进制数", decimals)
	}
	whole, ok := new(big.Int).SetString(matches[1], 10)
	if !ok {
		return nil, errors.New("金额整数部分无效")
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	result := new(big.Int).Mul(whole, scale)
	if matches[2] == "" {
		return result, nil
	}
	fraction := matches[2] + strings.Repeat("0", int(decimals)-len(matches[2]))
	fractionUnits, ok := new(big.Int).SetString(fraction, 10)
	if !ok {
		return nil, errors.New("金额小数部分无效")
	}
	return result.Add(result, fractionUnits), nil
}

// FormatDecimal 将非负最小单位整数精确格式化为不带多余尾零的十进制金额。
func FormatDecimal(value *big.Int, decimals uint8) (string, error) {
	if decimals == 0 || decimals > 18 {
		return "", errors.New("资产精度必须在 1 到 18 之间")
	}
	if err := RequireNonNegative(value); err != nil {
		return "", err
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole := new(big.Int)
	fraction := new(big.Int)
	whole.QuoRem(value, scale, fraction)
	if fraction.Sign() == 0 {
		return whole.String(), nil
	}
	fractionText := fmt.Sprintf("%0*s", int(decimals), fraction.String())
	return whole.String() + "." + strings.TrimRight(fractionText, "0"), nil
}

// ParseWei 将无符号十进制字符串解析为 Wei 整数。
func ParseWei(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	if !unsignedDecimal.MatchString(value) {
		return nil, errors.New("Wei 金额必须是无符号十进制整数")
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil, errors.New("Wei 金额无效")
	}
	return parsed, nil
}

// ParseETH 将最多 18 位小数的 ETH 十进制字符串精确转换为 Wei。
func ParseETH(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	matches := ethDecimal.FindStringSubmatch(value)
	if matches == nil {
		return nil, errors.New("ETH 金额必须是最多 18 位小数的非负十进制数")
	}
	whole, ok := new(big.Int).SetString(matches[1], 10)
	if !ok {
		return nil, errors.New("ETH 整数部分无效")
	}
	result := new(big.Int).Mul(whole, weiPerETH)
	if matches[2] == "" {
		return result, nil
	}
	fraction := matches[2] + strings.Repeat("0", 18-len(matches[2]))
	fractionWei, ok := new(big.Int).SetString(fraction, 10)
	if !ok {
		return nil, errors.New("ETH 小数部分无效")
	}
	return result.Add(result, fractionWei), nil
}

// FormatETH 将非负 Wei 精确格式化为不带多余尾零的 ETH 十进制字符串。
func FormatETH(value *big.Int) (string, error) {
	if err := RequireNonNegative(value); err != nil {
		return "", err
	}
	whole := new(big.Int)
	fraction := new(big.Int)
	whole.QuoRem(value, weiPerETH, fraction)
	if fraction.Sign() == 0 {
		return whole.String(), nil
	}
	fractionText := fmt.Sprintf("%018s", fraction.String())
	return whole.String() + "." + strings.TrimRight(fractionText, "0"), nil
}

// RequirePositive 校验 Wei 金额必须大于零。
func RequirePositive(value *big.Int) error {
	if value == nil || value.Sign() <= 0 {
		return errors.New("Wei 金额必须大于零")
	}
	return nil
}

// RequireNonNegative 校验 Wei 金额不能小于零。
func RequireNonNegative(value *big.Int) error {
	if value == nil || value.Sign() < 0 {
		return errors.New("Wei 金额不能小于零")
	}
	return nil
}

// Clone 复制一个不会与原值共享可变状态的大整数。
func Clone(value *big.Int) *big.Int {
	if value == nil {
		return nil
	}
	return new(big.Int).Set(value)
}
