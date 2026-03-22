package endorsement

type DefaultRequestBuilder struct{}

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
	payload := DecisionPayload{
		ParentHash: block.ParentHash,
		BlockHash:  block.BlockHash,
		BlockNum:   block.BlockNum,
		TxHash:     tx.Tx.Hash(),
		TxIndex:    tx.TxIndex,
		Receipt:    tx.Receipt,
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