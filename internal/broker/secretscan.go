package broker

import "regexp"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)["']?(?:api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["'][^"'\s]{12,}["']`),
	regexp.MustCompile(`\b(?:sk|glpat|ghp|gho)[-_][A-Za-z0-9_\-]{20,}`),
	// unquoted assignments: API_KEY=..., YAML "token: ...", ENV PASSWORD=...
	regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd|private[_-]?key|access[_-]?key)[A-Za-z0-9_]*["']?\s*[:=]\s*["']?[A-Za-z0-9_\-+/=.]{16,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`), // JWT
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                               // Slack
}

// LooksLikeSecret reports whether text has the shape of a credential: a
// private key header, an AWS access key id, a long quoted value assigned to
// api_key/secret/token/password, or a well-known token prefix. The broker
// refuses to send such text to the AI (second line after orchd's check).
// This is a best-effort second line of defence, not a guarantee: it matches
// well-known shapes only, and may flag harmless text (such files are skipped).
func LooksLikeSecret(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
