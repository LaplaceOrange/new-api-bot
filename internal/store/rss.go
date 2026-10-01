package store

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/fsykk/new-api-bot/internal/rss"
	bolt "go.etcd.io/bbolt"
)

var (
	rssSubscriptionsBucket = []byte("rss_subscriptions")
	rssGroupsBucket        = []byte("rss_groups")
	rssSeenBucket          = []byte("rss_seen")
	rssPendingBucket       = []byte("rss_pending")
)

func rssPut(bucket *bolt.Bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(key), data)
}

func rssGetSubscription(tx *bolt.Tx, group, id string) (rss.Subscription, error) {
	var subscription rss.Subscription
	data := tx.Bucket(rssSubscriptionsBucket).Get([]byte(id))
	if data == nil {
		return subscription, ErrNotFound
	}
	if err := json.Unmarshal(data, &subscription); err != nil {
		return subscription, err
	}
	if subscription.Group != group {
		return rss.Subscription{}, ErrNotFound
	}
	return subscription, nil
}

func (s *Store) RSSSubscription(group, id string) (rss.Subscription, error) {
	var sub rss.Subscription
	err := s.db.View(func(tx *bolt.Tx) (err error) {
		sub, err = rssGetSubscription(tx, group, id)
		return err
	})
	return sub, err
}

func (s *Store) ListRSSSubscriptions(group string) ([]rss.Subscription, error) {
	var subscriptions []rss.Subscription
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(rssSubscriptionsBucket).ForEach(func(_, data []byte) error {
			var sub rss.Subscription
			if err := json.Unmarshal(data, &sub); err != nil {
				return err
			}
			if group == "" || sub.Group == group {
				subscriptions = append(subscriptions, sub)
			}
			return nil
		})
	})
	sort.Slice(subscriptions, func(i, j int) bool {
		a, _ := strconv.ParseUint(subscriptions[i].ID, 10, 64)
		b, _ := strconv.ParseUint(subscriptions[j].ID, 10, 64)
		return a < b
	})
	return subscriptions, err
}

// AddRSSSubscription commits the initial baseline with the subscription.
func (s *Store) AddRSSSubscription(group, rawURL string, feed rss.Result, now time.Time) (rss.Subscription, bool, error) {
	var subscription rss.Subscription
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(rssSubscriptionsBucket)
		if err := bucket.ForEach(func(_, data []byte) error {
			var sub rss.Subscription
			if err := json.Unmarshal(data, &sub); err != nil {
				return err
			}
			if sub.Group == group && sub.URL == rawURL {
				subscription = sub
			}
			return nil
		}); err != nil {
			return err
		}
		if subscription.ID != "" {
			return nil
		}
		if group == "" || rawURL == "" || feed.NotModified {
			return errors.New("RSS 初始基线无效")
		}
		id, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		subscription = rss.Subscription{
			ID: strconv.FormatUint(id, 10), Group: group, URL: rawURL, Title: feed.Title,
			Enabled: true, Version: 1, CreatedAt: now, LastChecked: now, Validators: feed.Validators,
		}
		seen, err := tx.Bucket(rssSeenBucket).CreateBucket([]byte(subscription.ID))
		if err != nil {
			return err
		}
		if _, err := tx.Bucket(rssPendingBucket).CreateBucket([]byte(subscription.ID)); err != nil {
			return err
		}
		for _, article := range feed.Articles {
			if err := seen.Put([]byte(article.ID), []byte{1}); err != nil {
				return err
			}
		}
		created = true
		return rssPut(bucket, subscription.ID, subscription)
	})
	return subscription, created, err
}

func (s *Store) ChangeRSSSubscription(group, id string, remove bool, baseline *rss.Result, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		sub, err := rssGetSubscription(tx, group, id)
		if err != nil {
			return err
		}
		// A pause clears queued articles. A resume installs a fresh baseline.
		if err := tx.Bucket(rssPendingBucket).DeleteBucket([]byte(id)); err != nil {
			return err
		}
		if remove {
			if err := tx.Bucket(rssSeenBucket).DeleteBucket([]byte(id)); err != nil {
				return err
			}
			return tx.Bucket(rssSubscriptionsBucket).Delete([]byte(id))
		}
		if _, err := tx.Bucket(rssPendingBucket).CreateBucket([]byte(id)); err != nil {
			return err
		}
		sub.Version++
		sub.Enabled = baseline != nil
		sub.DeliveryError = ""
		if baseline != nil {
			if baseline.NotModified {
				return errors.New("RSS 恢复基线无效")
			}
			if err := tx.Bucket(rssSeenBucket).DeleteBucket([]byte(id)); err != nil {
				return err
			}
			seen, err := tx.Bucket(rssSeenBucket).CreateBucket([]byte(id))
			if err != nil {
				return err
			}
			for _, article := range baseline.Articles {
				if err := seen.Put([]byte(article.ID), []byte{1}); err != nil {
					return err
				}
			}
			sub.Title, sub.Validators = baseline.Title, baseline.Validators
			sub.LastChecked, sub.LastError = now, ""
		}
		return rssPut(tx.Bucket(rssSubscriptionsBucket), id, sub)
	})
}

// RecordRSSPoll atomically records seen IDs and durable delivery payloads.
// Version protects a newly resumed baseline from an older in-flight fetch.
func (s *Store) RecordRSSPoll(snapshot rss.Subscription, result rss.Result, pollErr error, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		sub, err := rssGetSubscription(tx, snapshot.Group, snapshot.ID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sub.Enabled || sub.Version != snapshot.Version {
			return nil
		}
		sub.LastChecked = now
		if pollErr != nil {
			sub.LastError = pollErr.Error() // Caller supplies only sanitized errors.
		} else {
			sub.LastError = ""
			sub.Validators = result.Validators
			if !result.NotModified {
				sub.Title = result.Title
				seen := tx.Bucket(rssSeenBucket).Bucket([]byte(sub.ID))
				pending := tx.Bucket(rssPendingBucket).Bucket([]byte(sub.ID))
				for _, article := range result.Articles {
					if seen.Get([]byte(article.ID)) != nil {
						continue
					}
					if err := seen.Put([]byte(article.ID), []byte{1}); err != nil {
						return err
					}
					number, err := pending.NextSequence()
					if err != nil {
						return err
					}
					id := strconv.FormatUint(number, 10)
					delivery := rss.Delivery{
						QueueID: id, Subscription: sub.ID, Group: sub.Group,
						Source: sub.Title, Version: sub.Version, Article: article,
					}
					if err := rssPut(pending, id, delivery); err != nil {
						return err
					}
				}
			}
		}
		return rssPut(tx.Bucket(rssSubscriptionsBucket), sub.ID, sub)
	})
}

func (s *Store) RSSGroupSettings(group string) (rss.GroupSettings, error) {
	var settings rss.GroupSettings
	err := s.db.View(func(tx *bolt.Tx) error {
		if data := tx.Bucket(rssGroupsBucket).Get([]byte(group)); data != nil {
			return json.Unmarshal(data, &settings)
		}
		return nil
	})
	return settings, err
}

// A zero interval only advances the scheduler cursor, preserving overrides.
func (s *Store) SetRSSGroupSettings(group string, interval time.Duration, cycle time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var settings rss.GroupSettings
		bucket := tx.Bucket(rssGroupsBucket)
		if data := bucket.Get([]byte(group)); data != nil {
			if err := json.Unmarshal(data, &settings); err != nil {
				return err
			}
		}
		if interval != 0 {
			settings.Interval = interval
		}
		settings.LastCycle = cycle
		return rssPut(bucket, group, settings)
	})
}

func rssDeliveryLess(a, b rss.Delivery) bool {
	if a.PublishedAt.IsZero() != b.PublishedAt.IsZero() {
		return !a.PublishedAt.IsZero()
	}
	if !a.PublishedAt.Equal(b.PublishedAt) {
		return a.PublishedAt.Before(b.PublishedAt)
	}
	left, _ := strconv.ParseUint(a.Subscription, 10, 64)
	right, _ := strconv.ParseUint(b.Subscription, 10, 64)
	if left != right {
		return left < right
	}
	left, _ = strconv.ParseUint(a.QueueID, 10, 64)
	right, _ = strconv.ParseUint(b.QueueID, 10, 64)
	return left < right
}

// Retains at most limit payloads in memory, regardless of backlog size.
func (s *Store) RSSPending(group string, limit int) ([]rss.Delivery, int, error) {
	var deliveries []rss.Delivery
	total := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(rssSubscriptionsBucket).ForEach(func(_, data []byte) error {
			var sub rss.Subscription
			if err := json.Unmarshal(data, &sub); err != nil {
				return err
			}
			if sub.Group != group || !sub.Enabled {
				return nil
			}
			return tx.Bucket(rssPendingBucket).Bucket([]byte(sub.ID)).ForEach(func(_, data []byte) error {
				total++
				if limit <= 0 {
					return nil
				}
				var delivery rss.Delivery
				if err := json.Unmarshal(data, &delivery); err != nil {
					return err
				}
				deliveries = append(deliveries, delivery)
				sort.SliceStable(deliveries, func(i, j int) bool { return rssDeliveryLess(deliveries[i], deliveries[j]) })
				if len(deliveries) > limit {
					deliveries = deliveries[:limit]
				}
				return nil
			})
		})
	})
	return deliveries, total, err
}

func (s *Store) RSSDeliveryCurrent(delivery rss.Delivery) (bool, error) {
	current := false
	err := s.db.View(func(tx *bolt.Tx) error {
		sub, err := rssGetSubscription(tx, delivery.Group, delivery.Subscription)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if sub.Enabled && sub.Version == delivery.Version {
			current = tx.Bucket(rssPendingBucket).Bucket([]byte(sub.ID)).Get([]byte(delivery.QueueID)) != nil
		}
		return nil
	})
	return current, err
}

func (s *Store) CompleteRSSDelivery(delivery rss.Delivery, failed bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		sub, err := rssGetSubscription(tx, delivery.Group, delivery.Subscription)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sub.Enabled || sub.Version != delivery.Version {
			return nil
		}
		sub.DeliveryError = ""
		if failed {
			sub.DeliveryError = "QQ 推送失败，将在下一轮重试"
		} else if err := tx.Bucket(rssPendingBucket).Bucket([]byte(sub.ID)).Delete([]byte(delivery.QueueID)); err != nil {
			return err
		}
		return rssPut(tx.Bucket(rssSubscriptionsBucket), sub.ID, sub)
	})
}

func (s *Store) MarkRSSSend(group string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var settings rss.GroupSettings
		bucket := tx.Bucket(rssGroupsBucket)
		if data := bucket.Get([]byte(group)); data != nil {
			if err := json.Unmarshal(data, &settings); err != nil {
				return err
			}
		}
		settings.LastSent = now
		return rssPut(bucket, group, settings)
	})
}
