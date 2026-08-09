package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const passwordEnv = "CUSTODY_KEYSTORE_PASSWORD"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "walletgen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "verify":
		return runVerify(args[1:])
	default:
		return usageError()
	}
}

func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	output := flags.String("out", "../secrets/custody-root.age", "encrypted custody root output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	password, err := passwordFromEnvironment()
	if err != nil {
		return err
	}
	encrypted, mnemonic, err := wallet.GenerateEncryptedRoot(password)
	if err != nil {
		return err
	}
	if err := wallet.WriteEncryptedRoot(*output, encrypted); err != nil {
		return err
	}
	provider, err := wallet.NewMnemonicKeyProvider(mnemonic)
	if err != nil {
		return err
	}
	fingerprint, err := provider.Address(context.Background(), wallet.TreasuryPath)
	if err != nil {
		return err
	}

	fmt.Println("encrypted custody root created:", *output)
	fmt.Println("root fingerprint address:", fingerprint.Hex())
	fmt.Println("WARNING: record this testnet mnemonic offline; it will not be shown again:")
	fmt.Println(mnemonic)
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	input := flags.String("file", "../secrets/custody-root.age", "encrypted custody root path")
	index := flags.Uint("index", 0, "Ethereum external address index")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if uint64(*index) > uint64(^uint32(0)) {
		return errors.New("index exceeds uint32 range")
	}
	password, err := passwordFromEnvironment()
	if err != nil {
		return err
	}
	provider, err := wallet.LoadProvider(*input, password)
	if err != nil {
		return err
	}
	path := wallet.UserPath(uint32(*index))
	address, err := provider.Address(context.Background(), path)
	if err != nil {
		return err
	}
	fmt.Println("keystore verified")
	fmt.Println("derivation path:", path)
	fmt.Println("address:", address.Hex())
	return nil
}

func passwordFromEnvironment() (string, error) {
	password := os.Getenv(passwordEnv)
	if password == "" {
		return "", fmt.Errorf("%s is required", passwordEnv)
	}
	return password, nil
}

func usageError() error {
	return errors.New("usage: walletgen <init|verify> [flags]")
}
