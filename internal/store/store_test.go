package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	bolt "go.etcd.io/bbolt"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestBindingUniqueness(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.CreateBinding(model.Binding{CanonicalID: "user:a", NewAPIID: 1, Email: "a@example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBinding(model.Binding{CanonicalID: "user:a", NewAPIID: 2, Email: "b@example.com", CreatedAt: now}); !errors.Is(err, ErrIdentityBound) {
		t.Fatalf("expected identity conflict, got %v", err)
	}
	if err := s.CreateBinding(model.Binding{CanonicalID: "user:b", NewAPIID: 1, Email: "a@example.com", CreatedAt: now}); !errors.Is(err, ErrNewAPIUserBound) {
		t.Fatalf("expected user conflict, got %v", err)
	}
}

func TestResolveCanonicalKeepsLinkMapping(t *testing.T) {
	s := openTestStore(t)
	if err := s.PutAlias("member:g:m", "user:u"); err != nil {
		t.Fatal(err)
	}
	canonical, err := s.ResolveCanonical(model.QQIdentity{UnionOpenID: "union", MemberOpenID: "m", GroupOpenID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "user:u" {
		t.Fatalf("canonical=%q", canonical)
	}
	canonical, err = s.ResolveCanonical(model.QQIdentity{UnionOpenID: "union"})
	if err != nil || canonical != "user:u" {
		t.Fatalf("union alias was not preserved: %q %v", canonical, err)
	}
}

func TestListGroupBindingsUsesObservedGroupAliases(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	for _, binding := range []model.Binding{
		{CanonicalID: "member:g1:direct", NewAPIID: 1, CreatedAt: now},
		{CanonicalID: "user:linked", NewAPIID: 2, CreatedAt: now},
		{CanonicalID: "member:g2:other", NewAPIID: 3, CreatedAt: now},
	} {
		if err := s.CreateBinding(binding); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutAlias("member:g1:linked", "user:linked"); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.ListGroupBindings("g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[0].NewAPIID != 1 || bindings[1].NewAPIID != 2 {
		t.Fatalf("unexpected group bindings: %#v", bindings)
	}
}

func TestListKnownGroupsIncludesObservedAndLegacyData(t *testing.T) {
	s := openTestStore(t)
	if err := s.ObserveGroup("group-observed"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBinding(model.Binding{CanonicalID: "member:group-binding:member-1", NewAPIID: 7, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutGroupWelcome(model.GroupWelcome{GroupOpenID: "group-welcome", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.ListKnownGroups()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"group-binding", "group-observed", "group-welcome"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("groups=%v, want %v", groups, want)
	}
}

func TestEmailRateLimit(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	keys := []string{"actor:user:a", "target:1"}
	if err := s.RecordEmailSent(keys, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEmailSent(keys, now.Add(10*time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	wait, err := s.EmailRateRemaining(keys, now.Add(20*time.Minute), time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if wait != 40*time.Minute {
		t.Fatalf("wait=%v", wait)
	}
}

func TestMessageDedup(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	duplicate, err := s.CheckAndMarkMessage("message", now, time.Hour)
	if err != nil || duplicate {
		t.Fatalf("first duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = s.CheckAndMarkMessage("message", now.Add(time.Minute), time.Hour)
	if err != nil || !duplicate {
		t.Fatalf("second duplicate=%v err=%v", duplicate, err)
	}
}

func TestGatewayEventInboxPersistsAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload := []byte(`{"EventType":"GROUP_MESSAGE_CREATE"}`)
	pending, err := s.EnqueueGatewayEvent("event-1", payload, now, time.Hour, 2)
	if err != nil || !pending {
		t.Fatalf("first enqueue pending=%v err=%v", pending, err)
	}
	pending, err = s.EnqueueGatewayEvent("event-1", payload, now.Add(time.Minute), time.Hour, 2)
	if err != nil || !pending {
		t.Fatalf("duplicate pending enqueue=%v err=%v", pending, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, err := s.ListPendingGatewayEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "event-1" || string(items[0].Payload) != string(payload) {
		t.Fatalf("unexpected recovered inbox: %#v", items)
	}
	if err := s.CompleteGatewayEvent("event-1"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.EnqueueGatewayEvent("event-1", payload, now.Add(2*time.Minute), time.Hour, 2)
	if err != nil || pending {
		t.Fatalf("completed duplicate pending=%v err=%v", pending, err)
	}
}

func TestGatewayEventInboxIsBounded(t *testing.T) {
	s := openTestStore(t)
	if pending, err := s.EnqueueGatewayEvent("event-1", []byte(`{"id":1}`), time.Now(), time.Hour, 1); err != nil || !pending {
		t.Fatalf("first enqueue pending=%v err=%v", pending, err)
	}
	if _, err := s.EnqueueGatewayEvent("event-2", []byte(`{"id":2}`), time.Now(), time.Hour, 1); !errors.Is(err, ErrEventInboxFull) {
		t.Fatalf("expected ErrEventInboxFull, got %v", err)
	}
}

func TestPruneEphemeralBoundsTemporaryDataAndAudit(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	for _, pending := range []model.PendingBind{
		{CanonicalID: "expired", ExpiresAt: now.Add(-time.Minute)},
		{CanonicalID: "active", ExpiresAt: now.Add(time.Minute)},
	} {
		if err := s.PutPendingBind(pending); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutSentBotMessage(model.SentBotMessage{GroupOpenID: "old", MessageID: "old", SentAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSentBotMessage(model.SentBotMessage{GroupOpenID: "new", MessageID: "new", SentAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("audit"))
		for index := 1; index <= maxAuditRecords+5; index++ {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], uint64(index))
			data, _ := json.Marshal(model.AuditRecord{At: now, Action: "test"})
			if err := bucket.Put(key[:], data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneEphemeral(now, time.Hour, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPendingBind("expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired pending bind still exists: %v", err)
	}
	if _, err := s.GetPendingBind("active"); err != nil {
		t.Fatalf("active pending bind was removed: %v", err)
	}
	if _, err := s.GetSentBotMessage("old", "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired sent message still exists: %v", err)
	}
	if _, err := s.GetSentBotMessage("new", "new"); err != nil {
		t.Fatalf("recent sent message was removed: %v", err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		if count := tx.Bucket([]byte("audit")).Stats().KeyN; count != maxAuditRecords {
			t.Fatalf("audit records=%d, want %d", count, maxAuditRecords)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
