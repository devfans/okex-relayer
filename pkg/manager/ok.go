package manager

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	amcodec "github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	proto "github.com/gogo/protobuf/proto"
	oksdk "github.com/okex/exchain-go-sdk"
	"github.com/polynetwork/okex-relayer/config"
	"github.com/polynetwork/okex-relayer/pkg/db"
	"github.com/polynetwork/okex-relayer/pkg/eccm_abi"
	"github.com/polynetwork/okex-relayer/pkg/log"
	"github.com/polynetwork/okex-relayer/pkg/tools"
	sdk "github.com/polynetwork/poly-go-sdk"
	"github.com/polynetwork/poly/common"
	common2 "github.com/polynetwork/poly/native/service/cross_chain_manager/common"
	"github.com/polynetwork/poly/native/service/cross_chain_manager/eth"
	"github.com/polynetwork/poly/native/service/cross_chain_manager/okex"
	mhcomm "github.com/polynetwork/poly/native/service/header_sync/common"
	"github.com/polynetwork/poly/native/service/header_sync/cosmos"
	okex2 "github.com/polynetwork/poly/native/service/header_sync/okex"
	"github.com/polynetwork/poly/native/service/utils"
	autils "github.com/polynetwork/poly/native/service/utils"
	"github.com/tendermint/tendermint/crypto/merkle"
	"github.com/tendermint/tendermint/types"
)

// OK ...
type OK struct {
	syncedOKHeight int64
	conf           *config.Config
	polySdk        *sdk.PolySdk
	polySigner     *sdk.Account
	ethClients     []*ethclient.Client
	tmClients      []*oksdk.Client
	db             *db.BoltDB
	header4sync    [][]byte
	crosstx4sync   []*CrossTransfer
	cdc            *amcodec.Codec
}

// CrossTransfer ...
type CrossTransfer struct {
	txIndex string
	txId    []byte
	value   []byte
	toChain uint32
	height  uint64
}

func (this *CrossTransfer) Serialization(sink *common.ZeroCopySink) {
	sink.WriteString(this.txIndex)
	sink.WriteVarBytes(this.txId)
	sink.WriteVarBytes(this.value)
	sink.WriteUint32(this.toChain)
	sink.WriteUint64(this.height)
}

func (this *CrossTransfer) Deserialization(source *common.ZeroCopySource) error {
	txIndex, eof := source.NextString()
	if eof {
		return fmt.Errorf("Waiting deserialize txIndex error")
	}
	txId, eof := source.NextVarBytes()
	if eof {
		return fmt.Errorf("Waiting deserialize txId error")
	}
	value, eof := source.NextVarBytes()
	if eof {
		return fmt.Errorf("Waiting deserialize value error")
	}
	toChain, eof := source.NextUint32()
	if eof {
		return fmt.Errorf("Waiting deserialize toChain error")
	}
	height, eof := source.NextUint64()
	if eof {
		return fmt.Errorf("Waiting deserialize height error")
	}
	this.txIndex = txIndex
	this.txId = txId
	this.value = value
	this.toChain = toChain
	this.height = height
	return nil
}

// NewOKEx ...
func NewOKEx(conf *config.Config, syncedOKHeight int64, polySigner *sdk.Account, polySdk *sdk.PolySdk, ethClients []*ethclient.Client, tmClients []*oksdk.Client, db *db.BoltDB) *OK {
	cdc := okex2.NewCDC()

	ok := &OK{
		conf:           conf,
		syncedOKHeight: syncedOKHeight,
		polySdk:        polySdk,
		polySigner:     polySigner,
		ethClients:     ethClients,
		tmClients:      tmClients,
		db:             db,
		cdc:            cdc,
	}

	ok.init()

	return ok
}

func (ok *OK) init() {
	if ok.syncedOKHeight > 0 {
		log.Infof("OKManager init - start height from flag: %d", ok.syncedOKHeight)
		return
	}
	// get latest height
	dbHeight := ok.db.GetOKHeight()
	epochHeight := ok.findLastestEpochHeight()
	if epochHeight > dbHeight {
		ok.syncedOKHeight = epochHeight
		log.Infof("OKManager init - start height from epoch: %d", ok.syncedOKHeight)
	} else {
		ok.syncedOKHeight = dbHeight
		log.Infof("OKManager init - start height from db: %d", ok.syncedOKHeight)
	}

}

func (ok *OK) MonitorChain() {
	for {
		ethClient := ok.ethClients[randIdx(len(ok.ethClients))]
		latestheightU64, err := ethClient.BlockNumber(context.Background())
		if err != nil {
			log.Errorf("OKManager MonitorChain - cannot get node height, err: %v", err)
			time.Sleep(time.Second)
			continue
		}

		latestheightU64--
		latestheight := int64(latestheightU64)
		if ok.syncedOKHeight >= latestheight-ok.conf.OKConfig.BlockConfig {
			log.Infof("OKManager MonitorChain - syncedOKHeight(%d) >= latestheight(%d) - OKConfig.BlockConfig(%d), wait...", ok.syncedOKHeight, latestheight, ok.conf.OKConfig.BlockConfig)
			time.Sleep(time.Second)
			continue
		}

		count := 0
		for ok.syncedOKHeight < latestheight-ok.conf.OKConfig.BlockConfig {
			log.Infof("OKManager MonitorChain handleNewBlock %d", ok.syncedOKHeight+1)
			if !ok.handleNewBlock(ok.syncedOKHeight + 1) {
				break
			}
			atomic.AddInt64(&ok.syncedOKHeight, 1)
			count++
			if count > 100 {
				break
			}
		}

		ok.db.UpdateOKHeight(ok.syncedOKHeight)
	}
}

func (ok *OK) handleNewBlock(height int64) bool {
	ret := ok.handleBlockHeader(height)
	if !ret {
		log.Errorf("handleNewBlock - handleBlockHeader on height :%d failed", height)
		return false
	}

	ret = ok.fetchLockDepositEvents(uint64(height))
	if !ret {
		log.Errorf("handleNewBlock - fetchLockDepositEvents on height :%d failed", height)
		return false
	}

	return true
}

func getValidators(tmClient *oksdk.Client, h int64) ([]*types.Validator, error) {
	vr, err := tmClient.Tendermint().QueryValidatorsResult(h)
	if err != nil {
		log.Errorf("getValidators on height :%d failed:%v", h, err)
		return nil, err
	}

	return vr.Validators, nil
}

func (ok *OK) handleBlockHeader(height int64) bool {
	tmClient := ok.tmClients[randIdx(len(ok.tmClients))]

	cr, err := tmClient.Tendermint().QueryCommitResult(height)
	if err != nil {
		log.Errorf("handleBlockHeader - QueryCommitResult on height :%d failed:%v", height, err)
		return false
	}
	if !bytes.Equal(cr.Header.ValidatorsHash, cr.Header.NextValidatorsHash) {
		vSet, err := getValidators(tmClient, height)
		if err != nil {
			log.Errorf("handleBlockHeader - getValidators on height :%d failed:%v", height, err)
			return false
		}
		hdr := cosmos.CosmosHeader{
			Header:  *cr.Header,
			Commit:  cr.Commit,
			Valsets: vSet,
		}

		raw, err := ok.cdc.MarshalBinaryBare(hdr)
		if err != nil {
			log.Errorf("handleBlockHeader - getValidators on height :%d failed:%v", height, err)
			return false
		}
		txhash, err := ok.polySdk.Native.Hs.SyncBlockHeader(ok.conf.OKConfig.SideChainId, ok.polySigner.Address, [][]byte{raw}, ok.polySigner)
		if err != nil {
			if strings.Contains(err.Error(), "no header you commited is useful") {
				return true
			}
			log.Errorf("handleBlockHeader - SyncBlockHeader on height :%d failed:%v", height, err)
			return false
		}

		if !waitPolyTxConfirm(txhash.ToHexString(), ok.polySdk) {
			log.Errorf("handleBlockHeader - SyncBlockHeader on height :%d txhash :%s failed", height, txhash.ToHexString(), err)
			return false
		}

		log.Infof("handleBlockHeader - synced new block on height %d", height)
	}
	return true
}

func waitPolyTxConfirm(polyTxHash string, polySdk *sdk.PolySdk) bool {
	start := time.Now()
	for {
		if time.Now().After(start.Add(time.Minute * 3)) {
			log.Infof("waiting poly_hash %s false after 3 min", polyTxHash)
			return false
		}
		time.Sleep(time.Second)
		tx, err := polySdk.GetTransaction(polyTxHash)
		if err != nil {
			log.Infof("waiting poly_hash %s", polyTxHash)
			continue
		}
		if tx == nil {
			log.Errorf("poly_hash %s not exists", polyTxHash)
			continue
		}
		break

	}

	return true
}

func (ok *OK) fetchLockDepositEvents(height uint64) bool {
	client := ok.ethClients[randIdx(len(ok.ethClients))]

	lockAddress := ethcommon.HexToAddress(ok.conf.OKConfig.ECCMContractAddress)
	lockContract, err := eccm_abi.NewEthCrossChainManager(lockAddress, client)
	if err != nil {
		log.Errorf("fetchLockDepositEvents NewEthCrossChainManager failed:%v", err)
		return false
	}
	opt := &bind.FilterOpts{
		Start:   height,
		End:     &height,
		Context: context.Background(),
	}
	events, err := lockContract.FilterCrossChainEvent(opt, nil)
	if err != nil {
		log.Errorf("fetchLockDepositEvents - FilterCrossChainEvent error :%v", err)
		return false
	}
	if events == nil {
		log.Infof("fetchLockDepositEvents - no events found on FilterCrossChainEvent")
		return false
	}

	for events.Next() {
		evt := events.Event

		param := &common2.MakeTxParam{}
		_ = param.Deserialization(common.NewZeroCopySource([]byte(evt.Rawdata)))
		raw, _ := ok.polySdk.GetStorage(autils.CrossChainManagerContractAddress.ToHexString(),
			append(append([]byte(common2.DONE_TX), autils.GetUint64Bytes(ok.conf.OKConfig.SideChainId)...), param.CrossChainID...))
		if len(raw) != 0 {
			log.Infof("fetchLockDepositEvents - ccid %s (tx_hash: %s) already on poly",
				hex.EncodeToString(param.CrossChainID), evt.Raw.TxHash.Hex())
			continue
		}

		index := big.NewInt(0)
		index.SetBytes(evt.TxId)
		crossTx := &CrossTransfer{
			txIndex: tools.EncodeBigInt(index),
			txId:    evt.Raw.TxHash.Bytes(),
			toChain: uint32(evt.ToChainId),
			value:   []byte(evt.Rawdata),
			height:  height,
		}
		sink := common.NewZeroCopySink(nil)
		crossTx.Serialization(sink)

		err = ok.db.PutRetry(sink.Bytes())
		if err != nil {
			log.Errorf("fetchLockDepositEvents - ok.db.PutRetry error: %s", err)
		}
		log.Infof("fetchLockDepositEvent -  height: %d", height)
	}
	return true
}

func (ok *OK) findLastestEpochHeight() int64 {
	for {
		val, err := ok.polySdk.GetStorage(utils.HeaderSyncContractAddress.ToHexString(), append([]byte(mhcomm.EPOCH_SWITCH), utils.GetUint64Bytes(ok.conf.OKConfig.SideChainId)...))
		if err != nil {
			log.Errorf("OKManager - findLastestEpochHeight GetStorage fail:%v", err)
			time.Sleep(time.Second)
			continue
		}

		info := &cosmos.CosmosEpochSwitchInfo{}
		if err = info.Deserialization(common.NewZeroCopySource(val)); err != nil {
			log.Errorf("OKManager - findLastestEpochHeight CosmosEpochSwitchInfo.Deserialization fail:%v", err)
			time.Sleep(time.Second)
			continue
		}

		return info.Height
	}
}

func (ok *OK) MonitorDeposit() {
	for {
		ethClient := ok.ethClients[randIdx(len(ok.ethClients))]
		heightU64, err := ethClient.BlockNumber(context.Background())
		if err != nil {
			log.Errorf("MonitorDeposit - ethClient.BlockNumber, err: %v", err)
			time.Sleep(time.Second)
			continue
		}
		height := int64(heightU64)
		snycheight := atomic.LoadInt64(&ok.syncedOKHeight)
		if height < snycheight {
			log.Infof("MonitorDeposit - height(%d) < snycheight(%d)", height, snycheight)
			time.Sleep(time.Second)
			continue
		}
		log.Info("MonitorDeposit ok - snyced ok height", snycheight, "ok height", height, "diff", height-snycheight)
		err = ok.handleLockDepositEvents(snycheight)
		if err != nil {
			log.Errorf("handleLockDepositEvents error: %v", err)
		}
	}
}

var (
	KeyPrefixStorage = []byte{0x05}
)

func CheckProofResult(result, value []byte) bool {
	var s_temp []byte
	err := rlp.DecodeBytes(result, &s_temp)
	if err != nil {
		log.Errorf("checkProofResult, rlp.DecodeBytes error:%s\n", err)
		return false
	}
	//
	var s []byte
	for i := len(s_temp); i < 32; i++ {
		s = append(s, 0)
	}
	s = append(s, s_temp...)
	hash := crypto.Keccak256(value)
	return bytes.Equal(s, hash)
}

func (ok *OK) handleLockDepositEvents(refHeight int64) error {
	retryList, err := ok.db.GetAllRetry()
	if err != nil {
		return fmt.Errorf("handleLockDepositEvents - ok.db.GetAllRetry error: %s", err)
	}

	for _, v := range retryList {
		// time.Sleep(time.Second * 1)
		crosstx := new(CrossTransfer)
		err := crosstx.Deserialization(common.NewZeroCopySource(v))
		if err != nil {
			log.Errorf("handleLockDepositEvents - retry.Deserialization error: %s", err)
			continue
		}

		//1. decode events
		key := crosstx.txIndex
		keyBytes, err := eth.MappingKeyAt(key, "01")
		if err != nil {
			log.Errorf("handleLockDepositEvents - MappingKeyAt error:%s\n", err.Error())
			continue
		}
		if refHeight <= int64(crosstx.height)+ok.conf.OKConfig.BlockConfig {
			continue
		}
		height := int64(refHeight - ok.conf.OKConfig.BlockConfig)
		heightHex := hexutil.EncodeBig(big.NewInt(height))
		proofKey := hexutil.Encode(keyBytes)

		//2. get proof
		proof, err := tools.GetProof(ok.conf.OKConfig.RandRestURL(), ok.conf.OKConfig.ECCDContractAddress, proofKey, heightHex)
		if err != nil {
			log.Errorf("handleLockDepositEvents - error :%v", err)
			continue
		}

		okProof := new(tools.ETHProof)
		err = json.Unmarshal(proof, okProof)
		if err != nil {
			log.Errorf("handleLockDepositEvents - ETHProof.Unmarshal error :%v", err)
			continue
		}

		var mproof merkle.Proof
		err = proto.UnmarshalText(okProof.StorageProofs[0].Proof[0], &mproof)
		if err != nil {
			log.Errorf("handleLockDepositEvents - proto.UnmarshalText failed:%v", err)
			continue
		}

		keyPath := "/"
		for i := range mproof.Ops {
			op := mproof.Ops[len(mproof.Ops)-1-i]
			keyPath += "x:" + hex.EncodeToString(op.Key)
			keyPath += "/"
		}

		keyPath = strings.TrimSuffix(keyPath, "/")

		tmClient := ok.tmClients[randIdx(len(ok.tmClients))]

		cr, err := tmClient.Tendermint().QueryCommitResult(height + 1)
		if err != nil {
			log.Errorf("handleLockDepositEvents - QueryCommitResult on height :%d failed:%v", height+1, err)
			continue
		}
		vSet, err := getValidators(tmClient, height+1)
		if err != nil {
			log.Errorf("handleLockDepositEvents - getValidators on height :%d failed:%v", height, err)
			continue
		}
		hdr := okex2.CosmosHeader{
			Header:  *cr.Header,
			Commit:  cr.Commit,
			Valsets: vSet,
		}
		raw, err := ok.cdc.MarshalBinaryBare(hdr)
		if err != nil {
			log.Errorf("handleLockDepositEvents - MarshalBinaryBare on height:%d failed:%v", height, err)
			continue
		}

		//3. commit proof to poly

		if !CheckProofResult(okProof.StorageProofs[0].Value.ToInt().Bytes(), crosstx.value) {
			panic(fmt.Sprintf("Keccak256 not match storage(%x) vs event(%x)", okProof.StorageProofs[0].Value.ToInt().Bytes(), crypto.Keccak256(crosstx.value)))
		}
		if len(mproof.Ops) != 2 {
			panic("proof size wrong")
		}
		if len(mproof.Ops[0].Key) != 1+ethcommon.HashLength+ethcommon.AddressLength {
			panic("storage key length not correct")
		}
		eccd, err := hex.DecodeString(strings.Replace(ok.conf.OKConfig.ECCDContractAddress, "0x", "", 1))
		if err != nil {
			panic(fmt.Sprintf("ECCDContractAddress decode fail:%v", err))
		}
		if !bytes.HasPrefix(mproof.Ops[0].Key, append(KeyPrefixStorage, eccd...)) {
			panic("storage key not from ccmc")
		}
		if !bytes.Equal(mproof.Ops[1].Key, []byte("evm")) {
			panic("wrong module for proof")
		}

		prt := rootmulti.DefaultProofRuntime()

		err = prt.VerifyValue(&mproof, cr.AppHash, keyPath, ethcrypto.Keccak256(crosstx.value))
		if err != nil {
			log.Fatalf("VerifyValue error: %v proof_height:%d commit_height:%d keyPath:%s cross_height:%d", err, height, cr.Header.Height, keyPath, crosstx.height)
		}

		storageProof, _ := ok.cdc.MarshalBinaryBare(mproof)
		txData, _ := ok.cdc.MarshalBinaryBare(&okex.CosmosProofValue{Kp: keyPath, Value: crosstx.value})
		txHash, err := ok.polySdk.Native.Ccm.ImportOuterTransfer(ok.conf.OKConfig.SideChainId, txData, uint32(height+1), storageProof, ok.polySigner.Address[:], raw, ok.polySigner)
		if err != nil {
			if strings.Contains(err.Error(), "tx already done") {
				log.Infof("handleLockDepositEvents - ok_tx %s already on poly", ethcommon.BytesToHash(crosstx.txId).String())
				if err := ok.db.DeleteRetry(v); err != nil {
					log.Errorf("handleLockDepositEvents - ok.db.DeleteRetry error: %s", err)
				}
				continue
			} else {
				log.Errorf("handleLockDepositEvents - ImportOuterTransfer on height:%d failed:%v", height, err)
				continue
			}
		}

		//4. put to check db for checking
		err = ok.db.PutCheck(txHash.ToHexString(), v)
		if err != nil {
			log.Errorf("handleLockDepositEvents - ok.db.PutCheck error: %s", err)
		}
		err = ok.db.DeleteRetry(v)
		if err != nil {
			log.Errorf("handleLockDepositEvents - ok.db.PutCheck error: %v", err)
		}

		log.Infof("handleLockDepositEvents - syncProofToAlia (ok_hash %s poly_hash %s) ", ethcommon.BytesToHash(crosstx.txId).Hex(), txHash.ToHexString())
	}

	return nil
}

func (ok *OK) CheckDeposit() {
	for {
		checkMap, err := ok.db.GetAllCheck()
		if err != nil {
			log.Errorf("CheckDeposit - ok.db.GetAllCheck error: %v", err)
			time.Sleep(time.Second)
			continue
		}

		for k, v := range checkMap {
			event, err := ok.polySdk.GetSmartContractEvent(k)
			if err != nil {
				log.Errorf("CheckDeposit - ok.aliaSdk.GetSmartContractEvent error: %v", err)
				continue
			}
			if event == nil {
				continue
			}
			if event.State != 1 {
				log.Infof("CheckDeposit - state of poly tx %s is not success", k)
				err := ok.db.PutRetry(v)
				if err != nil {
					log.Errorf("checkLockDepositEvents - ok.db.PutRetry error:%s", err)
				}
			}
			err = ok.db.DeleteCheck(k)
			if err != nil {
				log.Errorf("CheckDeposit - ok.db.DeleteRetry error:%s", err)
			}
		}
	}
}
