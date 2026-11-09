// Package broker is orchd's only way to the AI plan service and GitLab: a
// client of the orch-broker unix socket (protocol D1: one newline-terminated
// JSON request, one JSON reply, one connection per call). orchd holds no key
// and opens no other connection for these jobs.
package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"time"
)

// Limits of the wire.
const (
	// MaxRequestBytes is the broker's request cap (D1); bigger is refused
	// here, before connecting.
	MaxRequestBytes = 512 * 1024
	// MaxReplyBytes bounds what a reply may be (a plan with usage is far
	// smaller); more is treated as an unavailable broker.
	MaxReplyBytes = 1 << 20
	dialTimeout   = 2 * time.Second
)

// Codes the client itself produces (never the broker's text).
const (
	CodeUnavailable = "broker_unavailable"
	CodeTooLarge    = "too_large"
)

// Default whole-call limits: the broker's own processing limit (D1) plus a
// margin for connecting and writing.
var defaultTimeouts = map[string]time.Duration{
	"status":    8 * time.Second,
	"refine":    65 * time.Second,
	"create_mr": 25 * time.Second,
}

// OpError is a failed call. Code is the broker's registered code (a short
// lowercase word, validated here) or one of the client's own. Nothing else
// from the broker's reply is kept: its message text is never read.
type OpError struct{ Code string }

func (e *OpError) Error() string { return "broker: " + e.Code }

// Client talks to one broker socket. An empty Socket (feature off) answers
// broker_unavailable without connecting.
type Client struct {
	Socket string
	// Timeout, when positive, replaces the per-op default (tests).
	Timeout time.Duration
}

var codeRe = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

type request struct {
	V    int    `json:"v"`
	Op   string `json:"op"`
	Args any    `json:"args"`
}

type reply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Code   string          `json:"code"`
}

func unavailable() *OpError { return &OpError{Code: CodeUnavailable} }

// ValidSocketPath reports whether p may be configured: absolute and clean.
func ValidSocketPath(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }

// Do sends one request and returns the result object of an ok reply. Every
// failure is an *OpError with a fixed code; no transport or broker text
// escapes. The call is bounded by ctx and the op's time limit.
func (c *Client) Do(ctx context.Context, op string, args any) (json.RawMessage, *OpError) {
	if c == nil || c.Socket == "" || !ValidSocketPath(c.Socket) {
		return nil, unavailable()
	}
	body, err := json.Marshal(request{V: 1, Op: op, Args: args})
	if err != nil {
		return nil, &OpError{Code: "bad_request"}
	}
	if len(body) > MaxRequestBytes {
		return nil, &OpError{Code: CodeTooLarge}
	}
	d := c.Timeout
	if d <= 0 {
		d = defaultTimeouts[op]
	}
	if d <= 0 {
		d = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, dialTimeout)
	defer dcancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "unix", c.Socket)
	if err != nil {
		return nil, unavailable()
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// Closing the connection when ctx ends unblocks a stuck read or write.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return nil, unavailable()
	}
	line, err := readLine(conn)
	if err != nil {
		return nil, unavailable()
	}
	var r reply
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&r); err != nil {
		return nil, unavailable()
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, unavailable()
	}
	if !r.OK {
		if !codeRe.MatchString(r.Code) {
			return nil, unavailable()
		}
		return nil, &OpError{Code: r.Code}
	}
	if len(r.Result) == 0 || r.Result[0] != '{' {
		return nil, unavailable()
	}
	return r.Result, nil
}

func readLine(r io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, MaxReplyBytes+1), 32*1024)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxReplyBytes {
			return nil, errors.New("reply too large")
		}
		switch {
		case err == nil:
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}
