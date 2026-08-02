package bot

import (
	"bytes"
	"context"
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

type fakeNewAPI struct {
	user          newapi.User
	quotaAdds     int
	quotaSubs     int
	lastQuotaUser int
	lastQuota     int64
}

func (f *fakeNewAPI) GetStatus(context.Context, bool) (newapi.Status, error) {
	return newapi.Status{SystemName: "Test API", QuotaPerUnit: 500000}, nil
}
func (f *fakeNewAPI) GetUser(_ context.Context, id int) (newapi.User, error) {
	u := f.user
	u.ID = id
	return u, nil
}
func (f *fakeNewAPI) FindUserByEmail(context.Context, string) (newapi.User, error) {
	return f.user, nil
}
func (f *fakeNewAPI) AddQuota(_ context.Context, userID int, quota int64) error {
	f.quotaAdds++
	f.lastQuotaUser = userID
	f.lastQuota = quota
	return nil
}
func (f *fakeNewAPI) SubtractQuota(_ context.Context, userID int, quota int64) error {
	f.quotaSubs++
	f.lastQuotaUser = userID
	f.lastQuota = quota
	return nil
}

type fakeQQ struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeQQ) ReplyC2C(_ context.Context, _, _, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, content)
	return nil
}
func (f *fakeQQ) ReplyGroup(_ context.Context, _, _, content string) error {
	return f.ReplyC2C(context.Background(), "", "", content)
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
		QQAdminOpenIDs:      map[string]struct{}{"user:admin": {}},
		CheckinEnabled:      true,
		CheckinCredit:       "1",
		CheckinPeriod:       "daily",
		CheckinTimezone:     time.UTC,
		CheckinCodeTTL:      24 * time.Hour,
		BindCodeTTL:         10 * time.Minute,
		BindCodeMaxAttempts: 5,
		BindEmailLimit:      2,
		BindEmailWindow:     time.Hour,
		LinkCodeTTL:         10 * time.Minute,
		CreditMaxPerCommand: "1000",
		GatewayQueueSize:    64,
		GatewayWorkers:      2,
		MessageDedupTTL:     time.Hour,
	}
	api := &fakeNewAPI{user: newapi.User{ID: 42, Username: "alice", Email: "alice@example.com", Status: 1}}
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

func TestNonCommandMessageIsIgnored(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "你好"))
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if len(qqAPI.messages) != 0 {
		t.Fatalf("non-command message unexpectedly received %d replies", len(qqAPI.messages))
	}
}

func TestUnknownSlashCommandGetsReply(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "/unknown"))
	if reply := lastReply(t, qqAPI); reply == "" {
		t.Fatal("slash command should always receive a reply")
	}
}

func TestBindMissingArgumentShowsExactUsage(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	service.process(context.Background(), groupEvent("g1", "u1", "/bind"))
	if reply := lastReply(t, qqAPI); !strings.Contains(reply, "/bind <邮箱或New API用户ID>") {
		t.Fatalf("unexpected reply: %q", reply)
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
	service.process(context.Background(), c2cEvent("u1", "/bind vertify "+mail.code))
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
	service.process(context.Background(), groupEvent("g1", "u1", "/bind vertify "+mail.code))
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
