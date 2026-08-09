package wallet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/tyler-smith/go-bip39"
)

const keyFileVersion = 1

type rootPayload struct {
	Version  int    `json:"version"`
	Mnemonic string `json:"mnemonic"`
}

func GenerateEncryptedRoot(password string) (encrypted []byte, mnemonic string, err error) {
	if password == "" {
		return nil, "", errors.New("keystore password is required")
	}
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, "", fmt.Errorf("generate wallet entropy: %w", err)
	}
	mnemonic, err = bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, "", fmt.Errorf("generate wallet mnemonic: %w", err)
	}
	encrypted, err = EncryptMnemonic(mnemonic, password)
	if err != nil {
		return nil, "", err
	}
	return encrypted, mnemonic, nil
}

func EncryptMnemonic(mnemonic, password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("keystore password is required")
	}
	mnemonic = strings.TrimSpace(mnemonic)
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("invalid BIP-39 mnemonic")
	}

	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		return nil, errors.New("initialize keystore encryption")
	}

	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		return nil, errors.New("initialize encrypted keystore")
	}
	payload := rootPayload{Version: keyFileVersion, Mnemonic: mnemonic}
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		return nil, errors.New("encode encrypted keystore")
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("finalize encrypted keystore")
	}
	return encrypted.Bytes(), nil
}

func DecryptMnemonic(encrypted []byte, password string) (string, error) {
	if password == "" {
		return "", errors.New("keystore password is required")
	}
	identity, err := age.NewScryptIdentity(password)
	if err != nil {
		return "", errors.New("initialize keystore decryption")
	}
	reader, err := age.Decrypt(bytes.NewReader(encrypted), identity)
	if err != nil {
		return "", errors.New("decrypt keystore: invalid password or corrupted key file")
	}

	var payload rootPayload
	decoder := json.NewDecoder(io.LimitReader(reader, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return "", errors.New("decode encrypted keystore")
	}
	if payload.Version != keyFileVersion {
		return "", fmt.Errorf("unsupported keystore version %d", payload.Version)
	}
	payload.Mnemonic = strings.TrimSpace(payload.Mnemonic)
	if !bip39.IsMnemonicValid(payload.Mnemonic) {
		return "", errors.New("encrypted keystore contains an invalid mnemonic")
	}
	return payload.Mnemonic, nil
}

func WriteEncryptedRoot(path string, encrypted []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("keystore file path is required")
	}
	if len(encrypted) == 0 {
		return errors.New("encrypted keystore is empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create keystore directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create keystore file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(encrypted); err != nil {
		return fmt.Errorf("write keystore file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync keystore file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close keystore file: %w", err)
	}
	closed = true
	return nil
}

func ReadEncryptedRoot(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keystore file: %w", err)
	}
	return data, nil
}

func LoadProvider(path, password string) (*MnemonicKeyProvider, error) {
	encrypted, err := ReadEncryptedRoot(path)
	if err != nil {
		return nil, err
	}
	mnemonic, err := DecryptMnemonic(encrypted, password)
	if err != nil {
		return nil, err
	}
	return NewMnemonicKeyProvider(mnemonic)
}
