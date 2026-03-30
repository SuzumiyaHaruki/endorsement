package gethexec

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/offchainlabs/nitro/endorsement"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

func makeTestPolicyResolution() *endorsementpolicy.PolicyResolution {
	return &endorsementpolicy.PolicyResolution{
		Policy: &endorsementpolicy.EndorsementPolicy{
			ID: "default",
			Endorsers: endorsementpolicy.EndorserSet{
				Members: []endorsementpolicy.EndorserMember{
					{ID: "A"},
					{ID: "B"},
					{ID: "C"},
				},
			},
			Threshold: 2,
			FailMode:  endorsementpolicy.EndorsementFailDropTxAndRebuild,
		},
		MatchReason: "test",
	}
}

func makeTestTx(nonce uint64, to common.Address) *types.Transaction {
	return types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    common.Big1,
		Gas:      21000,
		GasPrice: common.Big1,
		Data:     nil,
	})
}

func makeTestReceipt(gasUsed uint64) *types.Receipt {
	return &types.Receipt{
		Status:  1,
		GasUsed: gasUsed,
	}
}

func makeCandidateBlockWith3Txs(t *testing.T) (*CandidateBlock, []*types.Transaction) {
	t.Helper()

	to1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	to3 := common.HexToAddress("0x3333333333333333333333333333333333333333")

	tx0 := makeTestTx(0, to1)
	tx1 := makeTestTx(1, to2)
	tx2 := makeTestTx(2, to3)

	policy := makeTestPolicyResolution()

	block := &CandidateBlock{
		QueueItems: []txQueueItem{
			{tx: tx0},
			{tx: tx1},
			{tx: tx2},
		},
		Txs: []*CandidateTx{
			{
				TxIndex: 0,
				Tx:      tx0,
				Receipt: nil,
				Policy:  policy,
			},
			{
				TxIndex: 1,
				Tx:      tx1,
				Receipt: nil,
				Policy:  policy,
			},
			{
				TxIndex: 2,
				Tx:      tx2,
				Receipt: nil,
				Policy:  policy,
			},
		},
	}

	if err := block.Validate(); err != nil {
		t.Fatalf("unexpected invalid candidate block: %v", err)
	}

	return block, []*types.Transaction{tx0, tx1, tx2}
}

func TestCandidateBlockValidate_OK(t *testing.T) {
	block, _ := makeCandidateBlockWith3Txs(t)

	if err := block.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestCandidateBlockValidate_LengthMismatch(t *testing.T) {
	block, _ := makeCandidateBlockWith3Txs(t)

	block.QueueItems = block.QueueItems[:2]

	err := block.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want non-nil")
	}
	if err != ErrCandidateBlockLengthMismatch {
		t.Fatalf("Validate() error = %v, want %v", err, ErrCandidateBlockLengthMismatch)
	}
}

func TestCandidateBlockValidate_IndexMismatch(t *testing.T) {
	block, _ := makeCandidateBlockWith3Txs(t)

	block.Txs[1].TxIndex = 99

	err := block.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "candidate tx index mismatch") {
		t.Fatalf("Validate() error = %v, want index mismatch", err)
	}
}

func TestCandidateBlockAttachReceipts_OK(t *testing.T) {
	block, txs := makeCandidateBlockWith3Txs(t)

	receipts := types.Receipts{
		makeTestReceipt(21000),
		makeTestReceipt(22000),
		makeTestReceipt(23000),
	}

	if err := block.AttachReceipts(receipts); err != nil {
		t.Fatalf("AttachReceipts() error = %v, want nil", err)
	}

	if !block.HasExecutionResults() {
		t.Fatal("HasExecutionResults() = false, want true")
	}

	if len(block.Receipts) != 3 {
		t.Fatalf("len(block.Receipts) = %d, want 3", len(block.Receipts))
	}

	for i := range block.Txs {
		if block.Txs[i].Receipt != receipts[i] {
			t.Fatalf("tx[%d].Receipt not attached correctly", i)
		}
		if block.Txs[i].Tx != txs[i] {
			t.Fatalf("tx[%d].Tx changed unexpectedly", i)
		}
	}
}

func TestCandidateBlockAttachReceipts_TooShort(t *testing.T) {
	block, _ := makeCandidateBlockWith3Txs(t)

	receipts := types.Receipts{
		makeTestReceipt(21000),
		makeTestReceipt(22000),
	}

	err := block.AttachReceipts(receipts)
	if err == nil {
		t.Fatal("AttachReceipts() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "receipts length is smaller than candidate tx count") {
		t.Fatalf("AttachReceipts() error = %v, want receipts too short", err)
	}
}

func TestFilterFailedTxsAndRebuildCandidateBlock_OK(t *testing.T) {
	oldBlock, oldTxs := makeCandidateBlockWith3Txs(t)

	// 模拟这是一个“执行后”的候选块，确保重建时这些字段会被清空
	oldBlock.Block = types.NewBlockWithHeader(&types.Header{
		Number:     common.Big1,
		ParentHash: common.HexToHash("0xabc"),
	})
	oldBlock.Receipts = types.Receipts{
		makeTestReceipt(21000),
		makeTestReceipt(22000),
		makeTestReceipt(23000),
	}
	oldBlock.ParentHash = common.HexToHash("0x111")
	oldBlock.BlockHash = common.HexToHash("0x222")
	oldBlock.BlockNum = 12345
	oldBlock.Txs[0].Receipt = oldBlock.Receipts[0]
	oldBlock.Txs[1].Receipt = oldBlock.Receipts[1]
	oldBlock.Txs[2].Receipt = oldBlock.Receipts[2]

	newBlock, err := FilterFailedTxsAndRebuildCandidateBlock(oldBlock, []int{1})
	if err != nil {
		t.Fatalf("FilterFailedTxsAndRebuildCandidateBlock() error = %v, want nil", err)
	}

	// 1) 长度应该减少
	if got, want := len(newBlock.Txs), 2; got != want {
		t.Fatalf("len(newBlock.Txs) = %d, want %d", got, want)
	}
	if got, want := len(newBlock.QueueItems), 2; got != want {
		t.Fatalf("len(newBlock.QueueItems) = %d, want %d", got, want)
	}

	// 2) 保留下来的应该是 old tx0 和 old tx2
	if newBlock.Txs[0].Tx != oldTxs[0] {
		t.Fatal("newBlock.Txs[0].Tx != old tx0")
	}
	if newBlock.Txs[1].Tx != oldTxs[2] {
		t.Fatal("newBlock.Txs[1].Tx != old tx2")
	}

	// 3) TxIndex 应该重新编号
	if got, want := newBlock.Txs[0].TxIndex, 0; got != want {
		t.Fatalf("newBlock.Txs[0].TxIndex = %d, want %d", got, want)
	}
	if got, want := newBlock.Txs[1].TxIndex, 1; got != want {
		t.Fatalf("newBlock.Txs[1].TxIndex = %d, want %d", got, want)
	}

	// 4) QueueItems 和 Txs 仍然要对齐
	if newBlock.QueueItems[0].tx != newBlock.Txs[0].Tx {
		t.Fatal("newBlock.QueueItems[0].tx != newBlock.Txs[0].Tx")
	}
	if newBlock.QueueItems[1].tx != newBlock.Txs[1].Tx {
		t.Fatal("newBlock.QueueItems[1].tx != newBlock.Txs[1].Tx")
	}

	// 5) Receipt 应该被清空，等待重新执行
	if newBlock.Txs[0].Receipt != nil {
		t.Fatal("newBlock.Txs[0].Receipt != nil, want nil")
	}
	if newBlock.Txs[1].Receipt != nil {
		t.Fatal("newBlock.Txs[1].Receipt != nil, want nil")
	}

	// 6) 执行后字段应全部清空
	if newBlock.Block != nil {
		t.Fatal("newBlock.Block != nil, want nil")
	}
	if newBlock.Receipts != nil {
		t.Fatal("newBlock.Receipts != nil, want nil")
	}
	if newBlock.ParentHash != (common.Hash{}) {
		t.Fatalf("newBlock.ParentHash = %v, want zero hash", newBlock.ParentHash)
	}
	if newBlock.BlockHash != (common.Hash{}) {
		t.Fatalf("newBlock.BlockHash = %v, want zero hash", newBlock.BlockHash)
	}
	if newBlock.BlockNum != 0 {
		t.Fatalf("newBlock.BlockNum = %d, want 0", newBlock.BlockNum)
	}

	// 7) 重建后的块本身仍然应当合法
	if err := newBlock.Validate(); err != nil {
		t.Fatalf("rebuilt block Validate() error = %v, want nil", err)
	}
}

func TestFilterFailedTxsAndRebuildCandidateBlock_IndexOutOfRange(t *testing.T) {
	block, _ := makeCandidateBlockWith3Txs(t)

	_, err := FilterFailedTxsAndRebuildCandidateBlock(block, []int{3})
	if err == nil {
		t.Fatal("FilterFailedTxsAndRebuildCandidateBlock() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "failed tx index out of range") {
		t.Fatalf("FilterFailedTxsAndRebuildCandidateBlock() error = %v, want out of range", err)
	}
}

func TestFindFailedTxIndexesByHash_OK(t *testing.T) {
	block, txs := makeCandidateBlockWith3Txs(t)

	indexes, err := FindFailedTxIndexesByHash(block, []common.Hash{
		txs[0].Hash(),
		txs[2].Hash(),
	})
	if err != nil {
		t.Fatalf("FindFailedTxIndexesByHash() error = %v, want nil", err)
	}

	if len(indexes) != 2 {
		t.Fatalf("len(indexes) = %d, want 2", len(indexes))
	}
	if indexes[0] != 0 || indexes[1] != 2 {
		t.Fatalf("indexes = %v, want [0 2]", indexes)
	}
}

func TestErrCandidateBlockRebuildRequired_Error(t *testing.T) {
	hash := common.HexToHash("0x1234")

	err := &ErrCandidateBlockRebuildRequired{
		Decision: &endorsement.BlockProcessingDecision{
			Rebuild: &endorsement.RebuildInstruction{
				FailedTxIndexes: []int{0, 2},
				FailedTxHashes:  []common.Hash{hash},
			},
		},
	}

	msg := err.Error()
	if !strings.Contains(msg, "candidate block rebuild required") {
		t.Fatalf("Error() = %q, want rebuild required prefix", msg)
	}
	if !strings.Contains(msg, "failedTxIndexes=[0 2]") {
		t.Fatalf("Error() = %q, want failedTxIndexes", msg)
	}
	if !strings.Contains(msg, hash.Hex()) {
		t.Fatalf("Error() = %q, want failedTxHashes", msg)
	}
}

func TestIsCandidateBlockRebuildRequired(t *testing.T) {
	err := &ErrCandidateBlockRebuildRequired{}
	if !IsCandidateBlockRebuildRequired(err) {
		t.Fatal("IsCandidateBlockRebuildRequired() = false, want true")
	}

	otherErr := ErrNilCandidateBlock
	if IsCandidateBlockRebuildRequired(otherErr) {
		t.Fatal("IsCandidateBlockRebuildRequired() = true for non-rebuild error, want false")
	}
}