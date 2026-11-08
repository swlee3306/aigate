package webauthn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

const (
	testRPID   = "console.example.test"
	testOrigin = "https://console.example.test"
)

// ---- test-only CBOR encoder (raw byte building, so malformed input is easy) ----

func cHead(major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return []byte{m | byte(n)}
	case n <= 0xff:
		return []byte{m | 24, byte(n)}
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16([]byte{m | 25}, uint16(n))
	case n <= 0xffffffff:
		return binary.BigEndian.AppendUint32([]byte{m | 26}, uint32(n))
	default:
		return binary.BigEndian.AppendUint64([]byte{m | 27}, n)
	}
}

func cInt(v int64) []byte {
	if v < 0 {
		return cHead(1, uint64(-1-v))
	}
	return cHead(0, uint64(v))
}

func cBytes(b []byte) []byte { return append(cHead(2, uint64(len(b))), b...) }
func cText(s string) []byte  { return append(cHead(3, uint64(len(s))), s...) }

// cMap builds a map from already-encoded key/value pairs: k1, v1, k2, v2, ...
func cMap(kv ...[]byte) []byte {
	out := cHead(5, uint64(len(kv)/2))
	for _, b := range kv {
		out = append(out, b...)
	}
	return out
}

func cArray(items ...[]byte) []byte {
	out := cHead(4, uint64(len(items)))
	for _, b := range items {
		out = append(out, b...)
	}
	return out
}

// ---- synthetic authenticator ----

type authn struct {
	key    *ecdsa.PrivateKey
	credID []byte
}

func newAuthn(t *testing.T) *authn {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &authn{key: k, credID: id}
}

func (a *authn) xy() (x, y []byte) {
	b, err := a.key.PublicKey.Bytes() // 0x04 || x || y
	if err != nil {
		panic(err)
	}
	return b[1:33], b[33:65]
}

func (a *authn) coseKey() []byte {
	x, y := a.xy()
	return cMap(cInt(1), cInt(2), cInt(3), cInt(-7), cInt(-1), cInt(1), cInt(-2), cBytes(x), cInt(-3), cBytes(y))
}

func rpHash(rpID string) []byte { h := sha256.Sum256([]byte(rpID)); return h[:] }

const (
	fUP = 0x01
	fUV = 0x04
	fAT = 0x40
	fED = 0x80
)

func authDataWith(rpID string, flags byte, attested []byte) []byte {
	b := append([]byte{}, rpHash(rpID)...)
	b = append(b, flags, 0, 0, 0, 7) // signCount 7 (ignored)
	return append(b, attested...)
}

func (a *authn) attestedData(cose []byte) []byte {
	b := make([]byte, 16) // aaguid
	b = binary.BigEndian.AppendUint16(b, uint16(len(a.credID)))
	b = append(b, a.credID...)
	return append(b, cose...)
}

func attObj(format string, attStmt, authData []byte) []byte {
	return cMap(cText("fmt"), cText(format), cText("attStmt"), attStmt, cText("authData"), cBytes(authData))
}

func (a *authn) registration(rpID string) []byte {
	return attObj("none", cMap(), authDataWith(rpID, fUP|fUV|fAT, a.attestedData(a.coseKey())))
}

func clientData(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cd(t *testing.T, typ string, challenge []byte, origin string) []byte {
	return clientData(t, map[string]any{
		"type": typ, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin,
	})
}

func (a *authn) sign(t *testing.T, authData, clientDataJSON []byte) []byte {
	t.Helper()
	ch := sha256.Sum256(clientDataJSON)
	d := sha256.Sum256(append(append([]byte{}, authData...), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func challenge() []byte {
	d := sha256.Sum256([]byte("aigate-approval-v1\nexample\n"))
	return d[:]
}

// assertion builds an input signed over authenticator data for rpID/flags and clientDataJSON.
func (a *authn) assertion(t *testing.T, rpID string, flags byte, clientDataJSON []byte) AssertionInput {
	ad := authDataWith(rpID, flags, nil)
	return AssertionInput{
		PublicKey: &a.key.PublicKey, AuthenticatorData: ad, ClientDataJSON: clientDataJSON,
		Signature: a.sign(t, ad, clientDataJSON), Challenge: challenge(), RPID: testRPID, Origin: testOrigin,
	}
}

func (a *authn) okAssertion(t *testing.T) AssertionInput {
	return a.assertion(t, testRPID, fUP|fUV, cd(t, "webauthn.get", challenge(), testOrigin))
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

// ---- tests ----

func TestRegisterNoneOK(t *testing.T) {
	a := newAuthn(t)
	c, err := ParseAttestationNone(a.registration(testRPID), testRPID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.ID, a.credID) {
		t.Fatalf("ID = %x, want %x", c.ID, a.credID)
	}
	if !c.PublicKey.Equal(&a.key.PublicKey) {
		t.Fatal("public key mismatch")
	}
	if err := CheckClientData(cd(t, "webauthn.create", challenge(), testOrigin), "webauthn.create", challenge(), testOrigin); err != nil {
		t.Fatal(err)
	}
	// The parsed key verifies assertions from the same authenticator.
	in := a.okAssertion(t)
	in.PublicKey = c.PublicKey
	if err := VerifyAssertion(in); err != nil {
		t.Fatal(err)
	}
}

func TestAssertOK(t *testing.T) {
	a := newAuthn(t)
	if err := VerifyAssertion(a.okAssertion(t)); err != nil {
		t.Fatal(err)
	}
	// Unknown clientData keys are fine; crossOrigin false is fine.
	cdj := clientData(t, map[string]any{
		"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(challenge()),
		"origin": testOrigin, "crossOrigin": false, "other_keys_can_be_added_here": "x",
	})
	if err := VerifyAssertion(a.assertion(t, testRPID, fUP|fUV, cdj)); err != nil {
		t.Fatal(err)
	}
}

func TestWrongOrigin(t *testing.T) {
	a := newAuthn(t)
	for _, o := range []string{"https://evil.example.test", "https://console.example.test:8443", "http://console.example.test", testOrigin + "/"} {
		in := a.assertion(t, testRPID, fUP|fUV, cd(t, "webauthn.get", challenge(), o))
		wantErr(t, VerifyAssertion(in), errOrigin)
		wantErr(t, CheckClientData(cd(t, "webauthn.create", challenge(), o), "webauthn.create", challenge(), testOrigin), errOrigin)
	}
}

func TestWrongRPID(t *testing.T) {
	a := newAuthn(t)
	_, err := ParseAttestationNone(a.registration("evil.example.test"), testRPID)
	wantErr(t, err, errRPID)

	in := a.assertion(t, "evil.example.test", fUP|fUV, cd(t, "webauthn.get", challenge(), testOrigin))
	wantErr(t, VerifyAssertion(in), errRPID)
	// Correct authenticator data but the verifier is configured for another rpID.
	in = a.okAssertion(t)
	in.RPID = "example.test"
	wantErr(t, VerifyAssertion(in), errRPID)
}

func TestMissingUserVerification(t *testing.T) {
	a := newAuthn(t)
	_, err := ParseAttestationNone(attObj("none", cMap(), authDataWith(testRPID, fUP|fAT, a.attestedData(a.coseKey()))), testRPID)
	wantErr(t, err, errFlags)
	wantErr(t, VerifyAssertion(a.assertion(t, testRPID, fUP, cd(t, "webauthn.get", challenge(), testOrigin))), errFlags)
}

func TestMissingUserPresence(t *testing.T) {
	a := newAuthn(t)
	_, err := ParseAttestationNone(attObj("none", cMap(), authDataWith(testRPID, fUV|fAT, a.attestedData(a.coseKey()))), testRPID)
	wantErr(t, err, errFlags)
	wantErr(t, VerifyAssertion(a.assertion(t, testRPID, fUV, cd(t, "webauthn.get", challenge(), testOrigin))), errFlags)
}

func TestMissingAttestedDataFlag(t *testing.T) {
	a := newAuthn(t)
	_, err := ParseAttestationNone(attObj("none", cMap(), authDataWith(testRPID, fUP|fUV, a.attestedData(a.coseKey()))), testRPID)
	wantErr(t, err, errFlags)
}

func TestWrongChallenge(t *testing.T) {
	a := newAuthn(t)
	other := sha256.Sum256([]byte("another approval"))
	in := a.assertion(t, testRPID, fUP|fUV, cd(t, "webauthn.get", other[:], testOrigin))
	wantErr(t, VerifyAssertion(in), errChallenge)

	enc := base64.RawURLEncoding.EncodeToString(challenge())
	for _, s := range []string{
		base64.URLEncoding.EncodeToString(challenge()), // padded
		base64.RawStdEncoding.EncodeToString([]byte{0xfb, 0xff}),
		enc[:len(enc)-1],
		enc + "A",
		"",
	} {
		cdj := clientData(t, map[string]any{"type": "webauthn.get", "challenge": s, "origin": testOrigin})
		wantErr(t, CheckClientData(cdj, "webauthn.get", challenge(), testOrigin), errChallenge)
	}
	// A challenge whose last character carries non-zero padding bits is not strict base64url.
	short := []byte{0xff}
	nonStrict := clientData(t, map[string]any{"type": "webauthn.get", "challenge": "_x", "origin": testOrigin})
	wantErr(t, CheckClientData(nonStrict, "webauthn.get", short, testOrigin), errChallenge)
}

func TestBadSignature(t *testing.T) {
	a := newAuthn(t)
	in := a.okAssertion(t)
	in.Signature = append([]byte{}, in.Signature...)
	in.Signature[len(in.Signature)-1] ^= 0x01
	wantErr(t, VerifyAssertion(in), errSignature)

	// A valid signature over different authenticator data (signCount differs).
	in = a.okAssertion(t)
	other := authDataWith(testRPID, fUP|fUV, nil)
	other[36] = 8
	in.Signature = a.sign(t, other, in.ClientDataJSON)
	wantErr(t, VerifyAssertion(in), errSignature)

	// A signature by another key.
	in = a.okAssertion(t)
	in.PublicKey = &newAuthn(t).key.PublicKey
	wantErr(t, VerifyAssertion(in), errSignature)
}

func TestTypeCreateRefusedForAssertion(t *testing.T) {
	a := newAuthn(t)
	in := a.assertion(t, testRPID, fUP|fUV, cd(t, "webauthn.create", challenge(), testOrigin))
	wantErr(t, VerifyAssertion(in), errType)
	wantErr(t, CheckClientData(cd(t, "webauthn.get", challenge(), testOrigin), "webauthn.create", challenge(), testOrigin), errType)
}

func TestCrossOriginTrueRefused(t *testing.T) {
	a := newAuthn(t)
	for _, v := range []any{true, "false", nil} {
		cdj := clientData(t, map[string]any{
			"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(challenge()),
			"origin": testOrigin, "crossOrigin": v,
		})
		wantErr(t, VerifyAssertion(a.assertion(t, testRPID, fUP|fUV, cdj)), errCrossOrigin)
	}
}

func TestTopOriginRefused(t *testing.T) {
	a := newAuthn(t)
	for _, v := range []any{testOrigin, nil, ""} {
		cdj := clientData(t, map[string]any{
			"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(challenge()),
			"origin": testOrigin, "topOrigin": v,
		})
		wantErr(t, VerifyAssertion(a.assertion(t, testRPID, fUP|fUV, cdj)), errTopOrigin)
	}
}

func TestClientDataMalformed(t *testing.T) {
	ok := cd(t, "webauthn.get", challenge(), testOrigin)
	for _, b := range [][]byte{
		nil,
		[]byte("[]"),
		[]byte(`{"type": 1}`),
		append(append([]byte{}, ok...), []byte(" {}")...), // trailing data
		ok[:len(ok)-1],
	} {
		if err := CheckClientData(b, "webauthn.get", challenge(), testOrigin); err == nil {
			t.Fatalf("accepted %q", b)
		}
	}
	// Keys are case-sensitive: "Type" is an unknown key, so "type" is missing.
	up := clientData(t, map[string]any{"Type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(challenge()), "origin": testOrigin})
	wantErr(t, CheckClientData(up, "webauthn.get", challenge(), testOrigin), errType)
}

func TestAttestationFormatNotNoneRefused(t *testing.T) {
	a := newAuthn(t)
	ad := authDataWith(testRPID, fUP|fUV|fAT, a.attestedData(a.coseKey()))
	packedStmt := cMap(cText("alg"), cInt(-7), cText("sig"), cBytes([]byte{1, 2, 3}))
	for name, obj := range map[string][]byte{
		"packed":            attObj("packed", packedStmt, ad),
		"packed-empty-stmt": attObj("packed", cMap(), ad),
		"none-nonempty":     attObj("none", packedStmt, ad),
		"none-stmt-array":   attObj("none", cArray(), ad),
		"fmt-not-text":      cMap(cText("fmt"), cBytes([]byte("none")), cText("attStmt"), cMap(), cText("authData"), cBytes(ad)),
		"missing-attStmt":   cMap(cText("fmt"), cText("none"), cText("authData"), cBytes(ad)),
		"extra-member": cMap(cText("fmt"), cText("none"), cText("attStmt"), cMap(), cText("authData"), cBytes(ad),
			cText("ep"), cInt(1)),
		"authData-not-bytes": cMap(cText("fmt"), cText("none"), cText("attStmt"), cMap(), cText("authData"), cText("x")),
	} {
		if _, err := ParseAttestationNone(obj, testRPID); !errors.Is(err, errFormat) {
			t.Errorf("%s: err = %v, want %v", name, err, errFormat)
		}
	}
}

func TestCBORMalformed(t *testing.T) {
	a := newAuthn(t)
	valid := a.registration(testRPID)
	for i := 0; i < len(valid); i++ {
		if _, err := ParseAttestationNone(valid[:i], testRPID); err == nil {
			t.Fatalf("truncation at %d accepted", i)
		}
	}
	// Truncations inside the authData byte string (re-wrapped so the CBOR stays well formed).
	ad := authDataWith(testRPID, fUP|fUV|fAT, a.attestedData(a.coseKey()))
	for i := 0; i < len(ad); i++ {
		if _, err := ParseAttestationNone(attObj("none", cMap(), ad[:i]), testRPID); err == nil {
			t.Fatalf("authData truncation at %d accepted", i)
		}
	}

	// Indefinite lengths: map, array, byte string, text string.
	indef := append([]byte{0xbf}, valid[1:]...)
	indef = append(indef, 0xff)
	for name, b := range map[string][]byte{
		"map":   indef,
		"array": {0x9f, 0x01, 0xff},
		"bytes": {0x5f, 0x41, 0x00, 0xff},
		"text":  {0x7f, 0x61, 0x61, 0xff},
	} {
		if _, err := decodeCBOR(b); !errors.Is(err, errCBOR) {
			t.Errorf("indefinite %s: err = %v", name, err)
		}
	}
	if _, err := ParseAttestationNone(indef, testRPID); !errors.Is(err, errCBOR) {
		t.Errorf("indefinite attestation: err = %v", err)
	}

	// Depth: 8 nested containers are fine, 9 are refused.
	nest := func(n int) []byte { return append(bytes.Repeat([]byte{0x81}, n-1), 0x80) }
	if _, err := decodeCBOR(nest(8)); err != nil {
		t.Fatalf("depth 8: %v", err)
	}
	if _, err := decodeCBOR(nest(9)); !errors.Is(err, errCBOR) {
		t.Fatalf("depth 9: err = %v", err)
	}
	if _, err := decodeCBOR(append(bytes.Repeat([]byte{0xa1, 0x01}, 8), 0xa0)); !errors.Is(err, errCBOR) {
		t.Fatal("map depth 9 accepted")
	}

	// Duplicate map keys.
	dup := cMap(cText("fmt"), cText("none"), cText("fmt"), cText("none"), cText("attStmt"), cMap(), cText("authData"), cBytes(ad))
	wantErr(t, func() error { _, err := ParseAttestationNone(dup, testRPID); return err }(), errCBOR)
	if _, err := decodeCBOR(cMap(cInt(-1), cInt(1), cInt(-1), cInt(2))); !errors.Is(err, errCBOR) {
		t.Fatal("duplicate int keys accepted")
	}
	// Different keys that only look alike are not duplicates.
	if _, err := decodeCBOR(cMap(cInt(1), cInt(0), cInt(-2), cInt(0), cText("1"), cInt(0))); err != nil {
		t.Fatalf("distinct keys refused: %v", err)
	}

	// Huge length claims.
	for name, b := range map[string][]byte{
		"bytes-2^64-1": {0x5b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"text-2^32":    {0x7b, 0, 0, 0, 1, 0, 0, 0, 0},
		"array-2^63":   {0x9b, 0x80, 0, 0, 0, 0, 0, 0, 0},
		"map-2^32-1":   {0xba, 0xff, 0xff, 0xff, 0xff},
		"bytes-past":   {0x58, 0x10, 0x00},
		"map-count":    {0xa2, 0x01, 0x02},
	} {
		if _, err := decodeCBOR(b); !errors.Is(err, errCBOR) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// Unsupported major types / reserved additional info.
	for name, b := range map[string][]byte{
		"tag":       {0xc1, 0x00},
		"float":     {0xf9, 0x3c, 0x00},
		"true":      {0xf5},
		"reserved":  {0x1c},
		"empty":     {},
		"map-key-b": cMap(cBytes([]byte{1}), cInt(0)),
	} {
		if _, err := decodeCBOR(b); !errors.Is(err, errCBOR) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// Trailing bytes at the top level.
	if _, err := ParseAttestationNone(append(append([]byte{}, valid...), 0x00), testRPID); !errors.Is(err, errCBOR) {
		t.Fatalf("trailing bytes: err = %v", err)
	}
	// Trailing bytes after the COSE key inside authData.
	adTrail := authDataWith(testRPID, fUP|fUV|fAT, a.attestedData(append(a.coseKey(), 0x00)))
	if _, err := ParseAttestationNone(attObj("none", cMap(), adTrail), testRPID); !errors.Is(err, errAuthData) {
		t.Fatalf("trailing after COSE key: err = %v", err)
	}
}

func TestOversizedInputRefused(t *testing.T) {
	a := newAuthn(t)
	big := attObj("none", cMap(), append(authDataWith(testRPID, fUP|fUV|fAT, a.attestedData(a.coseKey())), make([]byte, maxAttestationObject)...))
	_, err := ParseAttestationNone(big, testRPID)
	wantErr(t, err, errTooLarge)

	cdj := clientData(t, map[string]any{
		"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(challenge()),
		"origin": testOrigin, "pad": strings.Repeat("a", maxClientDataJSON),
	})
	wantErr(t, CheckClientData(cdj, "webauthn.get", challenge(), testOrigin), errTooLarge)
	wantErr(t, VerifyAssertion(a.assertion(t, testRPID, fUP|fUV, cdj)), errTooLarge)

	// Credential IDs are limited to 1..1023 bytes.
	for _, n := range []int{0, 1024} {
		b := &authn{key: a.key, credID: make([]byte, n)}
		_, err := ParseAttestationNone(b.registration(testRPID), testRPID)
		wantErr(t, err, errAuthData)
	}
	b := &authn{key: a.key, credID: make([]byte, 1023)}
	if _, err := ParseAttestationNone(b.registration(testRPID), testRPID); err != nil {
		t.Fatalf("1023-byte credential ID: %v", err)
	}
}

func TestCOSEKeyRefusals(t *testing.T) {
	a := newAuthn(t)
	x, y := a.xy()
	// An x coordinate that is not on the curve with this y.
	offX := append([]byte{}, x...)
	offX[31] ^= 0x01
	pairs := func(kv ...[]byte) []byte { return cMap(kv...) }
	k := func(i int64) []byte { return cInt(i) }
	cases := map[string][]byte{
		"kty-OKP":     pairs(k(1), k(1), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"kty-RSA":     pairs(k(1), k(3), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"alg-RS256":   pairs(k(1), k(2), k(3), k(-257), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"alg-ES384":   pairs(k(1), k(2), k(3), k(-35), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"crv-P384":    pairs(k(1), k(2), k(3), k(-7), k(-1), k(2), k(-2), cBytes(x), k(-3), cBytes(y)),
		"kty-text":    pairs(k(1), cText("EC2"), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"x-31":        pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x[1:]), k(-3), cBytes(y)),
		"x-33":        pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(append([]byte{0}, x...)), k(-3), cBytes(y)),
		"y-31":        pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y[1:])),
		"y-33":        pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(append([]byte{0}, y...))),
		"y-bool":      pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), []byte{0xf5}),
		"off-curve":   pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(offX), k(-3), cBytes(y)),
		"zero-point":  pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(make([]byte, 32)), k(-3), cBytes(make([]byte, 32))),
		"missing-alg": pairs(k(1), k(2), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"missing-y":   pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x)),
		"extra-kid":   pairs(k(1), k(2), k(2), cBytes([]byte("kid")), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y)),
		"extra-d":     pairs(k(1), k(2), k(3), k(-7), k(-1), k(1), k(-2), cBytes(x), k(-3), cBytes(y), k(-4), cBytes(x)),
		"not-a-map":   cArray(k(1), k(2)),
	}
	for name, cose := range cases {
		obj := attObj("none", cMap(), authDataWith(testRPID, fUP|fUV|fAT, a.attestedData(cose)))
		if _, err := ParseAttestationNone(obj, testRPID); !errors.Is(err, errCOSEKey) {
			t.Errorf("%s: err = %v, want %v", name, err, errCOSEKey)
		}
	}
	// Member order does not matter.
	reordered := pairs(k(-3), cBytes(y), k(-2), cBytes(x), k(-1), k(1), k(3), k(-7), k(1), k(2))
	obj := attObj("none", cMap(), authDataWith(testRPID, fUP|fUV|fAT, a.attestedData(reordered)))
	if _, err := ParseAttestationNone(obj, testRPID); err != nil {
		t.Fatalf("reordered key refused: %v", err)
	}
}

func TestNonCanonicalDERRefused(t *testing.T) {
	a := newAuthn(t)
	in := a.okAssertion(t)
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(in.Signature, &sig); err != nil {
		t.Fatal(err)
	}
	der := func(r, s []byte, longLen bool) []byte {
		body := append(append([]byte{0x02, byte(len(r))}, r...), append([]byte{0x02, byte(len(s))}, s...)...)
		if longLen {
			return append([]byte{0x30, 0x81, byte(len(body))}, body...)
		}
		return append([]byte{0x30, byte(len(body))}, body...)
	}
	minimal := func(v *big.Int) []byte {
		b := v.Bytes()
		if b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return b
	}
	r, s := minimal(sig.R), minimal(sig.S)
	if !bytes.Equal(der(r, s, false), in.Signature) {
		t.Fatal("test DER builder does not reproduce the signature")
	}
	for name, bad := range map[string][]byte{
		"long-form-length": der(r, s, true),
		"padded-r":         der(append([]byte{0, 0}, r...), s, false),
		"trailing-byte":    append(append([]byte{}, in.Signature...), 0x00),
		"empty":            {},
		"raw-r-s":          append(sig.R.FillBytes(make([]byte, 32)), sig.S.FillBytes(make([]byte, 32))...),
	} {
		in2 := in
		in2.Signature = bad
		if err := VerifyAssertion(in2); !errors.Is(err, errSignature) {
			t.Errorf("%s: err = %v, want %v", name, err, errSignature)
		}
	}
}

func TestExtensionsFlagRefused(t *testing.T) {
	a := newAuthn(t)
	// Registration: ED set with an (empty) extensions map after the COSE key.
	ad := authDataWith(testRPID, fUP|fUV|fAT|fED, a.attestedData(append(a.coseKey(), cMap()...)))
	_, err := ParseAttestationNone(attObj("none", cMap(), ad), testRPID)
	wantErr(t, err, errFlags)

	// Assertion: ED set, 37 bytes, and with an extensions map appended.
	cdj := cd(t, "webauthn.get", challenge(), testOrigin)
	wantErr(t, VerifyAssertion(a.assertion(t, testRPID, fUP|fUV|fED, cdj)), errFlags)
	ad = append(authDataWith(testRPID, fUP|fUV|fED, nil), cMap()...)
	in := a.okAssertion(t)
	in.AuthenticatorData, in.Signature = ad, a.sign(t, ad, cdj)
	wantErr(t, VerifyAssertion(in), errAuthData)
	// Assertion authenticator data must be exactly 37 bytes.
	in = a.okAssertion(t)
	in.AuthenticatorData = in.AuthenticatorData[:36]
	wantErr(t, VerifyAssertion(in), errAuthData)
}

func TestVerifyAssertionRejectsBadKey(t *testing.T) {
	a := newAuthn(t)
	in := a.okAssertion(t)
	in.PublicKey = nil
	wantErr(t, VerifyAssertion(in), errSignature)
}
