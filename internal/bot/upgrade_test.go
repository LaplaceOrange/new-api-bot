package bot

import (
	"context"
	"errors"
	"testing"

	"github.com/fsykk/new-api-bot/internal/store"
)

func TestUpgradeNotificationsPersistAcrossRestart(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.ObserveGroup("group-b"); err != nil {
		t.Fatal(err)
	}
	if err := storage.ObserveGroup("group-a"); err != nil {
		t.Fatal(err)
	}
	if err := service.AnnounceUpgradeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := replyCount(qqAPI); got != 2 {
		t.Fatalf("starting replies=%d, want 2", got)
	}
	notification, err := storage.GetUpgradeNotification()
	if err != nil {
		t.Fatal(err)
	}
	if len(notification.Groups) != 2 || notification.Groups[0] != "group-a" || notification.Groups[1] != "group-b" {
		t.Fatalf("pending groups=%v", notification.Groups)
	}

	connected := false
	service.SetGatewayConnectedFunc(func() bool { return connected })
	completed, err := service.sendPendingUpgradeCompletion(context.Background())
	if err != nil || completed {
		t.Fatalf("disconnected completion=%v err=%v", completed, err)
	}
	if got := replyCount(qqAPI); got != 2 {
		t.Fatalf("completion sent before gateway connected: replies=%d", got)
	}

	connected = true
	completed, err = service.sendPendingUpgradeCompletion(context.Background())
	if err != nil || !completed {
		t.Fatalf("connected completion=%v err=%v", completed, err)
	}
	if got := replyCount(qqAPI); got != 4 {
		t.Fatalf("total replies=%d, want 4", got)
	}
	if _, err := storage.GetUpgradeNotification(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("upgrade notification err=%v, want not found", err)
	}
}

func TestUpgradeCompletionWithoutPendingStateIsSilent(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	completed, err := service.sendPendingUpgradeCompletion(context.Background())
	if err != nil || !completed {
		t.Fatalf("completion=%v err=%v", completed, err)
	}
	if got := replyCount(qqAPI); got != 0 {
		t.Fatalf("unexpected replies=%d", got)
	}
}
