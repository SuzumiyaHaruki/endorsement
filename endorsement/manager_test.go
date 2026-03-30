package endorsement

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

func makeTestPolicyConfig() *endorsementpolicy.PolicyConfig {
	return &endorsementpolicy.PolicyConfig{
		BlockEndorsementTimeout: 2 * time.Second,
		MaxRebuildRounds:        3,
	}
}

func makeTestPolicyResolutionForManager(threshold uint32) *endorsementpolicy.PolicyResolution {
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

func makeTestTxForManager(nonce uint64, to common.Address) *types.Transaction {
	return types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    common.Big1,
		Gas:      21000,
		GasPrice: common.Big1,
	})
}

func makeTestReceiptForManager(gasUsed uint64) *types.Receipt {
	return &types.Receipt{
		Status:  1,
		GasUsed: gasUsed,
	}
}

func makeCandidateBlockInputWith1Tx(t *testing.T, threshold uint32) *CandidateBlockInput {
	t.Helper()

	to := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tx := makeTestTxForManager(0, to)
	policy := makeTestPolicyResolutionForManager(threshold)

	block := &CandidateBlockInput{
		BlockHash:  common.HexToHash("0xabc1"),
		ParentHash: common.HexToHash("0xdef1"),
		BlockNum:   100,
		Txs: []*CandidateTxInput{
			{
				TxIndex: 0,
				Tx:      tx,
				Receipt: makeTestReceiptForManager(21000),
				Policy:  policy,
			},
		},
	}

	if err := block.Validate(); err != nil {
		t.Fatalf("invalid test block: %v", err)
	}
	return block
}

func makeCandidateBlockInputWith2Txs(t *testing.T, threshold uint32) *CandidateBlockInput {
	t.Helper()

	to1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to2 := common.HexToAddress("0x2222222222222222222222222222222222222222")

	tx0 := makeTestTxForManager(0, to1)
	tx1 := makeTestTxForManager(1, to2)
	policy := makeTestPolicyResolutionForManager(threshold)

	block := &CandidateBlockInput{
		BlockHash:  common.HexToHash("0xabc2"),
		ParentHash: common.HexToHash("0xdef2"),
		BlockNum:   101,
		Txs: []*CandidateTxInput{
			{
				TxIndex: 0,
				Tx:      tx0,
				Receipt: makeTestReceiptForManager(21000),
				Policy:  policy,
			},
			{
				TxIndex: 1,
				Tx:      tx1,
				Receipt: makeTestReceiptForManager(22000),
				Policy:  policy,
			},
		},
	}

	if err := block.Validate(); err != nil {
		t.Fatalf("invalid test block: %v", err)
	}
	return block
}

func makeDefaultTestManager(reject map[endorsementpolicy.EndorserID]bool) *DefaultEndorsementManager {
	return &DefaultEndorsementManager{
		RequestBuilder:     &DefaultRequestBuilder{},
		Client:             &MockEndorsementClient{RejectByEndorser: reject},
		Collector:          &InMemoryResultCollector{},
		CertificateBuilder: &DefaultCertificateBuilder{},
		RootBuilder:        &DefaultRootBuilder{},
	}
}

func TestDefaultEndorsementManager_ProcessCandidateBlock_AllSatisfied_SingleTx(t *testing.T) {
	manager := makeDefaultTestManager(nil)
	cfg := makeTestPolicyConfig()
	block := makeCandidateBlockInputWith1Tx(t, 2)

	decision, err := manager.ProcessCandidateBlock(context.Background(), cfg, block)
	if err != nil {
		t.Fatalf("ProcessCandidateBlock() error = %v, want nil", err)
	}
	if decision == nil {
		t.Fatal("ProcessCandidateBlock() decision = nil, want non-nil")
	}

	if !decision.AllSatisfied {
		t.Fatal("decision.AllSatisfied = false, want true")
	}
	if decision.Rebuild != nil {
		t.Fatalf("decision.Rebuild = %v, want nil", decision.Rebuild)
	}

	if len(decision.Certificates) != 1 {
		t.Fatalf("len(decision.Certificates) = %d, want 1", len(decision.Certificates))
	}

	cert := decision.Certificates[0]
	if cert == nil {
		t.Fatal("decision.Certificates[0] = nil, want non-nil")
	}
	if cert.TxIndex != 0 {
		t.Fatalf("cert.TxIndex = %d, want 0", cert.TxIndex)
	}
	if cert.TxHash != block.Txs[0].Tx.Hash() {
		t.Fatalf("cert.TxHash = %s, want %s", cert.TxHash.Hex(), block.Txs[0].Tx.Hash().Hex())
	}
	if cert.PolicyID != block.Txs[0].Policy.Policy.ID {
		t.Fatalf("cert.PolicyID = %q, want %q", cert.PolicyID, block.Txs[0].Policy.Policy.ID)
	}
	if cert.Threshold != block.Txs[0].Policy.Policy.Threshold {
		t.Fatalf("cert.Threshold = %d, want %d", cert.Threshold, block.Txs[0].Policy.Policy.Threshold)
	}
	if len(cert.SignerIDs) < int(cert.Threshold) {
		t.Fatalf("len(cert.SignerIDs) = %d, want >= %d", len(cert.SignerIDs), cert.Threshold)
	}
	if len(cert.EncodedProof) == 0 {
		t.Fatal("cert.EncodedProof is empty, want non-empty")
	}

	if decision.CommitmentRoot == (common.Hash{}) {
		t.Fatal("decision.CommitmentRoot is zero hash, want non-zero")
	}
	if len(decision.CommitmentData) == 0 {
		t.Fatal("decision.CommitmentData is empty, want non-empty")
	}
}

func TestDefaultEndorsementManager_ProcessCandidateBlock_AllSatisfied_MultiTx(t *testing.T) {
	manager := makeDefaultTestManager(nil)
	cfg := makeTestPolicyConfig()
	block := makeCandidateBlockInputWith2Txs(t, 2)

	decision, err := manager.ProcessCandidateBlock(context.Background(), cfg, block)
	if err != nil {
		t.Fatalf("ProcessCandidateBlock() error = %v, want nil", err)
	}
	if decision == nil {
		t.Fatal("ProcessCandidateBlock() decision = nil, want non-nil")
	}

	if !decision.AllSatisfied {
		t.Fatal("decision.AllSatisfied = false, want true")
	}
	if decision.Rebuild != nil {
		t.Fatalf("decision.Rebuild = %v, want nil", decision.Rebuild)
	}
	if len(decision.Certificates) != 2 {
		t.Fatalf("len(decision.Certificates) = %d, want 2", len(decision.Certificates))
	}
	if decision.CommitmentRoot == (common.Hash{}) {
		t.Fatal("decision.CommitmentRoot is zero hash, want non-zero")
	}
	if len(decision.CommitmentData) == 0 {
		t.Fatal("decision.CommitmentData is empty, want non-empty")
	}

	for i, cert := range decision.Certificates {
		if cert == nil {
			t.Fatalf("decision.Certificates[%d] = nil, want non-nil", i)
		}
		if cert.TxIndex != i {
			t.Fatalf("cert[%d].TxIndex = %d, want %d", i, cert.TxIndex, i)
		}
		if cert.TxHash != block.Txs[i].Tx.Hash() {
			t.Fatalf("cert[%d].TxHash mismatch", i)
		}
	}
}

func TestDefaultEndorsementManager_ProcessCandidateBlock_RebuildRequired_SingleTx(t *testing.T) {
	manager := makeDefaultTestManager(map[endorsementpolicy.EndorserID]bool{
		"A": true,
		"B": true,
		"C": true,
	})
	cfg := makeTestPolicyConfig()
	block := makeCandidateBlockInputWith1Tx(t, 2)

	decision, err := manager.ProcessCandidateBlock(context.Background(), cfg, block)
	if err != nil {
		t.Fatalf("ProcessCandidateBlock() error = %v, want nil", err)
	}
	if decision == nil {
		t.Fatal("ProcessCandidateBlock() decision = nil, want non-nil")
	}

	if decision.AllSatisfied {
		t.Fatal("decision.AllSatisfied = true, want false")
	}
	if decision.Rebuild == nil {
		t.Fatal("decision.Rebuild = nil, want non-nil")
	}
	if len(decision.Rebuild.FailedTxIndexes) != 1 {
		t.Fatalf("len(decision.Rebuild.FailedTxIndexes) = %d, want 1", len(decision.Rebuild.FailedTxIndexes))
	}
	if decision.Rebuild.FailedTxIndexes[0] != 0 {
		t.Fatalf("decision.Rebuild.FailedTxIndexes[0] = %d, want 0", decision.Rebuild.FailedTxIndexes[0])
	}
	if len(decision.Rebuild.FailedTxHashes) != 1 {
		t.Fatalf("len(decision.Rebuild.FailedTxHashes) = %d, want 1", len(decision.Rebuild.FailedTxHashes))
	}
	if decision.Rebuild.FailedTxHashes[0] != block.Txs[0].Tx.Hash() {
		t.Fatalf("decision.Rebuild.FailedTxHashes[0] = %s, want %s",
			decision.Rebuild.FailedTxHashes[0].Hex(), block.Txs[0].Tx.Hash().Hex())
	}

	if len(decision.Certificates) != 0 {
		t.Fatalf("len(decision.Certificates) = %d, want 0", len(decision.Certificates))
	}
	if decision.CommitmentRoot != (common.Hash{}) {
		t.Fatalf("decision.CommitmentRoot = %s, want zero hash", decision.CommitmentRoot.Hex())
	}
	if len(decision.CommitmentData) != 0 {
		t.Fatalf("len(decision.CommitmentData) = %d, want 0", len(decision.CommitmentData))
	}
}

func TestDefaultEndorsementManager_ProcessCandidateBlock_RebuildRequired_MultiTx(t *testing.T) {
	manager := makeDefaultTestManager(map[endorsementpolicy.EndorserID]bool{
		"A": true,
		"B": true,
		"C": true,
	})
	cfg := makeTestPolicyConfig()
	block := makeCandidateBlockInputWith2Txs(t, 2)

	decision, err := manager.ProcessCandidateBlock(context.Background(), cfg, block)
	if err != nil {
		t.Fatalf("ProcessCandidateBlock() error = %v, want nil", err)
	}
	if decision == nil {
		t.Fatal("ProcessCandidateBlock() decision = nil, want non-nil")
	}

	if decision.AllSatisfied {
		t.Fatal("decision.AllSatisfied = true, want false")
	}
	if decision.Rebuild == nil {
		t.Fatal("decision.Rebuild = nil, want non-nil")
	}

	// 新语义：快速失败场景下，只剔除 definitely failed 的交易
	if len(decision.Rebuild.FailedTxIndexes) != 1 {
		t.Fatalf("len(decision.Rebuild.FailedTxIndexes) = %d, want 1", len(decision.Rebuild.FailedTxIndexes))
	}
	if len(decision.Rebuild.FailedTxHashes) != 1 {
		t.Fatalf("len(decision.Rebuild.FailedTxHashes) = %d, want 1", len(decision.Rebuild.FailedTxHashes))
	}

	// 在这个测试配置里，tx0 会先触发明确失败，因此只应返回 tx0
	if decision.Rebuild.FailedTxIndexes[0] != 0 {
		t.Fatalf("decision.Rebuild.FailedTxIndexes[0] = %d, want 0", decision.Rebuild.FailedTxIndexes[0])
	}
	if decision.Rebuild.FailedTxHashes[0] != block.Txs[0].Tx.Hash() {
		t.Fatalf("decision.Rebuild.FailedTxHashes[0] = %s, want %s",
			decision.Rebuild.FailedTxHashes[0].Hex(), block.Txs[0].Tx.Hash().Hex())
	}
}

func TestDefaultEndorsementManager_ProcessCandidateBlock_NilBlock(t *testing.T) {
	manager := makeDefaultTestManager(nil)
	cfg := makeTestPolicyConfig()

	decision, err := manager.ProcessCandidateBlock(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("ProcessCandidateBlock() error = nil, want non-nil")
	}
	if decision != nil {
		t.Fatalf("ProcessCandidateBlock() decision = %v, want nil", decision)
	}
	if err != ErrNilCandidateBlockInput {
		t.Fatalf("ProcessCandidateBlock() error = %v, want %v", err, ErrNilCandidateBlockInput)
	}
}

func TestDefaultEndorsementManager_ProcessCandidateBlock_NilConfig(t *testing.T) {
	manager := makeDefaultTestManager(nil)
	block := makeCandidateBlockInputWith1Tx(t, 2)

	decision, err := manager.ProcessCandidateBlock(context.Background(), nil, block)
	if err == nil {
		t.Fatal("ProcessCandidateBlock() error = nil, want non-nil")
	}
	if decision != nil {
		t.Fatalf("ProcessCandidateBlock() decision = %v, want nil", decision)
	}
}

func TestInMemoryResultCollector_GetDefinitelyFailedTxs_ExcludesUnsatisfied(t *testing.T) {
	c := &InMemoryResultCollector{}
	block := makeCollectorBlock2Txs(t, 2)

	if err := c.Init(block); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// tx0 明确失败：两个 reject 后，accepted + remaining < threshold
	if err := c.RecordResponse(0, "A", rejectResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}
	if err := c.RecordResponse(0, "B", rejectResp("B"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	// tx1 只是 unsatisfied：只收到一个 accept，还未满足，也未明确失败
	if err := c.RecordResponse(1, "A", acceptResp("A"), nil); err != nil {
		t.Fatalf("RecordResponse() error = %v", err)
	}

	allFailed := c.GetFailedTxs()
	definitelyFailed := c.GetDefinitelyFailedTxs()

	if len(allFailed.FailedTxIndexes) != 2 {
		t.Fatalf("len(allFailed.FailedTxIndexes) = %d, want 2", len(allFailed.FailedTxIndexes))
	}
	if len(definitelyFailed.FailedTxIndexes) != 1 {
		t.Fatalf("len(definitelyFailed.FailedTxIndexes) = %d, want 1", len(definitelyFailed.FailedTxIndexes))
	}
	if definitelyFailed.FailedTxIndexes[0] != 0 {
		t.Fatalf("definitelyFailed.FailedTxIndexes[0] = %d, want 0", definitelyFailed.FailedTxIndexes[0])
	}
	if definitelyFailed.FailedTxHashes[0] != block.Txs[0].Tx.Hash() {
		t.Fatalf("definitelyFailed.FailedTxHashes[0] = %s, want %s",
			definitelyFailed.FailedTxHashes[0].Hex(), block.Txs[0].Tx.Hash().Hex())
	}
}