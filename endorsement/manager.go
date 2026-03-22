package endorsement

import (
	"context"
	"errors"
	"sync"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type DefaultEndorsementManager struct {
	RequestBuilder     RequestBuilder
	Client             EndorsementClient
	Collector          ResultCollector
	CertificateBuilder CertificateBuilder
	RootBuilder        RootBuilder
}

func (m *DefaultEndorsementManager) ProcessCandidateBlock(
	ctx context.Context,
	cfg *endorsementpolicy.PolicyConfig,
	block *CandidateBlockInput,
) (*BlockProcessingDecision, error) {
	if block == nil {
		return nil, ErrNilCandidateBlockInput
	}
	if err := block.Validate(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("nil endorsement policy config")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if m.RequestBuilder == nil || m.Client == nil || m.Collector == nil || m.CertificateBuilder == nil || m.RootBuilder == nil {
		return nil, errors.New("endorsement manager has nil component")
	}
	if err := m.Collector.Init(block); err != nil {
		return nil, err
	}
	// 创建全局超时上下文
	endorseCtx, cancel := context.WithTimeout(ctx, cfg.BlockEndorsementTimeout)
	defer cancel()

	// 定义并发控制变量
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	// 遍历块内每笔交易
	for _, tx := range block.Txs {
		req, err := m.RequestBuilder.BuildRequest(block, tx)
		if err != nil {
			return nil, err
		}
		// 遍历这笔交易的所有背书节点
		for _, member := range tx.Policy.Policy.Endorsers.Members {
			endorserID := member.ID
			wg.Add(1)

			go func(txIndex int, endorser endorsementpolicy.EndorserID, request *EndorsementRequest) {
				defer wg.Done()

				select {
				case <-stopCh:
					return
				default:
				}

				resp, err := m.Client.RequestEndorsement(endorseCtx, endorser, request)
				_ = m.Collector.RecordResponse(txIndex, endorser, resp, err)

				if m.Collector.IsTxFailed(txIndex) {
					stop()
				}
			}(tx.TxIndex, endorserID, req)
		}
	}

	// 等待所有 goroutine 完成或超时
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 所有背书都已完成
	case <-endorseCtx.Done():
		// 全局超时后，collector 中未满足的交易由 GetFailedTxs 统一视作失败
		<-done
	case <-stopCh:
		// 某个交易已经明确失败，触发了快速停止
		cancel()
		<-done
	}

	if !m.Collector.AllSatisfied() {
		return &BlockProcessingDecision{
			AllSatisfied: false,
			Rebuild:      m.Collector.GetFailedTxs(),
		}, nil
	}

	certs := make([]*TxEndorsementCertificate, 0, len(block.Txs))
	for _, tx := range block.Txs {
		accepted, err := m.Collector.GetAcceptedResults(tx.TxIndex)
		if err != nil {
			return nil, err
		}
		cert, err := m.CertificateBuilder.BuildCertificate(tx, accepted)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}

	root, data, err := m.RootBuilder.BuildRoot(certs)
	if err != nil {
		return nil, err
	}

	return &BlockProcessingDecision{
		AllSatisfied:   true,
		Certificates:   certs,
		CommitmentRoot: root,
		CommitmentData: data,
	}, nil
}

