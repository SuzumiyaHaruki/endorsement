package endorsementpolicy

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// 背书策略id
type EndorsementPolicyID string

// 背书节点id
type EndorserID string

// 失败模式,决定了如何处理背书失败的交易
// 目前只设计了背书失败后移除对应交易后重新执行
type EndorsementFailMode uint8

const (
	EndorsementFailDropTxAndRebuild EndorsementFailMode = iota
)

// 背书签名聚合方式
type AggregationType uint8

const (
	AggregationIndividualSignatures AggregationType = iota
	AggregationBitmapSignatures
	AggregationBLS
	AggregationCommitmentOnly
)

// 区块级上下文信息
type BlockPolicyContext struct {
	ChainID             *big.Int
	ParentHash          common.Hash
	BlockNumber         uint64
	MessageIndex        uint64
	DelayedMessagesRead uint64
}

// 背书节点数据类型，未来可以考虑添加权重、是否是必要节点等字段
type EndorserMember struct {
	ID EndorserID
}

type EndorserSet struct {
	Members []EndorserMember
}

func (s EndorserSet) IDs() []EndorserID {
	ids := make([]EndorserID, 0, len(s.Members))
	for _, m := range s.Members {
		ids = append(ids, m.ID)
	}
	return ids
}

func (s EndorserSet) Len() int {
	return len(s.Members)
}

// EndorsementPolicy is the static policy resolved before execution.
//
// It answers:
// - who can endorse
// - how many endorsements are needed
// - what to do if endorsement fails
// - how endorsement proofs are encoded / aggregated
//
// Note: timeout is intentionally NOT part of the policy in the first version.
// Timeout is block-level and configured outside this package.
type EndorsementPolicy struct {
	ID              EndorsementPolicyID
	Endorsers       EndorserSet
	Threshold       uint32
	FailMode        EndorsementFailMode
	AggregationType AggregationType
}

// PolicyHint is an optional user- or system-provided hint.
// It is NOT the final decision. PolicyResolver may accept, ignore, or override it.
type PolicyHint struct {
	RequestedPolicyID *EndorsementPolicyID
	Class             string
}

// 背书策略解析器返回的结果，除了背书策略外还有对背书策略选择的说明
type PolicyResolution struct {
	Policy      *EndorsementPolicy
	MatchReason string
}

// 全局配置
type PolicyConfig struct {
	BlockEndorsementTimeout time.Duration
	MaxRebuildRounds        int
}
