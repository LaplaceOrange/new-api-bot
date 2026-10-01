package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type timedHongbaoRecallQQ struct {
	*recallQQ
	recalledAt []time.Time
}

func (f *timedHongbaoRecallQQ) RecallGroupMessage(ctx context.Context, group, messageID string) error {
	f.mu.Lock()
	f.recalledAt = append(f.recalledAt, time.Now())
	f.mu.Unlock()
	return f.recallQQ.RecallGroupMessage(ctx, group, messageID)
}

func TestHongbaoNoticesRecallReplyAndCommandAfterThirtySeconds(t *testing.T) {
	if hongbaoNoticeRecallAfter != 30*time.Second {
		t.Fatalf("notice recall delay = %s, want 30s", hongbaoNoticeRecallAfter)
	}
	for _, mode := range []string{"unauthorized", "readonly", "group-denied", "duplicate-active", "duplicate-completed", "duplicate-pending", "create-success", "claim-success"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			service, storage, api, qqAPI, _ := testService(t)
			// These notices must recall even if check-in/help recall is disabled.
			service.cfg.CheckinAutoRecallAfter = 0
			lifecycle, cancel := context.WithCancel(context.Background())
			service.lifecycleCtx = lifecycle
			t.Cleanup(cancel)
			event := groupEvent("g", "visitor", "/hongbao new 1 1")
			wantRecall := true
			wantText := "仅具备完整管理权限"
			wantSent := 1
			switch mode {
			case "group-denied":
				service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
				service.process(context.Background(), groupEvent("g", "admin", "/hongbao new 1 1 gpt-cheap gpt-smart"))
				if packet, err := storage.GetHongbao("g"); err != nil || len(packet.AllowedGroups) != 2 {
					t.Fatalf("restricted packet = %+v, err = %v", packet, err)
				}
				bindHongbaoUser(t, storage, "g", "visitor", 42)
				api.user.Group = "default"
				event = groupEvent("g", "visitor", "/hongbao")
				wantText = "无权限领取本轮红包"
			case "readonly":
				service.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"member:g:visitor": {}}
				service.cfg.CheckinAutoRecallAfter = time.Millisecond
				wantText = "只读管理员"
			case "duplicate-active", "duplicate-completed", "duplicate-pending", "claim-success":
				count := "2"
				if mode == "duplicate-completed" || mode == "claim-success" {
					count = "1"
				}
				createTestHongbao(t, service, "1", count)
				bindHongbaoUser(t, storage, "g", "visitor", 42)
				event = groupEvent("g", "visitor", "/hongbao")
				if mode == "claim-success" {
					wantRecall = false
					wantText = "红包领取成功"
					wantSent = 2 // successful claim and final summary
				} else {
					if mode == "duplicate-pending" {
						api.addQuotaErr = context.DeadlineExceeded
					}
					service.process(context.Background(), event)
					api.addQuotaErr = nil
					wantText = "本轮红包已领取"
					if mode == "duplicate-pending" {
						wantText = "尚待确认"
					}
				}
			case "create-success":
				service.cfg.QQAdminOpenIDs["member:g:visitor"] = struct{}{}
				wantRecall = false
				wantText = "红包已发放"
			}
			recaller := &timedHongbaoRecallQQ{recallQQ: &recallQQ{fakeQQ: qqAPI}}
			service.qq = recaller
			start := time.Now()
			service.process(context.Background(), event)
			recaller.mu.Lock()
			if len(recaller.sent) != wantSent {
				count := len(recaller.sent)
				recaller.mu.Unlock()
				t.Fatalf("sent = %d, want %d", count, wantSent)
			}
			botID := recaller.sent[0].ID
			claimReply := recaller.messages[len(recaller.messages)-wantSent]
			recaller.mu.Unlock()
			if !strings.Contains(claimReply, wantText) {
				t.Fatalf("reply = %q, want %q", claimReply, wantText)
			}
			if !wantRecall {
				// Wait beyond the real fixed delay to ensure success replies and
				// the final summary remain, not just that they are delayed.
				time.Sleep(hongbaoNoticeRecallAfter + 100*time.Millisecond)
				recaller.mu.Lock()
				defer recaller.mu.Unlock()
				if len(recaller.recalled) != 0 {
					t.Fatalf("success messages unexpectedly recalled: %v", recaller.recalled)
				}
				return
			}
			deadline := start.Add(40 * time.Second)
			for {
				recaller.mu.Lock()
				done := len(recaller.recalled) == 2
				recaller.mu.Unlock()
				if done {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("notice and user command were not both recalled")
				}
				time.Sleep(20 * time.Millisecond)
			}
			recaller.mu.Lock()
			defer recaller.mu.Unlock()
			if recaller.recalled[0] != botID || recaller.recalled[1] != event.Message.ID {
				t.Fatalf("recalled = %v, want bot %q then user %q", recaller.recalled, botID, event.Message.ID)
			}
			for _, at := range recaller.recalledAt {
				if at.Sub(start) < hongbaoNoticeRecallAfter {
					t.Fatalf("recalled after %s, want at least 30s", at.Sub(start))
				}
			}
		})
	}
}

func TestExplicitRecallDelayFallbacks(t *testing.T) {
	for _, mode := range []string{"c2c", "legacy-client", "send-error", "disabled", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			service, _, _, qqAPI, _ := testService(t)
			recaller := &recallQQ{fakeQQ: qqAPI}
			service.qq = recaller
			lifecycle, cancel := context.WithCancel(context.Background())
			defer cancel()
			service.lifecycleCtx = lifecycle
			event := groupEvent("g", "visitor", "/hongbao")
			delay := 10 * time.Millisecond
			switch mode {
			case "c2c":
				event = c2cEvent("visitor", "/hongbao")
			case "legacy-client":
				service.qq = qqAPI
			case "send-error":
				recaller.sendGroupErr = errors.New("send failed")
			case "disabled":
				delay = 0
			case "shutdown":
				cancel()
			}
			err := service.replyWithAutoRecallAfter(context.Background(), event, "notice", delay)
			if (err != nil) != (mode == "send-error") {
				t.Fatalf("reply error = %v, mode = %s", err, mode)
			}
			time.Sleep(40 * time.Millisecond)
			recaller.mu.Lock()
			defer recaller.mu.Unlock()
			if len(recaller.recalled) != 0 {
				t.Fatalf("unexpected recalls: %v", recaller.recalled)
			}
		})
	}
}
