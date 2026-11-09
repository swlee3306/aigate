package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Outbound OpenAI access. API choice (verified against the official docs at
// implementation time, see the task report): Responses API, POST /v1/responses,
// structured output via text.format {type: json_schema, strict: true}, no
// server-side storage via store:false, usage in usage.input_tokens and
// usage.output_tokens.

// DefaultOpenAIBaseURL is the only host the broker sends AI requests to. It
// is a constant, not a setting; tests inject another base through
// Options.OpenAIBaseURL.
const DefaultOpenAIBaseURL = "https://api.openai.com"

const (
	openAIUserAgent   = "orch-broker/1"
	openAIConnTimeout = 10 * time.Second
	openAITotalTime   = 55 * time.Second
	maxOpenAIResponse = 2 << 20
)

// errUpstream is the only error Complete returns: no upstream text, no
// request text, no key. Callers map it to the fixed code upstream_error.
var errUpstream = errors.New("upstream error")

// OpenAIClient calls the Responses API. Key is the API key, sent only in the
// Authorization header. HTTP nil means the hardened default client.
type OpenAIClient struct {
	BaseURL string // "" = DefaultOpenAIBaseURL
	Key     string
	Model   string
	HTTP    *http.Client
}

// newOpenAIHTTP builds a client that never follows redirects, never uses
// proxy settings from the environment, and has fixed timeouts.
func newOpenAIHTTP() *http.Client {
	return &http.Client{
		Timeout: openAITotalTime,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: openAIConnTimeout}).DialContext,
			TLSHandshakeTimeout: openAIConnTimeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type openAIUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type openAIResponse struct {
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *openAIUsage `json:"usage"`
}

// Complete sends one request and returns the model's text. The usage is
// returned whenever the response reported it, even when the text is unusable,
// so the budget can settle to the real cost. Every failure is errUpstream.
func (c *OpenAIClient) Complete(ctx context.Context, system, user string, maxTokens int64, schema json.RawMessage) (string, Usage, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultOpenAIBaseURL
	}
	body, err := json.Marshal(map[string]any{
		"model":             c.Model,
		"instructions":      system,
		"input":             user,
		"max_output_tokens": maxTokens,
		"store":             false,
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "plan", "strict": true, "schema": schema,
		}},
	})
	if err != nil {
		return "", Usage{}, errUpstream
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, errUpstream
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", openAIUserAgent)
	hc := c.HTTP
	if hc == nil {
		hc = newOpenAIHTTP()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", Usage{}, errUpstream // url.Error would quote the URL; drop it
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return "", Usage{}, errUpstream
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenAIResponse+1))
	if err != nil || len(raw) > maxOpenAIResponse {
		return "", Usage{}, errUpstream
	}
	var out openAIResponse
	if json.Unmarshal(raw, &out) != nil {
		return "", Usage{}, errUpstream
	}
	var u Usage
	if out.Usage != nil {
		u = Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}
	}
	if out.Status != "completed" {
		return "", u, errUpstream
	}
	var text string
	found := false
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, p := range item.Content {
			switch p.Type {
			case "output_text":
				text += p.Text
				found = true
			default: // refusal or anything unknown
				return "", u, errUpstream
			}
		}
	}
	if !found {
		return "", u, errUpstream
	}
	return text, u, nil
}
