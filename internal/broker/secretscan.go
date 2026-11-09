package broker

import "regexp"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)["']?(?:api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["'][^"'\s]{12,}["']`),
	regexp.MustCompile(`\b(?:sk|glpat|ghp|gho)[-_][A-Za-z0-9_\-]{20,}`),
}

// LooksLikeSecret reports whether text has the shape of a credential: a
// private key header, an AWS access key id, a long quoted value assigned to
// api_key/secret/token/password, or a well-known token prefix. The broker
// refuses to send such text to the AI (second line after orchd's check).
func LooksLikeSecret(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
