// Package config parses and validates .aigate.yml. It does not resolve
// presets into commands; that happens in the gate.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"aigate/internal/adapter"
)

const defaultTimeout = 300 * time.Second

// Check is one verification command.
type Check struct {
	ID       string
	Preset   string
	Adapter  string
	Command  []string
	Timeout  time.Duration
	Required bool
	// Setup is an optional command run in each worktree before the check.
	Setup []string
}

// Config is a parsed and validated .aigate.yml.
type Config struct {
	Version       int
	Protected     []string
	ApprovalOnAdd []string
	Checks        []Check
}

type rawCheck struct {
	ID       string    `yaml:"id"`
	Preset   string    `yaml:"preset"`
	Adapter  string    `yaml:"adapter"`
	Command  yaml.Node `yaml:"command"`
	Timeout  *string   `yaml:"timeout"`
	Required *bool     `yaml:"required"`
	Setup    yaml.Node `yaml:"setup"`
}

type rawConfig struct {
	Version       int        `yaml:"version"`
	Protected     []string   `yaml:"protected"`
	ApprovalOnAdd []string   `yaml:"approval_on_add"`
	Checks        []rawCheck `yaml:"checks"`
}

var adapterNames = map[string]bool{"junit": true, "gotest-json": true, "exit-code": true}

// Parse decodes and validates data.
func Parse(data []byte) (Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("config: empty document; version: 1 is required")
		}
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Config{}, fmt.Errorf("config: multiple documents or trailing content: %w", err)
		}
		return Config{}, errors.New("config: multiple documents are not allowed")
	}
	if raw.Version != 1 {
		return Config{}, fmt.Errorf("config: version must be 1 (got %d; missing?)", raw.Version)
	}
	cfg := Config{Version: 1, Protected: raw.Protected, ApprovalOnAdd: raw.ApprovalOnAdd}
	seen := map[string]bool{}
	for i, rc := range raw.Checks {
		if rc.ID == "" {
			return Config{}, fmt.Errorf("config: checks[%d]: id is required", i)
		}
		if seen[rc.ID] {
			return Config{}, fmt.Errorf("config: duplicate check id %q", rc.ID)
		}
		seen[rc.ID] = true
		c, err := buildCheck(rc)
		if err != nil {
			return Config{}, fmt.Errorf("config: check %q: %w", rc.ID, err)
		}
		cfg.Checks = append(cfg.Checks, c)
	}
	return cfg, nil
}

func buildCheck(rc rawCheck) (Check, error) {
	c := Check{ID: rc.ID, Preset: rc.Preset, Adapter: rc.Adapter, Timeout: defaultTimeout, Required: true}
	var err error
	if c.Command, err = argv(&rc.Command, "command"); err != nil {
		return Check{}, err
	}
	if c.Setup, err = argv(&rc.Setup, "setup"); err != nil {
		return Check{}, err
	}
	hasCmd := !rc.Command.IsZero()
	if rc.Preset != "" {
		if rc.Adapter != "" || hasCmd {
			return Check{}, errors.New("preset cannot be combined with adapter or command")
		}
		if _, ok := adapter.PresetFor(rc.Preset); !ok {
			return Check{}, fmt.Errorf("unknown preset %q (want pytest|go|vitest|jest)", rc.Preset)
		}
	} else {
		if rc.Adapter == "" || !hasCmd {
			return Check{}, errors.New("needs either preset, or both adapter and command")
		}
		if !adapterNames[rc.Adapter] {
			return Check{}, fmt.Errorf("unknown adapter %q (want junit|gotest-json|exit-code)", rc.Adapter)
		}
		if len(c.Command) == 0 {
			return Check{}, errors.New("command is empty")
		}
	}
	if rc.Timeout != nil {
		d, err := time.ParseDuration(*rc.Timeout)
		if err != nil {
			return Check{}, fmt.Errorf("invalid timeout %q: %w", *rc.Timeout, err)
		}
		if d <= 0 {
			return Check{}, fmt.Errorf("timeout must be > 0 (got %q)", *rc.Timeout)
		}
		c.Timeout = d
	}
	if rc.Required != nil {
		c.Required = *rc.Required
	}
	return c, nil
}

// argv converts a YAML scalar (split like a shell without shell features) or
// a sequence of strings into an argument vector. An absent node yields nil.
func argv(n *yaml.Node, field string) ([]string, error) {
	switch n.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		out, err := splitArgs(n.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return out, nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s must be a string or a list of strings", field)
}

// splitArgs splits s on whitespace honoring single quotes (literal), double
// quotes (backslash escapes) and backslash escapes outside quotes.
func splitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inArg := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		case r == '\\':
			if i+1 >= len(rs) {
				return nil, errors.New("trailing backslash")
			}
			i++
			cur.WriteRune(rs[i])
			inArg = true
		case r == '\'':
			inArg = true
			i++
			for ; i < len(rs) && rs[i] != '\''; i++ {
				cur.WriteRune(rs[i])
			}
			if i >= len(rs) {
				return nil, errors.New("unterminated single quote")
			}
		case r == '"':
			inArg = true
			i++
			for ; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				cur.WriteRune(rs[i])
			}
			if i >= len(rs) {
				return nil, errors.New("unterminated double quote")
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out, nil
}

// Hash returns a stable 16-hex-char sha256 prefix of the canonical form of
// the resolved config. It ignores YAML key order, whitespace and comments but
// changes with any value or list order.
func (c Config) Hash() string {
	type canonCheck struct {
		ID        string   `json:"id"`
		Preset    string   `json:"preset"`
		Adapter   string   `json:"adapter"`
		Command   []string `json:"command"`
		TimeoutNs int64    `json:"timeout_ns"`
		Required  bool     `json:"required"`
		Setup     []string `json:"setup"`
	}
	type canon struct {
		Version       int          `json:"version"`
		Protected     []string     `json:"protected"`
		ApprovalOnAdd []string     `json:"approval_on_add"`
		Checks        []canonCheck `json:"checks"`
	}
	nn := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	k := canon{Version: c.Version, Protected: nn(c.Protected), ApprovalOnAdd: nn(c.ApprovalOnAdd), Checks: []canonCheck{}}
	for _, ch := range c.Checks {
		k.Checks = append(k.Checks, canonCheck{ch.ID, ch.Preset, ch.Adapter, nn(ch.Command), int64(ch.Timeout), ch.Required, nn(ch.Setup)})
	}
	b, _ := json.Marshal(k)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}
