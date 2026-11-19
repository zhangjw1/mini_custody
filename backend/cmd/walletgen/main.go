package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"os"
)

const passwordEnv = "CUSTODY_KEYSTORE_PASSWORD"

// main 是离线根密钥管理工具入口。
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "密钥工具执行失败：", err)
		os.Exit(1)
	}
}

// run 根据子命令执行根密钥初始化或验证。
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

// runInit 生成并保存新的加密托管根密钥。
func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	output := flags.String("out", "../secrets/custody-root.age", "加密托管根密钥输出路径")
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

	fmt.Println("加密托管根密钥已创建：", *output)
	fmt.Println("根密钥指纹地址：", fingerprint.Hex())
	fmt.Println("警告：请离线记录以下测试网助记词，系统不会再次显示：")
	fmt.Println(mnemonic)
	return nil
}

// runVerify 验证加密根密钥并派生指定索引的地址。
// args 已去掉子命令名，例如完整命令 walletgen verify -file=x -index=0 时，此处为 ["-file=x", "-index=0"]。
func runVerify(args []string) error {
	// NewFlagSet：为子命令单独建一套 flag；ContinueOnError 表示解析失败返回 error，而不是直接 os.Exit
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	// String/Uint 三个参数：name（命令行名）、value（默认值）、usage（-h 帮助文案）
	// 返回值是指针（*string / *uint），必须等下面 Parse 之后再解引用取值
	input := flags.String("file", "../secrets/custody-root.age", "加密托管根密钥路径")
	index := flags.Uint("index", 0, "Ethereum 外部地址索引")
	// Parse 把 args 里的 -file/-index 写入 *input / *index
	if err := flags.Parse(args); err != nil {
		return err
	}
	// *index：解引用指针得到 uint 值
	// ^uint32(0)：按位取反，得到 uint32 最大值（全 1）；再转 uint64 做安全比较，避免强转截断
	if uint64(*index) > uint64(^uint32(0)) {
		return errors.New("地址索引超出 uint32 范围")
	}
	// 密码只从环境变量读取，避免出现在命令行历史中
	password, err := passwordFromEnvironment()
	if err != nil {
		return err
	}
	// *input：解引用得到密钥文件路径字符串
	provider, err := wallet.LoadProvider(*input, password)
	if err != nil {
		return err
	}
	// uint32(*index)：显式类型转换后按 BIP44 用户路径派生地址
	path := wallet.UserPath(uint32(*index))
	address, err := provider.Address(context.Background(), path)
	if err != nil {
		return err
	}
	fmt.Println("密钥库验证成功")
	fmt.Println("派生路径：", path)
	fmt.Println("地址：", address.Hex())
	return nil
}

// passwordFromEnvironment 从环境变量读取密钥库密码。
func passwordFromEnvironment() (string, error) {
	password := os.Getenv(passwordEnv)
	if password == "" {
		return "", fmt.Errorf("必须配置环境变量 %s", passwordEnv)
	}
	return password, nil
}

// usageError 返回密钥工具的中文用法错误。
func usageError() error {
	return errors.New("用法：walletgen <init|verify> [参数]")
}
