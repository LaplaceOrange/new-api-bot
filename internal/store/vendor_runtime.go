package store

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/fsykk/new-api-bot/internal/vendorstatus"
	bolt "go.etcd.io/bbolt"
)

var vendorRuntimeKey = []byte("configuration_v1")

type VendorRuntimeConfig struct {
	Version                     int                        `json:"version"`
	Overrides                   map[string]json.RawMessage `json:"overrides"`
	TranslationAPIKeyOverridden bool                       `json:"translation_api_key_overridden"`
	EncryptedTranslationAPIKey  string                     `json:"encrypted_translation_api_key,omitempty"`
	ProxyOverridden             bool                       `json:"proxy_overridden"`
	EncryptedProxy              string                     `json:"encrypted_proxy,omitempty"`
}

// VendorSettingsSnapshot reads runtime settings and subscription overrides in
// one transaction, avoiding a mixed whitelist during a concurrent replacement.
func (s *Store) VendorSettingsSnapshot() (VendorRuntimeConfig, map[string]bool, error) {
	runtime := VendorRuntimeConfig{Version: 1, Overrides: make(map[string]json.RawMessage)}
	subscriptions := map[string]bool{}
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(vendorStatusBucket)
		if data := bucket.Get(vendorRuntimeKey); data != nil {
			if err := json.Unmarshal(data, &runtime); err != nil {
				return err
			}
			if err := validateVendorRuntime(runtime); err != nil {
				return err
			}
		}
		var err error
		subscriptions, err = vendorSubscriptionsTx(bucket, nil)
		return err
	})
	if runtime.Overrides == nil {
		runtime.Overrides = make(map[string]json.RawMessage)
	}
	return runtime, subscriptions, err
}

func validateVendorRuntime(runtime VendorRuntimeConfig) error {
	if runtime.Version != 1 {
		return errors.New("未知厂商运行时配置版本")
	}
	for key, value := range runtime.Overrides {
		option, ok := vendorstatus.FindOption(key)
		if !ok || option.Key != key || option.ReadOnly || option.Secret || !json.Valid(value) {
			return errors.New("无效或包含明文密钥的厂商运行时覆盖")
		}
	}
	return nil
}

func (s *Store) PutVendorRuntimeConfig(runtime VendorRuntimeConfig, replaceSubscriptions bool) error {
	if err := validateVendorRuntime(runtime); err != nil {
		return err
	}
	data, err := json.Marshal(runtime)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("厂商运行时配置不能超过 1 MiB")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(vendorStatusBucket)
		if err := bucket.Put(vendorRuntimeKey, data); err != nil {
			return err
		}
		if replaceSubscriptions {
			cursor := bucket.Cursor()
			// Re-seek after deleting: bbolt may rebalance pages, so Next can
			// skip an entry that moved to the cursor's former position.
			for key, _ := cursor.Seek(vendorSubscriptionPrefix); bytes.HasPrefix(key, vendorSubscriptionPrefix); key, _ = cursor.Seek(vendorSubscriptionPrefix) {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
