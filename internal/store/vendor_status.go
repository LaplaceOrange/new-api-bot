package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/fsykk/new-api-bot/internal/vendorstatus"
	bolt "go.etcd.io/bbolt"
)

var vendorStatusBucket = []byte("vendor_status")
var vendorSubscriptionPrefix = []byte("subscription:")

func (s *Store) VendorStatusValues() (map[string]json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(vendorStatusBucket)
		for _, key := range []string{vendorstatus.StateKey, vendorstatus.TranslationKey} {
			if data := bucket.Get([]byte(key)); data != nil {
				values[key] = append(json.RawMessage(nil), data...)
			}
		}
		return nil
	})
	return values, err
}

func (s *Store) PutVendorStatusValue(key string, value json.RawMessage) error {
	if key != vendorstatus.StateKey && key != vendorstatus.TranslationKey {
		return errors.New("未知厂商状态存储键")
	}
	if len(value) > 24<<20 || !json.Valid(value) || len(bytes.TrimSpace(value)) == 0 || bytes.TrimSpace(value)[0] != '{' {
		return errors.New("厂商状态检查点必须是有效且不超过 24 MiB 的 JSON 对象")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(vendorStatusBucket).Put([]byte(key), value)
	})
}

func vendorSubscriptionsTx(bucket *bolt.Bucket, defaults []string) (map[string]bool, error) {
	groups := map[string]bool{}
	for _, group := range defaults {
		if group = strings.TrimSpace(group); group != "" {
			groups[group] = true
		}
	}
	cursor := bucket.Cursor()
	for key, data := cursor.Seek(vendorSubscriptionPrefix); bytes.HasPrefix(key, vendorSubscriptionPrefix); key, data = cursor.Next() {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return nil, err
		}
		groups[string(key[len(vendorSubscriptionPrefix):])] = enabled
	}
	return groups, nil
}

// VendorSubscriptions merges deployment defaults with durable per-group
// overrides. In particular, "off" remains off after restarting with the same
// environment whitelist; it is not accidentally re-enabled by configuration.
func (s *Store) VendorSubscriptions(defaults []string) ([]string, error) {
	var groups []string
	err := s.db.View(func(tx *bolt.Tx) error {
		current, err := vendorSubscriptionsTx(tx.Bucket(vendorStatusBucket), defaults)
		if err != nil {
			return err
		}
		for group, enabled := range current {
			if enabled {
				groups = append(groups, group)
			}
		}
		return nil
	})
	sort.Strings(groups)
	return groups, err
}

func (s *Store) SetVendorSubscription(group string, enabled bool, defaults []string) (changed bool, count int, err error) {
	group = strings.TrimSpace(group)
	if group == "" || strings.ContainsAny(group, ": \t\r\n") {
		return false, 0, errors.New("无效的 QQ group_openid")
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(vendorStatusBucket)
		current, err := vendorSubscriptionsTx(bucket, defaults)
		if err != nil {
			return err
		}
		if current[group] != enabled {
			data, _ := json.Marshal(enabled)
			if err := bucket.Put(append(append([]byte(nil), vendorSubscriptionPrefix...), group...), data); err != nil {
				return err
			}
			changed = true
			current[group] = enabled
		}
		for _, active := range current {
			if active {
				count++
			}
		}
		return nil
	})
	return
}
