package signing

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func goldenMessage() Message {
	return Message{
		Kind:   KindProtectedModified,
		Repo:   "0123456789ab",
		Task:   42,
		Base:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Hash:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Issued: time.Date(2026, 11, 9, 12, 34, 56, 0, time.UTC),
	}
}

const goldenText = "aigate-approval-v1\n" +
	"kind: protected_modified\n" +
	"repo: 0123456789ab\n" +
	"task: 42\n" +
	"base: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
	"hash: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" +
	"issued: 2026-11-09T12:34:56Z\n"

func TestMessageTextGolden(t *testing.T) {
	got := goldenMessage().Text()
	if got != goldenText {
		t.Fatalf("text mismatch:\n got %q\nwant %q", got, goldenText)
	}
}

func TestDigestGolden(t *testing.T) {
	d := goldenMessage().Digest()
	const want = "be7299ee50ed90eaa665df97a672381a3c06c624415cac2af7aaf0cac382ef96"
	if got := hex.EncodeToString(d[:]); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}

func TestNamespaceConstant(t *testing.T) {
	if NamespaceApproval != "aigate-approval" {
		t.Fatal(NamespaceApproval)
	}
}

func TestParseRoundTrip(t *testing.T) {
	m64 := goldenMessage()
	m64.Kind = KindPublishOutsideAllowlist
	m64.Base = strings.Repeat("b", 64)
	m64.Hash = strings.Repeat("c", 64)
	m64.Task = 1
	for _, m := range []Message{goldenMessage(), m64} {
		text := m.Text()
		got, err := ParseMessage(text)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got != m {
			t.Fatalf("round trip: got %+v want %+v", got, m)
		}
	}
}

func TestParseRejects(t *testing.T) {
	lines := strings.SplitAfter(goldenText, "\n") // last element is ""
	lines = lines[:len(lines)-1]
	join := func(l []string) string { return strings.Join(l, "") }
	replace := func(i int, s string) string {
		c := append([]string(nil), lines...)
		c[i] = s
		return join(c)
	}
	swap := func(i, j int) string {
		c := append([]string(nil), lines...)
		c[i], c[j] = c[j], c[i]
		return join(c)
	}
	drop := func(i int) string {
		c := append([]string(nil), lines[:i]...)
		return join(append(c, lines[i+1:]...))
	}
	cases := map[string]string{
		"empty":             "",
		"crlf":              strings.ReplaceAll(goldenText, "\n", "\r\n"),
		"no final newline":  strings.TrimSuffix(goldenText, "\n"),
		"extra blank line":  goldenText + "\n",
		"extra line":        goldenText + "extra: 1\n",
		"leading space":     " " + goldenText,
		"leading newline":   "\n" + goldenText,
		"trailing space":    replace(1, "kind: protected_modified \n"),
		"tab after colon":   replace(1, "kind:\tprotected_modified\n"),
		"double space":      replace(1, "kind:  protected_modified\n"),
		"no space":          replace(1, "kind:protected_modified\n"),
		"wrong magic":       replace(0, "aigate-approval-v2\n"),
		"magic trailing":    replace(0, "aigate-approval-v1 \n"),
		"missing kind":      drop(1),
		"missing issued":    drop(6),
		"reordered":         swap(2, 3),
		"reordered head":    swap(0, 1),
		"unknown kind":      replace(1, "kind: other\n"),
		"empty kind":        replace(1, "kind: \n"),
		"uppercase kind":    replace(1, "kind: Protected_Modified\n"),
		"repo too short":    replace(2, "repo: 0123456789a\n"),
		"repo too long":     replace(2, "repo: 0123456789abc\n"),
		"repo uppercase":    replace(2, "repo: 0123456789AB\n"),
		"repo non hex":      replace(2, "repo: 0123456789zz\n"),
		"task zero":         replace(3, "task: 0\n"),
		"task negative":     replace(3, "task: -1\n"),
		"task leading zero": replace(3, "task: 042\n"),
		"task plus":         replace(3, "task: +42\n"),
		"task non numeric":  replace(3, "task: x\n"),
		"task overflow":     replace(3, "task: 99999999999999999999\n"),
		"base wrong length": replace(4, "base: "+strings.Repeat("a", 41)+"\n"),
		"base uppercase":    replace(4, "base: "+strings.Repeat("A", 40)+"\n"),
		"base empty":        replace(4, "base: \n"),
		"hash empty":        replace(5, "hash: \n"),
		"hash wrong length": replace(5, "hash: "+strings.Repeat("a", 63)+"\n"),
		"hash 65 chars":     replace(5, "hash: "+strings.Repeat("a", 65)+"\n"),
		"hash 16 chars":     replace(5, "hash: 0123456789abcdef\n"), // pre-2026-11-09 diff hash
		"hash 32 chars":     replace(5, "hash: "+strings.Repeat("a", 32)+"\n"),
		"hash uppercase":    replace(5, "hash: "+strings.Repeat("0123456789ABCDEF", 4)+"\n"),
		"time offset":       replace(6, "issued: 2026-11-09T21:34:56+09:00\n"),
		"time zero offset":  replace(6, "issued: 2026-11-09T12:34:56+00:00\n"),
		"time fraction":     replace(6, "issued: 2026-11-09T12:34:56.5Z\n"),
		"time lowercase":    replace(6, "issued: 2026-11-09t12:34:56z\n"),
		"time date only":    replace(6, "issued: 2026-11-09\n"),
		"time empty":        replace(6, "issued: \n"),
		"time invalid date": replace(6, "issued: 2026-13-09T12:34:56Z\n"),
		"nul byte":          replace(2, "repo: 0123456789a\x00\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if m, err := ParseMessage(in); err == nil {
				t.Fatalf("accepted %q as %+v", in, m)
			}
		})
	}
}

// TestHashMustBeFull64HexForEveryKind: since 2026-11-09 the diff hash is the
// full SHA-256, so every kind's hash is exactly 64 lowercase hex. A 16-hex
// hash (the old diff hash format) is refused by Validate and ParseMessage
// for every kind, so nobody can sign or replay a downgraded short hash.
func TestHashMustBeFull64HexForEveryKind(t *testing.T) {
	for _, kind := range []string{KindProtectedModified, KindPublishOutsideAllowlist} {
		m := goldenMessage()
		m.Kind = kind
		if err := m.Validate(); err != nil {
			t.Fatalf("%s: 64-hex hash refused: %v", kind, err)
		}
		if _, err := ParseMessage(m.Text()); err != nil {
			t.Fatalf("%s: 64-hex message does not parse: %v", kind, err)
		}
		text := m.Text()
		m.Hash = "0123456789abcdef"
		err := m.Validate()
		if err == nil || !strings.Contains(err.Error(), "64 lowercase hex") {
			t.Fatalf("%s: 16-hex hash accepted or wrong error: %v", kind, err)
		}
		short := strings.Replace(text, "hash: "+goldenMessage().Hash+"\n", "hash: 0123456789abcdef\n", 1)
		if short == text {
			t.Fatal("test bug: hash line not replaced")
		}
		if _, err := ParseMessage(short); err == nil {
			t.Fatalf("%s: ParseMessage accepted a 16-hex hash", kind)
		}
	}
}

func TestTextRejectsInvalidMessage(t *testing.T) {
	mut := map[string]func(*Message){
		"kind":         func(m *Message) { m.Kind = "nope" },
		"repo":         func(m *Message) { m.Repo = "ABC" },
		"task":         func(m *Message) { m.Task = 0 },
		"base":         func(m *Message) { m.Base = "abc" },
		"hash":         func(m *Message) { m.Hash = "" },
		"hash newline": func(m *Message) { m.Hash = strings.Repeat("a", 63) + "\n" },
		"hash 16":      func(m *Message) { m.Hash = "0123456789abcdef" },
		"zero time":    func(m *Message) { m.Issued = time.Time{} },
		"non-utc":      func(m *Message) { m.Issued = m.Issued.In(time.FixedZone("KST", 9*3600)) },
		"fraction":     func(m *Message) { m.Issued = m.Issued.Add(time.Millisecond) },
		"injection":    func(m *Message) { m.Repo = "0123456789ab\nhash: " + strings.Repeat("a", 64) },
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			m := goldenMessage()
			f(&m)
			if txt := m.Text(); txt != "" {
				t.Fatalf("Text() = %q; want empty text for invalid message", txt)
			}
			if m.Digest() != ([32]byte{}) {
				t.Fatal("Digest of invalid message must be the zero value")
			}
			if m.Validate() == nil {
				t.Fatal("Validate accepted invalid message")
			}
		})
	}
}

// Every single-byte mutation of a valid text must either be rejected or
// parse back to a message whose canonical text is exactly the mutated input
// (e.g. deleting a digit of the task number is a different valid message).
func TestParseSingleByteMutations(t *testing.T) {
	for i := 0; i < len(goldenText); i++ {
		for _, b := range []byte{0, '\n', '\r', ' ', '\t', 'A', 'z', '0', ':', '-'} {
			if goldenText[i] == b {
				continue
			}
			in := goldenText[:i] + string([]byte{b}) + goldenText[i+1:]
			m, err := ParseMessage(in)
			if err != nil {
				continue
			}
			if m.Text() != in {
				t.Fatalf("mutation at %d (%q) parsed but is not canonical: %q", i, b, in)
			}
		}
		// deletion
		in := goldenText[:i] + goldenText[i+1:]
		if m, err := ParseMessage(in); err == nil && m.Text() != in {
			t.Fatalf("deletion at %d parsed but is not canonical: %q", i, in)
		}
	}
}
