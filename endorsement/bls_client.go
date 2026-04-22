package endorsement

import (
	"context"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
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

// 仅保留作临时实验用途；正式运行建议使用 AddSecretKeyHex / AddSecretKeyBytes 加载固定 key。
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

func (s *BLSSecretKeyStore) AddSecretKeyBytes(
	id endorsementpolicy.EndorserID,
	skBytes []byte,
) error {
	if err := ensureBLSInitialized(); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("empty endorser id")
	}
	if len(skBytes) == 0 {
		return fmt.Errorf("empty BLS secret key bytes for endorser %s", id)
	}

	var sk bls.SecretKey
	if err := sk.Deserialize(skBytes); err != nil {
		return fmt.Errorf("deserialize BLS secret key for endorser %s: %w", id, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[id] = &sk
	return nil
}

func (s *BLSSecretKeyStore) AddSecretKeyHex(
	id endorsementpolicy.EndorserID,
	skHex string,
) error {
	skHex = strings.TrimSpace(skHex)
	skHex = strings.TrimPrefix(skHex, "0x")
	skHex = strings.TrimPrefix(skHex, "0X")
	if skHex == "" {
		return fmt.Errorf("empty BLS secret key hex for endorser %s", id)
	}

	skBytes, err := hex.DecodeString(skHex)
	if err != nil {
		return fmt.Errorf("decode BLS secret key hex for endorser %s: %w", id, err)
	}

	return s.AddSecretKeyBytes(id, skBytes)
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

func (s *BLSSecretKeyStore) GetSecretKeyBytes(
	id endorsementpolicy.EndorserID,
) ([]byte, error) {
	sk, err := s.GetSecretKey(id)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), sk.Serialize()...), nil
}

func (s *BLSSecretKeyStore) GetPublicKeyBytes(
	id endorsementpolicy.EndorserID,
) ([]byte, error) {
	sk, err := s.GetSecretKey(id)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), sk.GetPublicKey().Serialize()...), nil
}

type BLSEndorsementClient struct {
	KeyStore *BLSSecretKeyStore
	Rules    EndorsementRejectRules

	// herumi BLS 的 cgo 路径先串行化，降低并发进入 cgo 的风险。
	signMu sync.Mutex
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
	if err := ValidateEndorsementRequest(req); err != nil {
		return nil, err
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
		"digest", req.SigningDigest,
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

	skBytes, err := c.KeyStore.GetSecretKeyBytes(endorser)
	if err != nil {
		return nil, err
	}

	// 复制 digest，避免直接把 request 里的底层切片交给 cgo。
	digest := append([]byte(nil), req.SigningDigest[:]...)
	if len(digest) == 0 {
		return nil, fmt.Errorf("empty signing digest")
	}

	// 每次签名前都反序列化出本地 SecretKey，避免共享 *bls.SecretKey 并发进入 cgo。
	var localSK bls.SecretKey
	if err := localSK.Deserialize(skBytes); err != nil {
		return nil, fmt.Errorf("deserialize BLS secret key for endorser %s: %w", endorser, err)
	}

	c.signMu.Lock()
	defer c.signMu.Unlock()

	// 显式 pin 住参与 cgo 调用的 Go 对象。
	var p runtime.Pinner
	p.Pin(&localSK)
	p.Pin(&digest[0])
	defer p.Unpin()

	log.Info("ABOUT_TO_CALL_BLS_SIGNHASH",
		"txHash", req.Envelope.TxHash,
		"txIndex", req.Envelope.TxIndex,
		"endorser", endorser,
		"digestLen", len(digest),
	)

	sig := localSK.SignHash(digest)
	if sig == nil {
		return nil, fmt.Errorf("bls SignHash returned nil for endorser %s", endorser)
	}

	sigBytes := append([]byte(nil), sig.Serialize()...)

	return &EndorsementResponse{
		RequestID:  req.Envelope.RequestID,
		EndorserID: endorser,
		Decision:   EndorsementDecisionAccept,
		Signature:  sigBytes,
	}, nil
}
