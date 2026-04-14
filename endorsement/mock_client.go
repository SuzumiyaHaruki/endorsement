package endorsement

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/log"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type MockEndorsementClient struct {
	Rules EndorsementRejectRules
}

func (c *MockEndorsementClient) RequestEndorsement(
	ctx context.Context,
	endorser endorsementpolicy.EndorserID,
	req *EndorsementRequest,
) (*EndorsementResponse, error) {
	if req == nil { 
		return nil, errors.New("nil endorsement request") 
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	reject, reason := c.Rules.ShouldReject(endorser, req)

	var toStr, fromStr string
	if req != nil && req.Decision.To != nil {
		toStr = req.Decision.To.Hex()
	}
	if req != nil && req.Decision.From != nil {
		fromStr = req.Decision.From.Hex()
	}

	log.Info("ENDORSEMENT_MOCK_DECISION",
		"txHash", req.Envelope.TxHash,
		"txIndex", req.Envelope.TxIndex,
		"to", toStr,
		"from", fromStr,
		"endorser", endorser,
		"reject", reject,
		"reason", reason,
	)

	if reject {
		return &EndorsementResponse{
			RequestID:  req.Envelope.RequestID,
			EndorserID: endorser,
			Decision:   EndorsementDecisionReject,
			ReasonCode: reason,
		}, nil
	}

	return &EndorsementResponse{
		RequestID:  req.Envelope.RequestID,
		EndorserID: endorser,
		Decision:   EndorsementDecisionAccept,
		Signature:  []byte("mock-signature-" + string(endorser)),
	}, nil
}