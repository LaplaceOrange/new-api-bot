package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVendorSubscriptionsPersistOverridesAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := []string{"configured", "configured"}
	if changed, count, err := storage.SetVendorSubscription("configured", true, defaults); err != nil || changed || count != 1 {
		t.Fatal(changed, count, err)
	}
	if changed, count, err := storage.SetVendorSubscription("configured", false, defaults); err != nil || !changed || count != 0 {
		t.Fatal(changed, count, err)
	}
	if changed, count, err := storage.SetVendorSubscription("command", true, defaults); err != nil || !changed || count != 1 {
		t.Fatal(changed, count, err)
	}
	if changed, _, err := storage.SetVendorSubscription("command", true, defaults); err != nil || changed {
		t.Fatal(changed, err)
	}
	if err := storage.PutVendorStatusValue("monitor_state_v1", json.RawMessage(`{"version":1,"sources":{},"deliveries":{}}`)); err != nil {
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
	if groups, err := storage.VendorSubscriptions(defaults); err != nil || !reflect.DeepEqual(groups, []string{"command"}) {
		t.Fatal(groups, err)
	}
	if values, err := storage.VendorStatusValues(); err != nil || len(values["monitor_state_v1"]) == 0 {
		t.Fatal(values, err)
	}
}

func TestVendorStatusStoreRejectsInvalidInput(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	for _, group := range []string{"", "1:GroupMessage:100", "bad group"} {
		if _, _, err := storage.SetVendorSubscription(group, true, nil); err == nil {
			t.Fatal(group)
		}
	}
	for _, value := range []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(`not-json`)} {
		if err := storage.PutVendorStatusValue("monitor_state_v1", value); err == nil {
			t.Fatal(string(value))
		}
	}
	if err := storage.PutVendorStatusValue("wrong", []byte(`{}`)); err == nil {
		t.Fatal("unknown storage key was accepted")
	}
}
