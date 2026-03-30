package endorsement

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type DefaultRequestBuilder struct{}

// recoverTxSender 从交易签名中恢复发送者地址 from
// 如果恢复失败，由调用方决定是否继续；这里不做强制约束。
func recoverTxSender(tx *types.Transaction) (*common.Address, error) {
	if tx == nil {
		return nil, ErrNilCandidateTxInput
	}

	signer := types.LatestSignerForChainID(tx.ChainId())

	from, err := types.Sender(signer, tx)
	if err != nil {
		return nil, err
	}
	return &from, nil
}

func copyToAddress(tx *types.Transaction) *common.Address {
	if tx == nil || tx.To() == nil {
		return nil
	}
	to := *tx.To()
	return &to
}

func (b *DefaultRequestBuilder) BuildRequest(
	block *CandidateBlockInput,
	tx *CandidateTxInput,
) (*EndorsementRequest, error) {
	if block == nil {
		return nil, ErrNilCandidateBlockInput
	}
	if tx == nil || tx.Tx == nil || tx.Policy == nil || tx.Policy.Policy == nil {
		return nil, ErrNilCandidateTxInput
	}

	reqID := CalcRequestID(block.BlockHash, tx.Tx.Hash(), tx.TxIndex)

	to := copyToAddress(tx.Tx)

	// 如果无法恢复 from，则将其设为空值
	from, err := recoverTxSender(tx.Tx)
	if err != nil {
		from = nil
	}

	payload := DecisionPayload{
		ParentHash: block.ParentHash,
		BlockHash:  block.BlockHash,
		BlockNum:   block.BlockNum,
		TxHash:     tx.Tx.Hash(),
		TxIndex:    tx.TxIndex,
		Receipt:    tx.Receipt,
		To:         to,
		From:       from,
	}

	return &EndorsementRequest{
		Envelope: EndorsementEnvelope{
			RequestID: reqID,
			BlockHash: block.BlockHash,
			BlockNum:  block.BlockNum,
			TxIndex:   tx.TxIndex,
			TxHash:    tx.Tx.Hash(),
			PolicyID:  tx.Policy.Policy.ID,
		},
		Decision:      payload,
		SigningDigest: CalcTxExecutionDigest(payload),
	}, nil
}