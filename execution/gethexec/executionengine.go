// Copyright 2022-2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

//go:build !wasm

package gethexec

/*
#cgo CFLAGS: -g -I../../target/include/
#cgo LDFLAGS: ${SRCDIR}/../../target/lib/libstylus.a -ldl -lm
#include "arbitrator.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"encoding/binary"

	"github.com/google/uuid"

	"github.com/ethereum/go-ethereum/arbitrum/multigas"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbosState"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/l1pricing"
	"github.com/offchainlabs/nitro/arbos/programs"
	arbosutil "github.com/offchainlabs/nitro/arbos/util"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/consensus"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/execution/gethexec/eventfilter"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/sharedmetrics"
	"github.com/offchainlabs/nitro/util/stopwaiter"

	// 背书相关
	"github.com/offchainlabs/nitro/endorsementpolicy"
	"github.com/offchainlabs/nitro/endorsement"
	"encoding/json"
	"github.com/herumi/bls-eth-go-binary/bls"
)

var (
	l1GasPriceEstimateGauge              = metrics.NewRegisteredGauge("arb/l1gasprice/estimate", nil)
	baseFeeGauge                         = metrics.NewRegisteredGauge("arb/block/basefee", nil)
	blockGasUsedHistogram                = metrics.NewRegisteredHistogram("arb/block/gasused", nil, metrics.NewBoundedHistogramSample())
	txCountHistogram                     = metrics.NewRegisteredHistogram("arb/block/transactions/count", nil, metrics.NewBoundedHistogramSample())
	txGasUsedHistogram                   = metrics.NewRegisteredHistogram("arb/block/transactions/gasused", nil, metrics.NewBoundedHistogramSample())
	gasUsedSinceStartupCounter           = metrics.NewRegisteredCounter("arb/gas_used", nil)
	multiGasUsedSinceStartupCounters     = make([]*metrics.Counter, multigas.NumResourceKind)
	totalMultiGasUsedSinceStartupCounter = metrics.NewRegisteredCounter("arb/multigas_used/total", nil)
	blockExecutionTimer                  = metrics.NewRegisteredHistogram("arb/block/execution", nil, metrics.NewBoundedHistogramSample())
	blockWriteToDbTimer                  = metrics.NewRegisteredHistogram("arb/block/writetodb", nil, metrics.NewBoundedHistogramSample())
)

var (
	ExecutionEngineBlockCreationStopped = errors.New("block creation stopped in execution engine")
	ResultNotFound                      = errors.New("result not found")
	BlockNumBeforeGenesis               = errors.New("block number is before genesis")
)

// ErrFilteredDelayedMessage 表示延迟消息中包含触达受过滤地址的交易。
// Sequencer 应暂停处理，并等待这些交易哈希被加入链上过滤器后再重试。
type ErrFilteredDelayedMessage struct {
	TxHashes      []common.Hash
	DelayedMsgIdx uint64
}

func (e *ErrFilteredDelayedMessage) Error() string {
	return fmt.Sprintf("delayed message %d: %d tx(es) touch filtered addresses: %v",
		e.DelayedMsgIdx, len(e.TxHashes), e.TxHashes)
}

// ErrDelayedTxFiltered 是出块过程中使用的内部错误，
// 用于表示某笔交易触达了受过滤地址且不在链上过滤器中。
var ErrDelayedTxFiltered = errors.New("delayed transaction filtered")

// DelayedFilteringSequencingHooks 在 NoopSequencingHooks 基础上增加了地址过滤能力，
// 用于处理延迟消息。它会收集所有触达受过滤地址且不在链上过滤器中的交易哈希。
// 出块结束后，调用方会检查是否收集到了这些哈希，若有则返回 ErrFilteredDelayedMessage。
type DelayedFilteringSequencingHooks struct {
	arbos.NoopSequencingHooks
	FilteredTxHashes []common.Hash
	eventFilter      *eventfilter.EventFilter
}

func NewDelayedFilteringSequencingHooks(txes types.Transactions, ef *eventfilter.EventFilter) *DelayedFilteringSequencingHooks {
	return &DelayedFilteringSequencingHooks{
		NoopSequencingHooks: *arbos.NewNoopSequencingHooks(txes),
		eventFilter:         ef,
	}
}

// PostTxFilter 会触达交易的 To/From 地址并检查 IsAddressFiltered。
// 它会收集触达受过滤地址但不在链上过滤器中的交易哈希。
// 该方法本身不返回错误，调用方会在出块后检查 FilteredTxHashes。
func (f *DelayedFilteringSequencingHooks) PostTxFilter(header *types.Header, db *state.StateDB, a *arbosState.ArbosState, tx *types.Transaction, sender common.Address, dataGas uint64, result *core.ExecutionResult) error {
	db.TouchAddress(sender)
	if tx.To() != nil {
		db.TouchAddress(*tx.To())
	}
	// 对于会对发送者地址做别名映射的交易类型（如无签名合约交易、retryable），
	// 还需要检查原始的 L1 地址。交易里的 sender 已经被 L1 bridge 做过别名映射，
	// 但受限地址列表里保存的是原始地址，而不是映射后的地址。
	txType := tx.Type()
	if arbosutil.DoesTxTypeAlias(&txType) {
		db.TouchAddress(arbosutil.InverseRemapL1Address(sender))
	}
	touchRetryableAddresses(db, tx)
	applyEventFilter(f.eventFilter, db)

	if db.IsAddressFiltered() {
		// 如果状态转换流程已经通过链上过滤机制处理了这笔交易，
		// 那么对应过滤条目已经被清理，此处无需再处理。
		var filteredErr *core.ErrFilteredTx
		if errors.As(result.Err, &filteredErr) {
			return nil
		}
		// 否则说明这笔交易触达了受过滤地址，但并不在链上过滤器中，
		// 需要收集下来，以便调用方中止流程。
		f.FilteredTxHashes = append(f.FilteredTxHashes, tx.Hash())
	}
	return nil
}

func applyEventFilter(ef *eventfilter.EventFilter, db *state.StateDB) {
	if ef == nil {
		return
	}
	logs := db.GetCurrentTxLogs()
	for _, l := range logs {
		for _, addr := range ef.AddressesForFiltering(l.Topics, l.Data, l.Address, common.Address{}) {
			db.TouchAddress(addr)
		}
	}
}

// touchRetryableAddresses 会触达 retryable 内部字段中的地址
// （Beneficiary、FeeRefundAddr、RetryTo），以便地址过滤器能够检测到它们。
// 同时也会触达去别名后的地址，以捕获被 Inbox 合约做过别名映射的 L1 合约地址。
func touchRetryableAddresses(db *state.StateDB, tx *types.Transaction) {
	if inner, ok := tx.GetInner().(*types.ArbitrumSubmitRetryableTx); ok {
		db.TouchAddress(inner.Beneficiary)
		db.TouchAddress(inner.FeeRefundAddr)
		if inner.RetryTo != nil {
			db.TouchAddress(*inner.RetryTo)
		}
		db.TouchAddress(arbosutil.InverseRemapL1Address(inner.Beneficiary))
		db.TouchAddress(arbosutil.InverseRemapL1Address(inner.FeeRefundAddr))
	}
}

type L1PriceDataOfMsg struct {
	callDataUnits            uint64
	cummulativeCallDataUnits uint64
}

type L1PriceData struct {
	mutex                   sync.RWMutex
	startOfL1PriceDataCache arbutil.MessageIndex
	endOfL1PriceDataCache   arbutil.MessageIndex
	msgToL1PriceData        []L1PriceDataOfMsg
}

// ExecutionEngine 负责驱动 L2 消息执行、区块生成、链重组处理，
// 以及交易过滤、区块背书等辅助校验流程。
type ExecutionEngine struct {
	stopwaiter.StopWaiter

	bc        *core.BlockChain
	consensus consensus.FullConsensusClient
	recorder  *BlockRecorder

	resequenceChan    chan []*arbostypes.MessageWithMetadata
	createBlocksMutex sync.Mutex

	newBlockNotifier    chan struct{}
	reorgEventsNotifier chan struct{}
	latestBlockMutex    sync.Mutex
	latestBlock         *types.Block

	nextScheduledVersionCheck time.Time // 受 createBlocksMutex 保护

	reorgSequencing bool

	disableStylusCacheMetricsCollection bool

	prefetchBlock bool

	cachedL1PriceData *L1PriceData

	wasmTargets []rawdb.WasmTarget

	syncTillBlock uint64

	exposeMultiGas bool

	runningMaintenance atomic.Bool

	addressChecker               state.AddressChecker
	eventFilter                  *eventfilter.EventFilter
	transactionFiltererRPCClient *TransactionFiltererRPCClient

	// 背书相关
	candidateBlockEndorser  endorsement.EndorsementManager
	policyConfig           *endorsementpolicy.PolicyConfig
	// 用于从元数据中反解析 commitment data 后执行 BLS 验签
	commitmentVerifierBLSPublicKeys endorsement.BLSPublicKeyRegistry
}

// NewL1PriceData 创建一个按消息缓存 L1 定价数据的空缓存。
func NewL1PriceData() *L1PriceData {
	return &L1PriceData{
		msgToL1PriceData: []L1PriceDataOfMsg{},
	}
}

func init() {
	for dimension := multigas.ResourceKind(0); dimension < multigas.NumResourceKind; dimension++ {
		metricName := fmt.Sprintf("arb/multigas_used/%v", strings.ToLower(dimension.String()))
		multiGasUsedSinceStartupCounters[dimension] = metrics.NewRegisteredCounter(metricName, nil)
	}
}

// NewExecutionEngine 基于给定区块链构造一个执行引擎实例。
func NewExecutionEngine(bc *core.BlockChain, syncTillBlock uint64, exposeMultiGas bool) *ExecutionEngine {
	return &ExecutionEngine{
		bc:                bc,
		resequenceChan:    make(chan []*arbostypes.MessageWithMetadata),
		newBlockNotifier:  make(chan struct{}, 1),
		cachedL1PriceData: NewL1PriceData(),
		exposeMultiGas:    exposeMultiGas,
		syncTillBlock:     syncTillBlock,
	}
}

func (s *ExecutionEngine) backlogCallDataUnits() uint64 {
	s.cachedL1PriceData.mutex.RLock()
	defer s.cachedL1PriceData.mutex.RUnlock()

	size := len(s.cachedL1PriceData.msgToL1PriceData)
	if size == 0 {
		return 0
	}
	return (s.cachedL1PriceData.msgToL1PriceData[size-1].cummulativeCallDataUnits -
		s.cachedL1PriceData.msgToL1PriceData[0].cummulativeCallDataUnits +
		s.cachedL1PriceData.msgToL1PriceData[0].callDataUnits)
}

// MarkFeedStart 将 L1 定价缓存裁剪到给定消息索引之后。
func (s *ExecutionEngine) MarkFeedStart(to arbutil.MessageIndex) {
	s.cachedL1PriceData.mutex.Lock()
	defer s.cachedL1PriceData.mutex.Unlock()

	if to < s.cachedL1PriceData.startOfL1PriceDataCache {
		log.Debug("trying to trim older L1 price data cache which doesn't exist anymore")
	} else if to >= s.cachedL1PriceData.endOfL1PriceDataCache {
		s.cachedL1PriceData.startOfL1PriceDataCache = 0
		s.cachedL1PriceData.endOfL1PriceDataCache = 0
		s.cachedL1PriceData.msgToL1PriceData = []L1PriceDataOfMsg{}
	} else {
		newStart := to - s.cachedL1PriceData.startOfL1PriceDataCache + 1
		s.cachedL1PriceData.msgToL1PriceData = s.cachedL1PriceData.msgToL1PriceData[newStart:]
		s.cachedL1PriceData.startOfL1PriceDataCache = to + 1
	}
}

// PopulateStylusTargetCache 为启用的目标架构配置本地 Stylus 程序缓存。
func PopulateStylusTargetCache(targetConfig *StylusTargetConfig) error {
	localTarget := rawdb.LocalTarget()
	targets := targetConfig.WasmTargets()
	var nativeSet bool
	for _, target := range targets {
		var effectiveStylusTarget string
		switch target {
		case rawdb.TargetWavm:
			// 跳过 wavm 目标
			continue
		case rawdb.TargetArm64:
			effectiveStylusTarget = targetConfig.Arm64
		case rawdb.TargetAmd64:
			effectiveStylusTarget = targetConfig.Amd64
		case rawdb.TargetHost:
			effectiveStylusTarget = targetConfig.Host
		default:
			return fmt.Errorf("unsupported stylus target: %v", target)
		}
		isNative := target == localTarget
		err := programs.SetTarget(target, effectiveStylusTarget, isNative)
		if err != nil {
			return fmt.Errorf("failed to set stylus target: %w", err)
		}
		nativeSet = nativeSet || isNative
	}
	if !nativeSet {
		return fmt.Errorf("local target %v missing in list of archs %v", localTarget, targets)
	}
	return nil
}

// Initialize 初始化执行引擎使用的 Stylus 执行状态和相关缓存。
func (s *ExecutionEngine) Initialize(rustCacheCapacityMB uint32, targetConfig *StylusTargetConfig) error {
	if rustCacheCapacityMB != 0 {
		programs.SetWasmLruCacheCapacity(arbmath.SaturatingUMul(uint64(rustCacheCapacityMB), 1024*1024))
	}
	if err := PopulateStylusTargetCache(targetConfig); err != nil {
		return fmt.Errorf("error populating stylus target cache: %w", err)
	}
	s.wasmTargets = targetConfig.WasmTargets()
	return nil
}

// SetRecorder 在执行引擎启动前安装可选的区块记录器。
func (s *ExecutionEngine) SetRecorder(recorder *BlockRecorder) {
	if s.Started() {
		panic("trying to set recorder after start")
	}
	if s.recorder != nil {
		panic("trying to set recorder policy when already set")
	}
	s.recorder = recorder
}

// SetReorgEventsNotifier 设置一个在发生重组事件时接收通知的通道。
func (s *ExecutionEngine) SetReorgEventsNotifier(reorgEventsNotifier chan struct{}) {
	if s.Started() {
		panic("trying to set reorg events notifier after start")
	}
	if s.reorgEventsNotifier != nil {
		panic("trying to set reorg events notifier when already set")
	}
	s.reorgEventsNotifier = reorgEventsNotifier
}

// EnableReorgSequencing 启用链重组后的交易重新排序流程。
func (s *ExecutionEngine) EnableReorgSequencing() {
	if s.Started() {
		panic("trying to enable reorg sequencing after start")
	}
	if s.reorgSequencing {
		panic("trying to enable reorg sequencing when already set")
	}
	s.reorgSequencing = true
}

// DisableStylusCacheMetricsCollection 关闭 Stylus 缓存指标采集。
func (s *ExecutionEngine) DisableStylusCacheMetricsCollection() {
	if s.Started() {
		panic("trying to disable stylus cache metrics collection after start")
	}
	if s.disableStylusCacheMetricsCollection {
		panic("trying to disable stylus cache metrics collection when already set")
	}
	s.disableStylusCacheMetricsCollection = true
}

// EnablePrefetchBlock 启用在 DigestMessage 时构建预取区块。
func (s *ExecutionEngine) EnablePrefetchBlock() {
	if s.Started() {
		panic("trying to enable prefetch block after start")
	}
	if s.prefetchBlock {
		panic("trying to enable prefetch block when already set")
	}
	s.prefetchBlock = true
}

// SetConsensus 设置用于获取元数据和批次访问能力的共识客户端。
func (s *ExecutionEngine) SetConsensus(consensus consensus.FullConsensusClient) {
	if s.Started() {
		panic("trying to set transaction consensus after start")
	}
	if s.consensus != nil {
		panic("trying to set transaction consensus when already set")
	}
	s.consensus = consensus
}

// BlockMetadataAtMessageIndex 返回共识层提供的指定消息对应区块元数据。
func (s *ExecutionEngine) BlockMetadataAtMessageIndex(ctx context.Context, msgIdx arbutil.MessageIndex) (common.BlockMetadata, error) {
	if s.consensus != nil {
		return s.consensus.BlockMetadataAtMessageIndex(msgIdx).Await(ctx)
	}
	return nil, errors.New("FullConsensusClient is not accessible to execution")
}

// GetBatchFetcher 返回执行引擎当前使用的批次获取器。
func (s *ExecutionEngine) GetBatchFetcher() consensus.BatchFetcher {
	return s.consensus
}

// Reorg 将链回滚到指定消息索引处，并应用新的替换消息。
func (s *ExecutionEngine) Reorg(msgIdxOfFirstMsgToAdd arbutil.MessageIndex, newMessages []arbostypes.MessageWithMetadataAndBlockInfo, oldMessages []*arbostypes.MessageWithMetadata) ([]*execution.MessageResult, error) {
	if msgIdxOfFirstMsgToAdd == 0 {
		return nil, errors.New("cannot reorg out genesis")
	}

	s.createBlocksMutex.Lock()
	resequencing := false
	defer func() {
		// 如果正在对旧消息重新排序，就不要在这里释放锁，
		// 锁会由监听 resequenceChan 的线程释放。
		if !resequencing {
			s.createBlocksMutex.Unlock()
		}
	}()
	lastBlockNumToKeep := s.MessageIndexToBlockNumber(msgIdxOfFirstMsgToAdd - 1)
	// lastBlockNumToKeep 来自 MessageIndexToBlockNumber，因此可以安全转换为 uint64。
	lastBlockToKeep := s.bc.GetBlockByNumber(uint64(lastBlockNumToKeep))
	if lastBlockToKeep == nil {
		log.Warn("reorg target block not found", "block", lastBlockNumToKeep)
		return nil, nil
	}

	currentSafeBlock := s.bc.CurrentSafeBlock()
	if currentSafeBlock != nil && lastBlockToKeep.Number().Cmp(currentSafeBlock.Number) < 0 {
		log.Warn("reorg target block is below safe block", "lastBlockNumToKeep", lastBlockNumToKeep, "currentSafeBlock", currentSafeBlock.Number)
		s.bc.SetSafe(nil)
	}
	currentFinalBlock := s.bc.CurrentFinalBlock()
	if currentFinalBlock != nil && lastBlockToKeep.Number().Cmp(currentFinalBlock.Number) < 0 {
		log.Warn("reorg target block is below final block", "lastBlockNumToKeep", lastBlockNumToKeep, "currentFinalBlock", currentFinalBlock.Number)
		s.bc.SetFinalized(nil)
	}

	tag := core.NewMessageCommitContext(nil).WasmCacheTag() // we don't pass any targets, we just want the tag
	// 重组 Rust 侧的 VM 状态
	C.stylus_reorg_vm(C.uint64_t(lastBlockNumToKeep), C.uint32_t(tag))

	err := s.bc.ReorgToOldBlock(lastBlockToKeep)
	if err != nil {
		return nil, err
	}

	if s.reorgEventsNotifier != nil {
		select {
		case s.reorgEventsNotifier <- struct{}{}:
		default:
		}
	}

	newMessagesResults := make([]*execution.MessageResult, 0, len(newMessages))
	for i := range newMessages {
		var msgForPrefetch *arbostypes.MessageWithMetadata
		if i < len(newMessages)-1 {
			msgForPrefetch = &newMessages[i].MessageWithMeta
		}
		nextMsgIdx := msgIdxOfFirstMsgToAdd + arbutil.MessageIndex(i)
		msgResult, err := s.digestMessageWithBlockMutex(nextMsgIdx, &newMessages[i].MessageWithMeta, msgForPrefetch)
		if err != nil {
			return nil, err
		}
		newMessagesResults = append(newMessagesResults, msgResult)
	}
	if s.recorder != nil {
		s.recorder.ReorgTo(lastBlockToKeep.Header())
	}
	if len(oldMessages) > 0 {
		s.resequenceChan <- oldMessages
		resequencing = true
	}
	return newMessagesResults, nil
}

func (s *ExecutionEngine) getCurrentHeader() (*types.Header, error) {
	currentBlock := s.bc.CurrentBlock()
	if currentBlock == nil {
		return nil, errors.New("failed to get current block")
	}
	return currentBlock, nil
}

// HeadMessageIndex 返回当前头区块对应的消息索引。
func (s *ExecutionEngine) HeadMessageIndex() (arbutil.MessageIndex, error) {
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return 0, err
	}
	return s.BlockNumberToMessageIndex(currentHeader.Number.Uint64())
}

// HeadMessageIndexSync 是测试辅助函数，会在持有出块锁时读取头消息索引。
func (s *ExecutionEngine) HeadMessageIndexSync(t *testing.T) (arbutil.MessageIndex, error) {
	s.createBlocksMutex.Lock()
	defer s.createBlocksMutex.Unlock()
	return s.HeadMessageIndex()
}

// NextDelayedMessageNumber 返回下一个延迟消息区块期望使用的延迟消息索引。
func (s *ExecutionEngine) NextDelayedMessageNumber() (uint64, error) {
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return 0, err
	}
	return currentHeader.Nonce.Uint64(), nil
}

// 调用方必须持有 createBlocksMutex。
func (s *ExecutionEngine) resequenceReorgedMessages(messages []*arbostypes.MessageWithMetadata) {
	if !s.reorgSequencing {
		return
	}

	log.Info("Trying to resequence messages", "number", len(messages))
	lastBlockHeader, err := s.getCurrentHeader()
	if err != nil {
		log.Error("block header not found during resequence", "err", err)
		return
	}

	nextDelayedMsgIdx := lastBlockHeader.Nonce.Uint64()

	for _, msg := range messages {
		// 出于稳妥考虑，先检查消息是否为 nil。
		if msg == nil || msg.Message == nil || msg.Message.Header == nil {
			continue
		}
		header := msg.Message.Header
		if header.RequestId != nil {
			delayedMsgIdx := header.RequestId.Big().Uint64()
			if delayedMsgIdx != nextDelayedMsgIdx {
				log.Info("not resequencing delayed message due to unexpected index", "expected", nextDelayedMsgIdx, "found", delayedMsgIdx)
				continue
			}
			_, err := s.sequenceDelayedMessageWithBlockMutex(msg.Message, delayedMsgIdx)
			if err != nil {
				log.Error("failed to re-sequence old delayed message removed by reorg", "err", err)
			}
			nextDelayedMsgIdx += 1
			continue
		}
		if header.Kind != arbostypes.L1MessageType_L2Message || header.Poster != l1pricing.BatchPosterAddress {
			// 这种消息理论上不应该出现。
			log.Warn("skipping non-standard sequencer message found from reorg", "header", header)
			continue
		}
		lastArbosVersion := types.DeserializeHeaderExtraInformation(lastBlockHeader).ArbOSFormatVersion
		txes, err := arbos.ParseL2Transactions(msg.Message, s.bc.Config().ChainID, lastArbosVersion)
		if err != nil {
			log.Warn("failed to parse sequencer message found from reorg", "err", err)
			continue
		}
		hooks := MakeZeroTxSizeSequencingHooksForTesting(txes, nil, nil, nil)
		block, err := s.sequenceTransactionsWithBlockMutex(msg.Message.Header, hooks, nil)
		if err != nil {
			log.Error("failed to re-sequence old user message removed by reorg", "err", err)
			return
		}
		if block != nil {
			lastBlockHeader = block.Header()
		}
	}
}

func (s *ExecutionEngine) sequencerWrapper(sequencerFunc func() (*types.Block, error)) (*types.Block, error) {
	attempts := 0
	for {
		block, err := func() (*types.Block, error) {
			s.createBlocksMutex.Lock()
			defer s.createBlocksMutex.Unlock()
			return sequencerFunc()
		}()
		if !errors.Is(err, execution.ErrSequencerInsertLockTaken) {
			return block, err
		}
		// 遇到了 SequencerInsertLockTaken。
		// 情况 1：发生了竞争，我们已经不是主 sequencer 了。
		_, chosenErr := s.consensus.ExpectChosenSequencer().Await(s.GetContext())
		if chosenErr != nil {
			return nil, chosenErr
		}
		// 情况 2：当前处于测试环境，sequencer 协调不够严格。
		if !s.bc.Config().ArbitrumChainParams.AllowDebugPrecompiles {
			// 情况 3：出现了异常情况，打印警告。
			log.Warn("sequence transactions: insert lock takent", "attempts", attempts)
		}
		// 情况 2/3 在重试过多次后也会失败。
		attempts++
		if attempts > 20 {
			return nil, err
		}
		<-time.After(time.Millisecond * 100)
	}
}

// SequenceTransactions 将 hooks 提供的交易打包并执行为下一个区块。
func (s *ExecutionEngine) SequenceTransactions(header *arbostypes.L1IncomingMessageHeader, hooks *FullSequencingHooks, timeboostedTxs map[common.Hash]struct{}) (*types.Block, error) {
	return s.sequencerWrapper(func() (*types.Block, error) {
		return s.sequenceTransactionsWithBlockMutex(header, hooks, timeboostedTxs)
	})
}

// SequenceTransactionsWithProfiling 在启用跟踪和 CPU 性能分析的情况下执行 SequenceTransactions。
// 如果出块耗时超过 2 秒，会保留对应的分析文件并在日志中输出文件名。
func (s *ExecutionEngine) SequenceTransactionsWithProfiling(header *arbostypes.L1IncomingMessageHeader, hooks *FullSequencingHooks, timeboostedTxs map[common.Hash]struct{}) (*types.Block, error) {
	pprofBuf, traceBuf := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	if err := pprof.StartCPUProfile(pprofBuf); err != nil {
		log.Error("Starting CPU profiling", "error", err)
	}
	if err := trace.Start(traceBuf); err != nil {
		log.Error("Starting tracing", "error", err)
	}
	start := time.Now()
	res, err := s.SequenceTransactions(header, hooks, timeboostedTxs)
	elapsed := time.Since(start)
	pprof.StopCPUProfile()
	trace.Stop()
	if elapsed > 2*time.Second {
		writeAndLog(pprofBuf, traceBuf)
		return res, err
	}
	return res, err
}

func writeAndLog(pprof, trace *bytes.Buffer) {
	id := uuid.NewString()
	pprofFile := path.Join(os.TempDir(), id+".pprof")
	if err := os.WriteFile(pprofFile, pprof.Bytes(), 0o600); err != nil {
		log.Error("Creating temporary file for pprof", "fileName", pprofFile, "error", err)
		return
	}
	traceFile := path.Join(os.TempDir(), id+".trace")
	if err := os.WriteFile(traceFile, trace.Bytes(), 0o600); err != nil {
		log.Error("Creating temporary file for trace", "fileName", traceFile, "error", err)
		return
	}
	log.Info("Transactions sequencing took longer than 2 seconds, created pprof and trace files", "pprof", pprofFile, "traceFile", traceFile)
}


func (s *ExecutionEngine) sequenceTransactionsWithBlockMutex(header *arbostypes.L1IncomingMessageHeader, hooks *FullSequencingHooks, timeboostedTxs map[common.Hash]struct{}) (*types.Block, error) {
	lastBlockHeader, err := s.getCurrentHeader()
	if err != nil {
		return nil, err
	}

	statedb, err := s.bc.StateAt(lastBlockHeader.Root)
	if err != nil {
		return nil, err
	}
	if s.addressChecker != nil {
		statedb.SetAddressChecker(s.addressChecker)
	}
	lastBlock := s.bc.GetBlock(lastBlockHeader.Hash(), lastBlockHeader.Number.Uint64())
	if lastBlock == nil {
		return nil, errors.New("can't find block for current header")
	}
	var witness *stateless.Witness
	var witnessStats *stateless.WitnessStats
	if s.bc.GetVMConfig().StatelessSelfValidation {
		witness, err = stateless.NewWitness(lastBlock.Header(), s.bc)
		if err != nil {
			return nil, err
		}
		if s.bc.GetVMConfig().EnableWitnessStats {
			witnessStats = stateless.NewWitnessStats()
		}
	}
	statedb.StartPrefetcher("Sequencer", witness, witnessStats)
	defer statedb.StopPrefetcher()
	delayedMessagesRead := lastBlockHeader.Nonce.Uint64()

	startTime := time.Now()
	block, receipts, err := arbos.ProduceBlockAdvanced(
		header,
		delayedMessagesRead,
		lastBlockHeader,
		statedb,
		s.bc,
		hooks,
		false,
		core.NewMessageCommitContext(s.wasmTargets),
		s.exposeMultiGas,
	)
	if err != nil {
		return nil, err
	}

	// 先对候选块做背书；若失败则直接返回 rebuild 错误，不提交任何结果
	decision, err := s.processCandidateBlockEndorsement(s.GetContext(), block, receipts, hooks)
	if err != nil {
		return nil, err
	}

	blockCalcTime := time.Since(startTime)
	blockExecutionTimer.Update(blockCalcTime.Nanoseconds())

	if len(receipts) == 0 {
		return nil, nil
	}

	allTxsErrored := true
	for _, err := range hooks.txErrors {
		if err == nil {
			allTxsErrored = false
			break
		}
	}
	if allTxsErrored {
		return nil, nil
	}

	msg, err := hooks.MessageFromTxes(header)
	if err != nil {
		return nil, err
	}

	msgIdx, err := s.BlockNumberToMessageIndex(lastBlockHeader.Number.Uint64() + 1)
	if err != nil {
		return nil, err
	}

	msgWithMeta := arbostypes.MessageWithMetadata{
		Message:             msg,
		DelayedMessagesRead: delayedMessagesRead,
	}
	msgResult, err := s.resultFromHeader(block.Header())
	if err != nil {
		return nil, err
	}

	blockMetadata := s.blockMetadataFromDecision(block, timeboostedTxs, decision)

	parsedMeta, parseErr := ParseBlockMetadata(blockMetadata)
	if parseErr != nil {
		log.Warn("ENDORSEMENT_METADATA_BUILD_PARSE_FAILED",
			"block", block.NumberU64(),
			"hash", block.Hash(),
			"err", parseErr,
		)
	} else {
		log.Info("ENDORSEMENT_METADATA_BUILT",
			"block", block.NumberU64(),
			"hash", block.Hash(),
			"metadataVersion", parsedMeta.Version,
			"flags", parsedMeta.Flags,
			"timeboostLen", len(parsedMeta.Timeboost),
			"hasCommitmentRoot", parsedMeta.CommitmentRoot != (common.Hash{}),
			"commitmentRoot", parsedMeta.CommitmentRoot,
			"commitmentDataLen", len(parsedMeta.CommitmentData),
		)

		if parsedMeta.CommitmentRoot != (common.Hash{}) && len(parsedMeta.CommitmentData) > 0 {
			parsedCerts, certErr := endorsement.ParseCommitmentData(parsedMeta.CommitmentData)
			if certErr != nil {
				log.Warn("ENDORSEMENT_COMMITMENTDATA_PARSE_FAILED",
					"block", block.NumberU64(),
					"hash", block.Hash(),
					"commitmentRoot", parsedMeta.CommitmentRoot,
					"commitmentDataLen", len(parsedMeta.CommitmentData),
					"err", certErr,
				)
			} else {
				recomputedRoot, _, rootErr := (&endorsement.DefaultRootBuilder{}).BuildRoot(parsedCerts)
				if rootErr != nil {
					log.Warn("ENDORSEMENT_COMMITMENTDATA_REBUILD_ROOT_FAILED",
						"block", block.NumberU64(),
						"hash", block.Hash(),
						"certCount", len(parsedCerts),
						"err", rootErr,
					)
				} else {
					log.Info("ENDORSEMENT_COMMITMENTDATA_PARSED",
						"block", block.NumberU64(),
						"hash", block.Hash(),
						"certCount", len(parsedCerts),
						"commitmentRoot", parsedMeta.CommitmentRoot,
						"recomputedRoot", recomputedRoot,
						"rootMatch", recomputedRoot == parsedMeta.CommitmentRoot,
					)
				}
				verifyErr := s.verifyParsedCommitmentCertificates(block, receipts, parsedCerts, hooks)
				log.Info("ENDORSEMENT_COMMITMENT_CERT_VERIFY_INPUT",
					"block", block.NumberU64(),
					"hash", block.Hash(),
					"blockTxCount", len(block.Transactions()),
					"receiptCount", len(receipts),
					"parsedCertCount", len(parsedCerts),
				)
				if verifyErr != nil {
					return nil, fmt.Errorf("commitment certificate verification failed: %w", verifyErr)
				}
				log.Info("ENDORSEMENT_COMMITMENT_CERT_VERIFY_OK",
					"block", block.NumberU64(),
					"hash", block.Hash(),
					"certCount", len(parsedCerts),
				)	
			}
		}
	}

	_, err = s.consensus.WriteMessageFromSequencer(msgIdx, msgWithMeta, *msgResult, blockMetadata).Await(s.GetContext())
	if err != nil {
		return nil, err
	}

	// 只有在消息写入完成后才写区块，这样如果节点在中途退出，
	// 启动时就能通过重新生成缺失区块来自然恢复。
	err = s.appendBlock(block, statedb, receipts, blockCalcTime)
	if err != nil {
		return nil, err
	}
	s.cacheL1PriceDataOfMsg(msgIdx, block, false)

	return block, nil
}


// 区块元数据版本定义
const (
	blockMetadataVersionTimeboostOnly   byte = 0
	blockMetadataVersionWithEndorsement byte = 1
)

const (
	blockMetadataFlagTimeboost   byte = 1 << 0
	blockMetadataFlagEndorsement byte = 1 << 1
)

// buildTimeboostMetadataPayload 仅生成 timeboost 负载内容。
func (s *ExecutionEngine) buildTimeboostMetadataPayload(
	block *types.Block,
	timeboostedTxs map[common.Hash]struct{},
) []byte {
	if block == nil {
		return nil
	}

	// 这里只生成纯负载，不再在第一个字节写入版本号。
	bits := make([]byte, arbmath.DivCeil(uint64(len(block.Transactions())), 8))
	if len(timeboostedTxs) == 0 {
		return bits
	}

	for i, tx := range block.Transactions() {
		if _, ok := timeboostedTxs[tx.Hash()]; ok {
			bits[i/8] |= 1 << (i % 8)
		}
	}
	return bits
}

func (s *ExecutionEngine) blockMetadataFromDecision(
	block *types.Block,
	timeboostedTxs map[common.Hash]struct{},
	decision *endorsement.BlockProcessingDecision,
) common.BlockMetadata {
	// 只要区块里有交易，哪怕没有任何一笔交易经过 timeboost，hasTimeboost 也可能为 true，
	// 因为负载内容即使全为 0，长度也仍然大于 0。
	// 所以 hasTimeboost 现在实际表示的是“是否存在 timeboost 负载段”，
	// 而不是“是否至少存在一笔经过 timeboost 的交易”。
	timeboostPayload := s.buildTimeboostMetadataPayload(block, timeboostedTxs)

	hasTimeboost := len(timeboostPayload) > 0
	// 只有 CommitmentRoot 和 CommitmentData 都非空时，才会走版本 1。
	// 未来需要考虑是否存在只有 CommitmentRoot 而没有 CommitmentData 的情况。
	hasEndorsement := decision != nil &&
		decision.AllSatisfied &&
		decision.CommitmentRoot != (common.Hash{}) &&
		len(decision.CommitmentData) > 0

	// 如果没有背书信息，就保持旧格式（版本 0）返回，
	// 避免影响现有依赖旧 metadata 格式的处理路径。
	if !hasEndorsement {
		bits := make(common.BlockMetadata, 1+len(timeboostPayload))
		bits[0] = blockMetadataVersionTimeboostOnly
		copy(bits[1:], timeboostPayload)

		log.Debug("ENDORSEMENT_METADATA_VERSION0",
			"hasDecision", decision != nil,
			"allSatisfied", decision != nil && decision.AllSatisfied,
			"timeboostPayloadLen", len(timeboostPayload),
		)
		return bits
	}

	flags := byte(0)
	if hasTimeboost {
		flags |= blockMetadataFlagTimeboost
	}
	if hasEndorsement {
		flags |= blockMetadataFlagEndorsement
	}

	// 结构为：
	// version(1) + flags(1) +
	// timeboostLen(4) + timeboostPayload +
	// commitmentRoot(32) +
	// commitmentDataLen(4) + commitmentData
	size := 1 + 1 + 4 + len(timeboostPayload) + 32 + 4 + len(decision.CommitmentData)
	out := make(common.BlockMetadata, 0, size)

	out = append(out, blockMetadataVersionWithEndorsement)
	out = append(out, flags)

	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(len(timeboostPayload)))
	out = append(out, tmp[:]...)
	out = append(out, timeboostPayload...)

	out = append(out, decision.CommitmentRoot[:]...)

	binary.BigEndian.PutUint32(tmp[:], uint32(len(decision.CommitmentData)))
	out = append(out, tmp[:]...)
	out = append(out, decision.CommitmentData...)

	log.Debug("ENDORSEMENT_METADATA_VERSION1",
		"timeboostPayloadLen", len(timeboostPayload),
		"commitmentRoot", decision.CommitmentRoot,
		"commitmentDataLen", len(decision.CommitmentData),
	)
	return out
}

// blockMetadataFromBlock 返回一个 timeboost 位图字节数组，用于表示区块中的交易是否经过 timeboost。
// blockMetadata 的第一个字节保留为版本号。
// 从第二个字节开始，第 N 笔交易对应第 N-1 个 bit，1 表示是，0 表示否。
// 可通过 blockMetadata[index / 8 + 1] & (1 << (index % 8)) != 0 判断，其中 index = N - 1。
// 注意，区块中的交易数通常会小于 (len(blockMetadata) - 1) * 8，但最多只会少 7。
func (s *ExecutionEngine) blockMetadataFromBlock(block *types.Block, timeboostedTxs map[common.Hash]struct{}) common.BlockMetadata {
	// bits := make(common.BlockMetadata, 1+arbmath.DivCeil(uint64(len(block.Transactions())), 8))
	// if len(timeboostedTxs) == 0 {
	// 	return bits
	// }
	// for i, tx := range block.Transactions() {
	// 	if _, ok := timeboostedTxs[tx.Hash()]; ok {
	// 		bits[1+i/8] |= 1 << (i % 8)
	// 	}
	// }
	// return bits
	return s.blockMetadataFromDecision(block, timeboostedTxs, nil)
}

// SequenceDelayedMessage 执行一条延迟收件箱消息并追加生成的区块。
func (s *ExecutionEngine) SequenceDelayedMessage(message *arbostypes.L1IncomingMessage, delayedMsgIdx uint64) error {
	_, err := s.sequencerWrapper(func() (*types.Block, error) {
		return s.sequenceDelayedMessageWithBlockMutex(message, delayedMsgIdx)
	})
	return err
}

func (s *ExecutionEngine) sequenceDelayedMessageWithBlockMutex(message *arbostypes.L1IncomingMessage, delayedMsgIdx uint64) (*types.Block, error) {
	if s.syncTillBlock > 0 && s.latestBlock != nil && s.latestBlock.NumberU64() >= s.syncTillBlock {
		return nil, ExecutionEngineBlockCreationStopped
	}
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return nil, err
	}

	expectedDelayedMsgIdx := currentHeader.Nonce.Uint64()

	msgIdx, err := s.BlockNumberToMessageIndex(currentHeader.Number.Uint64() + 1)
	if err != nil {
		return nil, err
	}

	if expectedDelayedMsgIdx != delayedMsgIdx {
		return nil, fmt.Errorf("wrong delayed message sequenced got %d expected %d", delayedMsgIdx, expectedDelayedMsgIdx)
	}

	messageWithMeta := arbostypes.MessageWithMetadata{
		Message:             message,
		DelayedMessagesRead: delayedMsgIdx + 1,
	}

	startTime := time.Now()
	block, statedb, receipts, err := s.createBlockFromNextMessage(&messageWithMeta, false, true)
	if err != nil {
		return nil, err
	}
	blockCalcTime := time.Since(startTime)
	blockExecutionTimer.Update(blockCalcTime.Nanoseconds())

	msgResult, err := s.resultFromHeader(block.Header())
	if err != nil {
		return nil, err
	}

	_, err = s.consensus.WriteMessageFromSequencer(msgIdx, messageWithMeta, *msgResult, s.blockMetadataFromBlock(block, nil)).Await(s.GetContext())
	if err != nil {
		return nil, err
	}

	err = s.appendBlock(block, statedb, receipts, blockCalcTime)
	if err != nil {
		return nil, err
	}
	s.cacheL1PriceDataOfMsg(msgIdx, block, true)

	log.Info("ExecutionEngine: Added DelayedMessages", "msgIdx", msgIdx, "delayedMsgIdx", delayedMsgIdx, "block-header", block.Header())

	return block, nil
}

// GetGenesisBlockNumber 返回配置中的 L2 创世区块号。
func (s *ExecutionEngine) GetGenesisBlockNumber() uint64 {
	return s.bc.Config().ArbitrumChainParams.GenesisBlockNum
}

// BlockNumberToMessageIndex 将 L2 区块号映射为对应的消息索引。
func (s *ExecutionEngine) BlockNumberToMessageIndex(blockNum uint64) (arbutil.MessageIndex, error) {
	genesis := s.GetGenesisBlockNumber()
	if blockNum < genesis {
		return 0, fmt.Errorf("%w: blockNum %d < genesis %d", BlockNumBeforeGenesis, blockNum, genesis)
	}
	return arbutil.MessageIndex(blockNum - genesis), nil
}

// MessageIndexToBlockNumber 将消息索引映射为其生成的区块号。
func (s *ExecutionEngine) MessageIndexToBlockNumber(msgIdx arbutil.MessageIndex) uint64 {
	return uint64(msgIdx) + s.GetGenesisBlockNumber()
}

// 调用方必须持有 createBlockMutex。
func (s *ExecutionEngine) createBlockFromNextMessage(msg *arbostypes.MessageWithMetadata, isMsgForPrefetch bool, applyDelayedFilter bool) (*types.Block, *state.StateDB, types.Receipts, error) {
	currentHeader := s.bc.CurrentBlock()
	if currentHeader == nil {
		return nil, nil, nil, errors.New("failed to get current block header")
	}

	currentBlock := s.bc.GetBlock(currentHeader.Hash(), currentHeader.Number.Uint64())
	if currentBlock == nil {
		return nil, nil, nil, errors.New("can't find block for current header")
	}

	err := s.bc.RecoverState(currentBlock)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to recover block %v state: %w", currentBlock.Number(), err)
	}

	statedb, err := s.bc.StateAt(currentHeader.Root)
	if err != nil {
		return nil, nil, nil, err
	}

	// 如果配置了地址检查器，则在这里挂载用于过滤。
	if s.addressChecker != nil {
		statedb.SetAddressChecker(s.addressChecker)
	}

	var witness *stateless.Witness
	var witnessStats *stateless.WitnessStats
	if s.bc.GetVMConfig().StatelessSelfValidation {
		witness, err = stateless.NewWitness(currentBlock.Header(), s.bc)
		if err != nil {
			return nil, nil, nil, err
		}
		if s.bc.GetVMConfig().EnableWitnessStats {
			witnessStats = stateless.NewWitnessStats()
		}
	}
	statedb.StartPrefetcher("TransactionStreamer", witness, witnessStats)
	defer statedb.StopPrefetcher()

	var runCtx *core.MessageRunContext
	if isMsgForPrefetch {
		runCtx = core.NewMessagePrefetchContext()
	} else {
		runCtx = core.NewMessageCommitContext(s.wasmTargets)
	}

	// 对于延迟消息排序，使用支持过滤的 DelayedFilteringSequencingHooks，
	// 它可以在命中过滤地址时中止流程。这里复用了 arbos.ProduceBlock 的整体逻辑，
	// 但使用了不同的 hooks，因为我们需要访问 filteringHooks.FilteredTxHash，
	// 以便上报是哪笔交易触发了中止。
	if applyDelayedFilter {
		chainConfig := s.bc.Config()
		currentArbosVersion := types.DeserializeHeaderExtraInformation(currentHeader).ArbOSFormatVersion
		txes, err := arbos.ParseL2Transactions(msg.Message, chainConfig.ChainID, currentArbosVersion)
		if err != nil {
			log.Warn("error parsing incoming message for filtering", "err", err)
			txes = types.Transactions{}
		}
		filteringHooks := NewDelayedFilteringSequencingHooks(txes, s.eventFilter)

		block, receipts, err := arbos.ProduceBlockAdvanced(
			msg.Message.Header,
			msg.DelayedMessagesRead,
			currentHeader,
			statedb,
			s.bc,
			filteringHooks,
			isMsgForPrefetch,
			runCtx,
			s.exposeMultiGas,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		// 检查是否有交易触达了受过滤地址，但尚未进入链上过滤器。
		if len(filteringHooks.FilteredTxHashes) > 0 {
			if s.transactionFiltererRPCClient != nil {
				s.LaunchThread(func(ctx context.Context) {
					// 顺序调用 transaction-filterer。
					// 为避免向 ArbFilteredTransactionsManager 添加交易时发生 nonce 冲突，
					// transaction-filterer 一次只会处理一个 Filter 调用。
					for _, filteredTxHash := range filteringHooks.FilteredTxHashes {
						_, err := s.transactionFiltererRPCClient.Filter(filteredTxHash).Await(ctx)
						if err != nil {
							log.Error("error reporting filtered tx to transaction-filterer", "filteredTxHash", filteredTxHash, "err", err)
						}
					}
				})
			}

			return nil, nil, nil, &ErrFilteredDelayedMessage{
				TxHashes:      filteringHooks.FilteredTxHashes,
				DelayedMsgIdx: msg.DelayedMessagesRead - 1,
			}
		}
		return block, statedb, receipts, nil
	}

	block, receipts, err := arbos.ProduceBlock(
		msg.Message,
		msg.DelayedMessagesRead,
		currentHeader,
		statedb,
		s.bc,
		isMsgForPrefetch,
		runCtx,
		s.exposeMultiGas,
	)

	return block, statedb, receipts, err
}

// 调用方必须持有 createBlockMutex。
func (s *ExecutionEngine) appendBlock(block *types.Block, statedb *state.StateDB, receipts types.Receipts, duration time.Duration) error {
	var logs []*types.Log
	for _, receipt := range receipts {
		logs = append(logs, receipt.Logs...)
	}
	startTime := time.Now()
	if s.bc.GetVMConfig().Tracer != nil {
		// InsertChain 基本等价于 WriteBlockAndSetHeadWithTime 再重新计算整个区块，
		// 同时整个过程也会被 tracing 捕获，因此适合直接用于在线问题定位。
		if _, err := s.bc.InsertChain([]*types.Block{block}); err != nil {
			return err
		}
	} else {
		status, err := s.bc.WriteBlockAndSetHeadWithTime(block, receipts, logs, statedb, true, duration)
		if err != nil {
			return err
		}
		if status == core.SideStatTy { // TODO: This check can be removed as this WriteStatus is never returned when setting head
			return errors.New("geth rejected block as non-canonical")
		}
	}
	blockWriteToDbTimer.Update(time.Since(startTime).Nanoseconds())
	baseFeeGauge.Update(block.BaseFee().Int64())
	txCountHistogram.Update(int64(len(block.Transactions()) - 1))
	var blockGasused uint64
	for i := 1; i < len(receipts); i++ {
		receipt := receipts[i]
		val := arbmath.SaturatingUSub(receipt.GasUsed, receipt.GasUsedForL1)
		txGasUsedHistogram.Update(int64(val))
		blockGasused += val

		if s.exposeMultiGas {
			for kind := range multiGasUsedSinceStartupCounters {
				amount := receipt.MultiGasUsed.Get(multigas.ResourceKind(kind))
				if amount > 0 {
					multiGasUsedSinceStartupCounters[kind].Inc(int64(amount))
				}
			}
			totalMultiGasUsedSinceStartupCounter.Inc(int64(receipt.MultiGasUsed.SingleGas()))
		}
	}
	blockGasUsedHistogram.Update(int64(blockGasused))
	gasUsedSinceStartupCounter.Inc(int64(blockGasused))
	s.updateL1GasPriceEstimateMetric()
	return nil
}

func (s *ExecutionEngine) resultFromHeader(header *types.Header) (*execution.MessageResult, error) {
	if header == nil {
		return nil, ResultNotFound
	}
	info := types.DeserializeHeaderExtraInformation(header)
	return &execution.MessageResult{
		BlockHash: header.Hash(),
		SendRoot:  info.SendRoot,
	}, nil
}

// ResultAtMessageIndex 返回指定消息索引对应的已保存执行结果。
func (s *ExecutionEngine) ResultAtMessageIndex(msgIdx arbutil.MessageIndex) (*execution.MessageResult, error) {
	return s.resultFromHeader(s.bc.GetHeaderByNumber(s.MessageIndexToBlockNumber(msgIdx)))
}

func (s *ExecutionEngine) updateL1GasPriceEstimateMetric() {
	bc := s.bc
	latestHeader := bc.CurrentBlock()
	latestState, err := bc.StateAt(latestHeader.Root)
	if err != nil {
		log.Error("error getting latest statedb while fetching l2 Estimate of L1 GasPrice")
		return
	}
	arbState, err := arbosState.OpenSystemArbosState(latestState, nil, true)
	if err != nil {
		log.Error("error opening system arbos state while fetching l2 Estimate of L1 GasPrice")
		return
	}
	l2EstimateL1GasPrice, err := arbState.L1PricingState().PricePerUnit()
	if err != nil {
		log.Error("error fetching l2 Estimate of L1 GasPrice")
		return
	}
	l1GasPriceEstimateGauge.Update(l2EstimateL1GasPrice.Int64())
}

func (s *ExecutionEngine) getL1PricingSurplus() (int64, error) {
	bc := s.bc
	latestHeader := bc.CurrentBlock()
	latestState, err := bc.StateAt(latestHeader.Root)
	if err != nil {
		return 0, errors.New("error getting latest statedb while fetching current L1 pricing surplus")
	}
	arbState, err := arbosState.OpenSystemArbosState(latestState, nil, true)
	if err != nil {
		return 0, errors.New("error opening system arbos state while fetching current L1 pricing surplus")
	}
	surplus, err := arbState.L1PricingState().GetL1PricingSurplus()
	if err != nil {
		return 0, errors.New("error fetching current L1 pricing surplus")
	}
	return surplus.Int64(), nil
}

func (s *ExecutionEngine) cacheL1PriceDataOfMsg(msgIdx arbutil.MessageIndex, block *types.Block, blockBuiltUsingDelayedMessage bool) {
	var callDataUnits uint64
	if !blockBuiltUsingDelayedMessage {
		// s.cachedL1PriceData 只跟踪 Nitro 发布消息对应的 L1 定价数据，
		// 因此延迟消息不应更新其中保存的累计值。

		for _, tx := range block.Transactions() {
			_, cachedUnits := tx.GetRawCachedCalldataUnits()
			callDataUnits += cachedUnits
		}
	}

	s.cachedL1PriceData.mutex.Lock()
	defer s.cachedL1PriceData.mutex.Unlock()

	resetCache := func() {
		s.cachedL1PriceData.startOfL1PriceDataCache = msgIdx
		s.cachedL1PriceData.endOfL1PriceDataCache = msgIdx
		s.cachedL1PriceData.msgToL1PriceData = []L1PriceDataOfMsg{{
			callDataUnits:            callDataUnits,
			cummulativeCallDataUnits: callDataUnits,
		}}
	}
	size := len(s.cachedL1PriceData.msgToL1PriceData)
	if size == 0 ||
		s.cachedL1PriceData.startOfL1PriceDataCache == 0 ||
		s.cachedL1PriceData.endOfL1PriceDataCache == 0 ||
		arbutil.MessageIndex(size) != s.cachedL1PriceData.endOfL1PriceDataCache-s.cachedL1PriceData.startOfL1PriceDataCache+1 {
		resetCache()
		return
	}
	if msgIdx != s.cachedL1PriceData.endOfL1PriceDataCache+1 {
		if msgIdx > s.cachedL1PriceData.endOfL1PriceDataCache+1 {
			log.Info("message position higher then current end of l1 price data cache, resetting cache to this message")
			resetCache()
		} else if msgIdx < s.cachedL1PriceData.startOfL1PriceDataCache {
			log.Info("message position lower than start of l1 price data cache, ignoring")
		} else {
			log.Info("message position already seen in l1 price data cache, ignoring")
		}
	} else {
		cummulativeCallDataUnits := s.cachedL1PriceData.msgToL1PriceData[size-1].cummulativeCallDataUnits
		s.cachedL1PriceData.msgToL1PriceData = append(s.cachedL1PriceData.msgToL1PriceData, L1PriceDataOfMsg{
			callDataUnits:            callDataUnits,
			cummulativeCallDataUnits: cummulativeCallDataUnits + callDataUnits,
		})
		s.cachedL1PriceData.endOfL1PriceDataCache = msgIdx
	}
}

// DigestMessage 会基于最新状态执行 msg，生成并保存对应区块。
// 同时，在创建这个区块的过程中，还会并行地基于最新状态执行 msgForPrefetch（即 msg+1），
// 生成一个仅用于预取的区块，但不会将其写入存储。
// 这样可以提前填充缓存，从而加快下一次出块速度。
func (s *ExecutionEngine) DigestMessage(msgIdx arbutil.MessageIndex, msg *arbostypes.MessageWithMetadata, msgForPrefetch *arbostypes.MessageWithMetadata) (*execution.MessageResult, error) {
	if !s.createBlocksMutex.TryLock() {
		return nil, errors.New("createBlock mutex held")
	}
	defer s.createBlocksMutex.Unlock()
	return s.digestMessageWithBlockMutex(msgIdx, msg, msgForPrefetch)
}

func (s *ExecutionEngine) digestMessageWithBlockMutex(msgIdxToDigest arbutil.MessageIndex, msg *arbostypes.MessageWithMetadata, msgForPrefetch *arbostypes.MessageWithMetadata) (*execution.MessageResult, error) {
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return nil, err
	}
	curMsgIdx, err := s.BlockNumberToMessageIndex(currentHeader.Number.Uint64())
	if err != nil {
		return nil, err
	}
	if curMsgIdx+1 != msgIdxToDigest {
		return nil, fmt.Errorf("wrong message number in digest got %d expected %d", msgIdxToDigest, curMsgIdx+1)
	}

	startTime := time.Now()
	if s.prefetchBlock && msgForPrefetch != nil {
		go func() {
			_, _, _, err := s.createBlockFromNextMessage(msgForPrefetch, true, false)
			if err != nil {
				return
			}
		}()
	}

	block, statedb, receipts, err := s.createBlockFromNextMessage(msg, false, false)
	if err != nil {
		return nil, err
	}
	blockCalcTime := time.Since(startTime)
	blockExecutionTimer.Update(blockCalcTime.Nanoseconds())

	err = s.appendBlock(block, statedb, receipts, blockCalcTime)
	if err != nil {
		return nil, err
	}
	s.cacheL1PriceDataOfMsg(msgIdxToDigest, block, false)

	if time.Now().After(s.nextScheduledVersionCheck) {
		s.nextScheduledVersionCheck = time.Now().Add(time.Minute)
		arbState, err := arbosState.OpenSystemArbosState(statedb, nil, true)
		if err != nil {
			return nil, err
		}
		version, timestampInt, err := arbState.GetScheduledUpgrade()
		if err != nil {
			return nil, err
		}
		var timeUntilUpgrade time.Duration
		var timestamp time.Time
		if timestampInt == 0 {
			// 该升级会在下一个区块生效。
			timestamp = time.Now()
		} else {
			// 该升级被安排在未来某个时间生效。
			timestamp = time.Unix(int64(timestampInt), 0)
			timeUntilUpgrade = time.Until(timestamp)
		}
		logLevel := log.Warn
		if timeUntilUpgrade < time.Hour*24 {
			logLevel = log.Error
		}
		if version > params.MaxArbosVersionSupported {
			logLevel(
				"you need to update your node to the latest version before this scheduled ArbOS upgrade",
				"timeUntilUpgrade", timeUntilUpgrade,
				"upgradeScheduledFor", timestamp,
				"maxSupportedArbosVersion", params.MaxArbosVersionSupported,
				"pendingArbosUpgradeVersion", version,
			)
		}
	}

	sharedmetrics.UpdateSequenceNumberInBlockGauge(msgIdxToDigest)
	s.latestBlockMutex.Lock()
	s.latestBlock = block
	s.latestBlockMutex.Unlock()
	select {
	case s.newBlockNotifier <- struct{}{}:
	default:
	}

	msgResult, err := s.resultFromHeader(block.Header())
	if err != nil {
		return nil, err
	}
	return msgResult, nil
}

// ArbOSVersionForMessageIndex 返回指定消息索引对应 ArbOS 版本的已就绪 Promise。
func (s *ExecutionEngine) ArbOSVersionForMessageIndex(msgIdx arbutil.MessageIndex) containers.PromiseInterface[uint64] {
	block := s.bc.GetBlockByNumber(s.MessageIndexToBlockNumber(msgIdx))
	if block == nil {
		return containers.NewReadyPromise(uint64(0), fmt.Errorf("couldn't find block for message index %d", msgIdx))
	}
	extra := types.DeserializeHeaderExtraInformation(block.Header())
	return containers.NewReadyPromise(extra.ArbOSFormatVersion, nil)
}

// StopAndWait 停止执行引擎拥有的后台任务并等待其退出。
func (s *ExecutionEngine) StopAndWait() {
	if s.transactionFiltererRPCClient != nil {
		s.transactionFiltererRPCClient.StopAndWait()
	}
	s.StopWaiter.StopAndWait()
}

// Start 启动执行引擎的后台任务，包括重组重排和缓存维护等流程。
func (s *ExecutionEngine) Start(ctxIn context.Context) error {
	s.StopWaiter.Start(ctxIn, s)

	ctx, err := s.GetContextSafe()
	if err != nil {
		return err
	}

	if s.transactionFiltererRPCClient != nil {
		err := s.transactionFiltererRPCClient.Start(ctx)
		if err != nil {
			return fmt.Errorf("failed to start transaction filterer RPC client: %w", err)
		}
	}

	s.LaunchThread(func(ctx context.Context) {
		for {
			if s.syncTillBlock > 0 && s.latestBlock != nil && s.latestBlock.NumberU64() >= s.syncTillBlock {
				log.Info("stopping block creation in execution engine", "syncTillBlock", s.syncTillBlock)
				return
			}
			select {
			case <-ctx.Done():
				return
			case resequence := <-s.resequenceChan:
				s.resequenceReorgedMessages(resequence)
				s.createBlocksMutex.Unlock()
			}
		}
	})
	s.LaunchThread(func(ctx context.Context) {
		var lastBlock *types.Block
		for {
			select {
			case <-s.newBlockNotifier:
			case <-ctx.Done():
				return
			}
			s.latestBlockMutex.Lock()
			block := s.latestBlock
			s.latestBlockMutex.Unlock()
			if block != nil && (lastBlock == nil || block.Hash() != lastBlock.Hash()) {
				log.Info(
					"created block",
					"l2Block", block.Number(),
					"l2BlockHash", block.Hash(),
				)
				lastBlock = block
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return
				}
			}
		}
	})
	if !s.disableStylusCacheMetricsCollection {
		// 定期更新 stylus 缓存指标。
		s.LaunchThread(func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
					programs.UpdateWasmCacheMetrics()
				}
			}
		})
	}

	return nil
}

// ShouldTriggerMaintenance 判断是否应当尽快触发 trie flush 维护任务。
func (s *ExecutionEngine) ShouldTriggerMaintenance(trieLimitBeforeFlushMaintenance time.Duration) bool {
	if s.runningMaintenance.Load() {
		return false
	}

	procTimeBeforeFlush, err := s.bc.ProcTimeBeforeFlush()
	if err != nil {
		log.Error("failed to get time before flush", "err")
		return false
	}

	if procTimeBeforeFlush <= trieLimitBeforeFlushMaintenance/2 {
		log.Warn("Time before flush is too low, maintenance should be triggered soon", "procTimeBeforeFlush", procTimeBeforeFlush)
	}
	return procTimeBeforeFlush <= trieLimitBeforeFlushMaintenance
}

// TriggerMaintenance 异步执行 trie 数据刷盘，并在期间阻塞并发出块。
func (s *ExecutionEngine) TriggerMaintenance(capLimit uint64) {
	if s.runningMaintenance.Swap(true) {
		log.Info("Maintenance already running, skipping")
		return
	}

	// 刷新 trie DB 可能比较耗时，因此放到新线程中执行。
	s.LaunchThread(func(ctx context.Context) {
		defer s.runningMaintenance.Store(false)

		s.createBlocksMutex.Lock()
		defer s.createBlocksMutex.Unlock()

		log.Info("Flushing trie db through maintenance, it can take a while")
		err := s.bc.FlushTrieDB(common.StorageSize(capLimit))
		if err != nil {
			log.Error("Failed to flush trie db through maintenance", "err", err)
		} else {
			log.Info("Flushed trie db through maintenance completed successfully")
		}
	})
}

// MaintenanceStatus 返回当前维护任务是否正在运行。
func (s *ExecutionEngine) MaintenanceStatus() *execution.MaintenanceStatus {
	return &execution.MaintenanceStatus{
		IsRunning: s.runningMaintenance.Load(),
	}
}

// SetAddressChecker 设置状态执行期间使用的地址检查器。
func (s *ExecutionEngine) SetAddressChecker(checker state.AddressChecker) {
	s.addressChecker = checker
}

// SetEventFilter 设置排序执行期间使用的事件地址过滤器。
func (s *ExecutionEngine) SetEventFilter(ef *eventfilter.EventFilter) {
	s.eventFilter = ef
}

// SetTransactionFiltererRPCClient 设置用于上报过滤交易的 RPC 客户端。
func (s *ExecutionEngine) SetTransactionFiltererRPCClient(client *TransactionFiltererRPCClient) {
	s.transactionFiltererRPCClient = client
}

// IsTxHashInOnchainFilter 检查指定交易哈希是否已经存在于链上过滤器中。
func (s *ExecutionEngine) IsTxHashInOnchainFilter(txHash common.Hash) (bool, error) {
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return false, err
	}

	statedb, err := s.bc.StateAt(currentHeader.Root)
	if err != nil {
		return false, err
	}

	arbState, err := arbosState.OpenSystemArbosState(statedb, nil, true)
	if err != nil {
		return false, err
	}

	return arbState.FilteredTransactions().IsFiltered(txHash)
}

// 候选区块背书处理
func (s *ExecutionEngine) processCandidateBlockEndorsement(
	ctx context.Context,
	block *types.Block,
	receipts types.Receipts,
	hooks *FullSequencingHooks,
) (*endorsement.BlockProcessingDecision, error) {
	if block == nil || hooks == nil {
		return nil, nil
	}

	log.Info(
		"ENDORSEMENT_DEBUG entered processCandidateBlockEndorsement",
		"hasBlock", block != nil,
		"hasHooks", hooks != nil,
	)

	candidateBlock := hooks.CandidateBlock()
	if candidateBlock == nil {
		log.Info(
			"ENDORSEMENT_DEBUG no candidate block attached to hooks",
		)
		return nil, nil
	}

	log.Info(
		"ENDORSEMENT_DEBUG candidate block attached",
		"hasCandidateBlock", candidateBlock != nil,
		"txCount", len(candidateBlock.Txs),
		"l2Block", block.NumberU64(),
		"blockHash", block.Hash(),
	)

	// 在禁用或基线模式下：如果未接入 endorsement manager 或 policyConfig，则直接跳过背书。
	// 这样后续会按普通出块路径继续提交区块，不触发 rebuild。
	if s.candidateBlockEndorser == nil || s.policyConfig == nil {
		log.Info(
			"ENDORSEMENT_DISABLED_SKIP",
			"l2Block", block.NumberU64(),
			"blockHash", block.Hash(),
			"hasEndorser", s.candidateBlockEndorser != nil,
			"hasPolicyConfig", s.policyConfig != nil,
			"txCount", len(candidateBlock.Txs),
		)
		return nil, nil
	}

	log.Info(
		"ENDORSEMENT_DEBUG endorser wiring",
		"hasEndorser", s.candidateBlockEndorser != nil,
		"hasPolicyConfig", s.policyConfig != nil,
	)

	candidateBlock.Block = block
	candidateBlock.ParentHash = block.ParentHash()
	candidateBlock.BlockHash = block.Hash()
	candidateBlock.BlockNum = block.NumberU64()

	if err := candidateBlock.AttachReceipts(receipts); err != nil {
		log.Error(
			"ENDORSEMENT_DEBUG failed to attach receipts to candidate block",
			"l2Block", block.NumberU64(),
			"err", err,
		)
		return nil, err
	}

	if err := candidateBlock.Validate(); err != nil {
		log.Error(
			"ENDORSEMENT_DEBUG candidate block validation failed before endorsement",
			"l2Block", block.NumberU64(),
			"err", err,
		)
		return nil, err
	}

	// 转换为 endorsement 包中的 CandidateBlockInput。
	input := &endorsement.CandidateBlockInput{
		BlockHash:  candidateBlock.BlockHash,
		ParentHash: candidateBlock.ParentHash,
		BlockNum:   candidateBlock.BlockNum,
		Txs:        make([]*endorsement.CandidateTxInput, 0, len(candidateBlock.Txs)),
	}

	for _, tx := range candidateBlock.Txs {
		if tx == nil {
			return nil, ErrNilCandidateTx
		}
		input.Txs = append(input.Txs, &endorsement.CandidateTxInput{
			TxIndex: tx.TxIndex,
			Tx:      tx.Tx,
			Receipt: tx.Receipt,
			Policy:  tx.Policy,
		})
	}

	log.Info(
		"ENDORSEMENT_DEBUG calling endorsement manager",
		"l2Block", block.NumberU64(),
		"txCount", len(input.Txs),
		"timeout", s.policyConfig.BlockEndorsementTimeout,
	)

	decision, err := s.candidateBlockEndorser.ProcessCandidateBlock(ctx, s.policyConfig, input)
	if err != nil {
		log.Error(
			"ENDORSEMENT_DEBUG endorsement manager returned error",
			"l2Block", block.NumberU64(),
			"err", err,
		)
		return nil, err
	}
	if decision == nil {
		return nil, errors.New("endorsement manager returned nil decision")
	}

	log.Info(
		"ENDORSEMENT_DEBUG endorsement manager returned",
		"l2Block", block.NumberU64(),
		"allSatisfied", decision.AllSatisfied,
		"certCount", len(decision.Certificates),
	)

	if !decision.AllSatisfied {
		if decision.Rebuild != nil {
			log.Warn(
				"candidate block endorsement failed",
				"l2Block", block.NumberU64(),
				"failedTxIndexes", decision.Rebuild.FailedTxIndexes,
				"failedTxHashes", decision.Rebuild.FailedTxHashes,
			)
		} else {
			log.Warn(
				"candidate block endorsement failed",
				"l2Block", block.NumberU64(),
				"failedTxIndexes", nil,
				"failedTxHashes", nil,
			)
		}
		// 由 Sequencer 外层捕获并处理重建。
		return nil, &ErrCandidateBlockRebuildRequired{
			Decision: decision,
		}
	}

	log.Info(
		"candidate block endorsement satisfied",
		"l2Block", block.NumberU64(),
		"certCount", len(decision.Certificates),
		"commitmentRoot", decision.CommitmentRoot,
	)

	return decision, nil
}


// SetCandidateBlockEndorser 设置在区块提交前校验候选区块的背书器。
func (s *ExecutionEngine) SetCandidateBlockEndorser(m endorsement.EndorsementManager) {
	if s.candidateBlockEndorser != nil {
		panic("candidateBlockEndorser already set")
	}
	if m == nil{
		panic("candidateBlockEndorser should not be nil")
	}
	s.candidateBlockEndorser = m
}

// SetPolicyConfig 设置区块背书流程使用的策略配置。
func (s *ExecutionEngine) SetPolicyConfig(cfg *endorsementpolicy.PolicyConfig) {
	if s.policyConfig != nil {
		panic("policyConfig already set")
	}
	if cfg == nil{
		panic("policyConfig should not be nil")
	}
	s.policyConfig = cfg
}

// ParsedBlockMetadata 表示反序列化后的区块元数据结构。
type ParsedBlockMetadata struct {
	Version        byte
	Flags          byte
	Timeboost      []byte
	CommitmentRoot common.Hash
	CommitmentData []byte
}

// ParseBlockMetadata 将带版本的区块元数据解析为结构化表示。
// 当前版本 1 固定包含 timeboostLen、timeboostPayload、commitmentRoot、commitmentData，
// 不支持根据 flags 缺省字段。
func ParseBlockMetadata(meta common.BlockMetadata) (*ParsedBlockMetadata, error) {
	if len(meta) == 0 {
		return nil, fmt.Errorf("empty block metadata")
	}

	switch meta[0] {
	case blockMetadataVersionTimeboostOnly:
		return &ParsedBlockMetadata{
			Version:   meta[0],
			Timeboost: append([]byte(nil), meta[1:]...),
		}, nil

	case blockMetadataVersionWithEndorsement:
		if len(meta) < 1+1+4+32+4 {
			return nil, fmt.Errorf("block metadata too short for endorsement format")
		}

		out := &ParsedBlockMetadata{
			Version: meta[0],
			Flags:   meta[1],
		}

		offset := 2
		timeboostLen := int(binary.BigEndian.Uint32(meta[offset : offset+4]))
		offset += 4

		if len(meta) < offset+timeboostLen+32+4 {
			return nil, fmt.Errorf("block metadata truncated before commitment fields")
		}

		out.Timeboost = append([]byte(nil), meta[offset:offset+timeboostLen]...)
		offset += timeboostLen

		copy(out.CommitmentRoot[:], meta[offset:offset+32])
		offset += 32

		commitmentDataLen := int(binary.BigEndian.Uint32(meta[offset : offset+4]))
		offset += 4

		if len(meta) < offset+commitmentDataLen {
			return nil, fmt.Errorf("block metadata truncated before commitment data")
		}

		out.CommitmentData = append([]byte(nil), meta[offset:offset+commitmentDataLen]...)
		return out, nil

	default:
		return nil, fmt.Errorf("unknown block metadata version %d", meta[0])
	}
}

// SetCommitmentVerifierBLSPublicKeys 设置用于证书验签的 BLS 公钥注册表。
func (s *ExecutionEngine) SetCommitmentVerifierBLSPublicKeys(reg endorsement.BLSPublicKeyRegistry) {
	if s.commitmentVerifierBLSPublicKeys != nil {
		panic("commitmentVerifierBLSPublicKeys already set")
	}
	s.commitmentVerifierBLSPublicKeys = reg
}

func (s *ExecutionEngine) verifyParsedCommitmentCertificates(
	block *types.Block,
	receipts types.Receipts,
	parsedCerts []*endorsement.TxEndorsementCertificate,
	hooks *FullSequencingHooks,
) error {
	if block == nil {
		return errors.New("nil block")
	}
	if hooks == nil {
		return errors.New("nil hooks")
	}
	if len(parsedCerts) == 0 {
		return nil
	}
	if len(receipts) != len(block.Transactions()) {
		return fmt.Errorf("receipt count mismatch: txs=%d receipts=%d", len(block.Transactions()), len(receipts))
	}
	// 这是提交前的自校验器，不是完全独立的离线验证器。
	candidateBlock := hooks.CandidateBlock()
	if candidateBlock == nil {
		return errors.New("nil candidate block in hooks")
	}
	if len(candidateBlock.Txs) == 0 {
		return errors.New("empty candidate block txs in hooks")
	}

	// 按 TxIndex 为证书建立索引；这里不要求证书数量等于区块交易数量，
	// 只要求 metadata 中声明过的证书都能够被验证。
	certByTxIndex := make(map[int]*endorsement.TxEndorsementCertificate, len(parsedCerts))
	for _, cert := range parsedCerts {
		if cert == nil {
			return errors.New("nil certificate in parsed certificates")
		}
		if cert.TxIndex < 0 {
			return fmt.Errorf("negative certificate tx index: %d", cert.TxIndex)
		}
		if _, exists := certByTxIndex[cert.TxIndex]; exists {
			return fmt.Errorf("duplicate certificate tx index: %d", cert.TxIndex)
		}
		certByTxIndex[cert.TxIndex] = cert
	}

	builder := &endorsement.DefaultRequestBuilder{}

	for txIndex, cert := range certByTxIndex {
		if txIndex >= len(candidateBlock.Txs) {
			return fmt.Errorf("certificate tx index out of range: txIndex=%d candidateTxs=%d", txIndex, len(candidateBlock.Txs))
		}
		if txIndex >= len(receipts) {
			return fmt.Errorf("certificate tx index out of receipt range: txIndex=%d receipts=%d", txIndex, len(receipts))
		}

		tx := candidateBlock.Txs[txIndex]
		if tx == nil || tx.Tx == nil || tx.Policy == nil || tx.Policy.Policy == nil {
			return fmt.Errorf("invalid candidate tx at txIndex=%d", txIndex)
		}

		// 这里要求 cert 的 txIndex 必须和 candidate tx 的 txIndex 对齐。
		if tx.TxIndex != txIndex {
			return fmt.Errorf("candidate tx index mismatch: candidate.TxIndex=%d mapKey=%d", tx.TxIndex, txIndex)
		}

		// 用真实 receipts 回填，确保重建 digest 时与最终区块保持一致。
		Receipt := receipts[txIndex]
		req, err := builder.BuildRequest(&endorsement.CandidateBlockInput{
			BlockHash:  block.Hash(),
			ParentHash: block.ParentHash(),
			BlockNum:   block.NumberU64(),
		}, &endorsement.CandidateTxInput{
			TxIndex: tx.TxIndex,
			Tx:      tx.Tx,
			Receipt: Receipt,
			Policy:  tx.Policy,
		})
		if err != nil {
			return fmt.Errorf("build request for txIndex=%d: %w", txIndex, err)
		}

		// 基础字段一致性检查
		if cert.TxHash != tx.Tx.Hash() {
			return fmt.Errorf(
				"certificate tx hash mismatch at txIndex=%d: cert=%s tx=%s",
				txIndex, cert.TxHash.Hex(), tx.Tx.Hash().Hex(),
			)
		}
		if cert.PolicyID != tx.Policy.Policy.ID {
			return fmt.Errorf(
				"certificate policy id mismatch at txIndex=%d: cert=%s policy=%s",
				txIndex, cert.PolicyID, tx.Policy.Policy.ID,
			)
		}
		if cert.Threshold != tx.Policy.Policy.Threshold {
			return fmt.Errorf(
				"certificate threshold mismatch at txIndex=%d: cert=%d policy=%d",
				txIndex, cert.Threshold, tx.Policy.Policy.Threshold,
			)
		}
		if uint32(len(cert.SignerIDs)) < cert.Threshold {
			return fmt.Errorf(
				"certificate signer count below threshold at txIndex=%d: signers=%d threshold=%d",
				txIndex, len(cert.SignerIDs), cert.Threshold,
			)
		}

		switch tx.Policy.Policy.AggregationType {
		case endorsementpolicy.AggregationBLS:
			if err := s.verifyBLSCertificate(req, cert); err != nil {
				return fmt.Errorf("verify BLS certificate at txIndex=%d: %w", txIndex, err)
			}

		case endorsementpolicy.AggregationIndividualSignatures:
			// 第一版只做结构检查；后续如有普通签名公钥体系，再补真正验签。
			if len(cert.EncodedProof) == 0 {
				return fmt.Errorf("empty individual-signatures proof at txIndex=%d", txIndex)
			}

		case endorsementpolicy.AggregationBitmapSignatures:
			// 第一版只做结构检查；后续可以解析 bitmap 负载并校验签名集合。
			if len(cert.EncodedProof) == 0 {
				return fmt.Errorf("empty bitmap-signatures proof at txIndex=%d", txIndex)
			}

		case endorsementpolicy.AggregationCommitmentOnly:
			// commitment-only 目前只有 commitment 语义，没有可恢复的单签，因此先校验 proof 非空即可。
			if len(cert.EncodedProof) == 0 {
				return fmt.Errorf("empty commitment-only proof at txIndex=%d", txIndex)
			}

		default:
			return fmt.Errorf("unsupported aggregation type %d at txIndex=%d", tx.Policy.Policy.AggregationType, txIndex)
		}
	}

	return nil
}

var (
	gethexecBLSInitOnce sync.Once
	gethexecBLSInitErr  error
)

func ensureGethexecBLSInitialized() error {
	gethexecBLSInitOnce.Do(func() {
		gethexecBLSInitErr = bls.Init(bls.BLS12_381)
	})
	return gethexecBLSInitErr
}

type parsedBLSAggregatePayload struct {
	Scheme              string                          `json:"scheme"`
	SignerIDs           []endorsementpolicy.EndorserID `json:"signer_ids"`
	AggregatedSignature []byte                          `json:"aggregated_signature"`
}

func (s *ExecutionEngine) verifyBLSCertificate(
	req *endorsement.EndorsementRequest,
	cert *endorsement.TxEndorsementCertificate,
) error {
	if req == nil {
		return errors.New("nil endorsement request")
	}
	if cert == nil {
		return errors.New("nil certificate")
	}
	if s.commitmentVerifierBLSPublicKeys == nil {
		return errors.New("nil commitment verifier BLS public key registry")
	}
	if err := ensureGethexecBLSInitialized(); err != nil {
		return fmt.Errorf("init bls: %w", err)
	}

	var payload parsedBLSAggregatePayload
	if err := json.Unmarshal(cert.EncodedProof, &payload); err != nil {
		return fmt.Errorf("unmarshal BLS aggregate payload: %w", err)
	}
	if len(payload.AggregatedSignature) == 0 {
		return errors.New("empty aggregated signature")
	}
	if len(payload.SignerIDs) == 0 {
		return errors.New("empty signer ids in BLS payload")
	}

	// 证书外层的 signerIDs 必须与 proof 内部的 signerIDs 保持一致。
	if len(payload.SignerIDs) != len(cert.SignerIDs) {
		return fmt.Errorf("signer id count mismatch: payload=%d cert=%d", len(payload.SignerIDs), len(cert.SignerIDs))
	}
	for i := range payload.SignerIDs {
		if payload.SignerIDs[i] != cert.SignerIDs[i] {
			return fmt.Errorf("signer id mismatch at position %d: payload=%s cert=%s", i, payload.SignerIDs[i], cert.SignerIDs[i])
		}
	}

	var aggSig bls.Sign
	if err := aggSig.Deserialize(payload.AggregatedSignature); err != nil {
		return fmt.Errorf("deserialize aggregated signature: %w", err)
	}

	pubs := make([]bls.PublicKey, 0, len(payload.SignerIDs))
	for _, id := range payload.SignerIDs {
		pubBytes, err := s.commitmentVerifierBLSPublicKeys.GetPublicKey(id)
		if err != nil {
			return fmt.Errorf("get BLS public key for signer %s: %w", id, err)
		}

		var pub bls.PublicKey
		if err := pub.Deserialize(pubBytes); err != nil {
			return fmt.Errorf("deserialize BLS public key for signer %s: %w", id, err)
		}
		pubs = append(pubs, pub)
	}

	// 签名端已经改为 SignHash(req.SigningDigest[:])，
	// 所以验证端不能再用 FastAggregateVerify(msg)，
	// 而要走 VerifyAggregateHashes(hashes)。
	hashes := make([][]byte, 0, len(payload.SignerIDs))
	for range payload.SignerIDs {
		h := make([]byte, len(req.SigningDigest))
		copy(h, req.SigningDigest[:])
		hashes = append(hashes, h)
	}

	if !aggSig.VerifyAggregateHashes(pubs, hashes) {
		return errors.New("BLS VerifyAggregateHashes failed")
	}

	return nil
}
