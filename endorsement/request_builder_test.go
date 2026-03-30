package endorsement

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

func makeTestRequestBuilderPolicyResolution() *endorsementpolicy.PolicyResolution {
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
			Threshold:       2,
			FailMode:        endorsementpolicy.EndorsementFailDropTxAndRebuild,
			AggregationType: endorsementpolicy.AggregationIndividualSignatures,
		},
		MatchReason: "test",
	}
}

func mustGenerateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	return key
}

func mustSignTransferTx(
	t *testing.T,
	key *ecdsa.PrivateKey,
	chainID int64,
	nonce uint64,
	to common.Address,
) *types.Transaction {
	t.Helper()

	unsignedTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     nonce,
		To:        &to,
		Value:     common.Big1,
		Gas:       21000,
		GasFeeCap: common.Big1,
		GasTipCap: common.Big1,
		Data:      nil,
	})

	signer := types.LatestSignerForChainID(unsignedTx.ChainId())
	signedTx, err := types.SignTx(unsignedTx, signer, key)
	if err != nil {
		t.Fatalf("SignTx() error = %v", err)
	}
	return signedTx
}

func mustSignContractCreateTx(
	t *testing.T,
	key *ecdsa.PrivateKey,
	chainID int64,
	nonce uint64,
	data []byte,
) *types.Transaction {
	t.Helper()

	unsignedTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     nonce,
		To:        nil, // 合约创建交易
		Value:     common.Big1,
		Gas:       500000,
		GasFeeCap: common.Big1,
		GasTipCap: common.Big1,
		Data:      data,
	})

	signer := types.LatestSignerForChainID(unsignedTx.ChainId())
	signedTx, err := types.SignTx(unsignedTx, signer, key)
	if err != nil {
		t.Fatalf("SignTx() error = %v", err)
	}
	return signedTx
}

func makeUnsignedTransferTx(
	t *testing.T,
	chainID int64,
	nonce uint64,
	to common.Address,
) *types.Transaction {
	t.Helper()

	return types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     nonce,
		To:        &to,
		Value:     common.Big1,
		Gas:       21000,
		GasFeeCap: common.Big1,
		GasTipCap: common.Big1,
		Data:      nil,
	})
}

func makeRequestBuilderCandidateBlockInput(
	t *testing.T,
	tx *types.Transaction,
) *CandidateBlockInput {
	t.Helper()

	block := &CandidateBlockInput{
		BlockHash:  common.HexToHash("0xabc123"),
		ParentHash: common.HexToHash("0xdef123"),
		BlockNum:   100,
		Txs: []*CandidateTxInput{
			{
				TxIndex: 0,
				Tx:      tx,
				Receipt: &types.Receipt{
					Status:  1,
					GasUsed: 21000,
				},
				Policy: makeTestRequestBuilderPolicyResolution(),
			},
		},
	}

	if err := block.Validate(); err != nil {
		t.Fatalf("invalid test block: %v", err)
	}
	return block
}

func TestDefaultRequestBuilder_BuildRequest_FillsDecisionToAndFrom(t *testing.T) {
	builder := &DefaultRequestBuilder{}

	key := mustGenerateKey(t)
	expectedFrom := crypto.PubkeyToAddress(key.PublicKey)
	expectedTo := common.HexToAddress("0x1111111111111111111111111111111111111111")

	tx := mustSignTransferTx(t, key, 1337, 0, expectedTo)
	block := makeRequestBuilderCandidateBlockInput(t, tx)

	req, err := builder.BuildRequest(block, block.Txs[0])
	if err != nil {
		t.Fatalf("BuildRequest() error = %v, want nil", err)
	}
	if req == nil {
		t.Fatal("BuildRequest() request = nil, want non-nil")
	}

	if req.Decision.To == nil {
		t.Fatal("req.Decision.To = nil, want non-nil")
	}
	if req.Decision.From == nil {
		t.Fatal("req.Decision.From = nil, want non-nil")
	}

	if tx.To() == nil {
		t.Fatal("tx.To() = nil, want non-nil")
	}
	if *req.Decision.To != *tx.To() {
		t.Fatalf("req.Decision.To = %s, want %s", req.Decision.To.Hex(), tx.To().Hex())
	}

	if *req.Decision.From != expectedFrom {
		t.Fatalf("req.Decision.From = %s, want %s", req.Decision.From.Hex(), expectedFrom.Hex())
	}
}

func TestDefaultRequestBuilder_BuildRequest_AllowsContractCreationTx(t *testing.T) {
	builder := &DefaultRequestBuilder{}

	key := mustGenerateKey(t)
	expectedFrom := crypto.PubkeyToAddress(key.PublicKey)

	// 随便给一段创建代码/数据即可，重点是 To=nil
	createData := common.FromHex("0x600060005560016000f3")
	tx := mustSignContractCreateTx(t, key, 1337, 1, createData)
	block := makeRequestBuilderCandidateBlockInput(t, tx)

	req, err := builder.BuildRequest(block, block.Txs[0])
	if err != nil {
		t.Fatalf("BuildRequest() error = %v, want nil", err)
	}
	if req == nil {
		t.Fatal("BuildRequest() request = nil, want non-nil")
	}

	// 合约创建交易允许 To == nil
	if req.Decision.To != nil {
		t.Fatalf("req.Decision.To = %v, want nil for contract creation tx", req.Decision.To)
	}

	// 已签名交易仍然应能恢复 from
	if req.Decision.From == nil {
		t.Fatal("req.Decision.From = nil, want non-nil")
	}
	if *req.Decision.From != expectedFrom {
		t.Fatalf("req.Decision.From = %s, want %s", req.Decision.From.Hex(), expectedFrom.Hex())
	}
}

func TestDefaultRequestBuilder_BuildRequest_AllowsMissingFromWhenSenderRecoveryFails(t *testing.T) {
	builder := &DefaultRequestBuilder{}

	expectedTo := common.HexToAddress("0x2222222222222222222222222222222222222222")

	// 未签名交易：To 存在，但 Sender 恢复应失败
	tx := makeUnsignedTransferTx(t, 1337, 2, expectedTo)
	block := makeRequestBuilderCandidateBlockInput(t, tx)

	req, err := builder.BuildRequest(block, block.Txs[0])
	if err != nil {
		t.Fatalf("BuildRequest() error = %v, want nil", err)
	}
	if req == nil {
		t.Fatal("BuildRequest() request = nil, want non-nil")
	}

	if req.Decision.To == nil {
		t.Fatal("req.Decision.To = nil, want non-nil")
	}
	if *req.Decision.To != expectedTo {
		t.Fatalf("req.Decision.To = %s, want %s", req.Decision.To.Hex(), expectedTo.Hex())
	}

	// sender 恢复失败时允许 From == nil
	if req.Decision.From != nil {
		t.Fatalf("req.Decision.From = %v, want nil", req.Decision.From)
	}
}