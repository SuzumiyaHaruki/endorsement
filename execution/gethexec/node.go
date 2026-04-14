// Copyright 2023-2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md
package gethexec

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/arbitrum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/programs"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/consensus"
	"github.com/offchainlabs/nitro/consensus/consensusrpcclient"
	"github.com/offchainlabs/nitro/endorsement"
	"github.com/offchainlabs/nitro/endorsementpolicy"
	"github.com/offchainlabs/nitro/execution"
	executionrpcserver "github.com/offchainlabs/nitro/execution/rpcserver"
	"github.com/offchainlabs/nitro/solgen/go/precompilesgen"
	"github.com/offchainlabs/nitro/util"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/dbutil"
	"github.com/offchainlabs/nitro/util/headerreader"
	"github.com/offchainlabs/nitro/util/rpcclient"
	"github.com/offchainlabs/nitro/util/rpcserver"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

type StylusTargetConfig struct {
	Arm64      string   `koanf:"arm64"`
	Amd64      string   `koanf:"amd64"`
	Host       string   `koanf:"host"`
	ExtraArchs []string `koanf:"extra-archs"`

	wasmTargets []rawdb.WasmTarget
}

func (c *StylusTargetConfig) WasmTargets() []rawdb.WasmTarget {
	return c.wasmTargets
}

func (c *StylusTargetConfig) Validate() error {
	targetsSet := make(map[rawdb.WasmTarget]bool, len(c.ExtraArchs))
	for _, arch := range c.ExtraArchs {
		target := rawdb.WasmTarget(arch)
		if !rawdb.IsSupportedWasmTarget(target) {
			return fmt.Errorf("unsupported architecture: %v, possible values: %s, %s, %s, %s", arch, rawdb.TargetWavm, rawdb.TargetArm64, rawdb.TargetAmd64, rawdb.TargetHost)
		}
		targetsSet[target] = true
	}
	targetsSet[rawdb.LocalTarget()] = true
	targets := make([]rawdb.WasmTarget, 0, len(c.ExtraArchs)+1)
	for target := range targetsSet {
		targets = append(targets, target)
	}
	sort.Slice(
		targets,
		func(i, j int) bool {
			return targets[i] < targets[j]
		})
	c.wasmTargets = targets
	return nil
}

var DefaultStylusTargetConfig = StylusTargetConfig{
	Arm64:      programs.DefaultTargetDescriptionArm,
	Amd64:      programs.DefaultTargetDescriptionX86,
	Host:       "",
	ExtraArchs: []string{string(rawdb.TargetWavm)},
}

func StylusTargetConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.String(prefix+".arm64", DefaultStylusTargetConfig.Arm64, "stylus programs compilation target for arm64 linux")
	f.String(prefix+".amd64", DefaultStylusTargetConfig.Amd64, "stylus programs compilation target for amd64 linux")
	f.String(prefix+".host", DefaultStylusTargetConfig.Host, "stylus programs compilation target for system other than 64-bit ARM or 64-bit x86")
	f.StringSlice(prefix+".extra-archs", DefaultStylusTargetConfig.ExtraArchs, fmt.Sprintf("Comma separated list of extra architectures to cross-compile stylus program to and cache in wasm store (additionally to local target). Currently must include at least %s. (supported targets: %s, %s, %s, %s)", rawdb.TargetWavm, rawdb.TargetWavm, rawdb.TargetArm64, rawdb.TargetAmd64, rawdb.TargetHost))
}

type TxIndexerConfig struct {
	Enable        bool          `koanf:"enable"`
	TxLookupLimit uint64        `koanf:"tx-lookup-limit"`
	Threads       int           `koanf:"threads"`
	MinBatchDelay time.Duration `koanf:"min-batch-delay"`
}

var DefaultTxIndexerConfig = TxIndexerConfig{
	Enable:        true,
	TxLookupLimit: 126_230_400, // 1 year at 4 blocks per second
	Threads:       util.GoMaxProcs(),
	MinBatchDelay: time.Second,
}

func TxIndexerConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.Bool(prefix+".enable", DefaultTxIndexerConfig.Enable, "enables transaction indexer")
	f.Uint64(prefix+".tx-lookup-limit", DefaultTxIndexerConfig.TxLookupLimit, "retain the ability to lookup transactions by hash for the past N blocks (0 = all blocks)")
	f.Int(prefix+".threads", DefaultTxIndexerConfig.Threads, "number of threads used to RLP decode blocks during indexing/unindexing of historical transactions")
	f.Duration(prefix+".min-batch-delay", DefaultTxIndexerConfig.MinBatchDelay, "minimum delay between transaction indexing/unindexing batches; the bigger the delay, the more blocks can be included in each batch")
}

// new
type EndorsementExperimentConfig struct {
	Enable bool `koanf:"enable"`

	// disabled | local | remote
	Mode string `koanf:"mode"`

	DefaultThreshold uint32 `koanf:"default-threshold"`
	StrictThreshold  uint32 `koanf:"strict-threshold"`

	// bls | individual | bitmap | commitment-only
	DefaultAggregation string `koanf:"default-aggregation"`
	StrictAggregation  string `koanf:"strict-aggregation"`

	BlockEndorsementTimeout time.Duration `koanf:"block-endorsement-timeout"`
	MaxRebuildRounds        int           `koanf:"max-rebuild-rounds"`

	FailToAddress string `koanf:"fail-to-address"`

	EndorserAURL string `koanf:"endorser-a-url"`
	EndorserBURL string `koanf:"endorser-b-url"`
	EndorserCURL string `koanf:"endorser-c-url"`

	EndorserAPubKeyHex string `koanf:"endorser-a-pubkey"`
	EndorserBPubKeyHex string `koanf:"endorser-b-pubkey"`
	EndorserCPubKeyHex string `koanf:"endorser-c-pubkey"`
}

func (c *EndorsementExperimentConfig) Validate() error {
	if c == nil {
		return errors.New("nil endorsement experiment config")
	}

	switch c.Mode {
	case "disabled", "local", "remote":
	default:
		return fmt.Errorf("invalid endorsement experiment mode: %s", c.Mode)
	}

	if c.DefaultThreshold == 0 {
		return errors.New("endorsement default-threshold must be greater than 0")
	}
	if c.StrictThreshold == 0 {
		return errors.New("endorsement strict-threshold must be greater than 0")
	}
	if c.BlockEndorsementTimeout <= 0 {
		return errors.New("endorsement block-endorsement-timeout must be greater than 0")
	}
	if c.MaxRebuildRounds < 0 {
		return errors.New("endorsement max-rebuild-rounds must be non-negative")
	}

	if c.FailToAddress == "" || !common.IsHexAddress(c.FailToAddress) {
		return fmt.Errorf("invalid endorsement fail-to-address: %q", c.FailToAddress)
	}

	if _, err := parseAggregationType(c.DefaultAggregation); err != nil {
		return fmt.Errorf("invalid default aggregation: %w", err)
	}
	if _, err := parseAggregationType(c.StrictAggregation); err != nil {
		return fmt.Errorf("invalid strict aggregation: %w", err)
	}

	if c.Mode == "remote" {
		if strings.TrimSpace(c.EndorserAURL) == "" ||
			strings.TrimSpace(c.EndorserBURL) == "" ||
			strings.TrimSpace(c.EndorserCURL) == "" {
			return errors.New("remote mode requires non-empty endorser URLs")
		}
		if strings.TrimSpace(c.EndorserAPubKeyHex) == "" ||
			strings.TrimSpace(c.EndorserBPubKeyHex) == "" ||
			strings.TrimSpace(c.EndorserCPubKeyHex) == "" {
			return errors.New("remote mode requires non-empty endorser public keys")
		}
	}

	return nil
}

var DefaultEndorsementExperimentConfig = EndorsementExperimentConfig{
	Enable: true,
	Mode:   "remote",

	DefaultThreshold: 2,
	StrictThreshold:  3,

	DefaultAggregation: "bls",
	StrictAggregation:  "bls",

	BlockEndorsementTimeout: 2 * time.Second,
	MaxRebuildRounds:        3,

	FailToAddress: "0x1111111111111111111111111111111111111111",

	EndorserAURL: "http://endorser-a:9001",
	EndorserBURL: "http://endorser-b:9002",
	EndorserCURL: "http://endorser-c:9003",

	EndorserAPubKeyHex: "a3f44d234234430c7c7c3268d5f49a674edc2281bb0aec9ea14be8b598c22c7ad0d909d14b35a4c5d64e155dd28d81ba",
	EndorserBPubKeyHex: "ad10f131ec7851674af913cbc0a62ebb7efd981535e3a0fdcdadc8c7e33bd7e494d4488b018d55659a7d44346b31ebc1",
	EndorserCPubKeyHex: "90f21d7ed995c790d21df79dcc2ad4e1464520108b46767c9326b56c6cce09bb9356962c06471ac1185ea9bc9df43a9b",
}

func EndorsementExperimentConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.Bool(prefix+".enable", DefaultEndorsementExperimentConfig.Enable, "enable endorsement experiment wiring")
	f.String(prefix+".mode", DefaultEndorsementExperimentConfig.Mode, "endorsement mode: disabled | local | remote")
	f.Uint32(prefix+".default-threshold", DefaultEndorsementExperimentConfig.DefaultThreshold, "endorsement threshold for default policy")
	f.Uint32(prefix+".strict-threshold", DefaultEndorsementExperimentConfig.StrictThreshold, "endorsement threshold for strict policy")
	f.String(prefix+".default-aggregation", DefaultEndorsementExperimentConfig.DefaultAggregation, "endorsement aggregation for default policy: bls | individual | bitmap | commitment-only")
	f.String(prefix+".strict-aggregation", DefaultEndorsementExperimentConfig.StrictAggregation, "endorsement aggregation for strict policy: bls | individual | bitmap | commitment-only")
	f.Duration(prefix+".block-endorsement-timeout", DefaultEndorsementExperimentConfig.BlockEndorsementTimeout, "endorsement timeout for one candidate block")
	f.Int(prefix+".max-rebuild-rounds", DefaultEndorsementExperimentConfig.MaxRebuildRounds, "max rebuild rounds after endorsement failure")
	f.String(prefix+".fail-to-address", DefaultEndorsementExperimentConfig.FailToAddress, "transactions sent to this address use strict endorsement policy")
	f.String(prefix+".endorser-a-url", DefaultEndorsementExperimentConfig.EndorserAURL, "remote URL for endorser A")
	f.String(prefix+".endorser-b-url", DefaultEndorsementExperimentConfig.EndorserBURL, "remote URL for endorser B")
	f.String(prefix+".endorser-c-url", DefaultEndorsementExperimentConfig.EndorserCURL, "remote URL for endorser C")
	f.String(prefix+".endorser-a-pubkey", DefaultEndorsementExperimentConfig.EndorserAPubKeyHex, "hex-encoded BLS public key for endorser A")
	f.String(prefix+".endorser-b-pubkey", DefaultEndorsementExperimentConfig.EndorserBPubKeyHex, "hex-encoded BLS public key for endorser B")
	f.String(prefix+".endorser-c-pubkey", DefaultEndorsementExperimentConfig.EndorserCPubKeyHex, "hex-encoded BLS public key for endorser C")
}

type Config struct {
	ParentChainReader           headerreader.Config    `koanf:"parent-chain-reader" reload:"hot"`
	Sequencer                   SequencerConfig        `koanf:"sequencer" reload:"hot"`
	RecordingDatabase           BlockRecorderConfig    `koanf:"recording-database"`
	TxPreChecker                TxPreCheckerConfig     `koanf:"tx-pre-checker" reload:"hot"`
	Forwarder                   ForwarderConfig        `koanf:"forwarder"`
	ForwardingTarget            string                 `koanf:"forwarding-target"`
	SecondaryForwardingTarget   []string               `koanf:"secondary-forwarding-target"`
	Caching                     CachingConfig          `koanf:"caching"`
	RPC                         arbitrum.Config        `koanf:"rpc"`
	TxIndexer                   TxIndexerConfig        `koanf:"tx-indexer"`
	EnablePrefetchBlock         bool                   `koanf:"enable-prefetch-block"`
	SyncMonitor                 SyncMonitorConfig      `koanf:"sync-monitor"`
	StylusTarget                StylusTargetConfig     `koanf:"stylus-target"`
	BlockMetadataApiCacheSize   uint64                 `koanf:"block-metadata-api-cache-size"`
	BlockMetadataApiBlocksLimit uint64                 `koanf:"block-metadata-api-blocks-limit"`
	VmTrace                     LiveTracingConfig      `koanf:"vmtrace"`
	ExposeMultiGas              bool                   `koanf:"expose-multi-gas"`
	RPCServer                   rpcserver.Config       `koanf:"rpc-server"`
	ConsensusRPCClient          rpcclient.ClientConfig `koanf:"consensus-rpc-client" reload:"hot"`

	forwardingTarget string
	EndorsementExperiment EndorsementExperimentConfig `koanf:"endorsement-experiment"`
}

func (c *Config) Validate() error {
	if err := c.Caching.Validate(); err != nil {
		return err
	}
	if err := c.Sequencer.Validate(); err != nil {
		return err
	}
	if !c.Sequencer.Enable && c.ForwardingTarget == "" {
		return errors.New("ForwardingTarget not set and not sequencer (can use \"null\")")
	}
	if c.ForwardingTarget == "null" {
		c.forwardingTarget = ""
	} else {
		c.forwardingTarget = c.ForwardingTarget
	}
	if c.forwardingTarget != "" && c.Sequencer.Enable {
		return errors.New("ForwardingTarget set and sequencer enabled")
	}
	if err := c.StylusTarget.Validate(); err != nil {
		return err
	}
	if err := c.RPC.Validate(); err != nil {
		return err
	}
	if err := c.ConsensusRPCClient.Validate(); err != nil {
		return fmt.Errorf("error validating ConsensusRPCClient config: %w", err)
	}
	if err := c.EndorsementExperiment.Validate(); err != nil {
		return fmt.Errorf("error validating EndorsementExperiment config: %w", err)
	}
	return nil
}

func ConfigAddOptions(prefix string, f *pflag.FlagSet) {
	arbitrum.ConfigAddOptions(prefix+".rpc", f)
	TxIndexerConfigAddOptions(prefix+".tx-indexer", f)
	SequencerConfigAddOptions(prefix+".sequencer", f)
	headerreader.AddOptions(prefix+".parent-chain-reader", f)
	BlockRecorderConfigAddOptions(prefix+".recording-database", f)
	f.String(prefix+".forwarding-target", ConfigDefault.ForwardingTarget, "transaction forwarding target URL, or \"null\" to disable forwarding (iff not sequencer)")
	f.StringSlice(prefix+".secondary-forwarding-target", ConfigDefault.SecondaryForwardingTarget, "secondary transaction forwarding target URL")
	AddOptionsForNodeForwarderConfig(prefix+".forwarder", f)
	TxPreCheckerConfigAddOptions(prefix+".tx-pre-checker", f)
	CachingConfigAddOptions(prefix+".caching", f)
	SyncMonitorConfigAddOptions(prefix+".sync-monitor", f)
	f.Bool(prefix+".enable-prefetch-block", ConfigDefault.EnablePrefetchBlock, "enable prefetching of blocks")
	StylusTargetConfigAddOptions(prefix+".stylus-target", f)
	f.Uint64(prefix+".block-metadata-api-cache-size", ConfigDefault.BlockMetadataApiCacheSize, "size (in bytes) of lru cache storing the blockMetadata to service arb_getRawBlockMetadata")
	f.Uint64(prefix+".block-metadata-api-blocks-limit", ConfigDefault.BlockMetadataApiBlocksLimit, "maximum number of blocks allowed to be queried for blockMetadata per arb_getRawBlockMetadata query. Enabled by default, set 0 to disable the limit")
	f.Bool(prefix+".expose-multi-gas", false, "experimental: expose multi-dimensional gas in transaction receipts")
	LiveTracingConfigAddOptions(prefix+".vmtrace", f)
	rpcserver.ConfigAddOptions(prefix+".rpc-server", "execution", f)
	rpcclient.RPCClientAddOptions(prefix+".consensus-rpc-client", f, &ConfigDefault.ConsensusRPCClient)
	EndorsementExperimentConfigAddOptions(prefix+".endorsement-experiment", f)
}

type LiveTracingConfig struct {
	TracerName string `koanf:"tracer-name"`
	JSONConfig string `koanf:"json-config"`
}

var DefaultLiveTracingConfig = LiveTracingConfig{
	TracerName: "",
	JSONConfig: "{}",
}

func LiveTracingConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.String(prefix+".tracer-name", DefaultLiveTracingConfig.TracerName, "(experimental) Name of tracer which should record internal VM operations (costly)")
	f.String(prefix+".json-config", DefaultLiveTracingConfig.JSONConfig, "(experimental) Tracer configuration in JSON format")
}

var ConfigDefault = Config{
	RPC:                       arbitrum.DefaultConfig,
	TxIndexer:                 DefaultTxIndexerConfig,
	Sequencer:                 DefaultSequencerConfig,
	ParentChainReader:         headerreader.DefaultConfig,
	RecordingDatabase:         DefaultBlockRecorderConfig,
	ForwardingTarget:          "",
	SecondaryForwardingTarget: []string{},
	TxPreChecker:              DefaultTxPreCheckerConfig,
	Caching:                   DefaultCachingConfig,
	Forwarder:                 DefaultNodeForwarderConfig,
	SyncMonitor:               DefaultSyncMonitorConfig,

	EnablePrefetchBlock:         true,
	StylusTarget:                DefaultStylusTargetConfig,
	BlockMetadataApiCacheSize:   100 * 1024 * 1024,
	BlockMetadataApiBlocksLimit: 100,
	VmTrace:                     DefaultLiveTracingConfig,
	ExposeMultiGas:              false,

	RPCServer: rpcserver.DefaultConfig,
	ConsensusRPCClient: rpcclient.ClientConfig{
		URL:                       "",
		JWTSecret:                 "",
		Retries:                   3,
		RetryErrors:               "websocket: close.*|dial tcp .*|.*i/o timeout|.*connection reset by peer|.*connection refused",
		ArgLogLimit:               2048,
		WebsocketMessageSizeLimit: 256 * 1024 * 1024,
	},

	EndorsementExperiment: DefaultEndorsementExperimentConfig,
}

type ConfigFetcher interface {
	Get() *Config
}

type ExecutionNode struct {
	stopwaiter.StopWaiter
	ExecutionDB              ethdb.Database
	Backend                  *arbitrum.Backend
	FilterSystem             *filters.FilterSystem
	ArbInterface             *ArbInterface
	ExecEngine               *ExecutionEngine
	Recorder                 *BlockRecorder
	Sequencer                *Sequencer // either nil or same as TxPublisher
	TxPreChecker             *TxPreChecker
	TxPublisher              TransactionPublisher
	ExpressLaneService       *expressLaneService
	configFetcher            ConfigFetcher
	SyncMonitor              *SyncMonitor
	ParentChainReader        *headerreader.HeaderReader
	ClassicOutbox            *ClassicOutboxRetriever
	started                  atomic.Bool
	bulkBlockMetadataFetcher *BulkBlockMetadataFetcher
	consensusRPCClient       *consensusrpcclient.ConsensusRPCClient
}

func CreateExecutionNode(
	ctx context.Context,
	stack *node.Node,
	executionDB ethdb.Database,
	l2BlockChain *core.BlockChain,
	l1client *ethclient.Client,
	configFetcher ConfigFetcher,
	parentChainID *big.Int,
	syncTillBlock uint64,
) (*ExecutionNode, error) {
	config := configFetcher.Get()

	execEngine := NewExecutionEngine(l2BlockChain, syncTillBlock, config.ExposeMultiGas)

	expCfg := config.EndorsementExperiment

	defaultAgg, err := parseAggregationType(expCfg.DefaultAggregation)
	if err != nil {
		return nil, fmt.Errorf("parse default aggregation: %w", err)
	}
	strictAgg, err := parseAggregationType(expCfg.StrictAggregation)
	if err != nil {
		return nil, fmt.Errorf("parse strict aggregation: %w", err)
	}

	defaultPolicy, strictPolicy := buildDefaultEndorsementPolicies(
		expCfg.DefaultThreshold,
		expCfg.StrictThreshold,
		defaultAgg,
		strictAgg,
	)

	failAddr := common.HexToAddress(expCfg.FailToAddress)
	rules := endorsementpolicy.BuildDefaultExperimentRules(failAddr, strictPolicy, defaultPolicy)

	ruleResolver, err := endorsementpolicy.NewRuleBasedResolver(defaultPolicy, rules)
	if err != nil {
		return nil, fmt.Errorf("failed to build rule-based endorsement resolver: %w", err)
	}

	policyConfig := &endorsementpolicy.PolicyConfig{
		BlockEndorsementTimeout: expCfg.BlockEndorsementTimeout,
		MaxRebuildRounds:        expCfg.MaxRebuildRounds,
	}

	var (
		mgr            *endorsement.DefaultEndorsementManager
		blsPubRegistry *endorsement.InMemoryBLSPublicKeyRegistry
	)

	if expCfg.Enable && expCfg.Mode != "disabled" {
		var client endorsement.EndorsementClient

		switch expCfg.Mode {
		case "local":
			client = &endorsement.MockEndorsementClient{
				Rules: endorsement.EndorsementRejectRules{
					RejectByToAndEndorser: map[common.Address]map[endorsementpolicy.EndorserID]bool{
						failAddr: {
							"A": true,
							"B": true,
							"C": true,
						},
					},
				},
			}

		case "remote":
			blsPubRegistry, err = buildBLSPublicKeyRegistryFromConfig(expCfg)
			if err != nil {
				return nil, err
			}

			client = &endorsement.RemoteEndorsementClient{
				Endpoints: map[endorsementpolicy.EndorserID]string{
					"A": expCfg.EndorserAURL,
					"B": expCfg.EndorserBURL,
					"C": expCfg.EndorserCURL,
				},
				HTTPClient: &http.Client{
					Timeout: expCfg.BlockEndorsementTimeout,
				},
			}

		default:
			return nil, fmt.Errorf("unknown endorsement mode: %s", expCfg.Mode)
		}

		var certBuilder endorsement.CertificateBuilder
		if blsPubRegistry != nil {
			certBuilder = &endorsement.DefaultCertificateBuilder{
				BLSPublicKeys: blsPubRegistry,
			}
		} else {
			certBuilder = &endorsement.DefaultCertificateBuilder{}
		}

		mgr = &endorsement.DefaultEndorsementManager{
			RequestBuilder: &endorsement.DefaultRequestBuilder{},
			Client:         client,
			Collector:      &endorsement.InMemoryResultCollector{},
			CertificateBuilder: certBuilder,
			RootBuilder:        &endorsement.DefaultRootBuilder{},
		}
	}

	if mgr != nil {
		execEngine.SetCandidateBlockEndorser(mgr)
		execEngine.SetPolicyConfig(policyConfig)
		if blsPubRegistry != nil {
			execEngine.SetCommitmentVerifierBLSPublicKeys(blsPubRegistry)
		}
	} else {
		log.Info("ENDORSEMENT_EXPERIMENT_DISABLED",
			"enable", expCfg.Enable,
			"mode", expCfg.Mode,
		)
	}

	if config.EnablePrefetchBlock {
		execEngine.EnablePrefetchBlock()
	}
	if config.Caching.DisableStylusCacheMetricsCollection {
		execEngine.DisableStylusCacheMetricsCollection()
	}

	recorder := NewBlockRecorder(&config.RecordingDatabase, execEngine, executionDB)
	var txPublisher TransactionPublisher
	var sequencer *Sequencer

	var parentChainReader *headerreader.HeaderReader
	if l1client != nil && !reflect.ValueOf(l1client).IsNil() {
		arbSys, _ := precompilesgen.NewArbSys(types.ArbSysAddress, l1client)
		parentChainReader, err = headerreader.New(ctx, l1client, func() *headerreader.Config { return &configFetcher.Get().ParentChainReader }, arbSys)
		if err != nil {
			return nil, err
		}
	} else if config.Sequencer.Enable {
		log.Warn("sequencer enabled without l1 client")
	}

	if config.Sequencer.Enable {
		seqConfigFetcher := func() *SequencerConfig { return &configFetcher.Get().Sequencer }
		sequencer, err = NewSequencer(execEngine, parentChainReader, seqConfigFetcher, parentChainID)
		if err != nil {
			return nil, err
		}

		sequencer.SetPolicyResolver(ruleResolver)

		if mgr != nil {
			sequencer.SetPolicyConfig(policyConfig)
		}

		txPublisher = sequencer
	} else {
		if config.Forwarder.RedisUrl != "" {
			txPublisher = NewRedisTxForwarder(config.forwardingTarget, &config.Forwarder)
		} else if config.forwardingTarget == "" {
			txPublisher = NewTxDropper()
		} else {
			targets := append([]string{config.forwardingTarget}, config.SecondaryForwardingTarget...)
			txPublisher = NewForwarder(targets, &config.Forwarder)
		}
	}

	txprecheckConfigFetcher := func() *TxPreCheckerConfig { return &configFetcher.Get().TxPreChecker }

	txPreChecker := NewTxPreChecker(txPublisher, l2BlockChain, txprecheckConfigFetcher)
	txPublisher = txPreChecker
	arbInterface, err := NewArbInterface(l2BlockChain, txPublisher)
	if err != nil {
		return nil, err
	}
	filterConfig := filters.Config{
		LogCacheSize: config.RPC.FilterLogCacheSize,
		Timeout:      config.RPC.FilterTimeout,
	}
	backend, filterSystem, err := arbitrum.NewBackend(stack, &config.RPC, executionDB, arbInterface, filterConfig, config.Caching.StateScheme)
	if err != nil {
		return nil, err
	}

	syncMon := NewSyncMonitor(&config.SyncMonitor, execEngine)

	var classicOutbox *ClassicOutboxRetriever

	if l2BlockChain.Config().ArbitrumChainParams.GenesisBlockNum > 0 {
		classicMsgDB, err := stack.OpenDatabaseWithOptions("classic-msg", node.DatabaseOptions{
			MetricsNamespace: "classicmsg/",
			Cache:            0,
			Handles:          0,
			ReadOnly:         true,
			NoFreezer:        true,
		})
		if dbutil.IsNotExistError(err) {
			log.Warn("Classic Msg Database not found", "err", err)
			classicOutbox = nil
		} else if err != nil {
			return nil, fmt.Errorf("Failed to open classic-msg database: %w", err)
		} else {
			if err := dbutil.UnfinishedConversionCheck(classicMsgDB); err != nil {
				return nil, fmt.Errorf("classic-msg unfinished database conversion check error: %w", err)
			}
			classicOutbox = NewClassicOutboxRetriever(classicMsgDB)
		}
	}

	bulkBlockMetadataFetcher := NewBulkBlockMetadataFetcher(l2BlockChain, execEngine, config.BlockMetadataApiCacheSize, config.BlockMetadataApiBlocksLimit)

	execNode := &ExecutionNode{
		ExecutionDB:              executionDB,
		Backend:                  backend,
		FilterSystem:             filterSystem,
		ArbInterface:             arbInterface,
		ExecEngine:               execEngine,
		Recorder:                 recorder,
		Sequencer:                sequencer,
		TxPreChecker:             txPreChecker,
		TxPublisher:              txPublisher,
		configFetcher:            configFetcher,
		SyncMonitor:              syncMon,
		ParentChainReader:        parentChainReader,
		ClassicOutbox:            classicOutbox,
		bulkBlockMetadataFetcher: bulkBlockMetadataFetcher,
	}

	if config.ConsensusRPCClient.URL != "" {
		consensusConfigFetcher := func() *rpcclient.ClientConfig { return &config.ConsensusRPCClient }
		execNode.consensusRPCClient = consensusrpcclient.NewConsensusRPCClient(consensusConfigFetcher, stack)
	}

	apis := []rpc.API{{
		Namespace: "arb",
		Version:   "1.0",
		Service:   NewArbAPI(txPublisher, bulkBlockMetadataFetcher, execEngine),
		Public:    false,
	}}
	apis = append(apis, rpc.API{
		Namespace:     "auctioneer",
		Version:       "1.0",
		Service:       NewArbTimeboostAuctioneerAPI(txPublisher),
		Public:        false,
		Authenticated: false,
	})
	apis = append(apis, rpc.API{
		Namespace: "timeboost",
		Version:   "1.0",
		Service:   NewArbTimeboostAPI(txPublisher),
		Public:    false,
	})
	apis = append(apis, rpc.API{
		Namespace: "arbdebug",
		Version:   "1.0",
		Service: NewArbDebugAPI(
			l2BlockChain,
			config.RPC.ArbDebug.BlockRangeBound,
			config.RPC.ArbDebug.TimeoutQueueBound,
		),
		Public: false,
	})
	apis = append(apis, rpc.API{
		Namespace: "arbtrace",
		Version:   "1.0",
		Service: NewArbTraceForwarderAPI(
			l2BlockChain.Config(),
			config.RPC.ClassicRedirect,
			config.RPC.ClassicRedirectTimeout,
		),
		Public: false,
	})
	apis = append(apis, rpc.API{
		Namespace: "debug",
		Service:   eth.NewDebugAPI(eth.NewArbEthereum(l2BlockChain, executionDB)),
		Public:    false,
	})
	if config.RPCServer.Enable {
		apis = append(apis, rpc.API{
			Namespace:     execution.RPCNamespace,
			Version:       "1.0",
			Service:       executionrpcserver.NewServer(execNode, execNode),
			Public:        config.RPCServer.Public,
			Authenticated: config.RPCServer.Authenticated,
		})
	}

	stack.RegisterAPIs(apis)

	return execNode, nil
}

func (n *ExecutionNode) MarkFeedStart(to arbutil.MessageIndex) containers.PromiseInterface[struct{}] {
	n.ExecEngine.MarkFeedStart(to)
	return containers.NewReadyPromise(struct{}{}, nil)
}

func (n *ExecutionNode) Initialize(ctx context.Context) error {
	config := n.configFetcher.Get()
	err := n.ExecEngine.Initialize(config.Caching.StylusLRUCacheCapacity, &config.StylusTarget)
	if err != nil {
		return fmt.Errorf("error initializing execution engine: %w", err)
	}
	n.ArbInterface.Initialize(n)
	err = n.Backend.Start()
	if err != nil {
		return fmt.Errorf("error starting geth backend: %w", err)
	}
	err = n.TxPublisher.Initialize(ctx)
	if err != nil {
		return fmt.Errorf("error initializing transaction publisher: %w", err)
	}
	err = n.Backend.APIBackend().SetSyncBackend(n.SyncMonitor)
	if err != nil {
		return fmt.Errorf("error setting sync backend: %w", err)
	}

	return nil
}

// not thread safe
func (n *ExecutionNode) Start(ctxIn context.Context) error {
	n.StopWaiter.Start(ctxIn, n)
	ctx, err := n.GetContextSafe()
	if err != nil {
		return err
	}

	if n.started.Swap(true) {
		return errors.New("already started")
	}
	if n.consensusRPCClient != nil {
		if err := n.consensusRPCClient.Start(ctx); err != nil {
			return fmt.Errorf("error starting consensus rpc client: %w", err)
		}
	}

	err = n.ExecEngine.Start(ctx)
	if err != nil {
		return fmt.Errorf("error starting execution engine: %w", err)
	}

	err = n.TxPublisher.Start(ctx)
	if err != nil {
		return fmt.Errorf("error starting transaction puiblisher: %w", err)
	}
	if n.ParentChainReader != nil {
		n.ParentChainReader.Start(ctx)
	}
	n.bulkBlockMetadataFetcher.Start(ctx)
	return nil
}

func (n *ExecutionNode) StopAndWait() {
	if !n.started.Load() {
		return
	}
	n.bulkBlockMetadataFetcher.StopAndWait()
	// TODO after separation
	// n.Stack.StopRPC() // does nothing if not running
	if n.TxPublisher.Started() {
		n.TxPublisher.StopAndWait()
	}
	n.Recorder.OrderlyShutdown()
	if n.ParentChainReader != nil && n.ParentChainReader.Started() {
		n.ParentChainReader.StopAndWait()
	}
	if n.ExecEngine.Started() {
		n.ExecEngine.StopAndWait()
	}
	if n.consensusRPCClient != nil {
		n.consensusRPCClient.StopAndWait()
	}
	n.ArbInterface.BlockChain().Stop() // does nothing if not running
	if err := n.Backend.Stop(); err != nil {
		log.Error("backend stop", "err", err)
	}
	// TODO after separation
	// if err := n.Stack.Close(); err != nil {
	// 	log.Error("error on stak close", "err", err)
	// }
	n.StopWaiter.StopAndWait()
}

func (n *ExecutionNode) DigestMessage(num arbutil.MessageIndex, msg *arbostypes.MessageWithMetadata, msgForPrefetch *arbostypes.MessageWithMetadata) containers.PromiseInterface[*execution.MessageResult] {
	return containers.NewReadyPromise(n.ExecEngine.DigestMessage(num, msg, msgForPrefetch))
}
func (n *ExecutionNode) Reorg(newHeadMsgIdx arbutil.MessageIndex, newMessages []arbostypes.MessageWithMetadataAndBlockInfo, oldMessages []*arbostypes.MessageWithMetadata) containers.PromiseInterface[[]*execution.MessageResult] {
	return containers.NewReadyPromise(n.ExecEngine.Reorg(newHeadMsgIdx, newMessages, oldMessages))
}
func (n *ExecutionNode) HeadMessageIndex() containers.PromiseInterface[arbutil.MessageIndex] {
	return containers.NewReadyPromise(n.ExecEngine.HeadMessageIndex())
}
func (n *ExecutionNode) NextDelayedMessageNumber() (uint64, error) {
	return n.ExecEngine.NextDelayedMessageNumber()
}
func (n *ExecutionNode) SequenceDelayedMessage(message *arbostypes.L1IncomingMessage, delayedSeqNum uint64) error {
	return n.ExecEngine.SequenceDelayedMessage(message, delayedSeqNum)
}
func (n *ExecutionNode) IsTxHashInOnchainFilter(txHash common.Hash) (bool, error) {
	return n.ExecEngine.IsTxHashInOnchainFilter(txHash)
}
func (n *ExecutionNode) ResultAtMessageIndex(msgIdx arbutil.MessageIndex) containers.PromiseInterface[*execution.MessageResult] {
	return containers.NewReadyPromise(n.ExecEngine.ResultAtMessageIndex(msgIdx))
}
func (n *ExecutionNode) ArbOSVersionForMessageIndex(msgIdx arbutil.MessageIndex) containers.PromiseInterface[uint64] {
	return n.ExecEngine.ArbOSVersionForMessageIndex(msgIdx)
}

func (n *ExecutionNode) RecordBlockCreation(
	pos arbutil.MessageIndex,
	msg *arbostypes.MessageWithMetadata,
	wasmTargets []rawdb.WasmTarget,
) containers.PromiseInterface[*execution.RecordResult] {
	return stopwaiter.LaunchPromiseThread(n, func(ctx context.Context) (*execution.RecordResult, error) {
		return n.Recorder.RecordBlockCreation(ctx, pos, msg, wasmTargets)
	})
}

func (n *ExecutionNode) PrepareForRecord(start, end arbutil.MessageIndex) containers.PromiseInterface[struct{}] {
	return stopwaiter.LaunchPromiseThread(n, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, n.Recorder.PrepareForRecord(ctx, start, end)
	})
}

func (n *ExecutionNode) Pause() {
	if n.Sequencer != nil {
		n.Sequencer.Pause()
	}
}

func (n *ExecutionNode) Activate() {
	if n.Sequencer != nil {
		n.Sequencer.Activate()
	}
}

func (n *ExecutionNode) ForwardTo(url string) error {
	if n.Sequencer != nil {
		return n.Sequencer.ForwardTo(url)
	} else {
		return errors.New("forwardTo not supported - sequencer not active")
	}
}

func (n *ExecutionNode) SetConsensusClient(consensus consensus.FullConsensusClient) {
	if n.consensusRPCClient != nil {
		consensus = n.consensusRPCClient
	}
	n.ExecEngine.SetConsensus(consensus)
	n.SyncMonitor.SetConsensusInfo(consensus)
}

func (n *ExecutionNode) MessageIndexToBlockNumber(messageNum arbutil.MessageIndex) containers.PromiseInterface[uint64] {
	blockNum := n.ExecEngine.MessageIndexToBlockNumber(messageNum)
	return containers.NewReadyPromise(blockNum, nil)
}
func (n *ExecutionNode) BlockNumberToMessageIndex(blockNum uint64) containers.PromiseInterface[arbutil.MessageIndex] {
	return containers.NewReadyPromise(n.ExecEngine.BlockNumberToMessageIndex(blockNum))
}

func (n *ExecutionNode) ShouldTriggerMaintenance() containers.PromiseInterface[bool] {
	return containers.NewReadyPromise(n.ExecEngine.ShouldTriggerMaintenance(n.configFetcher.Get().Caching.TrieTimeLimitBeforeFlushMaintenance), nil)
}
func (n *ExecutionNode) MaintenanceStatus() containers.PromiseInterface[*execution.MaintenanceStatus] {
	return containers.NewReadyPromise(n.ExecEngine.MaintenanceStatus(), nil)
}

func (n *ExecutionNode) TriggerMaintenance() containers.PromiseInterface[struct{}] {
	trieCapLimitBytes := arbmath.SaturatingUMul(uint64(n.configFetcher.Get().Caching.TrieCapLimit), 1024*1024)
	n.ExecEngine.TriggerMaintenance(trieCapLimitBytes)
	return containers.NewReadyPromise(struct{}{}, nil)
}

func (n *ExecutionNode) Synced(ctx context.Context) bool {
	return n.SyncMonitor.Synced(ctx)
}

func (n *ExecutionNode) FullSyncProgressMap(ctx context.Context) map[string]interface{} {
	return n.SyncMonitor.FullSyncProgressMap(ctx)
}

func (n *ExecutionNode) SetFinalityData(
	safeFinalityData *arbutil.FinalityData,
	finalizedFinalityData *arbutil.FinalityData,
	validatedFinalityData *arbutil.FinalityData,
) containers.PromiseInterface[struct{}] {
	err := n.SyncMonitor.SetFinalityData(n.ExecutionDB, safeFinalityData, finalizedFinalityData, validatedFinalityData)
	if err != nil {
		return containers.NewReadyPromise(struct{}{}, err)
	}
	if n.Recorder != nil && validatedFinalityData != nil {
		n.Recorder.MarkValid(validatedFinalityData.MsgIdx, validatedFinalityData.BlockHash)
	}
	return containers.NewReadyPromise(struct{}{}, nil)
}

func (n *ExecutionNode) SetConsensusSyncData(syncData *execution.ConsensusSyncData) containers.PromiseInterface[struct{}] {
	n.SyncMonitor.SetConsensusSyncData(syncData)
	return containers.NewReadyPromise(struct{}{}, nil)
}

func (n *ExecutionNode) InitializeTimeboost(ctx context.Context, chainConfig *params.ChainConfig) error {
	execNodeConfig := n.configFetcher.Get()
	if execNodeConfig.Sequencer.Timeboost.Enable {
		auctionContractAddr := common.HexToAddress(execNodeConfig.Sequencer.Timeboost.AuctionContractAddress)

		auctionContract, err := NewExpressLaneAuctionFromInternalAPI(
			n.Backend.APIBackend(),
			n.FilterSystem,
			auctionContractAddr)
		if err != nil {
			return err
		}

		roundTimingInfo, err := GetRoundTimingInfo(auctionContract)
		if err != nil {
			return err
		}

		var isActiveFunc func() bool
		if n.Sequencer != nil {
			isActiveFunc = func() bool {
				pause, forwarder := n.Sequencer.GetPauseAndForwarder()
				return pause == nil && forwarder == nil
			}
		}

		expressLaneTracker, err := NewExpressLaneTracker(
			*roundTimingInfo,
			execNodeConfig.Sequencer.MaxBlockSpeed,
			n.Backend.APIBackend(),
			auctionContract,
			auctionContractAddr,
			chainConfig,
			uint64(execNodeConfig.Sequencer.MaxTxDataSize), // #nosec G115
			execNodeConfig.Sequencer.Timeboost.EarlySubmissionGrace,
			isActiveFunc,
		)
		if err != nil {
			return fmt.Errorf("error creating express lane tracker: %w", err)
		}

		n.TxPreChecker.SetExpressLaneTracker(expressLaneTracker)

		if execNodeConfig.Sequencer.Enable {
			err := n.Sequencer.InitializeExpressLaneService(
				common.HexToAddress(execNodeConfig.Sequencer.Timeboost.AuctioneerAddress),
				roundTimingInfo,
				expressLaneTracker,
			)
			if err != nil {
				log.Error("failed to create express lane service", "err", err)
			}
			n.Sequencer.StartExpressLaneService(ctx)
		}

		expressLaneTracker.Start(ctx)
	}

	return nil
}

// new

func parseAggregationType(s string) (endorsementpolicy.AggregationType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bls":
		return endorsementpolicy.AggregationBLS, nil
	case "individual":
		return endorsementpolicy.AggregationIndividualSignatures, nil
	case "bitmap":
		return endorsementpolicy.AggregationBitmapSignatures, nil
	case "commitment-only":
		return endorsementpolicy.AggregationCommitmentOnly, nil
	default:
		return 0, fmt.Errorf("unknown aggregation type %q", s)
	}
}

func buildDefaultEndorsementPolicies(
	defaultThreshold uint32,
	strictThreshold uint32,
	defaultAgg endorsementpolicy.AggregationType,
	strictAgg endorsementpolicy.AggregationType,
) (
	defaultPolicy *endorsementpolicy.EndorsementPolicy,
	strictPolicy *endorsementpolicy.EndorsementPolicy,
) {
	defaultPolicy = &endorsementpolicy.EndorsementPolicy{
		ID: "default",
		Endorsers: endorsementpolicy.EndorserSet{
			Members: []endorsementpolicy.EndorserMember{
				{ID: "A"},
				{ID: "B"},
				{ID: "C"},
			},
		},
		Threshold:       defaultThreshold,
		FailMode:        endorsementpolicy.EndorsementFailDropTxAndRebuild,
		AggregationType: defaultAgg,
	}

	strictPolicy = &endorsementpolicy.EndorsementPolicy{
		ID: "strict",
		Endorsers: endorsementpolicy.EndorserSet{
			Members: []endorsementpolicy.EndorserMember{
				{ID: "A"},
				{ID: "B"},
				{ID: "C"},
			},
		},
		Threshold:       strictThreshold,
		FailMode:        endorsementpolicy.EndorsementFailDropTxAndRebuild,
		AggregationType: strictAgg,
	}

	return defaultPolicy, strictPolicy
}

func buildBLSPublicKeyRegistryFromConfig(
	cfg EndorsementExperimentConfig,
) (*endorsement.InMemoryBLSPublicKeyRegistry, error) {
	reg := endorsement.NewInMemoryBLSPublicKeyRegistry()

	pubkeys := map[endorsementpolicy.EndorserID]string{
		"A": cfg.EndorserAPubKeyHex,
		"B": cfg.EndorserBPubKeyHex,
		"C": cfg.EndorserCPubKeyHex,
	}

	for id, pubHex := range pubkeys {
		pubBytes, err := hex.DecodeString(strings.TrimSpace(pubHex))
		if err != nil {
			return nil, fmt.Errorf("failed to decode BLS public key hex for endorser %s: %w", id, err)
		}
		if err := reg.RegisterPublicKey(id, pubBytes); err != nil {
			return nil, fmt.Errorf("failed to register BLS public key for endorser %s: %w", id, err)
		}
	}
	return reg, nil
}
