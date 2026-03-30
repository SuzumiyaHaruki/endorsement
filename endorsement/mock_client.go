package endorsement

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type MockEndorsementClient struct {
	// 最高优先级：按 To 地址 + Endorser 拒绝
	RejectByToAndEndorser map[common.Address]map[endorsementpolicy.EndorserID]bool

	// 次高优先级：按 From 地址 + Endorser 拒绝
	RejectByFromAndEndorser map[common.Address]map[endorsementpolicy.EndorserID]bool

	// 兼容旧逻辑：某个 endorser 固定拒绝
	RejectByEndorser map[endorsementpolicy.EndorserID]bool

	// 某个 txIndex 固定拒绝
	RejectByTxIndex map[int]bool

	// 某个 txHash 固定拒绝
	RejectByTxHash map[common.Hash]bool
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

	reject, reason := c.shouldReject(endorser, req)

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

func (c *MockEndorsementClient) shouldReject(
	endorser endorsementpolicy.EndorserID,
	req *EndorsementRequest,
) (bool, string) {
	if req == nil {
		return false, ""
	}

	// 1) 按 To 地址 + Endorser
	if req.Decision.To != nil && c.RejectByToAndEndorser != nil {
		if perAddr, ok := c.RejectByToAndEndorser[*req.Decision.To]; ok {
			if perAddr != nil && perAddr[endorser] {
				return true, "mock reject by to address"
			}
		}
	}

	// 2) 按 From 地址 + Endorser
	if req.Decision.From != nil && c.RejectByFromAndEndorser != nil {
		if perAddr, ok := c.RejectByFromAndEndorser[*req.Decision.From]; ok {
			if perAddr != nil && perAddr[endorser] {
				return true, "mock reject by from address"
			}
		}
	}

	// 3) 按 tx hash
	if c.RejectByTxHash != nil && c.RejectByTxHash[req.Envelope.TxHash] {
		return true, "mock reject by tx hash"
	}

	// 4) 按 tx index
	if c.RejectByTxIndex != nil && c.RejectByTxIndex[req.Envelope.TxIndex] {
		return true, "mock reject by tx index"
	}

	// 5) 按 endorser
	if c.RejectByEndorser != nil && c.RejectByEndorser[endorser] {
		return true, "mock reject by endorser"
	}

	return false, ""
}