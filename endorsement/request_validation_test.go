package endorsement

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func makeTestEndorsementRequest() *EndorsementRequest {
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	from := common.HexToAddress("0x3333333333333333333333333333333333333333")

	payload := DecisionPayload{
		ParentHash: common.HexToHash("0x01"),
		BlockHash:  common.HexToHash("0x02"),
		BlockNum:   1,
		TxHash:     common.HexToHash("0x03"),
		TxIndex:    0,
		Receipt: &types.Receipt{
			GasUsed: 21000,
		},
		To:   &to,
		From: &from,
	}

	return &EndorsementRequest{
		Envelope: EndorsementEnvelope{
			RequestID: CalcRequestID(payload.BlockHash, payload.TxHash, payload.TxIndex),
			BlockHash: payload.BlockHash,
			BlockNum:  payload.BlockNum,
			TxIndex:   payload.TxIndex,
			TxHash:    payload.TxHash,
			PolicyID:  "default",
		},
		Decision:      payload,
		SigningDigest: CalcTxExecutionDigest(payload),
	}
}

func TestValidateEndorsementRequestOK(t *testing.T) {
	req := makeTestEndorsementRequest()
	if err := ValidateEndorsementRequest(req); err != nil {
		t.Fatalf("ValidateEndorsementRequest() error = %v, want nil", err)
	}
}

func TestValidateEndorsementRequestDigestMismatch(t *testing.T) {
	req := makeTestEndorsementRequest()
	req.SigningDigest[0] ^= 0xff

	err := ValidateEndorsementRequest(req)
	if !errors.Is(err, ErrSigningDigestMismatch) {
		t.Fatalf("ValidateEndorsementRequest() error = %v, want ErrSigningDigestMismatch", err)
	}
}

func TestValidateEndorsementRequestRequestIDMismatch(t *testing.T) {
	req := makeTestEndorsementRequest()
	req.Envelope.RequestID = common.HexToHash("0xdeadbeef")

	err := ValidateEndorsementRequest(req)
	if err == nil {
		t.Fatal("ValidateEndorsementRequest() error = nil, want non-nil")
	}
}
