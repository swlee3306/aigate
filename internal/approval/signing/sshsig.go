package signing

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHSIG armor and limits (OpenSSH PROTOCOL.sshsig).
const (
	sshsigBegin    = "-----BEGIN SSH SIGNATURE-----\n"
	sshsigEnd      = "-----END SSH SIGNATURE-----"
	sshsigMagic    = "SSHSIG"
	sshsigVersion  = 1
	maxSSHSigBody  = 8 << 10
	errSSHSigShape = "SSH 서명 형식이 올바르지 않습니다"
)

var strictStdBase64 = base64.StdEncoding.Strict()

// VerifySSHSig verifies an `ssh-keygen -Y sign` signature over message.
// authorizedKey must be a single ssh-ed25519 authorized_keys line without
// options; the signature must be ed25519, use namespace exactly, hash sha256 or
// sha512, and embed exactly that public key. Errors never echo the input.
func VerifySSHSig(authorizedKey, namespace string, message []byte, armored string) error {
	if err := validateSSHKey(authorizedKey); err != nil {
		return err
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return errors.New("public_key를 해석할 수 없습니다")
	}
	if namespace == "" {
		return errors.New("서명 네임스페이스가 비어 있습니다")
	}
	blob, err := decodeSSHSigArmor(armored)
	if err != nil {
		return err
	}

	r := wireReader{b: blob}
	if magic, ok := r.raw(len(sshsigMagic)); !ok || string(magic) != sshsigMagic {
		return errors.New(errSSHSigShape)
	}
	version, ok := r.uint32()
	if !ok || version != sshsigVersion {
		return errors.New("지원하지 않는 SSH 서명 버전입니다")
	}
	keyWire, ok1 := r.string()
	ns, ok2 := r.string()
	reserved, ok3 := r.string()
	hashAlg, ok4 := r.string()
	sigWire, ok5 := r.string()
	if !(ok1 && ok2 && ok3 && ok4 && ok5) || len(r.b) != 0 {
		return errors.New(errSSHSigShape)
	}
	sr := wireReader{b: sigWire}
	format, ok1 := sr.string()
	sigBlob, ok2 := sr.string()
	if !ok1 || !ok2 || len(sr.b) != 0 {
		return errors.New(errSSHSigShape)
	}

	if !bytes.Equal(keyWire, pub.Marshal()) {
		return errors.New("서명 안의 공개 키가 등록된 키와 다릅니다")
	}
	if string(ns) != namespace {
		return errors.New("서명 네임스페이스가 다릅니다")
	}
	if len(reserved) != 0 {
		return errors.New(errSSHSigShape)
	}
	if string(format) != ssh.KeyAlgoED25519 {
		return errors.New("서명 형식은 ssh-ed25519여야 합니다")
	}
	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		h := sha512.Sum512(message)
		digest = h[:]
	case "sha256":
		h := sha256.Sum256(message)
		digest = h[:]
	default:
		return errors.New("지원하지 않는 해시 알고리즘입니다")
	}

	// Signed data is built from the blob's (already checked) fields, as in
	// PROTOCOL.sshsig. The embedded public key is not covered by the signature,
	// which is why it is compared above; verification uses the registry key.
	signed := []byte(sshsigMagic)
	signed = appendWireString(signed, ns)
	signed = appendWireString(signed, reserved)
	signed = appendWireString(signed, hashAlg)
	signed = appendWireString(signed, digest)
	if err := pub.Verify(signed, &ssh.Signature{Format: string(format), Blob: sigBlob}); err != nil {
		return errors.New("SSH 서명 검증에 실패했습니다")
	}
	return nil
}

// decodeSSHSigArmor accepts exactly the BEGIN line, non-empty lines of strict
// standard base64 and the END line, optionally followed by one "\n". CR,
// spaces and any text outside the armor are rejected.
func decodeSSHSigArmor(armored string) ([]byte, error) {
	if len(armored) > len(sshsigBegin)+maxSSHSigBody+len(sshsigEnd)+2 {
		return nil, fmt.Errorf("SSH 서명이 너무 깁니다(최대 %d바이트)", maxSSHSigBody)
	}
	// base64 decoders silently skip CR/LF, so reject CR before decoding.
	if strings.ContainsRune(armored, '\r') {
		return nil, errors.New(errSSHSigShape)
	}
	rest, ok := strings.CutPrefix(armored, sshsigBegin)
	if !ok {
		return nil, errors.New(errSSHSigShape)
	}
	rest = strings.TrimSuffix(rest, "\n")
	body, ok := strings.CutSuffix(rest, "\n"+sshsigEnd)
	if !ok || body == "" || len(body) > maxSSHSigBody {
		return nil, errors.New(errSSHSigShape)
	}
	lines := strings.Split(body, "\n")
	for _, l := range lines {
		if l == "" {
			return nil, errors.New(errSSHSigShape)
		}
	}
	// Strict decoding rejects non-alphabet bytes and non-canonical padding bits,
	// so no two distinct bodies decode to the same blob (apart from line breaks).
	blob, err := strictStdBase64.DecodeString(strings.Join(lines, ""))
	if err != nil || len(blob) == 0 {
		return nil, errors.New(errSSHSigShape)
	}
	return blob, nil
}

// wireReader reads SSH wire-format fields; every length is bounded by the
// remaining bytes, so malformed input never panics or over-allocates.
type wireReader struct{ b []byte }

func (r *wireReader) raw(n int) ([]byte, bool) {
	if n < 0 || n > len(r.b) {
		return nil, false
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v, true
}

func (r *wireReader) uint32() (uint32, bool) {
	v, ok := r.raw(4)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint32(v), true
}

func (r *wireReader) string() ([]byte, bool) {
	n, ok := r.uint32()
	if !ok || uint64(n) > uint64(len(r.b)) {
		return nil, false
	}
	return r.raw(int(n))
}

func appendWireString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}
