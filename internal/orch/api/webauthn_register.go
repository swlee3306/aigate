package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/store"
	"aigate/internal/textsafe"
	"aigate/internal/webauthn"
)

// Passkey enrollment into a pending list (design 8.4, R8, R18). Three steps:
//
//	POST /api/v1/admin/enroll {"id"?, "label"?}          socket principal only
//	    -> 201 {"code","id","label","expires"}
//	POST /api/v1/auth/webauthn/register/begin {"code"}   unauthenticated
//	    -> 200 {"challenge_id","expires","options":{PublicKeyCredentialCreationOptionsJSON}}
//	POST /api/v1/auth/webauthn/register/finish {"challenge_id","credential"}   unauthenticated
//	    -> 201 {"id","label","fingerprint","rp_id","origin"}
//
// `orch admin enroll` (over the socket) gets a one-time enrollment code: 8
// Crockford base32 characters (40 random bits), valid EnrollCodeTTL. The code
// is never stored: the store row's id is "enroll-" + SHA-256(code), so the
// lookup at begin is by hash. begin consumes the code (single use, also when
// the ceremony is never finished) and issues a registration challenge bound
// to the id/label chosen at enroll; the browser supplies only the code.
// finish consumes the challenge (single use, also on failure), verifies the
// client data (webauthn.create, exact challenge, Config.Origin) and the "none"
// attestation (Config.RPID hash, UP+UV, ES256 P-256), and appends the
// credential to the pending file (Config.PendingFile) with the RPID and Origin
// in force (R8). A pending credential is not a signer: it cannot log in or
// approve until root activates it with `orch admin signers add --pending ID
// --fingerprint FP`, which validates it against the registry rules.
//
// Errors are fixed codes and texts, never the input or a webauthn error text
// (CF1). A wrong, used or expired code all answer bad_enroll_code. The
// challenge purposes are "enroll" and "webauthn-register", never "login"
// (CF14): each step refuses a challenge of any other purpose.

const (
	ChallengePurposeEnroll   = "enroll"
	ChallengePurposeRegister = "webauthn-register"

	// EnrollCodeTTL is how long an enrollment code can start a registration.
	EnrollCodeTTL = 10 * time.Minute
	// RegisterChallengeTTL is how long a registration ceremony can take.
	RegisterChallengeTTL = 5 * time.Minute
	// DefaultEnrollChallengeCap bounds the live enrollment codes and,
	// separately, the live registration challenges (503 busy at the cap).
	DefaultEnrollChallengeCap = 200
	// MaxPendingCredentials bounds the live entries of the pending file.
	MaxPendingCredentials = 20
	// PendingTTL is how long a pending credential waits for activation;
	// older entries are ignored and pruned on the next write (and the CLI
	// refuses to activate them).
	PendingTTL = 7 * 24 * time.Hour
	// DefaultPendingFile is the pending-enrollment file (the same default as
	// `orch admin signers --pending-file`).
	DefaultPendingFile = "/home/orch/orchd/state/signers.pending.json"

	enrollAlphabet  = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32
	enrollCodeLen   = 8
	maxPendingBytes = 1 << 20
	minCredIDLen    = 16
	maxCredIDLen    = 1023
	maxLabelLen     = 64
	rpName          = "aigate"
)

var (
	errEnrollBody       = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"id\", \"label\"}, 모두 선택)")
	errEnrollID         = apiErr(http.StatusBadRequest, "bad_request", "ID 형식이 올바르지 않습니다(소문자·숫자·._-, 63자 이하)")
	errEnrollLabel      = apiErr(http.StatusBadRequest, "bad_request", "라벨이 올바르지 않습니다(64바이트 이하, 제어 문자 불가)")
	errSocketOnly       = apiErr(http.StatusForbidden, "socket_only", "등록 코드는 서버의 소켓(orch admin enroll)에서만 발급합니다")
	errIDTaken          = apiErr(http.StatusConflict, "id_taken", "이미 쓰이는 ID입니다(서명자 목록 또는 대기 목록)")
	errEnrollBusy       = apiErr(http.StatusServiceUnavailable, "busy", "진행 중인 등록 요청이 너무 많거나 대기 목록이 가득 찼습니다. 잠시 뒤 다시 시도하세요")
	errWebAuthnOff      = apiErr(http.StatusServiceUnavailable, "webauthn_unconfigured", "이 서버에는 패스키(rp-id, origin)가 설정되어 있지 않습니다")
	errBeginBody        = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"code\"})")
	errFinishBody       = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"challenge_id\", \"credential\"})")
	errBadEnrollCode    = apiErr(http.StatusUnauthorized, "bad_enroll_code", "등록 코드가 올바르지 않거나 만료되었거나 이미 사용되었습니다. 서버에서 `orch admin enroll`로 새 코드를 받으세요")
	errBadRegChallenge  = apiErr(http.StatusUnauthorized, "bad_challenge", "등록 챌린지가 없거나 만료되었거나 이미 사용되었습니다. 처음부터 다시 등록하세요")
	errBadAttestation   = apiErr(http.StatusBadRequest, "bad_attestation", "패스키 등록 응답이 올바르지 않습니다. 처음부터 다시 등록하세요")
	errBadOrigin        = apiErr(http.StatusBadRequest, "bad_origin", "등록 요청의 출처(origin)가 이 서버와 다릅니다")
	errAlreadyPending   = apiErr(http.StatusConflict, "already_pending", "이 패스키나 ID는 이미 대기 목록에 있습니다")
	errPendingNotStored = apiErr(http.StatusInternalServerError, "internal", "내부 오류")
)

// PendingCredential is one entry of the pending-enrollment file. Its JSON form
// is exactly cmd/orch's pendingCredential (Task 15), which reads the file
// strictly (unknown fields refused):
//
//	{"version":1,"pending":[{"id","label","credential_id","public_key","rp_id","origin","created"}]}
//
// credential_id is the unpadded base64url credential id, public_key the
// unpadded base64url 65-byte uncompressed P-256 point.
type PendingCredential struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	CredentialID string    `json:"credential_id"`
	PublicKey    string    `json:"public_key"`
	RPID         string    `json:"rp_id"`
	Origin       string    `json:"origin"`
	Created      time.Time `json:"created"`
}

type pendingFileJSON struct {
	Version int                 `json:"version"`
	Pending []PendingCredential `json:"pending"`
}

var errPendingFull = errors.New("pending list full")
var errPendingDup = errors.New("credential or id already pending")

// ReadPending reads the pending file. A missing file is an empty list; a
// symlink, a non-regular file, over 1 MiB or a malformed file is an error.
func ReadPending(path string) ([]PendingCredential, error) {
	li, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("pending file %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(li, fi) {
		return nil, fmt.Errorf("pending file %s changed while opening", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPendingBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPendingBytes {
		return nil, fmt.Errorf("pending file %s over %d bytes", path, maxPendingBytes)
	}
	var pf pendingFileJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil || pf.Version != 1 {
		return nil, fmt.Errorf("pending file %s is malformed", path)
	}
	return pf.Pending, nil
}

// LivePending returns the entries that still wait for an administrator: not
// older than PendingTTL at now, and not already activated, i.e. neither their
// id nor their (decoded) credential id is in the trusted signers registry. An
// absent or untrusted registry activates nothing. Counting (begin, enroll)
// and pruning (AppendPending) use this one rule.
func LivePending(ps []PendingCredential, now time.Time) []PendingCredential {
	reg, regErr := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	var active [][]byte
	if regErr == nil {
		for _, s := range reg.Signers {
			if s.Type == signing.TypeWebAuthnES256 {
				if id, err := rawURLStrict.DecodeString(s.CredentialID); err == nil {
					active = append(active, id)
				}
			}
		}
	}
	var live []PendingCredential
	for _, p := range ps {
		if !now.Before(p.Created.Add(PendingTTL)) {
			continue
		}
		if regErr == nil {
			if _, ok := reg.Find(p.ID); ok {
				continue
			}
			cid, err := rawURLStrict.DecodeString(p.CredentialID)
			activated := false
			for _, a := range active {
				activated = activated || (err == nil && bytes.Equal(a, cid))
			}
			if activated {
				continue
			}
		}
		live = append(live, p)
	}
	return live
}

// AppendPending adds c to the pending file atomically (temp file in the same
// directory, mode 0640, fsync, rename, directory fsync). Entries that are no
// longer live at now (expired or activated, LivePending) are pruned. It refuses
// a credential id (compared decoded, CF2) or an id that is already pending,
// and a list that already holds max entries. The caller serialises calls.
func AppendPending(path string, c PendingCredential, max int, now time.Time) error {
	all, err := ReadPending(path)
	if err != nil {
		return err
	}
	ps := LivePending(all, now)
	newID, err := rawURLStrict.DecodeString(c.CredentialID)
	if err != nil {
		return fmt.Errorf("credential id: %w", err)
	}
	for _, p := range ps {
		old, err := rawURLStrict.DecodeString(p.CredentialID)
		if p.ID == c.ID || (err == nil && bytes.Equal(old, newID)) {
			return errPendingDup
		}
	}
	if len(ps) >= max {
		return errPendingFull
	}
	ps = append(ps, c)
	data, err := json.MarshalIndent(pendingFileJSON{Version: 1, Pending: ps}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxPendingBytes {
		return errPendingFull
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".signers.pending-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(0o640); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

var rawURLStrict = base64.RawURLEncoding.Strict()

// publicSuffixes are multi-label public suffixes refused as an rp-id (a
// small deny list, not the Public Suffix List; single labels other than
// localhost are refused separately).
var publicSuffixes = map[string]bool{"ts.net": true, "co.uk": true}

var rpIDRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ValidateWebAuthnConfig checks the relying party settings. Both empty means
// passkeys are off (the register routes answer webauthn_unconfigured). rpID
// is a lowercase host name (no scheme, port or path), not an IP address and
// not a public suffix (a single label other than localhost, or ts.net and
// co.uk). origin is "https://host[:port]" (never the default port) where
// host is rpID or a subdomain of it (WebAuthn's registrable-suffix rule), or
// "http://localhost[:port]" with rpID "localhost" for development. Pending credentials registered under the
// development origin cannot be activated (the registry accepts https only).
func ValidateWebAuthnConfig(rpID, origin string) error {
	if rpID == "" && origin == "" {
		return nil
	}
	if rpID == "" || origin == "" {
		return errors.New("--rp-id and --origin must be given together")
	}
	if len(rpID) > 253 || !rpIDRE.MatchString(rpID) {
		return fmt.Errorf("--rp-id %q must be a lowercase host name (no scheme, port or path)", rpID)
	}
	if net.ParseIP(rpID) != nil {
		return fmt.Errorf("--rp-id %q must be a host name, not an IP address", rpID)
	}
	if (!strings.Contains(rpID, ".") && rpID != "localhost") || publicSuffixes[rpID] {
		return fmt.Errorf("--rp-id %q is a public suffix, not a registrable domain", rpID)
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" ||
		u.Fragment != "" || u.ForceQuery || origin != u.Scheme+"://"+u.Host {
		return fmt.Errorf("--origin %q must be scheme://host[:port] with nothing after it", origin)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return fmt.Errorf("--origin %q has a bad port", origin)
		}
		// Browsers serialise an origin without its scheme's default port.
		if (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80) {
			return fmt.Errorf("--origin %q must not name the default port (browsers send the origin without it)", origin)
		}
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
		if host != rpID && !strings.HasSuffix(host, "."+rpID) {
			return fmt.Errorf("--origin host %q must be --rp-id %q or a subdomain of it", host, rpID)
		}
	case "http":
		if host != "localhost" || rpID != "localhost" {
			return errors.New("--origin may use http only as http://localhost with --rp-id localhost (development)")
		}
	default:
		return fmt.Errorf("--origin %q must be https", origin)
	}
	return nil
}

// enrollData is an enrollment code's (and a registration challenge's) stored
// identity: what root chose at `orch admin enroll`.
type enrollData struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type registerData struct {
	enrollData
	Challenge string `json:"challenge"` // base64url, 32 bytes
	UserID    string `json:"user_id"`
}

// enrollChallengeID is the store id of an enrollment code: only its hash is
// stored.
func enrollChallengeID(code string) string { return "enroll-" + store.HashSecret(code) }

// newEnrollCode is 8 Crockford base32 characters from 40 random bits.
func newEnrollCode() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	out := make([]byte, enrollCodeLen)
	for i := enrollCodeLen - 1; i >= 0; i-- {
		out[i] = enrollAlphabet[v&31]
		v >>= 5
	}
	return string(out), nil
}

// normalizeEnrollCode accepts the printed form ("ABCD-EFGH") case-insensitively
// with Crockford's aliases (O->0, I/L->1); "" when it is not a code.
func normalizeEnrollCode(s string) string {
	if len(s) > 32 {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if r > 0x7f || !strings.ContainsRune(enrollAlphabet, r) {
			return ""
		}
		b.WriteRune(r)
	}
	if b.Len() != enrollCodeLen {
		return ""
	}
	return b.String()
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// idTaken reports whether id is already pending or a registered signer (an
// untrusted or absent registry only skips that half; activation re-checks).
// Expired and activated pending entries do not hold their id.
func (h *handler) idTaken(id string) (bool, error) {
	ps, err := ReadPending(h.pendingFile)
	if err != nil {
		return false, err
	}
	for _, p := range LivePending(ps, h.clock()) {
		if p.ID == id {
			return true, nil
		}
	}
	if reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions); err == nil {
		if _, ok := reg.Find(id); ok {
			return true, nil
		}
	}
	return false, nil
}

// pendingFull reports whether the pending file has no room for another live
// entry (expired and activated entries do not count, LivePending).
func (h *handler) pendingFull() (bool, error) {
	ps, err := ReadPending(h.pendingFile)
	return len(LivePending(ps, h.clock())) >= MaxPendingCredentials, err
}

// atCap answers busy when purpose has h.enrollChallengeCap live rows.
func (h *handler) atCap(r *http.Request, purpose string) (*httpError, bool) {
	n, err := h.st.CountChallenges(r.Context(), purpose)
	if err != nil {
		log.Printf("orchd: enroll: count %s challenges: %v", purpose, err)
		return errInternal, true
	}
	if n >= h.enrollChallengeCap {
		log.Printf("orchd: enroll: refused, %d live %s challenges (cap %d)", n, purpose, h.enrollChallengeCap)
		return errEnrollBusy, true
	}
	return nil, false
}

// adminEnroll serves POST /api/v1/admin/enroll (socket principals only).
func (h *handler) adminEnroll(w http.ResponseWriter, r *http.Request) {
	if p, ok := PrincipalFrom(r.Context()); !ok || p.Kind != KindSocket {
		writeErr(w, errSocketOnly)
		return
	}
	if h.rpID == "" {
		writeErr(w, errWebAuthnOff)
		return
	}
	var req enrollData
	if herr := decodeLoginBody(w, r, &req, errEnrollBody); herr != nil {
		writeErr(w, herr)
		return
	}
	if req.ID == "" {
		suffix, err := randomHex(4)
		if err != nil {
			writeErr(w, errInternal)
			return
		}
		req.ID = "pk-" + suffix
	} else if !signing.ValidSignerID(req.ID) {
		writeErr(w, errEnrollID)
		return
	}
	if req.Label == "" {
		req.Label = req.ID
	}
	if _, bad := textsafe.FirstUnsafe(req.Label); bad || len(req.Label) > maxLabelLen || !utf8.ValidString(req.Label) {
		writeErr(w, errEnrollLabel)
		return
	}
	switch taken, err := h.idTaken(req.ID); {
	case err != nil:
		log.Printf("orchd: enroll: pending file: %v", err)
		writeErr(w, errInternal)
		return
	case taken:
		writeErr(w, errIDTaken)
		return
	}
	if herr, stop := h.atCap(r, ChallengePurposeEnroll); stop {
		writeErr(w, herr)
		return
	}
	data, err := json.Marshal(req)
	if err != nil {
		writeErr(w, errInternal)
		return
	}
	expires := h.clock().UTC().Truncate(time.Second).Add(EnrollCodeTTL)
	var code string
	for try := 0; ; try++ {
		if code, err = newEnrollCode(); err != nil {
			writeErr(w, errInternal)
			return
		}
		err = h.st.SaveChallenge(r.Context(), store.Challenge{ID: enrollChallengeID(code),
			Purpose: ChallengePurposeEnroll, Data: string(data), ExpiresAt: expires})
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrExists) || try > 0 {
			log.Printf("orchd: enroll: save: %v", err)
			writeErr(w, errInternal)
			return
		}
	}
	log.Printf("orchd: enrollment code issued for pending id %q", req.ID)
	writeJSON(w, http.StatusCreated, map[string]string{"code": code[:4] + "-" + code[4:],
		"id": req.ID, "label": req.Label, "expires": expires.Format(time.RFC3339)})
}

// registerBegin serves POST /api/v1/auth/webauthn/register/begin.
func (h *handler) registerBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if herr := decodeLoginBody(w, r, &req, errBeginBody); herr != nil {
		writeErr(w, herr)
		return
	}
	if h.rpID == "" {
		writeErr(w, errWebAuthnOff)
		return
	}
	code := normalizeEnrollCode(req.Code)
	if code == "" {
		writeErr(w, errBadEnrollCode)
		return
	}
	// Capacity first: a busy server must not burn a valid code.
	if herr, stop := h.atCap(r, ChallengePurposeRegister); stop {
		writeErr(w, herr)
		return
	}
	switch full, err := h.pendingFull(); {
	case err != nil:
		log.Printf("orchd: register begin: pending file: %v", err)
		writeErr(w, errInternal)
		return
	case full:
		log.Printf("orchd: register begin: refused, pending list full (%d)", MaxPendingCredentials)
		writeErr(w, errEnrollBusy)
		return
	}
	ctx := r.Context()
	// Consumed here, whatever happens next.
	ch, err := h.st.TakeChallenge(ctx, enrollChallengeID(code))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, errBadEnrollCode)
		return
	}
	if err != nil {
		log.Printf("orchd: register begin: take: %v", err)
		writeErr(w, errInternal)
		return
	}
	var ed enrollData
	if ch.Purpose != ChallengePurposeEnroll || json.Unmarshal([]byte(ch.Data), &ed) != nil ||
		!signing.ValidSignerID(ed.ID) || !h.clock().Before(ch.ExpiresAt) {
		writeErr(w, errBadEnrollCode)
		return
	}
	id, err1 := randomHex(32)
	chal, err2 := randomB64(32)
	user, err3 := randomB64(16)
	if err1 != nil || err2 != nil || err3 != nil {
		writeErr(w, errInternal)
		return
	}
	data, err := json.Marshal(registerData{enrollData: ed, Challenge: chal, UserID: user})
	if err != nil {
		writeErr(w, errInternal)
		return
	}
	expires := h.clock().UTC().Truncate(time.Second).Add(RegisterChallengeTTL)
	if err := h.st.SaveChallenge(ctx, store.Challenge{ID: id, Purpose: ChallengePurposeRegister,
		Data: string(data), ExpiresAt: expires}); err != nil {
		log.Printf("orchd: register begin: save: %v", err)
		writeErr(w, errInternal)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge_id": id,
		"expires":      expires.Format(time.RFC3339),
		"options": map[string]any{
			"rp":               map[string]string{"id": h.rpID, "name": rpName},
			"user":             map[string]string{"id": user, "name": "aigate:" + ed.ID, "displayName": ed.Label},
			"challenge":        chal,
			"pubKeyCredParams": []map[string]any{{"type": "public-key", "alg": -7}},
			"timeout":          RegisterChallengeTTL.Milliseconds(),
			"attestation":      "none",
			"authenticatorSelection": map[string]string{
				"residentKey": "preferred", "userVerification": "required"},
		},
	})
}

// registerCredential is the part of PublicKeyCredential.toJSON() that finish
// reads; other members are ignored.
type registerCredential struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
	} `json:"response"`
}

// decodeB64URL decodes canonical unpadded base64url (the decoder skips CR/LF,
// so the input must re-encode to itself).
func decodeB64URL(s string) ([]byte, bool) {
	b, err := rawURLStrict.DecodeString(s)
	if err != nil || len(b) == 0 || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// registerFinish serves POST /api/v1/auth/webauthn/register/finish.
func (h *handler) registerFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
	}
	if herr := decodeLoginBody(w, r, &req, errFinishBody); herr != nil {
		writeErr(w, herr)
		return
	}
	if h.rpID == "" {
		writeErr(w, errWebAuthnOff)
		return
	}
	ctx := r.Context()
	// Consumed here, whatever happens next (single use, also on failure).
	ch, err := h.st.TakeChallenge(ctx, req.ChallengeID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, errBadRegChallenge)
		return
	}
	if err != nil {
		log.Printf("orchd: register finish: take: %v", err)
		writeErr(w, errInternal)
		return
	}
	var rd registerData
	if ch.Purpose != ChallengePurposeRegister || json.Unmarshal([]byte(ch.Data), &rd) != nil ||
		!signing.ValidSignerID(rd.ID) || !h.clock().Before(ch.ExpiresAt) {
		writeErr(w, errBadRegChallenge)
		return
	}
	challenge, ok := decodeB64URL(rd.Challenge)
	if !ok {
		writeErr(w, errBadRegChallenge)
		return
	}
	var cred registerCredential
	if len(req.Credential) == 0 || req.Credential[0] != '{' || json.Unmarshal(req.Credential, &cred) != nil ||
		cred.Type != "public-key" {
		writeErr(w, errBadAttestation)
		return
	}
	clientData, ok1 := decodeB64URL(cred.Response.ClientDataJSON)
	attObj, ok2 := decodeB64URL(cred.Response.AttestationObject)
	credID, ok3 := decodeB64URL(cred.ID)
	if !(ok1 && ok2 && ok3) {
		writeErr(w, errBadAttestation)
		return
	}
	if err := webauthn.CheckClientData(clientData, "webauthn.create", challenge, h.origin); err != nil {
		log.Printf("orchd: register finish: client data refused: %v", err)
		switch {
		case errors.Is(err, webauthn.ErrOrigin):
			writeErr(w, errBadOrigin)
		case errors.Is(err, webauthn.ErrChallenge):
			writeErr(w, errBadRegChallenge)
		default:
			writeErr(w, errBadAttestation)
		}
		return
	}
	c, err := webauthn.ParseAttestationNone(attObj, h.rpID)
	if err != nil {
		log.Printf("orchd: register finish: attestation refused: %v", err)
		writeErr(w, errBadAttestation)
		return
	}
	if len(c.ID) < minCredIDLen || len(c.ID) > maxCredIDLen || !bytes.Equal(c.ID, credID) {
		log.Printf("orchd: register finish: credential id refused (%d bytes, matches id=%v)", len(c.ID), bytes.Equal(c.ID, credID))
		writeErr(w, errBadAttestation)
		return
	}
	if cred.RawID != "" {
		if raw, ok := decodeB64URL(cred.RawID); !ok || !bytes.Equal(raw, c.ID) {
			writeErr(w, errBadAttestation)
			return
		}
	}
	point, err := c.PublicKey.Bytes()
	if err != nil || len(point) != 65 {
		writeErr(w, errBadAttestation)
		return
	}
	pc := PendingCredential{ID: rd.ID, Label: rd.Label,
		CredentialID: base64.RawURLEncoding.EncodeToString(c.ID),
		PublicKey:    base64.RawURLEncoding.EncodeToString(point),
		RPID:         h.rpID, Origin: h.origin, Created: h.clock().UTC().Truncate(time.Second)}
	h.pendingMu.Lock()
	err = AppendPending(h.pendingFile, pc, MaxPendingCredentials, h.clock())
	h.pendingMu.Unlock()
	switch {
	case errors.Is(err, errPendingDup):
		writeErr(w, errAlreadyPending)
		return
	case errors.Is(err, errPendingFull):
		writeErr(w, errEnrollBusy)
		return
	case err != nil:
		log.Printf("orchd: register finish: pending file: %v", err)
		writeErr(w, errPendingNotStored)
		return
	}
	sum := sha256.Sum256(point)
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	log.Printf("orchd: passkey pending as %q (%s); activate with orch admin signers add --pending", pc.ID, fp)
	writeJSON(w, http.StatusCreated, map[string]string{"id": pc.ID, "label": pc.Label,
		"fingerprint": fp, "rp_id": pc.RPID, "origin": pc.Origin})
}
