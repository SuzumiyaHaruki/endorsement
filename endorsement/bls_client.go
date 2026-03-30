package endorsement

import (
	"context"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/log"
	"github.com/herumi/bls-eth-go-binary/bls"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type BLSSecretKeyStore struct {
	mu   sync.RWMutex
	keys map[endorsementpolicy.EndorserID]*bls.SecretKey
}

func NewBLSSecretKeyStore() *BLSSecretKeyStore {
	return &BLSSecretKeyStore{
		keys: make(map[endorsementpolicy.EndorserID]*bls.SecretKey),
	}
}

func (s *BLSSecretKeyStore) AddRandomKey(
	id endorsementpolicy.EndorserID,
) error {
	if err := ensureBLSInitialized(); err != nil {
		return err
	}

	var sk bls.SecretKey
	sk.SetByCSPRNG()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[id] = &sk
	return nil
}

func (s *BLSSecretKeyStore) GetSecretKey(
	id endorsementpolicy.EndorserID,
) (*bls.SecretKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sk := s.keys[id]
	if sk == nil {
		return nil, fmt.Errorf("missing BLS secret key for endorser %s", id)
	}
	return sk, nil
}

func (s *BLSSecretKeyStore) GetPublicKeyBytes(
	id endorsementpolicy.EndorserID,
) ([]byte, error) {
	sk, err := s.GetSecretKey(id)
	if err != nil {
		return nil, err
	}
	return sk.GetPublicKey().Serialize(), nil
}

type BLSEndorsementClient struct {
	KeyStore *BLSSecretKeyStore
	Rules    EndorsementRejectRules
}

func (c *BLSEndorsementClient) RequestEndorsement(
	ctx context.Context,
	endorser endorsementpolicy.EndorserID,
	req *EndorsementRequest,
) (*EndorsementResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if req == nil {
		return nil, fmt.Errorf("nil endorsement request")
	}
	if c.KeyStore == nil {
		return nil, fmt.Errorf("nil BLS key store")
	}

	reject, reason := c.Rules.ShouldReject(endorser, req)

	var toStr, fromStr string
	if req.Decision.To != nil {
		toStr = req.Decision.To.Hex()
	}
	if req.Decision.From != nil {
		fromStr = req.Decision.From.Hex()
	}

	log.Info("ENDORSEMENT_BLS_DECISION",
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

	sk, err := c.KeyStore.GetSecretKey(endorser)
	if err != nil {
		return nil, err
	}

	sig := sk.SignByte(req.SigningDigest[:])

	return &EndorsementResponse{
		RequestID:  req.Envelope.RequestID,
		EndorserID: endorser,
		Decision:   EndorsementDecisionAccept,
		Signature:  sig.Serialize(),
	}, nil
}