package refine

import (
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Exclusion reasons (fixed words; they are shown in the preview).
const (
	ReasonSecretName      = "secret_name"
	ReasonSymlink         = "symlink"
	ReasonSubmodule       = "submodule"
	ReasonBinary          = "binary"
	ReasonNotUTF8         = "not_utf8"
	ReasonTooLarge        = "too_large"
	ReasonLooksLikeSecret = "looks_like_secret"
	ReasonUnsafePath      = "unsafe_path"
	ReasonFileCap         = "file_cap"
	ReasonTotalCap        = "total_cap"
	ReasonUnreadable      = "unreadable"
)

// secretPatterns, assignRE and assignLooksSecret mirror the broker's scan
// (internal/broker/secretscan.go), which is the second line of defence: orchd
// must refuse at least what the broker would refuse, so a preview never
// offers a file the broker would then reject as a whole. They are copied, not
// imported, so orchd's binary carries nothing of the broker (it must have no
// way to reach the AI service itself).
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)["']?(?:api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["'][^"'\s]{12,}["']`),
	regexp.MustCompile(`\b(?:sk|glpat|ghp|gho)[-_][A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`), // JWT
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                               // Slack
}

var assignRE = regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(?:api[_-]?key|secret|token|password|passwd|private[_-]?key|access[_-]?key)[A-Za-z0-9_]*["']?\s*[:=]\s*["']?([A-Za-z0-9_\-+/=.]{16,})`)

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
// api_key/secret/token/password, an unquoted assignment of one, a JWT or a
// well-known token prefix. Best effort: it matches known shapes only and may
// flag harmless text; a match keeps the whole file (or request) out.
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

// secretName reports whether a path component names a file that usually holds
// credentials (D7): .env*, *.pem, *.key, id_rsa*, *secret*, *credential*,
// *.p12, *.pfx and a few relatives. Case-insensitive; every component of the
// path is checked, so secrets/x.txt is out too.
func secretName(p string) bool {
	for _, c := range strings.Split(p, "/") {
		l := strings.ToLower(c)
		switch {
		case l == ".git":
			return true
		case strings.HasPrefix(l, ".env"),
			strings.HasSuffix(l, ".pem"), strings.HasSuffix(l, ".key"),
			strings.HasSuffix(l, ".p12"), strings.HasSuffix(l, ".pfx"),
			strings.HasSuffix(l, ".keystore"), strings.HasSuffix(l, ".jks"),
			strings.HasPrefix(l, "id_rsa"), strings.HasPrefix(l, "id_dsa"),
			strings.HasPrefix(l, "id_ecdsa"), strings.HasPrefix(l, "id_ed25519"),
			strings.Contains(l, "secret"), strings.Contains(l, "credential"),
			l == ".netrc", l == ".npmrc", l == ".pgpass":
			return true
		}
	}
	return false
}

// badText mirrors the broker's: control characters (newline/tab only when
// allowNL), format characters (zero-width, bidi, soft hyphen, BOM), line and
// paragraph separators and invalid UTF-8.
func badText(s string, allowNL bool) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			if !allowNL {
				return true
			}
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return true
		case r == 0x061C || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069),
			r == 0x2028 || r == 0x2029, r == 0xFEFF:
			return true
		}
	}
	return false
}

// SafeRelPath is the broker's rule for a path it will accept: clean,
// relative, forward slashes, no "..", at most 200 bytes, no hostile
// characters.
func SafeRelPath(p string) bool {
	if p == "" || len(p) > 200 || badText(p, false) || strings.Contains(p, `\`) {
		return false
	}
	if strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "." {
		return false
	}
	return p != ".." && !strings.HasPrefix(p, "../")
}

// CheckRequirement validates the requirement text the way the console's
// findHostile does (C0/C1 controls, DEL, bidi/zero-width characters, line
// separators, invalid UTF-8; newline and tab allowed) and bounds its length
// (2000 characters, as the broker). CRLF must already be normalised.
func CheckRequirement(s string) bool {
	if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > MaxRequirementRunes {
		return false
	}
	if badText(s, true) {
		return false
	}
	for _, r := range s {
		// Characters of the console's INVISIBLE_RE that are not category Cf.
		switch {
		case r == 0x034F, r == 0x115F, r == 0x1160, r == 0x17B4, r == 0x17B5, r == 0x3164,
			(r >= 0xFE00 && r <= 0xFE0F), (r >= 0xFFF9 && r <= 0xFFFB), (r >= 0xE0000 && r <= 0xE007F):
			return false
		}
	}
	return true
}
