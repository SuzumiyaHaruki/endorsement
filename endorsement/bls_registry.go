package endorsement

import (
	"errors"
	"fmt"
	"sync"

	"github.com/offchainlabs/nitro/endorsementpolicy"
)

// 负责 BLS 公钥注册
type BLSPublicKeyRegistry interface {
	RegisterPublicKey(
		id endorsementpolicy.EndorserID,
		pubkey []byte,
	) error

	GetPublicKey(
		id endorsementpolicy.EndorserID,
	) ([]byte, error)
}

type InMemoryBLSPublicKeyRegistry struct {
	mu      sync.RWMutex
	pubkeys map[endorsementpolicy.EndorserID][]byte
}

func NewInMemoryBLSPublicKeyRegistry() *InMemoryBLSPublicKeyRegistry {
	return &InMemoryBLSPublicKeyRegistry{
		pubkeys: make(map[endorsementpolicy.EndorserID][]byte),
	}
}

// 将 pubkey 写入到 pubkeys[id]
func (r *InMemoryBLSPublicKeyRegistry) RegisterPublicKey(
	id endorsementpolicy.EndorserID,
	pubkey []byte,
) error {
	if id == "" {
		return errors.New("empty endorser id")
	}
	if len(pubkey) == 0 {
		return errors.New("empty BLS public key")
	}

	cp := make([]byte, len(pubkey))
	copy(cp, pubkey)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pubkeys[id] = cp
	return nil
}

// 根据 id 获取 pubkey
func (r *InMemoryBLSPublicKeyRegistry) GetPublicKey(
	id endorsementpolicy.EndorserID,
) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pub, ok := r.pubkeys[id]
	if !ok {
		return nil, fmt.Errorf("BLS public key not registered for endorser %s", id)
	}
	cp := make([]byte, len(pub))
	copy(cp, pub)
	return cp, nil
}