package endorsementpolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrNoDefaultPolicy = errors.New("no default endorsement policy configured")
	ErrNilTx           = errors.New("nil transaction")
	ErrInvalidRule     = errors.New("invalid endorsement policy rule")
)


// PolicyResolver 根据区块上下文、交易上下文等信息决定交易对应的背书策略
type PolicyResolver interface {
	ResolveTxPolicy(
		ctx context.Context,
		blockCtx *BlockPolicyContext,
		tx *types.Transaction,
		txIndex int,
		hint *PolicyHint,
	) (*PolicyResolution, error)
}

// 静态策略解析器，所有交易都返回相同的策略
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

// ========== 动态规则 resolver ==========


// RuleMatch 定义一条规则的匹配条件。
// 同一字段内按 OR 处理；不同字段之间按 AND 处理。
type RuleMatch struct {
	ToEquals   []common.Address		// 根据 to 地址决定背书策略
	FromEquals []common.Address		// 根据 from 地址决定背书策略

	// 4-byte method selector
	SelectorEquals [][]byte

	// 是否合约创建（tx.To() == nil）
	IsContractCreate *bool
}

// PolicyRule 表示一条动态策略规则。
// Priority 越大，优先级越高。
type PolicyRule struct {
	ID          string
	Priority    int
	Description string
	Enabled     bool

	Match  RuleMatch
	Policy *EndorsementPolicy
}

// RuleBasedResolver 基于多条规则解析交易背书策略。
// 若没有任何规则命中，则回落到 DefaultPolicy。
// 这里未来应该实现为一个数据库
type RuleBasedResolver struct {
	DefaultPolicy *EndorsementPolicy
	Rules         []PolicyRule
}

// NewRuleBasedResolver 会复制并按优先级排序 rules。
func NewRuleBasedResolver(
	defaultPolicy *EndorsementPolicy,
	rules []PolicyRule,
) (*RuleBasedResolver, error) {
	if defaultPolicy == nil {
		return nil, ErrNoDefaultPolicy
	}
	if err := defaultPolicy.Validate(); err != nil {
		return nil, err
	}

	cp := make([]PolicyRule, len(rules))
	copy(cp, rules)

	for i := range cp {
		rule := &cp[i]
		if rule.Policy == nil {
			return nil, fmt.Errorf("%w: rule %q has nil policy", ErrInvalidRule, rule.ID)
		}
		if err := rule.Policy.Validate(); err != nil {
			return nil, fmt.Errorf("%w: rule %q policy invalid: %v", ErrInvalidRule, rule.ID, err)
		}
	}

	sort.SliceStable(cp, func(i, j int) bool {
		return cp[i].Priority > cp[j].Priority
	})

	return &RuleBasedResolver{
		DefaultPolicy: defaultPolicy,
		Rules:         cp,
	}, nil
}

// 目前所谓的动态策略解析仍然不够全面，未来还应对策略解析功能进行加强，不只局限在 to/from/是否合约创建 等条件上
func (r *RuleBasedResolver) ResolveTxPolicy(
	ctx context.Context,
	blockCtx *BlockPolicyContext,
	tx *types.Transaction,
	txIndex int,
	hint *PolicyHint,
) (*PolicyResolution, error) {
	_ = ctx
	_ = blockCtx
	_ = txIndex
	_ = hint

	if r == nil || r.DefaultPolicy == nil {
		return nil, ErrNoDefaultPolicy
	}
	if tx == nil {
		return nil, ErrNilTx
	}

	// 解析 tx 基础上下文
	to := tx.To()
	from, _ := recoverSender(tx) // sender 恢复失败时先忽略，让规则自然不命中 from
	selector := extractSelector(tx)
	isContractCreate := (tx.To() == nil)

	for i := range r.Rules {
		rule := &r.Rules[i]
		if !rule.Enabled {
			continue
		}

		ok, reason := rule.match(tx, to, from, selector, isContractCreate)
		if !ok {
			continue
		}

		return &PolicyResolution{
			Policy:      rule.Policy,
			MatchReason: reason,
		}, nil
	}

	return &PolicyResolution{
		Policy:      r.DefaultPolicy,
		MatchReason: "fallback to default policy",
	}, nil
}

func (r *PolicyRule) match(
	tx *types.Transaction,
	to *common.Address,
	from *common.Address,
	selector []byte,
	isContractCreate bool,
) (bool, string) {
	if r == nil {
		return false, ""
	}

	// ToEquals
	if len(r.Match.ToEquals) > 0 {
		if to == nil || !containsAddress(r.Match.ToEquals, *to) {
			return false, ""
		}
	}

	// FromEquals
	if len(r.Match.FromEquals) > 0 {
		if from == nil || !containsAddress(r.Match.FromEquals, *from) {
			return false, ""
		}
	}

	// SelectorEquals
	if len(r.Match.SelectorEquals) > 0 {
		if len(selector) == 0 || !containsSelector(r.Match.SelectorEquals, selector) {
			return false, ""
		}
	}

	// IsContractCreate
	if r.Match.IsContractCreate != nil {
		if isContractCreate != *r.Match.IsContractCreate {
			return false, ""
		}
	}

	return true, buildMatchReason(r, to, from, selector, isContractCreate)
}

func recoverSender(tx *types.Transaction) (*common.Address, error) {
	if tx == nil {
		return nil, ErrNilTx
	}

	signer := types.LatestSignerForChainID(tx.ChainId())
	addr, err := types.Sender(signer, tx)
	if err != nil {
		return nil, err
	}
	return &addr, nil
}

func extractSelector(tx *types.Transaction) []byte {
	if tx == nil {
		return nil
	}
	data := tx.Data()
	if len(data) < 4 {
		return nil
	}
	return data[:4]
}

func containsAddress(list []common.Address, v common.Address) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func containsSelector(list [][]byte, v []byte) bool {
	for _, x := range list {
		if bytes.Equal(x, v) {
			return true
		}
	}
	return false
}

func buildMatchReason(
	r *PolicyRule,
	to *common.Address,
	from *common.Address,
	selector []byte,
	isContractCreate bool,
) string {
	reason := fmt.Sprintf("matched rule=%s priority=%d", r.ID, r.Priority)

	if len(r.Match.ToEquals) > 0 && to != nil {
		reason += fmt.Sprintf(" to=%s", to.Hex())
	}
	if len(r.Match.FromEquals) > 0 && from != nil {
		reason += fmt.Sprintf(" from=%s", from.Hex())
	}
	if len(r.Match.SelectorEquals) > 0 && len(selector) == 4 {
		reason += fmt.Sprintf(" selector=0x%x", selector)
	}
	if r.Match.IsContractCreate != nil {
		reason += fmt.Sprintf(" isContractCreate=%t", isContractCreate)
	}

	return reason
}

