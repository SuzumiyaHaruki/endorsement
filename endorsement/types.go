package endorsement

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type CandidateTxInput struct {
	TxIndex int
	Tx      *types.Transaction
	Receipt *types.Receipt
	Policy  *endorsementpolicy.PolicyResolution
}

type CandidateBlockInput struct {
	BlockHash  common.Hash
	ParentHash common.Hash
	BlockNum   uint64
	Txs        []*CandidateTxInput
}

// Envelope，请求的基本信息
type EndorsementEnvelope struct {
	RequestID common.Hash
	BlockHash common.Hash
	BlockNum  uint64
	TxIndex   int
	TxHash    common.Hash
	PolicyID  endorsementpolicy.EndorsementPolicyID
}

// DecisionPayload，为背书节点进行背书决策提供详细的交易相关信息
type DecisionPayload struct {
	ParentHash common.Hash
	BlockHash  common.Hash
	BlockNum   uint64
	TxHash     common.Hash
	TxIndex    int
	Receipt    *types.Receipt
}

// TxExecutionDigest，背书节点签名的对象
// 在包含足够信息的前提下足够小，并且能由 DecisionPayload 规范化计算得到
type TxExecutionDigest [32]byte

type EndorsementRequest struct {
	Envelope      EndorsementEnvelope
	Decision      DecisionPayload
	SigningDigest TxExecutionDigest
}

type EndorsementDecision uint8

const (
	EndorsementDecisionAccept EndorsementDecision = iota
	EndorsementDecisionReject
)

// 背书节点回复结构体
type EndorsementResponse struct {
	RequestID  common.Hash
	EndorserID endorsementpolicy.EndorserID
	Decision   EndorsementDecision
	Signature  []byte
	ReasonCode string
}

type TxEndorsementCertificate struct {
	TxIndex            int
	TxHash             common.Hash
	PolicyID           endorsementpolicy.EndorsementPolicyID
	SignerIDs          []endorsementpolicy.EndorserID
	Threshold          uint32
	EncodedProof 	   []byte
}

type RebuildInstruction struct {
	FailedTxIndexes []int			// 背书失败交易的index
	FailedTxHashes  []common.Hash	// 背书失败交易的hash
}

type BlockProcessingDecision struct {
	AllSatisfied   bool
	Rebuild        *RebuildInstruction
	Certificates   []*TxEndorsementCertificate
	CommitmentRoot common.Hash
	CommitmentData []byte
}

// EndorsementManager接口
type EndorsementManager interface {
	ProcessCandidateBlock(
		ctx context.Context,
		cfg *endorsementpolicy.PolicyConfig,
		block *CandidateBlockInput,
	) (*BlockProcessingDecision, error)
}

// 请求构造器接口
type RequestBuilder interface {
	BuildRequest(
		block *CandidateBlockInput,
		tx *CandidateTxInput,
	) (*EndorsementRequest, error)
}

// 背书客户端接口
// 通过 RequestEndorsement 向对应背书节点发送背书请求，等待背书节点响应
type EndorsementClient interface {
	RequestEndorsement(
		ctx context.Context,
		endorser endorsementpolicy.EndorserID,
		req *EndorsementRequest,
	) (*EndorsementResponse, error)
}

// 结果收集器接口
type ResultCollector interface {
	// 初始化整个候选块的收集状态
	Init(block *CandidateBlockInput) error
	// 记录某笔交易来自某个背书节点的响应
	RecordResponse(
		txIndex int,
		endorser endorsementpolicy.EndorserID,
		resp *EndorsementResponse,
		err error,
	) error
	// 判断某笔交易当前是否已经满足背书条件
	IsTxSatisfied(txIndex int) bool
	// 判断某笔交易当前是否已经明确失败
	IsTxFailed(txIndex int) bool
	// 判断整块是否已全部满足背书条件
	AllSatisfied() bool
	// 返回所有失败交易
	GetFailedTxs() *RebuildInstruction
	// 返回满足条件的交易背书材料，供后续生成 certificate
	GetAcceptedResults(txIndex int) (map[endorsementpolicy.EndorserID]*EndorsementResponse, error)
}

// 背书证书生成器
type CertificateBuilder interface {
	BuildCertificate(
		tx *CandidateTxInput,
		accepted map[endorsementpolicy.EndorserID]*EndorsementResponse,
	) (*TxEndorsementCertificate, error)
}

// 区块根构造器
type RootBuilder interface {
	BuildRoot(
		certs []*TxEndorsementCertificate,
	) (common.Hash, []byte, error)
}

var (
	ErrNilCandidateBlockInput = errors.New("nil candidate block input")
	ErrNilCandidateTxInput    = errors.New("nil candidate tx input")
)

func (b *CandidateBlockInput) Validate() error {
	if b == nil {
		return ErrNilCandidateBlockInput
	}
	for _, tx := range b.Txs {
		if tx == nil || tx.Tx == nil || tx.Policy == nil {
			return errors.New("invalid candidate tx input")
		}
		if err := tx.Policy.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func CalcRequestID(blockHash common.Hash, txHash common.Hash, txIndex int) common.Hash {
	h := sha256.New()
	h.Write(blockHash[:])
	h.Write(txHash[:])
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], uint64(txIndex))
	h.Write(idx[:])
	return common.BytesToHash(h.Sum(nil))
}

func CalcTxExecutionDigest(payload DecisionPayload) TxExecutionDigest {
	h := sha256.New()
	h.Write(payload.ParentHash[:])
	h.Write(payload.BlockHash[:])

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], payload.BlockNum)
	h.Write(buf[:])

	h.Write(payload.TxHash[:])

	binary.BigEndian.PutUint64(buf[:], uint64(payload.TxIndex))
	h.Write(buf[:])

	if payload.Receipt != nil {
		binary.BigEndian.PutUint64(buf[:], payload.Receipt.GasUsed)
		h.Write(buf[:])
	}

	var out TxExecutionDigest
	copy(out[:], h.Sum(nil))
	return out
}

func SortEndorserIDs(ids []endorsementpolicy.EndorserID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}

