package broker

import (
	"context"
	"encoding/json"
	"os"
)

// currentOpenAIKey is the AI key for the next call (decision D2/D10): the
// store's `openai` secret when it is present and trusted, otherwise the
// config's key file (checked again on every call with the W2b rules). ok is
// false when neither is usable. The value goes only to the outbound request.
func (s *Server) currentOpenAIKey() (string, bool) {
	if v, ok := s.Tokens.Value("openai"); ok {
		return v, true
	}
	owner := s.keyOwnerOK
	if owner == nil {
		euid := uint32(os.Geteuid())
		owner = func(uid uint32) bool { return uid == euid }
	}
	if v, err := readSecretFile(s.Config.OpenAI.KeyFile, owner); err == nil {
		return v, true
	}
	return "", false
}

// tokensHandler lists the tokens (read-only, no values; last4 is the only
// trace of a value, see the design's limits). `source` says where the broker
// gets the token from; the config's OpenAI key file is listed as source
// "config" when no usable stored `openai` exists.
func (s *Server) tokensHandler(context.Context, json.RawMessage) (any, *OpError) {
	list, err := s.Tokens.List(s.now())
	if err != nil {
		return fail("tokens_unavailable")
	}
	out := make([]map[string]any, 0, len(list)+1)
	haveOpenAI := false
	for _, t := range list {
		if t.Ref == "openai" && t.Usable {
			haveOpenAI = true
		}
		var dl any
		if t.DaysLeft != nil {
			dl = *t.DaysLeft
		}
		out = append(out, map[string]any{
			"ref": t.Ref, "kind": t.Kind, "last4": t.Last4, "expires": t.Expires,
			"set_at": t.SetAt, "source": "store", "days_left": dl, "usable": t.Usable,
		})
	}
	if !haveOpenAI {
		if _, ok := s.currentOpenAIKey(); ok {
			// Replace an unusable stored entry's row by the one that is in effect.
			rows := out[:0]
			for _, r := range out {
				if r["ref"] != "openai" {
					rows = append(rows, r)
				}
			}
			out = append(rows, map[string]any{
				"ref": "openai", "kind": "openai", "last4": "", "expires": "",
				"set_at": "", "source": "config", "days_left": nil, "usable": true,
			})
		}
	}
	return map[string]any{"tokens": out, "warn_days": WarnDays}, nil
}
