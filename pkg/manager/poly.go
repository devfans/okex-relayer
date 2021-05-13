package manager

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"poly_bridge_sdk"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ontio/ontology-crypto/keypair"
	"github.com/ontio/ontology-crypto/signature"
	"github.com/polynetwork/okex-relayer/pkg/log"
	"github.com/polynetwork/poly/common"
	common2 "github.com/polynetwork/poly/native/service/cross_chain_manager/common"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/polynetwork/okex-relayer/config"
	"github.com/polynetwork/okex-relayer/pkg/db"
	"github.com/polynetwork/okex-relayer/pkg/eccd_abi"
	"github.com/polynetwork/okex-relayer/pkg/eccm_abi"
	"github.com/polynetwork/okex-relayer/pkg/tools"
	sdk "github.com/polynetwork/poly-go-sdk"
	vconfig "github.com/polynetwork/poly/consensus/vbft/config"
	polytypes "github.com/polynetwork/poly/core/types"
)

// Poly ...
type Poly struct {
	conf             *config.Config
	polySdk          *sdk.PolySdk
	ethClients       []*ethclient.Client
	bridgeSdk        *poly_bridge_sdk.BridgeFeeCheck
	contractAbi      *abi.ABI
	ks               *tools.EthKeyStore
	db               *db.BoltDB
	syncedPolyHeight uint32
	eccdInstance     *eccd_abi.EthCrossChainData
	accountCh        chan accounts.Account
}

// NewPoly ...
func NewPoly(conf *config.Config, syncedPolyHeight uint32, polySdk *sdk.PolySdk, ethClients []*ethclient.Client, ks *tools.EthKeyStore, db *db.BoltDB, bridgeSdk *poly_bridge_sdk.BridgeFeeCheck) *Poly {
	contractAbi, err := abi.JSON(strings.NewReader(eccm_abi.EthCrossChainManagerABI))
	if err != nil {
		log.Fatalf("abi.JSON failed:%v", err)
	}

	return &Poly{
		conf:             conf,
		syncedPolyHeight: syncedPolyHeight,
		contractAbi:      &contractAbi,
		ks:               ks,
		db:               db,
		polySdk:          polySdk,
		ethClients:       ethClients,
		bridgeSdk:        bridgeSdk,
	}
}

func (p *Poly) findLatestHeight() uint32 {
	for {
		height, err := p.eccdInstance.GetCurEpochStartHeight(nil)
		if err != nil {
			log.Infof("PolyManager findLatestHeight - GetCurEpochStartHeight failed:%v", err)
			time.Sleep(time.Second)
			continue
		}
		return uint32(height)
	}
}

func (p *Poly) init() {
	address := ethcommon.HexToAddress(p.conf.OKConfig.ECCDContractAddress)
	instance, err := eccd_abi.NewEthCrossChainData(address, p.ethClients[0])
	if err != nil {
		log.Fatalf("findLatestHeight - eccd_abi.NewEthCrossChainData: %v", err)
	}
	p.eccdInstance = instance

	accountsAvailable := p.ks.GetAccounts()
	accountCh := make(chan accounts.Account, len(accountsAvailable))

	for i := range accountsAvailable {
		account := accountsAvailable[i]
		accountCh <- account
	}

	p.accountCh = accountCh

	if p.syncedPolyHeight > 0 {
		log.Infof("PolyManager init - start height from flag: %d", p.syncedPolyHeight)
		return
	}

	p.syncedPolyHeight = p.db.GetPolyHeight()
	latestHeight := p.findLatestHeight()
	if latestHeight > p.syncedPolyHeight {
		p.syncedPolyHeight = latestHeight
		log.Infof("PolyManager init - synced height from ECCM: %d", p.syncedPolyHeight)
		return
	}

	log.Infof("PolyManager init - synced height from DB: %d", p.syncedPolyHeight)

}

func (p *Poly) MonitorChain() {
	p.init()

	for {
		latestheight, err := p.polySdk.GetCurrentBlockHeight()
		if err != nil {
			log.Errorf("PolyManager MonitorChain - get chain block height error: %s", err)
			time.Sleep(time.Second)
			continue
		}
		latestheight--

		if latestheight-p.syncedPolyHeight < config.ONT_USEFUL_BLOCK_NUM {
			log.Infof("PolyManager MonitorChain - latestheight(%d) - syncedPolyHeight(%d) < ONT_USEFUL_BLOCK_NUM(%d)", latestheight, p.syncedPolyHeight, config.ONT_USEFUL_BLOCK_NUM)
			time.Sleep(time.Second)
			continue
		}

		log.Infof("PolyManager MonitorChain - latest height: %d, synced height: %d", latestheight, p.syncedPolyHeight)

		for p.syncedPolyHeight <= latestheight-config.ONT_USEFUL_BLOCK_NUM {
			log.Infof("PolyManager MonitorChain handleDepositEvents %d", p.syncedPolyHeight)

			if !p.handleDepositEvents(p.syncedPolyHeight) {
				break
			}

			p.syncedPolyHeight++
			if p.syncedPolyHeight%1000 == 0 {
				break
			}
		}

		if err = p.db.UpdatePolyHeight(p.syncedPolyHeight); err != nil {
			log.Errorf("PolyManager MonitorChain - failed to save height: %v", err)
		}
	}
}

func (this *Poly) handleDepositEvents(height uint32) bool {
	lastEpoch := this.findLatestHeight()
	hdr, err := this.polySdk.GetHeaderByHeight(height + 1)
	if err != nil {
		log.Errorf("handleDepositEvents - GetHeaderByHeight :%d failed, error:%v", height, err)
		return false
	}

	isCurr := lastEpoch <= height
	isEpoch, pubkList, err := this.isEpoch(hdr)
	if err != nil {
		log.Errorf("isEpoch failed: %v", err)
		return false
	}

	var (
		anchor *polytypes.Header
		hp     string
	)
	if !isCurr {
		anchor, _ = this.polySdk.GetHeaderByHeight(lastEpoch + 1)
		proof, _ := this.polySdk.GetMerkleProof(height+1, lastEpoch+1)
		hp = proof.AuditPath
	} else if isEpoch {
		anchor, _ = this.polySdk.GetHeaderByHeight(height + 2)
		proof, _ := this.polySdk.GetMerkleProof(height+1, height+2)
		hp = proof.AuditPath

		if !this.commitHeader(hdr, pubkList) {
			return false
		}
	}

	events, err := this.polySdk.GetSmartContractEventByBlock(height)
	for err != nil {
		log.Errorf("handleDepositEvents - get block event at height:%d error: %v", height, err)
		return false
	}

	for _, event := range events {
		for _, notify := range event.Notify {
			if notify.ContractAddress == this.conf.PolyConfig.EntranceContractAddress {
				states := notify.States.([]interface{})
				method, _ := states[0].(string)
				if method != "makeProof" {
					continue
				}

				if uint64(states[2].(float64)) != this.conf.OKConfig.SideChainId {
					continue
				}

				proof, err := this.polySdk.GetCrossStatesProof(height, states[5].(string))
				if err != nil {
					log.Errorf("handleDepositEvents - failed to get proof for key %s: %v", states[5].(string), err)
					continue
				}

				auditpath, _ := hex.DecodeString(proof.AuditPath)
				value, _, _, _ := tools.ParseAuditpath(auditpath)
				param := &common2.ToMerkleValue{}
				if err := param.Deserialization(common.NewZeroCopySource(value)); err != nil {
					log.Errorf("handleDepositEvents - failed to deserialize MakeTxParam (value: %x, err: %v)", value, err)
					continue
				}

				if !this.isPaid(param) {
					log.Infof("%v skipped because not paid", event.TxHash)
					continue
				}

				log.Infof("%v is paid, start processing", event.TxHash)

				account, ok := <-this.accountCh
				if !ok {
					return false
				}

				go func() {
					defer func() {
						this.accountCh <- account
					}()
					cnt := 0
					for {
						if this.commitDepositEventsWithHeader(account, hdr, param, hp, anchor, event.TxHash, auditpath) {
							break
						} else {
							cnt++
							if cnt > 10 {
								log.Errorf("commitDepositEventsWithHeader - fail too many times, skip (poly_tx_hash:%s)", event.TxHash)
								break
							}
							log.Errorf("commitDepositEventsWithHeader - fail, wait (poly_tx_hash:%s)", event.TxHash)
							time.Sleep(time.Second)
						}
					}
				}()

			}
		}
	}

	return true
}

func (p *Poly) isEpoch(hdr *polytypes.Header) (bool, []byte, error) {
	blkInfo := &vconfig.VbftBlockInfo{}
	if err := json.Unmarshal(hdr.ConsensusPayload, blkInfo); err != nil {
		return false, nil, fmt.Errorf("isEpoch - unmarshal blockInfo error: %s", err)
	}
	if hdr.NextBookkeeper == common.ADDRESS_EMPTY || blkInfo.NewChainConfig == nil {
		return false, nil, nil
	}

	rawKeepers, err := p.eccdInstance.GetCurEpochConPubKeyBytes(nil)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get current epoch keepers: %v", err)
	}

	var bookkeepers []keypair.PublicKey
	for _, peer := range blkInfo.NewChainConfig.Peers {
		keystr, _ := hex.DecodeString(peer.ID)
		key, _ := keypair.DeserializePublicKey(keystr)
		bookkeepers = append(bookkeepers, key)
	}
	bookkeepers = keypair.SortPublicKeys(bookkeepers)
	publickeys := make([]byte, 0)
	sink := common.NewZeroCopySink(nil)
	sink.WriteUint64(uint64(len(bookkeepers)))
	for _, key := range bookkeepers {
		raw := tools.GetNoCompresskey(key)
		publickeys = append(publickeys, raw...)
		sink.WriteVarBytes(crypto.Keccak256(tools.GetEthNoCompressKey(key)[1:])[12:])
	}
	if bytes.Equal(rawKeepers, sink.Bytes()) {
		return false, nil, nil
	}
	return true, publickeys, nil
}

func (p *Poly) isPaid(param *common2.ToMerkleValue) bool {
	return true
}

func randIdx(size int) int {
	return int(rand.Uint32()) % size
}

func (p *Poly) commitHeader(header *polytypes.Header, pubkList []byte) bool {
	account, ok := <-p.accountCh
	if !ok {
		return false
	}

	defer func() {
		p.accountCh <- account
	}()

	headerdata := header.GetMessage()

	var (
		txData []byte
		txErr  error
		sigs   []byte
	)

	ethClient := p.ethClients[randIdx(len(p.ethClients))]
	gasPrice, err := ethClient.SuggestGasPrice(context.Background())
	if err != nil {
		log.Errorf("commitHeader - get suggest sas price failed error: %v", err)
		return false
	}

	for _, sig := range header.SigData {
		temp := make([]byte, len(sig))
		copy(temp, sig)
		newsig, _ := signature.ConvertToEthCompatible(temp)
		sigs = append(sigs, newsig...)
	}

	txData, txErr = p.contractAbi.Pack("changeBookKeeper", headerdata, pubkList, sigs)
	if txErr != nil {
		log.Errorf("commitHeader - contractAbi.Pack err:v", err)
		return false
	}

	contractaddr := ethcommon.HexToAddress(p.conf.OKConfig.ECCMContractAddress)
	callMsg := ethereum.CallMsg{
		From: account.Address, To: &contractaddr, Gas: 0, GasPrice: gasPrice,
		Value: big.NewInt(0), Data: txData,
	}

	gasLimit, err := ethClient.EstimateGas(context.Background(), callMsg)
	if err != nil {
		log.Errorf("commitHeader - estimate gas limit error: %v", err)
		return true
	}

	nonce, err := ethClient.NonceAt(context.Background(), account.Address, nil)
	if err != nil {
		log.Errorf("commitHeader - estimate gas limit error: %v", err)
		return false
	}
	tx := types.NewTransaction(nonce, contractaddr, big.NewInt(0), gasLimit, gasPrice, txData)
	signedtx, err := p.ks.SignTransaction(tx, account)
	if err != nil {
		log.Errorf("commitHeader - sign raw tx error: %v", err)
		return false
	}
	txhash, err := ethClient.SendOKTransaction(context.Background(), signedtx)
	if err != nil {
		log.Errorf("commitHeader - send transaction error:%", err)
		return false
	}

	hash := header.Hash()

	isSuccess := p.waitTransactionConfirm(ethClient, txhash)

	if isSuccess {
		log.Infof("successful to relay poly header to ethereum: (header_hash: %s, height: %d, eth_txhash: %s, nonce: %d)",
			hash.ToHexString(), header.Height, txhash.String(), nonce)
	} else {
		log.Errorf("failed to relay poly header to ethereum: (header_hash: %s, height: %d, eth_txhash: %s, nonce: %d)",
			hash.ToHexString(), header.Height, txhash.String(), nonce)
	}

	return isSuccess
}

func (p *Poly) commitDepositEventsWithHeader(account accounts.Account, header *polytypes.Header, param *common2.ToMerkleValue, headerProof string, anchorHeader *polytypes.Header, polyTxHash string, rawAuditPath []byte) bool {

	ethClient := p.ethClients[randIdx(len(p.ethClients))]

	var (
		sigs       []byte
		headerData []byte
	)
	if anchorHeader != nil && headerProof != "" {
		for _, sig := range anchorHeader.SigData {
			temp := make([]byte, len(sig))
			copy(temp, sig)
			newsig, _ := signature.ConvertToEthCompatible(temp)
			sigs = append(sigs, newsig...)
		}
	} else {
		for _, sig := range header.SigData {
			temp := make([]byte, len(sig))
			copy(temp, sig)
			newsig, _ := signature.ConvertToEthCompatible(temp)
			sigs = append(sigs, newsig...)
		}
	}

	fromTx := [32]byte{}
	copy(fromTx[:], param.TxHash[:32])
	res, _ := p.eccdInstance.CheckIfFromChainTxExist(nil, param.FromChainID, fromTx)
	if res {
		log.Infof("already relayed to ok: ( from_chain_id: %d, from_txhash: %x,  param.Txhash: %x)",
			param.FromChainID, param.TxHash, param.MakeTxParam.TxHash)
		return true
	}

	rawProof, _ := hex.DecodeString(headerProof)
	var rawAnchor []byte
	if anchorHeader != nil {
		rawAnchor = anchorHeader.GetMessage()
	}

	headerData = header.GetMessage()

	txData, err := p.contractAbi.Pack("verifyHeaderAndExecuteTx", rawAuditPath, headerData, rawProof, rawAnchor, sigs)
	if err != nil {
		log.Errorf("commitDepositEventsWithHeader - err:%v", err)
		return false
	}

	gasPrice, err := ethClient.SuggestGasPrice(context.Background())
	if err != nil {
		log.Errorf("commitDepositEventsWithHeader - get suggest sas price failed error: %s", err.Error())
		return false
	}
	contractaddr := ethcommon.HexToAddress(p.conf.OKConfig.ECCMContractAddress)
	callMsg := ethereum.CallMsg{
		From: account.Address, To: &contractaddr, Gas: 0, GasPrice: gasPrice,
		Value: big.NewInt(0), Data: txData,
	}

	gasLimit, err := ethClient.EstimateGas(context.Background(), callMsg)
	if err != nil {
		log.Errorf("commitDepositEventsWithHeader - estimate gas limit error: %s", err.Error())
		return true
	}

	nonce, err := ethClient.NonceAt(context.Background(), account.Address, nil)
	if err != nil {
		log.Errorf("commitHeader - estimate gas limit error: %v", err)
		return false
	}
	tx := types.NewTransaction(nonce, contractaddr, big.NewInt(0), gasLimit, gasPrice, txData)
	signedtx, err := p.ks.SignTransaction(tx, account)
	if err != nil {
		log.Errorf("commitHeader - sign raw tx error: %v", err)
		return false
	}

	timerCtx, cancelFunc := context.WithTimeout(context.Background(), time.Second*20)
	defer cancelFunc()

	if err = ethClient.SendTransaction(timerCtx, signedtx); err != nil {
		log.Errorf("commitHeader - send transaction error:%v", err)
		return false
	}

	hash := header.Hash()
	txhash := signedtx.Hash()

	isSuccess := p.waitTransactionConfirm(ethClient, txhash)

	if isSuccess {
		log.Infof("successful to relay tx to okex: (header_hash: %s, height: %d, poly_hash: %s, eth_txhash: %s, nonce: %d)",
			hash.ToHexString(), header.Height, polyTxHash, txhash.String(), nonce)
	} else {
		log.Errorf("failed to relay tx to okex: (header_hash: %s, height: %d, poly_hash: %s, eth_txhash: %s, nonce: %d)",
			hash.ToHexString(), header.Height, polyTxHash, txhash.String(), nonce)
	}

	return true
}

func (p *Poly) waitTransactionConfirm(ethClient *ethclient.Client, hash ethcommon.Hash) bool {
	start := time.Now()
	for {
		if time.Now().After(start.Add(time.Minute * 3)) {
			return false
		}
		time.Sleep(time.Second * 1)
		_, ispending, err := ethClient.TransactionByHash(context.Background(), hash)
		if err != nil {
			continue
		}
		if ispending == true {
			continue
		} else {
			receipt, err := ethClient.TransactionReceipt(context.Background(), hash)
			if err != nil {
				continue
			}
			return receipt.Status == types.ReceiptStatusSuccessful
		}
	}
}
