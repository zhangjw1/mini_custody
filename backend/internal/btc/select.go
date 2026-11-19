package btc

import (
	"errors"
	"math"
	"sort"
)

const (
	p2wpkhTxOverheadVBytes int64 = 10
	p2wpkhInputVBytes      int64 = 68
	p2wpkhOutputVBytes     int64 = 31
)

// EstimateVSize 计算 P2WPKH 交易的预估虚拟字节大小。
func EstimateVSize(inputs, outputs int) (int64, error) {
	if inputs <= 0 || outputs <= 0 {
		return 0, errors.New("BTC 交易输入输出数量无效")
	}
	return p2wpkhTxOverheadVBytes + p2wpkhInputVBytes*int64(inputs) + p2wpkhOutputVBytes*int64(outputs), nil
}

// EstimateFee 统一计算指定输入输出数量与费率下的手续费（satoshi）。
func EstimateFee(inputs, outputs int, feeRateSatVB int64) (int64, error) {
	if feeRateSatVB <= 0 {
		return 0, errors.New("BTC 手续费率必须大于零")
	}
	vsize, err := EstimateVSize(inputs, outputs)
	if err != nil {
		return 0, err
	}
	if feeRateSatVB > math.MaxInt64/vsize {
		return 0, errors.New("BTC 提币费率超出范围")
	}
	return vsize * feeRateSatVB, nil
}

// SelectUTXOs 使用统一费用估算按金额降序确定性选择足够支付目标与手续费的 UTXO 集合。
func SelectUTXOs(items []UTXO, targetSats, feeRateSatVB int64) ([]UTXO, int64, error) {
	if targetSats < DustThresholdSats {
		return nil, 0, errors.New("BTC 提币金额低于 dust 下限")
	}
	if feeRateSatVB <= 0 {
		return nil, 0, errors.New("BTC 选币参数无效")
	}
	if len(items) == 0 {
		return nil, 0, errors.New("BTC 可用 UTXO 不足")
	}

	available := append([]UTXO(nil), items...)
	sort.SliceStable(available, func(i, j int) bool {
		if available[i].ValueSats != available[j].ValueSats {
			return available[i].ValueSats > available[j].ValueSats
		}
		if available[i].TxID != available[j].TxID {
			return available[i].TxID < available[j].TxID
		}
		if available[i].Vout != available[j].Vout {
			return available[i].Vout < available[j].Vout
		}
		return available[i].ID < available[j].ID
	})

	// 优先检查单个 UTXO 即可满足目标的最小超额候选，避免不必要的多输入合并。
	singleFeeNoChange, err := EstimateFee(1, 1, feeRateSatVB)
	if err != nil {
		return nil, 0, err
	}
	singleFeeWithChange, err := EstimateFee(1, 2, feeRateSatVB)
	if err != nil {
		return nil, 0, err
	}
	bestSingleIdx := -1
	for i := len(available) - 1; i >= 0; i-- {
		if available[i].ValueSats <= 0 {
			continue
		}
		if available[i].ValueSats >= targetSats+singleFeeNoChange {
			bestSingleIdx = i
			break
		}
	}
	if bestSingleIdx >= 0 {
		chosen := []UTXO{available[bestSingleIdx]}
		fee, _ := resolvedSelectionFee(chosen[0].ValueSats, targetSats, singleFeeNoChange, singleFeeWithChange)
		return chosen, fee, nil
	}

	limit := len(available)
	if limit > MaxSelectionInputs {
		limit = MaxSelectionInputs
	}
	chosen := make([]UTXO, 0, limit)
	var total int64
	for i := 0; i < limit; i++ {
		if available[i].ValueSats <= 0 {
			continue
		}
		if total > math.MaxInt64-available[i].ValueSats {
			return nil, 0, errors.New("BTC UTXO 累加金额超出范围")
		}
		chosen = append(chosen, available[i])
		total += available[i].ValueSats

		feeNoChange, err := EstimateFee(len(chosen), 1, feeRateSatVB)
		if err != nil {
			return nil, 0, err
		}
		if targetSats > math.MaxInt64-feeNoChange {
			return nil, 0, errors.New("BTC 提币金额与手续费超出范围")
		}
		if total >= targetSats+feeNoChange {
			feeWithChange, err := EstimateFee(len(chosen), 2, feeRateSatVB)
			if err != nil {
				return nil, 0, err
			}
			fee, _ := resolvedSelectionFee(total, targetSats, feeNoChange, feeWithChange)
			return chosen, fee, nil
		}
	}
	return nil, 0, errors.New("BTC 可用 UTXO 不足")
}

// resolvedSelectionFee 根据找零是否达到 dust 下限决定按 2 输出还是 1 输出并入剩余金额作为手续费。
func resolvedSelectionFee(total, targetSats, feeNoChange, feeWithChange int64) (fee int64, change int64) {
	if total >= targetSats+feeWithChange+DustThresholdSats {
		return feeWithChange, total - targetSats - feeWithChange
	}
	return total - targetSats, 0
}
