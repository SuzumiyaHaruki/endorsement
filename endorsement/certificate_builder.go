package endorsement

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type DefaultCertificateBuilder struct{}

func (b *DefaultCertificateBuilder) BuildCertificate(
	tx *CandidateTxInput,
	accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
) (*TxEndorsementCertificate, error) {
	if tx == nil || tx.Tx == nil || tx.Policy == nil || tx.Policy.Policy == nil {
		return nil, ErrNilCandidateTxInput
	}
	if uint32(len(accepted)) < tx.Policy.Policy.Threshold {
		return nil, errors.New("accepted endorsements do not satisfy threshold")
	}

	signerIDs := make([]endorsementpolicy.EndorserID, 0, len(accepted))
	for id := range accepted {
		signerIDs = append(signerIDs, id)
	}
	SortEndorserIDs(signerIDs)

	agg, err := b.buildAggregateSignature(
		tx.Policy.Policy.AggregationType,
		signerIDs,
		accepted,
	)
	if err != nil {
		return nil, err
	}

	return &TxEndorsementCertificate{
		TxIndex:            tx.TxIndex,
		TxHash:             tx.Tx.Hash(),
		PolicyID:           tx.Policy.Policy.ID,
		SignerIDs:          signerIDs,
		Threshold:          tx.Policy.Policy.Threshold,
		EncodedProof: agg,
	}, nil
}

func (b *DefaultCertificateBuilder) buildAggregateSignature(
	aggType endorsementpolicy.AggregationType,
	signerIDs []endorsementpolicy.EndorserID,
	accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
) ([]byte, error) {
	switch aggType {
	case endorsementpolicy.AggregationIndividualSignatures:
		return buildIndividualSignaturesAggregate(signerIDs, accepted)

	case endorsementpolicy.AggregationBitmapSignatures:
		return buildBitmapSignaturesAggregate(signerIDs, accepted)

	case endorsementpolicy.AggregationBLS:
		// 第一版暂不实现真正 BLS 聚合
		return nil, errors.New("BLS aggregation is not implemented")

	case endorsementpolicy.AggregationCommitmentOnly:
		return buildCommitmentOnlyAggregate(signerIDs, accepted)

	default:
		return nil, errors.New("unsupported aggregation type")
	}
}

func buildIndividualSignaturesAggregate(
	signerIDs []endorsementpolicy.EndorserID,
	accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
) ([]byte, error) {
	// 第一版仍然允许用“按 signerIDs 顺序拼接签名”的简单格式。
	// 注意必须按排序后的 signerIDs 拼接，保证确定性。
	var buf bytes.Buffer
	for _, id := range signerIDs {
		resp := accepted[id]
		if resp == nil {
			return nil, errors.New("missing accepted response for signer")
		}
		buf.Write(resp.Signature)
	}
	return buf.Bytes(), nil
}

type bitmapAggregatePayload struct {
	SignerIDs  []endorsementpolicy.EndorserID `json:"signer_ids"`
	Bitmap     []byte                         `json:"bitmap"`
	Signatures [][]byte                       `json:"signatures"`
}

func buildBitmapSignaturesAggregate(
	signerIDs []endorsementpolicy.EndorserID,
	accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
) ([]byte, error) {
	// 第一版简单实现：
	// - bitmap 长度按 signerIDs 计算
	// - 所有 signerIDs 都是“已接受”的，所以 bitmap 对应位全为 1
	// - 同时带上 signatures 数组
	//
	// 更完整的版本里，bitmap 通常应该相对于 policy 的全量 endorser 集合，而不是仅 signerIDs。
	// 但第一版先保持最小可用。
	bitmapLen := (len(signerIDs) + 7) / 8
	bitmap := make([]byte, bitmapLen)
	sigs := make([][]byte, 0, len(signerIDs))

	for i, id := range signerIDs {
		resp := accepted[id]
		if resp == nil {
			return nil, errors.New("missing accepted response for signer")
		}
		bitmap[i/8] |= 1 << (i % 8)
		sigs = append(sigs, resp.Signature)
	}

	payload := bitmapAggregatePayload{
		SignerIDs:  signerIDs,
		Bitmap:     bitmap,
		Signatures: sigs,
	}
	return json.Marshal(payload)
}

type commitmentOnlyPayload struct {
	SignerIDs      []endorsementpolicy.EndorserID `json:"signer_ids"`
	SignaturesHash []byte                         `json:"signatures_hash"`
}

func buildCommitmentOnlyAggregate(
	signerIDs []endorsementpolicy.EndorserID,
	accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
) ([]byte, error) {
	h := sha256.New()
	for _, id := range signerIDs {
		resp := accepted[id]
		if resp == nil {
			return nil, errors.New("missing accepted response for signer")
		}
		h.Write([]byte(id))
		h.Write(resp.Signature)
	}

	payload := commitmentOnlyPayload{
		SignerIDs:      signerIDs,
		SignaturesHash: h.Sum(nil),
	}
	return json.Marshal(payload)
}