package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
)

// scanAsset 从查询结果读取资产主数据模型。
func (s *Store) scanAsset(row rowScanner) (Asset, error) {
	var item Asset
	var contractAddress pgtype.Text
	var decimals int16
	if err := row.Scan(
		&item.ID, &item.Network, &item.AssetType, &item.Symbol, &contractAddress,
		&decimals, &item.Enabled, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return Asset{}, err
	}
	if decimals < 0 || decimals > 255 {
		return Asset{}, errors.New("数据库中的资产精度无效")
	}
	item.ContractAddress = optionalString(contractAddress)
	item.Decimals = uint8(decimals)
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanPlatformWallet 从查询结果读取平台钱包模型。
func (s *Store) scanPlatformWallet(row rowScanner) (PlatformWallet, error) {
	var item PlatformWallet
	var nextNonce string
	if err := row.Scan(
		&item.ID, &item.Network, &item.Role, &item.Address, &item.DerivationPath,
		&nextNonce, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return PlatformWallet{}, err
	}
	nonce, err := parseDatabaseWei(nextNonce, "platform wallet next_nonce")
	if err != nil {
		return PlatformWallet{}, err
	}
	item.NextNonce = nonce
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanTokenDeposit 从查询结果读取 Token 充值模型。
func (s *Store) scanTokenDeposit(row rowScanner) (TokenDeposit, error) {
	var item TokenDeposit
	var amountUnits string
	if err := row.Scan(
		&item.ID, &item.UserID, &item.AddressID, &item.AssetID, &item.TxHash,
		&item.LogIndex, &item.BlockNumber, &item.BlockHash, &item.FromAddress,
		&item.ToAddress, &amountUnits, &item.Confirmations, &item.Status,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return TokenDeposit{}, err
	}
	amount, err := parseDatabaseWei(amountUnits, "token deposit amount_units")
	if err != nil {
		return TokenDeposit{}, err
	}
	item.AmountUnits = amount
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanTokenSweep 从查询结果读取 Token 归集任务模型。
func (s *Store) scanTokenSweep(row rowScanner) (TokenSweep, error) {
	var item TokenSweep
	var recognizedAmount string
	var sweepAmount, nonce, maxFee, maxPriorityFee, actualFee pgtype.Text
	var gasTopupTransferID, gasLimit, blockNumber pgtype.Int8
	var txHash, errorCode, errorMessage pgtype.Text
	if err := row.Scan(
		&item.ID, &item.UserID, &item.AddressID, &item.AssetID, &item.TriggerDepositID,
		&recognizedAmount, &sweepAmount, &gasTopupTransferID, &nonce, &gasLimit,
		&maxFee, &maxPriorityFee, &item.RawTx, &txHash, &blockNumber,
		&item.Confirmations, &actualFee, &item.Status, &errorCode, &errorMessage,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return TokenSweep{}, err
	}
	var err error
	if item.RecognizedAmountUnits, err = parseDatabaseWei(recognizedAmount, "token sweep recognized_amount_units"); err != nil {
		return TokenSweep{}, err
	}
	if item.SweepAmountUnits, err = optionalWei(sweepAmount, "token sweep sweep_amount_units"); err != nil {
		return TokenSweep{}, err
	}
	if item.Nonce, err = optionalWei(nonce, "token sweep nonce"); err != nil {
		return TokenSweep{}, err
	}
	if item.MaxFeePerGasWei, err = optionalWei(maxFee, "token sweep max_fee_per_gas_wei"); err != nil {
		return TokenSweep{}, err
	}
	if item.MaxPriorityFeePerGasWei, err = optionalWei(maxPriorityFee, "token sweep max_priority_fee_per_gas_wei"); err != nil {
		return TokenSweep{}, err
	}
	if item.ActualFeeWei, err = optionalWei(actualFee, "token sweep actual_fee_wei"); err != nil {
		return TokenSweep{}, err
	}
	item.GasTopupTransferID = optionalInt64(gasTopupTransferID)
	item.GasLimit = optionalInt64(gasLimit)
	item.BlockNumber = optionalInt64(blockNumber)
	item.TxHash = optionalString(txHash)
	item.ErrorCode = optionalString(errorCode)
	item.ErrorMessage = optionalString(errorMessage)
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanInternalTransfer 从查询结果读取平台内部转账模型。
func (s *Store) scanInternalTransfer(row rowScanner) (InternalTransfer, error) {
	var item InternalTransfer
	var amountWei string
	var nonce, maxFee, maxPriorityFee, actualFee pgtype.Text
	var gasLimit, blockNumber pgtype.Int8
	var txHash, errorCode, errorMessage pgtype.Text
	if err := row.Scan(
		&item.ID, &item.PlatformWalletID, &item.SweepID, &item.TransferType,
		&item.FromAddress, &item.ToAddress, &amountWei, &nonce, &gasLimit,
		&maxFee, &maxPriorityFee, &item.RawTx, &txHash, &blockNumber,
		&item.Confirmations, &actualFee, &item.Status, &errorCode, &errorMessage,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return InternalTransfer{}, err
	}
	var err error
	if item.AmountWei, err = parseDatabaseWei(amountWei, "internal transfer amount_wei"); err != nil {
		return InternalTransfer{}, err
	}
	if item.Nonce, err = optionalWei(nonce, "internal transfer nonce"); err != nil {
		return InternalTransfer{}, err
	}
	if item.MaxFeePerGasWei, err = optionalWei(maxFee, "internal transfer max_fee_per_gas_wei"); err != nil {
		return InternalTransfer{}, err
	}
	if item.MaxPriorityFeePerGasWei, err = optionalWei(maxPriorityFee, "internal transfer max_priority_fee_per_gas_wei"); err != nil {
		return InternalTransfer{}, err
	}
	if item.ActualFeeWei, err = optionalWei(actualFee, "internal transfer actual_fee_wei"); err != nil {
		return InternalTransfer{}, err
	}
	item.GasLimit = optionalInt64(gasLimit)
	item.BlockNumber = optionalInt64(blockNumber)
	item.TxHash = optionalString(txHash)
	item.ErrorCode = optionalString(errorCode)
	item.ErrorMessage = optionalString(errorMessage)
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanTokenWithdrawal 从查询结果读取 Token 提币任务模型。
func (s *Store) scanTokenWithdrawal(row rowScanner) (TokenWithdrawal, error) {
	var item TokenWithdrawal
	var amountUnits string
	var nonce, maxFee, maxPriorityFee, actualFee pgtype.Text
	var gasLimit, blockNumber pgtype.Int8
	var txHash, errorCode, errorMessage pgtype.Text
	if err := row.Scan(
		&item.ID, &item.IdempotencyKey, &item.UserID, &item.AssetID,
		&item.PlatformWalletID, &item.ToAddress, &amountUnits, &nonce, &gasLimit,
		&maxFee, &maxPriorityFee, &item.RawTx, &txHash, &blockNumber,
		&item.Confirmations, &actualFee, &item.Status, &errorCode, &errorMessage,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return TokenWithdrawal{}, err
	}
	var err error
	if item.AmountUnits, err = parseDatabaseWei(amountUnits, "token withdrawal amount_units"); err != nil {
		return TokenWithdrawal{}, err
	}
	if item.Nonce, err = optionalWei(nonce, "token withdrawal nonce"); err != nil {
		return TokenWithdrawal{}, err
	}
	if item.MaxFeePerGasWei, err = optionalWei(maxFee, "token withdrawal max_fee_per_gas_wei"); err != nil {
		return TokenWithdrawal{}, err
	}
	if item.MaxPriorityFeePerGasWei, err = optionalWei(maxPriorityFee, "token withdrawal max_priority_fee_per_gas_wei"); err != nil {
		return TokenWithdrawal{}, err
	}
	if item.ActualFeeWei, err = optionalWei(actualFee, "token withdrawal actual_fee_wei"); err != nil {
		return TokenWithdrawal{}, err
	}
	item.GasLimit = optionalInt64(gasLimit)
	item.BlockNumber = optionalInt64(blockNumber)
	item.TxHash = optionalString(txHash)
	item.ErrorCode = optionalString(errorCode)
	item.ErrorMessage = optionalString(errorMessage)
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}
