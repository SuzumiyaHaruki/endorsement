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

	// 设置全局的超时时间
	endorseCtx, cancel := context.WithTimeout(ctx, cfg.BlockEndorsementTimeout)
	defer cancel()

	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	// 对于区块中的每一笔交易
	for _, tx := range block.Txs {
		req, err := m.RequestBuilder.BuildRequest(block, tx)
		if err != nil {
			return nil, err
		}
		// 对于交易对应的每一个背书节点
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

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	type completionMode int
	const (
		completedAll completionMode = iota
		completedTimeout
		completedEarlyFailure
	)

	mode := completedAll

	select {
	case <-done:
		mode = completedAll
	case <-endorseCtx.Done():
		// 全局超时：未满足阈值的交易也要视为失败
		mode = completedTimeout
		<-done
	case <-stopCh:
		// 某个交易已经明确失败：快速停止
		mode = completedEarlyFailure
		cancel()
		<-done
	}

	if !m.Collector.AllSatisfied() {
		var rebuild *RebuildInstruction
		switch mode {
		case completedTimeout:
			// 超时：failed + unsatisfied 全部剔除
			rebuild = m.Collector.GetFailedTxs()
		case completedEarlyFailure, completedAll:
			// 明确失败或全部结束但未全满足：只剔除 definitely failed
			rebuild = m.Collector.GetDefinitelyFailedTxs()
		default:
			rebuild = m.Collector.GetDefinitelyFailedTxs()
		}

		return &BlockProcessingDecision{
			AllSatisfied: false,
			Rebuild:      rebuild,
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

