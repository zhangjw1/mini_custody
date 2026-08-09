package amount

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
)

var unsignedDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

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
