package config

import (
	"strings"
	"testing"
	"time"
)

func TestRSSDeploymentConfig(t *testing.T) {
	// Load may report missing unrelated mandatory settings in this test;
	// inspect only RSS settings and RSS-specific diagnostics.
	for _, key := range []string{"RSS_ENABLED", "RSS_POLL_INTERVAL", "RSS_HTTP_TIMEOUT", "RSS_PROXY_URL"} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if !cfg.RSSEnabled || cfg.RSSPollInterval != 5*time.Minute || cfg.RSSHTTPTimeout != 20*time.Second {
		t.Fatal(cfg.RSSEnabled, cfg.RSSPollInterval, cfg.RSSHTTPTimeout)
	}
	if err != nil && strings.Contains(err.Error(), "RSS_") {
		t.Fatal(err)
	}
	t.Setenv("RSS_ENABLED", "false")
	t.Setenv("RSS_POLL_INTERVAL", "1h")
	t.Setenv("RSS_HTTP_TIMEOUT", "5s")
	t.Setenv("RSS_PROXY_URL", "socks5h://alice:secret@localhost:1080")
	cfg, err = Load()
	if cfg.RSSEnabled || cfg.RSSPollInterval != time.Hour || cfg.RSSHTTPTimeout != 5*time.Second ||
		(err != nil && strings.Contains(err.Error(), "RSS_")) {
		t.Fatal(cfg.RSSEnabled, cfg.RSSPollInterval, cfg.RSSHTTPTimeout, err)
	}
	for _, pair := range [][2]string{{"RSS_ENABLED", "bad"}, {"RSS_POLL_INTERVAL", "30s"}, {"RSS_POLL_INTERVAL", "25h"}, {"RSS_HTTP_TIMEOUT", "0s"}, {"RSS_HTTP_TIMEOUT", "3m"}, {"RSS_PROXY_URL", "ftp://user:secret@proxy"}} {
		t.Run(pair[0]+pair[1], func(t *testing.T) {
			t.Setenv(pair[0], pair[1])
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), pair[0]) || strings.Contains(err.Error(), "secret") {
				t.Fatal(pair[0], err)
			}
		})
	}
}
