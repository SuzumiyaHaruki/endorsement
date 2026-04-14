package endorsement

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"fmt"

	"github.com/herumi/bls-eth-go-binary/bls"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type DefaultCertificateBuilder struct{
	BLSPublicKeys BLSPublicKeyRegistry
}

var (
	blsInitOnce sync.Once
	blsInitErr  error
)

func ensureBLSInitialized() error {
	blsInitOnce.Do(func() {
		blsInitErr = bls.Init(bls.BLS12_381)
		// 为了尽量保证编译兼容性，这里不强绑 SetETHmode 常量；
		// 如果明确锁定了某个 herumi 版本，也可以在这里额外调用：
		// _ = bls.SetETHmode(bls.EthModeDraft07)
	})
	return blsInitErr
}


func (b *DefaultCertificateBuilder) BuildCertificate(
	req *EndorsementRequest,
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
		req,
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
	req *EndorsementRequest,
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
		return b.buildBLSAggregateAndVerify(req, signerIDs, accepted)

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

type blsAggregatePayload struct {
	Scheme              string                         `json:"scheme"`
	SignerIDs           []endorsementpolicy.EndorserID `json:"signer_ids"`
	AggregatedSignature []byte                         `json:"aggregated_signature"`
}

func (b *DefaultCertificateBuilder) buildBLSAggregateAndVerify(
	req *EndorsementRequest,
	signerIDs []endorsementpolicy.EndorserID,
	accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
) ([]byte, error) {
	if req == nil {
		return nil, errors.New("nil endorsement request for BLS certificate")
	}
	if b.BLSPublicKeys == nil {
		return nil, errors.New("nil BLS public key registry")
	}
	if len(signerIDs) == 0 {
		return nil, errors.New("empty signer set for BLS aggregate")
	}
	if err := ensureBLSInitialized(); err != nil {
		return nil, fmt.Errorf("init BLS library: %w", err)
	}

	sigs := make([]bls.Sign, 0, len(signerIDs))
	pubs := make([]bls.PublicKey, 0, len(signerIDs))

	for _, id := range signerIDs {
		resp := accepted[id]
		if resp == nil {
			return nil, fmt.Errorf("missing accepted response for signer %s", id)
		}
		if len(resp.Signature) == 0 {
			return nil, fmt.Errorf("empty BLS signature for signer %s", id)
		}

		var sig bls.Sign
		if err := sig.Deserialize(resp.Signature); err != nil {
			return nil, fmt.Errorf("deserialize BLS signature for signer %s: %w", id, err)
		}
		sigs = append(sigs, sig)

		pubBytes, err := b.BLSPublicKeys.GetPublicKey(id)
		if err != nil {
			return nil, err
		}

		var pub bls.PublicKey
		if err := pub.Deserialize(pubBytes); err != nil {
			return nil, fmt.Errorf("deserialize BLS public key for signer %s: %w", id, err)
		}
		pubs = append(pubs, pub)
	}

	var aggSig bls.Sign
	aggSig.Aggregate(sigs)

	// SignHash(req.SigningDigest[:]) 对应 VerifyAggregateHashes，而不是 FastAggregateVerify。
	hashes := make([][]byte, 0, len(signerIDs))
	for range signerIDs {
		h := make([]byte, len(req.SigningDigest))
		copy(h, req.SigningDigest[:])
		hashes = append(hashes, h)
	}

	if !aggSig.VerifyAggregateHashes(pubs, hashes) {
		return nil, errors.New("BLS VerifyAggregateHashes failed")
	}

	payload := blsAggregatePayload{
		Scheme:              "bls12-381-herumi-verify-aggregate-hashes",
		SignerIDs:           signerIDs,
		AggregatedSignature: aggSig.Serialize(),
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