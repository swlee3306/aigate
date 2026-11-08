package webauthn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
)

var errSignature = errors.New("webauthn: assertion signature invalid")

// AssertionInput is one navigator.credentials.get result plus what it must
// be bound to: the stored public key, the expected challenge, rpID and origin.
type AssertionInput struct {
	PublicKey                                               *ecdsa.PublicKey
	AuthenticatorData, ClientDataJSON, Signature, Challenge []byte
	RPID, Origin                                            string
}

// VerifyAssertion verifies an ES256 assertion. The authenticator data must be
// exactly 37 bytes (no attested data, no extensions), bound to RPID, with UP
// and UV set; the client data must be a "webauthn.get" for Challenge and
// Origin; and Signature must be a canonical DER ECDSA signature over
// authenticatorData || SHA-256(clientDataJSON). Signature counters are not
// checked. An empty RPID or Origin is refused (ErrConfig).
func VerifyAssertion(in AssertionInput) error {
	if in.RPID == "" || in.Origin == "" {
		return ErrConfig
	}
	if in.PublicKey == nil || in.PublicKey.Curve != elliptic.P256() {
		return fmt.Errorf("%w: public key must be P-256", errSignature)
	}
	if len(in.AuthenticatorData) != authDataHeaderLen {
		return fmt.Errorf("%w: assertion authenticator data must be %d bytes, got %d", errAuthData, authDataHeaderLen, len(in.AuthenticatorData))
	}
	flags, err := checkAuthDataHeader(in.AuthenticatorData, in.RPID)
	if err != nil {
		return err
	}
	if flags&flagAT != 0 {
		return fmt.Errorf("%w: attested credential data (AT) in an assertion", errFlags)
	}
	if err := CheckClientData(in.ClientDataJSON, "webauthn.get", in.Challenge, in.Origin); err != nil {
		return err
	}
	if !canonicalDER(in.Signature) {
		return fmt.Errorf("%w: not a canonical DER ECDSA signature", errSignature)
	}
	cdHash := sha256.Sum256(in.ClientDataJSON)
	signed := make([]byte, 0, len(in.AuthenticatorData)+len(cdHash))
	signed = append(append(signed, in.AuthenticatorData...), cdHash[:]...)
	digest := sha256.Sum256(signed)
	if !ecdsa.VerifyASN1(in.PublicKey, digest[:], in.Signature) {
		return errSignature
	}
	return nil
}

// canonicalDER reports whether sig is a DER SEQUENCE{r, s INTEGER} that
// re-encodes to exactly the same bytes.
func canonicalDER(sig []byte) bool {
	var rs struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(sig, &rs)
	if err != nil || len(rest) != 0 {
		return false
	}
	again, err := asn1.Marshal(rs)
	return err == nil && bytes.Equal(again, sig)
}
