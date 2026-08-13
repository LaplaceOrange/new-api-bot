package bot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/resetradar"
	"github.com/fsykk/new-api-bot/internal/store"
)

type fakeResetRadar struct {
	signal      resetradar.Signal
	snapshot    resetradar.Snapshot
	fetchErr    error
	err         error
	latestCalls int
	fetchCalls  int
	lastProxy   string
	fetchProxy  string
}

func (f *fakeResetRadar) Fetch(_ context.Context, proxyURL string) (resetradar.Snapshot, error) {
	f.fetchCalls++
	f.fetchProxy = proxyURL
	return f.snapshot, f.fetchErr
}

func (f *fakeResetRadar) Latest(_ context.Context, proxyURL string) (resetradar.Signal, error) {
	f.latestCalls++
	f.lastProxy = proxyURL
	return f.signal, f.err
}

func (f *fakeResetRadar) Close() {}

func TestResetLastIsAvailableWithoutBindingAndHasNoStateSideEffects(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.CheckinTimezone = time.FixedZone("CST", 8*60*60)
	radar := &fakeResetRadar{signal: resetradar.Signal{
		ID:        "x:123456789",
		Source:    "codex-reset.com",
		Text:      "A regular unrelated post.",
		URL:       "https://x.com/thsottiaux/status/123456789",
		CreatedAt: time.Date(2026, 8, 13, 1, 23, 45, 0, time.UTC),
		Stage:     resetradar.StageUnknown,
	}}
	service.resetRadar = radar

	service.process(context.Background(), groupEvent("g-reset-last", "ordinary", "/reset last"))
	reply := lastReply(t, qqAPI)
	for _, want := range []string{
		"Codex Reset API 最新重置事件",
		"公告时间：2026-08-13 09:23:45 CST",
		"机器人状态：未知",
		"API 判定：未知",
		"摘要：A regular unrelated post.",
		"https://x.com/thsottiaux/status/123456789",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("latest reply %q does not contain %q", reply, want)
		}
	}
	if radar.latestCalls != 1 || radar.lastProxy != "" {
		t.Fatalf("latest calls=%d proxy=%q", radar.latestCalls, radar.lastProxy)
	}
	if _, err := storage.GetResetSettings("g-reset-last"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("/reset last unexpectedly subscribed group: %v", err)
	}
	if _, err := storage.GetActiveResetActivity("g-reset-last"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("/reset last unexpectedly created activity: %v", err)
	}
}

func TestResetLastStrictSyntaxAndGroupOnly(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	radar := &fakeResetRadar{}
	service.resetRadar = radar

	service.process(context.Background(), groupEvent("g-reset-last", "ordinary", "/reset last extra"))
	if reply := lastReply(t, qqAPI); reply != "格式错误。正确用法：/reset last" {
		t.Fatalf("unexpected syntax reply: %q", reply)
	}
	service.process(context.Background(), c2cEvent("ordinary", "/reset last"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "只能在群聊中使用") {
		t.Fatalf("unexpected c2c reply: %q", reply)
	}
	if radar.latestCalls != 0 {
		t.Fatalf("invalid /reset last called radar %d times", radar.latestCalls)
	}
	if _, err := storage.GetResetSettings("g-reset-last"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid /reset last unexpectedly subscribed group: %v", err)
	}
}

func TestResetLastStageLabels(t *testing.T) {
	tests := []struct {
		stage resetradar.Stage
		want  string
	}{
		{stage: resetradar.StageUnknown, want: "未知"},
		{stage: resetradar.StagePossible, want: "可能重置"},
		{stage: resetradar.StageImminent, want: "即将重置"},
		{stage: resetradar.StageConfirmed, want: "确认重置"},
	}
	for _, test := range tests {
		if got := latestResetStageText(test.stage); got != test.want {
			t.Fatalf("latestResetStageText(%v)=%q, want %q", test.stage, got, test.want)
		}
	}
	if got := latestResetStageText(resetradar.StageConfirmed); strings.Contains(got, "抽奖进行中") {
		t.Fatalf("latest confirmed label used activity wording: %q", got)
	}
}

func TestResetRadarStatusTextUsesAPIVerification(t *testing.T) {
	tests := []struct {
		name   string
		signal resetradar.Signal
		want   string
	}{
		{name: "confirmed observation", signal: resetradar.Signal{ObservationResult: "reset_observed"}, want: "confirmed"},
		{name: "rejected observation wins", signal: resetradar.Signal{ObservationResult: "unchanged", VerificationStatus: "confirmed", Stage: resetradar.StageConfirmed}, want: "rejected"},
		{name: "pending", signal: resetradar.Signal{VerificationStatus: "pending", Stage: resetradar.StagePossible}, want: "pending"},
		{name: "archive confirmed", signal: resetradar.Signal{Stage: resetradar.StageConfirmed}, want: "confirmed"},
		{name: "hinted", signal: resetradar.Signal{AnnouncementState: "hinted", Stage: resetradar.StagePossible}, want: "hinted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizedResetRadarStatus(test.signal); got != test.want {
				t.Fatalf("status=%q, want %q", got, test.want)
			}
		})
	}
}

func TestResetLastConfirmedResultDoesNotAdvanceGroupStateOrCreateActivity(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if _, err := service.ensureResetSettings("g-reset-last"); err != nil {
		t.Fatal(err)
	}
	before := model.ResetSignal{
		ID:         "possible-before-last",
		Source:     "test",
		Stage:      model.ResetStagePossible,
		Summary:    "existing group state",
		OccurredAt: time.Now(),
	}
	if changed, err := storage.ApplyResetSignalToGroup("g-reset-last", before); err != nil || !changed {
		t.Fatalf("prepare group state: changed=%v err=%v", changed, err)
	}
	service.resetRadar = &fakeResetRadar{signal: resetradar.Signal{
		ID:        "x:confirmed-last",
		Text:      "confirmed reset post",
		CreatedAt: time.Now(),
		Stage:     resetradar.StageConfirmed,
	}}

	service.process(context.Background(), groupEvent("g-reset-last", "ordinary", "/reset last"))
	reply := lastReply(t, qqAPI)
	if !strings.Contains(reply, "机器人状态：确认重置") || strings.Contains(reply, "抽奖进行中") {
		t.Fatalf("unexpected confirmed latest reply: %q", reply)
	}
	state, err := storage.GetResetGroupState("g-reset-last")
	if err != nil || state.Stage != model.ResetStagePossible || state.SignalID != before.ID {
		t.Fatalf("latest query changed group state: %#v err=%v", state, err)
	}
	if _, err := storage.GetActiveResetActivity("g-reset-last"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("latest query created reset activity: %v", err)
	}
}

func TestResetLastUsesEncryptedProxyAndRedactsFailure(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	const proxyURL = "http://proxy-user:proxy-secret@127.0.0.1:8080"
	encrypted, err := service.secure.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.PutResetProxy(encrypted); err != nil {
		t.Fatal(err)
	}
	radar := &fakeResetRadar{err: errors.New("dial " + proxyURL + ": connection refused")}
	service.resetRadar = radar
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewTextHandler(&logs, nil))

	service.process(context.Background(), groupEvent("g-reset-last", "ordinary", "/reset last"))
	reply := lastReply(t, qqAPI)
	if !strings.Contains(reply, "获取 Codex Reset API 最新事件失败") || strings.Contains(reply, "proxy-user") || strings.Contains(reply, "proxy-secret") {
		t.Fatalf("unexpected failure reply: %q", reply)
	}
	if radar.latestCalls != 1 || radar.lastProxy != proxyURL {
		t.Fatalf("latest calls=%d proxy=%q", radar.latestCalls, radar.lastProxy)
	}
	logged := logs.String()
	if strings.Contains(logged, "proxy-user") || strings.Contains(logged, "proxy-secret") {
		t.Fatalf("proxy credentials leaked to logs: %q", logged)
	}
	if !strings.Contains(logged, "connection refused") || !strings.Contains(logged, "127.0.0.1:8080") {
		t.Fatalf("redacted log omitted useful error details: %q", logged)
	}
}

func TestResetLastDecryptFailureDoesNotCallRadar(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.PutResetProxy("not-valid-ciphertext"); err != nil {
		t.Fatal(err)
	}
	radar := &fakeResetRadar{}
	service.resetRadar = radar

	service.process(context.Background(), groupEvent("g-reset-last", "ordinary", "/reset last"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "读取重置检测代理配置失败") {
		t.Fatalf("unexpected decrypt failure reply: %q", reply)
	}
	if radar.latestCalls != 0 {
		t.Fatalf("radar called despite proxy decrypt failure: %d", radar.latestCalls)
	}
}

func TestResetCheckIsAvailableWithoutBindingAndSubscribesGroup(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}

	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/reset check"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "当前重置状态：未知") {
		t.Fatalf("unexpected reset status reply: %q", reply)
	}
	setting, err := storage.GetResetSettings("g-reset")
	if err != nil || !setting.Subscribed {
		t.Fatalf("reset group was not subscribed: %#v err=%v", setting, err)
	}
}

func TestResetJoinRequiresBindingAndActiveActivity(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g-reset", "u1", "/reset join"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "尚未绑定") {
		t.Fatalf("unexpected unbound reset join reply: %q", reply)
	}

	createBinding(t, storage, "member:g-reset:u1", 42)
	service.process(context.Background(), groupEvent("g-reset", "u1", "/reset join"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "当前没有正在进行") {
		t.Fatalf("unexpected inactive reset join reply: %q", reply)
	}
}

func TestResetJoinAndCheckConfirmedActivity(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	createBinding(t, storage, "member:g-reset:u1", 42)
	if _, err := service.ensureResetSettings("g-reset"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	activity, created, err := storage.CreateResetActivityFromSignal("g-reset", model.ResetSignal{
		ID:         "x:confirmed-test",
		Source:     "test",
		Stage:      model.ResetStageConfirmed,
		Summary:    "Codex usage limits have been reset.",
		OccurredAt: now,
	}, now)
	if err != nil || !created {
		t.Fatalf("create activity: %#v created=%v err=%v", activity, created, err)
	}

	service.process(context.Background(), groupEvent("g-reset", "u1", "/reset join"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "参加成功") || !strings.Contains(reply, "当前共有 1 人") {
		t.Fatalf("unexpected reset join reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g-reset", "u1", "/reset join"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已经参加") {
		t.Fatalf("unexpected duplicate reset join reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/reset check"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "确认重置（抽奖进行中）") || !strings.Contains(reply, "已参加：1 人") {
		t.Fatalf("unexpected confirmed reset status reply: %q", reply)
	}
}

func TestResetNewRequiresBoundAdminWithoutSideEffects(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}

	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/reset new"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "尚未绑定") {
		t.Fatalf("unexpected unbound non-admin reply: %q", reply)
	}
	if _, err := storage.GetResetSettings("g-reset"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unbound reset new changed group settings: %v", err)
	}

	createBinding(t, storage, "member:g-reset:ordinary", 42)
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/reset new"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "没有手动开启") {
		t.Fatalf("unexpected non-admin reply: %q", reply)
	}
	if _, err := storage.GetResetSettings("g-reset"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-admin reset new subscribed group: %v", err)
	}

	service.process(context.Background(), groupEvent("g-reset", "admin", "/reset new"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "尚未绑定") {
		t.Fatalf("unexpected unbound admin reply: %q", reply)
	}
}

func TestResetNewUsesCurrentSettingsAndEnqueuesManualAnnouncement(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	createBinding(t, storage, "member:g-reset:admin", 43)
	setting, err := service.ensureResetSettings("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = 90 * time.Minute
	setting.WinnerCount = 7
	setting.Lookback = 36 * time.Hour
	if err := storage.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}

	event := groupEvent("g-reset", "admin", "/reset new")
	event.Message.ID = "manual-new-1"
	service.process(context.Background(), event)
	reply := lastReply(t, qqAPI)
	for _, want := range []string{"已手动开启", "活动有效期：1h30m0s", "抽取人数：最多 7 人", "近 36h"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("manual create reply %q does not contain %q", reply, want)
		}
	}
	activity, err := storage.GetActiveResetActivity("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	if activity.SignalID != manualResetSignalID("g-reset", event.Message.ID) || activity.WinnerCount != 7 || activity.Lookback != 36*time.Hour || activity.EndsAt.Sub(activity.StartedAt) != 90*time.Minute {
		t.Fatalf("manual activity did not freeze settings: %#v", activity)
	}
	state, err := storage.GetResetGroupState("g-reset")
	if err != nil || state.Stage != model.ResetStageConfirmed || state.ActivityID != activity.ID || state.Source != manualResetSource {
		t.Fatalf("manual group state=%#v err=%v", state, err)
	}
	due, err := storage.ListDueResetNotifications(time.Now().Add(time.Minute), 10)
	if err != nil || len(due) != 1 || due[0].Kind != model.ResetNotificationActivityStarted || due[0].ActivityID != activity.ID {
		t.Fatalf("manual notification=%#v err=%v", due, err)
	}
	message, err := service.renderResetNotification(context.Background(), due[0])
	if err != nil || !strings.Contains(message, "管理员已手动开启") || strings.Contains(message, "已确认 Codex") {
		t.Fatalf("manual announcement=%q err=%v", message, err)
	}
}

func TestResetNewIsIdempotentAndRejectsConcurrentActivity(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	createBinding(t, storage, "member:g-reset:admin", 43)

	first := groupEvent("g-reset", "admin", "/reset new")
	first.Message.ID = "manual-new-replayed"
	service.process(context.Background(), first)
	activity, err := storage.GetActiveResetActivity("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), first)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "未重复开启") || !strings.Contains(reply, activity.ID) {
		t.Fatalf("unexpected replay reply: %q", reply)
	}

	second := groupEvent("g-reset", "admin", "/reset new")
	second.Message.ID = "manual-new-concurrent"
	service.process(context.Background(), second)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "未重复开启") || !strings.Contains(reply, activity.ID) {
		t.Fatalf("unexpected active activity reply: %q", reply)
	}
	current, err := storage.GetActiveResetActivity("g-reset")
	if err != nil || current.ID != activity.ID {
		t.Fatalf("duplicate reset new changed activity: %#v err=%v", current, err)
	}
	due, err := storage.ListDueResetNotifications(time.Now().Add(time.Minute), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("duplicate reset new enqueued notifications: %#v err=%v", due, err)
	}
}

func TestResetNewValidatesSyntaxAndMessageIDBeforeCreating(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	createBinding(t, storage, "member:g-reset:admin", 43)

	service.process(context.Background(), groupEvent("g-reset", "admin", "/reset new extra"))
	if reply := lastReply(t, qqAPI); reply != "格式错误。正确用法：/reset new" {
		t.Fatalf("unexpected reset new syntax reply: %q", reply)
	}
	missingID := groupEvent("g-reset", "admin", "/reset new")
	missingID.Message.ID = ""
	service.process(context.Background(), missingID)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "消息缺少唯一标识") {
		t.Fatalf("unexpected missing message ID reply: %q", reply)
	}
	if _, err := storage.GetActiveResetActivity("g-reset"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid reset new created activity: %v", err)
	}
	if _, err := storage.GetResetSettings("g-reset"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid reset new changed group settings: %v", err)
	}
}

func TestResetCheckShowsPossibleAndImminentStages(t *testing.T) {
	tests := []struct {
		name  string
		stage model.ResetStage
		want  string
	}{
		{name: "possible", stage: model.ResetStagePossible, want: "当前重置状态：可能重置"},
		{name: "imminent", stage: model.ResetStageImminent, want: "当前重置状态：即将重置"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, storage, _, qqAPI, _ := testService(t)
			if _, err := service.ensureResetSettings("g-reset"); err != nil {
				t.Fatal(err)
			}
			changed, err := storage.ApplyResetSignalToGroup("g-reset", model.ResetSignal{
				ID:         "signal-" + test.name,
				Source:     "test",
				Stage:      test.stage,
				Summary:    "stage test",
				OccurredAt: time.Now(),
			})
			if err != nil || !changed {
				t.Fatalf("apply reset signal: changed=%v err=%v", changed, err)
			}

			service.process(context.Background(), groupEvent("g-reset", "ordinary", "/reset check"))
			if reply := lastReply(t, qqAPI); !strings.Contains(reply, test.want) {
				t.Fatalf("unexpected reset status reply: %q", reply)
			}
		})
	}
}

func TestPollResetTimelineAppliesLatestRejectedStateWithoutActivity(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if _, err := service.ensureResetSettings("g-reset"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service.resetRadar = &fakeResetRadar{snapshot: resetradar.Snapshot{
		CheckedAt: now,
		Signals: []resetradar.Signal{
			{
				ID:                 "x:pending-event",
				Source:             "codex-reset.com",
				Text:               "A possible reset tomorrow.",
				CreatedAt:          now.Add(-2 * time.Hour),
				Stage:              resetradar.StagePossible,
				VerificationStatus: "pending",
			},
			{
				ID:                 "x:rejected-event",
				Source:             "codex-reset.com",
				Text:               "Reset announcement was not observed.",
				CreatedAt:          now.Add(-time.Hour),
				Stage:              resetradar.StageUnknown,
				ObservationResult:  "unchanged",
				VerificationStatus: "rejected",
			},
		},
	}}

	service.pollResetSignals(context.Background())
	state, err := storage.GetResetGroupState("g-reset")
	if err != nil || state.Stage != model.ResetStageUnknown || state.SignalID != "x:rejected-event" {
		t.Fatalf("latest rejected state=%#v err=%v", state, err)
	}
	if _, err := storage.GetActiveResetActivity("g-reset"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected timeline event created activity: %v", err)
	}
	service.processDueResetNotificationsAt(context.Background(), time.Now().Add(time.Minute))
	if len(qqAPI.messages) != 2 || !strings.Contains(qqAPI.messages[0], "可能重置") || !strings.Contains(qqAPI.messages[1], "标记为未确认") {
		t.Fatalf("timeline notifications=%#v", qqAPI.messages)
	}
}

func TestConfirmedTimelineEventCreatesOneActivity(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	if _, err := service.ensureResetSettings("g-reset"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	radar := &fakeResetRadar{snapshot: resetradar.Snapshot{CheckedAt: now, Signals: []resetradar.Signal{{
		ID:                 "x:confirmed-event",
		Source:             "codex-reset.com",
		Text:               "Reset observed.",
		CreatedAt:          now.Add(-time.Minute),
		Stage:              resetradar.StageConfirmed,
		ObservationResult:  "reset_observed",
		VerificationStatus: "confirmed",
	}}}}
	service.resetRadar = radar

	service.pollResetSignals(context.Background())
	activity, err := storage.GetActiveResetActivity("g-reset")
	if err != nil || activity.SignalID != "x:confirmed-event" {
		t.Fatalf("activity=%#v err=%v", activity, err)
	}
	service.pollResetSignals(context.Background())
	current, err := storage.GetActiveResetActivity("g-reset")
	if err != nil || current.ID != activity.ID {
		t.Fatalf("replayed API response changed activity: %#v err=%v", current, err)
	}
	due, err := storage.ListDueResetNotifications(time.Now().Add(time.Minute), 10)
	if err != nil || len(due) != 1 || due[0].Kind != model.ResetNotificationActivityStarted {
		t.Fatalf("confirmed notifications=%#v err=%v", due, err)
	}
}

func TestResetNotificationOutboxRetriesAfterQQFailure(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	now := time.Now()
	signal := model.ResetSignal{
		ID:         "x:outbox-retry",
		Source:     "test",
		Stage:      model.ResetStagePossible,
		Summary:    "possible reset",
		OccurredAt: now,
		DetectedAt: now,
		UpdatedAt:  now,
	}
	service.processResetSignalForGroup(context.Background(), "g-reset", signal)
	if replyCount(qqAPI) != 0 {
		t.Fatal("reset signal bypassed the notification outbox")
	}

	qqAPI.groupReplyErr = errors.New("temporary QQ failure")
	service.processDueResetNotificationsAt(context.Background(), now)
	if qqAPI.groupReplies != 1 || replyCount(qqAPI) != 0 {
		t.Fatalf("failed delivery calls=%d replies=%d", qqAPI.groupReplies, replyCount(qqAPI))
	}
	if due, err := storage.ListDueResetNotifications(now.Add(29*time.Second), 10); err != nil || len(due) != 0 {
		t.Fatalf("notification retried too early: due=%#v err=%v", due, err)
	}

	qqAPI.groupReplyErr = nil
	service.processDueResetNotificationsAt(context.Background(), now.Add(30*time.Second))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "检测到可能重置信号") {
		t.Fatalf("unexpected recovered notification: %q", reply)
	}
	service.processDueResetNotificationsAt(context.Background(), now.Add(time.Hour))
	if qqAPI.groupReplies != 2 || replyCount(qqAPI) != 1 {
		t.Fatalf("sent notification was delivered again: calls=%d replies=%d", qqAPI.groupReplies, replyCount(qqAPI))
	}
}

func TestResetNotificationOutboxResumesAtFailedChunk(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	now := time.Now()
	signal := model.ResetSignal{
		ID:         "x:outbox-chunks",
		Source:     "test",
		Stage:      model.ResetStagePossible,
		Summary:    "possible reset",
		OccurredAt: now,
		DetectedAt: now,
		UpdatedAt:  now,
	}
	service.processResetSignalForGroup(context.Background(), "g-reset", signal)
	due, err := storage.ListDueResetNotifications(now, 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	chunks := []string{"first frozen chunk", "second frozen chunk"}
	if _, err := storage.PrepareResetNotification(due[0].ID, chunks, now); err != nil {
		t.Fatal(err)
	}
	qqAPI.groupReplyErr = errors.New("second chunk failed")
	qqAPI.groupReplyErrAt = 2
	service.processDueResetNotificationsAt(context.Background(), now)
	if got := replyCount(qqAPI); got != 1 || lastReply(t, qqAPI) != chunks[0] {
		t.Fatalf("first attempt replies=%d last=%q", got, lastReply(t, qqAPI))
	}

	service.processDueResetNotificationsAt(context.Background(), now.Add(30*time.Second))
	if got := replyCount(qqAPI); got != 2 || lastReply(t, qqAPI) != chunks[1] {
		t.Fatalf("retry replies=%d last=%q", got, lastReply(t, qqAPI))
	}
	if qqAPI.groupReplies != 3 {
		t.Fatalf("group calls=%d, want first + failed second + retried second", qqAPI.groupReplies)
	}
}

func TestResetNotificationRenderFailureIsRetried(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	now := time.Now()
	signal := model.ResetSignal{
		ID:         "x:outbox-render",
		Source:     "test",
		Stage:      model.ResetStagePossible,
		OccurredAt: now,
		DetectedAt: now,
		UpdatedAt:  now,
	}
	service.processResetSignalForGroup(context.Background(), "g-reset", signal)
	due, err := storage.ListDueResetNotifications(now, 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	broken := due[0]
	broken.Kind = model.ResetNotificationKind("broken")
	service.processResetNotificationAt(context.Background(), broken, now)
	if early, err := storage.ListDueResetNotifications(now.Add(29*time.Second), 1); err != nil || len(early) != 0 {
		t.Fatalf("render failure retried too early: due=%#v err=%v", early, err)
	}
	retried, err := storage.ListDueResetNotifications(now.Add(30*time.Second), 1)
	if err != nil || len(retried) != 1 || retried[0].Attempts != 1 || retried[0].Status != model.ResetNotificationPending {
		t.Fatalf("render failure was discarded: due=%#v err=%v", retried, err)
	}
}

func TestResetSignalNotificationUsesEnqueuedStageSnapshot(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	now := time.Now()
	possible := model.ResetSignal{ID: "shared-signal", Source: "test", Stage: model.ResetStagePossible, Summary: "first summary", OccurredAt: now}
	if changed, err := storage.ApplyResetSignalToGroup("g-reset", possible); err != nil || !changed {
		t.Fatalf("possible changed=%v err=%v", changed, err)
	}
	imminent := possible
	imminent.Stage = model.ResetStageImminent
	imminent.Summary = "updated summary"
	if changed, err := storage.ApplyResetSignalToGroup("g-reset", imminent); err != nil || !changed {
		t.Fatalf("imminent changed=%v err=%v", changed, err)
	}
	service.processDueResetNotificationsAt(context.Background(), time.Now().Add(time.Second))
	if len(qqAPI.messages) != 2 {
		t.Fatalf("messages=%#v", qqAPI.messages)
	}
	if !strings.Contains(qqAPI.messages[0], "可能重置") || !strings.Contains(qqAPI.messages[0], "first summary") {
		t.Fatalf("possible notification drifted: %q", qqAPI.messages[0])
	}
	if !strings.Contains(qqAPI.messages[1], "即将重置") || !strings.Contains(qqAPI.messages[1], "updated summary") {
		t.Fatalf("imminent notification incorrect: %q", qqAPI.messages[1])
	}
}

func TestExpiredResetStartNotificationIsSuperseded(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	now := time.Now()
	setting, err := storage.GetOrCreateResetSettings("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Second
	if err := storage.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, created, err := storage.CreateResetActivityFromSignal("g-reset", model.ResetSignal{ID: "expired-start", Source: "test", Stage: model.ResetStageConfirmed, OccurredAt: now}, now)
	if err != nil || !created {
		t.Fatalf("activity=%#v created=%v err=%v", activity, created, err)
	}
	service.processDueResetNotificationsAt(context.Background(), activity.EndsAt)
	if got := replyCount(qqAPI); got != 0 {
		t.Fatalf("expired start notification was sent: %#v", qqAPI.messages)
	}
	if due, err := storage.ListDueResetNotifications(activity.EndsAt.Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("expired start notification remained due: %#v err=%v", due, err)
	}
}

func TestResetSettingsAndProxyPermissions(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	createBinding(t, storage, "member:g-reset:u1", 42)
	createBinding(t, storage, "member:g-reset:admin", 43)

	service.process(context.Background(), groupEvent("g-reset", "u1", "/reset set winners 7"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "没有修改重置活动设置的权限") {
		t.Fatalf("unexpected non-admin setting reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g-reset", "admin", "/reset set winners 7"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "抽取 7 人") {
		t.Fatalf("unexpected admin setting reply: %q", reply)
	}
	setting, err := storage.GetResetSettings("g-reset")
	if err != nil || setting.WinnerCount != 7 {
		t.Fatalf("reset winners not persisted: %#v err=%v", setting, err)
	}

	service.process(context.Background(), groupEvent("g-reset", "admin", "/reset proxy http://name:secret@127.0.0.1:8080"))
	reply := lastReply(t, qqAPI)
	if strings.Contains(reply, "secret") || !strings.Contains(reply, "127.0.0.1:8080") {
		t.Fatalf("proxy reply leaked or omitted address: %q", reply)
	}
	encrypted, err := storage.GetResetProxy()
	if err != nil || strings.Contains(encrypted, "secret") {
		t.Fatalf("proxy was not encrypted at rest: %q err=%v", encrypted, err)
	}
}

func TestDisabledResetJoinDoesNotHideResetCheck(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g-reset", "admin", `/disable "reset join"`))
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/help"))
	reply := lastReply(t, qqAPI)
	if !strings.Contains(reply, "/reset check") {
		t.Fatalf("reset check disappeared from help: %q", reply)
	}
	if strings.Contains(reply, "/reset join") {
		t.Fatalf("disabled reset join remained in help: %q", reply)
	}
}

func TestDisabledResetLastOnlyHidesResetLastHelp(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g-reset", "admin", `/disable "reset last"`))
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/help"))
	reply := lastReply(t, qqAPI)
	if strings.Contains(reply, "/reset last") {
		t.Fatalf("disabled reset last remained in help: %q", reply)
	}
	if !strings.Contains(reply, "/reset check") || !strings.Contains(reply, "/reset join") {
		t.Fatalf("disabling reset last hid unrelated reset commands: %q", reply)
	}
}

func TestDisabledResetNewOnlyHidesResetNewHelp(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	createBinding(t, storage, "member:g-reset:admin", 43)
	service.process(context.Background(), groupEvent("g-reset", "admin", `/disable "reset new"`))
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/help"))
	reply := lastReply(t, qqAPI)
	if strings.Contains(reply, "/reset new") {
		t.Fatalf("disabled reset new remained in help: %q", reply)
	}
	for _, command := range []string{"/reset check", "/reset last", "/reset join", "/reset set"} {
		if !strings.Contains(reply, command) {
			t.Fatalf("disabling reset new hid %s: %q", command, reply)
		}
	}
}

func TestResetHelpHiddenWhenFeatureDisabled(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.cfg.ResetEnabled = false
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/help"))
	if reply := lastReply(t, qqAPI); strings.Contains(reply, "/reset ") {
		t.Fatalf("disabled reset feature remained in help: %q", reply)
	}
}

func TestInvalidResetProxyNeverEchoesCredentials(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g-reset:admin"] = struct{}{}
	createBinding(t, storage, "member:g-reset:admin", 43)
	service.process(context.Background(), groupEvent("g-reset", "admin", "/reset proxy http://name:plain-secret@127.0.0.1:%zz"))
	if reply := lastReply(t, qqAPI); strings.Contains(reply, "plain-secret") || !strings.Contains(reply, "代理链接无效") {
		t.Fatalf("invalid proxy reply leaked credentials: %q", reply)
	}
}

func TestResetSettlementGrantsExactRecentUsage(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	now := time.Now()
	setting, err := service.ensureResetSettings("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Second
	setting.WinnerCount = 1
	setting.Lookback = 24 * time.Hour
	if err := storage.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, created, err := storage.CreateResetActivityFromSignal("g-reset", model.ResetSignal{ID: "x:settle", Source: "test", Stage: model.ResetStageConfirmed, OccurredAt: now.Add(-2 * time.Second)}, now.Add(-2*time.Second))
	if err != nil || !created {
		t.Fatalf("create activity: %#v created=%v err=%v", activity, created, err)
	}
	if _, joined, err := storage.JoinResetActivity("g-reset", model.ResetParticipant{NewAPIID: 42, CanonicalID: "member:g-reset:u1", MemberOpenID: "u1"}, now.Add(-1500*time.Millisecond)); err != nil || !joined {
		t.Fatalf("join: %v joined=%v", err, joined)
	}
	api.usageByUser = []newapi.UsageRecord{{UserID: 42, Username: "alice", Quota: 750000, Count: 3, TokenUsed: 100}}

	service.processDueResetActivities(context.Background())
	if len(api.usageRanges) != 1 {
		t.Fatalf("usage ranges=%#v", api.usageRanges)
	}
	wantUsageEnd := activity.StartedAt
	wantUsageStart := wantUsageEnd.Add(-activity.Lookback)
	if !api.usageRanges[0].Start.Equal(wantUsageStart) || !api.usageRanges[0].End.Equal(wantUsageEnd) {
		t.Fatalf("usage range=%s..%s, want %s..%s", api.usageRanges[0].Start, api.usageRanges[0].End, wantUsageStart, wantUsageEnd)
	}
	if api.usageRanges[0].End.Equal(activity.EndsAt) {
		t.Fatalf("usage range incorrectly includes activity period: end=%s", api.usageRanges[0].End)
	}
	if len(api.quotaAddCalls) != 1 || api.quotaAddCalls[0] != (quotaAddCall{UserID: 42, Quota: 750000}) {
		t.Fatalf("quota calls=%#v", api.quotaAddCalls)
	}
	completed, err := storage.GetResetActivity(activity.ID)
	if err != nil || completed.Status != model.ResetActivityCompleted || len(completed.Awards) != 1 || completed.Awards[0].Status != model.ResetAwardGranted {
		t.Fatalf("completed activity=%#v err=%v", completed, err)
	}
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "1.5 额度") || !strings.Contains(reply, "群重置状态已恢复为：未知") {
		t.Fatalf("unexpected completion reply: %q", reply)
	}
}

func TestResetSettlementMarksAmbiguousWriteWithoutRetry(t *testing.T) {
	service, storage, api, _, _ := testService(t)
	now := time.Now()
	setting, err := service.ensureResetSettings("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Second
	setting.WinnerCount = 1
	if err := storage.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, _, err := storage.CreateResetActivityFromSignal("g-reset", model.ResetSignal{ID: "x:ambiguous", Source: "test", Stage: model.ResetStageConfirmed, OccurredAt: now.Add(-2 * time.Second)}, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.JoinResetActivity("g-reset", model.ResetParticipant{NewAPIID: 42, CanonicalID: "member:g-reset:u1"}, now.Add(-1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	api.usageByUser = []newapi.UsageRecord{{UserID: 42, Quota: 500000}}
	api.addQuotaErr = context.DeadlineExceeded

	service.processDueResetActivities(context.Background())
	service.processDueResetActivities(context.Background())
	if len(api.quotaAddCalls) != 1 {
		t.Fatalf("ambiguous quota write retried: %#v", api.quotaAddCalls)
	}
	completed, err := storage.GetResetActivity(activity.ID)
	if err != nil || completed.Awards[0].Status != model.ResetAwardPendingConfirmation {
		t.Fatalf("completed activity=%#v err=%v", completed, err)
	}
}

func TestResetSettlementSkipsZeroUsage(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	now := time.Now()
	setting, err := service.ensureResetSettings("g-reset")
	if err != nil {
		t.Fatal(err)
	}
	setting.Duration = time.Second
	if err := storage.PutResetSettings(setting); err != nil {
		t.Fatal(err)
	}
	activity, _, err := storage.CreateResetActivityFromSignal("g-reset", model.ResetSignal{ID: "x:zero", Source: "test", Stage: model.ResetStageConfirmed, OccurredAt: now.Add(-2 * time.Second)}, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.JoinResetActivity("g-reset", model.ResetParticipant{NewAPIID: 42, CanonicalID: "member:g-reset:u1"}, now.Add(-1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	service.processDueResetActivities(context.Background())
	if len(api.quotaAddCalls) != 0 {
		t.Fatalf("zero quota called AddQuota: %#v", api.quotaAddCalls)
	}
	completed, err := storage.GetResetActivity(activity.ID)
	if err != nil || completed.Awards[0].Status != model.ResetAwardZero {
		t.Fatalf("completed activity=%#v err=%v", completed, err)
	}
	service.process(context.Background(), groupEvent("g-reset", "ordinary", "/reset check"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "当前重置状态：未知") {
		t.Fatalf("completed reset status did not return to unknown: %q", reply)
	}
}

func createBinding(t *testing.T, storage interface {
	CreateBinding(model.Binding) error
}, canonical string, userID int) {
	t.Helper()
	now := time.Now()
	if err := storage.CreateBinding(model.Binding{CanonicalID: canonical, NewAPIID: userID, Email: "user@example.com", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}
