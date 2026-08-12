package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotFound              = errors.New("not found")
	ErrIdentityBound         = errors.New("QQ 身份已经绑定 New API 账户")
	ErrNewAPIUserBound       = errors.New("该 New API 账户已经被其他 QQ 身份绑定")
	ErrLinkCodeInvalid       = errors.New("关联码无效或已过期")
	ErrCheckinDuplicated     = errors.New("本周期已经签到")
	ErrEventInboxFull        = errors.New("gateway event inbox is full")
	ErrResetActivityInactive = errors.New("当前没有可参加的重置活动")
	ErrResetActivityNotDue   = errors.New("重置活动尚未到期")
	ErrResetAwardFinalized   = errors.New("重置活动奖项已经完成发放")
)

const maxAuditRecords = 10_000

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
	[]byte("event_inbox"),
	[]byte("gateway"),
	[]byte("audit"),
	[]byte("quota_notifications"),
	[]byte("group_welcome"),
	[]byte("group_join_approval"),
	[]byte("pending_admin_actions"),
	[]byte("sent_bot_messages"),
	[]byte("benefit_campaigns"),
	[]byte("benefit_bans"),
	[]byte("command_rules"),
	[]byte("runtime_settings"),
	[]byte("reset_settings"),
	[]byte("reset_signals"),
	[]byte("reset_signal_deliveries"),
	[]byte("reset_group_states"),
	[]byte("reset_activities"),
	[]byte("reset_active_by_group"),
	[]byte("reset_participants"),
	[]byte("reset_due_activities"),
	[]byte("reset_notifications"),
	[]byte("reset_notification_due"),
	[]byte("reset_runtime"),
}

type Store struct {
	db                   *bolt.DB
	lastDedupCleanupUnix atomic.Int64
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
		return rebuildResetIndexesTx(tx)
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

func clearBucketTx(bucket *bolt.Bucket) error {
	cursor := bucket.Cursor()
	for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	return nil
}

func resetDueKey(at time.Time, id string) []byte {
	key := make([]byte, 8+len(id))
	binary.BigEndian.PutUint64(key[:8], uint64(at.UnixNano()))
	copy(key[8:], id)
	return key
}

func rebuildResetIndexesTx(tx *bolt.Tx) error {
	dueActivities := tx.Bucket([]byte("reset_due_activities"))
	if err := clearBucketTx(dueActivities); err != nil {
		return err
	}
	if err := tx.Bucket([]byte("reset_activities")).ForEach(func(_, data []byte) error {
		var activity model.ResetActivity
		if err := json.Unmarshal(data, &activity); err != nil {
			return err
		}
		if activity.Status != model.ResetActivityActive && activity.Status != model.ResetActivitySettling {
			return nil
		}
		return dueActivities.Put(resetDueKey(activity.EndsAt, activity.ID), []byte(activity.ID))
	}); err != nil {
		return err
	}

	dueNotifications := tx.Bucket([]byte("reset_notification_due"))
	if err := clearBucketTx(dueNotifications); err != nil {
		return err
	}
	return tx.Bucket([]byte("reset_notifications")).ForEach(func(_, data []byte) error {
		var notification model.ResetNotification
		if err := json.Unmarshal(data, &notification); err != nil {
			return err
		}
		if notification.Status != model.ResetNotificationPending {
			return nil
		}
		return dueNotifications.Put(resetDueKey(notification.NextAttemptAt, notification.ID), []byte(notification.ID))
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
	resolve := func(tx *bolt.Tx) (string, bool, error) {
		aliases := tx.Bucket([]byte("aliases"))
		bindings := tx.Bucket([]byte("bindings"))
		resolved := ""
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
				return "", false, ErrNotFound
			}
		}
		needsUpdate := false
		for _, key := range candidates {
			if key != resolved {
				if current := aliases.Get([]byte(key)); current == nil || string(current) != resolved {
					needsUpdate = true
				}
			}
		}
		return resolved, needsUpdate, nil
	}

	var resolved string
	var needsUpdate bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var resolveErr error
		resolved, needsUpdate, resolveErr = resolve(tx)
		return resolveErr
	})
	if err != nil {
		return "", err
	}
	if needsUpdate {
		err = s.db.Update(func(tx *bolt.Tx) error {
			var resolveErr error
			resolved, _, resolveErr = resolve(tx)
			if resolveErr != nil {
				return resolveErr
			}
			aliases := tx.Bucket([]byte("aliases"))
			for _, key := range candidates {
				if key == resolved {
					continue
				}
				if current := aliases.Get([]byte(key)); current != nil && string(current) == resolved {
					continue
				}
				if err := aliases.Put([]byte(key), []byte(resolved)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return "", err
		}
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
	var unchanged bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		unchanged = string(tx.Bucket([]byte("aliases")).Get([]byte(alias))) == canonical
		return nil
	}); err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("aliases")).Get([]byte(alias))) == canonical {
			return nil
		}
		return tx.Bucket([]byte("aliases")).Put([]byte(alias), []byte(canonical))
	})
}

func (s *Store) PutContact(canonical, userOpenID string) error {
	if canonical == "" || userOpenID == "" {
		return errors.New("canonical identity and user openid are required")
	}
	var unchanged bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		unchanged = string(tx.Bucket([]byte("contacts")).Get([]byte(canonical))) == userOpenID
		return nil
	}); err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("contacts")).Get([]byte(canonical))) == userOpenID {
			return nil
		}
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

// ListGroupBindings returns the uniquely bound accounts that have been seen in
// a specific group. QQ does not expose a general member-list API to this bot,
// so only members whose group identity was observed by the Gateway are included.
func (s *Store) ListGroupBindings(groupOpenID string) ([]model.Binding, error) {
	groupOpenID = strings.TrimSpace(groupOpenID)
	if groupOpenID == "" {
		return nil, errors.New("group openid is required")
	}
	prefix := "member:" + groupOpenID + ":"
	result := make([]model.Binding, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bindings := tx.Bucket([]byte("bindings"))
		aliases := tx.Bucket([]byte("aliases"))
		canonicalIDs := make(map[string]struct{})
		prefixBytes := []byte(prefix)
		aliasCursor := aliases.Cursor()
		for key, value := aliasCursor.Seek(prefixBytes); key != nil && bytes.HasPrefix(key, prefixBytes); key, value = aliasCursor.Next() {
			if len(value) > 0 {
				canonicalIDs[string(value)] = struct{}{}
			}
		}
		bindingCursor := bindings.Cursor()
		for key, _ := bindingCursor.Seek(prefixBytes); key != nil && bytes.HasPrefix(key, prefixBytes); key, _ = bindingCursor.Next() {
			canonicalIDs[string(key)] = struct{}{}
		}
		for canonicalID := range canonicalIDs {
			value := bindings.Get([]byte(canonicalID))
			if value == nil {
				continue
			}
			var binding model.Binding
			if err := json.Unmarshal(value, &binding); err != nil {
				return err
			}
			result = append(result, binding)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NewAPIID != result[j].NewAPIID {
			return result[i].NewAPIID < result[j].NewAPIID
		}
		return result[i].CanonicalID < result[j].CanonicalID
	})
	return result, nil
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

func (s *Store) ListCheckinsBetween(start, end time.Time) ([]model.CheckinRecord, error) {
	result := make([]model.CheckinRecord, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("checkins")).ForEach(func(_, value []byte) error {
			var record model.CheckinRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if !record.CreatedAt.Before(start) && record.CreatedAt.Before(end) {
				result = append(result, record)
			}
			return nil
		})
	})
	return result, err
}

func (s *Store) GetCheckinCreditOverride() (string, error) {
	var value string
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("runtime_settings")).Get([]byte("checkin_credit"))
		if data == nil {
			return ErrNotFound
		}
		value = string(data)
		return nil
	})
	return value, err
}

func (s *Store) PutCheckinCreditOverride(credit string) error {
	credit = strings.TrimSpace(credit)
	if credit == "" {
		return errors.New("签到额度不能为空")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("runtime_settings")).Put([]byte("checkin_credit"), []byte(credit))
	})
}

func checkinKey(canonical, period string) []byte {
	return []byte(canonical + "|" + period)
}

func (s *Store) CheckAndMarkMessage(key string, now time.Time, ttl time.Duration) (bool, error) {
	const cleanupInterval = 5 * time.Minute
	nowUnix := now.Unix()
	cleanup := false
	lastCleanup := s.lastDedupCleanupUnix.Load()
	if lastCleanup == 0 || nowUnix-lastCleanup >= int64(cleanupInterval/time.Second) {
		cleanup = s.lastDedupCleanupUnix.CompareAndSwap(lastCleanup, nowUnix)
	}
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
		if cleanup {
			cursor := bucket.Cursor()
			for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
				if len(v) == 8 && int64(binary.BigEndian.Uint64(v)) <= nowUnix {
					_ = cursor.Delete()
				}
			}
		}
		return nil
	})
	return duplicate, err
}

func (s *Store) UnmarkMessage(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("message_dedup")).Delete([]byte(key))
	})
}

// EnqueueGatewayEvent atomically records deduplication state and a recoverable
// event payload. The returned pending value is true for both newly accepted
// events and duplicate deliveries that still need processing.
func (s *Store) EnqueueGatewayEvent(key string, payload []byte, now time.Time, ttl time.Duration, maxPending int) (pending bool, err error) {
	key = strings.TrimSpace(key)
	if key == "" || len(payload) == 0 {
		return false, errors.New("gateway event key and payload are required")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxPending <= 0 {
		maxPending = 512
	}
	const cleanupInterval = 5 * time.Minute
	nowUnix := now.Unix()
	cleanup := false
	lastCleanup := s.lastDedupCleanupUnix.Load()
	if lastCleanup == 0 || nowUnix-lastCleanup >= int64(cleanupInterval/time.Second) {
		cleanup = s.lastDedupCleanupUnix.CompareAndSwap(lastCleanup, nowUnix)
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		dedup := tx.Bucket([]byte("message_dedup"))
		inbox := tx.Bucket([]byte("event_inbox"))
		keyBytes := []byte(key)
		if inbox.Get(keyBytes) != nil {
			var encoded [8]byte
			binary.BigEndian.PutUint64(encoded[:], uint64(now.Add(ttl).Unix()))
			if err := dedup.Put(keyBytes, encoded[:]); err != nil {
				return err
			}
			pending = true
			return nil
		}
		if value := dedup.Get(keyBytes); len(value) == 8 && int64(binary.BigEndian.Uint64(value)) > nowUnix {
			return nil
		}
		if inbox.Stats().KeyN >= maxPending {
			return ErrEventInboxFull
		}
		record, err := json.Marshal(model.PendingGatewayEvent{Payload: append([]byte(nil), payload...), CreatedAt: now})
		if err != nil {
			return err
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(now.Add(ttl).Unix()))
		if err := dedup.Put(keyBytes, encoded[:]); err != nil {
			return err
		}
		if err := inbox.Put(keyBytes, record); err != nil {
			return err
		}
		pending = true
		if cleanup {
			cursor := dedup.Cursor()
			for existingKey, value := cursor.First(); existingKey != nil; existingKey, value = cursor.Next() {
				if len(value) == 8 && int64(binary.BigEndian.Uint64(value)) <= nowUnix && inbox.Get(existingKey) == nil {
					if err := cursor.Delete(); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	return pending, err
}

func (s *Store) ListPendingGatewayEvents(limit int) ([]model.PendingGatewayEvent, error) {
	if limit <= 0 {
		limit = 64
	}
	result := make([]model.PendingGatewayEvent, 0, limit)
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket([]byte("event_inbox")).Cursor()
		for key, value := cursor.First(); key != nil && len(result) < limit; key, value = cursor.Next() {
			var item model.PendingGatewayEvent
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			item.Key = string(key)
			result = append(result, item)
		}
		return nil
	})
	return result, err
}

func (s *Store) CompleteGatewayEvent(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("event_inbox")).Delete([]byte(key))
	})
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
		if err := bucket.Put(key[:], data); err != nil {
			return err
		}
		if seq > maxAuditRecords {
			var expired [8]byte
			binary.BigEndian.PutUint64(expired[:], seq-maxAuditRecords)
			_ = bucket.Delete(expired[:])
		}
		return nil
	})
}

// PruneEphemeral removes records that have no value after their expiry window.
// It runs infrequently from the existing maintenance worker to keep bbolt and
// its mmap-backed resident set bounded during long-lived deployments.
func (s *Store) PruneEphemeral(now time.Time, emailWindow, sentRetention time.Duration) error {
	if emailWindow <= 0 {
		emailWindow = time.Hour
	}
	if sentRetention <= 0 {
		sentRetention = 10 * time.Minute
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := pruneJSONExpiry[model.PendingBind](tx.Bucket([]byte("pending_binds")), now, func(item model.PendingBind) time.Time { return item.ExpiresAt }); err != nil {
			return err
		}
		if err := pruneJSONExpiry[model.LinkChallenge](tx.Bucket([]byte("link_codes")), now, func(item model.LinkChallenge) time.Time { return item.ExpiresAt }); err != nil {
			return err
		}
		if err := pruneJSONExpiry[model.PendingAdminAction](tx.Bucket([]byte("pending_admin_actions")), now, func(item model.PendingAdminAction) time.Time { return item.ExpiresAt }); err != nil {
			return err
		}
		sentCutoff := now.Add(-sentRetention)
		if err := pruneJSONExpiry[model.SentBotMessage](tx.Bucket([]byte("sent_bot_messages")), sentCutoff, func(item model.SentBotMessage) time.Time { return item.SentAt }); err != nil {
			return err
		}
		rateBucket := tx.Bucket([]byte("email_rate"))
		rateCutoff := now.Add(-emailWindow).Unix()
		cursor := rateBucket.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			times, err := readTimes(value)
			if err != nil {
				return err
			}
			active := times[:0]
			for _, timestamp := range times {
				if timestamp > rateCutoff {
					active = append(active, timestamp)
				}
			}
			if len(active) == 0 {
				if err := cursor.Delete(); err != nil {
					return err
				}
				continue
			}
			if len(active) != len(times) {
				if err := rateBucket.Put(key, writeTimes(active)); err != nil {
					return err
				}
			}
		}
		audit := tx.Bucket([]byte("audit"))
		excess := audit.Stats().KeyN - maxAuditRecords
		auditCursor := audit.Cursor()
		for index := 0; index < excess; index++ {
			key, _ := auditCursor.First()
			if key == nil {
				break
			}
			if err := auditCursor.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

func pruneJSONExpiry[T any](bucket *bolt.Bucket, cutoff time.Time, expiresAt func(T) time.Time) error {
	cursor := bucket.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		var item T
		if err := json.Unmarshal(value, &item); err != nil {
			return err
		}
		expires := expiresAt(item)
		if expires.IsZero() || !expires.After(cutoff) {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
	}
	return nil
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
			if preference.Enabled || preference.DailyEnabled {
				preferences = append(preferences, preference)
			}
			return nil
		})
	})
	return preferences, err
}

func (s *Store) PutGroupWelcome(setting model.GroupWelcome) error {
	if strings.TrimSpace(setting.GroupOpenID) == "" {
		return errors.New("群 OpenID 不能为空")
	}
	setting.UpdatedAt = time.Now()
	data, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("group_welcome")).Put([]byte(setting.GroupOpenID), data)
	})
}

func (s *Store) GetGroupWelcome(group string) (model.GroupWelcome, error) {
	var result model.GroupWelcome
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("group_welcome")).Get([]byte(group))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) PutGroupJoinApproval(setting model.GroupJoinApproval) error {
	if strings.TrimSpace(setting.GroupOpenID) == "" {
		return errors.New("群 OpenID 不能为空")
	}
	setting.UpdatedAt = time.Now()
	data, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("group_join_approval")).Put([]byte(setting.GroupOpenID), data)
	})
}

func (s *Store) GetGroupJoinApproval(group string) (model.GroupJoinApproval, error) {
	var result model.GroupJoinApproval
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("group_join_approval")).Get([]byte(group))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) PutPendingAdminAction(action model.PendingAdminAction) error {
	data, err := json.Marshal(action)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("pending_admin_actions")).Put([]byte(action.Code), data)
	})
}

func (s *Store) TakePendingAdminAction(code, actor string, now time.Time) (model.PendingAdminAction, error) {
	var result model.PendingAdminAction
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("pending_admin_actions"))
		data := b.Get([]byte(code))
		if data == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
		if result.Actor != actor || now.After(result.ExpiresAt) {
			_ = b.Delete([]byte(code))
			return ErrNotFound
		}
		return b.Delete([]byte(code))
	})
	return result, err
}

func (s *Store) PutSentBotMessage(message model.SentBotMessage) error {
	if message.SentAt.IsZero() {
		message.SentAt = time.Now()
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sent_bot_messages"))
		if message.MessageID != "" {
			if err := b.Put([]byte("id|"+message.GroupOpenID+"|"+message.MessageID), data); err != nil {
				return err
			}
		}
		if message.MessageIdx != "" {
			if err := b.Put([]byte("idx|"+message.GroupOpenID+"|"+message.MessageIdx), data); err != nil {
				return err
			}
		}
		return b.Put([]byte("last|"+message.GroupOpenID), data)
	})
}

func (s *Store) GetSentBotMessage(group, reference string) (model.SentBotMessage, error) {
	var result model.SentBotMessage
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sent_bot_messages"))
		var data []byte
		if reference != "" {
			data = b.Get([]byte("id|" + group + "|" + reference))
			if data == nil {
				data = b.Get([]byte("idx|" + group + "|" + reference))
			}
		}
		if data == nil {
			data = b.Get([]byte("last|" + group))
		}
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) PutBenefitCampaign(campaign model.BenefitCampaign) error {
	if campaign.ID == "" || campaign.GroupOpenID == "" {
		return errors.New("福利活动缺少必要字段")
	}
	campaign.UpdatedAt = time.Now()
	data, err := json.Marshal(campaign)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("benefit_campaigns")).Put([]byte(campaign.ID), data) })
}

func (s *Store) GetBenefitCampaign(id string) (model.BenefitCampaign, error) {
	var result model.BenefitCampaign
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("benefit_campaigns")).Get([]byte(id))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) ListBenefitCampaigns() ([]model.BenefitCampaign, error) {
	result := make([]model.BenefitCampaign, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("benefit_campaigns")).ForEach(func(_, data []byte) error {
			var item model.BenefitCampaign
			if err := json.Unmarshal(data, &item); err != nil {
				return err
			}
			if item.Status == "pending" || item.Status == "active" {
				result = append(result, item)
			}
			return nil
		})
	})
	return result, err
}

func (s *Store) PutBenefitBan(ban model.BenefitBan) error {
	if ban.Key == "" {
		ban.Key = ban.CampaignID + "|" + strconv.Itoa(ban.UserID)
	}
	ban.UpdatedAt = time.Now()
	data, err := json.Marshal(ban)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("benefit_bans")).Put([]byte(ban.Key), data) })
}

func (s *Store) GetBenefitBan(campaignID string, userID int) (model.BenefitBan, error) {
	var result model.BenefitBan
	key := campaignID + "|" + strconv.Itoa(userID)
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("benefit_bans")).Get([]byte(key))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) ListBenefitBans() ([]model.BenefitBan, error) {
	result := make([]model.BenefitBan, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("benefit_bans")).ForEach(func(_, data []byte) error {
			var item model.BenefitBan
			if err := json.Unmarshal(data, &item); err != nil {
				return err
			}
			if item.Status == "disabled" || item.Status == "disable_pending" || item.Status == "disable_failed" || item.Status == "enable_failed" {
				result = append(result, item)
			}
			return nil
		})
	})
	return result, err
}

func (s *Store) PutCommandRule(rule model.CommandRule) error {
	keyword := strings.TrimSpace(rule.Keyword)
	if keyword == "" {
		return errors.New("命令关键词不能为空")
	}
	rule.Keyword = keyword
	rule.UpdatedAt = time.Now()
	data, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("command_rules")).Put([]byte(keyword), data)
	})
}

func (s *Store) GetCommandRule(keyword string) (model.CommandRule, error) {
	var result model.CommandRule
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("command_rules")).Get([]byte(keyword))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) ListCommandRules() ([]model.CommandRule, error) {
	result := make([]model.CommandRule, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("command_rules")).ForEach(func(_, data []byte) error {
			var rule model.CommandRule
			if err := json.Unmarshal(data, &rule); err != nil {
				return err
			}
			result = append(result, rule)
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Keyword < result[j].Keyword })
	return result, err
}

func defaultResetSettings(group string, now time.Time) model.ResetSettings {
	return model.ResetSettings{
		GroupOpenID: group,
		Duration:    model.DefaultResetDuration,
		WinnerCount: model.DefaultResetWinnerCount,
		Lookback:    model.DefaultResetLookback,
		Subscribed:  true,
		UpdatedAt:   now,
	}
}

func validateResetSettings(setting model.ResetSettings) error {
	if strings.TrimSpace(setting.GroupOpenID) == "" {
		return errors.New("群 OpenID 不能为空")
	}
	if setting.Duration <= 0 {
		return errors.New("重置活动有效期必须大于 0")
	}
	if setting.WinnerCount <= 0 {
		return errors.New("重置活动抽取人数必须大于 0")
	}
	if setting.Lookback <= 0 {
		return errors.New("重置活动补偿回溯时间必须大于 0")
	}
	return nil
}

func (s *Store) GetOrCreateResetSettings(group string) (model.ResetSettings, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return model.ResetSettings{}, errors.New("群 OpenID 不能为空")
	}
	var result model.ResetSettings
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("reset_settings"))
		if data := bucket.Get([]byte(group)); data != nil {
			return json.Unmarshal(data, &result)
		}
		result = defaultResetSettings(group, time.Now())
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(group), data)
	})
	return result, err
}

func (s *Store) GetResetSettings(group string) (model.ResetSettings, error) {
	var result model.ResetSettings
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("reset_settings")).Get([]byte(strings.TrimSpace(group)))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) PutResetSettings(setting model.ResetSettings) error {
	setting.GroupOpenID = strings.TrimSpace(setting.GroupOpenID)
	if err := validateResetSettings(setting); err != nil {
		return err
	}
	setting.UpdatedAt = time.Now()
	data, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("reset_settings")).Put([]byte(setting.GroupOpenID), data)
	})
}

func (s *Store) ListSubscribedResetSettings() ([]model.ResetSettings, error) {
	result := make([]model.ResetSettings, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("reset_settings")).ForEach(func(_, data []byte) error {
			var setting model.ResetSettings
			if err := json.Unmarshal(data, &setting); err != nil {
				return err
			}
			if setting.Subscribed {
				result = append(result, setting)
			}
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].GroupOpenID < result[j].GroupOpenID })
	return result, err
}

func (s *Store) GetResetProxy() (string, error) {
	var result string
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("reset_runtime")).Get([]byte("encrypted_proxy"))
		if data == nil {
			return ErrNotFound
		}
		result = string(data)
		return nil
	})
	return result, err
}

func (s *Store) PutResetProxy(encryptedProxy string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("reset_runtime"))
		if encryptedProxy == "" {
			return bucket.Delete([]byte("encrypted_proxy"))
		}
		return bucket.Put([]byte("encrypted_proxy"), []byte(encryptedProxy))
	})
}

func resetStageRank(stage model.ResetStage) int {
	switch stage {
	case model.ResetStagePossible:
		return 1
	case model.ResetStageImminent:
		return 2
	case model.ResetStageConfirmed:
		return 3
	default:
		return 0
	}
}

func validateResetSignal(signal model.ResetSignal) error {
	if signal.ID == "" {
		return errors.New("重置信号 ID 不能为空")
	}
	if resetStageRank(signal.Stage) == 0 {
		return errors.New("重置信号状态无效")
	}
	return nil
}

func normalizeResetSignal(signal model.ResetSignal, now time.Time) model.ResetSignal {
	if signal.DetectedAt.IsZero() {
		signal.DetectedAt = now
	}
	if signal.OccurredAt.IsZero() {
		signal.OccurredAt = signal.DetectedAt
	}
	signal.UpdatedAt = now
	return signal
}

func putResetSignalTx(tx *bolt.Tx, signal model.ResetSignal) (bool, error) {
	bucket := tx.Bucket([]byte("reset_signals"))
	if data := bucket.Get([]byte(signal.ID)); data != nil {
		var current model.ResetSignal
		if err := json.Unmarshal(data, &current); err != nil {
			return false, err
		}
		if resetStageRank(signal.Stage) <= resetStageRank(current.Stage) {
			return false, nil
		}
	}
	data, err := json.Marshal(signal)
	if err != nil {
		return false, err
	}
	if err := bucket.Put([]byte(signal.ID), data); err != nil {
		return false, err
	}

	runtime := tx.Bucket([]byte("reset_runtime"))
	shouldReplaceHighest := true
	if highestID := runtime.Get([]byte("highest_signal_id")); highestID != nil {
		if currentData := bucket.Get(highestID); currentData != nil {
			var current model.ResetSignal
			if err := json.Unmarshal(currentData, &current); err != nil {
				return false, err
			}
			shouldReplaceHighest = resetStageRank(signal.Stage) > resetStageRank(current.Stage) ||
				(resetStageRank(signal.Stage) == resetStageRank(current.Stage) && signal.OccurredAt.After(current.OccurredAt))
		}
	}
	if shouldReplaceHighest {
		if err := runtime.Put([]byte("highest_signal_id"), []byte(signal.ID)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *Store) RecordResetSignal(signal model.ResetSignal) (bool, error) {
	if err := validateResetSignal(signal); err != nil {
		return false, err
	}
	signal = normalizeResetSignal(signal, time.Now())
	var changed bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		changed, err = putResetSignalTx(tx, signal)
		return err
	})
	return changed, err
}

func (s *Store) GetResetSignal(id string) (model.ResetSignal, error) {
	var result model.ResetSignal
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetSignalTx(tx, strings.TrimSpace(id))
		return err
	})
	return result, err
}

func getResetSignalTx(tx *bolt.Tx, id string) (model.ResetSignal, error) {
	var result model.ResetSignal
	data := tx.Bucket([]byte("reset_signals")).Get([]byte(id))
	if data == nil {
		return result, ErrNotFound
	}
	return result, json.Unmarshal(data, &result)
}

func resetNotificationID(kind model.ResetNotificationKind, group, signalID string, stage model.ResetStage, activityID string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + group + "\x00" + signalID + "\x00" + string(stage) + "\x00" + activityID))
	return fmt.Sprintf("reset-notification-%x", sum[:12])
}

func enqueueResetNotificationTx(tx *bolt.Tx, kind model.ResetNotificationKind, group string, signal model.ResetSignal, activityID string, now time.Time) error {
	id := resetNotificationID(kind, group, signal.ID, signal.Stage, activityID)
	notifications := tx.Bucket([]byte("reset_notifications"))
	due := tx.Bucket([]byte("reset_notification_due"))
	if notifications == nil || due == nil {
		return errors.New("reset notification buckets are missing")
	}
	if notifications.Get([]byte(id)) != nil {
		return nil
	}
	notification := model.ResetNotification{
		ID:            id,
		Kind:          kind,
		Status:        model.ResetNotificationPending,
		GroupOpenID:   group,
		SignalID:      signal.ID,
		SignalStage:   signal.Stage,
		SignalSource:  signal.Source,
		SignalSummary: signal.Summary,
		SignalURL:     signal.URL,
		ActivityID:    activityID,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	data, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	if err := notifications.Put([]byte(id), data); err != nil {
		return err
	}
	return due.Put(resetDueKey(now, id), []byte(id))
}

func getResetNotificationTx(tx *bolt.Tx, id string) (model.ResetNotification, error) {
	var result model.ResetNotification
	data := tx.Bucket([]byte("reset_notifications")).Get([]byte(id))
	if data == nil {
		return result, ErrNotFound
	}
	return result, json.Unmarshal(data, &result)
}

func putResetNotificationTx(tx *bolt.Tx, notification model.ResetNotification) error {
	data, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte("reset_notifications")).Put([]byte(notification.ID), data)
}

func (s *Store) ListDueResetNotifications(now time.Time, limit int) ([]model.ResetNotification, error) {
	result := make([]model.ResetNotification, 0)
	if limit <= 0 {
		return result, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := resetDueKey(now, "\xff")
	err := s.db.View(func(tx *bolt.Tx) error {
		due := tx.Bucket([]byte("reset_notification_due"))
		cursor := due.Cursor()
		for key, id := cursor.First(); key != nil && bytes.Compare(key, cutoff) <= 0 && len(result) < limit; key, id = cursor.Next() {
			notification, err := getResetNotificationTx(tx, string(id))
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if notification.Status == model.ResetNotificationPending && !notification.NextAttemptAt.After(now) {
				result = append(result, notification)
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) PrepareResetNotification(id string, chunks []string, now time.Time) (model.ResetNotification, error) {
	var result model.ResetNotification
	if len(chunks) == 0 {
		return result, errors.New("重置通知内容为空")
	}
	for _, chunk := range chunks {
		if chunk == "" {
			return result, errors.New("重置通知包含空分块")
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetNotificationTx(tx, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		if result.Status != model.ResetNotificationPending || len(result.Chunks) > 0 {
			return nil
		}
		result.Chunks = append([]string(nil), chunks...)
		result.NextChunk = 0
		result.UpdatedAt = now
		return putResetNotificationTx(tx, result)
	})
	return result, err
}

func (s *Store) MarkResetNotificationChunkSent(id string, chunkIndex int, now time.Time) (model.ResetNotification, error) {
	var result model.ResetNotification
	if now.IsZero() {
		now = time.Now()
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetNotificationTx(tx, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		if result.Status != model.ResetNotificationPending || chunkIndex < result.NextChunk {
			return nil
		}
		if len(result.Chunks) == 0 || chunkIndex != result.NextChunk || chunkIndex >= len(result.Chunks) {
			return errors.New("重置通知分块进度无效")
		}
		result.NextChunk++
		result.LastError = ""
		result.UpdatedAt = now
		if result.NextChunk == len(result.Chunks) {
			if err := tx.Bucket([]byte("reset_notification_due")).Delete(resetDueKey(result.NextAttemptAt, result.ID)); err != nil {
				return err
			}
			result.Status = model.ResetNotificationSent
			result.SentAt = now
		}
		return putResetNotificationTx(tx, result)
	})
	return result, err
}

func (s *Store) MarkResetNotificationSent(id string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		notification, err := getResetNotificationTx(tx, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		if notification.Status != model.ResetNotificationPending {
			return nil
		}
		if err := tx.Bucket([]byte("reset_notification_due")).Delete(resetDueKey(notification.NextAttemptAt, notification.ID)); err != nil {
			return err
		}
		notification.Status = model.ResetNotificationSent
		notification.LastError = ""
		notification.SentAt = now
		notification.UpdatedAt = now
		return putResetNotificationTx(tx, notification)
	})
}

func (s *Store) MarkResetNotificationSuperseded(id string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		notification, err := getResetNotificationTx(tx, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		if notification.Status != model.ResetNotificationPending {
			return nil
		}
		if err := tx.Bucket([]byte("reset_notification_due")).Delete(resetDueKey(notification.NextAttemptAt, notification.ID)); err != nil {
			return err
		}
		notification.Status = model.ResetNotificationSuperseded
		notification.LastError = ""
		notification.UpdatedAt = now
		return putResetNotificationTx(tx, notification)
	})
}

func (s *Store) MarkResetNotificationFailed(id, lastError string, nextAttemptAt, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	if nextAttemptAt.IsZero() || nextAttemptAt.Before(now) {
		nextAttemptAt = now
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		notification, err := getResetNotificationTx(tx, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		if notification.Status != model.ResetNotificationPending {
			return nil
		}
		due := tx.Bucket([]byte("reset_notification_due"))
		if err := due.Delete(resetDueKey(notification.NextAttemptAt, notification.ID)); err != nil {
			return err
		}
		notification.Attempts++
		notification.LastError = strings.TrimSpace(lastError)
		notification.NextAttemptAt = nextAttemptAt
		notification.UpdatedAt = now
		if err := putResetNotificationTx(tx, notification); err != nil {
			return err
		}
		return due.Put(resetDueKey(notification.NextAttemptAt, notification.ID), []byte(notification.ID))
	})
}

func (s *Store) GetHighestResetSignal() (model.ResetSignal, error) {
	var result model.ResetSignal
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("reset_runtime")).Get([]byte("highest_signal_id"))
		if id == nil {
			return ErrNotFound
		}
		data := tx.Bucket([]byte("reset_signals")).Get(id)
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func (s *Store) GetResetGroupState(group string) (model.ResetGroupState, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return model.ResetGroupState{}, errors.New("群 OpenID 不能为空")
	}
	result := model.ResetGroupState{GroupOpenID: group, Stage: model.ResetStageUnknown}
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("reset_group_states")).Get([]byte(group))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

func getResetGroupStateTx(tx *bolt.Tx, group string) (model.ResetGroupState, error) {
	result := model.ResetGroupState{GroupOpenID: group, Stage: model.ResetStageUnknown}
	data := tx.Bucket([]byte("reset_group_states")).Get([]byte(group))
	if data == nil {
		return result, nil
	}
	return result, json.Unmarshal(data, &result)
}

func resetSignalDeliveryKey(group, signalID string) []byte {
	return []byte(group + "\x00" + signalID)
}

func getResetSignalDeliveryTx(tx *bolt.Tx, group, signalID string) (model.ResetSignalDelivery, bool, error) {
	var result model.ResetSignalDelivery
	data := tx.Bucket([]byte("reset_signal_deliveries")).Get(resetSignalDeliveryKey(group, signalID))
	if data == nil {
		return result, false, nil
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, false, err
	}
	return result, true, nil
}

func putResetSignalDeliveryTx(tx *bolt.Tx, group string, signal model.ResetSignal, activityID string, now time.Time) (bool, error) {
	bucket := tx.Bucket([]byte("reset_signal_deliveries"))
	key := resetSignalDeliveryKey(group, signal.ID)
	if data := bucket.Get(key); data != nil {
		var current model.ResetSignalDelivery
		if err := json.Unmarshal(data, &current); err != nil {
			return false, err
		}
		if resetStageRank(current.Stage) >= resetStageRank(signal.Stage) {
			return false, nil
		}
	}
	delivery := model.ResetSignalDelivery{
		GroupOpenID: group,
		SignalID:    signal.ID,
		Stage:       signal.Stage,
		ActivityID:  activityID,
		ProcessedAt: now,
	}
	data, err := json.Marshal(delivery)
	if err != nil {
		return false, err
	}
	if err := bucket.Put(key, data); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ApplyResetSignalToGroup(group string, signal model.ResetSignal) (bool, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return false, errors.New("群 OpenID 不能为空")
	}
	if signal.Stage != model.ResetStagePossible && signal.Stage != model.ResetStageImminent {
		return false, errors.New("该接口只接受可能重置或即将重置的信号")
	}
	if err := validateResetSignal(signal); err != nil {
		return false, err
	}
	signal = normalizeResetSignal(signal, time.Now())
	var changed bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		if _, err := putResetSignalTx(tx, signal); err != nil {
			return err
		}
		current, err := getResetGroupStateTx(tx, group)
		if err != nil {
			return err
		}
		shouldApply, err := putResetSignalDeliveryTx(tx, group, signal, current.ActivityID, signal.UpdatedAt)
		if err != nil {
			return err
		}
		if !shouldApply {
			return nil
		}
		if !current.LastCompletedAt.IsZero() && !signal.OccurredAt.After(current.LastCompletedAt) {
			return nil
		}
		if resetStageRank(signal.Stage) <= resetStageRank(current.Stage) {
			return nil
		}
		current = model.ResetGroupState{
			GroupOpenID:     group,
			Stage:           signal.Stage,
			SignalID:        signal.ID,
			Summary:         signal.Summary,
			Source:          signal.Source,
			SourceURL:       signal.URL,
			LastCompletedAt: current.LastCompletedAt,
			UpdatedAt:       signal.UpdatedAt,
		}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte("reset_group_states")).Put([]byte(group), data); err != nil {
			return err
		}
		if err := enqueueResetNotificationTx(tx, model.ResetNotificationSignal, group, signal, "", signal.UpdatedAt); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *Store) ExpireResetGroupStates(cutoff, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now()
	}
	expired := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("reset_group_states"))
		updates := make([]model.ResetGroupState, 0)
		if err := bucket.ForEach(func(_, data []byte) error {
			var state model.ResetGroupState
			if err := json.Unmarshal(data, &state); err != nil {
				return err
			}
			if state.Stage != model.ResetStagePossible && state.Stage != model.ResetStageImminent {
				return nil
			}
			if state.UpdatedAt.After(cutoff) {
				return nil
			}
			updates = append(updates, model.ResetGroupState{
				GroupOpenID:     state.GroupOpenID,
				Stage:           model.ResetStageUnknown,
				LastCompletedAt: state.LastCompletedAt,
				UpdatedAt:       now,
			})
			return nil
		}); err != nil {
			return err
		}
		for _, state := range updates {
			encoded, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(state.GroupOpenID), encoded); err != nil {
				return err
			}
			expired++
		}
		return nil
	})
	return expired, err
}

func resetActivityID(group, signalID string) string {
	sum := sha256.Sum256([]byte(group + "\x00" + signalID))
	return fmt.Sprintf("reset-%x", sum[:12])
}

func getResetActivityTx(tx *bolt.Tx, id string) (model.ResetActivity, error) {
	var result model.ResetActivity
	data := tx.Bucket([]byte("reset_activities")).Get([]byte(id))
	if data == nil {
		return result, ErrNotFound
	}
	return result, json.Unmarshal(data, &result)
}

func putResetActivityTx(tx *bolt.Tx, activity model.ResetActivity) error {
	data, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte("reset_activities")).Put([]byte(activity.ID), data)
}

func (s *Store) CreateResetActivityFromSignal(group string, signal model.ResetSignal, now time.Time) (model.ResetActivity, bool, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return model.ResetActivity{}, false, errors.New("群 OpenID 不能为空")
	}
	if signal.Stage != model.ResetStageConfirmed {
		return model.ResetActivity{}, false, errors.New("只有确认重置信号可以创建活动")
	}
	if err := validateResetSignal(signal); err != nil {
		return model.ResetActivity{}, false, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	signal = normalizeResetSignal(signal, now)
	var result model.ResetActivity
	var created bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		if _, err := putResetSignalTx(tx, signal); err != nil {
			return err
		}
		delivery, delivered, err := getResetSignalDeliveryTx(tx, group, signal.ID)
		if err != nil {
			return err
		}
		if delivered && resetStageRank(delivery.Stage) >= resetStageRank(model.ResetStageConfirmed) {
			if delivery.ActivityID != "" {
				if existing, getErr := getResetActivityTx(tx, delivery.ActivityID); getErr == nil {
					result = existing
				} else if !errors.Is(getErr, ErrNotFound) {
					return getErr
				}
			}
			return nil
		}
		currentState, err := getResetGroupStateTx(tx, group)
		if err != nil {
			return err
		}
		if !currentState.LastCompletedAt.IsZero() && !signal.OccurredAt.After(currentState.LastCompletedAt) {
			_, err := putResetSignalDeliveryTx(tx, group, signal, "", now)
			return err
		}
		activeByGroup := tx.Bucket([]byte("reset_active_by_group"))
		if activeID := activeByGroup.Get([]byte(group)); activeID != nil {
			current, err := getResetActivityTx(tx, string(activeID))
			if err == nil && (current.Status == model.ResetActivityActive || current.Status == model.ResetActivitySettling) {
				result = current
				_, err := putResetSignalDeliveryTx(tx, group, signal, current.ID, now)
				return err
			}
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err := activeByGroup.Delete([]byte(group)); err != nil {
				return err
			}
		}

		id := resetActivityID(group, signal.ID)
		if existing, err := getResetActivityTx(tx, id); err == nil {
			result = existing
			_, err := putResetSignalDeliveryTx(tx, group, signal, existing.ID, now)
			return err
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		settings := defaultResetSettings(group, now)
		settingsBucket := tx.Bucket([]byte("reset_settings"))
		if data := settingsBucket.Get([]byte(group)); data != nil {
			if err := json.Unmarshal(data, &settings); err != nil {
				return err
			}
		} else {
			data, err := json.Marshal(settings)
			if err != nil {
				return err
			}
			if err := settingsBucket.Put([]byte(group), data); err != nil {
				return err
			}
		}
		if err := validateResetSettings(settings); err != nil {
			return err
		}
		result = model.ResetActivity{
			ID:          id,
			GroupOpenID: group,
			SignalID:    signal.ID,
			Status:      model.ResetActivityActive,
			StartedAt:   now,
			EndsAt:      now.Add(settings.Duration),
			WinnerCount: settings.WinnerCount,
			Lookback:    settings.Lookback,
			UpdatedAt:   now,
		}
		if err := putResetActivityTx(tx, result); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("reset_due_activities")).Put(resetDueKey(result.EndsAt, result.ID), []byte(result.ID)); err != nil {
			return err
		}
		if err := activeByGroup.Put([]byte(group), []byte(id)); err != nil {
			return err
		}
		if _, err := putResetSignalDeliveryTx(tx, group, signal, id, now); err != nil {
			return err
		}
		state := model.ResetGroupState{
			GroupOpenID:     group,
			Stage:           model.ResetStageConfirmed,
			SignalID:        signal.ID,
			ActivityID:      id,
			Summary:         signal.Summary,
			Source:          signal.Source,
			SourceURL:       signal.URL,
			LastCompletedAt: currentState.LastCompletedAt,
			UpdatedAt:       now,
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte("reset_group_states")).Put([]byte(group), data); err != nil {
			return err
		}
		if err := enqueueResetNotificationTx(tx, model.ResetNotificationActivityStarted, group, signal, id, now); err != nil {
			return err
		}
		created = true
		return nil
	})
	return result, created, err
}

func (s *Store) GetResetActivity(id string) (model.ResetActivity, error) {
	var result model.ResetActivity
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetActivityTx(tx, id)
		return err
	})
	return result, err
}

func (s *Store) GetActiveResetActivity(group string) (model.ResetActivity, error) {
	var result model.ResetActivity
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("reset_active_by_group")).Get([]byte(strings.TrimSpace(group)))
		if id == nil {
			return ErrNotFound
		}
		var err error
		result, err = getResetActivityTx(tx, string(id))
		if err != nil {
			return err
		}
		if result.Status != model.ResetActivityActive && result.Status != model.ResetActivitySettling {
			return ErrNotFound
		}
		return nil
	})
	return result, err
}

func resetParticipantKey(activityID string, newAPIID int) []byte {
	return []byte(activityID + "\x00" + fmt.Sprintf("%020d", newAPIID))
}

func resetParticipantPrefix(activityID string) []byte {
	return []byte(activityID + "\x00")
}

func (s *Store) JoinResetActivity(group string, participant model.ResetParticipant, now time.Time) (model.ResetActivity, bool, error) {
	group = strings.TrimSpace(group)
	if group == "" || participant.NewAPIID <= 0 || participant.CanonicalID == "" {
		return model.ResetActivity{}, false, errors.New("重置活动参与信息不完整")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var result model.ResetActivity
	var joined bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("reset_active_by_group")).Get([]byte(group))
		if id == nil {
			return ErrResetActivityInactive
		}
		var err error
		result, err = getResetActivityTx(tx, string(id))
		if err != nil {
			return err
		}
		if result.Status != model.ResetActivityActive || !now.Before(result.EndsAt) {
			return ErrResetActivityInactive
		}
		participants := tx.Bucket([]byte("reset_participants"))
		key := resetParticipantKey(result.ID, participant.NewAPIID)
		if participants.Get(key) != nil {
			return nil
		}
		participant.ActivityID = result.ID
		participant.GroupOpenID = group
		participant.JoinedAt = now
		data, err := json.Marshal(participant)
		if err != nil {
			return err
		}
		if err := participants.Put(key, data); err != nil {
			return err
		}
		result.ParticipantCount++
		result.UpdatedAt = now
		if err := putResetActivityTx(tx, result); err != nil {
			return err
		}
		joined = true
		return nil
	})
	return result, joined, err
}

func (s *Store) ListResetParticipants(activityID string) ([]model.ResetParticipant, error) {
	result := make([]model.ResetParticipant, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("reset_participants"))
		cursor := bucket.Cursor()
		prefix := resetParticipantPrefix(activityID)
		for key, data := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, data = cursor.Next() {
			var participant model.ResetParticipant
			if err := json.Unmarshal(data, &participant); err != nil {
				return err
			}
			result = append(result, participant)
		}
		return nil
	})
	return result, err
}

func (s *Store) ListDueResetActivities(now time.Time) ([]model.ResetActivity, error) {
	result := make([]model.ResetActivity, 0)
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := resetDueKey(now, "\xff")
	err := s.db.View(func(tx *bolt.Tx) error {
		due := tx.Bucket([]byte("reset_due_activities"))
		cursor := due.Cursor()
		for key, id := cursor.First(); key != nil && bytes.Compare(key, cutoff) <= 0; key, id = cursor.Next() {
			activity, err := getResetActivityTx(tx, string(id))
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if activity.Status == model.ResetActivityActive || activity.Status == model.ResetActivitySettling {
				result = append(result, activity)
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) BeginResetSettlement(activityID string, awards []model.ResetAward, now time.Time) (model.ResetActivity, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var result model.ResetActivity
	var started bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetActivityTx(tx, activityID)
		if err != nil {
			return err
		}
		if result.Status == model.ResetActivitySettling || result.Status == model.ResetActivityCompleted {
			return nil
		}
		if result.Status != model.ResetActivityActive {
			return ErrResetActivityInactive
		}
		if now.Before(result.EndsAt) {
			return ErrResetActivityNotDue
		}
		if len(awards) > result.WinnerCount || len(awards) > result.ParticipantCount {
			return errors.New("重置活动中奖人数超过允许数量")
		}
		seen := make(map[int]struct{}, len(awards))
		participants := tx.Bucket([]byte("reset_participants"))
		for index := range awards {
			award := &awards[index]
			if award.NewAPIID <= 0 {
				return errors.New("重置活动奖项用户 ID 无效")
			}
			if _, exists := seen[award.NewAPIID]; exists {
				return errors.New("重置活动中奖用户重复")
			}
			seen[award.NewAPIID] = struct{}{}
			participantData := participants.Get(resetParticipantKey(activityID, award.NewAPIID))
			if participantData == nil {
				return errors.New("重置活动中奖用户未参加活动")
			}
			var participant model.ResetParticipant
			if err := json.Unmarshal(participantData, &participant); err != nil {
				return err
			}
			award.CanonicalID = participant.CanonicalID
			award.MemberOpenID = participant.MemberOpenID
			if award.RawQuota < 0 {
				return errors.New("重置活动补偿额度不能为负数")
			}
			if award.RawQuota == 0 {
				award.Status = model.ResetAwardZero
			} else if award.Status == "" {
				award.Status = model.ResetAwardPending
			}
			award.UpdatedAt = now
		}
		result.Status = model.ResetActivitySettling
		result.Awards = append([]model.ResetAward(nil), awards...)
		result.SelectedAt = now
		result.UpdatedAt = now
		if err := putResetActivityTx(tx, result); err != nil {
			return err
		}
		started = true
		return nil
	})
	return result, started, err
}

func isResetAwardFinal(status model.ResetAwardStatus) bool {
	switch status {
	case model.ResetAwardGranted, model.ResetAwardZero, model.ResetAwardFailed, model.ResetAwardPendingConfirmation:
		return true
	default:
		return false
	}
}

func validResetAwardStatus(status model.ResetAwardStatus) bool {
	switch status {
	case model.ResetAwardPending, model.ResetAwardGranting, model.ResetAwardGranted, model.ResetAwardZero,
		model.ResetAwardFailed, model.ResetAwardPendingConfirmation:
		return true
	default:
		return false
	}
}

func (s *Store) UpdateResetAward(activityID string, newAPIID int, status model.ResetAwardStatus, lastError string, now time.Time) (model.ResetActivity, error) {
	if !validResetAwardStatus(status) {
		return model.ResetActivity{}, errors.New("重置活动奖项状态无效")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var result model.ResetActivity
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetActivityTx(tx, activityID)
		if err != nil {
			return err
		}
		if result.Status != model.ResetActivitySettling {
			return ErrResetActivityInactive
		}
		for index := range result.Awards {
			award := &result.Awards[index]
			if award.NewAPIID != newAPIID {
				continue
			}
			if isResetAwardFinal(award.Status) && award.Status != status {
				return ErrResetAwardFinalized
			}
			award.Status = status
			award.LastError = lastError
			award.UpdatedAt = now
			result.UpdatedAt = now
			return putResetActivityTx(tx, result)
		}
		return ErrNotFound
	})
	return result, err
}

func (s *Store) CompleteResetActivity(activityID string, now time.Time) (model.ResetActivity, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var result model.ResetActivity
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		result, err = getResetActivityTx(tx, activityID)
		if err != nil {
			return err
		}
		if result.Status == model.ResetActivityCompleted {
			return nil
		}
		if result.Status != model.ResetActivitySettling {
			return ErrResetActivityInactive
		}
		for _, award := range result.Awards {
			if !isResetAwardFinal(award.Status) {
				return errors.New("重置活动仍有未完成的奖项")
			}
		}
		result.Status = model.ResetActivityCompleted
		result.CompletedAt = now
		result.UpdatedAt = now
		if err := putResetActivityTx(tx, result); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("reset_due_activities")).Delete(resetDueKey(result.EndsAt, result.ID)); err != nil {
			return err
		}
		activeByGroup := tx.Bucket([]byte("reset_active_by_group"))
		if activeID := activeByGroup.Get([]byte(result.GroupOpenID)); string(activeID) == activityID {
			if err := activeByGroup.Delete([]byte(result.GroupOpenID)); err != nil {
				return err
			}
		}
		state := model.ResetGroupState{
			GroupOpenID:     result.GroupOpenID,
			Stage:           model.ResetStageUnknown,
			LastCompletedAt: now,
			UpdatedAt:       now,
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte("reset_group_states")).Put([]byte(result.GroupOpenID), data); err != nil {
			return err
		}
		signal, err := getResetSignalTx(tx, result.SignalID)
		if err != nil {
			return err
		}
		return enqueueResetNotificationTx(tx, model.ResetNotificationActivityCompleted, result.GroupOpenID, signal, result.ID, now)
	})
	return result, err
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
