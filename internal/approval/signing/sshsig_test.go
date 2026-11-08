package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const nsLogin = "aigate-login"

var testMessage = []byte("aigate-approval-v1\n{\"repo\":\"r\"}\n")

// sshKeygen returns the path of ssh-keygen or skips the test.
func sshKeygen(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen이 없습니다")
	}
	return p
}

// genKey creates a key with ssh-keygen and returns (private key path, authorized_keys line).
func genKey(t *testing.T, typ string) (string, string) {
	t.Helper()
	bin := sshKeygen(t)
	key := filepath.Join(t.TempDir(), "id")
	args := []string{"-q", "-t", typ, "-N", "", "-C", "test", "-f", key}
	if typ == "rsa" {
		args = append(args, "-b", "2048")
	}
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return key, strings.TrimSpace(string(pub))
}

// sshSign signs msg with ssh-keygen -Y sign and returns the armored signature.
func sshSign(t *testing.T, key, ns string, msg []byte, extra ...string) string {
	t.Helper()
	bin := sshKeygen(t)
	args := append([]string{"-Y", "sign", "-f", key, "-n", ns}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(msg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v: %s", err, stderr.String())
	}
	return stdout.String()
}

// --- hand-built signatures ---

func wireString(b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

type sigFields struct {
	magic     string
	version   uint32
	pubWire   []byte
	namespace string
	reserved  string
	hashAlg   string
	sigFormat string
	sigBlob   []byte
	sigExtra  []byte // appended inside the signature field
	trailing  []byte // appended after the last field
}

func (f sigFields) blob() []byte {
	b := []byte(f.magic)
	b = binary.BigEndian.AppendUint32(b, f.version)
	b = append(b, wireString(f.pubWire)...)
	b = append(b, wireString([]byte(f.namespace))...)
	b = append(b, wireString([]byte(f.reserved))...)
	b = append(b, wireString([]byte(f.hashAlg))...)
	sig := append(wireString([]byte(f.sigFormat)), wireString(f.sigBlob)...)
	sig = append(sig, f.sigExtra...)
	b = append(b, wireString(sig)...)
	return append(b, f.trailing...)
}

func armor(blob []byte) string {
	enc := base64.StdEncoding.EncodeToString(blob)
	var sb strings.Builder
	sb.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		sb.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	sb.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return sb.String()
}

type goKey struct {
	signer ssh.Signer
	line   string
}

func newGoKey(t *testing.T) goKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return goKey{s, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))}
}

func signedData(ns, reserved, hashAlg string, msg []byte) []byte {
	var h []byte
	switch hashAlg {
	case "sha256":
		s := sha256.Sum256(msg)
		h = s[:]
	default:
		s := sha512.Sum512(msg)
		h = s[:]
	}
	d := []byte("SSHSIG")
	d = append(d, wireString([]byte(ns))...)
	d = append(d, wireString([]byte(reserved))...)
	d = append(d, wireString([]byte(hashAlg))...)
	return append(d, wireString(h)...)
}

// validFields returns fields of a correct signature by k over msg; tests mutate them.
func validFields(t *testing.T, k goKey, ns, hashAlg string, msg []byte) sigFields {
	t.Helper()
	sig, err := k.signer.Sign(rand.Reader, signedData(ns, "", hashAlg, msg))
	if err != nil {
		t.Fatal(err)
	}
	return sigFields{
		magic: "SSHSIG", version: 1, pubWire: k.signer.PublicKey().Marshal(),
		namespace: ns, hashAlg: hashAlg, sigFormat: sig.Format, sigBlob: sig.Blob,
	}
}

// --- tests ---

func TestVerifyOK(t *testing.T) {
	key, line := genKey(t, "ed25519")
	sig := sshSign(t, key, NamespaceApproval, testMessage)
	if err := VerifySSHSig(line, NamespaceApproval, testMessage, sig); err != nil {
		t.Fatalf("sha512 (default): %v", err)
	}
	sig256 := sshSign(t, key, NamespaceApproval, testMessage, "-O", "hashalg=sha256")
	if err := VerifySSHSig(line, NamespaceApproval, testMessage, sig256); err != nil {
		t.Fatalf("sha256: %v", err)
	}
	// Hand-built signatures (both hash algorithms) must verify too, so the
	// negative cases built from them differ only in the mutated field.
	k := newGoKey(t)
	for _, alg := range []string{"sha512", "sha256"} {
		if err := VerifySSHSig(k.line, NamespaceApproval, testMessage, armor(validFields(t, k, NamespaceApproval, alg, testMessage).blob())); err != nil {
			t.Fatalf("hand-built %s: %v", alg, err)
		}
	}
}

func TestWrongNamespace(t *testing.T) {
	key, line := genKey(t, "ed25519")
	sig := sshSign(t, key, nsLogin, testMessage)
	if err := VerifySSHSig(line, NamespaceApproval, testMessage, sig); err == nil {
		t.Fatal("login signature accepted as approval")
	}
	sig = sshSign(t, key, NamespaceApproval, testMessage)
	if err := VerifySSHSig(line, nsLogin, testMessage, sig); err == nil {
		t.Fatal("approval signature accepted as login")
	}
	for _, ns := range []string{"", "aigate-approva", "aigate-approval ", "AIGATE-APPROVAL"} {
		if err := VerifySSHSig(line, ns, testMessage, sig); err == nil {
			t.Fatalf("namespace %q accepted", ns)
		}
	}
}

func TestTamperedMessage(t *testing.T) {
	key, line := genKey(t, "ed25519")
	sig := sshSign(t, key, NamespaceApproval, testMessage)
	for _, m := range [][]byte{nil, testMessage[:len(testMessage)-1], append(bytes.Clone(testMessage), 'x')} {
		if err := VerifySSHSig(line, NamespaceApproval, m, sig); err == nil {
			t.Fatalf("tampered message %q accepted", m)
		}
	}
	m := bytes.Clone(testMessage)
	m[3] ^= 1
	if err := VerifySSHSig(line, NamespaceApproval, m, sig); err == nil {
		t.Fatal("bit-flipped message accepted")
	}
}

func TestWrongKey(t *testing.T) {
	key, _ := genKey(t, "ed25519")
	_, other := genKey(t, "ed25519")
	sig := sshSign(t, key, NamespaceApproval, testMessage)
	if err := VerifySSHSig(other, NamespaceApproval, testMessage, sig); err == nil {
		t.Fatal("signature verified under a different key")
	}
}

// TestEmbeddedKeyMismatch: the public key inside the blob is not covered by the
// signature, so a valid signature by the registry key with someone else's key
// embedded must still be refused.
func TestEmbeddedKeyMismatch(t *testing.T) {
	a, b := newGoKey(t), newGoKey(t)
	f := validFields(t, a, NamespaceApproval, "sha512", testMessage)
	f.pubWire = b.signer.PublicKey().Marshal()
	if err := VerifySSHSig(a.line, NamespaceApproval, testMessage, armor(f.blob())); err == nil {
		t.Fatal("embedded key differing from registry key accepted")
	}
}

func TestRSAKeyRefused(t *testing.T) {
	rsaKey, rsaLine := genKey(t, "rsa")
	rsaSig := sshSign(t, rsaKey, NamespaceApproval, testMessage)
	// A genuine rsa signature must not verify even against its own rsa key.
	if err := VerifySSHSig(rsaLine, NamespaceApproval, testMessage, rsaSig); err == nil {
		t.Fatal("rsa key accepted")
	}
	// And an rsa signature must not verify against an ed25519 registry key.
	_, edLine := genKey(t, "ed25519")
	if err := VerifySSHSig(edLine, NamespaceApproval, testMessage, rsaSig); err == nil {
		t.Fatal("rsa signature accepted for ed25519 key")
	}
}

func TestAuthorizedKeyForms(t *testing.T) {
	k := newGoKey(t)
	sig := armor(validFields(t, k, NamespaceApproval, "sha512", testMessage).blob())
	for name, line := range map[string]string{
		"empty":         "",
		"trailing LF":   k.line + "\n",
		"CRLF":          k.line + "\r\n",
		"two lines":     k.line + "\n" + k.line,
		"options":       `command="x" ` + k.line,
		"leading space": " " + k.line,
		"garbage":       "not a key",
	} {
		if err := VerifySSHSig(line, NamespaceApproval, testMessage, sig); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Certificates are not ed25519 keys.
	cert := &ssh.Certificate{Key: k.signer.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, k.signer); err != nil {
		t.Fatal(err)
	}
	certLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
	if err := VerifySSHSig(certLine, NamespaceApproval, testMessage, sig); err == nil {
		t.Error("certificate accepted")
	}
}

func TestBlobFieldChecks(t *testing.T) {
	k := newGoKey(t)
	resign := func(f sigFields) sigFields {
		// Sign over exactly the (possibly mutated) fields so only the field check can reject.
		s, err := k.signer.Sign(rand.Reader, signedData(f.namespace, f.reserved, f.hashAlg, testMessage))
		if err != nil {
			t.Fatal(err)
		}
		f.sigBlob = s.Blob
		return f
	}
	cases := map[string]func(*sigFields){
		"nonempty reserved": func(f *sigFields) { f.reserved = "x" },
		"version 0":         func(f *sigFields) { f.version = 0 },
		"version 2":         func(f *sigFields) { f.version = 2 },
		"hash sha1":         func(f *sigFields) { f.hashAlg = "sha1" },
		"hash SHA512":       func(f *sigFields) { f.hashAlg = "SHA512" },
		"hash empty":        func(f *sigFields) { f.hashAlg = "" },
		"wrong magic":       func(f *sigFields) { f.magic = "SSHSIH" },
		"format rsa":        func(f *sigFields) { f.sigFormat = "rsa-sha2-512" },
		"format sk":         func(f *sigFields) { f.sigFormat = "sk-ssh-ed25519@openssh.com" },
		"sig field extra":   func(f *sigFields) { f.sigExtra = []byte{0} },
		"trailing bytes":    func(f *sigFields) { f.trailing = []byte{0, 0, 0, 0} },
		"trailing one byte": func(f *sigFields) { f.trailing = []byte{0} },
		"short sig blob":    func(f *sigFields) { f.sigBlob = f.sigBlob[:63] },
	}
	for name, mutate := range cases {
		f := validFields(t, k, NamespaceApproval, "sha512", testMessage)
		mutate(&f)
		if f.sigFormat == "ssh-ed25519" && name != "short sig blob" {
			f = resign(f)
		}
		if err := VerifySSHSig(k.line, NamespaceApproval, testMessage, armor(f.blob())); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMalformedArmorNoPanic(t *testing.T) {
	// Pick a key whose (deterministic ed25519) signature blob encodes differently
	// in the URL and standard alphabets, so the "url base64" case is a real
	// variant and not the valid armor itself.
	var k goKey
	for i := 0; ; i++ {
		k = newGoKey(t)
		blob := validFields(t, k, NamespaceApproval, "sha512", testMessage).blob()
		if base64.URLEncoding.EncodeToString(blob) != base64.StdEncoding.EncodeToString(blob) {
			break
		}
		if i > 100 {
			t.Fatal("could not find a blob with + or / in its base64")
		}
	}
	valid := armor(validFields(t, k, NamespaceApproval, "sha512", testMessage).blob())
	if err := VerifySSHSig(k.line, NamespaceApproval, testMessage, valid); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	begin, end := "-----BEGIN SSH SIGNATURE-----\n", "\n-----END SSH SIGNATURE-----\n"
	wrap := func(blob []byte) string { return begin + base64.StdEncoding.EncodeToString(blob) + end }
	huge := []byte("SSHSIG\x00\x00\x00\x01\xff\xff\xff\xff")
	neg := []byte("SSHSIG\x00\x00\x00\x01\x80\x00\x00\x00")
	cases := map[string]string{
		"empty":            "",
		"only begin":       begin,
		"empty body":       begin + "-----END SSH SIGNATURE-----\n",
		"blank body line":  begin + "\n" + end[1:],
		"truncated base64": valid[:len(begin)+10] + end,
		"bad base64 char":  begin + "U1NI*0lH" + end,
		"wrong magic":      wrap([]byte("SSHSIH\x00\x00\x00\x01")),
		"magic only":       wrap([]byte("SSHSIG")),
		"short magic":      wrap([]byte("SSH")),
		"huge length":      wrap(huge),
		"negative length":  wrap(neg),
		"huge len + data":  wrap(append(bytes.Clone(huge), make([]byte, 64)...)),
		"CRLF armor":       strings.ReplaceAll(valid, "\n", "\r\n"),
		"CR in body":       begin + strings.Replace(valid[len(begin):], "\n", "\r\n", 1),
		"CR before end":    strings.Replace(valid, "\n-----END", "\r\n-----END", 1),
		"CR after end":     strings.TrimSuffix(valid, "\n") + "\r\n",
		"text before":      "x" + valid,
		"newline before":   "\n" + valid,
		"text after":       valid + "x",
		"two trailing LF":  valid + "\n",
		"two armors":       valid + valid,
		"space in body":    strings.Replace(valid, begin, begin+" ", 1),
		"tab in body":      strings.Replace(valid, begin, begin+"\t", 1),
		"oversize":         begin + strings.Repeat(strings.Repeat("A", 70)+"\n", 120) + "AAAA" + end,
		"lowercase header": strings.Replace(valid, "BEGIN", "begin", 1),
		"no end":           strings.TrimSuffix(valid, "-----END SSH SIGNATURE-----\n"),
		"url base64":       begin + base64.URLEncoding.EncodeToString(validFields(t, k, NamespaceApproval, "sha512", testMessage).blob()) + end,
	}
	for name, armored := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", name, r)
				}
			}()
			if err := VerifySSHSig(k.line, NamespaceApproval, testMessage, armored); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}()
	}
	// The trailing newline after END is optional (callers may trim it).
	if err := VerifySSHSig(k.line, NamespaceApproval, testMessage, strings.TrimSuffix(valid, "\n")); err != nil {
		t.Errorf("no final newline: %v", err)
	}

	// Truncate a real ssh-keygen signature at every offset and flip every byte.
	// With strict base64 (non-canonical padding bits rejected), CR rejected and
	// exact armor lines, no single-byte change may still verify.
	key, line := genKey(t, "ed25519")
	sig := sshSign(t, key, NamespaceApproval, testMessage)
	check := func(desc string, s string, wantErr bool) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: panic: %v", desc, r)
			}
		}()
		err := VerifySSHSig(line, NamespaceApproval, testMessage, s)
		if wantErr && err == nil {
			t.Fatalf("%s: accepted", desc)
		}
		if !wantErr && err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
	}
	for i := 0; i < len(sig); i++ {
		// Dropping only the final newline is the one truncation that stays valid.
		check("truncate", sig[:i], i != len(sig)-1)
		for _, x := range []byte{0x01, 0x20, 0x80} {
			b := []byte(sig)
			b[i] ^= x
			check("flip", string(b), true)
		}
	}
	// Same at the decoded-blob level: truncate/flip the raw blob and re-armor.
	blob := validFields(t, k, NamespaceApproval, "sha512", testMessage).blob()
	for i := 0; i < len(blob); i++ {
		check("blob truncate", armor(blob[:i]), true)
		b := bytes.Clone(blob)
		b[i] ^= 0x01
		check("blob flip", armor(b), true)
	}
}
