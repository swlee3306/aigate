package broker

import "testing"

func TestLooksLikeSecretShapes(t *testing.T) {
	yes := []string{
		"-----BEGIN PRIVATE KEY-----", "-----BEGIN EC PRIVATE KEY-----", "AKIAABCDEFGHIJKLMNOP",
		`api_key = "abcdef0123456789"`, `"secret": "abcdef0123456789abcd"`, `PASSWORD='longlonglonglong1'`,
		"glpat-abcdefghijklmnopqrstuvwxyz",
	}
	no := []string{
		"-----BEGIN CERTIFICATE-----", `token := r.Header.Get("X-Token")`, `api_key = os.Getenv("API_KEY")`,
		`password = "short"`, "AKIA is a prefix", "plain text",
	}
	for _, s := range yes {
		if !LooksLikeSecret(s) {
			t.Errorf("not flagged: %q", s)
		}
	}
	for _, s := range no {
		if LooksLikeSecret(s) {
			t.Errorf("flagged: %q", s)
		}
	}
}
