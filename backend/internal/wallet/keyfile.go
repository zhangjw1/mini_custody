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

// GenerateEncryptedRoot 生成新的 BIP-39 助记词及其加密根密钥文件内容。
func GenerateEncryptedRoot(password string) (encrypted []byte, mnemonic string, err error) {
	if password == "" {
		return nil, "", errors.New("必须提供密钥库密码")
	}
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, "", fmt.Errorf("生成钱包熵失败：%w", err)
	}
	mnemonic, err = bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, "", fmt.Errorf("生成钱包助记词失败：%w", err)
	}
	encrypted, err = EncryptMnemonic(mnemonic, password)
	if err != nil {
		return nil, "", err
	}
	return encrypted, mnemonic, nil
}

// EncryptMnemonic 使用 age scrypt 口令加密助记词。
func EncryptMnemonic(mnemonic, password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("必须提供密钥库密码")
	}
	mnemonic = strings.TrimSpace(mnemonic)
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("BIP-39 助记词无效")
	}

	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		return nil, errors.New("初始化密钥库加密失败")
	}

	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		return nil, errors.New("初始化加密密钥库失败")
	}
	payload := rootPayload{Version: keyFileVersion, Mnemonic: mnemonic}
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		return nil, errors.New("编码加密密钥库失败")
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("完成密钥库加密失败")
	}
	return encrypted.Bytes(), nil
}

// DecryptMnemonic 解密并校验根密钥文件中的助记词。
func DecryptMnemonic(encrypted []byte, password string) (string, error) {
	if password == "" {
		return "", errors.New("必须提供密钥库密码")
	}
	identity, err := age.NewScryptIdentity(password)
	if err != nil {
		return "", errors.New("初始化密钥库解密失败")
	}
	reader, err := age.Decrypt(bytes.NewReader(encrypted), identity)
	if err != nil {
		return "", errors.New("解密密钥库失败：密码错误或密钥文件已损坏")
	}

	var payload rootPayload
	decoder := json.NewDecoder(io.LimitReader(reader, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return "", errors.New("解码加密密钥库失败")
	}
	if payload.Version != keyFileVersion {
		return "", fmt.Errorf("不支持的密钥库版本：%d", payload.Version)
	}
	payload.Mnemonic = strings.TrimSpace(payload.Mnemonic)
	if !bip39.IsMnemonicValid(payload.Mnemonic) {
		return "", errors.New("加密密钥库包含无效助记词")
	}
	return payload.Mnemonic, nil
}

// WriteEncryptedRoot 以仅当前用户可读写的权限保存加密根密钥。
func WriteEncryptedRoot(path string, encrypted []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("必须提供密钥库文件路径")
	}
	if len(encrypted) == 0 {
		return errors.New("加密密钥库内容不能为空")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建密钥库目录失败：%w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建密钥库文件失败：%w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(encrypted); err != nil {
		return fmt.Errorf("写入密钥库文件失败：%w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步密钥库文件失败：%w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭密钥库文件失败：%w", err)
	}
	closed = true
	return nil
}

// ReadEncryptedRoot 从磁盘读取加密根密钥内容。
func ReadEncryptedRoot(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取密钥库文件失败：%w", err)
	}
	return data, nil
}

// LoadProvider 读取并解密根密钥文件，然后创建密钥提供器。
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
