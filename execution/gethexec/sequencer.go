// Copyright 2021-2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package gethexec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/arbitrum"
	"github.com/ethereum/go-ethereum/arbitrum_types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/arbnode/parent"
	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbosState"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/l1pricing"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/execution/gethexec/addressfilter"
	"github.com/offchainlabs/nitro/execution/gethexec/eventfilter"
	"github.com/offchainlabs/nitro/timeboost"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/headerreader"
	"github.com/offchainlabs/nitro/util/rpcclient"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	// new
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

var (
	sequencerBacklogGauge                   = metrics.NewRegisteredGauge("arb/sequencer/backlog", nil)
	sequencerQueueGauge                     = metrics.NewRegisteredGauge("arb/sequencer/queue/length", nil)
	sequencerQueueHistogram                 = metrics.NewRegisteredHistogram("arb/sequencer/queue/histogram", nil, metrics.NewBoundedHistogramSample())
	nonceCacheHitCounter                    = metrics.NewRegisteredCounter("arb/sequencer/noncecache/hit", nil)
	nonceCacheMissCounter                   = metrics.NewRegisteredCounter("arb/sequencer/noncecache/miss", nil)
	nonceCacheRejectedCounter               = metrics.NewRegisteredCounter("arb/sequencer/noncecache/rejected", nil)
	nonceCacheClearedCounter                = metrics.NewRegisteredCounter("arb/sequencer/noncecache/cleared", nil)
	nonceFailureCacheSizeGauge              = metrics.NewRegisteredGauge("arb/sequencer/noncefailurecache/size", nil)
	nonceFailureCacheOverflowCounter        = metrics.NewRegisteredCounter("arb/sequencer/noncefailurecache/overflow", nil)
	blockCreationTimer                      = metrics.NewRegisteredHistogram("arb/sequencer/block/creation", nil, metrics.NewBoundedHistogramSample())
	successfulBlocksCounter                 = metrics.NewRegisteredCounter("arb/sequencer/block/successful", nil)
	blockTxSizeHistogram                    = metrics.NewRegisteredHistogram("arb/sequencer/block/txsize", nil, metrics.NewBoundedHistogramSample())
	txSizeHistogram                         = metrics.NewRegisteredHistogram("arb/sequencer/transactions/txsize", nil, metrics.NewBoundedHistogramSample())
	conditionalTxRejectedBySequencerCounter = metrics.NewRegisteredCounter("arb/sequencer/conditionaltx/rejected", nil)
	conditionalTxAcceptedBySequencerCounter = metrics.NewRegisteredCounter("arb/sequencer/conditionaltx/accepted", nil)
	l1GasPriceGauge                         = metrics.NewRegisteredGauge("arb/sequencer/l1gasprice", nil)
	callDataUnitsBacklogGauge               = metrics.NewRegisteredGauge("arb/sequencer/calldataunitsbacklog", nil)
	currentSurplusGauge                     = metrics.NewRegisteredGauge("arb/sequencer/currentsurplus", nil)
	expectedSurplusGauge                    = metrics.NewRegisteredGauge("arb/sequencer/expectedsurplus", nil)
	waitForTxHistogram                      = metrics.NewRegisteredHistogram("arb/sequencer/waitfortx", nil, metrics.NewBoundedHistogramSample())
	// 区块结束原因统计
	// number of blocks ended because of block gas limit at least one tx wasn't included in block because of gas limit)
	gasLimitedBlocksCounter = metrics.NewRegisteredCounter("arb/sequencer/block/gaslimited", nil)
	// number of blocks ended because of txes data size limit
	dataLimitedBlocksCounter = metrics.NewRegisteredCounter("arb/sequencer/block/datalimited", nil)
	// number of blocks ended because of exhausting the transactions to sequence
	txExhaustedBlocksCounter = metrics.NewRegisteredCounter("arb/sequencer/block/txexhausted", nil)
)

// SequencerConfig 定义 sequencer 的运行参数，包括出块节奏、队列大小、过滤规则和 timeboost 等配置。
type SequencerConfig struct {
	Enable bool `koanf:"enable"`
	// 最小出块间隔
	MaxBlockSpeed                time.Duration              `koanf:"max-block-speed" reload:"hot"`
	ReadFromTxQueueTimeout       time.Duration              `koanf:"read-from-tx-queue-timeout" reload:"hot"`
	MaxRevertGasReject           uint64                     `koanf:"max-revert-gas-reject" reload:"hot"`
	MaxAcceptableTimestampDelta  time.Duration              `koanf:"max-acceptable-timestamp-delta" reload:"hot"`
	SenderWhitelist              []string                   `koanf:"sender-whitelist"`
	Forwarder                    ForwarderConfig            `koanf:"forwarder"`
	QueueSize                    int                        `koanf:"queue-size"`
	QueueTimeout                 time.Duration              `koanf:"queue-timeout" reload:"hot"`
	NonceCacheSize               int                        `koanf:"nonce-cache-size" reload:"hot"`
	MaxTxDataSize                int                        `koanf:"max-tx-data-size" reload:"hot"`
	NonceFailureCacheSize        int                        `koanf:"nonce-failure-cache-size" reload:"hot"`
	NonceFailureCacheExpiry      time.Duration              `koanf:"nonce-failure-cache-expiry" reload:"hot"`
	ExpectedSurplusGasPriceMode  string                     `koanf:"expected-surplus-gas-price-mode"`
	ExpectedSurplusSoftThreshold string                     `koanf:"expected-surplus-soft-threshold" reload:"hot"`
	ExpectedSurplusHardThreshold string                     `koanf:"expected-surplus-hard-threshold" reload:"hot"`
	EnableProfiling              bool                       `koanf:"enable-profiling" reload:"hot"`
	Timeboost                    TimeboostConfig            `koanf:"timeboost"`
	Dangerous                    DangerousConfig            `koanf:"dangerous"`
	TransactionFiltering         TransactionFilteringConfig `koanf:"transaction-filtering" reload:"hot"`
	expectedSurplusSoftThreshold int
	expectedSurplusHardThreshold int

	// 实验用途：拿到第一笔交易后，再额外等待这段时间收集更多交易
	ExperimentalBatchingWindow time.Duration `koanf:"experimental-batching-window" reload:"hot"`
}

// TransactionFilteringConfig 描述交易过滤相关的本地规则与外部 RPC 过滤器配置。
type TransactionFilteringConfig struct {
	EventFilter                  eventfilter.EventFilterConfig `koanf:"event-filter"`
	AddressFilter                addressfilter.Config          `koanf:"address-filter" reload:"hot"`
	TransactionFiltererRPCClient rpcclient.ClientConfig        `koanf:"transaction-filterer-rpc-client" reload:"hot"`
}

// Validate 校验交易过滤配置是否合法。
func (c *TransactionFilteringConfig) Validate() error {
	if err := c.EventFilter.Validate(); err != nil {
		return fmt.Errorf("invalid event filter config: %w", err)
	}
	if err := c.AddressFilter.Validate(); err != nil {
		return fmt.Errorf("error validating address-filter config: %w", err)
	}
	if err := c.TransactionFiltererRPCClient.Validate(); err != nil {
		return fmt.Errorf("error validating transaction-filterer-rpc-client config: %w", err)
	}
	return nil
}

var DefaultTransactionFilteringConfig = TransactionFilteringConfig{
	EventFilter:                  eventfilter.DefaultEventFilterConfig,
	AddressFilter:                addressfilter.DefaultConfig,
	TransactionFiltererRPCClient: DefaultTransactionFiltererRPCClientConfig,
}

// TransactionFilteringConfigAddOptions 向命令行参数集中注册交易过滤配置项。
func TransactionFilteringConfigAddOptions(prefix string, f *pflag.FlagSet) {
	EventFilterAddOptions(prefix+".event-filter", f)
	addressfilter.ConfigAddOptions(prefix+".address-filter", f)
	rpcclient.RPCClientAddOptions(prefix+".transaction-filterer-rpc-client", f, &DefaultTransactionFilteringConfig.TransactionFiltererRPCClient)
}

// DangerousConfig 保存会放宽安全检查的危险开关。
type DangerousConfig struct {
	DisableSeqInboxMaxDataSizeCheck bool `koanf:"disable-seq-inbox-max-data-size-check"`
	DisableBlobBaseFeeCheck         bool `koanf:"disable-blob-base-fee-check"`
}

// TimeboostConfig 定义 timeboost 快速通道和拍卖相关的配置。
type TimeboostConfig struct {
	Enable                       bool          `koanf:"enable"`
	AuctionContractAddress       string        `koanf:"auction-contract-address"`
	AuctioneerAddress            string        `koanf:"auctioneer-address"`
	ExpressLaneAdvantage         time.Duration `koanf:"express-lane-advantage"`
	SequencerHTTPEndpoint        string        `koanf:"sequencer-http-endpoint"`
	EarlySubmissionGrace         time.Duration `koanf:"early-submission-grace"`
	MaxFutureSequenceDistance    uint64        `koanf:"max-future-sequence-distance"`
	RedisUrl                     string        `koanf:"redis-url"`
	RedisUpdateEventsChannelSize uint64        `koanf:"redis-update-events-channel-size"`
	QueueTimeoutInBlocks         uint64        `koanf:"queue-timeout-in-blocks"`
}

var DefaultTimeboostConfig = TimeboostConfig{
	Enable:                       false,
	AuctionContractAddress:       "",
	AuctioneerAddress:            "",
	ExpressLaneAdvantage:         time.Millisecond * 200,
	SequencerHTTPEndpoint:        "http://localhost:8547",
	EarlySubmissionGrace:         time.Second * 2,
	MaxFutureSequenceDistance:    1000,
	RedisUrl:                     "unset",
	RedisUpdateEventsChannelSize: 500,
	QueueTimeoutInBlocks:         5,
}

// Validate 校验 sequencer 配置，并解析依赖于字符串输入的阈值参数。
func (c *SequencerConfig) Validate() error {
	for _, address := range c.SenderWhitelist {
		if len(address) == 0 {
			continue
		}
		if !common.IsHexAddress(address) {
			return fmt.Errorf("sequencer sender whitelist entry \"%v\" is not a valid address", address)
		}
	}
	if c.ExpectedSurplusGasPriceMode != "CalldataPrice" &&
		c.ExpectedSurplusGasPriceMode != "BlobPrice" &&
		c.ExpectedSurplusGasPriceMode != "CalldataPrice7623" {
		return fmt.Errorf("undefined expected-surplus-gas-price-mode: %s", c.ExpectedSurplusGasPriceMode)
	}

	var err error
	if c.ExpectedSurplusSoftThreshold != "default" {
		if c.expectedSurplusSoftThreshold, err = strconv.Atoi(c.ExpectedSurplusSoftThreshold); err != nil {
			return fmt.Errorf("invalid expected-surplus-soft-threshold value provided in sequencer config %w", err)
		}
	}
	if c.ExpectedSurplusHardThreshold != "default" {
		if c.expectedSurplusHardThreshold, err = strconv.Atoi(c.ExpectedSurplusHardThreshold); err != nil {
			return fmt.Errorf("invalid expected-surplus-hard-threshold value provided in sequencer config %w", err)
		}
	}
	if c.expectedSurplusSoftThreshold < c.expectedSurplusHardThreshold {
		return errors.New("expected-surplus-soft-threshold cannot be lower than expected-surplus-hard-threshold")
	}
	maxTxDataSize := uint64(c.MaxTxDataSize) // #nosec G115
	if err := ValidateMaxTxDataSize(maxTxDataSize); err != nil {
		return err
	}
	if c.Timeboost.Enable {
		if len(c.Timeboost.AuctionContractAddress) > 0 && !common.IsHexAddress(c.Timeboost.AuctionContractAddress) {
			return fmt.Errorf("invalid timeboost.auction-contract-address \"%v\"", c.Timeboost.AuctionContractAddress)
		}
		if c.Enable {
			if c.Timeboost.RedisUrl == DefaultTimeboostConfig.RedisUrl {
				return errors.New("timeboost is enabled but no redis-url was set")
			}
			if c.Timeboost.MaxFutureSequenceDistance == 0 {
				return errors.New("timeboost max-future-sequence-distance option cannot be zero, it should be set to a positive value")
			}
			if len(c.Timeboost.AuctioneerAddress) > 0 && !common.IsHexAddress(c.Timeboost.AuctioneerAddress) {
				return fmt.Errorf("invalid timeboost.auctioneer-address \"%v\"", c.Timeboost.AuctioneerAddress)
			}
		}
	}
	if c.ReadFromTxQueueTimeout >= c.MaxBlockSpeed {
		log.Warn("Sequencer ReadFromTxQueueTimeout is higher than MaxBlockSpeed", "ReadFromTxQueueTimeout", c.ReadFromTxQueueTimeout, "MaxBlockSpeed", c.MaxBlockSpeed)
	}

	if err := c.TransactionFiltering.Validate(); err != nil {
		return err
	}

	return nil
}

// ValidateMaxTxDataSize 检查单笔交易允许的最大数据长度是否超出 Nitro 可接受的上限。
func ValidateMaxTxDataSize(maxTxDataSize uint64) error {
	// tighter limit https://github.com/OffchainLabs/nitro/commit/ed015e752d7d24e59ec9e6f894fe1a26ffa19036
	// The default block gas limit can fit 1523 txs
	// Each Tx adds an 8-byte of length prefix.
	// 50K is enoguh to add 1523 times 8 bytes and still stay in an L2 message
	const maxConfigurableTxDataSize = arbostypes.MaxL2MessageSize - 50000
	if maxTxDataSize > maxConfigurableTxDataSize {
		return fmt.Errorf("max-tx-data-size %d exceeds maximum allowed value of %d", maxTxDataSize, maxConfigurableTxDataSize)
	}
	return nil
}

// SequencerConfigFetcher 返回当前生效的 sequencer 配置，便于支持热更新读取。
type SequencerConfigFetcher func() *SequencerConfig

var DefaultSequencerConfig = SequencerConfig{
	Enable:                      false,
	MaxBlockSpeed:               time.Second,            //time.Millisecond * 250,
	ReadFromTxQueueTimeout:      200 * time.Millisecond, //	time.Millisecond * 10,
	MaxRevertGasReject:          0,
	MaxAcceptableTimestampDelta: time.Hour,
	SenderWhitelist:             []string{},
	Forwarder:                   DefaultSequencerForwarderConfig,
	QueueSize:                   1024,
	QueueTimeout:                time.Second * 12,
	NonceCacheSize:              1024,
	// 95% of the default batch poster limit, leaving 5KB for headers and such
	// This default is overridden for L3 chains in applyChainParameters in cmd/nitro/nitro.go
	MaxTxDataSize:                95000,
	NonceFailureCacheSize:        1024,
	NonceFailureCacheExpiry:      time.Second,
	ExpectedSurplusGasPriceMode:  "BlobPrice",
	ExpectedSurplusSoftThreshold: "default",
	ExpectedSurplusHardThreshold: "default",
	EnableProfiling:              false,
	Timeboost:                    DefaultTimeboostConfig,
	Dangerous:                    DefaultDangerousConfig,
	TransactionFiltering:         DefaultTransactionFilteringConfig,

	// 实验默认值：拿到第一笔后再等 800ms
	ExperimentalBatchingWindow: 800 * time.Millisecond,
}

var DefaultDangerousConfig = DangerousConfig{
	DisableSeqInboxMaxDataSizeCheck: false,
}

// SequencerConfigAddOptions 向命令行参数集中注册 sequencer 的配置项。
func SequencerConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.Bool(prefix+".enable", DefaultSequencerConfig.Enable, "act and post to l1 as sequencer")
	f.Duration(prefix+".read-from-tx-queue-timeout", DefaultSequencerConfig.ReadFromTxQueueTimeout, "timeout for reading new messages")
	f.Duration(prefix+".max-block-speed", DefaultSequencerConfig.MaxBlockSpeed, "minimum delay between blocks (sets a maximum speed of block production)")
	f.Uint64(prefix+".max-revert-gas-reject", DefaultSequencerConfig.MaxRevertGasReject, "maximum gas executed in a revert for the sequencer to reject the transaction instead of posting it (anti-DOS)")
	f.Duration(prefix+".max-acceptable-timestamp-delta", DefaultSequencerConfig.MaxAcceptableTimestampDelta, "maximum acceptable time difference between the local time and the latest L1 block's timestamp")
	f.StringSlice(prefix+".sender-whitelist", DefaultSequencerConfig.SenderWhitelist, "comma separated whitelist of authorized senders (if empty, everyone is allowed)")
	AddOptionsForSequencerForwarderConfig(prefix+".forwarder", f)
	TimeboostAddOptions(prefix+".timeboost", f)

	DangerousAddOptions(prefix+".dangerous", f)
	f.Int(prefix+".queue-size", DefaultSequencerConfig.QueueSize, "size of the pending tx queue")
	f.Duration(prefix+".queue-timeout", DefaultSequencerConfig.QueueTimeout, "maximum amount of time transaction can wait in queue")
	f.Int(prefix+".nonce-cache-size", DefaultSequencerConfig.NonceCacheSize, "size of the tx sender nonce cache")
	f.Int(prefix+".max-tx-data-size", DefaultSequencerConfig.MaxTxDataSize, "maximum transaction size the sequencer will accept")
	f.Int(prefix+".nonce-failure-cache-size", DefaultSequencerConfig.NonceFailureCacheSize, "number of transactions with too high of a nonce to keep in memory while waiting for their predecessor")
	f.Duration(prefix+".nonce-failure-cache-expiry", DefaultSequencerConfig.NonceFailureCacheExpiry, "maximum amount of time to wait for a predecessor before rejecting a tx with nonce too high")
	f.String(prefix+".expected-surplus-gas-price-mode", DefaultSequencerConfig.ExpectedSurplusGasPriceMode, "gas price setting to be used in calculating estimated surplus. Allowed values- CalldataPrice, BlobPrice and CalldataPrice7523")
	f.String(prefix+".expected-surplus-soft-threshold", DefaultSequencerConfig.ExpectedSurplusSoftThreshold, "if expected surplus is lower than this value, warnings are posted")
	f.String(prefix+".expected-surplus-hard-threshold", DefaultSequencerConfig.ExpectedSurplusHardThreshold, "if expected surplus is lower than this value, new incoming transactions will be denied")
	f.Bool(prefix+".enable-profiling", DefaultSequencerConfig.EnableProfiling, "enable CPU profiling and tracing")
	TransactionFilteringConfigAddOptions(prefix+".transaction-filtering", f)

	// new
	f.Duration(
		prefix+".experimental-batching-window",
		DefaultSequencerConfig.ExperimentalBatchingWindow,
		"experimental extra wait after receiving the first tx, to aggregate more txs into the same candidate block",
	)
}

// TimeboostAddOptions 向命令行参数集中注册 timeboost 配置项。
func TimeboostAddOptions(prefix string, f *pflag.FlagSet) {
	f.Bool(prefix+".enable", DefaultTimeboostConfig.Enable, "enable timeboost based on express lane auctions")
	f.String(prefix+".auction-contract-address", DefaultTimeboostConfig.AuctionContractAddress, "Address of the proxy pointing to the ExpressLaneAuction contract")
	f.String(prefix+".auctioneer-address", DefaultTimeboostConfig.AuctioneerAddress, "Address of the Timeboost Autonomous Auctioneer")
	f.Duration(prefix+".express-lane-advantage", DefaultTimeboostConfig.ExpressLaneAdvantage, "specify the express lane advantage")
	f.String(prefix+".sequencer-http-endpoint", DefaultTimeboostConfig.SequencerHTTPEndpoint, "this sequencer's http endpoint")
	f.Duration(prefix+".early-submission-grace", DefaultTimeboostConfig.EarlySubmissionGrace, "period of time before the next round where submissions for the next round will be queued")
	f.Uint64(prefix+".max-future-sequence-distance", DefaultTimeboostConfig.MaxFutureSequenceDistance, "maximum allowed difference (in terms of sequence numbers) between a future express lane tx and the current sequence count of a round")
	f.String(prefix+".redis-url", DefaultTimeboostConfig.RedisUrl, "the Redis URL for expressLaneService to coordinate via")
	f.Uint64(prefix+".redis-update-events-channel-size", DefaultTimeboostConfig.RedisUpdateEventsChannelSize, "size of update events' buffered channels in timeboost redis coordinator")
	f.Uint64(prefix+".queue-timeout-in-blocks", DefaultTimeboostConfig.QueueTimeoutInBlocks, "maximum amount of time (measured in blocks) that Express Lane transactions can wait in the sequencer's queue")
}

// DangerousAddOptions 向命令行参数集中注册危险配置项。
func DangerousAddOptions(prefix string, f *pflag.FlagSet) {
	f.Bool(prefix+".disable-seq-inbox-max-data-size-check", DefaultDangerousConfig.DisableSeqInboxMaxDataSizeCheck, "DANGEROUS! disables nitro checks on sequencer MaxTxDataSize against the sequencer inbox MaxDataSize")
	f.Bool(prefix+".disable-blob-base-fee-check", DefaultDangerousConfig.DisableBlobBaseFeeCheck, "DANGEROUS! disables nitro checks on sequencer for blob base fee")
}

// EventFilterAddOptions 向命令行参数集中注册事件过滤器配置项。
func EventFilterAddOptions(prefix string, f *pflag.FlagSet) {
	f.String(prefix+".path", "", "path to JSON file containing event filter rules")
}

// txQueueItem 表示进入 sequencer 内部队列的一笔待排序交易。
// 它除了保存原始交易本身，还携带这笔交易在排队、超时控制、结果回传、
// 以及 timeboost 优先级调度中所需的全部上下文信息。
type txQueueItem struct {
	// tx 是实际要被 sequencer 排序和执行的以太坊交易对象。
	tx *types.Transaction
	// txSize 是交易编码后的字节大小，用于限制单笔交易大小和累计候选块的总交易大小。
	txSize int
	// options 保存条件交易的附加约束，例如要求特定 L1/L2 状态满足时才允许执行。
	options *arbitrum_types.ConditionalOptions
	// resultChan 用于把这笔交易最终的处理结果返回给提交方。
	// 成功时通常发送 nil，失败时发送对应错误，然后关闭该 channel。
	resultChan chan<- error
	// returnedResult 用于保证 resultChan 只被写入并关闭一次，避免重复返回结果。
	returnedResult *atomic.Bool
	// ctx 绑定这笔交易的生命周期，用于控制排队等待、取消提交和超时退出。
	ctx context.Context
	// firstAppearance 记录这笔交易第一次进入 sequencer 视角的时间，
	// 主要用于计算队列停留时间，以及 nonce failure 缓存的过期时刻。
	firstAppearance time.Time
	// isTimeboosted 标记该交易是否来自 timeboost/express lane 通道。
	// 这会影响它的排队优先级和超时处理逻辑。
	isTimeboosted bool
	// blockStamp 记录 timeboost 交易进入主队列时的区块高度快照。
	// sequencer 会结合它和 QueueTimeoutInBlocks 判断该交易是否已经等待过久。
	blockStamp uint64
}

// returnResultMaybeLog 向调用方返回一次处理结果，并在重复返回时按需记录日志。
func (i *txQueueItem) returnResultMaybeLog(err error, outputLog bool) {
	if i.returnedResult.Swap(true) {
		if outputLog {
			log.Error("attempting to return result to already finished queue item", "err", err)
		}
		return
	}
	i.resultChan <- err
	close(i.resultChan)
}

// returnResult 向调用方返回处理结果，不额外输出重复返回日志。
func (i *txQueueItem) returnResult(err error) {
	i.returnResultMaybeLog(err, false)
}

// nonceCache 缓存当前候选区块上下文下各账户的 nonce，减少重复访问状态树。
type nonceCache struct {
	cache *containers.LruCache[common.Address, uint64]
	block common.Hash
	dirty *types.Header
}

// newNonceCache 创建指定容量的 nonce 缓存。
func newNonceCache(size int) *nonceCache {
	return &nonceCache{
		cache: containers.NewLruCache[common.Address, uint64](size),
		// block 是 nonce 缓存当前关联的区块上下文的锚点，通常是正在构建的候选区块的父区块哈希。
		block: common.Hash{},
		// dirty 是正在构建的候选区块，如果不为 nil 则表示缓存中的 nonce 可能已经被更新但还未落地到新区块
		dirty: nil,
	}
}

// matches 判断缓存是否仍与当前待执行区块头匹配。
func (c *nonceCache) matches(header *types.Header) bool {
	if c.dirty != nil {
		// Note, even though the of the header changes, c.dirty points to the
		// same header, hence hashes will be the same and this check will pass.
		return headerreader.HeadersEqual(c.dirty, header)
	}
	return c.block == header.ParentHash
}

// Reset 清空 nonce 缓存并重置其关联的区块上下文。
func (c *nonceCache) Reset(block common.Hash) {
	if c.cache.Len() > 0 {
		nonceCacheClearedCounter.Inc(1)
	}
	c.cache.Clear()
	c.block = block
	c.dirty = nil
}

// BeginNewBlock 在开始构建新区块前准备 nonce 缓存状态。
func (c *nonceCache) BeginNewBlock() {
	if c.dirty != nil {
		c.Reset(common.Hash{})
	}
}

// Get 获取地址在当前区块上下文中的 nonce，未命中时回退到状态树查询。
func (c *nonceCache) Get(header *types.Header, statedb *state.StateDB, addr common.Address) uint64 {
	if !c.matches(header) {
		c.Reset(header.ParentHash)
	}
	nonce, ok := c.cache.Get(addr)
	if ok {
		nonceCacheHitCounter.Inc(1)
		return nonce
	}
	nonceCacheMissCounter.Inc(1)
	nonce = statedb.GetNonce(addr)
	c.cache.Add(addr, nonce)
	return nonce
}

// Update 更新地址在当前候选区块中的最新 nonce。
func (c *nonceCache) Update(header *types.Header, addr common.Address, nonce uint64) {
	if !c.matches(header) {
		c.Reset(header.ParentHash)
	}
	c.dirty = header
	c.cache.Add(addr, nonce)
}

// Finalize 在候选区块成功落地后，把缓存锚点推进到新区块。
func (c *nonceCache) Finalize(block *types.Block) {
	// Note: we don't use c.matches here because the header will have changed
	if c.block == block.ParentHash() {
		c.block = block.Hash()
		c.dirty = nil
	} else {
		c.Reset(block.Hash())
	}
}

// Caching 返回 nonce 缓存当前是否启用。
func (c *nonceCache) Caching() bool {
	return c.cache != nil && c.cache.Size() > 0
}

// Resize 调整 nonce 缓存容量。
func (c *nonceCache) Resize(newSize int) {
	c.cache.Resize(newSize)
}

// addressAndNonce 是 nonce 失败缓存的键，唯一标识某地址上的某个 nonce。
type addressAndNonce struct {
	address common.Address
	nonce   uint64
}

// nonceFailure 记录因 nonce 过高而暂存的交易及其过期信息。
type nonceFailure struct {
	queueItem txQueueItem
	nonceErr  error
	expiry    time.Time
	revived   bool
}

// nonceFailureCache 保存等待前序交易到达的 nonce 失败交易。
type nonceFailureCache struct {
	*containers.LruCache[addressAndNonce, *nonceFailure]
	getExpiry func() time.Duration
}

// Contains 判断给定 nonce 错误对应的交易是否已在失败缓存中。
func (c nonceFailureCache) Contains(err NonceError) bool {
	key := addressAndNonce{err.sender, err.txNonce}
	return c.LruCache.Contains(key)
}

// Add 将 nonce 过高的交易加入失败缓存，等待其前序交易出现。
func (c nonceFailureCache) Add(err NonceError, queueItem txQueueItem) {
	expiry := queueItem.firstAppearance.Add(c.getExpiry())
	// 如果交易已经在缓存中或者已经过期，则直接返回错误结果，不再加入缓存。
	if c.Contains(err) || time.Now().After(expiry) {
		queueItem.returnResult(err)
		return
	}
	key := addressAndNonce{err.sender, err.txNonce}
	val := &nonceFailure{
		queueItem: queueItem,
		nonceErr:  err,
		expiry:    expiry,
		revived:   false,
	}
	evicted := c.LruCache.Add(key, val)
	if evicted {
		nonceFailureCacheOverflowCounter.Inc(1)
	}
}

// synchronizedTxQueue 为重试队列提供线程安全的 push/pop 操作。
type synchronizedTxQueue struct {
	queue containers.Queue[txQueueItem]
	mutex sync.RWMutex
}

// Push 向同步队列尾部压入一个待处理项。
func (q *synchronizedTxQueue) Push(item txQueueItem) {
	q.mutex.Lock()
	q.queue.Push(item)
	q.mutex.Unlock()
}

// Pop 从同步队列头部弹出一个待处理项。
func (q *synchronizedTxQueue) Pop() txQueueItem {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	return q.queue.Pop()

}

// Len 返回同步队列当前长度。
func (q *synchronizedTxQueue) Len() int {
	q.mutex.RLock()
	defer q.mutex.RUnlock()
	return q.queue.Len()
}

// Sequencer 负责接收交易、筛选交易、构建候选区块并驱动执行引擎完成排序出块。
type Sequencer struct {
	stopwaiter.StopWaiter

	execEngine         *ExecutionEngine
	txQueue            chan txQueueItem
	txRetryQueue       synchronizedTxQueue
	l1Reader           *headerreader.HeaderReader
	config             SequencerConfigFetcher
	senderWhitelist    map[common.Address]struct{}
	nonceCache         *nonceCache
	nonceFailures      *nonceFailureCache
	expressLaneService *expressLaneService
	onForwarderSet     chan struct{}
	parentChain        *parent.ParentChain

	L1BlockAndTimeMutex sync.Mutex
	l1BlockNumber       atomic.Uint64
	l1Timestamp         uint64

	// activeMutex manages pauseChan (pauses execution) and forwarder
	// at most one of these is non-nil at any given time
	// both are nil for the active sequencer
	activeMutex sync.Mutex
	pauseChan   chan struct{}
	forwarder   *TxForwarder

	expectedSurplusMutex              sync.RWMutex
	expectedSurplus                   int64
	expectedSurplusUpdated            bool
	expectedSurplusFailureCount       int
	auctioneerAddr                    common.Address
	timeboostAuctionResolutionTxQueue chan txQueueItem

	eventFilter          *eventfilter.EventFilter
	addressFilterService *addressfilter.FilterService
	// new
	policyResolver endorsementpolicy.PolicyResolver
	policyConfig   *endorsementpolicy.PolicyConfig
}

// SetPolicyResolver 设置 endorsement policy 解析器，必须在 sequencer 启动前调用。
func (s *Sequencer) SetPolicyResolver(resolver endorsementpolicy.PolicyResolver) {
	if s.Started() {
		panic("trying to set policy resolver after start")
	}
	if s.policyResolver != nil {
		panic("trying to set policy resolver when already set")
	}
	s.policyResolver = resolver
}

// SetPolicyConfig 设置 endorsement policy 的运行参数，必须在 sequencer 启动前调用。
func (s *Sequencer) SetPolicyConfig(cfg *endorsementpolicy.PolicyConfig) {
	if s.Started() {
		panic("trying to set policy config after start")
	}
	if s.policyConfig != nil {
		panic("trying to set policy config when already set")
	}
	s.policyConfig = cfg
}

// NewSequencer 创建并初始化一个 Sequencer 实例及其依赖组件。
func NewSequencer(execEngine *ExecutionEngine, l1Reader *headerreader.HeaderReader, configFetcher SequencerConfigFetcher, parentChainId *big.Int) (*Sequencer, error) {
	config := configFetcher()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	senderWhitelist := make(map[common.Address]struct{})
	for _, address := range config.SenderWhitelist {
		if len(address) == 0 {
			continue
		}
		senderWhitelist[common.HexToAddress(address)] = struct{}{}
	}

	eventFilter, err := eventfilter.NewEventFilterFromConfig(config.TransactionFiltering.EventFilter)
	if err != nil {
		return nil, err
	}

	var addressFilterService *addressfilter.FilterService
	addressFilterService, err = addressfilter.NewFilterService(&config.TransactionFiltering.AddressFilter)
	if err != nil {
		return nil, fmt.Errorf("failed to create restricted addr service: %w", err)
	}

	if config.Enable && config.TransactionFiltering.TransactionFiltererRPCClient.URL != "" {
		filtererConfigFetcher := func() *rpcclient.ClientConfig {
			return &configFetcher().TransactionFiltering.TransactionFiltererRPCClient
		}
		transactionFiltererRPCClient := NewTransactionFiltererRPCClient(filtererConfigFetcher)
		execEngine.SetTransactionFiltererRPCClient(transactionFiltererRPCClient)
	}

	s := &Sequencer{
		execEngine:      execEngine,
		txQueue:         make(chan txQueueItem, config.QueueSize),
		l1Reader:        l1Reader,
		config:          configFetcher,
		senderWhitelist: senderWhitelist,
		nonceCache:      newNonceCache(config.NonceCacheSize),
		l1Timestamp:     0,
		pauseChan:       nil,
		onForwarderSet:  make(chan struct{}, 1),
		parentChain: &parent.ParentChain{
			ChainID:  parentChainId,
			L1Reader: l1Reader,
		},
		timeboostAuctionResolutionTxQueue: make(chan txQueueItem, 10), // There should never be more than 1 outstanding auction resolutions
		eventFilter:                       eventFilter,
		addressFilterService:              addressFilterService,
	}
	s.nonceFailures = &nonceFailureCache{
		containers.NewLruCacheWithOnEvict(config.NonceCacheSize, s.onNonceFailureEvict),
		func() time.Duration { return configFetcher().NonceFailureCacheExpiry },
	}
	s.Pause()
	execEngine.EnableReorgSequencing()
	execEngine.SetEventFilter(eventFilter)
	return s, nil
}

// onNonceFailureEvict 在 nonce 失败缓存项被驱逐时返回最终结果或尝试转发该交易。
func (s *Sequencer) onNonceFailureEvict(_ addressAndNonce, failure *nonceFailure) {
	if failure.revived {
		return
	}
	queueItem := failure.queueItem
	err := queueItem.ctx.Err()
	if err != nil {
		queueItem.returnResult(err)
		return
	}
	_, forwarder := s.GetPauseAndForwarder()
	if forwarder != nil {
		// We might not have gotten the predecessor tx because our forwarder did. Let's try there instead.
		// We run this in a background goroutine because LRU eviction needs to be quick.
		// We use an untracked thread for a few reasons:
		//   - It's guaranteed to run even when stopped (we need to return *some* result).
		//   - It acquires mutexes and this might need to happen a lot.
		//   - We don't need the context because queueItem has its own.
		//   - The RPC handler is on a separate StopWaiter anyways -- we should respect its context.
		s.LaunchUntrackedThread(func() {
			err = forwarder.PublishTransaction(queueItem.ctx, queueItem.tx, queueItem.options)
			queueItem.returnResult(err)
		})
	} else {
		queueItem.returnResult(failure.nonceErr)
	}
}

// ctxWithTimeout is like context.WithTimeout except a timeout of 0 means unlimited instead of instantly expired.
func ctxWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	// 如果 timeout 是 0，返回一个没有截止时间的 context，这样调用方就不需要担心 context 超时了。
	if timeout == time.Duration(0) {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// PublishTransaction 接收普通交易提交请求，并等待该交易得到排序结果。
func (s *Sequencer) PublishTransaction(parentCtx context.Context, tx *types.Transaction, options *arbitrum_types.ConditionalOptions) error {
	// 先检查当前节点是否处于 forward 模式。
	// 如果是，就优先把交易转发给目标 sequencer，避免本地重复排队。
	// 只有在对端明确返回 ErrNoSequencer 时，才回退到本地 sequencer 流程。
	_, forwarder := s.GetPauseAndForwarder()
	if forwarder != nil {
		err := forwarder.PublishTransaction(parentCtx, tx, options)
		if !errors.Is(err, ErrNoSequencer) {
			return err
		}
	}

	config := s.config()
	queueTimeout := config.QueueTimeout
	// queueCtx 控制交易在后台排队和等待处理的生命周期。
	// 对普通交易来说，这里额外叠加 ExpressLaneAdvantage，
	// 因为 express lane 激活时普通交易可能被主动延后这么久。
	queueCtx, cancelFunc := ctxWithTimeout(parentCtx, queueTimeout+config.Timeboost.ExpressLaneAdvantage)
	defer cancelFunc()

	// resultChan 用来接收后台排序线程最终返回的处理结果。
	resultChan := make(chan error, 1)
	// 把交易压入主队列。这里传入 false，表示它不是 express lane controller 的 timeboost 交易，
	// 因此如果当前轮次存在 express lane 控制者，它可能需要让出一段优势窗口。
	err := s.publishTransactionToQueue(queueCtx, tx, options, resultChan, false /* delay tx if express lane is active */)
	if err != nil {
		return err
	}

	now := time.Now()
	// abortCtx 是前台请求侧更宽松的一层总超时保护。
	// queueCtx 只约束后台排队过程，而 abortCtx 防止调用方在异常情况下无限等待。
	abortCtx, cancel := ctxWithTimeout(parentCtx, queueTimeout*2)
	defer cancel()

	select {
	case res := <-resultChan:
		// 正常拿到后台处理结果，可能是成功(nil)也可能是明确的业务错误。
		return res
	case <-abortCtx.Done():
		// We use abortCtx here and not queueCtx, because the QueueTimeout only applies to the background queue.
		// We want to give the background queue as much time as possible to make a response.
		err := abortCtx.Err()
		if parentCtx.Err() == nil {
			// If we've hit the abort deadline (as opposed to parentCtx being canceled), something went wrong.
			log.Warn("Transaction sequencing hit abort deadline", "err", err, "submittedAt", now, "queueTimeout", queueTimeout*2, "txHash", tx.Hash())
		}
		return err
	}
}

// PublishAuctionResolutionTransaction 提交拍卖结算交易，并给予高优先级处理。
func (s *Sequencer) PublishAuctionResolutionTransaction(ctx context.Context, tx *types.Transaction) error {
	if !s.config().Timeboost.Enable {
		return errors.New("timeboost not enabled")
	}

	forwarder, err := s.getForwarder(ctx)
	if err != nil {
		return err
	}
	if forwarder != nil {
		err := forwarder.PublishAuctionResolutionTransaction(ctx, tx)
		if !errors.Is(err, ErrNoSequencer) {
			return err
		}
	}

	arrivalTime := time.Now()
	auctioneerAddr := s.auctioneerAddr
	if auctioneerAddr == (common.Address{}) {
		return errors.New("invalid auctioneer address")
	}
	if tx.To() == nil {
		return errors.New("transaction has no recipient")
	}
	if *tx.To() != s.expressLaneService.AuctionContractAddr() {
		return fmt.Errorf("transaction recipient %#x is not the auction contract %#x", *tx.To(), s.expressLaneService.AuctionContractAddr())
	}
	signer := types.LatestSigner(s.execEngine.bc.Config())
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return err
	}
	if sender != auctioneerAddr {
		return fmt.Errorf("sender %#x is not the auctioneer address %#x", sender, auctioneerAddr)
	}
	if !s.expressLaneService.roundTimingInfo.IsWithinAuctionCloseWindow(arrivalTime) {
		return fmt.Errorf("transaction arrival time not within auction closure window: %v", arrivalTime)
	}
	log.Info("Prioritizing auction resolution transaction from auctioneer", "txHash", tx.Hash().Hex())
	s.timeboostAuctionResolutionTxQueue <- txQueueItem{
		tx:              tx,
		txSize:          int(tx.Size()), // #nosec G115
		options:         nil,
		resultChan:      make(chan error, 1),
		returnedResult:  &atomic.Bool{},
		ctx:             s.GetContext(),
		firstAppearance: time.Now(),
		isTimeboosted:   false,
	}
	return nil
}

// PublishExpressLaneTransaction 提交 express lane 交易，并交给 timeboost 逻辑校验和排序。
func (s *Sequencer) PublishExpressLaneTransaction(ctx context.Context, msg *timeboost.ExpressLaneSubmission) error {
	if !s.config().Timeboost.Enable {
		return errors.New("timeboost not enabled")
	}

	forwarder, err := s.getForwarder(ctx)
	if err != nil {
		return err
	}
	if forwarder != nil {
		return forwarder.PublishExpressLaneTransaction(ctx, msg)
	}

	if s.expressLaneService == nil {
		return errors.New("express lane service not enabled")
	}
	if err := s.expressLaneService.ValidateExpressLaneTx(msg); err != nil {
		return err
	}

	forwarder, err = s.getForwarder(ctx)
	if err != nil {
		return err
	}
	if forwarder != nil {
		return forwarder.PublishExpressLaneTransaction(ctx, msg)
	}

	return s.expressLaneService.sequenceExpressLaneSubmission(msg)
}

// PublishTimeboostedTransaction 将由 express lane 控制器提交的交易压入 timeboost 队列。
func (s *Sequencer) PublishTimeboostedTransaction(queueCtx context.Context, tx *types.Transaction, options *arbitrum_types.ConditionalOptions) error {
	resultChan := make(chan error, 1)
	return s.publishTransactionToQueue(queueCtx, tx, options, resultChan, true)
}

// publishTransactionToQueue 在完成基础准入检查后，把交易封装成 txQueueItem 放入主队列。
func (s *Sequencer) publishTransactionToQueue(queueCtx context.Context, tx *types.Transaction, options *arbitrum_types.ConditionalOptions, resultChan chan error, isExpressLaneController bool) error {
	config := s.config()
	// 如果配置了预期剩余 gas 价格的硬阈值，并且当前已经更新过预期剩余 gas 价格且它低于该硬阈值，则拒绝接受新交易。
	if s.l1Reader != nil && config.ExpectedSurplusHardThreshold != "default" {
		s.expectedSurplusMutex.RLock()
		if s.expectedSurplusUpdated && s.expectedSurplus < int64(config.expectedSurplusHardThreshold) {
			return errors.New("currently not accepting transactions due to expected surplus being below threshold")
		}
		s.expectedSurplusMutex.RUnlock()
	}

	sequencerBacklogGauge.Inc(1)
	defer sequencerBacklogGauge.Dec(1)

	// 如果配置了发送者白名单，则在入队前先完成签名恢复并校验发送者身份。
	if len(s.senderWhitelist) > 0 {
		signer := types.LatestSigner(s.execEngine.bc.Config())
		sender, err := types.Sender(signer, tx)
		if err != nil {
			return err
		}
		_, authorized := s.senderWhitelist[sender]
		if !authorized {
			return errors.New("transaction sender is not on the whitelist")
		}
	}
	if tx.Type() >= types.ArbitrumDepositTxType || tx.Type() == types.BlobTxType {
		// Should be unreachable for Arbitrum types due to UnmarshalBinary not accepting Arbitrum internal txs
		// and we want to disallow BlobTxType since Arbitrum doesn't support EIP-4844 txs yet.
		return types.ErrTxTypeNotSupported
	}

	// 当 timeboost 生效且当前轮次已有 express lane 控制者时，
	// 普通交易需要主动等待一小段优势窗口，让优先通道交易先进入排序流程。
	if s.config().Timeboost.Enable && s.expressLaneService != nil {
		if !isExpressLaneController && s.expressLaneService.currentRoundHasController() {
			time.Sleep(s.config().Timeboost.ExpressLaneAdvantage)
		}
	}

	var blockStamp uint64
	// 对 express lane controller 提交的交易记录一个入队时的区块高度，
	// 后续 createBlock 会据此判断它是否已经在队列中等待了太多个区块。
	if isExpressLaneController && config.Timeboost.QueueTimeoutInBlocks > 0 {
		blockStamp = s.execEngine.bc.CurrentBlock().Number.Uint64()
	}

	// 封装成内部队列项，把交易内容、条件参数、回包通道、上下文和 timeboost 元信息一起带入队列。
	queueItem := txQueueItem{
		tx:              tx,
		txSize:          int(tx.Size()), // #nosec G115
		options:         options,
		resultChan:      resultChan,
		returnedResult:  &atomic.Bool{},
		ctx:             queueCtx,
		firstAppearance: time.Now(),
		isTimeboosted:   isExpressLaneController,
		blockStamp:      blockStamp,
	}
	select {
	case s.txQueue <- queueItem:
		// 成功入队后，后续结果会由 createBlock 或重试/失败路径写回 resultChan。
	case <-queueCtx.Done():
		// 如果在真正入队前调用方就取消或超时，直接把 context 错误返回给上层。
		return queueCtx.Err()
	}
	return nil
}

// preTxFilter 在交易执行前做 nonce、条件交易和地址过滤等检查。
func (s *Sequencer) preTxFilter(_ *params.ChainConfig, header *types.Header, statedb *state.StateDB, _ *arbosState.ArbosState, tx *types.Transaction, options *arbitrum_types.ConditionalOptions, sender common.Address, l1Info *arbos.L1Info) error {
	if s.nonceCache.Caching() {
		stateNonce := s.nonceCache.Get(header, statedb, sender)
		err := MakeNonceError(sender, tx.Nonce(), stateNonce)
		if err != nil {
			nonceCacheRejectedCounter.Inc(1)
			return err
		}
	}
	if options != nil {
		err := options.Check(l1Info.L1BlockNumber(), header.Time, statedb)
		if err != nil {
			conditionalTxRejectedBySequencerCounter.Inc(1)
			return err
		}
		conditionalTxAcceptedBySequencerCounter.Inc(1)
	}
	statedb.TouchAddress(sender)
	if tx.To() != nil {
		statedb.TouchAddress(*tx.To())
	}
	if statedb.IsTxFiltered() || statedb.IsAddressFiltered() {
		return state.ErrArbTxFilter
	}
	return nil
}

// postTxFilter 在交易执行后更新过滤状态、处理 revert 策略并推进 nonce 缓存。
func (s *Sequencer) postTxFilter(header *types.Header, statedb *state.StateDB, _ *arbosState.ArbosState, tx *types.Transaction, sender common.Address, dataGas uint64, result *core.ExecutionResult) error {
	if s.eventFilter != nil {
		logs := statedb.GetCurrentTxLogs()
		for _, l := range logs {
			for _, addr := range s.eventFilter.AddressesForFiltering(l.Topics, l.Data, l.Address, sender) {
				statedb.TouchAddress(addr)
			}
		}
	}

	if statedb.IsTxFiltered() || statedb.IsAddressFiltered() {
		return state.ErrArbTxFilter
	}

	if result.Err != nil && result.UsedGas > dataGas && result.UsedGas-dataGas <= s.config().MaxRevertGasReject {
		return arbitrum.NewRevertReason(result)
	}
	newNonce := tx.Nonce() + 1
	s.nonceCache.Update(header, sender, newNonce)
	newAddrAndNonce := addressAndNonce{sender, newNonce}
	nonceFailure, haveNonceFailure := s.nonceFailures.Get(newAddrAndNonce)
	if haveNonceFailure {
		nonceFailure.revived = true // prevent the expiry hook from taking effect
		s.nonceFailures.Remove(newAddrAndNonce)
		// Immediately check if the transaction submission has been canceled
		err := nonceFailure.queueItem.ctx.Err()
		if err != nil {
			nonceFailure.queueItem.returnResult(err)
		} else {
			// Add this transaction (whose nonce is now correct) back into the queue
			s.txRetryQueue.Push(nonceFailure.queueItem)
		}
	}
	return nil
}

// CheckHealth 检查 sequencer 或其当前 forwarder 是否处于可服务状态。
func (s *Sequencer) CheckHealth(ctx context.Context) error {
	pauseChan, forwarder := s.GetPauseAndForwarder()
	if forwarder != nil {
		return forwarder.CheckHealth(ctx)
	}
	if pauseChan != nil {
		return nil
	}
	_, err := s.execEngine.consensus.ExpectChosenSequencer().Await(ctx)
	return err
}

// ForwardTarget 返回当前配置的主转发目标地址。
func (s *Sequencer) ForwardTarget() string {
	s.activeMutex.Lock()
	defer s.activeMutex.Unlock()
	if s.forwarder == nil {
		return ""
	}
	return s.forwarder.PrimaryTarget()
}

// ForwardTo 将 sequencer 切换到转发模式，把交易转发给指定目标。
func (s *Sequencer) ForwardTo(url string) error {
	s.activeMutex.Lock()
	defer s.activeMutex.Unlock()
	if s.forwarder != nil {
		if s.forwarder.PrimaryTarget() == url {
			log.Warn("attempted to update sequencer forward target with existing target", "url", url)
			return nil
		}
		s.forwarder.Disable()
	}
	s.forwarder = NewForwarder([]string{url}, &s.config().Forwarder)
	err := s.forwarder.Initialize(s.GetContext())
	if err != nil {
		log.Error("failed to set forward agent", "err", err)
		s.forwarder = nil
	}
	if s.pauseChan != nil {
		close(s.pauseChan)
		s.pauseChan = nil
	}
	if err == nil {
		// If createBlocks is waiting for a new queue item, notify it that it needs to clear the nonceFailures.
		select {
		case s.onForwarderSet <- struct{}{}:
		default:
		}
	}
	return err
}

// Activate 让 sequencer 进入活跃出块状态，并关闭已有 forwarder。
func (s *Sequencer) Activate() {
	s.activeMutex.Lock()
	defer s.activeMutex.Unlock()
	if s.forwarder != nil {
		s.forwarder.Disable()
		s.forwarder = nil
	}
	if s.pauseChan != nil {
		close(s.pauseChan)
		s.pauseChan = nil
	}
	if s.expressLaneService != nil {
		s.LaunchThread(func(context.Context) {
			// We launch redis sync (which is best effort) in parallel to avoid blocking sequencer activation
			s.expressLaneService.syncFromRedis()
			time.Sleep(time.Second)
			s.expressLaneService.syncFromRedis()
		})
	}
}

// Pause 暂停本地出块，并清理现有 forwarder 状态。
func (s *Sequencer) Pause() {
	s.activeMutex.Lock()
	defer s.activeMutex.Unlock()
	if s.forwarder != nil {
		s.forwarder.Disable()
		s.forwarder = nil
	}
	if s.pauseChan == nil {
		s.pauseChan = make(chan struct{})
	}
}

var ErrNoSequencer = errors.New("sequencer temporarily not available")

// GetPauseAndForwarder 读取当前暂停信号和 forwarder 状态。
func (s *Sequencer) GetPauseAndForwarder() (chan struct{}, *TxForwarder) {
	s.activeMutex.Lock()
	defer s.activeMutex.Unlock()
	return s.pauseChan, s.forwarder
}

// getForwarder 返回当前可用的 forwarder；如果 sequencer 处于暂停态则等待恢复。
func (s *Sequencer) getForwarder(ctx context.Context) (*TxForwarder, error) {
	for {
		pause, forwarder := s.GetPauseAndForwarder()
		if pause == nil {
			return forwarder, nil
		}
		// if paused: wait till unpaused
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pause:
		}
	}
}

// handleInactive 在本节点不可出块时把当前候选交易转发或重新入队。
func (s *Sequencer) handleInactive(ctx context.Context, queueItems []txQueueItem) bool {
	forwarder, err := s.getForwarder(ctx)
	if err != nil {
		return true
	}
	if forwarder == nil {
		return false
	}
	publishResults := make(chan *txQueueItem, len(queueItems))
	for _, item := range queueItems {
		item := item
		// 并行转发每笔交易，减少等待时间；每笔交易转发完成后会把结果写回 publishResults 通道。
		go func() {
			res := forwarder.PublishTransaction(item.ctx, item.tx, item.options)
			if errors.Is(res, ErrNoSequencer) {
				publishResults <- &item
			} else {
				publishResults <- nil
				item.returnResult(res)
			}
		}()
	}
	for range queueItems {
		remainingItem := <-publishResults
		if remainingItem != nil {
			s.txRetryQueue.Push(*remainingItem)
		}
	}
	// Evict any leftover nonce failures, forwarding them
	s.nonceFailures.Clear()
	return true
}

var sequencerInternalError = errors.New("sequencer internal error")

// FullSequencingHooks 实现执行引擎所需的 hooks，并携带当前候选区块的交易与过滤逻辑。
type FullSequencingHooks struct {
	queueItems               []txQueueItem
	sequencedQueueItemsCount int
	sequencedTxsSizeSoFar    int
	maxSequencedTxsSize      int
	txErrors                 []error
	preTxFilter              func(*params.ChainConfig, *types.Header, *state.StateDB, *arbosState.ArbosState, *types.Transaction, *arbitrum_types.ConditionalOptions, common.Address, *arbos.L1Info) error
	postTxFilter             func(*types.Header, *state.StateDB, *arbosState.ArbosState, *types.Transaction, common.Address, uint64, *core.ExecutionResult) error
	blockFilter              func(*types.Header, *state.StateDB, types.Transactions, types.Receipts) error
	txSizeLimitReached       bool
	// new
	candidateBlock *CandidateBlock
}

// SetCandidateBlock 记录当前这轮排序所对应的候选区块。
func (s *FullSequencingHooks) SetCandidateBlock(c *CandidateBlock) {
	s.candidateBlock = c
}

// CandidateBlock 返回当前绑定的候选区块。
func (s *FullSequencingHooks) CandidateBlock() *CandidateBlock {
	return s.candidateBlock
}

// QueueItems 返回本轮参与排序的队列项。
func (s *FullSequencingHooks) QueueItems() []txQueueItem {
	return s.queueItems
}

// Txes 提取队列项中的原始交易列表。
func (s *FullSequencingHooks) Txes() types.Transactions {
	txs := make(types.Transactions, 0, len(s.queueItems))
	for _, item := range s.queueItems {
		txs = append(txs, item.tx)
	}
	return txs
}

// MessageFromTxes 将本轮成功排序的交易编码为一条 L1 incoming message。
func (s *FullSequencingHooks) MessageFromTxes(header *arbostypes.L1IncomingMessageHeader) (*arbostypes.L1IncomingMessage, error) {
	var l2Message []byte
	// 如果本轮只有一笔交易成功排序，就直接按单笔 SignedTx 消息编码。
	// 这是更紧凑的编码形式，不需要外层 batch 包装和逐笔长度前缀。
	if len(s.txErrors) == 1 && s.txErrors[0] == nil {
		tx, err := s.SequencedTx(0)
		if err != nil {
			return nil, err
		}
		// 把交易编码成二进制，作为 L2 消息 payload 的主体内容。
		txBytes, err := tx.MarshalBinary()
		if err != nil {
			return nil, err
		}
		// 单笔交易消息格式：
		// [1 byte kind = SignedTx] + [tx binary]
		l2Message = append(l2Message, arbos.L2MessageKind_SignedTx)
		l2Message = append(l2Message, txBytes...)
	} else {
		// 多笔交易时改用 Batch 消息格式。
		// 外层先写一个 Batch kind，里面再顺序拼接每一笔成功交易：
		// [8 bytes item length] + [1 byte kind = SignedTx] + [tx binary]
		//
		// 注意这里只会编码成功排序的交易；执行失败的交易会在 txErrors 中体现，
		// 但不会被打进最终的 L2 message。
		l2Message = append(l2Message, arbos.L2MessageKind_Batch)
		sizeBuf := make([]byte, 8)
		for i := 0; i < len(s.txErrors); i++ {
			// 跳过本轮执行失败或未被接受的交易，只保留成功交易。
			if s.txErrors[i] != nil {
				continue
			}
			tx, err := s.SequencedTx(i)
			if err != nil {
				return nil, err
			}
			// 每笔交易先序列化，再写入 batch item。
			txBytes, err := tx.MarshalBinary()
			if err != nil {
				return nil, err
			}
			// Batch 中每个元素的长度前缀包含：
			// - 1 字节的内部消息类型（这里固定是 SignedTx）
			// - 后续交易二进制长度
			// #nosec G115
			binary.BigEndian.PutUint64(sizeBuf, uint64(len(txBytes)+1))
			l2Message = append(l2Message, sizeBuf...)
			l2Message = append(l2Message, arbos.L2MessageKind_SignedTx)
			l2Message = append(l2Message, txBytes...)
		}
	}
	// 最终生成的 L2 message 必须受 ArbOS 的单条消息大小上限约束。
	if len(l2Message) > arbostypes.MaxL2MessageSize {
		return nil, errors.New("l2message too long")
	}
	// 用调用方提供的 header 和刚刚编码出的 L2 payload 组装完整的 L1 incoming message。
	return &arbostypes.L1IncomingMessage{
		Header: header,
		L2msg:  l2Message,
	}, nil
}

// GetTxErrors 返回当前已记录的每笔交易执行结果。
func (s *FullSequencingHooks) GetTxErrors() []error {
	return s.txErrors
}

// InsertLastTxError 追加最近一笔已排序交易的执行结果。
func (s *FullSequencingHooks) InsertLastTxError(err error) {
	s.txErrors = append(s.txErrors, err)
}

// NextTxToSequence returns the next transaction to be included in the block, or nil if there are no more transactions to include.
// It will skip transactions that would cause the total size of included transactions to exceed maxSequencedTxsSize.
func (s *FullSequencingHooks) NextTxToSequence() (*types.Transaction, *arbitrum_types.ConditionalOptions, error) {
	for {
		// This is not supposed to happen, if so we have a bug
		if len(s.txErrors) != s.sequencedQueueItemsCount {
			return nil, nil, fmt.Errorf("FullSequencingHooks: GetNextTx detected out of order request to sequence tx. hookTxErrors: %d, nextTxIdToBeSequenced: %d", len(s.txErrors), s.sequencedQueueItemsCount)
		}
		if s.sequencedQueueItemsCount > 0 && s.txErrors[s.sequencedQueueItemsCount-1] == nil {
			s.sequencedTxsSizeSoFar += s.queueItems[s.sequencedQueueItemsCount-1].txSize
		}
		if s.sequencedQueueItemsCount >= len(s.queueItems) {
			return nil, nil, nil
		}
		if s.sequencedTxsSizeSoFar+s.queueItems[s.sequencedQueueItemsCount].txSize > s.maxSequencedTxsSize {
			s.sequencedQueueItemsCount += 1
			s.InsertLastTxError(core.ErrGasLimitReached)
			s.txSizeLimitReached = true
		} else {
			s.sequencedQueueItemsCount += 1
			break
		}
	}
	return s.queueItems[s.sequencedQueueItemsCount-1].tx, s.queueItems[s.sequencedQueueItemsCount-1].options, nil
}

// DiscardInvalidTxsEarly 指示执行引擎可在早期丢弃明显无效的交易。
func (s *FullSequencingHooks) DiscardInvalidTxsEarly() bool {
	return true
}

// SequencedTx 返回指定序号上已经进入排序流程的交易。
func (s *FullSequencingHooks) SequencedTx(txId int) (*types.Transaction, error) {
	// This is not supposed to happen, if so we have a bug
	if txId > s.sequencedQueueItemsCount {
		return nil, fmt.Errorf("transaction queried for was not scheduled by the FullSequencingHooks. txId: %d, sequencedCount: %d", txId, s.sequencedQueueItemsCount)
	}
	return s.queueItems[txId].tx, nil
}

// PreTxFilter 调用外部注入的交易前过滤逻辑。
func (s *FullSequencingHooks) PreTxFilter(config *params.ChainConfig, header *types.Header, db *state.StateDB, a *arbosState.ArbosState, transaction *types.Transaction, options *arbitrum_types.ConditionalOptions, address common.Address, info *arbos.L1Info) error {
	if s.preTxFilter != nil {
		return s.preTxFilter(config, header, db, a, transaction, options, address, info)
	}
	return nil
}

// PostTxFilter 调用外部注入的交易后过滤逻辑。
func (s *FullSequencingHooks) PostTxFilter(header *types.Header, db *state.StateDB, a *arbosState.ArbosState, transaction *types.Transaction, address common.Address, u uint64, result *core.ExecutionResult) error {
	if s.postTxFilter != nil {
		return s.postTxFilter(header, db, a, transaction, address, u, result)
	}
	return nil
}

// BlockFilter 调用外部注入的区块级过滤逻辑。
func (s *FullSequencingHooks) BlockFilter(header *types.Header, db *state.StateDB, transactions types.Transactions, receipts types.Receipts) error {
	if s.blockFilter != nil {
		return s.blockFilter(header, db, transactions, receipts)
	}
	return nil
}

// MakeSequencingHooks 构造一组用于完整交易排序流程的 hooks。
func MakeSequencingHooks(
	items []txQueueItem,
	maxSequencedTxsSize int,
	preTxFilter func(*params.ChainConfig, *types.Header, *state.StateDB, *arbosState.ArbosState, *types.Transaction, *arbitrum_types.ConditionalOptions, common.Address, *arbos.L1Info) error,
	postTxFilter func(*types.Header, *state.StateDB, *arbosState.ArbosState, *types.Transaction, common.Address, uint64, *core.ExecutionResult) error,
	blockFilter func(*types.Header, *state.StateDB, types.Transactions, types.Receipts) error,
) *FullSequencingHooks {
	res := &FullSequencingHooks{
		queueItems:               items,
		sequencedQueueItemsCount: 0,
		sequencedTxsSizeSoFar:    0,
		maxSequencedTxsSize:      maxSequencedTxsSize,
		preTxFilter:              preTxFilter,
		postTxFilter:             postTxFilter,
		blockFilter:              blockFilter,
	}
	return res
}

// MakeZeroTxSizeSequencingHooksForTesting creates sequencing hooks for testing with tx size always zero.
// This allows all transactions to be included in a block regardless of size.
func MakeZeroTxSizeSequencingHooksForTesting(
	txes types.Transactions,
	preTxFilter func(*params.ChainConfig, *types.Header, *state.StateDB, *arbosState.ArbosState, *types.Transaction, *arbitrum_types.ConditionalOptions, common.Address, *arbos.L1Info) error,
	postTxFilter func(*types.Header, *state.StateDB, *arbosState.ArbosState, *types.Transaction, common.Address, uint64, *core.ExecutionResult) error,
	blockFilter func(*types.Header, *state.StateDB, types.Transactions, types.Receipts) error,
) *FullSequencingHooks {
	var items []txQueueItem
	for _, tx := range txes {
		items = append(items, txQueueItem{
			tx: tx,
		})
	}
	return MakeSequencingHooks(
		items,
		0,
		preTxFilter,
		postTxFilter,
		blockFilter,
	)
}

// expireNonceFailures 清理已过期的 nonce 失败交易，并返回下一次过期触发计时器。
func (s *Sequencer) expireNonceFailures() *time.Timer {
	defer nonceFailureCacheSizeGauge.Update(int64(s.nonceFailures.Len()))
	for {
		_, failure, ok := s.nonceFailures.GetOldest()
		if !ok {
			return nil
		}
		untilExpiry := time.Until(failure.expiry)
		if untilExpiry > 0 {
			return time.NewTimer(untilExpiry)
		}

		// Check queueCtx status before notifying client
		queueItem := failure.queueItem
		err := queueItem.ctx.Err()
		if err != nil {
			// queueCtx has already timed out, return that error
			queueItem.returnResultMaybeLog(err, true)
		} else {
			// nonce-failure-cache-expiry timeout, return the original nonce error
			queueItem.returnResultMaybeLog(failure.nonceErr, true)
		}

		s.nonceFailures.RemoveOldest()
	}
}

// precheckNonces 基于当前状态和已见交易顺序做一次轻量 nonce 预检查。
func (s *Sequencer) precheckNonces(queueItems []txQueueItem) []txQueueItem {
	bc := s.execEngine.bc
	latestHeader := bc.CurrentBlock()
	// 取当前最新区块对应的状态快照，作为本轮 nonce 预检查的基准状态。
	// 这里不执行交易，只做一次便宜的前置筛查，尽量早地剔除必然失败的交易。
	latestState, err := bc.StateAt(latestHeader.Root)
	if err != nil {
		log.Error("failed to get current state to pre-check nonces", "err", err)
		return queueItems
	}
	// 构造“下一块”语义下的 signer，用它来恢复交易发送者地址。
	nextHeaderNumber := arbmath.BigAdd(latestHeader.Number, common.Big1)
	arbosVersion := types.DeserializeHeaderExtraInformation(latestHeader).ArbOSFormatVersion
	signer := types.MakeSigner(bc.Config(), nextHeaderNumber, latestHeader.Time, arbosVersion)
	// outputQueueItems 是通过预检查后，仍然值得进入后续正式排序流程的交易。
	outputQueueItems := make([]txQueueItem, 0, len(queueItems))
	// nextQueueItem 用于“插队”处理刚被前序交易唤醒的 nonce failure 交易，
	// 让它尽快在当前扫描过程中重新参与判断。
	var nextQueueItem *txQueueItem
	var queueItemsIdx int
	// pendingNonces 记录“按当前扫描顺序推演出来的每个地址下一可用 nonce”。
	// 它不等于真实 state，而是“假设前面已经接受的交易都成功”时的临时视图。
	pendingNonces := make(map[common.Address]uint64)
	for {
		var queueItem txQueueItem
		// 优先处理被前序交易刚刚唤醒的交易；否则按原队列顺序继续扫描。
		if nextQueueItem != nil {
			queueItem = *nextQueueItem
			nextQueueItem = nil
		} else if queueItemsIdx < len(queueItems) {
			queueItem = queueItems[queueItemsIdx]
			queueItemsIdx++
		} else {
			break
		}
		tx := queueItem.tx
		// 先恢复发送者地址；如果签名本身有问题，就可以直接返回错误。
		sender, err := types.Sender(signer, tx)
		if err != nil {
			queueItem.returnResult(err)
			continue
		}
		// stateNonce 是链上当前状态里的真实 nonce。
		// pendingNonce 是把当前批次前面已经“暂时接受”的交易考虑进去后的推演 nonce。
		stateNonce := s.nonceCache.Get(latestHeader, latestState, sender)
		pendingNonce, pending := pendingNonces[sender]
		if !pending {
			pendingNonce = stateNonce
		}
		txNonce := tx.Nonce()
		if txNonce == pendingNonce {
			// 这是当前顺序下刚好可接上的交易，先把该地址的推演 nonce 向前推进一位。
			pendingNonces[sender] = txNonce + 1
			nextKey := addressAndNonce{sender, txNonce + 1}
			revivingFailure, exists := s.nonceFailures.Get(nextKey)
			if exists {
				// This tx was the predecessor to one that had failed its nonce check
				// Re-enqueue the tx whose nonce should now be correct, unless it expired
				revivingFailure.revived = true
				s.nonceFailures.Remove(nextKey)
				err := revivingFailure.queueItem.ctx.Err()
				if err != nil {
					revivingFailure.queueItem.returnResult(err)
				} else {
					// 让刚被唤醒的后继交易在下一轮循环中优先处理，
					// 这样可以在一次扫描里连续接上 nonce 链。
					nextQueueItem = &revivingFailure.queueItem
				}
			}
		} else if txNonce < stateNonce || txNonce > pendingNonce {
			// It's impossible for this tx to succeed so far,
			// because its nonce is lower than the state nonce
			// or higher than the highest tx nonce we've seen.
			err := MakeNonceError(sender, txNonce, stateNonce)
			if errors.Is(err, core.ErrNonceTooHigh) {
				var nonceError NonceError
				if !errors.As(err, &nonceError) {
					log.Warn("unreachable nonce error is not nonceError")
					continue
				}
				// nonce 过高说明它可能只是缺少前序交易。
				// 先把它放进 nonceFailures，等待前一个 nonce 的交易出现后再复活。
				s.nonceFailures.Add(nonceError, queueItem)
				continue
			} else if err != nil {
				// 其余 nonce 错误（例如 nonce 过低）在当前状态下不可能成功，
				// 可以直接拒绝，不必进入正式执行阶段。
				nonceCacheRejectedCounter.Inc(1)
				queueItem.returnResult(err)
				continue
			} else {
				log.Warn("unreachable nonce err == nil condition hit in precheckNonces")
			}
		}
		// If neither if condition was hit, then txNonce >= stateNonce && txNonce < pendingNonce
		// This tx might still go through if previous txs fail.
		// We'll include it in the output queue in case that happens.
		//
		// 典型场景是：当前队列里已经看到了同地址更高优先级/更早位置的交易，
		// 所以 pendingNonce 已经被推进了，但那些前序交易在正式执行阶段仍有可能失败。
		// 因此这里不能草率拒绝，仍然要保留给后续排序逻辑决定。
		outputQueueItems = append(outputQueueItems, queueItem)
	}
	// 更新指标，反映还有多少笔交易正因 nonce 过高而暂存在失败缓存中。
	nonceFailureCacheSizeGauge.Update(int64(s.nonceFailures.Len()))
	return outputQueueItems
}

// resolvePoliciesForQueueItems 为当前候选交易批次解析 endorsement policy，并生成候选区块。
func (s *Sequencer) resolvePoliciesForQueueItems(
	ctx context.Context,
	lastBlockHeader *types.Header,
	queueItems []txQueueItem,
) (*CandidateBlock, error) {
	if s.policyResolver == nil {
		return nil, nil
	}

	msgIdx, err := s.execEngine.BlockNumberToMessageIndex(lastBlockHeader.Number.Uint64() + 1)
	if err != nil {
		return nil, err
	}

	blockCtx := &endorsementpolicy.BlockPolicyContext{
		ChainID:             s.execEngine.bc.Config().ChainID,
		ParentHash:          lastBlockHeader.Hash(),
		BlockNumber:         lastBlockHeader.Number.Uint64() + 1,
		MessageIndex:        uint64(msgIdx),
		DelayedMessagesRead: lastBlockHeader.Nonce.Uint64(),
	}

	result := &CandidateBlock{
		QueueItems: make([]txQueueItem, len(queueItems)),
		Txs:        make([]*CandidateTx, 0, len(queueItems)),
	}
	copy(result.QueueItems, queueItems)

	for i := range queueItems {
		item := &queueItems[i]

		resolution, err := s.policyResolver.ResolveTxPolicy(
			ctx,
			blockCtx,
			item.tx,
			i,
			nil, // 第一版不使用 tx 自带 hint
		)
		if err != nil {
			return nil, err
		}
		if err := resolution.Validate(); err != nil {
			return nil, err
		}

		result.Txs = append(result.Txs, &CandidateTx{
			TxIndex: i,
			Tx:      item.tx,
			Receipt: nil, // 执行前还没有 receipt
			Policy:  resolution,
		})
	}

	return result, nil
}

// createBlock 从队列中拉取交易、执行排序并尝试生成一个新区块。
func (s *Sequencer) createBlock(ctx context.Context) (returnValue bool) {
	var queueItems []txQueueItem
	seenTxHashes := make(map[common.Hash]struct{})

	defer func() {
		// createBlock 是 sequencer 的核心路径之一，这里兜底捕获 panic，
		// 避免某一轮构块异常导致调用方永远收不到结果。
		panicErr := recover()
		if panicErr != nil {
			log.Error("sequencer block creation panicked", "panic", panicErr, "backtrace", string(debug.Stack()))
			for _, item := range queueItems {
				if !item.returnedResult.Load() {
					item.returnResult(sequencerInternalError)
				}
			}
			returnValue = true
		}
	}()
	defer nonceFailureCacheSizeGauge.Update(int64(s.nonceFailures.Len()))

	appendQueueItemIfNotDuplicate := func(item txQueueItem) bool {
		// 构造候选块时按 tx hash 去重，避免同一笔交易被重复加入当前批次。
		// 这里即使重复来源于不同内部队列，也只保留第一份。
		if item.tx == nil {
			return false
		}
		txHash := item.tx.Hash()
		if _, exists := seenTxHashes[txHash]; exists {
			log.Warn(
				"ENDORSEMENT_DEBUG dropping duplicate tx while building candidate queue",
				"txHash", txHash,
				"nonce", item.tx.Nonce(),
				"to", item.tx.To(),
			)
			return false
		}
		seenTxHashes[txHash] = struct{}{}
		queueItems = append(queueItems, item)
		return true
	}

	config := s.config()
	lastBlock := s.execEngine.bc.CurrentBlock()

	// 按最新配置调整 nonce failure cache 容量，并清理已经过期的“nonce 太高”交易。
	s.nonceFailures.Resize(config.NonceFailureCacheSize)
	nextNonceExpiryTimer := s.expireNonceFailures()
	defer func() {
		if nextNonceExpiryTimer != nil {
			nextNonceExpiryTimer.Stop()
		}
	}()

	txQueueLen := int64(len(s.txQueue))
	sequencerQueueGauge.Update(txQueueLen)
	sequencerQueueHistogram.Update(txQueueLen)

	// 第一阶段：从多个内部来源收集一批候选交易。
	// 来源按优先级大致包括：
	// - 重试队列 txRetryQueue
	// - timeboost 拍卖结算队列
	// - 主接收队列 txQueue
	//
	// 收集策略并不是“有多少拿多少”，而是：
	// - 至少等到第一笔交易
	// - 收到第一笔后继续短暂观察，尽量聚合更多交易进同一块
	// - 超过读队列窗口后停止收集，进入正式排序流程
	var startOfReadingFromTxQueue time.Time
	startOfBlockCreation := time.Now()
	for {
		if len(queueItems) == 1 && startOfReadingFromTxQueue.IsZero() {
			// 第一笔交易进入候选集后，开始计算后续收集窗口。
			startOfReadingFromTxQueue = time.Now()

			waitForFirstTx := time.Since(startOfBlockCreation)
			if waitForFirstTx < time.Millisecond {
				waitForTxHistogram.Update(0)
			} else {
				waitForTxHistogram.Update(waitForFirstTx.Nanoseconds())
			}
		} else if len(queueItems) > 1 && time.Since(startOfReadingFromTxQueue) > config.ReadFromTxQueueTimeout {
			// 收到第一笔后再等一个短窗口，如果已经有多笔交易且窗口到期，就开始构块。
			break
		}

		var queueItem txQueueItem

		if s.txRetryQueue.Len() > 0 {
			// 已经重试过的交易优先于主队列，这样能更快处理上轮因时机问题未完成的交易。
			select {
			case queueItem = <-s.timeboostAuctionResolutionTxQueue:
				log.Debug("Popped the auction resolution tx", "txHash", queueItem.tx.Hash())
			default:
				queueItem = s.txRetryQueue.Pop()
			}
		} else if len(queueItems) == 0 {
			// 当前还没有任何候选交易时，允许阻塞等待：
			// - 新交易到达
			// - 拍卖结算交易到达
			// - nonce failure 到期
			// - 切换到 forward 模式
			// - 上下文结束
			var nextNonceExpiryChan <-chan time.Time
			if nextNonceExpiryTimer != nil {
				nextNonceExpiryChan = nextNonceExpiryTimer.C
			}
			select {
			case queueItem = <-s.timeboostAuctionResolutionTxQueue:
				log.Debug("Popped the auction resolution tx", "txHash", queueItem.tx.Hash())
			default:
				select {
				case queueItem = <-s.txQueue:
				case queueItem = <-s.timeboostAuctionResolutionTxQueue:
					log.Debug("Popped the auction resolution tx", "txHash", queueItem.tx.Hash())
				case <-nextNonceExpiryChan:
					// nonce failure 到期后先清理，再继续等待交易。
					nextNonceExpiryTimer = s.expireNonceFailures()
					continue
				case <-s.onForwarderSet:
					// 如果切到了 forward 模式，本地就不该再继续等待 predecessor，
					// 清空 nonceFailures，后续由转发路径处理。
					_, forwarder := s.GetPauseAndForwarder()
					if forwarder != nil {
						s.nonceFailures.Clear()
					}
					continue
				case <-ctx.Done():
					return false
				}
			}
		} else {
			done := false

			// 已经至少拿到一笔交易后，不再长时间阻塞；
			// 尽量把当前队列里“已经就绪”的交易快速收集完。
			select {
			case queueItem = <-s.timeboostAuctionResolutionTxQueue:
				log.Debug("Popped the auction resolution tx", "txHash", queueItem.tx.Hash())
			default:
				select {
				case queueItem = <-s.txQueue:
				case queueItem = <-s.timeboostAuctionResolutionTxQueue:
					log.Debug("Popped the auction resolution tx", "txHash", queueItem.tx.Hash())
				default:
					done = true
				}
			}

			if done {
				if len(queueItems) == 1 && config.ExperimentalBatchingWindow > 0 && !startOfReadingFromTxQueue.IsZero() {
					// 实验性 batching：只有拿到第一笔交易时，再额外短等一小段时间，
					// 争取把随后到达的交易并进同一个候选块。
					elapsed := time.Since(startOfReadingFromTxQueue)
					remaining := config.ExperimentalBatchingWindow - elapsed
					if remaining > 0 {
						timer := time.NewTimer(remaining)
						select {
						case queueItem = <-s.txQueue:
							done = false
						case queueItem = <-s.timeboostAuctionResolutionTxQueue:
							log.Debug("Popped the auction resolution tx", "txHash", queueItem.tx.Hash())
							done = false
						case <-timer.C:
							done = true
						case <-ctx.Done():
							timer.Stop()
							return false
						}
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
					}
				}

				if done {
					// 当前没有更多可立即收集的交易，结束收集阶段。
					break
				}
			}
		}

		// 对每个出队交易先做轻量本地校验，把明显无效的情况尽早返回给调用方。
		err := queueItem.ctx.Err()
		if err != nil {
			queueItem.returnResult(err)
			continue
		}
		if queueItem.txSize > config.MaxTxDataSize {
			queueItem.returnResult(txpool.ErrOversizedData)
			continue
		}
		if queueItem.isTimeboosted &&
			queueItem.blockStamp != 0 &&
			lastBlock.Number.Uint64() >= queueItem.blockStamp+config.Timeboost.QueueTimeoutInBlocks {
			err := fmt.Errorf(
				"timeboosted tx: %s has hit block based timeout. currentBlockNum: %d, blockStamp: %d, blockExpiry: %d",
				queueItem.tx.Hash(),
				lastBlock.Number.Uint64()+1,
				queueItem.blockStamp,
				queueItem.blockStamp+config.Timeboost.QueueTimeoutInBlocks,
			)
			queueItem.returnResult(err)
			log.Info("Error sequencing timeboost tx", "err", err)
			continue
		}
		if arbmath.BigLessThan(queueItem.tx.GasFeeCap(), lastBlock.BaseFee) {
			queueItem.returnResult(fmt.Errorf("%w: maxFeePerGas: %s baseFee: %s", core.ErrFeeCapTooLow, queueItem.tx.GasFeeCap(), lastBlock.BaseFee))
			continue
		}

		// 通过所有轻量检查后，再加入本轮候选集。
		appendQueueItemIfNotDuplicate(queueItem)
	}

	// 第二阶段：在真正执行前，基于当前状态做一次 nonce 预筛查，
	// 尽量把“必然太高/太低”的交易提前处理掉。
	s.nonceCache.Resize(config.NonceCacheSize)
	s.nonceCache.BeginNewBlock()
	queueItems = s.precheckNonces(queueItems)
	maxTxDataSize := s.config().MaxTxDataSize

	if len(queueItems) == 0 {
		// 本轮虽然可能收到了交易，但经过预检查后没有可继续排序的候选项。
		return false
	}

	lastBlockHeader := s.execEngine.bc.CurrentBlock()
	if lastBlockHeader == nil {
		log.Error("sequencer failed to get current block header")
		return true
	}

	// 第三阶段：如果启用了 endorsement policy，在正式执行前先为初始候选块解析策略。
	// 这里的结果会挂到 CandidateBlock 上，供后续执行和必要时重建候选块使用。
	candidateBlock, candierr := s.resolvePoliciesForQueueItems(ctx, lastBlock, queueItems)
	if candierr != nil {
		log.Error("failed to resolve endorsement policies", "err", candierr)
		for _, queueItem := range queueItems {
			if !queueItem.returnedResult.Load() {
				queueItem.returnResult(candierr)
			}
		}
		return false
	}

	currentCandidateBlock := candidateBlock
	maxRebuildRounds := 0
	if s.policyConfig != nil {
		maxRebuildRounds = s.policyConfig.MaxRebuildRounds
	}

	for rebuildRound := 0; ; rebuildRound++ {
		// 第四阶段：正式尝试执行当前候选块。
		// 如果上一轮因为 endorsement 失败要求重建，这里就基于 currentCandidateBlock 继续。
		currentQueueItems := queueItems
		if currentCandidateBlock != nil {
			currentQueueItems = cloneQueueItemsFromCandidateBlock(currentCandidateBlock)
		}

		if len(currentQueueItems) == 0 {
			log.Warn(
				"no candidate transactions left after endorsement rebuild",
				"rebuildRound", rebuildRound,
			)
			return false
		}

		// 把当前轮需要参与排序的 timeboost 交易单独标记出来，传给执行引擎。
		timeboostedTxs := make(map[common.Hash]struct{})
		hooks := MakeSequencingHooks(
			currentQueueItems,
			maxTxDataSize,
			s.preTxFilter,
			s.postTxFilter,
			nil,
		)
		hooks.SetCandidateBlock(currentCandidateBlock)

		if currentCandidateBlock != nil {
			log.Info(
				"ENDORSEMENT_DEBUG sequencer attached candidate block",
				"rebuildRound", rebuildRound,
				"txCount", len(currentCandidateBlock.Txs),
			)
			for _, item := range currentCandidateBlock.Txs {
				if item == nil || item.Tx == nil || item.Policy == nil || item.Policy.Policy == nil {
					continue
				}
				log.Info(
					"resolved tx endorsement policy",
					"txHash", item.Tx.Hash(),
					"txIndex", item.TxIndex,
					"policyID", item.Policy.Policy.ID,
					"threshold", item.Policy.Policy.Threshold,
				)
			}
		}

		for _, queueItem := range currentQueueItems {
			if queueItem.isTimeboosted {
				timeboostedTxs[queueItem.tx.Hash()] = struct{}{}
			}
		}

		// 如果当前节点已经不该自己出块，就把这批候选交易转发/回退，而不是继续本地执行。
		if s.handleInactive(ctx, currentQueueItems) {
			return false
		}

		// 构造本轮 L1 incoming message header 所需的父链块高和时间戳。
		timestamp := time.Now().Unix()
		s.L1BlockAndTimeMutex.Lock()
		l1Block := s.l1BlockNumber.Load()
		l1Timestamp := s.l1Timestamp
		s.L1BlockAndTimeMutex.Unlock()

		// sequencer 需要确保自己看到的 L1 时间没有明显漂移，
		// 否则会构造出不可靠的 L2 区块时间戳。
		if s.l1Reader != nil && (l1Block == 0 || math.Abs(float64(l1Timestamp)-float64(timestamp)) > config.MaxAcceptableTimestampDelta.Seconds()) {
			for _, queueItem := range currentQueueItems {
				s.txRetryQueue.Push(queueItem)
			}
			log.Error(
				"cannot sequence: unknown L1 block or L1 timestamp too far from local clock time",
				"l1Block", l1Block,
				"l1Timestamp", time.Unix(int64(l1Timestamp), 0),
				"localTimestamp", time.Unix(timestamp, 0),
			)
			return true
		}

		// 这是执行引擎要消费的 L1 incoming message header，代表“本轮要注入的 L2 消息”。
		header := &arbostypes.L1IncomingMessageHeader{
			Kind:        arbostypes.L1MessageType_L2Message,
			Poster:      l1pricing.BatchPosterAddress,
			BlockNumber: l1Block,
			Timestamp:   arbmath.SaturatingUCast[uint64](timestamp),
			RequestId:   nil,
			L1BaseFee:   nil,
		}

		start := time.Now()
		var (
			block *types.Block
			err   error
		)
		// 进入执行引擎做真正的交易排序与区块构建。
		if config.EnableProfiling {
			block, err = s.execEngine.SequenceTransactionsWithProfiling(header, hooks, timeboostedTxs)
		} else {
			block, err = s.execEngine.SequenceTransactions(header, hooks, timeboostedTxs)
		}
		elapsed := time.Since(start)
		blockCreationTimer.Update(elapsed.Nanoseconds())
		if elapsed >= time.Second*5 {
			var blockNum *big.Int
			if block != nil {
				blockNum = block.Number()
			}
			log.Warn(
				"took over 5 seconds to sequence a block",
				"elapsed", elapsed,
				"numTxes", hooks.sequencedQueueItemsCount,
				"success", block != nil,
				"l2Block", blockNum,
			)
		}

		if err == nil {
			// 执行成功后，hooks.txErrors 应该与实际进入排序流程的交易数量一一对应。
			// 若后面还有未消费的 queueItems，说明它们来不及进入本轮，需要重新入重试队列。
			if len(hooks.txErrors) != hooks.sequencedQueueItemsCount {
				err = fmt.Errorf(
					"unexpected number of error results: %v vs number of txes %v",
					len(hooks.txErrors),
					hooks.sequencedQueueItemsCount,
				)
			} else {
				for i := hooks.sequencedQueueItemsCount; i < len(hooks.queueItems); i++ {
					s.txRetryQueue.Push(hooks.queueItems[i])
				}
			}
		}

		// endorsement 失败：根据失败交易列表重建候选块，并在允许的次数内重试执行。
		var rebuildErr *ErrCandidateBlockRebuildRequired
		if errors.As(err, &rebuildErr) {
			if rebuildErr.Decision == nil || rebuildErr.Decision.Rebuild == nil {
				log.Error("candidate block rebuild required but rebuild instruction is nil")
				for _, queueItem := range currentQueueItems {
					if !queueItem.returnedResult.Load() {
						queueItem.returnResult(err)
					}
				}
				return false
			}

			if rebuildRound >= maxRebuildRounds {
				log.Error(
					"candidate block rebuild rounds exceeded",
					"rebuildRound", rebuildRound,
					"maxRebuildRounds", maxRebuildRounds,
					"failedTxIndexes", rebuildErr.Decision.Rebuild.FailedTxIndexes,
				)
				for _, queueItem := range currentQueueItems {
					if !queueItem.returnedResult.Load() {
						queueItem.returnResult(err)
					}
				}
				return false
			}

			log.Warn(
				"ENDORSEMENT_DEBUG rebuilding candidate block after endorsement failure",
				"rebuildRound", rebuildRound,
				"failedTxIndexes", rebuildErr.Decision.Rebuild.FailedTxIndexes,
				"failedTxHashes", rebuildErr.Decision.Rebuild.FailedTxHashes,
			)

			// 先给本轮被过滤掉的失败交易返回结果，避免静默丢失
			for _, failedIdx := range rebuildErr.Decision.Rebuild.FailedTxIndexes {
				if failedIdx < 0 || failedIdx >= len(currentQueueItems) {
					continue
				}
				failedItem := currentQueueItems[failedIdx]
				if !failedItem.returnedResult.Load() {
					failedItem.returnResult(rebuildErr)
				}
			}

			if currentCandidateBlock == nil {
				log.Error("rebuild required but current candidate block is nil")
				for _, queueItem := range currentQueueItems {
					if !queueItem.returnedResult.Load() {
						queueItem.returnResult(err)
					}
				}
				return false
			}

			nextCandidateBlock, rebuildBlockErr := FilterFailedTxsAndRebuildCandidateBlock(
				currentCandidateBlock,
				rebuildErr.Decision.Rebuild.FailedTxIndexes,
			)
			if rebuildBlockErr != nil {
				log.Error("failed to rebuild candidate block", "err", rebuildBlockErr)
				for _, queueItem := range currentQueueItems {
					if !queueItem.returnedResult.Load() {
						queueItem.returnResult(rebuildBlockErr)
					}
				}
				return false
			}

			if nextCandidateBlock == nil || len(nextCandidateBlock.Txs) == 0 {
				log.Warn(
					"all candidate txs filtered out after endorsement failure",
					"rebuildRound", rebuildRound,
				)
				return false
			}

			currentCandidateBlock = nextCandidateBlock
			continue
		}

		if errors.Is(err, execution.ErrRetrySequencer) {
			log.Warn("error sequencing transactions", "err", err)
			if s.handleInactive(ctx, currentQueueItems) {
				return false
			}
			for _, item := range currentQueueItems {
				s.txRetryQueue.Push(item)
			}
			return false
		}

		if err != nil {
			// context.Canceled 通常意味着本轮被外部打断，先把交易放回重试队列，等待下一轮。
			if errors.Is(err, context.Canceled) {
				for _, item := range currentQueueItems {
					s.txRetryQueue.Push(item)
				}
				return true
			}
			log.Error("error sequencing transactions", "err", err)
			for _, queueItem := range currentQueueItems {
				if !queueItem.returnedResult.Load() {
					queueItem.returnResult(err)
				}
			}
			return false
		}

		if block != nil {
			// 只有真正产出了区块，才能把 nonce 缓存推进到新区块。
			successfulBlocksCounter.Inc(1)
			s.nonceCache.Finalize(block)
		}

		// 第五阶段：把执行结果逐笔回写给调用方，同时把需要延后处理的交易重新缓存/入队。
		madeBlock := false
		var blockTxSize int64
		blockGasLimitReached := false
		for i, err := range hooks.txErrors {
			queueItem := currentQueueItems[i]
			if err == nil {
				// 没有错误表示这笔交易已经成功进入本轮区块。
				madeBlock = true
				blockTxSize += int64(queueItem.txSize)
				txSizeHistogram.Update(int64(queueItem.txSize))
			}
			if errors.Is(err, core.ErrGasLimitReached) {
				if madeBlock {
					// 如果区块已经装入了前面的交易，再遇到 gas limit，说明只是这笔及其后续没塞进去，
					// 它仍有机会在下一块成功，因此重新放回重试队列。
					blockGasLimitReached = true
					s.txRetryQueue.Push(queueItem)
					continue
				}
			}
			if errors.Is(err, core.ErrIntrinsicGas) {
				// 统一一下错误类型，避免把内部包装后的错误泄漏到上层调用方。
				err = core.ErrIntrinsicGas
			}
			var nonceError NonceError
			if errors.As(err, &nonceError) && nonceError.txNonce > nonceError.stateNonce {
				// 正式执行阶段发现 nonce 太高，说明它仍依赖前序交易，
				// 先放进 nonce failure cache，等 predecessor 到来后再唤醒。
				s.nonceFailures.Add(nonceError, queueItem)
				continue
			}
			queueItem.returnResult(err)
		}

		if madeBlock {
			// 记录本轮区块是因为哪类原因停止继续装交易。
			blockTxSizeHistogram.Update(blockTxSize)
			if hooks.txSizeLimitReached {
				dataLimitedBlocksCounter.Inc(1)
			} else if blockGasLimitReached {
				gasLimitedBlocksCounter.Inc(1)
			} else {
				txExhaustedBlocksCounter.Inc(1)
			}
		}
		return madeBlock
	}
}

// updateLatestParentChainBlock 用更近的父链头更新本地缓存的 L1 块高和时间戳。
func (s *Sequencer) updateLatestParentChainBlock(header *types.Header) {
	s.L1BlockAndTimeMutex.Lock()
	defer s.L1BlockAndTimeMutex.Unlock()

	l1BlockNumber := arbutil.ParentHeaderToL1BlockNumber(header)
	if header.Time > s.l1Timestamp || (header.Time == s.l1Timestamp && l1BlockNumber > s.l1BlockNumber.Load()) {
		s.l1Timestamp = header.Time
		s.l1BlockNumber.Store(l1BlockNumber)
	}
}

// Initialize 初始化 sequencer 启动前依赖的父链与地址过滤状态。
func (s *Sequencer) Initialize(ctx context.Context) error {
	if s.l1Reader == nil {
		return nil
	}

	header, err := s.l1Reader.LastHeader(ctx)
	if err != nil {
		return err
	}
	s.updateLatestParentChainBlock(header)

	if s.addressFilterService != nil {
		if err = s.addressFilterService.Initialize(ctx); err != nil {
			return fmt.Errorf("error initializing restricted addr service: %w", err)
		}
	}

	return nil
}

// InitializeExpressLaneService 创建并挂载 express lane 服务。
func (s *Sequencer) InitializeExpressLaneService(
	auctioneerAddr common.Address,
	roundTimingInfo *timeboost.RoundTimingInfo,
	expressLaneTracker *ExpressLaneTracker,
) error {
	els, err := newExpressLaneService(
		s,
		s.config,
		roundTimingInfo,
		s.execEngine.bc,
		expressLaneTracker,
	)
	if err != nil {
		return fmt.Errorf("failed to create express lane service. err: %w", err)
	}
	s.auctioneerAddr = auctioneerAddr
	s.expressLaneService = els
	return nil
}

const maxConsecutiveExpectedSurplusFailures = 20

var (
	usableBytesInBlob    = big.NewInt(int64(len(kzg4844.Blob{}) * 31 / 32))
	blobTxBlobGasPerBlob = big.NewInt(params.BlobTxBlobGasPerBlob)
)

// logExpectedSurplusError 记录 expected surplus 更新失败日志，并统计连续失败次数。
func (s *Sequencer) logExpectedSurplusError(err error) {
	s.expectedSurplusFailureCount++

	logLevel := log.Error
	if s.expectedSurplusFailureCount <= maxConsecutiveExpectedSurplusFailures {
		logLevel = log.Warn
	}

	logLevel("expected surplus soft/hard thresholds are enabled but unable to fetch latest expected surplus, retrying",
		"err", err,
		"consecutiveFailures", s.expectedSurplusFailureCount)
}

// updateExpectedSurplus 重新计算当前 backlog 对应的预期 L1 surplus。
func (s *Sequencer) updateExpectedSurplus(ctx context.Context) (int64, error) {
	header, err := s.l1Reader.LastHeader(ctx)
	if err != nil {
		return 0, fmt.Errorf("error encountered getting latest header from l1reader while updating expectedSurplus: %w", err)
	}
	l1GasPrice := header.BaseFee.Int64()

	// #nosec G115
	backlogCallDataUnits := int64(s.execEngine.backlogCallDataUnits())
	var backlogCost int64 // tx's cached calldata units are already scaled by TxDataNonZeroGasEIP2028 = 16, so we divide them by 16 while calculating cost for blobs and for EIP7623 pricing accordingly
	switch s.config().ExpectedSurplusGasPriceMode {
	case "CalldataPrice":
		backlogCost = backlogCallDataUnits * header.BaseFee.Int64()
	case "BlobPrice":
		if s.config().Dangerous.DisableBlobBaseFeeCheck {
			if !s.expectedSurplusUpdated {
				// only print notification once
				log.Info("expected surplus calculation is set to use blob price but --execution.sequencer.dangerous.disable-blob-base-fee-check is set, falling back to calldata price model")
			}
			backlogCost = backlogCallDataUnits * header.BaseFee.Int64()
		} else if header.BlobGasUsed == nil || header.ExcessBlobGas == nil {
			if !s.expectedSurplusUpdated {
				// only print notification once
				log.Info("expected surplus calculation is set to use blob price but latest parent chain header has BlobGasUsed or ExcessBlobGas as nil, falling back to calldata price model")
			}
			backlogCost = backlogCallDataUnits * header.BaseFee.Int64()
		} else {
			blobFeePerByte, err := s.parentChain.BlobFeePerByte(ctx, header)
			if err != nil {
				return 0, fmt.Errorf("error encountered getting blob base fee while updating expectedSurplus: %w", err)
			}

			// We want to calculate the following two values:
			// - l1GasPrice = (blobFeePerByte * blobTxBlobGasPerBlob) / (usableBytesInBlob * 16)
			// - backlogCost = backlogCallDataUnits * (blobFeePerByte * blobTxBlobGasPerBlob) / (usableBytesInBlob * 16)
			// If we divide by usableBytesInBlob too early, the value of blobFeePerByte becomes zero because of rounding.
			// Then even if we multiply with backlogCallDataUnits, the value will still remain zero.
			// This is why we multiply with backlogCallDataUnits before we divide.
			if backlogCallDataUnits == 0 {
				blobFeePerByte.Mul(blobFeePerByte, blobTxBlobGasPerBlob)
				blobFeePerByte.Div(blobFeePerByte, usableBytesInBlob)
				l1GasPrice = blobFeePerByte.Int64() / 16
				backlogCost = 0
			} else {
				// l1GasPrice can be zero because of roundings, hence backlogCost is calculated separately
				backlogFee := big.NewInt(backlogCallDataUnits)
				backlogFee.Mul(backlogFee, blobFeePerByte)
				backlogFee.Mul(backlogFee, blobTxBlobGasPerBlob)
				backlogFee.Div(backlogFee, usableBytesInBlob)
				backlogCost = backlogFee.Int64() / 16
				l1GasPrice = backlogCost / backlogCallDataUnits
			}
		}
	case "CalldataPrice7623":
		l1GasPrice = (header.BaseFee.Int64() * 40) / 16
		backlogCost = (backlogCallDataUnits * header.BaseFee.Int64() * 40) / 16
	default:
		return 0, fmt.Errorf("unrecognized ExpectedSurplusGasPriceMode: %s", s.config().ExpectedSurplusGasPriceMode)
	}

	surplus, err := s.execEngine.getL1PricingSurplus()
	if err != nil {
		return 0, fmt.Errorf("error encountered getting l1 pricing surplus while updating expectedSurplus: %w", err)
	}
	expectedSurplus := surplus - backlogCost

	// update metrics
	l1GasPriceGauge.Update(l1GasPrice)
	callDataUnitsBacklogGauge.Update(backlogCallDataUnits)
	currentSurplusGauge.Update(surplus)
	expectedSurplusGauge.Update(expectedSurplus)
	config := s.config()
	if config.ExpectedSurplusSoftThreshold != "default" && expectedSurplus < int64(config.expectedSurplusSoftThreshold) {
		log.Warn("expected surplus is below soft threshold", "value", expectedSurplus, "threshold", config.expectedSurplusSoftThreshold)
	}
	s.expectedSurplusFailureCount = 0
	return expectedSurplus, nil
}

// StartExpressLaneService 启动 express lane 服务。
func (s *Sequencer) StartExpressLaneService(ctx context.Context) {
	if s.expressLaneService != nil {
		s.expressLaneService.Start(ctx)
	}
}

// Start 启动 sequencer 的后台循环，包括父链订阅、surplus 更新和出块任务。
func (s *Sequencer) Start(ctxIn context.Context) error {
	s.StopWaiter.Start(ctxIn, s)

	ctx, err := s.GetContextSafe()
	if err != nil {
		return err
	}

	config := s.config()
	if (config.ExpectedSurplusHardThreshold != "default" || config.ExpectedSurplusSoftThreshold != "default") && s.l1Reader == nil {
		return errors.New("expected surplus soft/hard thresholds are enabled but l1Reader is nil")
	}

	if s.addressFilterService != nil {
		s.addressFilterService.Start(ctx)
		s.execEngine.SetAddressChecker(s.addressFilterService.GetAddressChecker())
	}

	if s.l1Reader != nil {
		initialBlockNr := s.l1BlockNumber.Load()
		if initialBlockNr == 0 {
			return errors.New("sequencer not initialized")
		}

		expectedSurplus, err := s.updateExpectedSurplus(ctxIn)
		if err != nil {
			if config.ExpectedSurplusHardThreshold != "default" {
				return fmt.Errorf("expected-surplus-hard-threshold is enabled but error fetching initial expected surplus value: %w", err)
			}
			log.Error("expected-surplus-soft-threshold is enabled but error fetching initial expected surplus value", "err", err)
		} else {
			s.expectedSurplus = expectedSurplus
			s.expectedSurplusUpdated = true
		}
		s.CallIteratively(func(ctx context.Context) time.Duration {
			expectedSurplus, err := s.updateExpectedSurplus(ctxIn)
			s.expectedSurplusMutex.Lock()
			defer s.expectedSurplusMutex.Unlock()
			if err != nil {
				s.expectedSurplusUpdated = false
				s.logExpectedSurplusError(err)
				return 0
			}
			s.expectedSurplusUpdated = true
			s.expectedSurplus = expectedSurplus
			return 5 * time.Second
		})

		headerChan, cancel := s.l1Reader.Subscribe(false)

		s.LaunchThread(func(ctx context.Context) {
			defer cancel()
			for {
				select {
				case header, ok := <-headerChan:
					if !ok {
						return
					}
					s.updateLatestParentChainBlock(header)
				case <-ctx.Done():
					return
				}
			}
		})
	}

	s.CallIteratively(func(ctx context.Context) time.Duration {
		nextBlock := time.Now().Add(s.config().MaxBlockSpeed)
		if s.createBlock(ctx) {
			// Note: this may return a negative duration, but timers are fine with that (they treat negative durations as 0).
			return time.Until(nextBlock)
		}
		// If we didn't make a block, try again immediately.
		return 0
	})

	return nil
}

// TxSource 标记关机转发阶段的交易来源队列。
type TxSource int

const (
	RetryQueue TxSource = iota + 1
	NonceFailures
	TxQueue
	TimeboostAuctionResolutionTxQueue
)

var txSources = []string{"unknown", "retryQueue", "nonceFailures", "txQueue", "timeboostAuctionResolutionTxQueue"}

// String 返回交易来源的可读字符串。
func (s TxSource) String() string {
	if int(s) > len(txSources) || s < 0 {
		return txSources[0]
	}
	return txSources[s]
}

// StopAndWait 停止 sequencer，并在可能时把剩余交易转发出去。
func (s *Sequencer) StopAndWait() {
	s.StopWaiter.StopAndWait()
	if s.addressFilterService != nil {
		s.addressFilterService.StopAndWait()
	}
	if s.config().Timeboost.Enable && s.expressLaneService != nil {
		s.expressLaneService.StopAndWait()
	}
	if s.txRetryQueue.Len() == 0 &&
		len(s.txQueue) == 0 &&
		s.nonceFailures.Len() == 0 &&
		len(s.timeboostAuctionResolutionTxQueue) == 0 {
		return
	}
	// this usually means that coordinator's safe-shutdown-delay is too low
	log.Warn("Sequencer has queued items while shutting down",
		"txQueue", len(s.txQueue),
		"retryQueue", s.txRetryQueue.Len(),
		"nonceFailures", s.nonceFailures.Len(),
		"timeboostAuctionResolutionTxQueue", len(s.timeboostAuctionResolutionTxQueue))
	_, forwarder := s.GetPauseAndForwarder()
	if forwarder != nil {
		var wg sync.WaitGroup
	emptyqueues:
		for {
			var item txQueueItem
			var source TxSource
			if s.txRetryQueue.Len() > 0 {
				item = s.txRetryQueue.Pop()
				source = RetryQueue
			} else if s.nonceFailures.Len() > 0 {
				_, failure, _ := s.nonceFailures.GetOldest()
				failure.revived = true
				item = failure.queueItem
				source = NonceFailures
				s.nonceFailures.RemoveOldest()
			} else {
				select {
				case item = <-s.txQueue:
					source = TxQueue
				case item = <-s.timeboostAuctionResolutionTxQueue:
					source = TimeboostAuctionResolutionTxQueue
				default:
					break emptyqueues
				}
			}
			wg.Add(1)
			go func(it txQueueItem, src TxSource) {
				defer wg.Done()
				var err error
				if src == TimeboostAuctionResolutionTxQueue {
					err = forwarder.PublishAuctionResolutionTransaction(it.ctx, it.tx)
				} else {
					err = forwarder.PublishTransaction(it.ctx, it.tx, it.options)
				}
				if err != nil {
					log.Warn("failed to forward transaction while shutting down", "source", src.String(), "err", err)
				}
			}(item, source)
		}
		wg.Wait()
	}
}

// cloneQueueItemsFromCandidateBlock 从候选区块中复制一份 queue items，供重建后重新排序使用。
func cloneQueueItemsFromCandidateBlock(block *CandidateBlock) []txQueueItem {
	if block == nil {
		return nil
	}
	out := make([]txQueueItem, len(block.QueueItems))
	copy(out, block.QueueItems)
	return out
}
