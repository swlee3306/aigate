package broker

import "testing"

func TestLooksLikeSecretShapes(t *testing.T) {
	yes := []string{
		"-----BEGIN PRIVATE KEY-----", "-----BEGIN EC PRIVATE KEY-----", "AKIAABCDEFGHIJKLMNOP",
		`api_key = "abcdef0123456789"`, `"secret": "abcdef0123456789abcd"`, `PASSWORD='longlonglonglong1'`,
		"glpat-abcdefghijklmnopqrstuvwxyz",
		"API_KEY=abcd1234efgh5678", "token: abcdef0123456789xyz", "ENV PASSWORD=Xk29fjQ8sLmP4aZq",
		"MY_API_KEY=Xk29fjQ8sLmP4aZq",
	}
	no := []string{
		"-----BEGIN CERTIFICATE-----", `token := r.Header.Get("X-Token")`, `api_key = os.Getenv("API_KEY")`,
		`password = "short"`,
		"tokenizer = AutoTokenizer.from_pretrained(m)", "token = request.headers.authorization",
		"secretName: my-app-secret-name-prod", "tokenFile: /var/run/secrets/foo",
		"secret: process.env.SECRET_KEY_BASE", "passwordHash = bcrypt.GenerateFromPassword(p)", "AKIA is a prefix", "plain text",
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
