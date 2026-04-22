package endorsement

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

var (
	ErrNilEndorsementRequest      = errors.New("nil endorsement request")
	ErrSigningDigestMismatch      = errors.New("signing digest mismatch")
	ErrMissingEnvelopeRequestID   = errors.New("missing endorsement request id")
	ErrMissingEnvelopeBlockHash   = errors.New("missing endorsement block hash")
	ErrMissingEnvelopeTxHash      = errors.New("missing endorsement tx hash")
	ErrMissingDecisionBlockHash   = errors.New("missing decision block hash")
	ErrMissingDecisionParentHash  = errors.New("missing decision parent hash")
	ErrDecisionBlockNumZero       = errors.New("decision block number cannot be zero")
)

// ValidateEndorsementRequest recomputes the digest from DecisionPayload and
// verifies it matches the digest shipped in the request.
func ValidateEndorsementRequest(req *EndorsementRequest) error {
	if req == nil {
		return ErrNilEndorsementRequest
	}
	if req.Envelope.RequestID == (common.Hash{}) {
		return ErrMissingEnvelopeRequestID
	}
	if req.Envelope.BlockHash == (common.Hash{}) {
		return ErrMissingEnvelopeBlockHash
	}
	if req.Envelope.TxHash == (common.Hash{}) {
		return ErrMissingEnvelopeTxHash
	}
	if req.Decision.BlockHash == (common.Hash{}) {
		return ErrMissingDecisionBlockHash
	}
	if req.Decision.ParentHash == (common.Hash{}) {
		return ErrMissingDecisionParentHash
	}
	if req.Decision.BlockNum == 0 {
		return ErrDecisionBlockNumZero
	}

	if req.Envelope.BlockHash != req.Decision.BlockHash {
		return fmt.Errorf(
			"envelope/decision block hash mismatch: got %s want %s",
			req.Envelope.BlockHash.Hex(), req.Decision.BlockHash.Hex(),
		)
	}
	if req.Envelope.BlockNum != req.Decision.BlockNum {
		return fmt.Errorf(
			"envelope/decision block number mismatch: got %d want %d",
			req.Envelope.BlockNum, req.Decision.BlockNum,
		)
	}
	if req.Envelope.TxHash != req.Decision.TxHash {
		return fmt.Errorf(
			"envelope/decision tx hash mismatch: got %s want %s",
			req.Envelope.TxHash.Hex(), req.Decision.TxHash.Hex(),
		)
	}
	if req.Envelope.TxIndex != req.Decision.TxIndex {
		return fmt.Errorf(
			"envelope/decision tx index mismatch: got %d want %d",
			req.Envelope.TxIndex, req.Decision.TxIndex,
		)
	}

	expected := CalcTxExecutionDigest(req.Decision)
	if req.SigningDigest != expected {
		return fmt.Errorf(
			"%w: got %x want %x",
			ErrSigningDigestMismatch, req.SigningDigest[:], expected[:],
		)
	}

	expectedRequestID := CalcRequestID(req.Envelope.BlockHash, req.Envelope.TxHash, req.Envelope.TxIndex)
	if req.Envelope.RequestID != expectedRequestID {
		return fmt.Errorf(
			"request id mismatch: got %s want %s",
			req.Envelope.RequestID.Hex(), expectedRequestID.Hex(),
		)
	}

	return nil
}
