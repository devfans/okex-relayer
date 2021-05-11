package config

import (
	"encoding/json"
	"io/ioutil"
)

const (
	ONT_USEFUL_BLOCK_NUM = 1
)

// PolyConfig ...
type PolyConfig struct {
	RestURL                 string
	EntranceContractAddress string
	WalletFile              string
	WalletPwd               string
}

// OKConfig ...
type OKConfig struct {
	SideChainId         uint64
	RestURL             []string
	RestURLTM           []string
	ECCMContractAddress string
	ECCDContractAddress string
	KeyStorePath        string
	KeyStorePwdSet      map[string]string
	BlockConfig         uint64
}

// Config ...
type Config struct {
	PolyConfig   PolyConfig
	OKConfig     OKConfig
	BoltDbPath   string
	BridgeConfig *BridgeConfig
}

// BridgeConfig ...
type BridgeConfig struct {
	RestURL [][]string
}

// LoadConfig ...
func LoadConfig(confFile string) (config *Config, err error) {
	jsonBytes, err := ioutil.ReadFile(confFile)
	if err != nil {
		return
	}

	config = &Config{}
	err = json.Unmarshal(jsonBytes, config)
	return
}
