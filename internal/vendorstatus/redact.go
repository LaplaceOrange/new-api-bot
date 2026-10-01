package vendorstatus

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var sensitiveURL = regexp.MustCompile(`(?i)\b(?:https?|socks5h?|proxy)://[^\s"'<>]+`)
var sensitiveUserInfo = regexp.MustCompile(`[A-Za-z0-9_.%+-]+:[^\s@]+@[^\s"'<>]+`)
var sensitiveField = regexp.MustCompile(`(?i)\b(api[_ -]?key|access[_ -]?token|app[_ -]?secret|password|passwd|token|secret|username|authorization)\b(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}\]]+)`)
var sensitiveBearer = regexp.MustCompile(`(?i)\b(bearer|qqbot|basic)\s+[A-Za-z0-9+/=_\-.]+`)
var sensitiveAPIKey = regexp.MustCompile(`\b(?:sk-|AIza)[A-Za-z0-9_-]{8,}`)

type redactedError struct {
	cause   error
	message string
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// RedactError preserves cancellation/deadline identity while removing the
// exact credentials belonging to this worker's configuration snapshot.
func RedactError(err error, cfg Config) error {
	if err == nil {
		return nil
	}
	return &redactedError{cause: err, message: Redact(err.Error(), cfg)}
}

// Redact removes configured secrets, URL credentials/query tokens, and common
// credential fields before either public messages or diagnostic logs.
func Redact(text string, cfg Config, extra ...string) string {
	secrets := append([]string{cfg.Translation.APIKey, cfg.Proxy}, extra...)
	if proxy, err := url.Parse(cfg.Proxy); err == nil && proxy != nil && proxy.User != nil {
		password, _ := proxy.User.Password()
		secrets = append(secrets, proxy.User.Username(), password)
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, encoded := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			text = strings.ReplaceAll(text, encoded, "[已脱敏]")
		}
	}
	text = sensitiveURL.ReplaceAllString(text, "[URL已脱敏]")
	text = sensitiveUserInfo.ReplaceAllString(text, "[认证信息已脱敏]")
	text = sensitiveBearer.ReplaceAllString(text, "[认证信息已脱敏]")
	text = sensitiveField.ReplaceAllString(text, "$1$2[已脱敏]")
	text = sensitiveAPIKey.ReplaceAllString(text, "[密钥已脱敏]")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}
