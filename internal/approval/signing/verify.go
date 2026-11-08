package signing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"time"

	"aigate/internal/webauthn"
)

// Verification errors. Verify returns exactly one of these (never wrapping
// the underlying cause, which may echo attacker-chosen input).
var (
	ErrBadSignature   = errors.New("서명이 올바르지 않습니다")
	ErrUnknownSigner  = errors.New("등록되지 않은 서명자입니다")
	ErrStale          = errors.New("서명 시각이 허용 범위(±5분)를 벗어났습니다")
	ErrInvalidMessage = errors.New("서명 메시지가 올바르지 않습니다")
)

const defaultMaxAge = 5 * time.Minute

// Signature is an approval signature as carried in approval files and API
// requests. Sig is an armored SSHSIG for ssh-ed25519, or the base64url (no
// padding) DER ECDSA signature for webauthn-es256.
type Signature struct {
	Signer, Alg, Sig string
	WebAuthn         *WebAuthnData
}

// WebAuthnData holds the assertion's other signed parts, base64url without
// padding.
type WebAuthnData struct{ AuthenticatorData, ClientDataJSON string }

// VerifyOptions controls the freshness check. WebAuthn RPID and Origin are
// not options: they come from the signer entry (R8).
type VerifyOptions struct {
	Now           time.Time     // zero means time.Now()
	MaxAge        time.Duration // zero means 5 minutes
	SkipFreshness bool          // for stored approvals, which do not expire
}

// Verify checks that sig is a valid approval signature over msg by a signer
// in reg: the message must be valid, the signer known, the algorithm equal to
// the signer's type, msg.Issued within MaxAge of Now (unless SkipFreshness),
// and the signature must verify. SSH signatures cover msg.Text() in the
// NamespaceApproval namespace; WebAuthn assertions use msg.Digest() as the
// challenge and the signer's pinned RPID and Origin.
func Verify(reg Registry, msg Message, sig Signature, o VerifyOptions) error {
	if msg.Validate() != nil {
		return ErrInvalidMessage
	}
	signer, ok := reg.Find(sig.Signer)
	if !ok {
		return ErrUnknownSigner
	}
	if sig.Alg != signer.Type {
		return ErrBadSignature
	}
	if !o.SkipFreshness {
		now, maxAge := o.Now, o.MaxAge
		if now.IsZero() {
			now = time.Now()
		}
		if maxAge == 0 {
			maxAge = defaultMaxAge
		}
		if d := now.Sub(msg.Issued); d > maxAge || d < -maxAge {
			return ErrStale
		}
	}
	switch signer.Type {
	case TypeSSHEd25519:
		if VerifySSHSig(signer.PublicKey, NamespaceApproval, []byte(msg.Text()), sig.Sig) != nil {
			return ErrBadSignature
		}
		return nil
	case TypeWebAuthnES256:
		return verifyWebAuthn(signer, msg, sig)
	default:
		return ErrBadSignature
	}
}

func verifyWebAuthn(signer Signer, msg Message, sig Signature) error {
	if sig.WebAuthn == nil {
		return ErrBadSignature
	}
	der, ok1 := decodeExactRawURL(sig.Sig)
	authData, ok2 := decodeExactRawURL(sig.WebAuthn.AuthenticatorData)
	clientData, ok3 := decodeExactRawURL(sig.WebAuthn.ClientDataJSON)
	rawKey, ok4 := decodeExactRawURL(signer.PublicKey)
	if !(ok1 && ok2 && ok3 && ok4) {
		return ErrBadSignature
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), rawKey)
	if err != nil {
		return ErrBadSignature
	}
	digest := msg.Digest()
	if webauthn.VerifyAssertion(webauthn.AssertionInput{
		PublicKey:         pub,
		AuthenticatorData: authData,
		ClientDataJSON:    clientData,
		Signature:         der,
		Challenge:         digest[:],
		RPID:              signer.RPID,
		Origin:            signer.Origin,
	}) != nil {
		return ErrBadSignature
	}
	return nil
}

// decodeExactRawURL decodes strict, unpadded base64url and requires the input
// to be the canonical encoding (the decoder silently skips CR/LF).
func decodeExactRawURL(s string) ([]byte, bool) {
	b, err := rawURL.DecodeString(s)
	if err != nil || len(b) == 0 || rawURL.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}
