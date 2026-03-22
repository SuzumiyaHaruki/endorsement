package endorsement

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
)

type DefaultRootBuilder struct{}

func (b *DefaultRootBuilder) BuildRoot(
	certs []*TxEndorsementCertificate,
) (common.Hash, []byte, error) {
	var buf bytes.Buffer
	var tmp [8]byte

	for _, cert := range certs {
		if cert == nil {
			continue
		}

		buf.Write(cert.TxHash[:])

		binary.BigEndian.PutUint64(tmp[:], uint64(cert.TxIndex))
		buf.Write(tmp[:])

		buf.Write([]byte(cert.PolicyID))

		binary.BigEndian.PutUint64(tmp[:], uint64(cert.Threshold))
		buf.Write(tmp[:])

		binary.BigEndian.PutUint64(tmp[:], uint64(len(cert.SignerIDs)))
		buf.Write(tmp[:])
		for _, signer := range cert.SignerIDs {
			binary.BigEndian.PutUint64(tmp[:], uint64(len(signer)))
			buf.Write(tmp[:])
			buf.Write([]byte(signer))
		}

		binary.BigEndian.PutUint64(tmp[:], uint64(len(cert.EncodedProof)))
		buf.Write(tmp[:])
		buf.Write(cert.EncodedProof)
	}

	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return common.BytesToHash(sum[:]), data, nil
}