module github.com/polynetwork/okex-relayer

go 1.15

require (
	github.com/boltdb/bolt v1.3.1
	github.com/btcsuite/btcd v0.21.0-beta
	github.com/ethereum/go-ethereum v1.9.25
	github.com/okex/exchain-go-sdk v0.18.1
	github.com/ontio/ontology-crypto v1.0.9
	github.com/polynetwork/poly v1.4.0
	github.com/polynetwork/poly-go-sdk v0.0.0-20200730112529-d9c0c7ddf3d8
	poly_bridge_sdk v0.0.0-00010101000000-000000000000
)

replace (
	github.com/cosmos/cosmos-sdk => github.com/okex/cosmos-sdk v0.39.2-exchain3
	github.com/tendermint/iavl => github.com/okex/iavl v0.14.3-exchain
	github.com/tendermint/tendermint => github.com/okex/tendermint v0.33.9-exchain2
	poly_bridge_sdk => github.com/blockchain-develop/poly_bridge_sdk v0.0.0-20210327080022-0e6eb4b31700
)
