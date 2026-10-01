package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestVendorRuntimePersistsAndAtomicallyReplacesSubscriptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"one", "two", "three"} {
		if _, _, err := storage.SetVendorSubscription(group, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	runtime := VendorRuntimeConfig{
		Version: 1,
		Overrides: map[string]json.RawMessage{
			"card_theme": []byte(`"midnight"`), "group_whitelist": []byte(`["new"]`),
		},
		TranslationAPIKeyOverridden: true, EncryptedTranslationAPIKey: "ciphertext",
	}
	if err := storage.PutVendorRuntimeConfig(runtime, true); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	got, groups, err := storage.VendorSettingsSnapshot()
	if err != nil || len(groups) != 0 || string(got.Overrides["card_theme"]) != `"midnight"` || got.EncryptedTranslationAPIKey != "ciphertext" {
		t.Fatal(got, groups, err)
	}
}

func TestVendorRuntimeRejectsPlaintextKeysAndInvalidVersions(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	for _, runtime := range []VendorRuntimeConfig{
		{Version: 2},
		{Version: 1, Overrides: map[string]json.RawMessage{"translation.api_key": []byte(`"secret"`)}},
		{Version: 1, Overrides: map[string]json.RawMessage{"translation": []byte(`{"api_key":"secret"}`)}},
		{Version: 1, Overrides: map[string]json.RawMessage{"platform_id": []byte(`"other"`)}},
	} {
		if err := storage.PutVendorRuntimeConfig(runtime, false); err == nil {
			t.Fatal("invalid runtime accepted")
		}
	}
}
