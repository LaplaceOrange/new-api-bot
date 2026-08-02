package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
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
