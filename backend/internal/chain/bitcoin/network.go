package bitcoin

import (
	"errors"
	"github.com/btcsuite/btcd/chaincfg"
)

// NetworkProfile 描述单次部署选择的 Bitcoin 测试网络。
type NetworkProfile struct {
	Name            string
	RPCChain        string
	DatabaseNetwork string
	Params          *chaincfg.Params
	DefaultRPCPort  string
}

// ResolveNetwork 根据配置名称解析 Signet 或 Testnet4 网络。
func ResolveNetwork(name string) (NetworkProfile, error) {
	switch name {
	case "signet", "":
		return NetworkProfile{Name: "signet", RPCChain: "signet", DatabaseNetwork: "bitcoin-signet", Params: &chaincfg.SigNetParams, DefaultRPCPort: "38332"}, nil
	case "testnet4":
		return NetworkProfile{Name: "testnet4", RPCChain: "testnet4", DatabaseNetwork: "bitcoin-testnet4", Params: &chaincfg.TestNet3Params, DefaultRPCPort: "48332"}, nil
	default:
		return NetworkProfile{}, errors.New("Bitcoin 网络必须是 signet 或 testnet4")
	}
}
