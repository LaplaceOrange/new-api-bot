package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/config"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/secure"
	"github.com/fsykk/new-api-bot/internal/store"
)

func TestPublicErrorMapsExistingAdministratorPermissionFailure(t *testing.T) {
	err := errors.New("New API 请求失败（HTTP 200）：No permission to update users of same or higher permission level")
	if got := publicError(err); got != "该用户已经是管理员。" {
		t.Fatalf("publicError() = %q", got)
	}
}

type fakeNewAPI struct {
	user           newapi.User
	users          []newapi.User
	quotaAdds      int
	quotaSubs      int
	lastQuotaUser  int
	lastQuota      int64
	quotaAddCalls  []quotaAddCall
	subscriptions  map[int][]newapi.UserSubscriptionRecord
	nextSubID      int
	usageByUser    []newapi.UsageRecord
	usageUserCalls int
	usageRanges    []usageRange
	usageByModel   []newapi.UsageRecord
	logs           []newapi.LogRecord
	models         []string
	managedAction  string
	reset2FA       int
	resetPasskey   int
	redemptions    []newapi.Redemption
	addQuotaErr    error
	logQueryStarts []time.Time
	logSplitAfter  time.Duration
}

type quotaAddCall struct {
	UserID int
	Quota  int64
}

type usageRange struct {
	Start time.Time
	End   time.Time
}

func (f *fakeNewAPI) GetStatus(context.Context, bool) (newapi.Status, error) {
	return newapi.Status{SystemName: "Test API", QuotaPerUnit: 500000}, nil
}
func (f *fakeNewAPI) GetUser(_ context.Context, id int) (newapi.User, error) {
	if f.user.ID == id {
		return f.user, nil
	}
	for _, user := range f.users {
		if user.ID == id {
			return user, nil
		}
	}
	u := f.user
	u.ID = id
	return u, nil
}
func (f *fakeNewAPI) ListUsers(context.Context) ([]newapi.User, error) {
	if len(f.users) > 0 {
		users := append([]newapi.User(nil), f.users...)
		for index := range users {
			if users[index].ID == f.user.ID {
				users[index] = f.user
			}
		}
		return users, nil
	}
	return []newapi.User{f.user}, nil
}
func (f *fakeNewAPI) FindUserByEmail(context.Context, string) (newapi.User, error) {
	return f.user, nil
}
func (f *fakeNewAPI) AddQuota(_ context.Context, userID int, quota int64) error {
	f.quotaAdds++
	f.lastQuotaUser = userID
	f.lastQuota = quota
	f.quotaAddCalls = append(f.quotaAddCalls, quotaAddCall{UserID: userID, Quota: quota})
	return f.addQuotaErr
}
func (f *fakeNewAPI) SubtractQuota(_ context.Context, userID int, quota int64) error {
	f.quotaSubs++
	f.lastQuotaUser = userID
	f.lastQuota = quota
	return nil
}
func (f *fakeNewAPI) ListUsageByUser(_ context.Context, start, end time.Time) ([]newapi.UsageRecord, error) {
	f.usageUserCalls++
	f.usageRanges = append(f.usageRanges, usageRange{Start: start, End: end})
	return append([]newapi.UsageRecord(nil), f.usageByUser...), nil
}
func (f *fakeNewAPI) ListUsageByModel(_ context.Context, _ time.Time, _ time.Time, username string) ([]newapi.UsageRecord, error) {
	if username == "" {
		return append([]newapi.UsageRecord(nil), f.usageByModel...), nil
	}
	result := make([]newapi.UsageRecord, 0)
	for _, record := range f.usageByModel {
		if strings.EqualFold(record.Username, username) {
			result = append(result, record)
		}
	}
	return result, nil
}
func (f *fakeNewAPI) ListLogs(_ context.Context, _ time.Time, _ time.Time, username string, _, pageSize int) (newapi.LogPage, error) {
	items := make([]newapi.LogRecord, 0, pageSize)
	for _, record := range f.logs {
		if username != "" && !strings.EqualFold(record.Username, username) {
			continue
		}
		items = append(items, record)
		if len(items) >= pageSize {
			break
		}
	}
	return newapi.LogPage{Items: items, Total: len(items)}, nil
}
func (f *fakeNewAPI) ListEnabledModels(context.Context) ([]string, error) {
	return append([]string(nil), f.models...), nil
}
func (f *fakeNewAPI) ListUserModels(context.Context, string) ([]string, error) {
	return append([]string(nil), f.models...), nil
}
func (f *fakeNewAPI) ManageUserStatus(_ context.Context, userID int, action string) error {
	f.lastQuotaUser = userID
	f.managedAction = action
	return nil
}
func (f *fakeNewAPI) CreateRedemptions(_ context.Context, name string, count int, quota int64, expires time.Time) ([]string, error) {
	keys := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := len(f.redemptions) + 1
		key := fmt.Sprintf("BENEFIT-%03d", id)
		keys = append(keys, key)
		f.redemptions = append(f.redemptions, newapi.Redemption{ID: id, Name: name, Key: key, Quota: quota, ExpiredTime: expires.Unix()})
	}
	return keys, nil
}
func (f *fakeNewAPI) SearchRedemptions(_ context.Context, name string, _ int) ([]newapi.Redemption, error) {
	result := make([]newapi.Redemption, 0)
	for _, item := range f.redemptions {
		if item.Name == name {
			result = append(result, item)
		}
	}
	return result, nil
}
func (f *fakeNewAPI) ListLogsByType(_ context.Context, start time.Time, end time.Time, username string, logType, _, pageSize int) (newapi.LogPage, error) {
	f.logQueryStarts = append(f.logQueryStarts, start)
	if f.logSplitAfter > 0 && end.Sub(start) > f.logSplitAfter {
		return newapi.LogPage{Items: []newapi.LogRecord{}, Total: maxBenefitLogPages*100 + 1}, nil
	}
	items := make([]newapi.LogRecord, 0, pageSize)
	for _, record := range f.logs {
		if logType != 0 && record.Type != logType {
			continue
		}
		if username != "" && !strings.EqualFold(record.Username, username) {
			continue
		}
		items = append(items, record)
		if len(items) >= pageSize {
			break
		}
	}
	return newapi.LogPage{Items: items, Total: len(items)}, nil
}
func (f *fakeNewAPI) ResetUser2FA(_ context.Context, userID int) error {
	f.reset2FA = userID
	return nil
}
func (f *fakeNewAPI) ResetUserPasskey(_ context.Context, userID int) error {
	f.resetPasskey = userID
	return nil
}
func (f *fakeNewAPI) ListUserSubscriptions(_ context.Context, userID int) ([]newapi.UserSubscriptionRecord, error) {
	records := f.subscriptions[userID]
	return append([]newapi.UserSubscriptionRecord(nil), records...), nil
}
func (f *fakeNewAPI) CreateUserSubscription(_ context.Context, userID, planID int) error {
	f.nextSubID++
	now := time.Now().Unix()
	record := newapi.UserSubscriptionRecord{Subscription: newapi.UserSubscription{
		ID: f.nextSubID, UserID: userID, PlanID: planID, Status: "active",
		StartTime: now, EndTime: now + 30*24*60*60, CreatedAt: now,
	}}
	f.subscriptions[userID] = append(f.subscriptions[userID], record)
	return nil
}
func (f *fakeNewAPI) InvalidateUserSubscription(_ context.Context, subscriptionID int) error {
	for userID, records := range f.subscriptions {
		for index := range records {
			if records[index].Subscription.ID == subscriptionID {
				records[index].Subscription.Status = "cancelled"
				records[index].Subscription.EndTime = time.Now().Unix()
				f.subscriptions[userID] = records
				return nil
			}
		}
	}
	return nil
}

type fakeQQ struct {
	mu              sync.Mutex
	messages        []string
	groupReplyErr   error
	groupReplyErrAt int
	groupReplies    int
	joinApprovals   []qq.GroupJoinRequest
	muteState       qq.GroupMuteState
	muteMember      string
	muteOperation   string
	muteExpiresAt   time.Time
}

func (f *fakeQQ) ReplyC2C(_ context.Context, _, _, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, content)
	return nil
}
func (f *fakeQQ) ReplyGroup(_ context.Context, _, _, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupReplies++
	if f.groupReplyErr != nil && (f.groupReplyErrAt == 0 || f.groupReplyErrAt == f.groupReplies) {
		return f.groupReplyErr
	}
	f.messages = append(f.messages, content)
	return nil
}
func (f *fakeQQ) ReviewGroupJoinRequest(_ context.Context, group, member, requestID, operation, _ string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinApprovals = append(f.joinApprovals, qq.GroupJoinRequest{GroupOpenID: group, MemberOpenID: member, JoinRequestID: requestID, ApplySource: operation})
	return nil
}
func (f *fakeQQ) GetGroupMuteState(context.Context, string) (qq.GroupMuteState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.muteState, nil
}
func (f *fakeQQ) SetGroupMemberMute(_ context.Context, _ string, member, operation string, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.muteMember = member
	f.muteOperation = operation
	f.muteExpiresAt = expiresAt
	return nil
}

type fakeMailer struct {
	code string
}

func (f *fakeMailer) SendVerification(_ context.Context, _, code string, _ time.Duration, _ string) error {
	f.code = code
	return nil
}

func testService(t *testing.T) (*Service, *store.Store, *fakeNewAPI, *fakeQQ, *fakeMailer) {
	t.Helper()
	storage, err := store.Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	box, err := secure.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		QQAdminOpenIDs:             map[string]struct{}{"user:admin": {}},
		CheckinEnabled:             true,
		CheckinCredit:              "1",
		CheckinPeriod:              "daily",
		CheckinTimezone:            time.UTC,
		CheckinCodeTTL:             24 * time.Hour,
		BindCodeTTL:                10 * time.Minute,
		BindCodeMaxAttempts:        5,
		BindEmailLimit:             2,
		BindEmailWindow:            time.Hour,
		LinkCodeTTL:                10 * time.Minute,
		CreditMaxPerCommand:        "1000",
		GatewayQueueSize:           64,
		GatewayWorkers:             2,
		MessageDedupTTL:            time.Hour,
		NewAPITimeout:              3 * time.Second,
		NotifyCheckInterval:        time.Hour,
		UsageChartEnabled:          true,
		NotifyEnabled:              true,
		AdminReportExportEnabled:   true,
		AdminUserManagementEnabled: true,
		BenefitEnabled:             true,
		BenefitMaxCount:            100,
		BenefitMaxBanDays:          365,
		BenefitCheckInterval:       time.Minute,
		ResetEnabled:               true,
		ResetPollInterval:          3 * time.Minute,
		ResetHTTPTimeout:           time.Second,
		ResetSignalMaxAge:          24 * time.Hour,
		ResetDefaultDuration:       5 * time.Hour,
		ResetDefaultWinners:        5,
		ResetDefaultLookback:       24 * time.Hour,
	}
	api := &fakeNewAPI{
		user:          newapi.User{ID: 42, Username: "alice", Email: "alice@example.com", Status: 1},
		users:         []newapi.User{{ID: 42, Username: "alice", Email: "alice@example.com", Status: 1}},
		subscriptions: make(map[int][]newapi.UserSubscriptionRecord),
	}
	qqAPI := &fakeQQ{}
	mail := &fakeMailer{}
	service := New(cfg, storage, box, api, qqAPI, mail, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	return service, storage, api, qqAPI, mail
}

func c2cEvent(user, content string) qq.MessageEvent {
	return qq.MessageEvent{EventType: "C2C_MESSAGE_CREATE", Message: qq.Message{ID: "m-" + content, Content: content, Author: qq.MessageAuthor{UserOpenID: user}}}
}

func groupEvent(group, member, content string) qq.MessageEvent {
	return qq.MessageEvent{
		EventType: "GROUP_MESSAGE_CREATE",
		Message: qq.Message{
			ID:          "m-" + content,
			Content:     content,
			GroupOpenID: group,
			Author:      qq.MessageAuthor{MemberOpenID: member},
		},
	}
}

func lastReply(t *testing.T, api *fakeQQ) string {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.messages) == 0 {
		t.Fatal("expected a bot reply")
	}
	return api.messages[len(api.messages)-1]
}

func replyCount(api *fakeQQ) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return len(api.messages)
}

func TestCommandRuleListsAreAvailableWithoutBinding(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)

	service.process(context.Background(), c2cEvent("ordinary", "/disable list"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "当前没有明确设置为禁用") {
		t.Fatalf("unexpected disabled list reply: %q", reply)
	}
	service.process(context.Background(), c2cEvent("ordinary", "/enable list"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "当前没有明确设置为启用") {
		t.Fatalf("unexpected enabled list reply: %q", reply)
	}
}

func TestCommandInputAcceptsAngleBracketArgumentsWithoutSpaces(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)

	service.process(context.Background(), c2cEvent("ordinary", "/whoami<>"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "user:ordinary") {
		t.Fatalf("angle brackets were not normalized for a command without arguments: %q", reply)
	}

	service.process(context.Background(), c2cEvent("admin", `/disable<"bind view">`))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已禁用") || !strings.Contains(reply, "bind view") {
		t.Fatalf("angle brackets were not normalized for command arguments: %q", reply)
	}
}

func TestOnlyAdminCanManageCommandRules(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)

	service.process(context.Background(), c2cEvent("ordinary", `/disable "usage"`))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "没有管理命令启用状态的权限") {
		t.Fatalf("unexpected permission reply: %q", reply)
	}
	if _, err := storage.GetCommandRule("usage"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-admin unexpectedly saved a command rule: %v", err)
	}
}

func TestAdminCanDisableAndEnableCommandKeyword(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)

	service.process(context.Background(), c2cEvent("admin", `/disable "bind view"`))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已禁用") || !strings.Contains(reply, "bind view") {
		t.Fatalf("unexpected disable reply: %q", reply)
	}
	rule, err := storage.GetCommandRule("bind view")
	if err != nil || rule.Enabled {
		t.Fatalf("disabled rule not stored: %#v err=%v", rule, err)
	}

	before := replyCount(qqAPI)
	service.process(context.Background(), c2cEvent("ordinary", "/bind   view 123"))
	if got := replyCount(qqAPI); got != before {
		t.Fatalf("disabled command replied: before=%d after=%d", before, got)
	}

	service.process(context.Background(), c2cEvent("ordinary", "/disable list"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "bind view") {
		t.Fatalf("disabled list omitted rule: %q", reply)
	}

	service.process(context.Background(), c2cEvent("admin", `/enable "bind view"`))
	rule, err = storage.GetCommandRule("bind view")
	if err != nil || !rule.Enabled {
		t.Fatalf("enabled rule not stored: %#v err=%v", rule, err)
	}
	before = replyCount(qqAPI)
	service.process(context.Background(), c2cEvent("ordinary", "/bind view 123"))
	if got := replyCount(qqAPI); got != before+1 {
		t.Fatalf("re-enabled command did not reply: before=%d after=%d", before, got)
	}
	service.process(context.Background(), c2cEvent("ordinary", "/enable list"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "bind view") {
		t.Fatalf("enabled list omitted rule: %q", reply)
	}
}

func TestDisabledKeywordIsRemovedFromHelp(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)

	service.process(context.Background(), c2cEvent("admin", `/disable "usage"`))
	service.process(context.Background(), c2cEvent("ordinary", "/help"))
	reply := lastReply(t, qqAPI)
	if strings.Contains(reply, "/usage") {
		t.Fatalf("help still contains disabled usage commands: %q", reply)
	}
	if !strings.Contains(reply, "/bind") {
		t.Fatalf("help unexpectedly removed unrelated commands: %q", reply)
	}
}

func TestNonCommandMessageIsIgnored(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "你好"))
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if len(qqAPI.messages) != 0 {
		t.Fatalf("non-command message unexpectedly received %d replies", len(qqAPI.messages))
	}
}

func TestHelpOmitsDisabledFeatures(t *testing.T) {
	text := helpText(config.Config{})
	for _, command := range []string{"/usage chart", "/notify", "/admin report export", "/admin user"} {
		if strings.Contains(text, command) {
			t.Fatalf("disabled command %q appeared in help: %q", command, text)
		}
	}
}

func TestUnknownSlashCommandGetsReply(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "/unknown"))
	if reply := lastReply(t, qqAPI); reply == "" {
		t.Fatal("slash command should always receive a reply")
	}
}

func TestOversizedCommandIsCollapsedBeforeQueueing(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.Start(ctx)
	event := groupEvent("g1", "u1", "/"+strings.Repeat("x", maxCommandBytes+1))
	if accepted := service.HandleGateway(ctx, event); !accepted {
		t.Fatal("oversized command was not accepted for a bounded error reply")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && replyCount(qqAPI) == 0 {
		time.Sleep(time.Millisecond)
	}
	service.Stop()
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "4096") {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

func TestNonCommandBypassesPersistentDedup(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	event := groupEvent("g1", "u1", "hello")
	if accepted := service.HandleGateway(context.Background(), event); !accepted {
		t.Fatal("non-command event should be intentionally accepted")
	}
	key := event.EventType + "|" + event.Message.ID + "|"
	duplicate, err := storage.CheckAndMarkMessage(key, time.Now(), time.Hour)
	if err != nil || duplicate {
		t.Fatalf("non-command touched persistent dedup: duplicate=%v err=%v", duplicate, err)
	}
}

func TestQueueBackpressureSpillsToPersistentInbox(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	for index := 0; index < cap(service.queue); index++ {
		event := groupEvent("g1", "u1", fmt.Sprintf("/queued-%d", index))
		if accepted := service.HandleGateway(context.Background(), event); !accepted {
			t.Fatalf("queue rejected event %d before reaching capacity", index)
		}
	}
	overflow := groupEvent("g1", "u1", "/overflow")
	if accepted := service.HandleGateway(context.Background(), overflow); !accepted {
		t.Fatal("full memory queue should accept a durably stored event")
	}
	if accepted := service.HandleGateway(context.Background(), overflow); !accepted {
		t.Fatal("duplicate pending event should be acknowledged")
	}
	pending, err := storage.ListPendingGatewayEvents(cap(service.queue) + 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != cap(service.queue)+1 {
		t.Fatalf("pending events=%d, want %d", len(pending), cap(service.queue)+1)
	}
}

func TestPendingGatewayEventIsRecoveredByDispatcher(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	event := groupEvent("g1", "u1", "/help")
	if accepted := service.HandleGateway(context.Background(), event); !accepted {
		t.Fatal("event was not accepted")
	}
	queued := <-service.queue
	service.releaseGatewayEvent(queued.key)

	ctx, cancel := context.WithCancel(context.Background())
	service.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for replyCount(qqAPI) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	service.Stop()
	cancel()
	if replyCount(qqAPI) == 0 {
		t.Fatal("recovered event was not processed")
	}
	pending, err := storage.ListPendingGatewayEvents(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("completed event remained in inbox: %#v", pending)
	}
}

func TestPendingGatewayEventPayloadIsEncrypted(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	event := groupEvent("g1", "u1", "/bind verify 123456")
	if accepted := service.HandleGateway(context.Background(), event); !accepted {
		t.Fatal("event was not accepted")
	}
	items, err := storage.ListPendingGatewayEvents(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("pending events=%d, want 1", len(items))
	}
	if bytes.Contains(items[0].Payload, []byte("123456")) || bytes.Contains(items[0].Payload, []byte("bind verify")) {
		t.Fatalf("pending event contains plaintext command: %q", items[0].Payload)
	}
	plaintext, err := service.secure.Decrypt(string(items[0].Payload))
	if err != nil || !strings.Contains(plaintext, "123456") {
		t.Fatalf("encrypted payload could not be restored: plaintext=%q err=%v", plaintext, err)
	}
}

func TestBindMissingArgumentShowsExactUsage(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "/bind"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "/bind <邮箱或New API用户ID>") {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

func TestWelcomeAndMemberAdd(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.CreateBinding(model.Binding{CanonicalID: "user:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	event := groupEvent("g1", "u-admin", "/welcome set 欢迎使用机器人")
	event.Message.Author.UserOpenID = "admin"
	service.process(context.Background(), event)
	service.process(context.Background(), qq.MessageEvent{EventType: "GROUP_MEMBER_ADD", Member: qq.GroupMemberEvent{GroupOpenID: "g1", MemberOpenID: "new-user", Timestamp: time.Now().Unix()}})
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "欢迎使用机器人") || !strings.Contains(reply, `<qqbot-at-user id="new-user" />`) {
		t.Fatalf("unexpected welcome reply: %q", reply)
	}
}

func TestGroupJoinApprovalMatchesNewAPIEmailAndID(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.PutGroupJoinApproval(model.GroupJoinApproval{GroupOpenID: "g1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for index, answer := range []string{"alice@example.com", "42"} {
		service.process(context.Background(), qq.MessageEvent{EventType: "GROUP_JOIN_REQUEST", JoinRequest: qq.GroupJoinRequest{
			GroupOpenID: "g1", MemberOpenID: fmt.Sprintf("member-%d", index), JoinRequestID: fmt.Sprintf("request-%d", index),
			VerifyInfo: qq.GroupJoinVerifyInfo{Method: "verify_message", VerifyMessage: answer},
		}})
	}
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if len(qqAPI.joinApprovals) != 2 {
		t.Fatalf("join approvals=%d, want 2", len(qqAPI.joinApprovals))
	}
	for _, approval := range qqAPI.joinApprovals {
		if approval.GroupOpenID != "g1" || approval.JoinRequestID == "" || approval.ApplySource != "approve" {
			t.Fatalf("unexpected approval: %#v", approval)
		}
	}
}

func TestGroupJoinApprovalLeavesUnsafeOrUnknownRequestsPending(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.PutGroupJoinApproval(model.GroupJoinApproval{GroupOpenID: "g1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	requests := []qq.GroupJoinRequest{
		{GroupOpenID: "g1", MemberOpenID: "unsafe", JoinRequestID: "r1", RiskTips: "warning", VerifyInfo: qq.GroupJoinVerifyInfo{VerifyMessage: "42"}},
		{GroupOpenID: "g1", MemberOpenID: "unknown", JoinRequestID: "r2", VerifyInfo: qq.GroupJoinVerifyInfo{VerifyMessage: "not-an-account"}},
		{GroupOpenID: "g2", MemberOpenID: "disabled-group", JoinRequestID: "r3", VerifyInfo: qq.GroupJoinVerifyInfo{VerifyMessage: "42"}},
	}
	for _, request := range requests {
		service.process(context.Background(), qq.MessageEvent{EventType: "GROUP_JOIN_REQUEST", JoinRequest: request})
	}
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if len(qqAPI.joinApprovals) != 0 {
		t.Fatalf("unexpected approvals: %#v", qqAPI.joinApprovals)
	}
}

func TestJoinCommandConfiguresCurrentGroup(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), groupEvent("g1", "admin", "/join on"))
	setting, err := storage.GetGroupJoinApproval("g1")
	if err != nil || !setting.Enabled {
		t.Fatalf("join setting=%#v err=%v", setting, err)
	}
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已开启") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g1", "admin", "/join status"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "自动审批：开启") {
		t.Fatalf("unexpected status: %q", reply)
	}
}

func TestJoinCommandConfiguresLevelAndContentLimits(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	service.process(context.Background(), groupEvent("g1", "admin", "/join limit 20"))
	service.process(context.Background(), groupEvent("g1", "admin", `/join check "内部 用户"`))
	setting, err := storage.GetGroupJoinApproval("g1")
	if err != nil {
		t.Fatal(err)
	}
	if setting.MinQQLevel != 20 || setting.MatchText != "内部 用户" {
		t.Fatalf("unexpected setting: %#v", setting)
	}

	service.process(context.Background(), groupEvent("g1", "admin", "/join status"))
	status := lastReply(t, qqAPI)
	if !strings.Contains(status, "至少 20 级") || !strings.Contains(status, `"内部 用户"`) {
		t.Fatalf("unexpected status: %q", status)
	}

	service.process(context.Background(), groupEvent("g1", "admin", "/join limit 0"))
	service.process(context.Background(), groupEvent("g1", "admin", `/join check ""`))
	setting, err = storage.GetGroupJoinApproval("g1")
	if err != nil || setting.MinQQLevel != 0 || setting.MatchText != "" {
		t.Fatalf("limits were not cleared: setting=%#v err=%v", setting, err)
	}
}

func TestJoinCommandRejectsInvalidLevelAndUnquotedCheck(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	service.process(context.Background(), groupEvent("g1", "admin", "/join limit -1"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "非负整数") {
		t.Fatalf("unexpected level error: %q", reply)
	}
	service.process(context.Background(), groupEvent("g1", "admin", "/join check unquoted"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, `/join check "<匹配字符串>"`) {
		t.Fatalf("unexpected check error: %q", reply)
	}
}

func TestGroupJoinApprovalRequiresConfiguredLevelAndContent(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.PutGroupJoinApproval(model.GroupJoinApproval{GroupOpenID: "g1", Enabled: true, MinQQLevel: 20, MatchText: "内部用户"}); err != nil {
		t.Fatal(err)
	}
	requests := []qq.GroupJoinRequest{
		{
			GroupOpenID: "g1", MemberOpenID: "approved", JoinRequestID: "r1", QQLevel: qq.OptionalInt{Value: 25, Set: true},
			VerifyInfo: qq.GroupJoinVerifyInfo{ReviewQAList: []qq.GroupJoinReviewQA{{Answer: "alice@example.com"}, {Answer: "我是内部用户"}}},
		},
		{
			GroupOpenID: "g1", MemberOpenID: "low-level", JoinRequestID: "r2", QQLevel: qq.OptionalInt{Value: 19, Set: true},
			VerifyInfo: qq.GroupJoinVerifyInfo{ReviewQAList: []qq.GroupJoinReviewQA{{Answer: "alice@example.com"}, {Answer: "我是内部用户"}}},
		},
		{
			GroupOpenID: "g1", MemberOpenID: "missing-level", JoinRequestID: "r3",
			VerifyInfo: qq.GroupJoinVerifyInfo{ReviewQAList: []qq.GroupJoinReviewQA{{Answer: "alice@example.com"}, {Answer: "我是内部用户"}}},
		},
		{
			GroupOpenID: "g1", MemberOpenID: "wrong-content", JoinRequestID: "r4", QQLevel: qq.OptionalInt{Value: 25, Set: true},
			VerifyInfo: qq.GroupJoinVerifyInfo{ReviewQAList: []qq.GroupJoinReviewQA{{Answer: "alice@example.com"}, {Answer: "外部用户"}}},
		},
	}
	for _, request := range requests {
		service.process(context.Background(), qq.MessageEvent{EventType: "GROUP_JOIN_REQUEST", JoinRequest: request})
	}
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if len(qqAPI.joinApprovals) != 1 || qqAPI.joinApprovals[0].MemberOpenID != "approved" {
		t.Fatalf("unexpected approvals: %#v", qqAPI.joinApprovals)
	}
}

func TestMuteCommandsUseOfficialGroupModerationAPI(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	event := groupEvent("g1", "admin", "/mute @alice 2h")
	event.Message.Mentions = []qq.MessageAuthor{{MemberOpenID: "target", Username: "alice"}}
	service.process(context.Background(), event)
	qqAPI.mu.Lock()
	if qqAPI.muteMember != "target" || qqAPI.muteOperation != "add" || time.Until(qqAPI.muteExpiresAt) < 119*time.Minute {
		qqAPI.mu.Unlock()
		t.Fatalf("unexpected mute call: member=%q operation=%q expires=%s", qqAPI.muteMember, qqAPI.muteOperation, qqAPI.muteExpiresAt)
	}
	qqAPI.mu.Unlock()
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "禁言 2 小时") {
		t.Fatalf("unexpected mute reply: %q", reply)
	}

	event.Message.ID = "mute-off"
	event.Message.Content = "/mute off @alice"
	service.process(context.Background(), event)
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if qqAPI.muteOperation != "del" || qqAPI.muteMember != "target" {
		t.Fatalf("unexpected unmute call: member=%q operation=%q", qqAPI.muteMember, qqAPI.muteOperation)
	}
}

func TestAdminDisableRequiresConfirmation(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	if err := storage.CreateBinding(model.Binding{CanonicalID: "user:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	event := groupEvent("g1", "u-admin", "/admin user disable 42")
	event.Message.Author.UserOpenID = "admin"
	service.process(context.Background(), event)
	reply := lastReply(t, qqAPI)
	marker := "/confirm "
	index := strings.Index(reply, marker)
	if index < 0 {
		t.Fatalf("missing confirmation: %q", reply)
	}
	code := strings.Fields(reply[index+len(marker):])[0]
	event.Message.Content = "/confirm " + strings.TrimSuffix(code, "。")
	event.Message.ID = "m-confirm"
	service.process(context.Background(), event)
	if api.managedAction != "disable" || api.lastQuotaUser != 42 {
		t.Fatalf("action not executed: %s %d", api.managedAction, api.lastQuotaUser)
	}
}

func TestBenefitCreatesCodesAndEnforcesSingleClaim(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	if err := storage.CreateBinding(model.Binding{CanonicalID: "user:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	event := groupEvent("g1", "u-admin", "/benefit 1 3 24 7")
	event.Message.Author.UserOpenID = "admin"
	service.process(context.Background(), event)
	reply := lastReply(t, qqAPI)
	if !strings.Contains(reply, "@everyone") || strings.Contains(reply, "<@everyone>") || !strings.Contains(reply, "每个 $1，共 3 个") || !strings.Contains(reply, "每个用户限领一个") {
		t.Fatalf("unexpected benefit reply: %q", reply)
	}
	campaign, err := storage.GetBenefitCampaign(benefitCampaignID(event.Message.ID))
	if err != nil || len(campaign.RedemptionIDs) != 3 {
		t.Fatalf("campaign not stored: %#v err=%v", campaign, err)
	}
	campaign.CreatedAt = campaign.CreatedAt.Add(-2 * time.Minute)
	api.logs = []newapi.LogRecord{{ID: 1, UserID: 9, Type: 1, Content: fmt.Sprintf("通过兑换码充值，兑换码ID %d", campaign.RedemptionIDs[0])}, {ID: 2, UserID: 9, Type: 1, Content: fmt.Sprintf("通过兑换码充值，兑换码ID %d", campaign.RedemptionIDs[1])}}
	if err := service.detectBenefitViolations(context.Background(), &campaign); err != nil {
		t.Fatal(err)
	}
	if api.managedAction != "disable" || api.lastQuotaUser != 9 {
		t.Fatalf("violator not disabled: %s %d", api.managedAction, api.lastQuotaUser)
	}
	ban, err := storage.GetBenefitBan(campaign.ID, 9)
	if err != nil || ban.Status != "disabled" {
		t.Fatalf("ban not stored: %#v err=%v", ban, err)
	}
	firstCheckedAt := campaign.LastCheckedAt
	if firstCheckedAt.IsZero() || len(campaign.ClaimedCodes[9]) != 2 {
		t.Fatalf("campaign scan state not persisted: %#v", campaign)
	}
	if err := service.detectBenefitViolations(context.Background(), &campaign); err != nil {
		t.Fatal(err)
	}
	if len(api.logQueryStarts) < 2 {
		t.Fatalf("benefit scan did not run twice: starts=%v", api.logQueryStarts)
	}
	secondStart := api.logQueryStarts[len(api.logQueryStarts)-1]
	if secondStart.Before(campaign.CreatedAt) || secondStart.After(firstCheckedAt) {
		t.Fatalf("benefit scan overlap is outside the safe window: start=%v created=%v checked=%v", secondStart, campaign.CreatedAt, firstCheckedAt)
	}
	ban.EnableAt = time.Now().Add(-time.Minute)
	if err := storage.PutBenefitBan(ban); err != nil {
		t.Fatal(err)
	}
	service.processBenefitBans(context.Background())
	if api.managedAction != "enable" {
		t.Fatalf("user not re-enabled: %s", api.managedAction)
	}
}

func TestBenefitLogScanSplitsOversizedTimeRange(t *testing.T) {
	service, _, api, _, _ := testService(t)
	api.logSplitAfter = time.Second
	start := time.Unix(100, 0)
	if err := service.scanBenefitLogs(context.Background(), start, start.Add(4*time.Second), map[int]struct{}{1: {}}, make(map[int]map[int]struct{})); err != nil {
		t.Fatal(err)
	}
	if len(api.logQueryStarts) < 3 {
		t.Fatalf("oversized log range was not split: starts=%v", api.logQueryStarts)
	}
}

func TestBenefitCampaignKeepsFinalScanGrace(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	now := time.Now()
	campaign := model.BenefitCampaign{
		ID: "benefit-grace", GroupOpenID: "g1", Status: "active", Announced: true,
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute), RedemptionIDs: []int{1},
	}
	if err := storage.PutBenefitCampaign(campaign); err != nil {
		t.Fatal(err)
	}
	service.processBenefitCampaign(context.Background(), campaign)
	stored, err := storage.GetBenefitCampaign(campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "active" || len(stored.RedemptionIDs) != 1 {
		t.Fatalf("campaign expired before ingestion grace: %#v", stored)
	}
	stored.ExpiresAt = now.Add(-benefitLogIngestionGrace - time.Minute)
	if err := storage.PutBenefitCampaign(stored); err != nil {
		t.Fatal(err)
	}
	service.processBenefitCampaign(context.Background(), stored)
	stored, err = storage.GetBenefitCampaign(campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "expired" || len(stored.RedemptionIDs) != 0 {
		t.Fatalf("campaign did not expire after ingestion grace: %#v", stored)
	}
}

func TestUnauthorizedAdminCommandRepliesPermissionDenied(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	canonical := "member:g1:u1"
	if err := storage.CreateBinding(model.Binding{CanonicalID: canonical, NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), groupEvent("g1", "u1", "/credit show 42"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "权限") {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

func TestBindFlow(t *testing.T) {
	service, storage, _, _, mail := testService(t)
	service.process(context.Background(), c2cEvent("u1", "/bind alice@example.com"))
	if mail.code == "" {
		t.Fatal("verification email was not sent")
	}
	service.process(context.Background(), c2cEvent("u1", "/bind verify "+mail.code))
	binding, err := storage.GetBinding("user:u1")
	if err != nil {
		t.Fatal(err)
	}
	if binding.NewAPIID != 42 {
		t.Fatalf("binding user=%d", binding.NewAPIID)
	}
}

func TestGroupBindFlow(t *testing.T) {
	service, storage, _, qqAPI, mail := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "/bind alice@example.com"))
	if mail.code == "" {
		t.Fatal("verification email was not sent")
	}
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "当前群") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g1", "u1", "/bind verify "+mail.code))
	binding, err := storage.GetBinding("member:g1:u1")
	if err != nil {
		t.Fatal(err)
	}
	if binding.NewAPIID != 42 {
		t.Fatalf("binding user=%d", binding.NewAPIID)
	}
}

func TestBindStatusAndSelfUnbind(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	canonical := "member:g1:u1"
	if err := storage.CreateBinding(model.Binding{CanonicalID: canonical, NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), groupEvent("g1", "u1", "/bind status"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "New API 用户 ID：42") {
		t.Fatalf("unexpected status reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g1", "u1", "/unbind"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已解除") {
		t.Fatalf("unexpected unbind reply: %q", reply)
	}
	if _, err := storage.GetBinding(canonical); err == nil {
		t.Fatal("binding still exists after /unbind")
	}
}

func TestCreditMentionTargetAndSubtractFloor(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 7, Email: "admin@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:target", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	event := groupEvent("g1", "admin", "/credit add @alice 2")
	event.Message.Mentions = []qq.MessageAuthor{{MemberOpenID: "target", Username: "alice"}}
	service.process(context.Background(), event)
	if api.quotaAdds != 1 || api.lastQuotaUser != 42 || api.lastQuota != 1000000 {
		t.Fatalf("mention add: adds=%d user=%d quota=%d", api.quotaAdds, api.lastQuotaUser, api.lastQuota)
	}

	api.user.Quota = 500000
	event.Message.ID = "sub-too-much"
	event.Message.Content = "/credit sub @alice 2"
	service.process(context.Background(), event)
	if api.quotaSubs != 0 {
		t.Fatalf("subtract executed despite negative result: %d", api.quotaSubs)
	}
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "余额将为负数") {
		t.Fatalf("unexpected subtract rejection: %q", reply)
	}

	event.Message.ID = "sub-ok"
	event.Message.Content = "/credit sub @alice 1"
	service.process(context.Background(), event)
	if api.quotaSubs != 1 || api.lastQuotaUser != 42 || api.lastQuota != 500000 {
		t.Fatalf("subtract: subs=%d user=%d quota=%d", api.quotaSubs, api.lastQuotaUser, api.lastQuota)
	}
}

func TestAdminUnbindSupportsMentionTarget(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 7, Email: "admin@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:target", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	event := groupEvent("g1", "admin", "/admin unbind @alice")
	event.Message.Mentions = []qq.MessageAuthor{{MemberOpenID: "target", Username: "alice"}}
	service.process(context.Background(), event)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已解除被 @ 用户绑定的 New API 用户 42") {
		t.Fatalf("unexpected admin unbind reply: %q", reply)
	}
	if _, err := storage.GetBinding("member:g1:target"); err == nil {
		t.Fatal("binding still exists after mention-based admin unbind")
	}
}

func TestBindDoesNotTreatMentionAsUserTarget(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	event := groupEvent("g1", "u1", "/bind @alice")
	event.Message.Mentions = []qq.MessageAuthor{{MemberOpenID: "target", Username: "alice"}}
	service.process(context.Background(), event)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "/bind 不支持使用 @群成员") {
		t.Fatalf("unexpected bind mention reply: %q", reply)
	}
}

func TestPlanAdminAndSelfViewFlow(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 7, Email: "admin@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:target", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	event := groupEvent("g1", "admin", "/plan add 3 @alice")
	event.Message.Mentions = []qq.MessageAuthor{{MemberOpenID: "target", Username: "alice"}}
	service.process(context.Background(), event)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "订阅添加成功") || !strings.Contains(reply, "当前订阅编号：1") {
		t.Fatalf("unexpected add reply: %q", reply)
	}
	newer := time.Now().Unix() + 10
	api.subscriptions[42] = append(api.subscriptions[42], newapi.UserSubscriptionRecord{Subscription: newapi.UserSubscription{
		ID: 2, UserID: 42, PlanID: 4, Status: "expired", StartTime: newer, EndTime: newer + 60, CreatedAt: newer,
	}})
	api.nextSubID = 2

	service.process(context.Background(), groupEvent("g1", "target", "/plan view"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "订阅编号：1") || !strings.Contains(reply, "订阅编号：2") || !strings.Contains(reply, "生效中") || !strings.Contains(reply, "开始时间") || !strings.Contains(reply, "结束时间") {
		t.Fatalf("unexpected self view reply: %q", reply)
	} else if strings.Index(reply, "订阅编号：2") > strings.Index(reply, "订阅编号：1") {
		t.Fatalf("subscriptions are not sorted newest first: %q", reply)
	}

	event.Message.ID = "plan-sub"
	event.Message.Content = "/plan sub 1 @alice"
	service.process(context.Background(), event)
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "取消订阅成功") || !strings.Contains(reply, "当前订阅编号：1") {
		t.Fatalf("unexpected sub reply: %q", reply)
	}
	if got := api.subscriptions[42][0].Subscription.Status; got != "cancelled" {
		t.Fatalf("subscription status=%q", got)
	}
}

func TestPlanViewOtherRequiresAdmin(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:u1", NewAPIID: 9, Email: "u1@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), groupEvent("g1", "u1", "/plan view 42"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "没有查看其他用户订阅的权限") {
		t.Fatalf("unexpected permission reply: %q", reply)
	}
}

func TestUsageSelfSummaryAndRanking(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:u1", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	api.user = newapi.User{ID: 42, Username: "alice", Quota: 2500000, Status: 1}
	api.users = []newapi.User{
		api.user,
		{ID: 43, Username: "bob", Quota: 1000000, Status: 1},
	}
	api.usageByUser = []newapi.UsageRecord{
		{Username: "alice", Count: 3, TokenUsed: 1200, Quota: 500000},
		{Username: "bob", Count: 2, TokenUsed: 800, Quota: 250000},
	}
	api.usageByModel = []newapi.UsageRecord{
		{Username: "alice", ModelName: "gpt-test", Count: 3, TokenUsed: 1200, Quota: 500000},
	}

	service.process(context.Background(), groupEvent("g1", "u1", "/usage 7d"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "请求次数：3") || !strings.Contains(reply, "消耗额度：1") || !strings.Contains(reply, "gpt-test") {
		t.Fatalf("unexpected self usage reply: %q", reply)
	}

	service.process(context.Background(), groupEvent("g1", "u1", "/usage 7d all"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "全站用量汇总") || !strings.Contains(reply, "请求次数：5") || !strings.Contains(reply, "Token 用量：2000") || !strings.Contains(reply, "消耗额度：1.5") || strings.Contains(reply, "ID 42") {
		t.Fatalf("unexpected all usage summary reply: %q", reply)
	}

	service.process(context.Background(), groupEvent("g1", "u1", "/usage 7d 2"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "用量排行榜") || !strings.Contains(reply, "ID 42｜alice") || !strings.Contains(reply, "ID 43｜bob") {
		t.Fatalf("unexpected usage ranking reply: %q", reply)
	}
}

func TestLogsModelsAndAdminReport(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	if err := storage.CreateBinding(model.Binding{CanonicalID: "member:g1:admin", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	api.user = newapi.User{ID: 42, Username: "alice", Quota: 2000000, Group: "default", Status: 1}
	api.users = []newapi.User{api.user}
	api.logs = []newapi.LogRecord{{UserID: 42, Username: "alice", CreatedAt: time.Now().Unix(), ModelName: "gpt-test", PromptTokens: 10, CompletionTokens: 5, Quota: 500000, UseTime: 2}}
	api.models = []string{"gpt-test", "claude-test"}
	api.usageByUser = []newapi.UsageRecord{{Username: "alice", Count: 1, TokenUsed: 15, Quota: 500000}}
	api.usageByModel = []newapi.UsageRecord{{Username: "alice", ModelName: "gpt-test", Count: 1, TokenUsed: 15, Quota: 500000}}

	service.process(context.Background(), groupEvent("g1", "admin", "/logs 5"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "gpt-test") || !strings.Contains(reply, "输入 10 / 输出 5") {
		t.Fatalf("unexpected logs reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g1", "admin", "/models"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "claude-test") || !strings.Contains(reply, "用户分组：default") {
		t.Fatalf("unexpected models reply: %q", reply)
	}
	service.process(context.Background(), groupEvent("g1", "admin", "/admin report 7d"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "全站用量报告") || !strings.Contains(reply, "热门模型") {
		t.Fatalf("unexpected report reply: %q", reply)
	}
}

func TestQuotaNotificationSendsOnceAndRearms(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	api.user = newapi.User{ID: 42, Username: "alice", DisplayName: "Alice", Quota: 250000, Status: 1}
	api.users = []newapi.User{api.user}
	preference := model.QuotaNotification{CanonicalID: "member:g1:u1", NewAPIID: 42, GroupOpenID: "g1", Threshold: "1", Enabled: true}
	if err := storage.PutQuotaNotification(preference); err != nil {
		t.Fatal(err)
	}
	service.checkQuotaNotifications(context.Background())
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "额度提醒") || !strings.Contains(reply, "Alice") {
		t.Fatalf("unexpected notify reply: %q", reply)
	}
	saved, err := storage.GetQuotaNotification("member:g1:u1")
	if err != nil || !saved.Alerted {
		t.Fatalf("notification not marked alerted: %#v err=%v", saved, err)
	}
	messageCount := len(qqAPI.messages)
	service.checkQuotaNotifications(context.Background())
	if len(qqAPI.messages) != messageCount {
		t.Fatal("duplicate quota notification was sent")
	}
	api.user.Quota = 1000000
	service.checkQuotaNotifications(context.Background())
	saved, _ = storage.GetQuotaNotification("member:g1:u1")
	if saved.Alerted {
		t.Fatal("quota notification was not rearmed after balance recovery")
	}
}

func TestDailyNotificationsShareOneUsageQuery(t *testing.T) {
	service, storage, api, _, _ := testService(t)
	service.cfg.NotifyDailyTime = time.Now().UTC().Format("15:04")
	api.users = []newapi.User{
		{ID: 42, Username: "alice", Quota: 500000},
		{ID: 43, Username: "bob", Quota: 500000},
	}
	api.usageByUser = []newapi.UsageRecord{
		{Username: "alice", Count: 2, TokenUsed: 10, Quota: 100},
		{Username: "bob", Count: 3, TokenUsed: 20, Quota: 200},
	}
	for _, preference := range []model.QuotaNotification{
		{CanonicalID: "member:g1:u1", NewAPIID: 42, GroupOpenID: "g1", DailyEnabled: true},
		{CanonicalID: "member:g2:u2", NewAPIID: 43, GroupOpenID: "g2", DailyEnabled: true},
	} {
		if err := storage.PutQuotaNotification(preference); err != nil {
			t.Fatal(err)
		}
	}
	service.checkQuotaNotifications(context.Background())
	if api.usageUserCalls != 1 {
		t.Fatalf("daily usage queries=%d, want 1", api.usageUserCalls)
	}
}

func TestParseInsightRange(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	start, end, _, err := parseInsightRange("7d", now, time.UTC)
	if err != nil || !end.Equal(now) || end.Sub(start) != 7*24*time.Hour {
		t.Fatalf("7d range start=%v end=%v err=%v", start, end, err)
	}
	if _, _, _, err := parseInsightRange("32d", now, time.UTC); err == nil {
		t.Fatal("expected range limit error")
	}
	for _, value := range []string{"10", "top10", "前10名"} {
		count, err := parseUsageRank(value)
		if err != nil || count != 10 {
			t.Fatalf("rank %q count=%d err=%v", value, count, err)
		}
	}
	if _, err := parseUsageRank("101"); err == nil {
		t.Fatal("expected rank limit error")
	}
}

func TestCheckinIsIdempotent(t *testing.T) {
	service, storage, api, _, _ := testService(t)
	now := time.Now()
	if err := storage.CreateBinding(model.Binding{CanonicalID: "user:u1", NewAPIID: 42, Email: "alice@example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), c2cEvent("u1", "/checkin"))
	service.process(context.Background(), c2cEvent("u1", "/checkin"))
	if api.quotaAdds != 1 {
		t.Fatalf("quota adds=%d", api.quotaAdds)
	}
	if api.lastQuotaUser != 42 || api.lastQuota != 500000 {
		t.Fatalf("quota user=%d raw=%d", api.lastQuotaUser, api.lastQuota)
	}
}

func TestCheckinResponseHeaderTimeoutStaysPendingWithoutRetry(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	now := time.Now()
	if err := storage.CreateBinding(model.Binding{CanonicalID: "user:u1", NewAPIID: 42, Email: "alice@example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	api.addQuotaErr = &newapi.APIError{Message: `Post "https://example.com/api/user/manage": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`}

	service.process(context.Background(), c2cEvent("u1", "/checkin"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "发放结果待确认") {
		t.Fatalf("unexpected timeout reply: %q", reply)
	}
	period, _ := periodKey(time.Now(), service.cfg.CheckinPeriod, service.cfg.CheckinTimezone)
	record, err := storage.GetCheckin("user:u1", period)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "pending_confirmation" || record.LastError == "" {
		t.Fatalf("unexpected checkin record: %#v", record)
	}

	api.addQuotaErr = nil
	service.process(context.Background(), c2cEvent("u1", "/checkin"))
	if api.quotaAdds != 1 {
		t.Fatalf("ambiguous request was retried: quota adds=%d", api.quotaAdds)
	}
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "发放结果待确认") {
		t.Fatalf("unexpected pending reply: %q", reply)
	}
}

func TestCheckinTruncatedHTTP200StaysPendingWithoutRetry(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	if err := storage.CreateBinding(model.Binding{CanonicalID: "user:u1", NewAPIID: 42, Email: "alice@example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	api.addQuotaErr = &newapi.APIError{StatusCode: 200, Message: "解析响应失败", Cause: io.ErrUnexpectedEOF}
	service.process(context.Background(), c2cEvent("u1", "/checkin"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "发放结果待确认") {
		t.Fatalf("unexpected truncated response reply: %q", reply)
	}
	period, _ := periodKey(time.Now(), service.cfg.CheckinPeriod, service.cfg.CheckinTimezone)
	record, err := storage.GetCheckin("user:u1", period)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "pending_confirmation" {
		t.Fatalf("checkin status=%q, want pending_confirmation", record.Status)
	}
}

func TestUsageChartBusyFailsFast(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.chartSemaphore <- struct{}{}
	start := time.Now()
	err := service.handleUsageChart(context.Background(), groupEvent("g1", "u1", "/usage chart 7d"), "member:g1:u1", model.QQIdentity{}, "7d", "")
	<-service.chartSemaphore
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("busy chart waited instead of failing fast: %s", elapsed)
	}
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "已有用量图表") {
		t.Fatalf("unexpected busy chart reply: %q", reply)
	}
}

func TestCommandTimeoutAllowsTwoNewAPIRequestsAndReply(t *testing.T) {
	if got, want := commandTimeout(30*time.Second, 10*time.Second), 75*time.Second; got != want {
		t.Fatalf("commandTimeout()=%s, want %s", got, want)
	}
	if got, want := commandTimeout(time.Second, time.Second), 25*time.Second; got != want {
		t.Fatalf("commandTimeout minimum=%s, want %s", got, want)
	}
}

func TestAdminCheckinStatsAndImmediateCreditEdit(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	now := time.Now()
	for _, binding := range []model.Binding{
		{CanonicalID: "user:admin", NewAPIID: 42, Email: "admin@example.com", CreatedAt: now},
		{CanonicalID: "user:checked-1", NewAPIID: 43, Email: "one@example.com", CreatedAt: now},
		{CanonicalID: "user:checked-2", NewAPIID: 44, Email: "two@example.com", CreatedAt: now},
		{CanonicalID: "user:later", NewAPIID: 45, Email: "later@example.com", CreatedAt: now},
		{CanonicalID: "user:fresh", NewAPIID: 46, Email: "fresh@example.com", CreatedAt: now},
	} {
		if err := storage.CreateBinding(binding); err != nil {
			t.Fatal(err)
		}
	}
	period, _ := periodKey(now, service.cfg.CheckinPeriod, service.cfg.CheckinTimezone)
	for _, record := range []model.CheckinRecord{
		{CanonicalID: "user:checked-1", NewAPIID: 43, PeriodKey: period, DisplayCredit: "1", RawQuota: 500000, CreatedAt: now, Status: "completed"},
		{CanonicalID: "user:checked-2", NewAPIID: 44, PeriodKey: period, DisplayCredit: "2", RawQuota: 1000000, CreatedAt: now, Status: "completed"},
		{CanonicalID: "user:later", NewAPIID: 45, PeriodKey: period, DisplayCredit: "1", RawQuota: 500000, CreatedAt: now, Status: "pending"},
	} {
		if _, created, err := storage.ReserveCheckin(record); err != nil || !created {
			t.Fatalf("reserve checkin created=%v err=%v", created, err)
		}
	}

	service.process(context.Background(), c2cEvent("admin", "/admin checkin"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "签到人数：2") || !strings.Contains(reply, "已发放额度：3") || !strings.Contains(reply, "处理中签到：1") {
		t.Fatalf("unexpected checkin stats: %q", reply)
	}
	service.process(context.Background(), c2cEvent("admin", "/admin checkin edit 2.5"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "更新为 2.5") || !strings.Contains(reply, "已立即生效") {
		t.Fatalf("unexpected checkin edit reply: %q", reply)
	}
	if credit, err := storage.GetCheckinCreditOverride(); err != nil || credit != "2.5" {
		t.Fatalf("checkin credit override=%q err=%v", credit, err)
	}
	service.process(context.Background(), c2cEvent("fresh", "/checkin"))
	if api.lastQuota != 1250000 {
		t.Fatalf("updated checkin quota=%d, want 1250000", api.lastQuota)
	}
}

func TestPeriodKey(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	key, next := periodKey(now, "daily", time.UTC)
	if key != "2026-01-01" || !next.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily key=%q next=%v", key, next)
	}
	key, _ = periodKey(now, "weekly", time.UTC)
	if key != "2026-W01" {
		t.Fatalf("weekly key=%q", key)
	}
	key, next = periodKey(now, "monthly", time.UTC)
	if key != "2026-01" || !next.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly key=%q next=%v", key, next)
	}
}
