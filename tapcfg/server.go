package tapcfg

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taro"
	"github.com/lightninglabs/taro/address"
	"github.com/lightninglabs/taro/proof"
	"github.com/lightninglabs/taro/tapdb"
	"github.com/lightninglabs/taro/tapdb/sqlc"
	"github.com/lightninglabs/taro/tapfreighter"
	"github.com/lightninglabs/taro/tapgarden"
	"github.com/lightninglabs/taro/universe"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/ticker"
)

// databaseBackend is an interface that contains all methods our different
// database backends implement.
type databaseBackend interface {
	tapdb.BatchedQuerier
	WithTx(tx *sql.Tx) *sqlc.Queries
}

// genServerConfig generates a server config from the given tarod config.
//
// NOTE: The RPCConfig and SignalInterceptor fields must be set by the caller
// after genereting the server config.
func genServerConfig(cfg *Config, cfgLogger btclog.Logger,
	lndServices *lndclient.LndServices,
	mainErrChan chan<- error) (*taro.Config, error) {

	var err error

	// Now that we know where the database will live, we'll go ahead and
	// open up the default implementation of it.
	var db databaseBackend
	switch cfg.DatabaseBackend {
	case DatabaseBackendSqlite:
		cfgLogger.Infof("Opening sqlite3 database at: %v",
			cfg.Sqlite.DatabaseFileName)
		db, err = tapdb.NewSqliteStore(cfg.Sqlite)

	case DatabaseBackendPostgres:
		cfgLogger.Infof("Opening postgres database at: %v",
			cfg.Postgres.DSN(true))
		db, err = tapdb.NewPostgresStore(cfg.Postgres)

	default:
		return nil, fmt.Errorf("unknown database backend: %s",
			cfg.DatabaseBackend)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to open database: %v", err)
	}

	rksDB := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.KeyStore {
			return db.WithTx(tx)
		},
	)
	mintingStore := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.PendingAssetStore {
			return db.WithTx(tx)
		},
	)
	assetMintingStore := tapdb.NewAssetMintingStore(mintingStore)

	assetDB := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.ActiveAssetsStore {
			return db.WithTx(tx)
		},
	)

	addrBookDB := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.AddrBook {
			return db.WithTx(tx)
		},
	)
	taroChainParams := address.ParamsForChain(cfg.ActiveNetParams.Name)
	tapdbAddrBook := tapdb.NewTaroAddressBook(
		addrBookDB, &taroChainParams,
	)

	keyRing := taro.NewLndRpcKeyRing(lndServices)
	walletAnchor := taro.NewLndRpcWalletAnchor(lndServices)
	chainBridge := taro.NewLndRpcChainBridge(lndServices)

	addrBook := address.NewBook(address.BookConfig{
		Store:        tapdbAddrBook,
		StoreTimeout: tapdb.DefaultStoreTimeout,
		KeyRing:      keyRing,
		Chain:        taroChainParams,
	})

	assetStore := tapdb.NewAssetStore(assetDB)

	uniDB := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.BaseUniverseStore {
			return db.WithTx(tx)
		},
	)
	uniForestDB := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.BaseUniverseForestStore {
			return db.WithTx(tx)
		},
	)
	uniForest := tapdb.NewBaseUniverseForest(uniForestDB)

	uniStatsDB := tapdb.NewTransactionExecutor(
		db, func(tx *sql.Tx) tapdb.UniverseStatsStore {
			return db.WithTx(tx)
		},
	)
	universeStats := tapdb.NewUniverseStats(uniStatsDB)

	headerVerifier := tapgarden.GenHeaderVerifier(
		context.Background(), chainBridge,
	)
	uniCfg := universe.MintingArchiveConfig{
		NewBaseTree: func(id universe.Identifier) universe.BaseBackend {
			return tapdb.NewBaseUniverseTree(
				uniDB, id,
			)
		},
		HeaderVerifier: headerVerifier,
		UniverseForest: uniForest,
		UniverseStats:  universeStats,
	}

	federationStore := tapdb.NewTransactionExecutor(db,
		func(tx *sql.Tx) tapdb.UniverseServerStore {
			return db.WithTx(tx)
		},
	)
	federationDB := tapdb.NewUniverseFederationDB(federationStore)

	proofFileStore, err := proof.NewFileArchiver(cfg.networkDir)
	if err != nil {
		return nil, fmt.Errorf("unable to open disk archive: %v", err)
	}
	proofArchive := proof.NewMultiArchiver(
		&proof.BaseVerifier{}, tapdb.DefaultStoreTimeout,
		assetStore, proofFileStore,
	)

	var hashMailCourier proof.Courier[proof.Recipient]
	if cfg.HashMailCourier != nil {
		hashMailBox, err := proof.NewHashMailBox(
			cfg.HashMailCourier.Addr,
			cfg.HashMailCourier.TlsCertPath,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to make mailbox: %v",
				err)
		}

		hashMailCourier, err = proof.NewHashMailCourier(
			cfg.HashMailCourier, hashMailBox, assetStore,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to make hashmail "+
				"courier: %v", err)
		}
	}

	baseUni := universe.NewMintingArchive(uniCfg)

	universeSyncer := universe.NewSimpleSyncer(universe.SimpleSyncCfg{
		LocalDiffEngine:     baseUni,
		NewRemoteDiffEngine: taro.NewRpcUniverseDiff,
		LocalRegistrar:      baseUni,
	})

	universeFederation := universe.NewFederationEnvoy(
		universe.FederationConfig{
			FederationDB:       federationDB,
			UniverseSyncer:     universeSyncer,
			LocalRegistrar:     baseUni,
			SyncInterval:       cfg.UniverseSyncInterval,
			NewRemoteRegistrar: taro.NewRpcUniverseRegistar,
			ErrChan:            mainErrChan,
		},
	)

	virtualTxSigner := taro.NewLndRpcVirtualTxSigner(lndServices)
	coinSelect := tapfreighter.NewCoinSelect(assetStore)
	assetWallet := tapfreighter.NewAssetWallet(&tapfreighter.WalletConfig{
		CoinSelector: coinSelect,
		AssetProofs:  proofArchive,
		AddrBook:     tapdbAddrBook,
		KeyRing:      keyRing,
		Signer:       virtualTxSigner,
		TxValidator:  &taro.ValidatorV0{},
		Wallet:       walletAnchor,
		ChainParams:  &taroChainParams,
	})

	return &taro.Config{
		DebugLevel:  cfg.DebugLevel,
		ChainParams: cfg.ActiveNetParams,
		AssetMinter: tapgarden.NewChainPlanter(tapgarden.PlanterConfig{
			GardenKit: tapgarden.GardenKit{
				Wallet:      walletAnchor,
				ChainBridge: chainBridge,
				Log:         assetMintingStore,
				KeyRing:     keyRing,
				GenSigner: taro.NewLndRpcGenSigner(
					lndServices,
				),
				ProofFiles: proofFileStore,
				Universe:   universeFederation,
			},
			BatchTicker: ticker.NewForce(cfg.BatchMintingInterval),
			ErrChan:     mainErrChan,
		}),
		AssetCustodian: tapgarden.NewCustodian(
			&tapgarden.CustodianConfig{
				ChainParams:   &taroChainParams,
				WalletAnchor:  walletAnchor,
				ChainBridge:   chainBridge,
				AddrBook:      addrBook,
				ProofArchive:  proofArchive,
				ProofNotifier: assetStore,
				ErrChan:       mainErrChan,
				ProofCourier:  hashMailCourier,
			},
		),
		ChainBridge:  chainBridge,
		AddrBook:     addrBook,
		ProofArchive: proofArchive,
		AssetWallet:  assetWallet,
		ChainPorter: tapfreighter.NewChainPorter(
			&tapfreighter.ChainPorterConfig{
				CoinSelector: coinSelect,
				Signer:       virtualTxSigner,
				TxValidator:  &taro.ValidatorV0{},
				ExportLog:    assetStore,
				ChainBridge:  chainBridge,
				Wallet:       walletAnchor,
				KeyRing:      keyRing,
				AssetWallet:  assetWallet,
				AssetProofs:  proofFileStore,
				ProofCourier: hashMailCourier,
				ErrChan:      mainErrChan,
			},
		),
		BaseUniverse:       baseUni,
		UniverseSyncer:     universeSyncer,
		UniverseFederation: universeFederation,
		UniverseStats:      universeStats,
		LogWriter:          cfg.LogWriter,
		DatabaseConfig: &taro.DatabaseConfig{
			RootKeyStore:   tapdb.NewRootKeyStore(rksDB),
			MintingStore:   assetMintingStore,
			AssetStore:     assetStore,
			TaroAddrBook:   tapdbAddrBook,
			UniverseForest: uniForest,
			FederationDB:   federationDB,
		},
	}, nil
}

// CreateServerFromConfig creates a new Taro server from the given CLI config.
func CreateServerFromConfig(cfg *Config, cfgLogger btclog.Logger,
	shutdownInterceptor signal.Interceptor,
	mainErrChan chan<- error) (*taro.Server, error) {

	// Given the config above, grab the TLS config which includes the set
	// of dial options, and also the listeners we'll use to listen on the
	// RPC system.
	serverOpts, restDialOpts, restListen, err := getTLSConfig(
		cfg, cfgLogger,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load TLS credentials: %v",
			err)
	}

	cfgLogger.Infof("Attempting to establish connection to lnd...")

	lndConn, err := getLnd(
		cfg.ChainConf.Network, cfg.Lnd, shutdownInterceptor,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to lnd node: %v", err)
	}

	cfgLogger.Infof("lnd connection initialized")

	serverCfg, err := genServerConfig(
		cfg, cfgLogger, &lndConn.LndServices, mainErrChan,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate server config: %v",
			err)
	}

	serverCfg.SignalInterceptor = shutdownInterceptor

	serverCfg.RPCConfig = &taro.RPCConfig{
		LisCfg:            &lnd.ListenerCfg{},
		RPCListeners:      cfg.rpcListeners,
		RESTListeners:     cfg.restListeners,
		GrpcServerOpts:    serverOpts,
		RestDialOpts:      restDialOpts,
		RestListenFunc:    restListen,
		WSPingInterval:    cfg.RpcConf.WSPingInterval,
		WSPongWait:        cfg.RpcConf.WSPongWait,
		RestCORS:          cfg.RpcConf.RestCORS,
		NoMacaroons:       cfg.RpcConf.NoMacaroons,
		MacaroonPath:      cfg.RpcConf.MacaroonPath,
		LetsEncryptDir:    cfg.RpcConf.LetsEncryptDir,
		LetsEncryptListen: cfg.RpcConf.LetsEncryptListen,
		LetsEncryptEmail:  cfg.RpcConf.LetsEncryptEmail,
		LetsEncryptDomain: cfg.RpcConf.LetsEncryptDomain,
	}

	return taro.NewServer(serverCfg), nil
}

// CreateServerFromConfig creates a new Taro server from the given CLI config.
func CreateSubServerFromConfig(cfg *Config, cfgLogger btclog.Logger,
	lndServices *lndclient.LndServices,
	mainErrChan chan<- error) (*taro.Server, error) {

	serverCfg, err := genServerConfig(
		cfg, cfgLogger, lndServices, mainErrChan,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate server config: %v",
			err)
	}

	serverCfg.RPCConfig = &taro.RPCConfig{
		NoMacaroons:  cfg.RpcConf.NoMacaroons,
		MacaroonPath: cfg.RpcConf.MacaroonPath,
	}

	return taro.NewServer(serverCfg), nil
}
