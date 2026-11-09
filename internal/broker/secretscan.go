package broker

import (
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)["']?(?:api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["'][^"'\s]{12,}["']`),
	regexp.MustCompile(`\b(?:sk|glpat|ghp|gho)[-_][A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`), // JWT
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                               // Slack
}

// assignRE finds unquoted assignments such as API_KEY=..., YAML "token: ..."
// or ENV PASSWORD=...; assignLooksSecret then judges the value.
var assignRE = regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(?:api[_-]?key|secret|token|password|passwd|private[_-]?key|access[_-]?key)[A-Za-z0-9_]*["']?\s*[:=]\s*["']?([A-Za-z0-9_\-+/=.]{16,})`)

// assignLooksSecret: the value must contain a digit or mix upper and lower
// case (pure lowercase words are identifiers), and a value with '.' or '/'
// (property access, paths) needs a digit.
func assignLooksSecret(v string) bool {
	var digit, upper, lower bool
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		}
	}
	dotSlash := strings.ContainsAny(v, "./")
	return (digit || (upper && lower)) && (!dotSlash || digit)
}

// LooksLikeSecret reports whether text has the shape of a credential: a
// private key header, an AWS access key id, a long quoted value assigned to
// api_key/secret/token/password, or a well-known token prefix. The broker
// refuses to send such text to the AI (second line after orchd's check).
// This is a best-effort second line of defence, not a guarantee: it matches
// well-known shapes only, and may flag harmless text; a match refuses the whole request with
// looks_like_secret (nothing is sent).
func LooksLikeSecret(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	for _, m := range assignRE.FindAllStringSubmatch(text, -1) {
		if assignLooksSecret(m[1]) {
			return true
		}
	}
	return false
}
