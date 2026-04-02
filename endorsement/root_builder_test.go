package endorsement

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

func TestDefaultRootBuilder_RoundTrip(t *testing.T) {
	builder := &DefaultRootBuilder{}

	certs := []*TxEndorsementCertificate{
		{
			TxIndex:   0,
			TxHash:    common.HexToHash("0x01"),
			PolicyID:  endorsementpolicy.EndorsementPolicyID("default"),
			SignerIDs: []endorsementpolicy.EndorserID{"A", "B", "C"},
			Threshold: 2,
			EncodedProof: []byte(`{"scheme":"bls12-381","aggregated_signature":"abc"}`),
		},
		{
			TxIndex:   1,
			TxHash:    common.HexToHash("0x02"),
			PolicyID:  endorsementpolicy.EndorsementPolicyID("strict"),
			SignerIDs: []endorsementpolicy.EndorserID{"A", "C"},
			Threshold: 2,
			EncodedProof: []byte(`{"scheme":"bitmap","bitmap":"03","signatures":["x","y"]}`),
		},
	}

	root1, data1, err := builder.BuildRoot(certs)
	if err != nil {
		t.Fatalf("BuildRoot failed: %v", err)
	}

	parsed, err := ParseCommitmentData(data1)
	if err != nil {
		t.Fatalf("ParseCommitmentData failed: %v", err)
	}

	assertCertificatesEqual(t, certs, parsed)

	root2, data2, err := builder.BuildRoot(parsed)
	if err != nil {
		t.Fatalf("BuildRoot(parsed) failed: %v", err)
	}

	if root1 != root2 {
		t.Fatalf("root mismatch: %v != %v", root1, root2)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatalf("data mismatch after round-trip")
	}
}

func TestDefaultRootBuilder_EmptyCertificates(t *testing.T) {
	builder := &DefaultRootBuilder{}

	root, data, err := builder.BuildRoot(nil)
	if err != nil {
		t.Fatalf("BuildRoot(nil) failed: %v", err)
	}

	parsed, err := ParseCommitmentData(data)
	if err != nil {
		t.Fatalf("ParseCommitmentData failed: %v", err)
	}
	if len(parsed) != 0 {
		t.Fatalf("expected 0 certs, got %d", len(parsed))
	}

	sum := sha256.Sum256(data)
	expectedRoot := common.BytesToHash(sum[:])
	if root != expectedRoot {
		t.Fatalf("unexpected root: got %v want %v", root, expectedRoot)
	}
}

func TestParseCommitmentData_InvalidMagic(t *testing.T) {
	data := []byte("XXXX\x01\x00\x00\x00\x00")

	_, err := ParseCommitmentData(data)
	if err == nil {
		t.Fatal("expected error for invalid magic, got nil")
	}
	if err != errInvalidCommitmentDataMagic {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseCommitmentData_UnsupportedVersion(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(commitmentDataMagic)
	buf.WriteByte(byte(99))
	mustWriteUint32ForTest(t, &buf, 0)

	_, err := ParseCommitmentData(buf.Bytes())
	if err == nil {
		t.Fatal("expected error for unsupported version, got nil")
	}
}

func TestParseCommitmentData_UnexpectedTrailingBytes(t *testing.T) {
	builder := &DefaultRootBuilder{}

	certs := []*TxEndorsementCertificate{
		{
			TxIndex:      7,
			TxHash:       common.HexToHash("0x77"),
			PolicyID:     endorsementpolicy.EndorsementPolicyID("default"),
			SignerIDs:    []endorsementpolicy.EndorserID{"A"},
			Threshold:    1,
			EncodedProof: []byte("proof"),
		},
	}

	_, data, err := builder.BuildRoot(certs)
	if err != nil {
		t.Fatalf("BuildRoot failed: %v", err)
	}

	data = append(data, 0x99)

	_, err = ParseCommitmentData(data)
	if err == nil {
		t.Fatal("expected error for trailing bytes, got nil")
	}
	if err != errUnexpectedTrailingBytes {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseCommitmentData_TruncatedCertPayload(t *testing.T) {
	builder := &DefaultRootBuilder{}

	certs := []*TxEndorsementCertificate{
		{
			TxIndex:      3,
			TxHash:       common.HexToHash("0x03"),
			PolicyID:     endorsementpolicy.EndorsementPolicyID("strict"),
			SignerIDs:    []endorsementpolicy.EndorserID{"A", "B"},
			Threshold:    2,
			EncodedProof: []byte("hello"),
		},
	}

	_, data, err := builder.BuildRoot(certs)
	if err != nil {
		t.Fatalf("BuildRoot failed: %v", err)
	}

	// 去掉最后几个字节，制造截断
	truncated := data[:len(data)-3]

	_, err = ParseCommitmentData(truncated)
	if err == nil {
		t.Fatal("expected error for truncated payload, got nil")
	}
}

func TestSerializeCommitmentData_NilCertificate(t *testing.T) {
	_, err := SerializeCommitmentData([]*TxEndorsementCertificate{nil})
	if err == nil {
		t.Fatal("expected error for nil certificate, got nil")
	}
}

func TestParseCommitmentData_MultipleBoundaryCases(t *testing.T) {
	builder := &DefaultRootBuilder{}

	certs := []*TxEndorsementCertificate{
		{
			TxIndex:      0,
			TxHash:       common.Hash{},
			PolicyID:     endorsementpolicy.EndorsementPolicyID(""),
			SignerIDs:    nil,
			Threshold:    0,
			EncodedProof: nil,
		},
		{
			TxIndex:      42,
			TxHash:       common.HexToHash("0x1234"),
			PolicyID:     endorsementpolicy.EndorsementPolicyID("p"),
			SignerIDs:    []endorsementpolicy.EndorserID{},
			Threshold:    1,
			EncodedProof: []byte{},
		},
		{
			TxIndex:   99,
			TxHash:    common.HexToHash("0xabcd"),
			PolicyID:  endorsementpolicy.EndorsementPolicyID("very-long-policy-id-for-test"),
			SignerIDs: []endorsementpolicy.EndorserID{"endorser-1", "endorser-2", "endorser-3"},
			Threshold: 3,
			EncodedProof: bytes.Repeat([]byte{0x42}, 128),
		},
	}

	root1, data, err := builder.BuildRoot(certs)
	if err != nil {
		t.Fatalf("BuildRoot failed: %v", err)
	}

	parsed, err := ParseCommitmentData(data)
	if err != nil {
		t.Fatalf("ParseCommitmentData failed: %v", err)
	}

	assertCertificatesEqual(t, certs, parsed)

	root2, data2, err := builder.BuildRoot(parsed)
	if err != nil {
		t.Fatalf("BuildRoot(parsed) failed: %v", err)
	}

	if root1 != root2 {
		t.Fatalf("root mismatch: %v != %v", root1, root2)
	}
	if !bytes.Equal(data, data2) {
		t.Fatal("data mismatch after round-trip")
	}
}

func TestParseCommitmentData_BadCertLengthPrefix(t *testing.T) {
	var buf bytes.Buffer

	buf.WriteString(commitmentDataMagic)
	buf.WriteByte(commitmentDataVersion)
	mustWriteUint32ForTest(t, &buf, 1)

	// 声明 cert payload 长度为 100，但只写 2 个字节
	mustWriteUint32ForTest(t, &buf, 100)
	buf.Write([]byte{0x01, 0x02})

	_, err := ParseCommitmentData(buf.Bytes())
	if err == nil {
		t.Fatal("expected error for bad cert length prefix, got nil")
	}
}

func TestParseCommitmentData_BadSignerEntry(t *testing.T) {
	certBytes := makeMalformedCertPayloadForBadSignerEntry(t)

	var buf bytes.Buffer
	buf.WriteString(commitmentDataMagic)
	buf.WriteByte(commitmentDataVersion)
	mustWriteUint32ForTest(t, &buf, 1)
	mustWriteBytesWithLenForTest(t, &buf, certBytes)

	_, err := ParseCommitmentData(buf.Bytes())
	if err == nil {
		t.Fatal("expected error for malformed signer entry, got nil")
	}
}

func makeMalformedCertPayloadForBadSignerEntry(t *testing.T) []byte {
	var buf bytes.Buffer

	var txHash common.Hash
	copy(txHash[:], common.HexToHash("0x55").Bytes())
	buf.Write(txHash[:])

	mustWriteUint64ForTest(t, &buf, 1)
	mustWriteStringWithLenForTest(t, &buf, "default")
	mustWriteUint32ForTest(t, &buf, 1) // threshold
	mustWriteUint32ForTest(t, &buf, 1) // signerCount

	// 声明 signerID 长度 10，但只写 2 个字节
	mustWriteUint32ForTest(t, &buf, 10)
	buf.Write([]byte("AB"))

	// 后面本来还应有 EncodedProofLen + EncodedProof，这里故意省略
	return buf.Bytes()
}

func assertCertificatesEqual(t *testing.T, want, got []*TxEndorsementCertificate) {
	t.Helper()

	if len(want) != len(got) {
		t.Fatalf("certificate count mismatch: want %d got %d", len(want), len(got))
	}

	for i := range want {
		w := want[i]
		g := got[i]

		if w == nil || g == nil {
			t.Fatalf("nil certificate at index %d: want=%v got=%v", i, w, g)
		}

		if w.TxIndex != g.TxIndex {
			t.Fatalf("TxIndex mismatch at %d: want %d got %d", i, w.TxIndex, g.TxIndex)
		}
		if w.TxHash != g.TxHash {
			t.Fatalf("TxHash mismatch at %d: want %v got %v", i, w.TxHash, g.TxHash)
		}
		if w.PolicyID != g.PolicyID {
			t.Fatalf("PolicyID mismatch at %d: want %q got %q", i, w.PolicyID, g.PolicyID)
		}
		if w.Threshold != g.Threshold {
			t.Fatalf("Threshold mismatch at %d: want %d got %d", i, w.Threshold, g.Threshold)
		}
		if len(w.SignerIDs) != len(g.SignerIDs) {
			t.Fatalf("SignerIDs length mismatch at %d: want %d got %d", i, len(w.SignerIDs), len(g.SignerIDs))
		}
		for j := range w.SignerIDs {
			if w.SignerIDs[j] != g.SignerIDs[j] {
				t.Fatalf("SignerIDs mismatch at cert %d signer %d: want %q got %q", i, j, w.SignerIDs[j], g.SignerIDs[j])
			}
		}
		if !bytes.Equal(w.EncodedProof, g.EncodedProof) {
			t.Fatalf("EncodedProof mismatch at %d", i)
		}
	}
}

func mustWriteUint32ForTest(t *testing.T, buf *bytes.Buffer, v uint32) {
	t.Helper()
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	if _, err := buf.Write(tmp[:]); err != nil {
		t.Fatalf("write uint32 failed: %v", err)
	}
}

func mustWriteUint64ForTest(t *testing.T, buf *bytes.Buffer, v uint64) {
	t.Helper()
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	if _, err := buf.Write(tmp[:]); err != nil {
		t.Fatalf("write uint64 failed: %v", err)
	}
}

func mustWriteBytesWithLenForTest(t *testing.T, buf *bytes.Buffer, b []byte) {
	t.Helper()
	mustWriteUint32ForTest(t, buf, uint32(len(b)))
	if _, err := buf.Write(b); err != nil {
		t.Fatalf("write bytes failed: %v", err)
	}
}

func mustWriteStringWithLenForTest(t *testing.T, buf *bytes.Buffer, s string) {
	t.Helper()
	mustWriteBytesWithLenForTest(t, buf, []byte(s))
}