package endorsement

import (
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type txCollectState struct {
	policy    *endorsementpolicy.EndorsementPolicy
	total     int
	accepted  map[endorsementpolicy.EndorserID]*EndorsementResponse
	rejected  map[endorsementpolicy.EndorserID]*EndorsementResponse
	responded map[endorsementpolicy.EndorserID]struct{}
	failed    bool
	satisfied bool
	txHash    common.Hash
}

type InMemoryResultCollector struct {
	mu     sync.Mutex
	states map[int]*txCollectState
}

func (c *InMemoryResultCollector) Init(block *CandidateBlockInput) error {
	if block == nil {
		return ErrNilCandidateBlockInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.states = make(map[int]*txCollectState, len(block.Txs))
	for _, tx := range block.Txs {
		if tx == nil || tx.Policy == nil || tx.Policy.Policy == nil || tx.Tx == nil {
			return errors.New("invalid candidate tx in collector init")
		}
		p := tx.Policy.Policy
		c.states[tx.TxIndex] = &txCollectState{
			policy:    p,
			total:     len(p.Endorsers.Members),
			accepted:  make(map[endorsementpolicy.EndorserID]*EndorsementResponse),
			rejected:  make(map[endorsementpolicy.EndorserID]*EndorsementResponse),
			responded: make(map[endorsementpolicy.EndorserID]struct{}),
			txHash:    tx.Tx.Hash(),
		}
	}
	return nil
}

func (c *InMemoryResultCollector) RecordResponse(
	txIndex int,
	endorser endorsementpolicy.EndorserID,
	resp *EndorsementResponse,
	err error,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.states[txIndex]
	if !ok {
		return errors.New("unknown tx index")
	}
	if st.failed || st.satisfied {
		return nil
	}
	if _, ok := st.responded[endorser]; ok {
		return nil
	}
	st.responded[endorser] = struct{}{}

	if err != nil {
		c.updateState(st)
		return nil
	}

	if resp != nil && resp.Decision == EndorsementDecisionAccept {
		st.accepted[endorser] = resp
	} else {
		if resp == nil {
			resp = &EndorsementResponse{
				EndorserID: endorser,
				Decision:   EndorsementDecisionReject,
				ReasonCode: "nil response",
			}
		}
		st.rejected[endorser] = resp
	}

	c.updateState(st)
	return nil
}

func (c *InMemoryResultCollector) updateState(st *txCollectState) {
	if uint32(len(st.accepted)) >= st.policy.Threshold {
		st.satisfied = true
		return
	}

	remaining := st.total - len(st.responded)
	maxPossibleAccepted := len(st.accepted) + remaining
	if uint32(maxPossibleAccepted) < st.policy.Threshold {
		st.failed = true
	}
}

func (c *InMemoryResultCollector) IsTxSatisfied(txIndex int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.states[txIndex]
	return ok && st.satisfied
}

func (c *InMemoryResultCollector) IsTxFailed(txIndex int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.states[txIndex]
	return ok && st.failed
}

func (c *InMemoryResultCollector) AllSatisfied() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.states {
		if !st.satisfied {
			return false
		}
	}
	return true
}

func (c *InMemoryResultCollector) GetFailedTxs() *RebuildInstruction {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := &RebuildInstruction{}
	for txIndex, st := range c.states {
		if st.failed || !st.satisfied {
			out.FailedTxIndexes = append(out.FailedTxIndexes, txIndex)
			out.FailedTxHashes = append(out.FailedTxHashes, st.txHash)
		}
	}
	return out
}

func (c *InMemoryResultCollector) GetAcceptedResults(
	txIndex int,
) (map[endorsementpolicy.EndorserID]*EndorsementResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.states[txIndex]
	if !ok {
		return nil, errors.New("unknown tx index")
	}
	if !st.satisfied {
		return nil, errors.New("tx is not satisfied")
	}

	out := make(map[endorsementpolicy.EndorserID]*EndorsementResponse, len(st.accepted))
	for k, v := range st.accepted {
		out[k] = v
	}
	return out, nil
}