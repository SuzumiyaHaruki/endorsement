package endorsement

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/offchainlabs/nitro/endorsementpolicy"
	"github.com/ethereum/go-ethereum/log"
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
	if m == nil {
		return nil, errors.New("nil endorsement manager")
	}
	if m.RequestBuilder == nil {
		return nil, errors.New("nil request builder")
	}
	if m.Client == nil {
		return nil, errors.New("nil endorsement client")
	}
	if m.Collector == nil {
		return nil, errors.New("nil result collector")
	}
	if m.CertificateBuilder == nil {
		return nil, errors.New("nil certificate builder")
	}
	if m.RootBuilder == nil {
		return nil, errors.New("nil root builder")
	}

	if err := m.Collector.Init(block); err != nil {
		return nil, err
	}

	reqByTxIndex := make(map[int]*EndorsementRequest, len(block.Txs))
	for _, tx := range block.Txs {
		req, err := m.RequestBuilder.BuildRequest(block, tx)
		if err != nil {
			return nil, err
		}
		reqByTxIndex[tx.TxIndex] = req
	}

	endorseCtx, cancel := context.WithTimeout(ctx, cfg.BlockEndorsementTimeout)
	defer cancel()

	var (
		wg           sync.WaitGroup
		stopOnce     sync.Once
		firstErr     error
		firstErrOnce sync.Once
		failedLoggedMu   sync.Mutex
		failedLoggedOnce = make(map[int]struct{})
	)

	stopCh := make(chan struct{})
	stop := func() {
		stopOnce.Do(func() {
			close(stopCh)
			cancel()
		})
	}
	setFirstErr := func(err error) {
		if err == nil {
			return
		}
		firstErrOnce.Do(func() {
			firstErr = err
		})
	}

	for _, tx := range block.Txs {
		for _, member := range tx.Policy.Policy.Endorsers.Members {
			endorserID := member.ID
			req := reqByTxIndex[tx.TxIndex]
			if req == nil {
				return nil, fmt.Errorf("missing endorsement request for tx %d", tx.TxIndex)
			}

			wg.Add(1)
			go func(txIndex int, endorser endorsementpolicy.EndorserID, req *EndorsementRequest) {
				defer wg.Done()

				select {
				case <-stopCh:
					return
				default:
				}

				resp, err := m.Client.RequestEndorsement(endorseCtx, endorser, req)
				// 根据收到的背书结果输出日志
				if err != nil {
					log.Warn("ENDORSEMENT_MANAGER_REQUEST_FAILED",
						"txIndex", txIndex,
						"endorser", endorser,
						"requestID", req.Envelope.RequestID,
						"err", err,
					)
				} else if resp == nil {
					log.Warn("ENDORSEMENT_MANAGER_RESPONSE_NIL",
						"txIndex", txIndex,
						"endorser", endorser,
						"requestID", req.Envelope.RequestID,
					)
				} else {
					log.Info("ENDORSEMENT_MANAGER_RESPONSE_OK",
						"txIndex", txIndex,
						"endorser", endorser,
						"requestID", req.Envelope.RequestID,
						"decision", resp.Decision,
						"reasonCode", resp.ReasonCode,
						"sigLen", len(resp.Signature),
					)
				}

				// 将接收到的背书结果写入记录
				if recErr := m.Collector.RecordResponse(txIndex, endorser, resp, err); recErr != nil {
					log.Error("ENDORSEMENT_MANAGER_RECORD_RESPONSE_FAILED",
						"txIndex", txIndex,
						"endorser", endorser,
						"err", recErr,
					)
					setFirstErr(fmt.Errorf(
						"record endorsement response failed for txIndex=%d endorser=%s: %w",
						txIndex, endorser, recErr,
					))
					stop()
					return
				}

				// 如果交易已经无法满足背书策略要求
				if m.Collector.IsTxFailed(txIndex) {
					shouldLog := false

					failedLoggedMu.Lock()
					if _, exists := failedLoggedOnce[txIndex]; !exists {
						failedLoggedOnce[txIndex] = struct{}{}
						shouldLog = true
					}
					failedLoggedMu.Unlock()

					if shouldLog {
						log.Warn("ENDORSEMENT_MANAGER_TX_FAILED",
							"txIndex", txIndex,
							"endorser", endorser,
						)
					}
				}
			}(tx.TxIndex, endorserID, req)
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	mode := completedAll
	select {
	case <-done:
		mode = completedAll
	case <-endorseCtx.Done():
		mode = completedTimeout
		<-done
	}

	if firstErr != nil {
		return nil, firstErr
	}

	var rebuildCount int
	if !m.Collector.AllSatisfied() {
		switch mode {
		case completedTimeout:
			rebuildCount = len(m.Collector.GetFailedTxs().FailedTxIndexes)
		default:
			rebuildCount = len(m.Collector.GetDefinitelyFailedTxs().FailedTxIndexes)
		}
	}

	if !m.Collector.AllSatisfied() {
		var rebuild *RebuildInstruction
		switch mode {
		case completedTimeout:
			rebuild = m.Collector.GetFailedTxs()
		case completedAll:
			rebuild = m.Collector.GetDefinitelyFailedTxs()
		default:
			rebuild = m.Collector.GetDefinitelyFailedTxs()
		}

		log.Warn("Candidate Block Endorsement Incomplete",
			"AllSatisfied", false,
			"Mode", mode.String(),
			"RebuildTxCount", rebuildCount,
			"FailedTxIndexes", rebuild.FailedTxIndexes,
		)

		return &BlockProcessingDecision{
			AllSatisfied: false,
			Rebuild:      rebuild,
		}, nil
	}

	log.Info("Candidate Block Endorsement Complete",
		"AllSatisfied", true,
		"Mode", mode.String(),
		"RebuildTxCount", rebuildCount,
	)

	// 为每笔交易生成背书成功证明
	certs := make([]*TxEndorsementCertificate, 0, len(block.Txs))
	for _, tx := range block.Txs {
		accepted, err := m.Collector.GetAcceptedResults(tx.TxIndex)
		if err != nil {
			return nil, err
		}
		req := reqByTxIndex[tx.TxIndex]
		if req == nil {
			return nil, errors.New("missing endorsement request for tx")
		}
		cert, err := m.CertificateBuilder.BuildCertificate(req, tx, accepted)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}

	// 生成区块级证明
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

type completionMode int

const (
	completedAll completionMode = iota
	completedTimeout
)

func (m completionMode) String() string {
	switch m {
	case completedAll:
		return "completed_all"
	case completedTimeout:
		return "completed_timeout"
	default:
		return "unknown"
	}
}