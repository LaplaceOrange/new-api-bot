package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/rss"
	"github.com/fsykk/new-api-bot/internal/store"
)

type fakeRSSClient struct {
	mu         sync.Mutex
	result     rss.Result
	err        error
	calls      int
	validators []rss.Validators
	fetch      func(context.Context) (rss.Result, error)
}

func (f *fakeRSSClient) Fetch(ctx context.Context, _ string, validators rss.Validators) (rss.Result, error) {
	f.mu.Lock()
	f.calls++
	f.validators = append(f.validators, validators)
	result, err, fetch := f.result, f.err, f.fetch
	f.mu.Unlock()
	if fetch != nil {
		return fetch(ctx)
	}
	return result, err
}
func (f *fakeRSSClient) Close() {}

func rssBotResult(ids ...string) rss.Result {
	result := rss.Result{Feed: rss.Feed{Title: "测试来源"}, Validators: rss.Validators{ETag: `"one"`}}
	for _, id := range ids {
		result.Articles = append(result.Articles, rss.Article{ID: id, Title: "标题 " + id, Summary: "摘要 " + id, URL: "https://example.com/posts/" + id})
	}
	return result
}

func setupRSSService(t *testing.T) (*Service, *store.Store, *fakeQQ, *fakeRSSClient) {
	t.Helper()
	s, storage, _, qqAPI, _ := testService(t)
	s.cfg.RSSEnabled = true
	s.cfg.RSSPollInterval = 5 * time.Minute
	s.cfg.RSSHTTPTimeout = time.Second
	s.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
	s.cfg.QQReadOnlyAdminOpenIDs["member:g:readonly"] = struct{}{}
	s.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	s.cfg.QQAdminOpenIDs["member:other:admin"] = struct{}{}
	s.rssClient.Close()
	client := &fakeRSSClient{result: rssBotResult("old")}
	s.rssClient = client
	return s, storage, qqAPI, client
}

func rssProcess(s *Service, group, actor, command string) {
	s.process(context.Background(), groupEvent(group, actor, command))
}

func TestRSSCommandPermissionsGroupIsolationAndHelp(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	for _, actor := range []string{"ordinary", "readonly"} {
		rssProcess(s, "g", actor, "/rss add https://example.com/rss")
		if subscriptions, _ := storage.ListRSSSubscriptions(""); len(subscriptions) != 0 || client.calls != 0 {
			t.Fatal("unauthorized add executed", actor)
		}
	}
	s.process(context.Background(), c2cEvent("admin", "/rss add https://example.com/rss"))
	if client.calls != 0 || !strings.Contains(lastReply(t, qqAPI), "群聊") {
		t.Fatal("private add permitted")
	}
	rssProcess(s, "g", "admin", "/rss add https://example.com/rss?token=secret")
	if !strings.Contains(lastReply(t, qqAPI), "编号：1") || strings.Contains(lastReply(t, qqAPI), "secret") {
		t.Fatal(lastReply(t, qqAPI))
	}
	rssProcess(s, "g", "admin", "/rss add https://example.com/rss?token=secret")
	if client.calls != 1 {
		t.Fatal("duplicate add fetched again", client.calls)
	}
	rssProcess(s, "other", "admin", "/rss remove 1")
	if _, err := storage.RSSSubscription("g", "1"); err != nil {
		t.Fatal("cross-group removal", err)
	}
	rssProcess(s, "other", "admin", "/rss list")
	if strings.Contains(lastReply(t, qqAPI), "测试来源") {
		t.Fatal("cross-group listing")
	}
	rssProcess(s, "g", "ordinary", "/rss status")
	if !strings.Contains(lastReply(t, qqAPI), "待发送：0") {
		t.Fatal(lastReply(t, qqAPI))
	}
	rssProcess(s, "g", "ordinary", "/rss check 1")
	if client.calls != 1 {
		t.Fatal("ordinary user ran check")
	}
	rssProcess(s, "g", "readonly", "/rss check 1")
	if client.calls != 2 || !strings.Contains(lastReply(t, qqAPI), "未改变推送进度") {
		t.Fatal(lastReply(t, qqAPI))
	}
	for _, actor := range []string{"ordinary", "readonly", "admin"} {
		rssProcess(s, "g", actor, "/rss help")
		reply := lastReply(t, qqAPI)
		if !strings.Contains(reply, "/rss list") || strings.Contains(reply, "/rss add") != (actor == "admin") ||
			strings.Contains(reply, "/rss check") != (actor != "ordinary") {
			t.Fatal(actor, reply)
		}
	}
	identity := model.QQIdentity{UserOpenID: "admin"}
	if err := storage.PutCommandRule(model.CommandRule{Keyword: "rss", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	s.commandRules.Store(nil)
	if strings.Contains(s.helpTextFor(identity, ""), "/rss") {
		t.Fatal("disabled rss still in help")
	}
	calls := client.calls
	rssProcess(s, "g", "admin", "/rss check 1")
	if client.calls != calls {
		t.Fatal("disabled rss executed")
	}
}

func TestRSSManagementChecksDoNotAdvanceBaseline(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	rssProcess(s, "g", "admin", "/rss add https://example.com/rss")
	before, _ := storage.RSSSubscription("g", "1")
	client.result = rssBotResult("old", "new")
	rssProcess(s, "g", "admin", "/rss check 1")
	after, _ := storage.RSSSubscription("g", "1")
	if !after.LastChecked.Equal(before.LastChecked) || after.Version != before.Version {
		t.Fatal("check changed baseline")
	}
	for _, value := range []string{"0s", "30s", "25h", "bad"} {
		rssProcess(s, "g", "admin", "/rss interval "+value)
		if !strings.Contains(lastReply(t, qqAPI), "1m 至 24h") {
			t.Fatal(value, lastReply(t, qqAPI))
		}
	}
	rssProcess(s, "g", "admin", "/rss interval 1h")
	settings, _ := storage.RSSGroupSettings("g")
	if settings.Interval != time.Hour {
		t.Fatal(settings)
	}
	rssProcess(s, "g", "admin", "/rss pause 1")
	client.err = errors.New("RSS 网络请求失败")
	rssProcess(s, "g", "admin", "/rss resume 1")
	after, _ = storage.RSSSubscription("g", "1")
	if after.Enabled {
		t.Fatal("failed resume enabled subscription")
	}
	client.err = nil
	rssProcess(s, "g", "admin", "/rss resume 1")
	after, _ = storage.RSSSubscription("g", "1")
	if !after.Enabled || after.Version <= before.Version {
		t.Fatal(after)
	}
	rssProcess(s, "g", "admin", "/rss remove 1")
	if _, err := storage.RSSSubscription("g", "1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	// Removed IDs are never reused.
	rssProcess(s, "g", "admin", "/rss add https://example.com/rss")
	if !strings.Contains(lastReply(t, qqAPI), "编号：2") {
		t.Fatal(lastReply(t, qqAPI))
	}
}

func TestRSSCycleSharedFetchIndependentProgressAndRetry(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	a, _, _ := storage.AddRSSSubscription("a", "https://example.com/rss", rssBotResult("old"), s.now())
	_, _, _ = storage.AddRSSSubscription("b", a.URL, rssBotResult("old", "new"), s.now())
	client.result = rssBotResult("old", "new")
	qqAPI.groupReplyErr = errors.New("simulated QQ failure")
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatal("same URL fetched more than once per cycle", client.calls)
	}
	if _, count, _ := storage.RSSPending("a", 10); count != 1 {
		t.Fatal("failed send lost pending", count)
	}
	if _, count, _ := storage.RSSPending("b", 10); count != 0 {
		t.Fatal("independent baseline not respected", count)
	}
	qqAPI.groupReplyErr = nil
	// Polling and queued retries both honor the group's interval.
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || len(qqAPI.messages) != 0 {
		t.Fatal("poll interval ignored")
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := storage.MarkRSSSend("a", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, count, _ := storage.RSSPending("a", 10); count != 0 || len(qqAPI.messages) != 1 {
		t.Fatal(count, qqAPI.messages)
	}
	message := qqAPI.messages[0]
	for _, value := range []string{"测试来源", "标题 new", "摘要 new", "https://example.com/posts/new"} {
		if !strings.Contains(message, value) {
			t.Fatal("missing article field", message)
		}
	}
}

func TestRSSCycleDifferentValidatorsFetchUnconditionallyAndFailedGroupDoesNotBlock(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	a, _, _ := storage.AddRSSSubscription("a", "https://example.com/rss", rssBotResult(), s.now())
	result := rssBotResult()
	result.ETag = `"other"`
	_, _, _ = storage.AddRSSSubscription("b", a.URL, result, s.now())
	client.result = rssBotResult("new")
	qqAPI.groupReplyErr = errors.New("first group fails")
	qqAPI.groupReplyErrAt = 1
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || client.validators[0] != (rss.Validators{}) || len(qqAPI.messages) != 1 {
		t.Fatal(client.calls, client.validators, qqAPI.messages)
	}
	_, countA, _ := storage.RSSPending("a", 10)
	_, countB, _ := storage.RSSPending("b", 10)
	if countA != 1 || countB != 0 {
		t.Fatal(countA, countB)
	}
}

func TestRSSGlobalDisabledAndGatewayDisconnected(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	_, _, _ = storage.AddRSSSubscription("g", "https://example.com/rss", rssBotResult(), s.now())
	client.result = rssBotResult("new")
	s.cfg.RSSEnabled = false
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	rssProcess(s, "g", "ordinary", "/rss status")
	if !strings.Contains(lastReply(t, qqAPI), "全局关闭") || client.calls != 0 {
		t.Fatal(qqAPI.messages, client.calls)
	}
	s.cfg.RSSEnabled = true
	s.SetGatewayConnectedFunc(func() bool { return false })
	if err := s.runRSSCycle(context.Background()); err != nil || client.calls != 0 {
		t.Fatal(err, client.calls)
	}
}

func TestRSSSendRateLimitAndCancellation(t *testing.T) {
	s, storage, qqAPI, _ := setupRSSService(t)
	sub, _, _ := storage.AddRSSSubscription("g", "https://example.com/rss", rssBotResult(), s.now())
	if err := storage.RecordRSSPoll(sub, rssBotResult("one", "two"), nil, s.now()); err != nil {
		t.Fatal(err)
	}
	var timestamps []time.Time
	qqAPI.groupReplyHook = func(_ int) { timestamps = append(timestamps, time.Now()) }
	if err := s.deliverRSSGroup(context.Background(), "g"); err != nil {
		t.Fatal(err)
	}
	if len(timestamps) != 2 || timestamps[1].Sub(timestamps[0]) < 1990*time.Millisecond {
		t.Fatal("messages not individually rate-limited", timestamps)
	}
	if err := storage.RecordRSSPoll(sub, rssBotResult("one", "two", "three"), nil, s.now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.deliverRSSGroup(ctx, "g"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("rate-limit wait ignored shutdown", err)
	}
	if _, count, _ := storage.RSSPending("g", 10); count != 1 {
		t.Fatal("cancellation dropped pending", count)
	}
}

func TestRSSSendBatchLimit(t *testing.T) {
	s, storage, qqAPI, _ := setupRSSService(t)
	sub, _, _ := storage.AddRSSSubscription("g", "https://example.com/rss", rssBotResult(), s.now())
	ids := []string{}
	for i := 0; i < 12; i++ {
		ids = append(ids, fmt.Sprint(i))
	}
	if err := storage.RecordRSSPoll(sub, rssBotResult(ids...), nil, s.now()); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverRSSGroup(context.Background(), "g"); err != nil {
		t.Fatal(err)
	}
	if _, count, _ := storage.RSSPending("g", 10); count != 2 || len(qqAPI.messages) != 10 {
		t.Fatal(count, qqAPI.messages)
	}
}

func TestRSSWorkerStopsInFlightRequest(t *testing.T) {
	s, storage, _, client := setupRSSService(t)
	_, _, _ = storage.AddRSSSubscription("g", "https://example.com/rss", rssBotResult(), s.now())
	started := make(chan struct{})
	client.fetch = func(ctx context.Context) (rss.Result, error) {
		close(started)
		<-ctx.Done()
		return rss.Result{}, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		s.runRSSWorker(context.Background())
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	close(s.notifyStop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker failed to stop in-flight request")
	}
}

func TestRSSPauseDuringInFlightPollRejectsStaleResult(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	sub, _, _ := storage.AddRSSSubscription("g", "https://example.com/rss", rssBotResult("old"), s.now())
	started, release := make(chan struct{}), make(chan struct{})
	client.fetch = func(ctx context.Context) (rss.Result, error) {
		close(started)
		select {
		case <-release:
			return rssBotResult("old", "new"), nil
		case <-ctx.Done():
			return rss.Result{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.runRSSCycle(ctx) }()
	<-started
	rssProcess(s, "g", "admin", "/rss pause "+sub.ID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	pending, count, _ := storage.RSSPending("g", 10)
	if count != 0 || len(pending) != 0 {
		t.Fatal("paused subscription accepted in-flight articles", pending)
	}
	for _, message := range qqAPI.messages {
		if strings.HasPrefix(message, "【RSS") {
			t.Fatal("paused subscription received an article", message)
		}
	}
}

func TestRSSFetchFailureDoesNotAdvanceBaseline(t *testing.T) {
	s, storage, qqAPI, client := setupRSSService(t)
	sub, _, _ := storage.AddRSSSubscription("g", "https://example.com/rss", rssBotResult("old"), s.now())
	client.err = errors.New("RSS 网络请求失败")
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := storage.RSSSubscription("g", sub.ID)
	if current.LastError == "" || current.Validators != sub.Validators || len(qqAPI.messages) != 0 {
		t.Fatal(current, qqAPI.messages)
	}
	client.err = nil
	client.result = rssBotResult("old", "new")
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := s.runRSSCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(qqAPI.messages) != 1 || !strings.Contains(qqAPI.messages[0], "标题 new") {
		t.Fatal("recovery skipped new article", qqAPI.messages)
	}
}
