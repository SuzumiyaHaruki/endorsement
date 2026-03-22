package endorsement

import (
	"context"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type MockEndorsementClient struct {
	RejectByEndorser map[endorsementpolicy.EndorserID]bool
}

func (c *MockEndorsementClient) RequestEndorsement(
	ctx context.Context,
	endorser endorsementpolicy.EndorserID,
	req *EndorsementRequest,
) (*EndorsementResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if c.RejectByEndorser != nil && c.RejectByEndorser[endorser] {
		return &EndorsementResponse{
			RequestID:  req.Envelope.RequestID,
			EndorserID: endorser,
			Decision:   EndorsementDecisionReject,
			ReasonCode: "mock reject",
		}, nil
	}

	return &EndorsementResponse{
		RequestID:  req.Envelope.RequestID,
		EndorserID: endorser,
		Decision:   EndorsementDecisionAccept,
		Signature:  []byte("mock-signature-" + string(endorser)),
	}, nil
}