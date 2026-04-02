package endorsement

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type DefaultRootBuilder struct{}

const (
	commitmentDataMagic   = "ECMT"
	commitmentDataVersion = byte(1)
)

var (
	errInvalidCommitmentDataMagic   = errors.New("invalid commitment data magic")
	errUnsupportedCommitmentVersion = errors.New("unsupported commitment data version")
	errUnexpectedTrailingBytes      = errors.New("unexpected trailing bytes in commitment data")
)

func (b *DefaultRootBuilder) BuildRoot(
	certs []*TxEndorsementCertificate,
) (common.Hash, []byte, error) {
	data, err := SerializeCommitmentData(certs)
	if err != nil {
		return common.Hash{}, nil, err
	}

	sum := sha256.Sum256(data)
	return common.BytesToHash(sum[:]), data, nil
}

// SerializeCommitmentData 把证书列表编码成可反解析的 commitment data。
func SerializeCommitmentData(certs []*TxEndorsementCertificate) ([]byte, error) {
	var buf bytes.Buffer

	// Header
	buf.WriteString(commitmentDataMagic)
	buf.WriteByte(commitmentDataVersion)

	if err := writeUint32(&buf, uint32(len(certs))); err != nil {
		return nil, err
	}

	// Body
	for i, cert := range certs {
		if cert == nil {
			return nil, fmt.Errorf("nil certificate at index %d", i)
		}

		certPayload, err := serializeCertificate(cert)
		if err != nil {
			return nil, fmt.Errorf("serialize certificate %d: %w", i, err)
		}

		if err := writeBytesWithUint32Len(&buf, certPayload); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// ParseCommitmentData 把 commitment data 反解析成证书列表。
func ParseCommitmentData(data []byte) ([]*TxEndorsementCertificate, error) {
	r := bytes.NewReader(data)

	// Header
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("read commitment magic: %w", err)
	}
	if string(magic) != commitmentDataMagic {
		return nil, errInvalidCommitmentDataMagic
	}

	version, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("read commitment version: %w", err)
	}
	if version != commitmentDataVersion {
		return nil, fmt.Errorf("%w: got %d", errUnsupportedCommitmentVersion, version)
	}

	certCount, err := readUint32(r)
	if err != nil {
		return nil, fmt.Errorf("read certificate count: %w", err)
	}

	certs := make([]*TxEndorsementCertificate, 0, certCount)
	for i := uint32(0); i < certCount; i++ {
		certPayload, err := readBytesWithUint32Len(r)
		if err != nil {
			return nil, fmt.Errorf("read certificate payload %d: %w", i, err)
		}

		cert, err := parseCertificate(certPayload)
		if err != nil {
			return nil, fmt.Errorf("parse certificate %d: %w", i, err)
		}
		certs = append(certs, cert)
	}

	if r.Len() != 0 {
		return nil, errUnexpectedTrailingBytes
	}

	return certs, nil
}

func serializeCertificate(cert *TxEndorsementCertificate) ([]byte, error) {
	if cert == nil {
		return nil, errors.New("nil certificate")
	}

	var buf bytes.Buffer

	// TxHash (32 bytes)
	buf.Write(cert.TxHash[:])

	// TxIndex (8 bytes)
	if err := writeUint64(&buf, uint64(cert.TxIndex)); err != nil {
		return nil, err
	}

	// PolicyID (len + bytes)
	if err := writeStringWithUint32Len(&buf, string(cert.PolicyID)); err != nil {
		return nil, err
	}

	// Threshold (4 bytes)
	if err := writeUint32(&buf, cert.Threshold); err != nil {
		return nil, err
	}

	// SignerIDs
	if err := writeUint32(&buf, uint32(len(cert.SignerIDs))); err != nil {
		return nil, err
	}
	for _, signer := range cert.SignerIDs {
		if err := writeStringWithUint32Len(&buf, string(signer)); err != nil {
			return nil, err
		}
	}

	// EncodedProof
	if err := writeBytesWithUint32Len(&buf, cert.EncodedProof); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func parseCertificate(data []byte) (*TxEndorsementCertificate, error) {
	r := bytes.NewReader(data)

	var txHash common.Hash
	if _, err := io.ReadFull(r, txHash[:]); err != nil {
		return nil, fmt.Errorf("read tx hash: %w", err)
	}

	txIndexU64, err := readUint64(r)
	if err != nil {
		return nil, fmt.Errorf("read tx index: %w", err)
	}

	policyID, err := readStringWithUint32Len(r)
	if err != nil {
		return nil, fmt.Errorf("read policy id: %w", err)
	}

	threshold, err := readUint32(r)
	if err != nil {
		return nil, fmt.Errorf("read threshold: %w", err)
	}

	signerCount, err := readUint32(r)
	if err != nil {
		return nil, fmt.Errorf("read signer count: %w", err)
	}

	signerIDs := make([]endorsementpolicy.EndorserID, 0, signerCount)
	for i := uint32(0); i < signerCount; i++ {
		signer, err := readStringWithUint32Len(r)
		if err != nil {
			return nil, fmt.Errorf("read signer %d: %w", i, err)
		}
		signerIDs = append(signerIDs, endorsementpolicy.EndorserID(signer))
	}

	encodedProof, err := readBytesWithUint32Len(r)
	if err != nil {
		return nil, fmt.Errorf("read encoded proof: %w", err)
	}

	if r.Len() != 0 {
		return nil, errUnexpectedTrailingBytes
	}

	return &TxEndorsementCertificate{
		TxIndex:     int(txIndexU64),
		TxHash:      txHash,
		PolicyID:    endorsementpolicy.EndorsementPolicyID(policyID),
		SignerIDs:   signerIDs,
		Threshold:   threshold,
		EncodedProof: encodedProof,
	}, nil
}

func writeUint32(w io.Writer, v uint32) error {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func writeUint64(w io.Writer, v uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func readUint32(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

func readUint64(r io.Reader) (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(buf[:]), nil
}

func writeStringWithUint32Len(w io.Writer, s string) error {
	return writeBytesWithUint32Len(w, []byte(s))
}

func readStringWithUint32Len(r io.Reader) (string, error) {
	b, err := readBytesWithUint32Len(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeBytesWithUint32Len(w io.Writer, b []byte) error {
	if err := writeUint32(w, uint32(len(b))); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readBytesWithUint32Len(r io.Reader) ([]byte, error) {
	n, err := readUint32(r)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return []byte{}, nil
	}

	out := make([]byte, n)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return out, nil
}