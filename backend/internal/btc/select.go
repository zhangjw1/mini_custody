package btc

import (
	"errors"
	"sort"
)

// SelectUTXOs 确定性选择覆盖目标金额和手续费的最小超额 UTXO 集合。
func SelectUTXOs(items []UTXO, targetSats, feeRateSatVB int64) ([]UTXO, int64, error) {
	if targetSats <= 0 || feeRateSatVB <= 0 {
		return nil, 0, errors.New("BTC 选币参数无效")
	}
	available := append([]UTXO(nil), items...)
	sort.SliceStable(available, func(i, j int) bool {
		if available[i].ValueSats == available[j].ValueSats {
			return available[i].TxID < available[j].TxID
		}
		return available[i].ValueSats > available[j].ValueSats
	})
	var best []UTXO
	var bestExcess int64 = -1
	var search func(int, []UTXO, int64)
	search = func(index int, chosen []UTXO, total int64) {
		fee := feeRateSatVB * int64(10+68*len(chosen)+31)
		if len(chosen) > 0 && total >= targetSats+fee {
			excess := total - targetSats - fee
			if bestExcess < 0 || excess < bestExcess {
				bestExcess = excess
				best = append([]UTXO(nil), chosen...)
			}
			return
		}
		if index >= len(available) || len(chosen) >= 12 {
			return
		}
		for i := index; i < len(available); i++ {
			search(i+1, append(chosen, available[i]), total+available[i].ValueSats)
		}
	}
	search(0, nil, 0)
	if bestExcess < 0 {
		return nil, 0, errors.New("BTC 可用 UTXO 不足")
	}
	fee := feeRateSatVB * int64(10+68*len(best)+31)
	return best, fee, nil
}
