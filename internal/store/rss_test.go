package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/rss"
)

func rssTestResult(ids ...string) rss.Result {
	result := rss.Result{Feed: rss.Feed{Title: "Feed"}, Validators: rss.Validators{ETag: "v1"}}
	for i, id := range ids {
		result.Articles = append(result.Articles, rss.Article{
			ID: id, Title: id, PublishedAt: time.Unix(int64(i), 0),
		})
	}
	return result
}

func TestRSSBaselineDedupAndGroupIsolation(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	first, created, err := s.AddRSSSubscription("a", "https://example.com/rss", rssTestResult("old"), now)
	if err != nil || !created {
		t.Fatal(first, created, err)
	}
	duplicate, created, err := s.AddRSSSubscription("a", first.URL, rssTestResult("other"), now)
	if err != nil || created || duplicate.ID != first.ID {
		t.Fatal(duplicate, created, err)
	}
	other, created, err := s.AddRSSSubscription("b", first.URL, rssTestResult("old", "new"), now)
	if err != nil || !created || first.ID == other.ID {
		t.Fatal(other, created, err)
	}
	if _, count, err := s.RSSPending("a", 10); err != nil || count != 0 {
		t.Fatal("baseline queued history", count, err)
	}
	if _, err := s.RSSSubscription("b", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-group lookup", err)
	}
	result := rssTestResult("old", "new")
	for _, sub := range []rss.Subscription{first, other} {
		if err := s.RecordRSSPoll(sub, result, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	pending, count, err := s.RSSPending("a", 10)
	if err != nil || count != 1 || pending[0].Article.ID != "new" || pending[0].Version != first.Version {
		t.Fatal(pending, count, err)
	}
	if _, count, _ := s.RSSPending("b", 10); count != 0 {
		t.Fatal("groups shared baseline", count)
	}
	if err := s.RecordRSSPoll(first, result, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, count, _ := s.RSSPending("a", 10); count != 1 {
		t.Fatal("duplicate article queued", count)
	}
	if err := s.CompleteRSSDelivery(pending[0], true); err != nil {
		t.Fatal(err)
	}
	if _, count, _ := s.RSSPending("a", 10); count != 1 {
		t.Fatal("failed delivery lost", count)
	}
	if err := s.CompleteRSSDelivery(pending[0], false); err != nil {
		t.Fatal(err)
	}
	if current, err := s.RSSDeliveryCurrent(pending[0]); err != nil || current {
		t.Fatal(current, err)
	}
}

func TestRSSPauseResumeRejectsStaleFetchAndDelivery(t *testing.T) {
	s := openTestStore(t)
	sub, _, err := s.AddRSSSubscription("g", "https://example.com/rss", rssTestResult("old"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRSSPoll(sub, rssTestResult("old", "new"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	oldPending, _, _ := s.RSSPending("g", 10)
	if err := s.ChangeRSSSubscription("g", sub.ID, false, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, count, _ := s.RSSPending("g", 10); count != 0 {
		t.Fatal(count)
	}
	baseline := rssTestResult("old", "new", "paused-period")
	if err := s.ChangeRSSSubscription("g", sub.ID, false, &baseline, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRSSPoll(sub, rssTestResult("stale"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, _ := s.RSSSubscription("g", sub.ID)
	if err := s.RecordRSSPoll(current, rssTestResult("old", "new", "paused-period", "later"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	pending, count, _ := s.RSSPending("g", 10)
	if count != 1 || pending[0].Article.ID != "later" {
		t.Fatal(pending, count)
	}
	// A reset queue can reuse the same sequence, but not the same version.
	if enabled, err := s.RSSDeliveryCurrent(oldPending[0]); enabled || err != nil {
		t.Fatal("stale delivery allowed", enabled, err)
	}
	if err := s.CompleteRSSDelivery(oldPending[0], false); err != nil {
		t.Fatal(err)
	}
	if _, count, _ := s.RSSPending("g", 10); count != 1 {
		t.Fatal("stale ack deleted a newer queued article")
	}
	if err := s.ChangeRSSSubscription("g", sub.ID, true, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.RSSDeliveryCurrent(pending[0]); enabled || err != nil {
		t.Fatal(enabled, err)
	}
}

func TestRSSPollErrorAnd304KeepBaseline(t *testing.T) {
	s := openTestStore(t)
	sub, _, _ := s.AddRSSSubscription("g", "https://example.com/rss", rssTestResult("old"), time.Now())
	if err := s.RecordRSSPoll(sub, rss.Result{}, errors.New("safe failure"), time.Now()); err != nil {
		t.Fatal(err)
	}
	current, _ := s.RSSSubscription("g", sub.ID)
	if current.LastError != "safe failure" || current.ETag != "v1" {
		t.Fatal(current)
	}
	if err := s.RecordRSSPoll(current, rss.Result{NotModified: true, Validators: current.Validators}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRSSPoll(current, rssTestResult("old", "new"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if pending, count, _ := s.RSSPending("g", 10); count != 1 || pending[0].Article.ID != "new" {
		t.Fatal(pending, count)
	}
}

func TestRSSPersistsAcrossReopenAndBoundsPendingSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sub, _, err := s.AddRSSSubscription("g", "https://example.com/rss", rssTestResult(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for i := 0; i < 30; i++ {
		ids = append(ids, fmt.Sprintf("post-%d", i))
	}
	if err := s.RecordRSSPoll(sub, rssTestResult(ids...), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	cycle := time.Now().Truncate(time.Second)
	if err := s.SetRSSGroupSettings("g", time.Hour, cycle); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRSSSend("g", cycle); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, count, err := s.RSSPending("g", 10)
	if err != nil || count != 30 || len(pending) != 10 || pending[0].Article.ID != "post-0" || pending[9].Article.ID != "post-9" {
		t.Fatal(pending, count, err)
	}
	settings, err := s.RSSGroupSettings("g")
	if err != nil || settings.Interval != time.Hour || !settings.LastCycle.Equal(cycle) || !settings.LastSent.Equal(cycle) {
		t.Fatal(settings, err)
	}
	if err := s.SetRSSGroupSettings("g", 0, cycle.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	settings, _ = s.RSSGroupSettings("g")
	if settings.Interval != time.Hour || !settings.LastSent.Equal(cycle) {
		t.Fatal("scheduler overwrote group settings", settings)
	}
}
