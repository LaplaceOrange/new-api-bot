package vendorstatus

import (
	"encoding/json"
	"testing"
)

func TestProgressModeDefaultValidationAndRuntimeOverride(t *testing.T) {
	base := DefaultConfig()
	if base.ProgressMode != "detailed" {
		t.Fatal(base.ProgressMode)
	}
	if err := DecodeConfig([]byte(`{"card_theme":"paper"}`), &base); err != nil || base.ProgressMode != "detailed" {
		t.Fatal("legacy JSON lost default", err)
	}
	for _, mode := range []string{"detailed", "simple", "off"} {
		encoded, _ := json.Marshal(mode)
		cfg, err := ApplyOverrides(base, map[string]json.RawMessage{"progress_mode": encoded})
		if err != nil || cfg.ProgressMode != mode {
			t.Fatal(mode, err)
		}
	}
	for _, mode := range []string{"", "none", "verbose", "true"} {
		cfg := base
		cfg.ProgressMode = mode
		if cfg.Validate() == nil {
			t.Fatal("invalid mode accepted", mode)
		}
	}
}
