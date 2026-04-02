// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package gethexec

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/endorsement"
	"github.com/ethereum/go-ethereum/trie"
)

// TestSequencerWrapperMutexReleasedOnPanic verifies that createBlocksMutex is
// properly released even when sequencerFunc panics. Without defer, a panic
// would bypass Unlock() and leave the mutex locked, causing a deadlock on the
// next call (e.g. after createBlock's recover() catches the panic).
func TestSequencerWrapperMutexReleasedOnPanic(t *testing.T) {
	engine := &ExecutionEngine{
		cachedL1PriceData: NewL1PriceData(),
	}

	// Mirrors what createBlock does: call into sequencerWrapper and recover.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected a panic but got none")
			}
		}()
		_, _ = engine.sequencerWrapper(func() (*types.Block, error) {
			panic("simulated sequencer panic")
		})
	}()

	// The mutex must be unlocked after the panic is recovered upstream.
	if !engine.createBlocksMutex.TryLock() {
		t.Fatal("createBlocksMutex is still locked after panic recovery; would deadlock on next call")
	}
	engine.createBlocksMutex.Unlock()
}

// TestSequencerWrapperMutexReleasedOnSuccess verifies that normal (non-panic)
// returns also leave the mutex unlocked.
func TestSequencerWrapperMutexReleasedOnSuccess(t *testing.T) {
	engine := &ExecutionEngine{
		cachedL1PriceData: NewL1PriceData(),
	}

	sentinel := errors.New("stop retrying")
	_, err := engine.sequencerWrapper(func() (*types.Block, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("unexpected error: %v", err)
	}

	if !engine.createBlocksMutex.TryLock() {
		t.Fatal("createBlocksMutex is still locked after normal return")
	}
	engine.createBlocksMutex.Unlock()
}

// makeTestBlockWithTxs creates a minimal block with the provided txs.
func makeTestBlockWithTxs(txs ...*types.Transaction) *types.Block {
	header := &types.Header{
		Number: common.Big1,
	}
	return types.NewBlock(
		header,
		&types.Body{Transactions: txs},
		nil,
		trie.NewStackTrie(nil),
	)
}

func makeLegacyTestTx(nonce uint64, to common.Address) *types.Transaction {
	return types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    common.Big1,
		Gas:      21000,
		GasPrice: common.Big1,
	})
}

func TestBlockMetadataFromBlock_TimeboostOnly_UsesVersion0(t *testing.T) {
	engine := &ExecutionEngine{
		cachedL1PriceData: NewL1PriceData(),
	}

	to1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to2 := common.HexToAddress("0x2222222222222222222222222222222222222222")

	tx0 := makeLegacyTestTx(0, to1)
	tx1 := makeLegacyTestTx(1, to2)
	block := makeTestBlockWithTxs(tx0, tx1)

	timeboosted := map[common.Hash]struct{}{
		tx1.Hash(): {},
	}

	meta := engine.blockMetadataFromBlock(block, timeboosted)
	if len(meta) == 0 {
		t.Fatal("metadata is empty")
	}
	if meta[0] != blockMetadataVersionTimeboostOnly {
		t.Fatalf("metadata version = %d, want %d", meta[0], blockMetadataVersionTimeboostOnly)
	}

	parsed, err := ParseBlockMetadata(meta)
	if err != nil {
		t.Fatalf("ParseBlockMetadata() error = %v, want nil", err)
	}
	if parsed.Version != blockMetadataVersionTimeboostOnly {
		t.Fatalf("parsed.Version = %d, want %d", parsed.Version, blockMetadataVersionTimeboostOnly)
	}
	if parsed.Flags != 0 {
		t.Fatalf("parsed.Flags = %d, want 0 for version 0", parsed.Flags)
	}
	if len(parsed.CommitmentData) != 0 {
		t.Fatalf("len(parsed.CommitmentData) = %d, want 0", len(parsed.CommitmentData))
	}
	if parsed.CommitmentRoot != (common.Hash{}) {
		t.Fatalf("parsed.CommitmentRoot = %s, want zero hash", parsed.CommitmentRoot.Hex())
	}

	// For 2 txs, timeboost payload should be ceil(2/8) = 1 byte.
	wantLen := int(arbmath.DivCeil(uint64(len(block.Transactions())), 8))
	if len(parsed.Timeboost) != wantLen {
		t.Fatalf("len(parsed.Timeboost) = %d, want %d", len(parsed.Timeboost), wantLen)
	}

	// tx1 is timeboosted, tx0 is not => bit 1 should be set, bit 0 unset
	if parsed.Timeboost[0]&(1<<0) != 0 {
		t.Fatal("timeboost bit for tx0 is set, want unset")
	}
	if parsed.Timeboost[0]&(1<<1) == 0 {
		t.Fatal("timeboost bit for tx1 is unset, want set")
	}
}

func TestBlockMetadataFromDecision_WithEndorsement_UsesVersion1(t *testing.T) {
	engine := &ExecutionEngine{
		cachedL1PriceData: NewL1PriceData(),
	}

	to := common.HexToAddress("0x3333333333333333333333333333333333333333")
	tx := makeLegacyTestTx(0, to)
	block := makeTestBlockWithTxs(tx)

	root := common.HexToHash("0x1234")
	data := []byte("endorsement-commitment-data")

	decision := &endorsement.BlockProcessingDecision{
		AllSatisfied:   true,
		CommitmentRoot: root,
		CommitmentData: data,
	}

	meta := engine.blockMetadataFromDecision(block, nil, decision)
	if len(meta) == 0 {
		t.Fatal("metadata is empty")
	}
	if meta[0] != blockMetadataVersionWithEndorsement {
		t.Fatalf("metadata version = %d, want %d", meta[0], blockMetadataVersionWithEndorsement)
	}

	parsed, err := ParseBlockMetadata(meta)
	if err != nil {
		t.Fatalf("ParseBlockMetadata() error = %v, want nil", err)
	}

	if parsed.Version != blockMetadataVersionWithEndorsement {
		t.Fatalf("parsed.Version = %d, want %d", parsed.Version, blockMetadataVersionWithEndorsement)
	}
	if parsed.CommitmentRoot != root {
		t.Fatalf("parsed.CommitmentRoot = %s, want %s", parsed.CommitmentRoot.Hex(), root.Hex())
	}
	if string(parsed.CommitmentData) != string(data) {
		t.Fatalf("parsed.CommitmentData = %q, want %q", string(parsed.CommitmentData), string(data))
	}

	// Even when no tx is timeboosted, version 1 format still carries the payload
	// for this block's tx count. For 1 tx, payload length should be 1 byte.
	wantLen := int(arbmath.DivCeil(uint64(len(block.Transactions())), 8))
	if len(parsed.Timeboost) != wantLen {
		t.Fatalf("len(parsed.Timeboost) = %d, want %d", len(parsed.Timeboost), wantLen)
	}
}

func TestBlockMetadataFromDecision_WithTimeboostAndEndorsement(t *testing.T) {
	engine := &ExecutionEngine{
		cachedL1PriceData: NewL1PriceData(),
	}

	to1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	tx0 := makeLegacyTestTx(0, to1)
	tx1 := makeLegacyTestTx(1, to2)
	block := makeTestBlockWithTxs(tx0, tx1)

	timeboosted := map[common.Hash]struct{}{
		tx0.Hash(): {},
	}

	root := common.HexToHash("0xabcd")
	data := []byte{0x01, 0x02, 0x03, 0x04}

	decision := &endorsement.BlockProcessingDecision{
		AllSatisfied:   true,
		CommitmentRoot: root,
		CommitmentData: data,
	}

	meta := engine.blockMetadataFromDecision(block, timeboosted, decision)
	parsed, err := ParseBlockMetadata(meta)
	if err != nil {
		t.Fatalf("ParseBlockMetadata() error = %v, want nil", err)
	}

	if parsed.Version != blockMetadataVersionWithEndorsement {
		t.Fatalf("parsed.Version = %d, want %d", parsed.Version, blockMetadataVersionWithEndorsement)
	}
	if parsed.CommitmentRoot != root {
		t.Fatalf("parsed.CommitmentRoot = %s, want %s", parsed.CommitmentRoot.Hex(), root.Hex())
	}
	if len(parsed.CommitmentData) != len(data) {
		t.Fatalf("len(parsed.CommitmentData) = %d, want %d", len(parsed.CommitmentData), len(data))
	}
	if parsed.Timeboost[0]&(1<<0) == 0 {
		t.Fatal("timeboost bit for tx0 is unset, want set")
	}
	if parsed.Timeboost[0]&(1<<1) != 0 {
		t.Fatal("timeboost bit for tx1 is set, want unset")
	}
}

func TestParseBlockMetadata_Version0LegacyFormat(t *testing.T) {
	meta := common.BlockMetadata{blockMetadataVersionTimeboostOnly, 0x05}

	parsed, err := ParseBlockMetadata(meta)
	if err != nil {
		t.Fatalf("ParseBlockMetadata() error = %v, want nil", err)
	}
	if parsed.Version != blockMetadataVersionTimeboostOnly {
		t.Fatalf("parsed.Version = %d, want %d", parsed.Version, blockMetadataVersionTimeboostOnly)
	}
	if len(parsed.Timeboost) != 1 {
		t.Fatalf("len(parsed.Timeboost) = %d, want 1", len(parsed.Timeboost))
	}
	if parsed.Timeboost[0] != 0x05 {
		t.Fatalf("parsed.Timeboost[0] = 0x%x, want 0x05", parsed.Timeboost[0])
	}
	if parsed.CommitmentRoot != (common.Hash{}) {
		t.Fatalf("parsed.CommitmentRoot = %s, want zero hash", parsed.CommitmentRoot.Hex())
	}
	if len(parsed.CommitmentData) != 0 {
		t.Fatalf("len(parsed.CommitmentData) = %d, want 0", len(parsed.CommitmentData))
	}
}

func TestParseBlockMetadata_InvalidTruncatedVersion1(t *testing.T) {
	// version=1 but far too short to contain the expected fields
	meta := common.BlockMetadata{blockMetadataVersionWithEndorsement, 0x00, 0x00}

	_, err := ParseBlockMetadata(meta)
	if err == nil {
		t.Fatal("ParseBlockMetadata() error = nil, want non-nil")
	}
}