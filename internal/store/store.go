package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrIdentityBound     = errors.New("QQ 身份已经绑定 New API 账户")
	ErrNewAPIUserBound   = errors.New("该 New API 账户已经被其他 QQ 身份绑定")
	ErrLinkCodeInvalid   = errors.New("关联码无效或已过期")
	ErrCheckinDuplicated = errors.New("本周期已经签到")
)

var buckets = [][]byte{
	[]byte("bindings"),
	[]byte("bindings_by_user"),
	[]byte("aliases"),
	[]byte("contacts"),
	[]byte("pending_binds"),
	[]byte("email_rate"),
	[]byte("link_codes"),
	[]byte("checkins"),
	[]byte("checkins_by_user"),
	[]byte("message_dedup"),
	[]byte("gateway"),
	[]byte("audit"),
	[]byte("quota_notifications"),
}

type Store struct {
	db *bolt.DB
}

type GatewayState struct {
	SessionID string `json:"session_id"`
	Sequence  int64  `json:"sequence"`
}

func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error {
	return s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("bindings")) == nil {
			return errors.New("bindings bucket is missing")
		}
		return nil
	})
}

func (s *Store) ResolveCanonical(identity model.QQIdentity) (string, error) {
	unionKey := ""
	if identity.UnionOpenID != "" {
		unionKey = "union:" + identity.UnionOpenID
	}
	userKey := ""
	if identity.UserOpenID != "" {
		userKey = "user:" + identity.UserOpenID
	}
	groupKey := identity.GroupAlias()
	candidates := make([]string, 0, 3)
	// 群别名优先：通过 /link 建立的映射不能被后来出现的 union_openid 覆盖。
	for _, key := range []string{groupKey, userKey, unionKey} {
		if key != "" {
			candidates = append(candidates, key)
		}
	}
	if len(candidates) == 0 {
		return "", ErrNotFound
	}
	var resolved string
	err := s.db.Update(func(tx *bolt.Tx) error {
		aliases := tx.Bucket([]byte("aliases"))
		bindings := tx.Bucket([]byte("bindings"))
		for _, key := range candidates {
			if value := aliases.Get([]byte(key)); value != nil {
				resolved = string(value)
				break
			}
			if bindings.Get([]byte(key)) != nil {
				resolved = key
				break
			}
		}
		if resolved == "" {
			if unionKey != "" {
				resolved = unionKey
			} else if userKey != "" {
				resolved = userKey
			} else if groupKey != "" {
				// QQ 群机器人可能只提供 group_openid + member_openid。
				// 在纯群聊模式下直接以群成员身份作为主身份，无需再通过私聊 /link。
				resolved = groupKey
			} else {
				return ErrNotFound
			}
		}
		for _, key := range candidates {
			if key != resolved {
				if err := aliases.Put([]byte(key), []byte(resolved)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if identity.UserOpenID != "" {
		_ = s.PutContact(resolved, identity.UserOpenID)
	}
	return resolved, nil
}

func (s *Store) PutAlias(alias, canonical string) error {
	if alias == "" || canonical == "" {
		return errors.New("alias and canonical identity are required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("aliases")).Put([]byte(alias), []byte(canonical))
	})
}

func (s *Store) PutContact(canonical, userOpenID string) error {
	if canonical == "" || userOpenID == "" {
		return errors.New("canonical identity and user openid are required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("contacts")).Put([]byte(canonical), []byte(userOpenID))
	})
}

func (s *Store) GetContact(canonical string) (string, error) {
	var result string
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("contacts")).Get([]byte(canonical))
		if value == nil {
			return ErrNotFound
		}
		result = string(value)
		return nil
	})
	return result, err
}

func (s *Store) CreateBinding(binding model.Binding) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		byIdentity := tx.Bucket([]byte("bindings"))
		byUser := tx.Bucket([]byte("bindings_by_user"))
		if byIdentity.Get([]byte(binding.CanonicalID)) != nil {
			return ErrIdentityBound
		}
		userKey := []byte(strconv.Itoa(binding.NewAPIID))
		if byUser.Get(userKey) != nil {
			return ErrNewAPIUserBound
		}
		data, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		if err := byIdentity.Put([]byte(binding.CanonicalID), data); err != nil {
			return err
		}
		if err := byUser.Put(userKey, []byte(binding.CanonicalID)); err != nil {
			return err
		}
		return tx.Bucket([]byte("pending_binds")).Delete([]byte(binding.CanonicalID))
	})
}

func (s *Store) GetBinding(canonical string) (model.Binding, error) {
	var result model.Binding
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("bindings")).Get([]byte(canonical))
		if value == nil {
			return ErrNotFound
		}
		return json.Unmarshal(value, &result)
	})
	return result, err
}

func (s *Store) GetBindingByNewAPIID(id int) (model.Binding, error) {
	var canonical string
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("bindings_by_user")).Get([]byte(strconv.Itoa(id)))
		if value == nil {
			return ErrNotFound
		}
		canonical = string(value)
		return nil
	})
	if err != nil {
		return model.Binding{}, err
	}
	return s.GetBinding(canonical)
}

func (s *Store) UnbindByNewAPIID(id int) (model.Binding, error) {
	var removed model.Binding
	err := s.db.Update(func(tx *bolt.Tx) error {
		byIdentity := tx.Bucket([]byte("bindings"))
		byUser := tx.Bucket([]byte("bindings_by_user"))
		userKey := []byte(strconv.Itoa(id))
		canonical := byUser.Get(userKey)
		if canonical == nil {
			return ErrNotFound
		}
		value := byIdentity.Get(canonical)
		if value == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(value, &removed); err != nil {
			return err
		}
		if err := byIdentity.Delete(canonical); err != nil {
			return err
		}
		return byUser.Delete(userKey)
	})
	return removed, err
}

func (s *Store) ListBindings(page, pageSize int) ([]model.Binding, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	var all []model.Binding
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("bindings")).ForEach(func(_, value []byte) error {
			var binding model.Binding
			if err := json.Unmarshal(value, &binding); err != nil {
				return err
			}
			all = append(all, binding)
			return nil
		})
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := len(all)
	start := (page - 1) * pageSize
	if start >= total {
		return []model.Binding{}, total, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return all[start:end], total, nil
}

func (s *Store) PutPendingBind(pending model.PendingBind) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("pending_binds")).Put([]byte(pending.CanonicalID), data)
	})
}

func (s *Store) GetPendingBind(canonical string) (model.PendingBind, error) {
	var pending model.PendingBind
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("pending_binds")).Get([]byte(canonical))
		if value == nil {
			return ErrNotFound
		}
		return json.Unmarshal(value, &pending)
	})
	return pending, err
}

func (s *Store) DeletePendingBind(canonical string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("pending_binds")).Delete([]byte(canonical))
	})
}

func (s *Store) IncrementPendingAttempts(canonical string) (int, error) {
	var attempts int
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("pending_binds"))
		value := b.Get([]byte(canonical))
		if value == nil {
			return ErrNotFound
		}
		var pending model.PendingBind
		if err := json.Unmarshal(value, &pending); err != nil {
			return err
		}
		pending.Attempts++
		attempts = pending.Attempts
		data, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		return b.Put([]byte(canonical), data)
	})
	return attempts, err
}

func (s *Store) EmailRateRemaining(keys []string, now time.Time, window time.Duration, limit int) (time.Duration, error) {
	var maxWait time.Duration
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("email_rate"))
		for _, key := range keys {
			times, err := readTimes(bucket.Get([]byte(key)))
			if err != nil {
				return err
			}
			cutoff := now.Add(-window).Unix()
			active := times[:0]
			for _, ts := range times {
				if ts > cutoff {
					active = append(active, ts)
				}
			}
			if err := bucket.Put([]byte(key), writeTimes(active)); err != nil {
				return err
			}
			if len(active) >= limit {
				wait := time.Unix(active[0], 0).Add(window).Sub(now)
				if wait > maxWait {
					maxWait = wait
				}
			}
		}
		return nil
	})
	return maxWait, err
}

func (s *Store) RecordEmailSent(keys []string, now time.Time, window time.Duration) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("email_rate"))
		cutoff := now.Add(-window).Unix()
		for _, key := range keys {
			times, err := readTimes(bucket.Get([]byte(key)))
			if err != nil {
				return err
			}
			active := times[:0]
			for _, ts := range times {
				if ts > cutoff {
					active = append(active, ts)
				}
			}
			active = append(active, now.Unix())
			if err := bucket.Put([]byte(key), writeTimes(active)); err != nil {
				return err
			}
		}
		return nil
	})
}

func readTimes(data []byte) ([]int64, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var result []int64
	return result, json.Unmarshal(data, &result)
}

func writeTimes(times []int64) []byte {
	data, _ := json.Marshal(times)
	return data
}

func (s *Store) PutLinkChallenge(mac string, challenge model.LinkChallenge) error {
	data, err := json.Marshal(challenge)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("link_codes")).Put([]byte(mac), data)
	})
}

func (s *Store) ConsumeLinkChallenge(mac, groupAlias string, now time.Time) (string, error) {
	var canonical string
	err := s.db.Update(func(tx *bolt.Tx) error {
		codes := tx.Bucket([]byte("link_codes"))
		value := codes.Get([]byte(mac))
		if value == nil {
			return ErrLinkCodeInvalid
		}
		var challenge model.LinkChallenge
		if err := json.Unmarshal(value, &challenge); err != nil {
			return err
		}
		if now.After(challenge.ExpiresAt) {
			_ = codes.Delete([]byte(mac))
			return ErrLinkCodeInvalid
		}
		if tx.Bucket([]byte("bindings")).Get([]byte(challenge.CanonicalID)) == nil {
			return ErrNotFound
		}
		if err := tx.Bucket([]byte("aliases")).Put([]byte(groupAlias), []byte(challenge.CanonicalID)); err != nil {
			return err
		}
		if err := codes.Delete([]byte(mac)); err != nil {
			return err
		}
		canonical = challenge.CanonicalID
		return nil
	})
	return canonical, err
}

func (s *Store) ReserveCheckin(record model.CheckinRecord) (model.CheckinRecord, bool, error) {
	var result model.CheckinRecord
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		checkins := tx.Bucket([]byte("checkins"))
		byUser := tx.Bucket([]byte("checkins_by_user"))
		key := checkinKey(record.CanonicalID, record.PeriodKey)
		userKey := []byte(strconv.Itoa(record.NewAPIID) + "|" + record.PeriodKey)
		if existingKey := byUser.Get(userKey); existingKey != nil {
			value := checkins.Get(existingKey)
			if value == nil {
				return ErrCheckinDuplicated
			}
			return json.Unmarshal(value, &result)
		}
		if value := checkins.Get(key); value != nil {
			return json.Unmarshal(value, &result)
		}
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := checkins.Put(key, data); err != nil {
			return err
		}
		if err := byUser.Put(userKey, key); err != nil {
			return err
		}
		result = record
		created = true
		return nil
	})
	return result, created, err
}

func (s *Store) FinalizeCheckin(record model.CheckinRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("checkins")).Put(checkinKey(record.CanonicalID, record.PeriodKey), data)
	})
}

func (s *Store) DeletePendingCheckin(record model.CheckinRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		checkins := tx.Bucket([]byte("checkins"))
		key := checkinKey(record.CanonicalID, record.PeriodKey)
		value := checkins.Get(key)
		if value == nil {
			return nil
		}
		var current model.CheckinRecord
		if err := json.Unmarshal(value, &current); err != nil {
			return err
		}
		if current.Status != "pending" {
			return nil
		}
		if err := checkins.Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte("checkins_by_user")).Delete([]byte(strconv.Itoa(record.NewAPIID) + "|" + record.PeriodKey))
	})
}

func (s *Store) GetCheckin(canonical, period string) (model.CheckinRecord, error) {
	var result model.CheckinRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("checkins")).Get(checkinKey(canonical, period))
		if value == nil {
			return ErrNotFound
		}
		return json.Unmarshal(value, &result)
	})
	return result, err
}

func checkinKey(canonical, period string) []byte {
	return []byte(canonical + "|" + period)
}

func (s *Store) CheckAndMarkMessage(key string, now time.Time, ttl time.Duration) (bool, error) {
	duplicate := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("message_dedup"))
		if value := bucket.Get([]byte(key)); len(value) == 8 {
			expires := int64(binary.BigEndian.Uint64(value))
			if expires > now.Unix() {
				duplicate = true
				return nil
			}
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(now.Add(ttl).Unix()))
		if err := bucket.Put([]byte(key), encoded[:]); err != nil {
			return err
		}
		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			if len(v) == 8 && int64(binary.BigEndian.Uint64(v)) <= now.Unix() {
				_ = cursor.Delete()
			}
		}
		return nil
	})
	return duplicate, err
}

func (s *Store) GetGatewayState() (GatewayState, error) {
	var state GatewayState
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("gateway")).Get([]byte("state"))
		if value == nil {
			return ErrNotFound
		}
		return json.Unmarshal(value, &state)
	})
	return state, err
}

func (s *Store) PutGatewayState(state GatewayState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("gateway")).Put([]byte("state"), data)
	})
}

func (s *Store) ClearGatewayState() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("gateway")).Delete([]byte("state"))
	})
}

func (s *Store) AddAudit(record model.AuditRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("audit"))
		seq, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		var key [8]byte
		binary.BigEndian.PutUint64(key[:], seq)
		return bucket.Put(key[:], data)
	})
}

func (s *Store) PutQuotaNotification(preference model.QuotaNotification) error {
	if strings.TrimSpace(preference.CanonicalID) == "" || preference.NewAPIID <= 0 || strings.TrimSpace(preference.GroupOpenID) == "" {
		return errors.New("额度提醒配置缺少必要字段")
	}
	preference.UpdatedAt = time.Now()
	data, err := json.Marshal(preference)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("quota_notifications")).Put([]byte(preference.CanonicalID), data)
	})
}

func (s *Store) GetQuotaNotification(canonical string) (model.QuotaNotification, error) {
	var preference model.QuotaNotification
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("quota_notifications")).Get([]byte(canonical))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &preference)
	})
	return preference, err
}

func (s *Store) DeleteQuotaNotification(canonical string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("quota_notifications")).Delete([]byte(canonical))
	})
}

func (s *Store) ListQuotaNotifications() ([]model.QuotaNotification, error) {
	preferences := make([]model.QuotaNotification, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("quota_notifications")).ForEach(func(_, value []byte) error {
			var preference model.QuotaNotification
			if err := json.Unmarshal(value, &preference); err != nil {
				return err
			}
			if preference.Enabled {
				preferences = append(preferences, preference)
			}
			return nil
		})
	})
	return preferences, err
}

func MaskEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 || len(parts[0]) == 0 {
		return "***"
	}
	local := []rune(parts[0])
	if len(local) == 1 {
		return string(local[0]) + "***@" + parts[1]
	}
	return string(local[0]) + "***" + string(local[len(local)-1]) + "@" + parts[1]
}
