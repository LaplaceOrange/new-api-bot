package vendorstatus

import (
	"strings"
	"testing"
)

func TestProxyURLValidation(t *testing.T) {
	for _, proxy := range []string{"", "socks5://localhost:1080", "socks5h://username:password@host:1080", "socks5h://u:p%40ss%3Aword@[::1]:1080"} {
		if err := ValidateProxy(proxy); err != nil {
			t.Fatal("valid proxy rejected", err)
		}
	}
	for _, proxy := range []string{"http://host:1080", "socks5://host", "socks5h://host:0", "socks5://host:65536", "socks5://u:secret@host:bad", "socks5h://host:1080/path", "socks5h://host:1080?token=secret"} {
		if err := ValidateProxy(proxy); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid proxy accepted or credentials leaked")
		}
	}
}
