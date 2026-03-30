package endorsement

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type EndorsementRejectRules struct {
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

func (r *EndorsementRejectRules) ShouldReject(
	endorser endorsementpolicy.EndorserID,
	req *EndorsementRequest,
) (bool, string) {
	if req == nil || r == nil {
		return false, ""
	}

	// 1) 按 To 地址 + Endorser
	if req.Decision.To != nil && r.RejectByToAndEndorser != nil {
		if perAddr, ok := r.RejectByToAndEndorser[*req.Decision.To]; ok {
			if perAddr != nil && perAddr[endorser] {
				return true, "mock reject by to address"
			}
		}
	}

	// 2) 按 From 地址 + Endorser
	if req.Decision.From != nil && r.RejectByFromAndEndorser != nil {
		if perAddr, ok := r.RejectByFromAndEndorser[*req.Decision.From]; ok {
			if perAddr != nil && perAddr[endorser] {
				return true, "mock reject by from address"
			}
		}
	}

	// 3) 按 tx hash
	if r.RejectByTxHash != nil && r.RejectByTxHash[req.Envelope.TxHash] {
		return true, "mock reject by tx hash"
	}

	// 4) 按 tx index
	if r.RejectByTxIndex != nil && r.RejectByTxIndex[req.Envelope.TxIndex] {
		return true, "mock reject by tx index"
	}

	// 5) 按 endorser
	if r.RejectByEndorser != nil && r.RejectByEndorser[endorser] {
		return true, "mock reject by endorser"
	}

	return false, ""
}