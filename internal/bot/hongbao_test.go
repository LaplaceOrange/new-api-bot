package bot

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type hongbaoSequencedQQ struct {
	*fakeQQ
	sequences []int
	replyIDs  []string
}

func (f *hongbaoSequencedQQ) SendGroupTextWithSequence(ctx context.Context, group, replyTo, content string, sequence int) (qq.SentMessage, error) {
	f.sequences = append(f.sequences, sequence)
	f.replyIDs = append(f.replyIDs, replyTo)
	err := f.ReplyGroup(ctx, group, replyTo, content)
	return qq.SentMessage{}, err
}

func TestHongbaoFinalReplyUsesDistinctSequences(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	client := &hongbaoSequencedQQ{fakeQQ: qqAPI}
	service.qq = client
	createTestHongbao(t, service, "1", "1")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if len(client.sequences) != 3 || client.sequences[1] != 1 || client.sequences[2] != 2 ||
		client.replyIDs[1] == "" || client.replyIDs[1] != client.replyIDs[2] {
		t.Fatalf("sequences = %v, reply IDs = %v", client.sequences, client.replyIDs)
	}
}

func bindHongbaoUser(t *testing.T, storage *store.Store, group, member string, id int) {
	t.Helper()
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:" + group + ":" + member, NewAPIID: id}); err != nil {
		t.Fatal(err)
	}
}

func createTestHongbao(t *testing.T, service *Service, total, count string) {
	t.Helper()
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao new "+total+" "+count))
	packet, err := service.store.GetHongbao("g")
	if err != nil || packet.TotalCount == 0 {
		t.Fatalf("packet = %+v, error = %v", packet, err)
	}
}

func TestHongbaoCreateClaimAndSummary(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	current := start
	service.now = func() time.Time { return current }
	createTestHongbao(t, service, "10", "2")
	if got := lastReply(t, qqAPI); !strings.Contains(got, "总额度：10") || !strings.Contains(got, "红包数量：2 个") {
		t.Fatal(got)
	}
	bindHongbaoUser(t, storage, "g", "alice", 42)
	bindHongbaoUser(t, storage, "g", "bob", 43)
	current = start.Add(1500 * time.Millisecond)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	first, err := storage.GetHongbao("g")
	if err != nil {
		t.Fatal(err)
	}
	claim := first.Claims[42]
	got := lastReply(t, qqAPI)
	if !strings.Contains(got, "领取额度："+newapi.QuotaToDisplay(claim.RawQuota, 500000)) ||
		!strings.Contains(got, "剩余红包：1 个") ||
		!strings.Contains(got, "剩余额度："+newapi.QuotaToDisplay(first.RemainingQuota, 500000)) {
		t.Fatal(got)
	}
	current = start.Add(3750 * time.Millisecond)
	service.process(context.Background(), groupEvent("g", "bob", "/hongbao"))
	packet, _ := storage.GetHongbao("g")
	if api.quotaAdds != 2 || api.quotaSubs != 0 || packet.RemainingCount != 0 || packet.RemainingQuota != 0 ||
		packet.GrantedCount != 2 || !packet.SummarySent || packet.Claims[42].RawQuota+packet.Claims[43].RawQuota != 5000000 {
		t.Fatalf("packet = %+v, adds = %d", packet, api.quotaAdds)
	}
	got = lastReply(t, qqAPI)
	if !strings.Contains(got, "红包总数：2 个") || !strings.Contains(got, "发放总额度：10") || !strings.Contains(got, "领取用时：3.75 秒") {
		t.Fatal(got)
	}
	if !strings.Contains(qqAPI.messages[len(qqAPI.messages)-2], "剩余额度：0") {
		t.Fatal("missing final claim reply")
	}
}

func TestHongbaoPermissionBindingAndValidation(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		admin    bool
		readOnly bool
		c2c      bool
		want     string
	}{
		{"non-admin", "/hongbao new 10 2", false, false, false, "仅具备完整管理权限"},
		{"readonly-create", "/hongbao new 10 2", false, true, false, "只读管理员"},
		{"readonly-claim", "/hongbao", false, true, false, "只读管理员"},
		{"private", "/hongbao new 10 2", true, false, true, "仅限群聊"},
		{"unbound", "/hongbao", false, false, false, "完成账户绑定"},
		{"syntax", "/hongbao new 10", true, false, false, "使用方式"},
		{"unknown-subcommand", "/hongbao list", true, false, false, "使用方式"},
		{"zero", "/hongbao new 0 2", true, false, false, "正数额度"},
		{"negative", "/hongbao new -1 2", true, false, false, "正数额度"},
		{"nan", "/hongbao new NaN 2", true, false, false, "正数额度"},
		{"overflow", "/hongbao new 999999999999999999999 2", true, false, false, "支持范围"},
		{"fractional-quota", "/hongbao new 0.000001 2", true, false, false, "整数 quota"},
		{"too-few-quota", "/hongbao new 0.000002 2", true, false, false, "总金额不足"},
		{"limit", "/hongbao new 1001 2", true, false, false, "单次额度上限"},
		{"zero-count", "/hongbao new 1 0", true, false, false, "红包个数"},
		{"negative-count", "/hongbao new 1 -1", true, false, false, "红包个数"},
		{"fractional-count", "/hongbao new 1 1.5", true, false, false, "红包个数"},
		{"too-many", "/hongbao new 1 10001", true, false, false, "红包个数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, storage, api, qqAPI, _ := testService(t)
			if tc.admin {
				service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
			}
			if tc.readOnly {
				service.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"member:g:admin": {}}
			}
			event := groupEvent("g", "admin", tc.command)
			if tc.c2c {
				event = c2cEvent("admin", tc.command)
			}
			service.process(context.Background(), event)
			if got := lastReply(t, qqAPI); !strings.Contains(got, tc.want) {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
			if api.quotaAdds != 0 {
				t.Fatal("unexpected quota write")
			}
			if _, err := storage.GetHongbao("g"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unexpected packet, err = %v", err)
			}
		})
	}
}

func TestHongbaoDuplicateClaimAndAccountRebinding(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "2", "2")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	first, _ := storage.GetHongbao("g")
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 1 || !strings.Contains(lastReply(t, qqAPI), "本轮红包已领取") {
		t.Fatal("duplicate claim was not rejected")
	}
	if _, err := storage.UnbindByNewAPIID(42); err != nil {
		t.Fatal(err)
	}
	bindHongbaoUser(t, storage, "g", "other", 42)
	service.process(context.Background(), groupEvent("g", "other", "/hongbao"))
	current, _ := storage.GetHongbao("g")
	if api.quotaAdds != 1 || current.RemainingQuota != first.RemainingQuota {
		t.Fatal("rebinding allowed repeat claim")
	}
}

func TestHongbaoGroupIsolationAndActivePacket(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "2", "1")
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao new 3 2"))
	if !strings.Contains(lastReply(t, qqAPI), "仍有未结束") {
		t.Fatal(lastReply(t, qqAPI))
	}
	bindHongbaoUser(t, storage, "other", "alice", 42)
	service.process(context.Background(), groupEvent("other", "alice", "/hongbao"))
	if !strings.Contains(lastReply(t, qqAPI), "暂无可领取") || api.quotaAdds != 0 {
		t.Fatal(lastReply(t, qqAPI))
	}
	packet, _ := storage.GetHongbao("g")
	if packet.TotalQuota != 1000000 || packet.TotalCount != 1 {
		t.Fatal("active packet overwritten")
	}
}

func TestHongbaoConcurrentClaims(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "0.000004", "2")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	bindHongbaoUser(t, storage, "g", "bob", 43)
	bindHongbaoUser(t, storage, "g", "carol", 44)
	var workers sync.WaitGroup
	for i := 0; i < 30; i++ {
		member := []string{"alice", "bob", "carol"}[i%3]
		workers.Add(1)
		go func() {
			defer workers.Done()
			service.process(context.Background(), groupEvent("g", member, "/hongbao"))
		}()
	}
	workers.Wait()
	packet, _ := storage.GetHongbao("g")
	if api.quotaAdds != 2 || packet.RemainingQuota != 0 || packet.RemainingCount != 0 || !packet.SummarySent {
		t.Fatalf("packet = %+v, adds = %d", packet, api.quotaAdds)
	}
	summaries := 0
	for _, message := range qqAPI.messages {
		if strings.Contains(message, "领取用时") {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("summaries = %d", summaries)
	}
}

func TestHongbaoQuotaFailureAndAmbiguousWrite(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "definite", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			service, storage, api, qqAPI, _ := testService(t)
			createTestHongbao(t, service, "1", "1")
			bindHongbaoUser(t, storage, "g", "alice", 42)
			if ambiguous {
				api.addQuotaErr = context.DeadlineExceeded
			} else {
				api.addQuotaErr = &newapi.APIError{StatusCode: 400, Message: "quota rejected"}
			}
			service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
			packet, _ := storage.GetHongbao("g")
			if packet.GrantedCount != 0 || !packet.CompletedAt.IsZero() {
				t.Fatal("failed request marked completed")
			}
			api.addQuotaErr = nil
			restarted := New(service.cfg, storage, service.secure, api, qqAPI, service.mailer, service.logger)
			restarted.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
			if ambiguous {
				if api.quotaAdds != 1 || packet.RemainingCount != 0 || packet.Claims[42].Status != "pending_confirmation" ||
					!strings.Contains(lastReply(t, qqAPI), "尚待确认") {
					t.Fatal("ambiguous request was retried")
				}
			} else if api.quotaAdds != 2 || packet.RemainingCount != 1 || packet.RemainingQuota != 500000 {
				t.Fatalf("definite failure did not release reservation: %+v", packet)
			}
		})
	}
}

func TestHongbaoSummaryFailureRecoveryAndNextRound(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	createTestHongbao(t, service, "1", "1")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	qqAPI.groupReplyErr = errors.New("send failed")
	qqAPI.groupReplyErrAt = 3 // create, claim, summary
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	packet, _ := storage.GetHongbao("g")
	if packet.CompletedAt.IsZero() || packet.SummarySent || api.quotaAdds != 1 {
		t.Fatalf("packet = %+v", packet)
	}
	qqAPI.groupReplyErr = nil
	restarted := New(service.cfg, storage, service.secure, api, qqAPI, service.mailer, service.logger)
	restarted.cfg.BenefitEnabled = false
	restarted.checkBenefitLifecycle(context.Background())
	packet, _ = storage.GetHongbao("g")
	if !packet.SummarySent || !strings.Contains(lastReply(t, qqAPI), "领取用时") {
		t.Fatal("summary was not recovered")
	}
	before := len(qqAPI.messages)
	restarted.checkBenefitLifecycle(context.Background())
	if len(qqAPI.messages) != before {
		t.Fatal("summary sent twice")
	}
	restarted.process(context.Background(), groupEvent("g", "admin", "/hongbao new 1 1"))
	if !strings.Contains(lastReply(t, qqAPI), "发放请求已处理") {
		t.Fatal("replayed creation event was not rejected")
	}
	next := groupEvent("g", "admin", "/hongbao new 1 1")
	next.Message.ID = "next-round"
	restarted.process(context.Background(), next)
	restarted.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 2 {
		t.Fatal("account could not claim next round")
	}
}

func TestRandomHongbaoQuotaConservation(t *testing.T) {
	for _, tc := range []struct {
		total int64
		count int
	}{{1, 1}, {100, 100}, {5000000, 100}, {math.MaxInt64, 2}, {math.MaxInt64, 100}} {
		for trial := 0; trial < 10; trial++ {
			left := tc.total
			for count := tc.count; count > 0; count-- {
				quota, err := randomHongbaoQuota(left, count)
				if err != nil || quota < 1 || quota > left-int64(count-1) {
					t.Fatalf("quota = %d, left = %d, count = %d, err = %v", quota, left, count, err)
				}
				left -= quota
			}
			if left != 0 {
				t.Fatalf("remainder = %d", left)
			}
		}
	}
	for _, tc := range []struct {
		total int64
		count int
	}{{0, 1}, {1, 2}, {1, 0}, {-1, 1}} {
		if _, err := randomHongbaoQuota(tc.total, tc.count); err == nil {
			t.Fatal("invalid allocation accepted")
		}
	}
}

func TestHongbaoHelpAndDisableRule(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	if !strings.Contains(service.filteredHelpText(), "/hongbao new <总金额> <个数>") {
		t.Fatal("missing help")
	}
	if err := storage.PutCommandRule(model.CommandRule{Keyword: "hongbao", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	service.commandRules.Store(nil)
	if strings.Contains(service.filteredHelpText(), "hongbao") {
		t.Fatal("disabled command still in help")
	}
	service.process(context.Background(), groupEvent("g", "admin", "/hongbao new 1 1"))
	if len(qqAPI.messages) != 0 || api.quotaAdds != 0 {
		t.Fatal("disabled command was not ignored")
	}
}
