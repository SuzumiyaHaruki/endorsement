package endorsement

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

func makeTestCollectorPolicyResolution(threshold uint32) *endorsementpolicy.PolicyResolution {
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
			Threshold:       threshold,
			FailMode:        endorsementpolicy.EndorsementFailDropTxAndRebuild,
			AggregationType: endorsementpolicy.AggregationIndividualSignatures,
		},
		MatchReason: "test",
	}
}

func makeTestCollectorTx(nonce uint64, to common.Address) *types.Transaction {
	return types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    common.Big1,
		Gas:      21000,
		GasPrice: common.Big1,
	})
}

func makeTestCollectorReceipt(gasUsed uint64) *types.Receipt {
	return &types.Receipt{
		Status:  1,
		GasUsed: gasUsed,
	}
}

func makeCollectorBlock1Tx(t *testing.T, threshold uint32) *CandidateBlockInput {
	t.Helper()

	to := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tx := makeTestCollectorTx(0, to)
	policy := makeTestCollectorPolicyResolution(threshold)

	block := &CandidateBlockInput{
		BlockHash:  common.HexToHash("0xabc1"),
		ParentHash: common.HexToHash("0xdef1"),
		BlockNum:   1,
		Txs: []*CandidateTxInput{
			{
				TxIndex: 0,
				Tx:      tx,
				Receipt: makeTestCollectorReceipt(21000),
				Policy:  policy,
			},
		},
	}
	if err := block.Validate(); err != nil {
		t.Fatalf("invalid test block: %v", err)
	}
	return block
}

func makeCollectorBlock2Txs(t *testing.T, threshold uint32) *CandidateBlockInput {
	t.Helper()

	to1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to2 := common.HexToAddress("0x2222222222222222222222222222222222222222")

	tx0 := makeTestCollectorTx(0, to1)
	tx1 := makeTestCollectorTx(1, to2)
	policy := makeTestCollectorPolicyResolution(threshold)

	block := &CandidateBlockInput{
		BlockHash:  common.HexToHash("0xabc2"),
		ParentHash: common.HexToHash("0xdef2"),
		BlockNum:   2,
		Txs: []*CandidateTxInput{
			{
				TxIndex: 0,
				Tx:      tx0,
				Receipt: makeTestCollectorReceipt(21000),
				Policy:  policy,
			},
			{
				TxIndex: 1,
				Tx:      tx1,
				Receipt: makeTestCollectorReceipt(22000),
				Policy:  policy,
			},
		},
	}
	if err := block.Validate(); err != nil {
		t.Fatalf("invalid test block: %v", err)
	}
	return block
}

func acceptResp(endorser endorsementpolicy.EndorserID) *EndorsementResponse {
	return &EndorsementResponse{
		EndorserID: endorser,
		Decision:   EndorsementDecisionAccept,
		Signature:  []byte{0x01, 0x02},
	}
}

func rejectResp(endorser endorsementpolicy.EndorserID) *EndorsementResponse {
	return &EndorsementResponse{
		EndorserID: endorser,
		Decision:   EndorsementDecisionReject,
		ReasonCode: "reject",
	}
}

func TestInMemoryResultCollector_Init_OK(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock2Txs(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v, want nil", err)
	}

	if c.states == nil {
		t.Fatal("c.states = nil, want non-nil")
	}
	if len(c.states) != 2 {
		t.Fatalf("len(c.states) = %d, want 2", len(c.states))
	}

	st0, ok := c.states[0]
	if !ok {
		t.Fatal("state for txIndex 0 missing")
	}
	if st0.total != 3 {
		t.Fatalf("st0.total = %d, want 3", st0.total)
	}
	if st0.policy == nil || st0.policy.Threshold != 2 {
		t.Fatal("st0.policy invalid")
	}
	if st0.txHash != block.Txs[0].Tx.Hash() {
		t.Fatalf("st0.txHash = %s, want %s", st0.txHash.Hex(), block.Txs[0].Tx.Hash().Hex())
	}
}

func TestInMemoryResultCollector_Init_NilBlock(t *testing.T) {
	c := &InMemoryResultCollector{}

	err := c.Init(nil)
	if err == nil {
		t.Fatal("Init() error = nil, want non-nil")
	}
	if err != ErrNilCandidateBlockInput {
		t.Fatalf("Init() error = %v, want %v", err, ErrNilCandidateBlockInput)
	}
}

func TestInMemoryResultCollector_RecordResponse_SatisfiedAtThreshold(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := c.RecordResponse(0, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if c.IsTxSatisfied(0) {
		t.Fatal("IsTxSatisfied(0) = true too early")
	}
	if c.IsTxFailed(0) {
		t.Fatal("IsTxFailed(0) = true unexpectedly")
	}

	if err := c.RecordResponse(0, "B", acceptResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if !c.IsTxSatisfied(0) {
		t.Fatal("IsTxSatisfied(0) = false, want true")
	}
	if c.IsTxFailed(0) {
		t.Fatal("IsTxFailed(0) = true, want false")
	}
	if !c.AllSatisfied() {
		t.Fatal("AllSatisfied() = false, want true")
	}
}

func TestInMemoryResultCollector_RecordResponse_FailedWhenMaxPossibleBelowThreshold(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := c.RecordResponse(0, "A", rejectResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if c.IsTxFailed(0) {
		t.Fatal("IsTxFailed(0) = true too early")
	}

	if err := c.RecordResponse(0, "B", rejectResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if !c.IsTxFailed(0) {
		t.Fatal("IsTxFailed(0) = false, want true")
	}
	if c.IsTxSatisfied(0) {
		t.Fatal("IsTxSatisfied(0) = true, want false")
	}
	if c.AllSatisfied() {
		t.Fatal("AllSatisfied() = true, want false")
	}
}

func TestInMemoryResultCollector_RecordResponse_ErrorCountsAsResponseAndMayFail(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := c.RecordResponse(0, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "B", nil, errors.New("network timeout")); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "C", nil, errors.New("network timeout")); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	if !c.IsTxFailed(0) {
		t.Fatal("IsTxFailed(0) = false, want true")
	}
}

func TestInMemoryResultCollector_RecordResponse_NilResponseTreatedAsReject(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := c.RecordResponse(0, "A", nil, nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	st := c.states[0]
	resp, ok := st.rejected["A"]
	if !ok {
		t.Fatal("rejected[A] missing, want present")
	}
	if resp == nil {
		t.Fatal("rejected[A] response = nil, want non-nil")
	}
	if resp.Decision != EndorsementDecisionReject {
		t.Fatalf("resp.Decision = %v, want reject", resp.Decision)
	}
	if resp.ReasonCode != "nil response" {
		t.Fatalf("resp.ReasonCode = %q, want %q", resp.ReasonCode, "nil response")
	}
}

func TestInMemoryResultCollector_RecordResponse_DuplicateResponseIgnored(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := c.RecordResponse(0, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "A", rejectResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	st := c.states[0]
	if len(st.responded) != 1 {
		t.Fatalf("len(st.responded) = %d, want 1", len(st.responded))
	}
	if len(st.accepted) != 1 {
		t.Fatalf("len(st.accepted) = %d, want 1", len(st.accepted))
	}
	if len(st.rejected) != 0 {
		t.Fatalf("len(st.rejected) = %d, want 0", len(st.rejected))
	}
}

func TestInMemoryResultCollector_GetAcceptedResults_OnlyAfterSatisfied(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	_, err := c.GetAcceptedResults(0)
	if err == nil {
		t.Fatal("GetAcceptedResults() error = nil, want non-nil before satisfied")
	}

	if err := c.RecordResponse(0, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "B", acceptResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	accepted, err := c.GetAcceptedResults(0)
	if err != nil {
		t.Fatalf("GetAcceptedResults() error = %v, want nil", err)
	}
	if len(accepted) != 2 {
		t.Fatalf("len(accepted) = %d, want 2", len(accepted))
	}
	if accepted["A"] == nil || accepted["B"] == nil {
		t.Fatal("accepted results missing A/B")
	}
}

func TestInMemoryResultCollector_GetFailedTxs_ReturnsFailedAndUnsatisfied(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock2Txs(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// tx0 satisfied
	if err := c.RecordResponse(0, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "B", acceptResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	// tx1 remains unsatisfied / not failed yet
	if err := c.RecordResponse(1, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	failed := c.GetFailedTxs()
	if failed == nil {
		t.Fatal("GetFailedTxs() = nil, want non-nil")
	}
	if len(failed.FailedTxIndexes) != 1 {
		t.Fatalf("len(FailedTxIndexes) = %d, want 1", len(failed.FailedTxIndexes))
	}
	if failed.FailedTxIndexes[0] != 1 {
		t.Fatalf("FailedTxIndexes[0] = %d, want 1", failed.FailedTxIndexes[0])
	}
	if len(failed.FailedTxHashes) != 1 {
		t.Fatalf("len(FailedTxHashes) = %d, want 1", len(failed.FailedTxHashes))
	}
	if failed.FailedTxHashes[0] != block.Txs[1].Tx.Hash() {
		t.Fatalf("FailedTxHashes[0] = %s, want %s", failed.FailedTxHashes[0].Hex(), block.Txs[1].Tx.Hash().Hex())
	}
}

func TestInMemoryResultCollector_MultiTxStatesIndependent(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock2Txs(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// tx0 satisfied
	if err := c.RecordResponse(0, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "B", acceptResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	// tx1 failed
	if err := c.RecordResponse(1, "A", rejectResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(1, "B", rejectResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	if !c.IsTxSatisfied(0) {
		t.Fatal("tx0 not satisfied, want satisfied")
	}
	if c.IsTxFailed(0) {
		t.Fatal("tx0 failed, want not failed")
	}
	if !c.IsTxFailed(1) {
		t.Fatal("tx1 not failed, want failed")
	}
	if c.IsTxSatisfied(1) {
		t.Fatal("tx1 satisfied, want not satisfied")
	}
	if c.AllSatisfied() {
		t.Fatal("AllSatisfied() = true, want false")
	}
}

func TestInMemoryResultCollector_RecordResponse_UnknownTxIndex(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := c.RecordResponse(99, "A", acceptResp("A"), nil)
	if err == nil {
		t.Fatal("RecordResponse() error = nil, want non-nil")
	}
}

func TestInMemoryResultCollector_GetAcceptedResults_UnknownTxIndex(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock1Tx(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	_, err := c.GetAcceptedResults(99)
	if err == nil {
		t.Fatal("GetAcceptedResults() error = nil, want non-nil")
	}
}