package bot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
)

func TestHongbaoStopPreservesAwardsBlocksClaimsAndAllowsNextRound(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "10", "3", "gpt-cheap")
	api.user.Group = "gpt-cheap"
	bindHongbaoUser(t, storage, "g", "alice", 42)
	bindHongbaoUser(t, storage, "g", "bob", 43)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	before, _ := storage.GetHongbao("g")
	stoppedAt := time.Now().UTC()
	service.now = func() time.Time { return stoppedAt }
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao stop"))
	packet, err := storage.GetHongbao("g")
	if err != nil {
		t.Fatal(err)
	}
	expected := before
	expected.StoppedAt = stoppedAt
	if !reflect.DeepEqual(packet, expected) || !packet.CompletedAt.IsZero() || api.quotaAdds != 1 || api.quotaSubs != 0 {
		t.Fatalf("stop changed awards or balances: packet=%+v before=%+v", packet, before)
	}
	if got := lastReply(t, qqAPI); !strings.Contains(got, "已停止领取") ||
		!strings.Contains(got, "已领取红包：1 个") || !strings.Contains(got, "剩余红包：2 个") {
		t.Fatal(got)
	}
	lookup := &hongbaoGroupLookupAPI{fakeNewAPI: api, getUserErr: errors.New("lookup should not run")}
	service.newAPI = lookup
	for _, member := range []string{"alice", "bob"} {
		service.process(context.Background(), groupEvent("g", member, "/hongbao"))
		if !strings.Contains(lastReply(t, qqAPI), "已停止领取") {
			t.Fatal(lastReply(t, qqAPI))
		}
	}
	if api.quotaAdds != 1 || len(lookup.userIDs) != 0 {
		t.Fatal("stopped packet performed a group lookup or granted quota")
	}
	service.now = func() time.Time { return stoppedAt.Add(time.Hour) }
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao stop"))
	current, _ := storage.GetHongbao("g")
	if !reflect.DeepEqual(current, packet) || !strings.Contains(lastReply(t, qqAPI), "无需重复操作") {
		t.Fatal("repeated stop changed packet state")
	}
	restarted := New(service.cfg, storage, service.secure, lookup, qqAPI, service.mailer, service.logger)
	restarted.process(context.Background(), groupEvent("g", "bob", "/hongbao"))
	if !strings.Contains(lastReply(t, qqAPI), "已停止领取") || api.quotaAdds != 1 {
		t.Fatal("restart reactivated stopped packet")
	}
	replyCount := len(qqAPI.messages)
	restarted.retryHongbaoSummaries(context.Background())
	if len(qqAPI.messages) != replyCount {
		t.Fatal("stopped packet produced a completion summary")
	}
	next := groupEvent("g", "admin", "/hongbao new 10 3")
	next.Message.ID = "next-after-stop"
	restarted.process(context.Background(), next)
	current, _ = storage.GetHongbao("g")
	if current.ID == packet.ID || !current.StoppedAt.IsZero() || current.RemainingCount != 3 {
		t.Fatal("could not create a fresh round after stopping")
	}
	restarted.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 2 {
		t.Fatal("account could not claim the next round")
	}
}

func TestHongbaoStopPermissionSyntaxAndGroupIsolation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  string
		full     bool
		readOnly bool
		private  bool
		other    bool
		stopped  bool
		want     string
	}{
		{"ordinary-user", "/hongbao stop", false, false, false, false, false, "仅具备完整管理权限"},
		{"readonly-admin", "/hongbao stop", false, true, false, false, false, "只读管理员"},
		{"full-admin-overrides-readonly", "/hongbao stop", true, true, false, false, true, "已停止领取"},
		{"uppercase", "/HONGBAO STOP", true, false, false, false, true, "已停止领取"},
		{"extra-argument", "/hongbao stop extra", true, false, false, false, false, "使用方式"},
		{"private", "/hongbao stop", true, false, true, false, false, "仅限群聊"},
		{"other-group", "/hongbao stop", true, false, false, true, false, "暂无正在发放"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, storage, api, qqAPI, _ := testService(t)
			createTestHongbao(t, service, "1", "1")
			before, _ := storage.GetHongbao("g")
			if tc.full {
				service.cfg.QQAdminOpenIDs["member:g:visitor"] = struct{}{}
				service.cfg.QQAdminOpenIDs["member:other:visitor"] = struct{}{}
			}
			if tc.readOnly {
				service.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"member:g:visitor": {}}
			}
			event := groupEvent("g", "visitor", tc.command)
			if tc.private {
				event = c2cEvent("admin", tc.command)
			}
			if tc.other {
				event = groupEvent("other", "visitor", tc.command)
			}
			service.process(context.Background(), event)
			packet, _ := storage.GetHongbao("g")
			if got := lastReply(t, qqAPI); !strings.Contains(got, tc.want) {
				t.Fatalf("reply=%q want=%q", got, tc.want)
			}
			if tc.stopped {
				if packet.StoppedAt.IsZero() {
					t.Fatal("administrator could not stop without a binding")
				}
			} else if !reflect.DeepEqual(before, packet) {
				t.Fatal("rejected stop modified current group packet")
			}
			if api.quotaAdds != 0 || api.quotaSubs != 0 {
				t.Fatal("stop unexpectedly changed account quota")
			}
		})
	}
}

func TestHongbaoStopMissingAndCompletedPackets(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao stop"))
	if !strings.Contains(lastReply(t, qqAPI), "暂无正在发放") {
		t.Fatal(lastReply(t, qqAPI))
	}
	createTestHongbao(t, service, "1", "1")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	before, _ := storage.GetHongbao("g")
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao stop"))
	after, _ := storage.GetHongbao("g")
	if !strings.Contains(lastReply(t, qqAPI), "已全部领取，无需停止") || !reflect.DeepEqual(before, after) || api.quotaAdds != 1 {
		t.Fatal("completed packet was changed by stop")
	}
	if !strings.Contains(service.helpTextFor(model.QQIdentity{UserOpenID: "admin"}, "/hongbao"), "/hongbao stop") || !strings.Contains(hongbaoUsage(), "/hongbao stop") {
		t.Fatal("missing stop help")
	}
}

func TestHongbaoStopPreservesPendingWrites(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "1", "2")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	api.addQuotaErr = context.DeadlineExceeded
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	before, _ := storage.GetHongbao("g")
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao stop"))
	stopped, _ := storage.GetHongbao("g")
	if stopped.StoppedAt.IsZero() || stopped.Claims[42].Status != "pending_confirmation" ||
		stopped.RemainingQuota != before.RemainingQuota || stopped.RemainingCount != before.RemainingCount {
		t.Fatal("stop discarded or released an ambiguous write")
	}
	if !strings.Contains(lastReply(t, qqAPI), "发放结果待确认") {
		t.Fatal(lastReply(t, qqAPI))
	}
	api.addQuotaErr = nil
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao new 2 2"))
	current, _ := storage.GetHongbao("g")
	if api.quotaAdds != 1 || !reflect.DeepEqual(current, stopped) || !strings.Contains(lastReply(t, qqAPI), "核查后再发放") {
		t.Fatal("pending write was retried or its record overwritten")
	}
	// Simulate an administrator confirming the original quota write.
	claim := stopped.Claims[42]
	claim.Status = "granted"
	stopped.Claims[42] = claim
	stopped.GrantedCount++
	if err := storage.PutHongbao(stopped); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao new 2 2"))
	current, _ = storage.GetHongbao("g")
	if current.ID == stopped.ID || !current.StoppedAt.IsZero() {
		t.Fatal("confirmed stopped packet prevented a new round")
	}
}

func TestHongbaoStopSerializesWithConcurrentClaims(t *testing.T) {
	service, storage, api, _, _ := testService(t)
	createTestHongbao(t, service, "100", "100")
	const claimants = 30
	for i := 0; i < claimants; i++ {
		bindHongbaoUser(t, storage, "g", fmt.Sprintf("user-%d", i), 100+i)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < claimants; i++ {
		workers.Add(1)
		go func(member string) {
			defer workers.Done()
			<-start
			service.process(context.Background(), groupEvent("g", member, "/hongbao"))
		}(fmt.Sprintf("user-%d", i))
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		service.process(context.Background(), groupEvent("g", "admin", "/hongbao stop"))
	}()
	close(start)
	workers.Wait()
	packet, _ := storage.GetHongbao("g")
	if packet.StoppedAt.IsZero() || packet.GrantedCount != api.quotaAdds || packet.RemainingCount+packet.GrantedCount != 100 {
		t.Fatalf("inconsistent concurrent stop: packet=%+v adds=%d", packet, api.quotaAdds)
	}
	var granted int64
	for _, claim := range packet.Claims {
		if claim.Status != "granted" {
			t.Fatal("concurrent stop left an unfinished claim")
		}
		granted += claim.RawQuota
	}
	if granted+packet.RemainingQuota != packet.TotalQuota {
		t.Fatal("concurrent stop changed total quota")
	}
	adds := api.quotaAdds
	service.process(context.Background(), groupEvent("g", "user-0", "/hongbao"))
	if api.quotaAdds != adds {
		t.Fatal("quota was granted after stop")
	}
}

func TestHongbaoStopFlagSuppressesCompletionSummary(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "1", "1")
	packet, _ := storage.GetHongbao("g")
	packet.StoppedAt = time.Now()
	packet.CompletedAt = packet.StoppedAt
	if err := storage.PutHongbao(packet); err != nil {
		t.Fatal(err)
	}
	if pending, err := storage.ListPendingHongbaoSummaries(); err != nil || len(pending) != 0 {
		t.Fatalf("stopped summary was queued: pending=%v err=%v", pending, err)
	}
	before := len(qqAPI.messages)
	if err := service.announceHongbaoSummary(context.Background(), packet, ""); err != nil {
		t.Fatal(err)
	}
	if len(qqAPI.messages) != before {
		t.Fatal("stopped packet announced all red packets claimed")
	}
}
