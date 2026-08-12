package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	bolt "go.etcd.io/bbolt"
)

func resetTestSignal(id string, stage model.ResetStage, at time.Time) model.ResetSignal {
	return model.ResetSignal{
		ID:         id,
		Source:     "test",
		Stage:      stage,
		Summary:    "test signal",
		URL:        "https://example.com/signal",
		OccurredAt: at,
		DetectedAt: at,
	}
}

func TestResetSettingsDefaultsAndProxy(t *testing.T) {
	s := openTestStore(t)
	setting, err := s.GetOrCreateResetSettings("group-a")
	if err != nil {
		t.Fatal(err)
	}
	if setting.Duration != 5*time.Hour || setting.WinnerCount != 5 || setting.Lookback != 24*time.Hour || !setting.Subscribed {
		t.Fatalf("unexpected defaults: %#v", setting)
	}
	setting.Duration = 2 * time.Hour
	setting.WinnerCount = 3
	setting.Lookback = 12 * time.Hour
	if err := s.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	settings, err := s.ListSubscribedResetSettings()
	if err != nil || len(settings) != 1 || settings[0].WinnerCount != 3 {
		t.Fatalf("unexpected settings: %#v err=%v", settings, err)
	}
	if err := s.PutResetProxy("encrypted-value"); err != nil {
		t.Fatal(err)
	}
	if value, err := s.GetResetProxy(); err != nil || value != "encrypted-value" {
		t.Fatalf("proxy=%q err=%v", value, err)
	}
	if err := s.PutResetProxy(""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetResetProxy(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected removed proxy, got %v", err)
	}
}

func TestApplyResetSignalToGroupOnlyUpgradesStage(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	changed, err := s.ApplyResetSignalToGroup("group-a", resetTestSignal("possible", model.ResetStagePossible, now))
	if err != nil || !changed {
		t.Fatalf("possible changed=%v err=%v", changed, err)
	}
	changed, err = s.ApplyResetSignalToGroup("group-a", resetTestSignal("older-possible", model.ResetStagePossible, now.Add(time.Minute)))
	if err != nil || changed {
		t.Fatalf("same severity changed=%v err=%v", changed, err)
	}
	changed, err = s.ApplyResetSignalToGroup("group-a", resetTestSignal("imminent", model.ResetStageImminent, now.Add(2*time.Minute)))
	if err != nil || !changed {
		t.Fatalf("imminent changed=%v err=%v", changed, err)
	}
	changed, err = s.ApplyResetSignalToGroup("group-a", resetTestSignal("late-possible", model.ResetStagePossible, now.Add(3*time.Minute)))
	if err != nil || changed {
		t.Fatalf("downgrade changed=%v err=%v", changed, err)
	}
	state, err := s.GetResetGroupState("group-a")
	if err != nil || state.Stage != model.ResetStageImminent || state.SignalID != "imminent" {
		t.Fatalf("unexpected state: %#v err=%v", state, err)
	}
}

func TestApplyResetSignalDeliveryAllowsSameSignalUpgrade(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	signal := resetTestSignal("same-id", model.ResetStagePossible, now)
	changed, err := s.ApplyResetSignalToGroup("group-a", signal)
	if err != nil || !changed {
		t.Fatalf("possible changed=%v err=%v", changed, err)
	}
	if changed, err = s.ApplyResetSignalToGroup("group-a", signal); err != nil || changed {
		t.Fatalf("duplicate possible changed=%v err=%v", changed, err)
	}
	signal.Stage = model.ResetStageImminent
	signal.UpdatedAt = time.Time{}
	changed, err = s.ApplyResetSignalToGroup("group-a", signal)
	if err != nil || !changed {
		t.Fatalf("imminent upgrade changed=%v err=%v", changed, err)
	}
	activity, created, err := s.CreateResetActivityFromSignal("group-a", resetTestSignal("same-id", model.ResetStageConfirmed, now.Add(time.Minute)), now.Add(time.Minute))
	if err != nil || !created || activity.Status != model.ResetActivityActive {
		t.Fatalf("confirmed upgrade activity=%#v created=%v err=%v", activity, created, err)
	}
}

func TestExpireResetGroupStates(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	for group, signal := range map[string]model.ResetSignal{
		"old":    resetTestSignal("old", model.ResetStagePossible, now.Add(-2*time.Hour)),
		"recent": resetTestSignal("recent", model.ResetStageImminent, now.Add(-10*time.Minute)),
	} {
		if changed, err := s.ApplyResetSignalToGroup(group, signal); err != nil || !changed {
			t.Fatalf("apply %s changed=%v err=%v", group, changed, err)
		}
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("reset_group_states"))
		var state model.ResetGroupState
		if err := json.Unmarshal(bucket.Get([]byte("old")), &state); err != nil {
			return err
		}
		state.UpdatedAt = now.Add(-2 * time.Hour)
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return bucket.Put([]byte("old"), data)
	}); err != nil {
		t.Fatal(err)
	}
	activity, _, err := s.CreateResetActivityFromSignal("active", resetTestSignal("confirmed", model.ResetStageConfirmed, now.Add(-2*time.Hour)), now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := s.ExpireResetGroupStates(now.Add(-time.Hour), now)
	if err != nil || expired != 1 {
		t.Fatalf("expired=%d err=%v", expired, err)
	}
	oldState, _ := s.GetResetGroupState("old")
	recentState, _ := s.GetResetGroupState("recent")
	activeState, _ := s.GetResetGroupState("active")
	if oldState.Stage != model.ResetStageUnknown || recentState.Stage != model.ResetStageImminent || activeState.Stage != model.ResetStageConfirmed || activeState.ActivityID != activity.ID {
		t.Fatalf("states old=%#v recent=%#v active=%#v", oldState, recentState, activeState)
	}
}

func TestCreateResetActivityConcurrentAndJoinUnique(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	setting, err := s.GetOrCreateResetSettings("group-a")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Hour
	setting.WinnerCount = 2
	if err := s.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	signal := resetTestSignal("confirmed-1", model.ResetStageConfirmed, now)

	const attempts = 8
	var wg sync.WaitGroup
	created := make(chan bool, attempts)
	errs := make(chan error, attempts)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, err := s.CreateResetActivityFromSignal("group-a", signal, now)
			created <- wasCreated
			errs <- err
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	createdCount := 0
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for value := range created {
		if value {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count=%d, want 1", createdCount)
	}
	activity, err := s.GetActiveResetActivity("group-a")
	if err != nil || activity.ID != resetActivityID("group-a", signal.ID) {
		t.Fatalf("activity=%#v err=%v", activity, err)
	}

	participant := model.ResetParticipant{NewAPIID: 7, CanonicalID: "user:seven", MemberOpenID: "member-seven"}
	activity, joined, err := s.JoinResetActivity("group-a", participant, now.Add(time.Minute))
	if err != nil || !joined || activity.ParticipantCount != 1 {
		t.Fatalf("first join activity=%#v joined=%v err=%v", activity, joined, err)
	}
	activity, joined, err = s.JoinResetActivity("group-a", participant, now.Add(2*time.Minute))
	if err != nil || joined || activity.ParticipantCount != 1 {
		t.Fatalf("duplicate join activity=%#v joined=%v err=%v", activity, joined, err)
	}
	participants, err := s.ListResetParticipants(activity.ID)
	if err != nil || len(participants) != 1 || participants[0].NewAPIID != 7 {
		t.Fatalf("participants=%#v err=%v", participants, err)
	}
	state, err := s.GetResetGroupState("group-a")
	if err != nil || state.Stage != model.ResetStageConfirmed || state.ActivityID != activity.ID {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}

func TestResetSettlementPersistsAwardsAndCompletesToUnknown(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	setting, err := s.GetOrCreateResetSettings("group-a")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Hour
	setting.WinnerCount = 2
	if err := s.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, _, err := s.CreateResetActivityFromSignal("group-a", resetTestSignal("confirmed-1", model.ResetStageConfirmed, now), now)
	if err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 2; id++ {
		_, joined, err := s.JoinResetActivity("group-a", model.ResetParticipant{
			NewAPIID: id, CanonicalID: "user:" + string(rune('0'+id)),
		}, now.Add(time.Minute))
		if err != nil || !joined {
			t.Fatalf("join %d: joined=%v err=%v", id, joined, err)
		}
	}
	if due, err := s.ListDueResetActivities(now.Add(30 * time.Minute)); err != nil || len(due) != 0 {
		t.Fatalf("early due=%#v err=%v", due, err)
	}
	due, err := s.ListDueResetActivities(now.Add(time.Hour))
	if err != nil || len(due) != 1 || due[0].ID != activity.ID {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	activity, started, err := s.BeginResetSettlement(activity.ID, []model.ResetAward{
		{NewAPIID: 1, RawQuota: 250},
		{NewAPIID: 2, RawQuota: 0},
	}, now.Add(time.Hour))
	if err != nil || !started || activity.Status != model.ResetActivitySettling {
		t.Fatalf("settlement=%#v started=%v err=%v", activity, started, err)
	}
	activity, started, err = s.BeginResetSettlement(activity.ID, []model.ResetAward{{NewAPIID: 2}}, now.Add(2*time.Hour))
	if err != nil || started || len(activity.Awards) != 2 || activity.Awards[0].RawQuota != 250 {
		t.Fatalf("second settlement changed=%v activity=%#v err=%v", started, activity, err)
	}
	if due, err := s.ListDueResetActivities(now.Add(2 * time.Hour)); err != nil || len(due) != 1 || due[0].Status != model.ResetActivitySettling {
		t.Fatalf("settling activity missing from due index: %#v err=%v", due, err)
	}
	if _, err := s.UpdateResetAward(activity.ID, 1, model.ResetAwardGranted, "", now.Add(time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteResetActivity(activity.ID, now.Add(time.Hour+2*time.Minute))
	if err != nil || completed.Status != model.ResetActivityCompleted {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	state, err := s.GetResetGroupState("group-a")
	if err != nil || state.Stage != model.ResetStageUnknown || state.ActivityID != "" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	if _, err := s.GetActiveResetActivity("group-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no active activity, got %v", err)
	}
	if due, err := s.ListDueResetActivities(now.Add(3 * time.Hour)); err != nil || len(due) != 0 {
		t.Fatalf("completed activity remained due: %#v err=%v", due, err)
	}
}

func TestResetActivityRecoversAfterReopen(t *testing.T) {
	path := t.TempDir() + "/reset.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	activity, _, err := s.CreateResetActivityFromSignal("group-a", resetTestSignal("confirmed-1", model.ResetStageConfirmed, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JoinResetActivity("group-a", model.ResetParticipant{NewAPIID: 9, CanonicalID: "user:nine"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginResetSettlement(activity.ID, []model.ResetAward{{NewAPIID: 9, RawQuota: 123}}, activity.EndsAt); err != nil {
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
	due, err := s.ListDueResetActivities(activity.EndsAt.Add(time.Minute))
	if err != nil || len(due) != 1 || due[0].Awards[0].RawQuota != 123 || due[0].Awards[0].Status != model.ResetAwardPending {
		t.Fatalf("recovered due=%#v err=%v", due, err)
	}
}

func TestConfirmedSignalsAreConsumedAcrossActivityLifecycle(t *testing.T) {
	s := openTestStore(t)
	startedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	setting, err := s.GetOrCreateResetSettings("group-a")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Hour
	if err := s.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	firstSignal := resetTestSignal("confirmed-a", model.ResetStageConfirmed, startedAt)
	secondSignal := resetTestSignal("confirmed-b", model.ResetStageConfirmed, startedAt.Add(30*time.Minute))
	activity, created, err := s.CreateResetActivityFromSignal("group-a", firstSignal, startedAt)
	if err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	active, created, err := s.CreateResetActivityFromSignal("group-a", secondSignal, startedAt.Add(30*time.Minute))
	if err != nil || created || active.ID != activity.ID {
		t.Fatalf("second while active activity=%#v created=%v err=%v", active, created, err)
	}
	if _, _, err := s.BeginResetSettlement(activity.ID, nil, activity.EndsAt); err != nil {
		t.Fatal(err)
	}
	completedAt := activity.EndsAt.Add(time.Minute)
	if _, err := s.CompleteResetActivity(activity.ID, completedAt); err != nil {
		t.Fatal(err)
	}
	for _, signal := range []model.ResetSignal{firstSignal, secondSignal} {
		if got, created, err := s.CreateResetActivityFromSignal("group-a", signal, completedAt.Add(time.Minute)); err != nil || created || got.ID != activity.ID {
			t.Fatalf("replayed %s activity=%#v created=%v err=%v", signal.ID, got, created, err)
		}
	}
	lateOldSignal := resetTestSignal("confirmed-late-old", model.ResetStageConfirmed, completedAt.Add(-time.Second))
	if got, created, err := s.CreateResetActivityFromSignal("group-a", lateOldSignal, completedAt.Add(2*time.Minute)); err != nil || created || got.ID != "" {
		t.Fatalf("late old activity=%#v created=%v err=%v", got, created, err)
	}
	newSignal := resetTestSignal("confirmed-new", model.ResetStageConfirmed, completedAt.Add(time.Minute))
	newActivity, created, err := s.CreateResetActivityFromSignal("group-a", newSignal, completedAt.Add(2*time.Minute))
	if err != nil || !created || newActivity.SignalID != newSignal.ID {
		t.Fatalf("new activity=%#v created=%v err=%v", newActivity, created, err)
	}
}

func TestResetNotificationsAreEnqueuedDeterministically(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	possible := resetTestSignal("possible-notification", model.ResetStagePossible, now)
	if changed, err := s.ApplyResetSignalToGroup("group-a", possible); err != nil || !changed {
		t.Fatalf("apply possible changed=%v err=%v", changed, err)
	}
	if changed, err := s.ApplyResetSignalToGroup("group-a", possible); err != nil || changed {
		t.Fatalf("duplicate possible changed=%v err=%v", changed, err)
	}
	imminent := resetTestSignal("imminent-notification", model.ResetStageImminent, now.Add(time.Minute))
	if changed, err := s.ApplyResetSignalToGroup("group-a", imminent); err != nil || !changed {
		t.Fatalf("apply imminent changed=%v err=%v", changed, err)
	}
	confirmed := resetTestSignal("confirmed-notification", model.ResetStageConfirmed, now.Add(2*time.Minute))
	activity, created, err := s.CreateResetActivityFromSignal("group-a", confirmed, now.Add(2*time.Minute))
	if err != nil || !created {
		t.Fatalf("create activity=%#v created=%v err=%v", activity, created, err)
	}
	if _, created, err := s.CreateResetActivityFromSignal("group-a", confirmed, now.Add(3*time.Minute)); err != nil || created {
		t.Fatalf("duplicate activity created=%v err=%v", created, err)
	}
	if _, _, err := s.BeginResetSettlement(activity.ID, nil, activity.EndsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteResetActivity(activity.ID, activity.EndsAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteResetActivity(activity.ID, activity.EndsAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	notifications, err := s.ListDueResetNotifications(activity.EndsAt.Add(3*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 4 {
		t.Fatalf("notifications=%#v, want 4", notifications)
	}
	kinds := make(map[model.ResetNotificationKind]int)
	ids := make(map[string]struct{})
	for _, notification := range notifications {
		kinds[notification.Kind]++
		ids[notification.ID] = struct{}{}
		if notification.Status != model.ResetNotificationPending || notification.GroupOpenID != "group-a" {
			t.Fatalf("unexpected notification: %#v", notification)
		}
		if notification.SignalSource != "test" || notification.SignalSummary != "test signal" || notification.SignalURL != "https://example.com/signal" {
			t.Fatalf("signal snapshot was not persisted: %#v", notification)
		}
	}
	if len(ids) != 4 || kinds[model.ResetNotificationSignal] != 2 || kinds[model.ResetNotificationActivityStarted] != 1 || kinds[model.ResetNotificationActivityCompleted] != 1 {
		t.Fatalf("ids=%d kinds=%#v", len(ids), kinds)
	}
	stored, err := s.GetResetSignal(confirmed.ID)
	if err != nil || stored.Stage != model.ResetStageConfirmed {
		t.Fatalf("signal=%#v err=%v", stored, err)
	}
}

func TestResetNotificationFailureRetryAndSent(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if changed, err := s.ApplyResetSignalToGroup("group-a", resetTestSignal("retry-signal", model.ResetStagePossible, now)); err != nil || !changed {
		t.Fatalf("apply changed=%v err=%v", changed, err)
	}
	due, err := s.ListDueResetNotifications(now.Add(time.Minute), 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	retryAt := now.Add(10 * time.Minute)
	failedAt := now.Add(2 * time.Minute)
	if err := s.MarkResetNotificationFailed(due[0].ID, "temporary failure", retryAt, failedAt); err != nil {
		t.Fatal(err)
	}
	if early, err := s.ListDueResetNotifications(retryAt.Add(-time.Nanosecond), 10); err != nil || len(early) != 0 {
		t.Fatalf("early=%#v err=%v", early, err)
	}
	due, err = s.ListDueResetNotifications(retryAt, 10)
	if err != nil || len(due) != 1 || due[0].Attempts != 1 || due[0].LastError != "temporary failure" || !due[0].NextAttemptAt.Equal(retryAt) {
		t.Fatalf("retried due=%#v err=%v", due, err)
	}
	if err := s.MarkResetNotificationSent(due[0].ID, retryAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if remaining, err := s.ListDueResetNotifications(retryAt.Add(time.Hour), 10); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining=%#v err=%v", remaining, err)
	}
	if err := s.MarkResetNotificationFailed(due[0].ID, "late failure", retryAt.Add(2*time.Hour), retryAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if remaining, err := s.ListDueResetNotifications(retryAt.Add(3*time.Hour), 10); err != nil || len(remaining) != 0 {
		t.Fatalf("sent notification was requeued: %#v err=%v", remaining, err)
	}
}

func TestResetNotificationFrozenChunksPersistProgress(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if changed, err := s.ApplyResetSignalToGroup("group-a", resetTestSignal("chunk-progress", model.ResetStagePossible, now)); err != nil || !changed {
		t.Fatalf("apply changed=%v err=%v", changed, err)
	}
	due, err := s.ListDueResetNotifications(time.Now().Add(time.Second), 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	frozen := []string{"first", "second"}
	prepared, err := s.PrepareResetNotification(due[0].ID, frozen, now)
	if err != nil || len(prepared.Chunks) != 2 || prepared.NextChunk != 0 {
		t.Fatalf("prepared=%#v err=%v", prepared, err)
	}
	preparedAgain, err := s.PrepareResetNotification(due[0].ID, []string{"changed"}, now.Add(time.Second))
	if err != nil || len(preparedAgain.Chunks) != 2 || preparedAgain.Chunks[0] != "first" {
		t.Fatalf("frozen content changed: %#v err=%v", preparedAgain, err)
	}
	progress, err := s.MarkResetNotificationChunkSent(due[0].ID, 0, now.Add(2*time.Second))
	if err != nil || progress.NextChunk != 1 || progress.Status != model.ResetNotificationPending {
		t.Fatalf("first progress=%#v err=%v", progress, err)
	}
	if current, err := s.ListDueResetNotifications(now.Add(2*time.Second), 1); err != nil || len(current) != 1 || current[0].NextChunk != 1 {
		t.Fatalf("persisted progress=%#v err=%v", current, err)
	}
	progress, err = s.MarkResetNotificationChunkSent(due[0].ID, 1, now.Add(3*time.Second))
	if err != nil || progress.NextChunk != 2 || progress.Status != model.ResetNotificationSent || progress.SentAt.IsZero() {
		t.Fatalf("final progress=%#v err=%v", progress, err)
	}
	if remaining, err := s.ListDueResetNotifications(now.Add(time.Hour), 1); err != nil || len(remaining) != 0 {
		t.Fatalf("sent notification remained due: %#v err=%v", remaining, err)
	}
}

func TestResetNotificationCanBeSuperseded(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if changed, err := s.ApplyResetSignalToGroup("group-a", resetTestSignal("superseded", model.ResetStagePossible, now)); err != nil || !changed {
		t.Fatalf("apply changed=%v err=%v", changed, err)
	}
	due, err := s.ListDueResetNotifications(time.Now().Add(time.Second), 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	if err := s.MarkResetNotificationSuperseded(due[0].ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if remaining, err := s.ListDueResetNotifications(now.Add(time.Hour), 1); err != nil || len(remaining) != 0 {
		t.Fatalf("superseded notification remained due: %#v err=%v", remaining, err)
	}
}

func TestResetNotificationEnqueueRollsBackState(t *testing.T) {
	s := openTestStore(t)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket([]byte("reset_notification_due"))
	}); err != nil {
		t.Fatal(err)
	}
	signal := resetTestSignal("atomic-signal", model.ResetStagePossible, time.Now())
	if changed, err := s.ApplyResetSignalToGroup("group-a", signal); err == nil || changed {
		t.Fatalf("apply changed=%v err=%v, want rolled back error", changed, err)
	}
	state, err := s.GetResetGroupState("group-a")
	if err != nil || state.Stage != model.ResetStageUnknown {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	if _, err := s.GetResetSignal(signal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("signal should have rolled back, got %v", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket([]byte("reset_notification_due"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ApplyResetSignalToGroup("group-a", signal); err != nil || !changed {
		t.Fatalf("apply after repair changed=%v err=%v", changed, err)
	}
	if due, err := s.ListDueResetNotifications(time.Now().Add(time.Minute), 10); err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
}

func TestResetIndexesRecoverAfterReopen(t *testing.T) {
	path := t.TempDir() + "/reset-index.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	setting, err := s.GetOrCreateResetSettings("group-a")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Hour
	if err := s.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, created, err := s.CreateResetActivityFromSignal("group-a", resetTestSignal("reopen-confirmed", model.ResetStageConfirmed, now), now)
	if err != nil || !created {
		t.Fatalf("activity=%#v created=%v err=%v", activity, created, err)
	}
	notifications, err := s.ListDueResetNotifications(now.Add(time.Minute), 10)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("notifications=%#v err=%v", notifications, err)
	}
	retryAt := now.Add(30 * time.Minute)
	if err := s.MarkResetNotificationFailed(notifications[0].ID, "retry after restart", retryAt, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if err := clearBucketTx(tx.Bucket([]byte("reset_due_activities"))); err != nil {
			return err
		}
		return clearBucketTx(tx.Bucket([]byte("reset_notification_due")))
	}); err != nil {
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
	if early, err := s.ListDueResetNotifications(retryAt.Add(-time.Nanosecond), 10); err != nil || len(early) != 0 {
		t.Fatalf("early notifications=%#v err=%v", early, err)
	}
	if recovered, err := s.ListDueResetNotifications(retryAt, 10); err != nil || len(recovered) != 1 || recovered[0].Attempts != 1 {
		t.Fatalf("recovered notifications=%#v err=%v", recovered, err)
	}
	if due, err := s.ListDueResetActivities(activity.EndsAt.Add(-time.Nanosecond)); err != nil || len(due) != 0 {
		t.Fatalf("early activities=%#v err=%v", due, err)
	}
	if due, err := s.ListDueResetActivities(activity.EndsAt); err != nil || len(due) != 1 || due[0].ID != activity.ID {
		t.Fatalf("recovered activities=%#v err=%v", due, err)
	}
}

func TestResetNotificationProgressRecoversAfterReopen(t *testing.T) {
	path := t.TempDir() + "/reset-notification-progress.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if changed, err := s.ApplyResetSignalToGroup("group-a", resetTestSignal("reopen-progress", model.ResetStagePossible, now)); err != nil || !changed {
		t.Fatalf("apply changed=%v err=%v", changed, err)
	}
	due, err := s.ListDueResetNotifications(time.Now().Add(time.Second), 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	if _, err := s.PrepareResetNotification(due[0].ID, []string{"first", "second"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkResetNotificationChunkSent(due[0].ID, 0, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(time.Minute)
	if err := s.MarkResetNotificationFailed(due[0].ID, "second failed", retryAt, now.Add(2*time.Second)); err != nil {
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
	if early, err := s.ListDueResetNotifications(retryAt.Add(-time.Nanosecond), 1); err != nil || len(early) != 0 {
		t.Fatalf("early=%#v err=%v", early, err)
	}
	recovered, err := s.ListDueResetNotifications(retryAt, 1)
	if err != nil || len(recovered) != 1 || recovered[0].NextChunk != 1 || recovered[0].Attempts != 1 || len(recovered[0].Chunks) != 2 || recovered[0].Chunks[1] != "second" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
}

func TestListDueResetActivitiesStopsAtDueIndexCutoff(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	setting, err := s.GetOrCreateResetSettings("group-a")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Hour
	if err := s.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, created, err := s.CreateResetActivityFromSignal("group-a", resetTestSignal("future-confirmed", model.ResetStageConfirmed, now), now)
	if err != nil || !created {
		t.Fatalf("activity=%#v created=%v err=%v", activity, created, err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("reset_activities")).Put([]byte("corrupt-history"), []byte("not-json"))
	}); err != nil {
		t.Fatal(err)
	}
	if due, err := s.ListDueResetActivities(now); err != nil || len(due) != 0 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
}
