package vendorstatus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRedactedErrorPreservesDeadlineIdentity(t *testing.T) {
	err := RedactError(context.DeadlineExceeded, DefaultConfig())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if RedactError(nil, DefaultConfig()) != nil {
		t.Fatal("nil error changed")
	}
}

func TestRedactConfiguredSecretsURLsHeadersAndBareCredentials(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Proxy = "socks5h://proxyuser:p%40ss%3Aword@proxy.test:1080"
	cfg.Translation.APIKey = "translation-secret"
	message := `RuntimeError: SOCKS auth failed: p@ss:word p%40ss%3Aword proxyuser
https://api.test/status?token=unconfigured-secret
socks5://other:other-password@other-proxy.test:1080
Authorization: Bearer unknown-token
api_key="literal-key" password='literal-password'
user:bare-password@host:80 sk-unknownapikey123456
translation-secret admin-private-secret HTTP 403`
	got := Redact(message, cfg, "admin-private-secret")
	for _, secret := range []string{
		"p@ss:word", "p%40ss%3Aword", "proxyuser", "unconfigured-secret",
		"other-password", "unknown-token", "literal-key", "literal-password",
		"bare-password", "sk-unknownapikey123456", "translation-secret", "admin-private-secret",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret not redacted: %s", secret)
		}
	}
	if !strings.Contains(got, "RuntimeError") || !strings.Contains(got, "HTTP 403") {
		t.Fatal("diagnostic context lost", got)
	}
}
