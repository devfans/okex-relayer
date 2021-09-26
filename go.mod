module github.com/polynetwork/okex-relayer

go 1.15

require (
	github.com/boltdb/bolt v1.3.1
	github.com/btcsuite/btcd v0.21.0-beta
	github.com/cosmos/cosmos-sdk v0.39.2
	github.com/ethereum/go-ethereum v1.9.25
	github.com/gogo/protobuf v1.3.1
	github.com/okex/exchain-go-sdk v0.18.0
	github.com/ontio/ontology-crypto v1.0.9
	github.com/polynetwork/poly v1.4.0
	github.com/polynetwork/poly-bridge/bridgesdk v0.0.2
	github.com/polynetwork/poly-go-sdk v0.0.0-20210114035303-84e1615f4ad4
	github.com/tendermint/tendermint v0.33.9
)

replace (
	github.com/cosmos/cosmos-sdk => github.com/okex/cosmos-sdk v0.39.2-exchain3
	github.com/ethereum/go-ethereum => github.com/zhiqiangxu/go-ethereum v0.0.0-20210513053854-b16fac27e406
	github.com/polynetwork/poly => github.com/zhiqiangxu/poly v0.0.0-20210512064417-e8c8ac7678d4
	github.com/tendermint/iavl => github.com/okex/iavl v0.14.3-exchain
	github.com/tendermint/tendermint => github.com/okex/tendermint v0.33.9-exchain2
)
