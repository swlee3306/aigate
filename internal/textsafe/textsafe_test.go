package textsafe

import "testing"

func TestFirstUnsafe(t *testing.T) {
	cases := []struct {
		name, in string
		ok       bool
		line     int
		desc     string
	}{
		{"clean ascii", "a\tb\nc\n", true, 0, ""},
		{"clean korean", "+    assert 합계 == 3\t# 주석\n", true, 0, ""},
		{"empty", "", true, 0, ""},
		{"ESC cursor up", "ok\n+x\x1b[1A\x1b[2K\n", false, 2, `\x1b`},
		{"CR", "+a\r-b\n", false, 1, `\x0d`},
		{"NUL", "a\n\n+\x00\n", false, 3, `\x00`},
		{"DEL", "\x7f", false, 1, `\x7f`},
		{"bell", "x\a", false, 1, `\x07`},
		{"invalid utf8", "ok\n+\xff\xfe\n", false, 2, `\xff`},
		{"truncated utf8", "+\xe2\x80", false, 1, `\xe2`},
		{"C1 CSI", "+\u009b31m\n", false, 1, "U+009B"},
		{"C1 NEL", "\u0085", false, 1, "U+0085"},
		{"bidi RLO", "+a\u202eb\n", false, 1, "U+202E"},
		{"bidi LRE", "\u202a", false, 1, "U+202A"},
		{"bidi isolate", "x\n\n\n\u2066", false, 4, "U+2066"},
		{"bidi PDI", "\u2069", false, 1, "U+2069"},
		{"U+2065 not bidi", "\u2065", true, 0, ""},
		{"U+00A0 allowed", "\u00a0é", true, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, bad := FirstUnsafe(c.in)
			if bad == c.ok {
				t.Fatalf("FirstUnsafe(%q) bad=%v want ok=%v (%+v)", c.in, bad, c.ok, f)
			}
			if bad && (f.Line != c.line || f.Desc != c.desc) {
				t.Fatalf("FirstUnsafe(%q) = line %d %q, want line %d %q", c.in, f.Line, f.Desc, c.line, c.desc)
			}
		})
	}
}

func TestEscape(t *testing.T) {
	cases := map[string]string{
		"plain 한글\ttab": "plain 한글\ttab",
		"a\x1b[2Kb":     `a\x1b[2Kb`,
		"line1\nline2":  `line1\x0aline2`,
		"cr\r":          `cr\x0d`,
		"bad\xff":       `bad\xff`,
		"rlo\u202eend":  `rlo\u202eend`,
		"c1\u009bx":     `c1\u009bx`,
	}
	for in, want := range cases {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeKeepsNewlineWhenAsked(t *testing.T) {
	if got := EscapeText("a\nb\x1bc"); got != `a`+"\n"+`b\x1bc` {
		t.Errorf("EscapeText = %q", got)
	}
}
