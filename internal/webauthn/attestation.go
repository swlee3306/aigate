// Package webauthn is a minimal, verify-only WebAuthn implementation:
// ES256 (P-256) credentials, attestation format "none", and assertions.
package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

// Credential is a registered passkey: its credential ID and ES256 public key.
type Credential struct {
	ID        []byte
	PublicKey *ecdsa.PublicKey
}

var (
	errTooLarge = errors.New("webauthn: input too large")
	errFormat   = errors.New("webauthn: attestation object is not fmt \"none\" with an empty attStmt")
	errAuthData = errors.New("webauthn: malformed authenticator data")
	errRPID     = errors.New("webauthn: rpIdHash does not match the relying party ID")
	errFlags    = errors.New("webauthn: authenticator flags not acceptable")
	errCOSEKey  = errors.New("webauthn: credential public key is not an ES256 P-256 COSE key")
)

const (
	maxAttestationObject = 8 << 10
	maxCredentialID      = 1023

	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagAT = 0x40 // attested credential data included
	flagED = 0x80 // extension data included

	authDataHeaderLen = 32 + 1 + 4 // rpIdHash, flags, signCount
)

// ParseAttestationNone parses a registration attestation object of format
// "none" and returns the attested ES256 credential. The authenticator data
// must be bound to rpID and carry the UP, UV and AT flags; extension data is
// refused.
func ParseAttestationNone(attestationObject []byte, rpID string) (Credential, error) {
	if len(attestationObject) > maxAttestationObject {
		return Credential{}, fmt.Errorf("%w: attestation object over %d bytes", errTooLarge, maxAttestationObject)
	}
	obj, err := decodeCBOR(attestationObject)
	if err != nil {
		return Credential{}, err
	}
	if obj.kind != cborMap || len(obj.m) != 3 {
		return Credential{}, errFormat
	}
	var fmtOK, stmtOK bool
	var authData []byte
	for _, p := range obj.m {
		switch {
		case p.key.isText("fmt"):
			fmtOK = p.val.isText("none")
		case p.key.isText("attStmt"):
			stmtOK = p.val.kind == cborMap && len(p.val.m) == 0
		case p.key.isText("authData") && p.val.kind == cborBytes:
			authData = p.val.b
		}
	}
	if !fmtOK || !stmtOK || authData == nil {
		return Credential{}, errFormat
	}
	return parseRegistrationAuthData(authData, rpID)
}

// checkAuthDataHeader verifies rpIdHash and the UP/UV flags and refuses
// extension data. It returns the flags byte.
func checkAuthDataHeader(authData []byte, rpID string) (byte, error) {
	if len(authData) < authDataHeaderLen {
		return 0, fmt.Errorf("%w: %d bytes", errAuthData, len(authData))
	}
	want := sha256.Sum256([]byte(rpID))
	if subtle.ConstantTimeCompare(authData[:32], want[:]) != 1 {
		return 0, errRPID
	}
	flags := authData[32]
	if flags&flagUP == 0 {
		return 0, fmt.Errorf("%w: user presence (UP) not set", errFlags)
	}
	if flags&flagUV == 0 {
		return 0, fmt.Errorf("%w: user verification (UV) not set", errFlags)
	}
	if flags&flagED != 0 {
		return 0, fmt.Errorf("%w: extension data (ED) not accepted", errFlags)
	}
	return flags, nil
}

func parseRegistrationAuthData(authData []byte, rpID string) (Credential, error) {
	flags, err := checkAuthDataHeader(authData, rpID)
	if err != nil {
		return Credential{}, err
	}
	if flags&flagAT == 0 {
		return Credential{}, fmt.Errorf("%w: attested credential data (AT) not set", errFlags)
	}
	rest := authData[authDataHeaderLen:]
	if len(rest) < 16+2 { // aaguid, credentialIdLength
		return Credential{}, fmt.Errorf("%w: attested credential data truncated", errAuthData)
	}
	n := int(binary.BigEndian.Uint16(rest[16:18]))
	rest = rest[18:]
	if n < 1 || n > maxCredentialID || n > len(rest) {
		return Credential{}, fmt.Errorf("%w: bad credential ID length %d", errAuthData, n)
	}
	id := append([]byte(nil), rest[:n]...)
	key, tail, err := decodeCBORPrefix(rest[n:])
	if err != nil {
		return Credential{}, fmt.Errorf("%w: %w", errCOSEKey, err)
	}
	if len(tail) != 0 {
		return Credential{}, fmt.Errorf("%w: %d bytes after the credential public key", errAuthData, len(tail))
	}
	pub, err := parseCOSEKey(key)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: id, PublicKey: pub}, nil
}

// parseCOSEKey accepts exactly {1: 2 (EC2), 3: -7 (ES256), -1: 1 (P-256),
// -2: x, -3: y} with 32-byte coordinates of a point on P-256.
func parseCOSEKey(k cborValue) (*ecdsa.PublicKey, error) {
	if k.kind != cborMap || len(k.m) != 5 {
		return nil, fmt.Errorf("%w: want a map of exactly 5 members", errCOSEKey)
	}
	var kty, alg, crv bool
	var x, y []byte
	for _, p := range k.m {
		switch {
		case p.key.isInt(1):
			kty = p.val.isInt(2)
		case p.key.isInt(3):
			alg = p.val.isInt(-7)
		case p.key.isInt(-1):
			crv = p.val.isInt(1)
		case p.key.isInt(-2) && p.val.kind == cborBytes:
			x = p.val.b
		case p.key.isInt(-3) && p.val.kind == cborBytes:
			y = p.val.b
		}
	}
	if !kty || !alg || !crv {
		return nil, fmt.Errorf("%w: kty/alg/crv must be EC2/ES256/P-256", errCOSEKey)
	}
	if len(x) != 32 || len(y) != 32 {
		return nil, fmt.Errorf("%w: coordinates must be 32 bytes", errCOSEKey)
	}
	point := append(append([]byte{0x04}, x...), y...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCOSEKey, err)
	}
	return pub, nil
}
