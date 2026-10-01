package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVendorProgressModeEnvironment(t *testing.T) {
	t.Setenv("VENDOR_STATUS_PROGRESS_MODE", "simple")
	cfg, err := loadVendorStatusConfig("UTC")
	if err != nil || cfg.ProgressMode != "simple" {
		t.Fatal(cfg.ProgressMode, err)
	}
	t.Setenv("VENDOR_STATUS_PROGRESS_MODE", "off")
	cfg, err = loadVendorStatusConfig("UTC")
	if err != nil || cfg.ProgressMode != "off" {
		t.Fatal(cfg.ProgressMode, err)
	}
	t.Setenv("VENDOR_STATUS_PROGRESS_MODE", "invalid")
	if _, err := loadVendorStatusConfig("UTC"); err == nil {
		t.Fatal("invalid environment mode accepted")
	}
}

func TestVendorConfigDefaultsAndEnvironmentOverrides(t *testing.T) {
	cfg, err := loadVendorStatusConfig("UTC")
	if err != nil || !cfg.Enabled || cfg.Timezone != "UTC" || cfg.PollIntervalSeconds != 300 {
		t.Fatal(cfg, err)
	}
	t.Setenv("VENDOR_STATUS_ENABLED", "false")
	t.Setenv("VENDOR_STATUS_GROUP_WHITELIST", " one, two,one, ")
	t.Setenv("VENDOR_STATUS_POLL_INTERVAL", "2m")
	t.Setenv("VENDOR_STATUS_DISPLAY_LANGUAGE", "en-US")
	t.Setenv("VENDOR_STATUS_HISTORY_LOOKBACK_HOURS", "0")
	cfg, err = loadVendorStatusConfig("UTC")
	if err != nil || cfg.Enabled || len(cfg.GroupWhitelist) != 2 || cfg.PollIntervalSeconds != 120 || cfg.DisplayLanguage != "en-US" || cfg.HistoryLookbackHours != 0 {
		t.Fatal(cfg, err)
	}
}

func TestVendorConfigJSONMigrationAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte(`{"enabled":false,"card_theme":"terminal","platform_type":"qq_official","platform_id":"old","translation_provider_id":"old","sources":{"openai":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VENDOR_STATUS_CONFIG_PATH", path)
	cfg, err := loadVendorStatusConfig("UTC")
	if err != nil || cfg.Enabled || cfg.CardTheme != "terminal" || cfg.Sources["openai"] {
		t.Fatal(cfg, err)
	}
	t.Setenv("VENDOR_STATUS_CARD_THEME", "paper")
	cfg, err = loadVendorStatusConfig("UTC")
	if err != nil || cfg.CardTheme != "paper" {
		t.Fatal(cfg, err)
	}
	for _, pair := range [][2]string{
		{"VENDOR_STATUS_ENABLED", "bad"},
		{"VENDOR_STATUS_POLL_INTERVAL", "1s"},
		{"VENDOR_STATUS_HTTP_TIMEOUT", "-1s"},
		{"VENDOR_STATUS_HISTORY_LOOKBACK_HOURS", "169"},
		{"VENDOR_STATUS_SOURCE_FAILURE_THRESHOLD", "text"},
		{"VENDOR_STATUS_WORKER_TIMEOUT", "0s"},
		{"VENDOR_STATUS_GROUP_WHITELIST", "1:GroupMessage:100"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			t.Setenv(pair[0], pair[1])
			if _, err := loadVendorStatusConfig("UTC"); err == nil {
				t.Fatal("invalid value accepted")
			}
		})
	}
}
