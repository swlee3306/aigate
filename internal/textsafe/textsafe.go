// Package textsafe finds and escapes bytes that a terminal or a markdown
// viewer could interpret instead of showing: C0 controls other than "\n" and
// "\t" (ESC, CR, NUL, ...), DEL, invalid UTF-8, C1 controls (U+0080–U+009F)
// and the bidi embedding/override/isolate characters (U+202A–U+202E,
// U+2066–U+2069). It is a leaf package.
package textsafe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Finding locates the first unsafe byte or rune of a text.
type Finding struct {
	Offset int    // byte offset in the text
	Line   int    // 1-based line number ("\n" separated)
	Desc   string // `\xNN` for a byte, `U+XXXX` for a rune; never the raw text
}

// unsafeRune reports whether r (decoded with width size) is unsafe to show.
func unsafeRune(r rune, size int) bool {
	switch {
	case r == utf8.RuneError && size <= 1:
		return true // invalid UTF-8 byte
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

func describe(s string, i int, r rune, size int) string {
	if r < 0x80 || (r == utf8.RuneError && size <= 1) {
		return fmt.Sprintf(`\x%02x`, s[i])
	}
	return fmt.Sprintf("U+%04X", r)
}

// FirstUnsafe returns the first unsafe byte or rune of s; ok is false when s
// is entirely safe.
func FirstUnsafe(s string) (f Finding, found bool) {
	line := 1
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if unsafeRune(r, size) {
			return Finding{Offset: i, Line: line, Desc: describe(s, i, r, size)}, true
		}
		if r == '\n' {
			line++
		}
		i += size
	}
	return Finding{}, false
}

func escape(s string, keepNewline bool) string {
	if _, bad := FirstUnsafe(s); !bad && (keepNewline || !strings.Contains(s, "\n")) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n' && !keepNewline:
			b.WriteString(`\x0a`)
		case unsafeRune(r, size) && (r < 0x80 || size <= 1):
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unsafeRune(r, size):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Escape returns s on one line with every unsafe byte or rune, and "\n",
// written as a visible escape (`\x1b`, `\x0a`, `\xff`, `‮`).
func Escape(s string) string { return escape(s, false) }

// EscapeText is Escape but keeps "\n" (and "\t") as they are.
func EscapeText(s string) string { return escape(s, true) }
