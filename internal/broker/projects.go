package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// Registry write ops (register_project, set_flag) change nothing without a
// valid signature of a trusted signer over the exact change (decisions D3-D7).
// Every failure is a fixed code; signing/registry error text is never forwarded.

const issuedLayout = "2006-01-02T15:04:05Z"

// Timeouts: a `timeout` reply to register_project or set_flag means the
// outcome is UNKNOWN (the handler may still complete); callers must re-read
// `projects` before retrying.

// opCodeErr carries a fixed code out of a RegistryStore.Update callback.
type opCodeErr string

func (e opCodeErr) Error() string { return string(e) }

func fail1(code string) *OpError { return &OpError{Code: code} }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var (
	registerKeys  = []string{"id", "path", "kind", "ai_context", "change", "request", "issued", "signature"}
	setFlagKeys   = []string{"id", "flag", "value", "change", "request", "issued", "signature"}
	signatureKeys = []string{"signer", "alg", "sig", "webauthn"}
	webauthnKeys  = []string{"authenticator_data", "client_data_json"}
)

// exactObjectKeys reports whether raw is a JSON object whose keys are all in allowed
// (exact case: encoding/json would also match "Path" to "path") and unique.
// It returns the raw values by key.
func exactObjectKeys(raw []byte, allowed []string) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		key, isStr := kt.(string)
		if err != nil || !isStr {
			return nil, false
		}
		known := false
		for _, a := range allowed {
			known = known || a == key
		}
		if _, dup := out[key]; dup || !known {
			return nil, false
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return nil, false
		}
		out[key] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	_, err := dec.Token()
	return out, err == io.EOF
}

// decodeStrict decodes the args object into v: only the exact allowed keys
// (also inside the signature object), no duplicates, no trailing data.
func decodeStrict(raw json.RawMessage, v any, allowed []string) bool {
	top, ok := exactObjectKeys(raw, allowed)
	if !ok {
		return false
	}
	if sig, has := top["signature"]; has && !bytes.Equal(bytes.TrimSpace(sig), []byte("null")) {
		sm, ok := exactObjectKeys(sig, signatureKeys)
		if !ok {
			return false
		}
		if wa, has := sm["webauthn"]; has && !bytes.Equal(bytes.TrimSpace(wa), []byte("null")) {
			if _, ok := exactObjectKeys(wa, webauthnKeys); !ok {
				return false
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

// projectsHandler is the read-only, secret-free registry listing (D7).
func (s *Server) projectsHandler(context.Context, json.RawMessage) (any, *OpError) {
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	r := s.Registry.Snapshot()
	projects := r.Projects
	if projects == nil {
		projects = []Project{}
	}
	return map[string]any{
		"projects":          projects,
		"providers":         []struct{}{},
		"auto_merge_global": r.AutoMergeGlobal,
	}, nil
}

type registerArgs struct {
	ID        string             `json:"id"`
	Path      string             `json:"path"`
	Kind      string             `json:"kind"`
	AIContext *bool              `json:"ai_context"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

type setFlagArgs struct {
	ID        string             `json:"id"`
	Flag      string             `json:"flag"`
	Value     *bool              `json:"value"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

func (s *Server) registerProjectHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a registerArgs
	if !decodeStrict(raw, &a, registerKeys) || a.AIContext == nil {
		return fail("bad_request")
	}
	fields := map[string]any{"path": a.Path, "kind": a.Kind, "ai_context": *a.AIContext}
	if oe := s.verifySigned(registrymsg.ActionRegisterProject, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed: later refusals cannot be retried
	// with the same signature.
	if !s.pathAllowed(a.Path) {
		return fail("path_not_allowed")
	}
	var out Project
	err := s.Registry.Update(func(r *Registry) error {
		for _, p := range r.Projects {
			if p.ID == a.ID || p.Path == a.Path {
				return opCodeErr("project_exists")
			}
		}
		out = Project{ID: a.ID, Path: a.Path, Kind: a.Kind, AIContext: *a.AIContext}
		r.Projects = append(r.Projects, out)
		return nil
	})
	if err != nil {
		return nil, updateFailure(err)
	}
	return out, nil
}

func (s *Server) setFlagHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a setFlagArgs
	if !decodeStrict(raw, &a, setFlagKeys) || a.Value == nil {
		return fail("bad_request")
	}
	// W3a changes only ai_context; auto_merge, the global switch and every
	// other flag are refused (unsigned too, so no ID is spent on them).
	if a.Flag != "ai_context" {
		return fail("not_supported")
	}
	fields := map[string]any{"flag": a.Flag, "value": *a.Value}
	if oe := s.verifySigned(registrymsg.ActionSetFlag, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature); oe != nil {
		return nil, oe
	}
	var out Project
	err := s.Registry.Update(func(r *Registry) error {
		for i := range r.Projects {
			if r.Projects[i].ID == a.ID {
				r.Projects[i].AIContext = *a.Value
				out = r.Projects[i]
				return nil
			}
		}
		return opCodeErr("project_unknown")
	})
	if err != nil {
		return nil, updateFailure(err)
	}
	return out, nil
}

func updateFailure(err error) *OpError {
	var ce opCodeErr
	if errors.As(err, &ce) {
		return fail1(string(ce))
	}
	return fail1("registry_unavailable") // broken, too big, cannot be saved
}

// verifySigned runs the shared checks of the write ops in the required order:
// registry usable -> signature present -> message built from the request ->
// signer list reloaded (every call) -> signature verified -> signed change
// equals the change recomputed from the request -> request ID consumed.
// claimedChange is the change hash the signer saw (required); the signature is
// verified over it and it must equal the hash recomputed from the request.
func (s *Server) verifySigned(action, subject string, fields map[string]any, claimedChange, request, issuedText string, sig *signing.Signature) *OpError {
	if s.Registry.Err() != nil {
		return fail1("registry_unavailable")
	}
	if sig == nil {
		return fail1("not_signed")
	}
	if claimedChange == "" { // `change` is required: the signer's hash must travel with the request
		return fail1("change_mismatch")
	}
	issued, err := time.Parse(issuedLayout, issuedText)
	if err != nil {
		return fail1("bad_request")
	}
	want, err := registrymsg.ChangeHash(action, subject, fields)
	if err != nil {
		return fail1("bad_request")
	}
	signed := claimedChange
	msg := signing.RegistryMessage{Action: action, Subject: subject, Change: signed, Request: request, Issued: issued.UTC()}
	if msg.Validate() != nil {
		return fail1("bad_request")
	}
	signers, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	if err != nil {
		return fail1("signers_unavailable")
	}
	now := s.now()
	switch err := signing.VerifyRegistry(signers, msg, *sig, signing.VerifyOptions{Now: now}); {
	case err == nil:
	case errors.Is(err, signing.ErrUnknownSigner):
		return fail1("unknown_signer")
	case errors.Is(err, signing.ErrStale):
		return fail1("stale_signature")
	case errors.Is(err, signing.ErrInvalidMessage):
		return fail1("bad_request")
	default:
		return fail1("bad_signature")
	}
	if signed != want {
		return fail1("change_mismatch")
	}
	switch err := s.Used.Mark(request, now); {
	case err == nil:
	case errors.Is(err, ErrRequestReused):
		return fail1("request_reused")
	case errors.Is(err, ErrBadRequestID): // unreachable (msg.Validate checked the ID): defence in depth
		return fail1("change_mismatch")
	default: // broken file, full, cannot be saved
		return fail1("registry_unavailable")
	}
	return nil
}

// pathAllowed enforces the SYNTACTIC registration path rules; the broker never
// touches the filesystem for a registered path (its account cannot read
// /home/orch/repos by design, and the property it protects is only that the AI
// context applies to exactly the path the signer approved). The path must be at
// most maxPathBytes long, absolute, clean and exactly one level below a
// configured repo_roots entry (exact string match; the root itself is refused).
// Whether it is a real, non-symlink git repository is orchd's check at propose
// and apply time, and use time refuses a path that was swapped later.
func (s *Server) pathAllowed(p string) bool {
	if len(p) == 0 || len(p) > maxPathBytes || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	parent := filepath.Dir(p)
	if parent == p {
		return false
	}
	for _, r := range s.Config.RepoRoots {
		if r == parent {
			return true
		}
	}
	return false
}
