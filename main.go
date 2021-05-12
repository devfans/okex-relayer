package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"poly_bridge_sdk"

	"github.com/ethereum/go-ethereum/ethclient"
	oksdk "github.com/okex/exchain-go-sdk"
	"github.com/polynetwork/okex-relayer/config"
	"github.com/polynetwork/okex-relayer/pkg/db"
	"github.com/polynetwork/okex-relayer/pkg/log"
	"github.com/polynetwork/okex-relayer/pkg/manager"
	"github.com/polynetwork/okex-relayer/pkg/tools"
	sdk "github.com/polynetwork/poly-go-sdk"
)

var confFile string
var syncedOKHeight int64
var syncedPolyHeight int64

func init() {
	flag.StringVar(&confFile, "conf", "./config.json", "configuration file path")
	flag.Int64Var(&syncedOKHeight, "ok", 0, "specify ok height")
	flag.Int64Var(&syncedPolyHeight, "poly", 0, "specify poly height")

	flag.Parse()
}

func setUpPoly(polySdk *sdk.PolySdk, rpcAddr string) error {
	polySdk.NewRpcClient().SetAddress(rpcAddr)
	hdr, err := polySdk.GetHeaderByHeight(0)
	if err != nil {
		return err
	}
	polySdk.SetChainId(hdr.ChainID)
	return nil
}

func setUpOkClientAndKeyStore(okConfig *config.OKConfig) ([]*ethclient.Client, []*oksdk.Client, *tools.EthKeyStore) {
	var clients []*ethclient.Client
	for _, node := range okConfig.RestURL {
		client, err := ethclient.Dial(node)
		if err != nil {
			log.Fatal(fmt.Sprintf("ethclient.Dial failed:%v", err))
		}

		clients = append(clients, client)
	}

	var tmClients []*oksdk.Client
	for _, tmNode := range okConfig.RestURL {
		config, _ := oksdk.NewClientConfig(tmNode, "okexchain-65", oksdk.BroadcastBlock, "0.01okt", 200000, 0, "")
		client := oksdk.NewClient(config)

		tmClients = append(tmClients, &client)
	}

	start := time.Now()
	chainID, err := clients[0].ChainID(context.Background())
	if err != nil {
		log.Fatal(fmt.Sprintf("clients[0].ChainID failed:%v", err))
	}
	log.Infof("SideChain %d ChainID() took %v", okConfig.SideChainId, time.Now().Sub(start).String())

	ks := tools.NewEthKeyStore(okConfig.KeyStorePath, okConfig.KeyStorePwdSet, chainID)

	return clients, tmClients, ks
}

func main() {

	log.InitLog(log.InfoLog, "./Log/", log.Stdout)
	conf, err := config.LoadConfig(confFile)
	if err != nil {
		log.Fatal("LoadConfig fail", err)
	}

	polySdk := sdk.NewPolySdk()
	err = setUpPoly(polySdk, conf.PolyConfig.RestURL)
	if err != nil {
		log.Fatalf("setUpPoly failed: %v", err)
	}

	wallet, err := polySdk.OpenWallet(conf.PolyConfig.WalletFile)
	if err != nil {
		log.Fatalf("polySdk.OpenWallet failed: %v", err)
	}
	signer, err := wallet.GetDefaultAccount([]byte(conf.PolyConfig.WalletPwd))
	if err != nil {
		log.Fatalf("wallet.GetDefaultAccount failed: %v", err)
	}

	ethClients, tmClients, ks := setUpOkClientAndKeyStore(&conf.OKConfig)

	var boltDB *db.BoltDB
	if conf.BoltDbPath == "" {
		boltDB, err = db.NewBoltDB("boltdb")
	} else {
		boltDB, err = db.NewBoltDB(conf.BoltDbPath)
	}
	if err != nil {
		log.Fatalf("db.NewWaitingDB error:%s", err)
		return
	}

	bridgeSdk := poly_bridge_sdk.NewBridgeFeeCheck(conf.BridgeConfig.RestURL, 5)

	polyMgr := manager.NewPoly(conf, uint32(syncedPolyHeight), polySdk, ethClients, ks, boltDB, bridgeSdk)

	okMgr := manager.NewOKEx(conf, syncedOKHeight, signer, polySdk, ethClients, tmClients, boltDB)

	go polyMgr.MonitorChain()

	go okMgr.MonitorChain()
	go okMgr.MonitorDeposit()
	go okMgr.CheckDeposit()

	waitToExit()
}

func waitToExit() {
	exit := make(chan bool, 0)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sc {
			log.Infof("waitToExit - BSC relayer received exit signal:%v.", sig.String())
			close(exit)
			break
		}
	}()
	<-exit
}
