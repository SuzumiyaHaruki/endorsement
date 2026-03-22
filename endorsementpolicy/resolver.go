package endorsementpolicy

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrNoDefaultPolicy = errors.New("no default endorsement policy configured")
)
// PolicyResolver resolves the endorsement policy for a tx before execution.
//
// The resolver should only depend on:
// - block context
// - tx contents
// - optional user / system hint
//
// It must NOT depend on execution results.
type PolicyResolver interface {
	ResolveTxPolicy(
		ctx context.Context,
		blockCtx *BlockPolicyContext,
		tx *types.Transaction,
		txIndex int,
		hint *PolicyHint,
	) (*PolicyResolution, error)
}

// 静态策略，所有交易都返回相同的策略
type StaticResolver struct {
	DefaultPolicy *EndorsementPolicy
}

func (r *StaticResolver) ResolveTxPolicy(
	ctx context.Context,
	blockCtx *BlockPolicyContext,
	tx *types.Transaction,
	txIndex int,
	hint *PolicyHint,
) (*PolicyResolution, error) {
	_ = ctx
	_ = blockCtx
	_ = tx
	_ = txIndex
	_ = hint

	if r == nil || r.DefaultPolicy == nil {
		return nil, ErrNoDefaultPolicy
	}
	if err := r.DefaultPolicy.Validate(); err != nil {
		return nil, err
	}

	return &PolicyResolution{
		Policy:      r.DefaultPolicy,
		MatchReason: "static default policy",
	}, nil
}