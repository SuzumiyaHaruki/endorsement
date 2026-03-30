package gethexec

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/offchainlabs/nitro/endorsementpolicy"
	"github.com/offchainlabs/nitro/endorsement"
)

var (
	ErrNilCandidateBlock         = errors.New("nil candidate block")
	ErrCandidateBlockLengthMismatch = errors.New("candidate block queueItems/txs length mismatch")
	ErrNilCandidateTx           = errors.New("nil candidate tx")
	ErrNilCandidateTxPolicy     = errors.New("nil candidate tx policy")
	ErrNilCandidateTxObject     = errors.New("nil candidate tx object")
)

// CandidateTx represents one tx inside the current candidate block.
//
// TxIndex is the tx's index inside the CURRENT candidate block:
// - it must match the position in CandidateBlock.Txs
// - it must also match the position in CandidateBlock.QueueItems
// - after rebuild, TxIndex should be re-numbered from 0..N-1
//
// Before execution:
//   Receipt == nil
//
// After execution:
//   Receipt is filled using receipts[TxIndex]
type CandidateTx struct {
	TxIndex int
	Tx      *types.Transaction
	Receipt *types.Receipt
	Policy  *endorsementpolicy.PolicyResolution
}

// CandidateBlock represents one candidate block under construction.
//
// QueueItems and Txs must stay aligned:
// - len(QueueItems) == len(Txs)
// - Txs[i].TxIndex == i
// - Txs[i].Tx == QueueItems[i].tx
type CandidateBlock struct {
	QueueItems []txQueueItem
	Txs        []*CandidateTx

	Block      *types.Block
	Receipts   types.Receipts
	ParentHash common.Hash
	BlockHash  common.Hash
	BlockNum   uint64
}

func (b *CandidateBlock) Validate() error {
	if b == nil {
		return ErrNilCandidateBlock
	}
	if len(b.QueueItems) != len(b.Txs) {
		return ErrCandidateBlockLengthMismatch
	}
	for i, tx := range b.Txs {
		if tx == nil {
			return ErrNilCandidateTx
		}
		if tx.Tx == nil {
			return ErrNilCandidateTxObject
		}
		if tx.Policy == nil {
			return ErrNilCandidateTxPolicy
		}
		if err := tx.Policy.Validate(); err != nil {
			return err
		}
		if tx.TxIndex != i {
			return errors.New("candidate tx index mismatch")
		}
		if b.QueueItems[i].tx != tx.Tx {
			return errors.New("candidate tx and queue item tx mismatch")
		}
	}
	return nil
}

// HasExecutionResults reports whether receipts have been attached.
func (b *CandidateBlock) HasExecutionResults() bool {
	return b != nil && len(b.Receipts) > 0
}

// AttachReceipts fills tx.Receipt from the provided receipts slice.
// It assumes tx indexes are block-local indexes.
func (b *CandidateBlock) AttachReceipts(receipts types.Receipts) error {
	if b == nil {
		return ErrNilCandidateBlock
	}
	if len(receipts) < len(b.Txs) {
		return errors.New("receipts length is smaller than candidate tx count")
	}
	b.Receipts = receipts
	for i, tx := range b.Txs {
		if tx == nil {
			return ErrNilCandidateTx
		}
		tx.Receipt = receipts[i]
	}
	return nil
}

// FilterFailedTxsAndRebuildCandidateBlock removes failed txs from the candidate block,
// rebuilds QueueItems / Txs, and re-numbers TxIndex from 0..N-1.
//
// failedTxIndexes must refer to tx indexes INSIDE THE CURRENT candidate block.
func FilterFailedTxsAndRebuildCandidateBlock(
	old *CandidateBlock,
	failedTxIndexes []int,
) (*CandidateBlock, error) {
	if old == nil {
		return nil, ErrNilCandidateBlock
	}
	if err := old.Validate(); err != nil {
		return nil, err
	}

	failed := make(map[int]struct{}, len(failedTxIndexes))
	for _, idx := range failedTxIndexes {
		if idx < 0 || idx >= len(old.Txs) {
			return nil, errors.New("failed tx index out of range")
		}
		failed[idx] = struct{}{}
	}

	newQueueItems := make([]txQueueItem, 0, len(old.QueueItems)-len(failed))
	newTxs := make([]*CandidateTx, 0, len(old.Txs)-len(failed))

	for i := range old.Txs {
		if _, drop := failed[i]; drop {
			continue
		}

		newQueueItems = append(newQueueItems, old.QueueItems[i])

		oldTx := old.Txs[i]
		newTx := &CandidateTx{
			TxIndex: len(newTxs), // 重新按新块内顺序编号
			Tx:      oldTx.Tx,
			Receipt: nil, // 重建后的候选块需要重新执行，所以 receipt 先清空
			Policy:  oldTx.Policy,
		}
		newTxs = append(newTxs, newTx)
	}

	newBlock := &CandidateBlock{
		QueueItems: newQueueItems,
		Txs:        newTxs,

		// 下面这些字段表示“执行后的候选块结果”，重建后都应该清空或重算
		Block:      nil,
		Receipts:   nil,
		ParentHash: common.Hash{},
		BlockHash:  common.Hash{},
		BlockNum:   0,
	}

	if err := newBlock.Validate(); err != nil {
		return nil, err
	}
	return newBlock, nil
}

func FindFailedTxIndexesByHash(
	block *CandidateBlock,
	failedTxHashes []common.Hash,
) ([]int, error) {
	if block == nil {
		return nil, ErrNilCandidateBlock
	}

	failedSet := make(map[common.Hash]struct{}, len(failedTxHashes))
	for _, h := range failedTxHashes {
		failedSet[h] = struct{}{}
	}

	var indexes []int
	for i, tx := range block.Txs {
		if tx == nil || tx.Tx == nil {
			return nil, ErrNilCandidateTxObject
		}
		if _, ok := failedSet[tx.Tx.Hash()]; ok {
			indexes = append(indexes, i)
		}
	}
	return indexes, nil
}

// 重建错误类型
type ErrCandidateBlockRebuildRequired struct {
	Decision *endorsement.BlockProcessingDecision
}

func (e *ErrCandidateBlockRebuildRequired) Error() string {
	if e == nil || e.Decision == nil || e.Decision.Rebuild == nil {
		return "candidate block rebuild required"
	}
	return fmt.Sprintf(
		"candidate block rebuild required: failedTxIndexes=%v failedTxHashes=%v",
		e.Decision.Rebuild.FailedTxIndexes,
		e.Decision.Rebuild.FailedTxHashes,
	)
}

func IsCandidateBlockRebuildRequired(err error) bool {
	var rebuildErr *ErrCandidateBlockRebuildRequired
	return errors.As(err, &rebuildErr)
}