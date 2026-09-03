package btc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
)

// ScanBlock extracts only outputs whose script exactly matches a controlled address.
// ScanBlock 扫描区块中的受控地址输出。
func ScanBlock(block bitcoin.Block, addresses map[string]Address) ([]DepositObservation, error) {
	if block.Height < 0 || len(block.Hash) != 64 {
		return nil, errors.New("Bitcoin 区块元数据无效")
	}
	observations := make([]DepositObservation, 0)
	for _, tx := range block.Tx {
		id := strings.ToLower(strings.TrimSpace(tx.TxID))
		if len(id) != 64 {
			return nil, fmt.Errorf("Bitcoin 交易 txid 无效: %s", id)
		}
		for _, output := range tx.Vout {
			if int64(output.Value) <= 0 {
				continue
			}
			script, err := hex.DecodeString(strings.TrimSpace(output.ScriptPubKey.Hex))
			if err != nil {
				return nil, errors.New("Bitcoin 输出脚本无效")
			}
			for _, address := range addresses {
				if string(script) != string(address.ScriptPubKey) {
					continue
				}
				observations = append(observations, DepositObservation{UserID: address.UserID, AddressID: address.ID, TxID: id, Vout: output.N, BlockHash: strings.ToLower(block.Hash), BlockHeight: block.Height, AmountSats: int64(output.Value), ScriptPubKey: script})
				break
			}
		}
	}
	return observations, nil
}
