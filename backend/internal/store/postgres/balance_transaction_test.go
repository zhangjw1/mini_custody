package postgres

import (
	"math/big"
	"testing"
)

// TestAllowedWithdrawalTransition 验证提币状态机只允许安全迁移。
func TestAllowedWithdrawalTransition(t *testing.T) {
	tests := []struct {
		current string
		target  string
		want    bool
	}{
		{WithdrawalCreated, WithdrawalSigning, true},
		{WithdrawalSigning, WithdrawalSigned, true},
		{WithdrawalSigned, WithdrawalBroadcasting, true},
		{WithdrawalBroadcasting, WithdrawalBroadcastUnknown, true},
		{WithdrawalBroadcastUnknown, WithdrawalBroadcasted, true},
		{WithdrawalBroadcasted, WithdrawalConfirming, true},
		{WithdrawalSigning, WithdrawalFailed, false},
		{WithdrawalCreated, WithdrawalCompleted, false},
		{WithdrawalCompleted, WithdrawalSigning, false},
	}
	for _, tt := range tests {
		if got := allowedWithdrawalTransition(tt.current, tt.target); got != tt.want {
			t.Errorf("allowedWithdrawalTransition(%q, %q) = %v, want %v", tt.current, tt.target, got, tt.want)
		}
	}
}

// TestValidateWithdrawalRequestRejectsInvalidValues 验证提币请求输入约束。
func TestValidateWithdrawalRequestRejectsInvalidValues(t *testing.T) {
	valid := WithdrawalRequest{
		IdempotencyKey: "request-1",
		UserID:         1,
		AddressID:      1,
		ToAddress:      "0x1111111111111111111111111111111111111111",
		AmountWei:      big.NewInt(1),
		ReservedFeeWei: big.NewInt(0),
	}
	if err := validateWithdrawalRequest(valid); err != nil {
		t.Fatalf("validateWithdrawalRequest(valid) error = %v", err)
	}

	tests := []WithdrawalRequest{
		{IdempotencyKey: "", UserID: 1, AddressID: 1, ToAddress: valid.ToAddress, AmountWei: big.NewInt(1), ReservedFeeWei: big.NewInt(0)},
		{IdempotencyKey: "request", UserID: 0, AddressID: 1, ToAddress: valid.ToAddress, AmountWei: big.NewInt(1), ReservedFeeWei: big.NewInt(0)},
		{IdempotencyKey: "request", UserID: 1, AddressID: 1, ToAddress: "invalid", AmountWei: big.NewInt(1), ReservedFeeWei: big.NewInt(0)},
		{IdempotencyKey: "request", UserID: 1, AddressID: 1, ToAddress: valid.ToAddress, AmountWei: big.NewInt(0), ReservedFeeWei: big.NewInt(0)},
		{IdempotencyKey: "request", UserID: 1, AddressID: 1, ToAddress: valid.ToAddress, AmountWei: big.NewInt(1), ReservedFeeWei: big.NewInt(-1)},
	}
	for i, request := range tests {
		if err := validateWithdrawalRequest(request); err == nil {
			t.Errorf("invalid request %d returned nil error", i)
		}
	}
}

// TestValidateDepositObservationRejectsFractionalOrMalformedData 验证充值观察数据约束。
func TestValidateDepositObservationRejectsFractionalOrMalformedData(t *testing.T) {
	validHash := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	valid := DepositObservation{
		UserID:      1,
		AddressID:   1,
		TxHash:      validHash,
		TxIndex:     0,
		BlockNumber: 1,
		BlockHash:   validHash,
		AmountWei:   big.NewInt(1),
	}
	if err := validateDepositObservation(valid); err != nil {
		t.Fatalf("validateDepositObservation(valid) error = %v", err)
	}
	valid.TxHash = "0x1234"
	if err := validateDepositObservation(valid); err == nil {
		t.Fatal("malformed transaction hash returned nil error")
	}
}
