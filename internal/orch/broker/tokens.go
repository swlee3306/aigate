package broker

import (
	"context"
	"encoding/json"
	"time"
)

// Token ops (W3b). `tokens` is an unsigned, secret-free read. `set_token` and
// `delete_token` are signed writes (the broker verifies the signature; orchd
// never judges it). set_token is the ONLY call that carries a token value.
// Like the registry ops, this file only shapes calls and replies: the broker's
// own text is never read.

// Op names of the token store.
const (
	OpTokens      = "tokens"
	OpSetToken    = "set_token"
	OpDeleteToken = "delete_token"
)

func init() {
	// A little above the broker's own limits (5 s read, 10 s writes).
	defaultTimeouts[OpTokens] = 8 * time.Second
	defaultTimeouts[OpSetToken] = 15 * time.Second
	defaultTimeouts[OpDeleteToken] = 15 * time.Second
}

// TokenRow is one `tokens` row as the broker reports it. Unknown fields of a
// newer broker are ignored (encoding/json does not reject them); Usable and
// Source are passed on to the console. A row never carries a value.
type TokenRow struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	Last4    string `json:"last4"`
	Expires  string `json:"expires"`
	SetAt    string `json:"set_at"`
	Source   string `json:"source"`
	DaysLeft *int   `json:"days_left"`
	Usable   bool   `json:"usable"`
}

// TokenList is the `tokens` reply.
type TokenList struct {
	Tokens   []TokenRow `json:"tokens"`
	WarnDays int        `json:"warn_days"`
}

// Tokens reads the token list (no signature, no side effect, no values).
func (c *Client) Tokens(ctx context.Context) (*TokenList, *OpError) {
	res, oerr := c.Do(ctx, OpTokens, map[string]any{})
	if oerr != nil {
		return nil, oerr
	}
	var v TokenList
	if err := json.Unmarshal(res, &v); err != nil {
		return nil, unavailable()
	}
	return &v, nil
}

// WriteToken sends set_token or delete_token once and returns the raw result
// object (a TokenRow for set_token, {ref, deleted} for delete_token). It never
// retries: see UnknownOutcome. A decode failure of an ok reply is the caller's
// to treat as an unknown outcome.
func (c *Client) WriteToken(ctx context.Context, op string, args any) (json.RawMessage, *OpError) {
	return c.Do(ctx, op, args)
}
