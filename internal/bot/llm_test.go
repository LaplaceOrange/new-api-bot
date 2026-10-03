package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/llm"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/rss"
	"github.com/fsykk/new-api-bot/internal/store"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

type scriptedLLM struct {
	mu        sync.Mutex
	responses []llm.Completion
	inputs    [][]llm.Message
	hook      func(context.Context)
	err       error
}

func (f *scriptedLLM) Complete(ctx context.Context, _ llm.Config, messages []llm.Message, _ []llm.Tool) (llm.Completion, error) {
	f.mu.Lock()
	n := len(f.inputs)
	f.inputs = append(f.inputs, append([]llm.Message(nil), messages...))
	hook := f.hook
	err := f.err
	result := llm.Completion{Message: llm.Message{Role: "assistant", Content: "完成"}}
	if n < len(f.responses) {
		result = f.responses[n]
	}
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if ctx.Err() != nil {
		return llm.Completion{}, ctx.Err()
	}
	return result, err
}

type fakeLLMSearch struct {
	mu      sync.Mutex
	queries []string
	err     error
}

func (f *fakeLLMSearch) Search(_ context.Context, _ llm.Config, query string) (llm.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	return llm.SearchResult{Sources: []llm.Source{{Title: "真实来源", URL: "https://example.test/source"}}}, f.err
}
func toolCompletion(name, args string) llm.Completion {
	call := llm.ToolCall{ID: "call-" + name, Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = args
	return llm.Completion{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}}}
}
func setupLLM(t *testing.T) (*Service, *scriptedLLM, *fakeQQ) {
	t.Helper()
	s, _, _, q, _ := testService(t)
	cfg := llm.DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = "https://model.example.test/v1"
	cfg.APIKey = "model-key-secret"
	cfg.Model = "test"
	s.cfg.LLM = cfg
	s.cfg.QQAppID = "our-bot"
	modelClient := &scriptedLLM{}
	s.llmCompleter = modelClient
	s.llmSearcher = &fakeLLMSearch{}
	return s, modelClient, q
}
func bindLLM(t *testing.T, s *Service, event qq.MessageEvent, userID int) {
	t.Helper()
	canonical := identityFromEvent(event).Canonical()
	if canonical == "" {
		canonical = "user:" + event.Message.Author.MemberOpenID
	}
	if event.Message.GroupOpenID != "" {
		if err := s.store.PutAlias(identityFromEvent(event).GroupAlias(), canonical); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.CreateBinding(model.Binding{CanonicalID: canonical, NewAPIID: userID, Email: "private@example.test"}); err != nil {
		t.Fatal(err)
	}
}
func pendingLLM(t *testing.T, s *Service) store.LLMJob {
	t.Helper()
	jobs, err := s.store.PendingLLMJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	return jobs[0]
}
func processLLMNow(t *testing.T, s *Service, event qq.MessageEvent) store.LLMJob {
	t.Helper()
	s.process(context.Background(), event)
	job := pendingLLM(t, s)
	s.runLLMJob(context.Background(), job)
	updated, err := s.store.LLMJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
func decodeLLMPayload(t *testing.T, s *Service, j store.LLMJob) llmJobPayload {
	t.Helper()
	plain, err := s.secure.Decrypt(j.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var p llmJobPayload
	if err = json.Unmarshal([]byte(plain), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestLLMEntryPreservesCodeAndHelpSuffix(t *testing.T) {
	s, m, q := setupLLM(t)
	event := c2cEvent("u", "/chat <T>\n```go\nx < 1\n```\nplease help")
	bindLLM(t, s, event, 42)
	j := processLLMNow(t, s, event)
	if j.Status != "sent" {
		t.Fatal(j.Status)
	}
	if len(m.inputs) != 1 || !strings.Contains(m.inputs[0][len(m.inputs[0])-1].Content, "<T>\n```go\nx < 1\n```\nplease help") {
		t.Fatal(m.inputs)
	}
	if lastReply(t, q) != "完成" {
		t.Fatal(lastReply(t, q))
	}
	p := decodeLLMPayload(t, s, j)
	if p.Config.SystemPrompt != "" {
		t.Fatal("a persona was hard-coded")
	}
	if strings.Contains(j.Payload, "model-key-secret") || strings.Contains(j.Payload, "please help") || strings.Contains(j.Result, "完成") {
		t.Fatal("plaintext job data")
	}
}
func TestLLMAdmissionRequiresBindingAndExplicitGroupEnable(t *testing.T) {
	s, _, q := setupLLM(t)
	e := groupEvent("g", "u", "/chat question")
	s.process(context.Background(), e)
	if !strings.Contains(lastReply(t, q), "绑定") {
		t.Fatal(lastReply(t, q))
	}
	bindLLM(t, s, e, 42)
	s.process(context.Background(), e)
	if !strings.Contains(lastReply(t, q), "未开启") {
		t.Fatal(lastReply(t, q))
	}
	s.process(context.Background(), c2cEvent("admin", "/chat on"))
	if !strings.Contains(lastReply(t, q), "群聊") {
		t.Fatal(lastReply(t, q))
	}
	admin := groupEvent("g", "admin", "/chat on")
	admin.Message.Author.UserOpenID = "admin"
	s.process(context.Background(), admin)
	enabled, err := s.store.LLMGroupEnabled("g")
	if err != nil || !enabled {
		t.Fatal(enabled, err)
	}
	if _, err = s.store.GetBinding("user:admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("enabling unexpectedly required admin binding")
	}
	s.process(context.Background(), e)
	j := pendingLLM(t, s)
	if j.Group != "g" {
		t.Fatal(j)
	}
}
func TestLLMGatewayAtAndIgnoredMessages(t *testing.T) {
	s, _, _ := setupLLM(t)
	for _, e := range []qq.MessageEvent{c2cEvent("u", "ordinary"), groupEvent("g", "u", "ordinary")} {
		if !s.HandleGateway(context.Background(), e) || len(s.queue) != 0 {
			t.Fatal("ordinary text accepted")
		}
	}
	e := groupEvent("g", "u", "<@!our-bot> code <T>\nhelp")
	if !s.HandleGateway(context.Background(), e) || len(s.queue) != 1 {
		t.Fatal("at event ignored")
	}
	item := <-s.queue
	if item.event.Message.Content != "/chat code <T>\nhelp" {
		t.Fatal(item.event.Message.Content)
	}
}
func TestLLMConfigPersonaHotUpdateSecretsAndReadonly(t *testing.T) {
	s, _, q := setupLLM(t)
	s.process(context.Background(), c2cEvent("admin", "/llm_config set system_prompt \"可配置角色\n保持 <原文>\""))
	cfg, err := s.llmConfigSnapshot()
	if err != nil || cfg.SystemPrompt != "可配置角色\n保持 <原文>" {
		t.Fatal(cfg.SystemPrompt, err)
	}
	s.process(context.Background(), c2cEvent("admin", "/llm_config set system_prompt \"\""))
	cfg, _ = s.llmConfigSnapshot()
	if cfg.SystemPrompt != "" {
		t.Fatal(cfg.SystemPrompt)
	}
	adminGroup := groupEvent("g", "admin", "/llm_config set api_key exposed")
	adminGroup.Message.Author.UserOpenID = "admin"
	s.process(context.Background(), adminGroup)
	if !strings.Contains(lastReply(t, q), "单聊") {
		t.Fatal(lastReply(t, q))
	}
	s.process(context.Background(), c2cEvent("admin", "/llm_config set api_key super-secret"))
	encrypted, err := s.store.LLMConfig()
	if err != nil || strings.Contains(encrypted, "super-secret") {
		t.Fatal(encrypted, err)
	}
	s.process(context.Background(), c2cEvent("admin", "/llm_config show api_key"))
	if strings.Contains(lastReply(t, q), "super-secret") || !strings.Contains(lastReply(t, q), "不回显") {
		t.Fatal(lastReply(t, q))
	}
	s.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
	s.process(context.Background(), c2cEvent("readonly", "/llm_config set model unauthorized"))
	if !strings.Contains(lastReply(t, q), "只读") {
		t.Fatal(lastReply(t, q))
	}
	s.process(context.Background(), c2cEvent("readonly", "/llm_config show model"))
	if !strings.Contains(lastReply(t, q), `"test"`) {
		t.Fatal(lastReply(t, q))
	}
	s.process(context.Background(), c2cEvent("admin", "/llm_config reset api_key"))
	cfg, _ = s.llmConfigSnapshot()
	if cfg.APIKey != "model-key-secret" {
		t.Fatal("reset did not restore environment default")
	}
}
func TestLLMGroupHistorySharedAndBusinessExcluded(t *testing.T) {
	s, m, q := setupLLM(t)
	a := groupEvent("g", "a", "/chat ordinary first")
	b := groupEvent("g", "b", "/chat ordinary second")
	bindLLM(t, s, a, 42)
	bindLLM(t, s, b, 43)
	_ = s.store.SetLLMGroup("g", true, s.now())
	processLLMNow(t, s, a)
	processLLMNow(t, s, b)
	if len(m.inputs[1]) != 4 || !strings.Contains(m.inputs[1][1].Content, "ordinary first") {
		t.Fatal(m.inputs)
	}
	m.responses = []llm.Completion{{}, {}, toolCompletion("account", "{}"), {Message: llm.Message{Content: "PRIVATE BALANCE"}}, {Message: llm.Message{Content: "normal"}}}
	a.Message.ID = "business"
	a.Message.Content = "/chat my balance"
	processLLMNow(t, s, a)
	b.Message.ID = "after"
	b.Message.Content = "/chat previous question"
	processLLMNow(t, s, b)
	for _, msg := range m.inputs[4] {
		if strings.Contains(msg.Content, "PRIVATE BALANCE") || strings.Contains(msg.Content, "my balance") {
			t.Fatal("business turn leaked to shared history")
		}
	}
	if lastReply(t, q) != "normal" {
		t.Fatal(lastReply(t, q))
	}
}
func TestLLMPrivateHistoryIsolatedAndBindingScoped(t *testing.T) {
	s, m, _ := setupLLM(t)
	a := c2cEvent("a", "/chat question")
	b := c2cEvent("b", "/chat question")
	bindLLM(t, s, a, 42)
	bindLLM(t, s, b, 43)
	m.responses = []llm.Completion{toolCompletion("account", "{}"), {Message: llm.Message{Content: "PRIVATE42"}}, {Message: llm.Message{Content: "B"}}}
	processLLMNow(t, s, a)
	processLLMNow(t, s, b)
	for _, msg := range m.inputs[2] {
		if strings.Contains(msg.Content, "PRIVATE42") {
			t.Fatal("private sessions not isolated")
		}
	}
	if _, err := s.store.UnbindByNewAPIID(42); err != nil {
		t.Fatal(err)
	}
	bindLLM(t, s, a, 44)
	a.Message.ID = "rebound"
	a.Message.Content = "/chat another"
	processLLMNow(t, s, a)
	for _, msg := range m.inputs[3] {
		if strings.Contains(msg.Content, "PRIVATE42") {
			t.Fatal("business history survived binding change")
		}
	}
}
func TestLLMToolAuthorizationValidationAndNoWrites(t *testing.T) {
	s, _, _ := setupLLM(t)
	e := c2cEvent("u", "/chat test")
	bindLLM(t, s, e, 42)
	s.process(context.Background(), e)
	j := pendingLLM(t, s)
	p := decodeLLMPayload(t, s, j)
	for _, tc := range []struct{ name, args string }{
		{"account", `{"user_id":99}`}, {"bindings", "{}"}, {"admin_report", "{}"}, {"unknown", "{}"},
		{"account", `{"identity":"user:admin"}`}, {"usage", `{"range":"100d"}`}, {"logs", `{"count":99}`},
		{"account", `{"user_id":"42"}`}, {"account", `null`}, {"account", `{} trailing`},
	} {
		if _, err := s.executeLLMBusinessTool(context.Background(), j, p, tc.name, tc.args); err == nil {
			t.Fatalf("unauthorized tool accepted: %+v", tc)
		}
	}
	value, err := s.executeLLMBusinessTool(context.Background(), j, p, "account", "{}")
	data, _ := json.Marshal(value)
	if err != nil || strings.Contains(string(data), "private@example") {
		t.Fatal(string(data), err)
	}
	_ = s.store.PutCommandRule(model.CommandRule{Keyword: "me", Enabled: false})
	s.commandRules.Store(nil)
	if _, err = s.executeLLMBusinessTool(context.Background(), j, p, "account", "{}"); err == nil {
		t.Fatal("tool bypassed command disable")
	}
	_ = s.store.PutCommandRule(model.CommandRule{Keyword: "me", Enabled: true})
	s.commandRules.Store(nil)
	if _, err = s.store.UnbindByNewAPIID(42); err != nil {
		t.Fatal(err)
	}
	if _, err = s.executeLLMBusinessTool(context.Background(), j, p, "account", "{}"); err == nil {
		t.Fatal("tool did not recheck binding")
	}
	api := s.newAPI.(*fakeNewAPI)
	if api.quotaAdds != 0 || api.quotaSubs != 0 || api.managedAction != "" || api.reset2FA != 0 {
		t.Fatal("LLM tool executed a write")
	}
}
func TestLLMAllBusinessToolsAndRedaction(t *testing.T) {
	s, _, _ := setupLLM(t)
	e := groupEvent("g", "admin", "/chat test")
	e.Message.Author.UserOpenID = "admin"
	bindLLM(t, s, e, 42)
	_ = s.store.SetLLMGroup("g", true, s.now())
	s.cfg.RSSEnabled = true
	s.process(context.Background(), e)
	j := pendingLLM(t, s)
	p := decodeLLMPayload(t, s, j)
	api := s.newAPI.(*fakeNewAPI)
	api.logs = []newapi.LogRecord{{UserID: 42, Username: "alice", Content: "secret request body", ModelName: "m"}, {UserID: 99, Username: "intruder", Content: "intruder"}}
	api.usageByModel = []newapi.UsageRecord{{UserID: 42, Username: "alice", ModelName: "m", Count: 2, Quota: 10}}
	if _, _, err := s.store.AddRSSSubscription("g", "https://user:feed-secret@example.test/rss?token=private-feed-token", rss.Result{Feed: rss.Feed{Title: "Private feed"}}, s.now()); err != nil {
		t.Fatal(err)
	}
	for _, spec := range llmToolSpecs {
		value, err := s.executeLLMBusinessTool(context.Background(), j, p, spec.name, "{}")
		if err != nil {
			t.Fatalf("%s: %v", spec.name, err)
		}
		b, _ := json.Marshal(value)
		for _, secret := range []string{"private@example.test", "secret request body", "intruder", "model-key-secret", "user:admin", "feed-secret", "private-feed-token"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("%s returned %s: %s", spec.name, secret, b)
			}
		}
	}
	if api.quotaAdds != 0 || api.quotaSubs != 0 {
		t.Fatal("queries changed quota")
	}
}
func TestLLMSearchCitationsErrorsAndPrivacy(t *testing.T) {
	for _, business := range []bool{false, true} {
		t.Run(fmt.Sprint(business), func(t *testing.T) {
			s, m, q := setupLLM(t)
			s.cfg.LLM.SearchBackend = "bing_serpapi"
			search := &fakeLLMSearch{}
			s.llmSearcher = search
			e := c2cEvent("u", "/chat lookup")
			bindLLM(t, s, e, 42)
			if business {
				m.responses = append(m.responses, toolCompletion("account", "{}"))
			}
			m.responses = append(m.responses, toolCompletion("web_search", `{"query":"standalone question"}`), llm.Completion{Message: llm.Message{Content: "answer"}})
			processLLMNow(t, s, e)
			if business {
				if len(search.queries) != 0 || !strings.Contains(lastReply(t, q), "禁止") {
					t.Fatal("private data search was not blocked", search.queries, lastReply(t, q))
				}
			} else {
				if len(search.queries) != 1 || !strings.Contains(lastReply(t, q), "https://example.test/source") {
					t.Fatal(search.queries, lastReply(t, q))
				}
			}
		})
	}
	s, m, q := setupLLM(t)
	s.cfg.LLM.SearchBackend = "tavily"
	s.llmSearcher = &fakeLLMSearch{err: errors.New("search unavailable")}
	e := c2cEvent("u", "/chat lookup")
	bindLLM(t, s, e, 42)
	m.responses = []llm.Completion{toolCompletion("web_search", `{"query":"query"}`), {Message: llm.Message{Content: "answer"}}}
	processLLMNow(t, s, e)
	if !strings.Contains(lastReply(t, q), "search unavailable") || strings.Contains(lastReply(t, q), "来源：") {
		t.Fatal(lastReply(t, q))
	}
}
func TestLLMToolBudgetResetAndDisableDoNotRegenerate(t *testing.T) {
	s, m, q := setupLLM(t)
	s.cfg.LLM.MaxTools = 1
	e := c2cEvent("u", "/chat test")
	bindLLM(t, s, e, 42)
	m.responses = []llm.Completion{toolCompletion("account", "{}"), toolCompletion("account", "{}")}
	j := processLLMNow(t, s, e)
	if !strings.Contains(lastReply(t, q), "上限") || len(m.inputs) != 2 {
		t.Fatal(lastReply(t, q))
	}
	s.process(context.Background(), e)
	if len(m.inputs) != 2 {
		t.Fatal("duplicate regenerated result")
	}
	e.Message.ID = "reset"
	s.process(context.Background(), e)
	queued := pendingLLM(t, s)
	_ = s.store.ResetLLMSession(queued.Session, s.now())
	s.runLLMJob(context.Background(), queued)
	now, _ := s.store.LLMJob(queued.ID)
	if now.Status != "canceled" || len(m.inputs) != 2 {
		t.Fatal(now)
	}
	_ = s.store.PutCommandRule(model.CommandRule{Keyword: "chat", Enabled: false})
	s.commandRules.Store(nil)
	e.Message.ID = "disabled"
	s.process(context.Background(), e)
	jobs, _ := s.store.PendingLLMJobs()
	if len(jobs) != 0 {
		t.Fatal("disabled chat admitted")
	}
	_ = j
}

type sequenceLLMQQ struct {
	mu     sync.Mutex
	ids    []string
	seqs   []int
	texts  []string
	failAt int
}

func (q *sequenceLLMQQ) ReplyC2C(context.Context, string, string, string) error {
	return errors.New("unexpected fallback")
}
func (q *sequenceLLMQQ) ReplyGroup(context.Context, string, string, string) error {
	return errors.New("unexpected fallback")
}
func (q *sequenceLLMQQ) send(id, text string, seq int) (qq.SentMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = append(q.ids, id)
	q.seqs = append(q.seqs, seq)
	q.texts = append(q.texts, text)
	if seq == q.failAt {
		return qq.SentMessage{}, errors.New("send failed")
	}
	return qq.SentMessage{ID: fmt.Sprint(seq)}, nil
}
func (q *sequenceLLMQQ) SendGroupTextWithSequence(_ context.Context, _, id, text string, seq int) (qq.SentMessage, error) {
	return q.send(id, text, seq)
}
func (q *sequenceLLMQQ) SendC2CTextWithSequence(_ context.Context, _, id, text string, seq int) (qq.SentMessage, error) {
	return q.send(id, text, seq)
}
func TestLLMReplySequencesPartialFailureAndCap(t *testing.T) {
	for _, group := range []bool{true, false} {
		s, _, _ := setupLLM(t)
		q := &sequenceLLMQQ{}
		s.qq = q
		e := c2cEvent("u", "/chat x")
		if group {
			e = groupEvent("g", "u", "/chat x")
		}
		if err := s.sendLLMResult(context.Background(), e, strings.Repeat("字", 9000), s.cfg.LLM); err != nil {
			t.Fatal(err)
		}
		if len(q.ids) != 4 || len([]rune(strings.Join(q.texts, ""))) > 6000 {
			t.Fatal(len(q.ids))
		}
		for i, id := range q.ids {
			if id != e.Message.ID || q.seqs[i] != i+1 {
				t.Fatal(q.ids, q.seqs)
			}
		}
		q = &sequenceLLMQQ{failAt: 2}
		s.qq = q
		if err := s.sendLLMResult(context.Background(), e, strings.Repeat("字", 5000), s.cfg.LLM); err == nil || len(q.ids) != 2 {
			t.Fatal(err, q.ids)
		}
	}
}
func TestLLMDispatcherDoesNotBlockGatewayAndSerializesSessions(t *testing.T) {
	s, m, q := setupLLM(t)
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	m.hook = func(ctx context.Context) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	e := c2cEvent("u", "/chat slow")
	bindLLM(t, s, e, 42)
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	defer func() {
		cancel()
		stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := s.StopContext(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	if !s.HandleGateway(ctx, e) {
		t.Fatal("gateway rejected")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("LLM did not start")
	}
	second := e
	second.Message.ID = "second"
	second.Message.Content = "/chat next"
	s.HandleGateway(ctx, second)
	s.HandleGateway(ctx, c2cEvent("any", "/whoami"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		texts := strings.Join(q.messages, "\n")
		q.mu.Unlock()
		if strings.Contains(texts, "OpenID") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	q.mu.Lock()
	texts := strings.Join(q.messages, "\n")
	q.mu.Unlock()
	if !strings.Contains(texts, "OpenID") {
		t.Fatal("slow model blocked gateway workers")
	}
	select {
	case <-entered:
		t.Fatal("same session ran concurrently")
	default:
	}
	close(release)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("second session turn not scheduled")
	}
}
func TestLLMVendorSnapshotMarksUnknownAndStale(t *testing.T) {
	s, _, _ := setupLLM(t)
	value, err := s.llmVendorSnapshot()
	b, _ := json.Marshal(value)
	if err != nil || !strings.Contains(string(b), `"available":false`) {
		t.Fatal(string(b), err)
	}
	raw := json.RawMessage(`{"version":1,"sources":{"openai":{"issues":{"issue":{"title":"incident","severity":"warning","status_url":"https://user:status-secret@example.test/?token=status-private"}}}},"health":{"openai":{"last_success_at":"2000-01-01T00:00:00Z","consecutive_failures":0}}}`)
	if err = s.store.PutVendorStatusValue(vendorstatus.StateKey, raw); err != nil {
		t.Fatal(err)
	}
	value, err = s.llmVendorSnapshot()
	b, _ = json.Marshal(value)
	if err != nil || !strings.Contains(string(b), `"stale":true`) || strings.Contains(string(b), "status-secret") || strings.Contains(string(b), "status-private") {
		t.Fatal(string(b), err)
	}
}

func TestLLMHelpConfigRecoveryAndPersistence(t *testing.T) {
	s, _, q := setupLLM(t)
	for _, cmd := range []string{"/chat help", "/chat status help", "/llm_config help", "/llm_config set help"} {
		user := "admin"
		if strings.HasPrefix(cmd, "/chat") {
			user = "unbound"
		}
		s.process(context.Background(), c2cEvent(user, cmd))
		if !strings.Contains(lastReply(t, q), "帮助") && !strings.Contains(lastReply(t, q), "用法") {
			t.Fatalf("%s: %s", cmd, lastReply(t, q))
		}
		jobs, _ := s.store.PendingLLMJobs()
		if len(jobs) != 0 {
			t.Fatal("help admitted a paid task")
		}
	}
	s.process(context.Background(), c2cEvent("admin", `/llm_config set system_prompt "可持久化人格"`))
	s2 := New(s.cfg, s.store, s.secure, s.newAPI, q, s.mailer, s.logger)
	defer s2.llmClient.Close()
	cfg, err := s2.llmConfigSnapshot()
	if err != nil || cfg.SystemPrompt != "可持久化人格" {
		t.Fatal(cfg, err)
	}
	if err = s.store.PutLLMConfig("broken ciphertext"); err != nil {
		t.Fatal(err)
	}
	s.process(context.Background(), c2cEvent("admin", "/llm_config reset all"))
	cfg, err = s.llmConfigSnapshot()
	if err != nil || cfg != s.cfg.LLM {
		t.Fatal(cfg, err)
	}
}

func TestLLMReadyResultResumesWithoutModelCall(t *testing.T) {
	s, m, q := setupLLM(t)
	e := c2cEvent("u", "/chat test")
	bindLLM(t, s, e, 42)
	s.process(context.Background(), e)
	j := pendingLLM(t, s)
	data, _ := json.Marshal(llmSavedResult{Text: "cached answer"})
	encrypted, _ := s.secure.Encrypt(string(data))
	if _, err := s.store.TransitionLLMJob(j.ID, "queued", "ready", encrypted, s.now()); err != nil {
		t.Fatal(err)
	}
	if err := s.store.RecoverLLMJobs(s.now()); err != nil {
		t.Fatal(err)
	}
	j = pendingLLM(t, s)
	s.runLLMJob(context.Background(), j)
	if len(m.inputs) != 0 || lastReply(t, q) != "cached answer" {
		t.Fatal(m.inputs, lastReply(t, q))
	}
}

func TestLLMReadyAdminResultRechecksPrivilege(t *testing.T) {
	s, m, q := setupLLM(t)
	e := c2cEvent("admin", "/chat report")
	bindLLM(t, s, e, 42)
	s.process(context.Background(), e)
	j := pendingLLM(t, s)
	data, _ := json.Marshal(llmSavedResult{Text: "ADMIN PRIVATE REPORT", RequiresAdmin: true})
	encrypted, _ := s.secure.Encrypt(string(data))
	_, _ = s.store.TransitionLLMJob(j.ID, "queued", "ready", encrypted, s.now())
	delete(s.cfg.QQAdminOpenIDs, "user:admin")
	j = pendingLLM(t, s)
	s.runLLMJob(context.Background(), j)
	q.mu.Lock()
	replies := len(q.messages)
	q.mu.Unlock()
	updated, _ := s.store.LLMJob(j.ID)
	if len(m.inputs) != 0 || replies != 0 || updated.Status != "canceled" {
		t.Fatal("cached admin result bypassed current permissions")
	}
}

func TestLLMExpiredInputAndEventAliases(t *testing.T) {
	s, m, q := setupLLM(t)
	e := groupEvent("g", "u", "/chat test")
	bindLLM(t, s, e, 42)
	_ = s.store.SetLLMGroup("g", true, s.now())
	e.ReceivedAt = s.now().Add(-time.Hour)
	s.process(context.Background(), e)
	if !strings.Contains(lastReply(t, q), "时效") {
		t.Fatal(lastReply(t, q))
	}
	rate, _ := s.store.LLMRate(s.llmActor("user:u"), llmDay(s.now()), s.now())
	if rate.Count != 0 {
		t.Fatal(rate)
	}
	e.ReceivedAt = s.now()
	processLLMNow(t, s, e)
	e.EventType = "GROUP_AT_MESSAGE_CREATE"
	s.process(context.Background(), e)
	jobs, _ := s.store.PendingLLMJobs()
	if len(jobs) != 0 || len(m.inputs) != 1 {
		t.Fatal("group event alias charged twice")
	}
}

func TestLLMResetRevocationAndShutdownDuringModel(t *testing.T) {
	for _, action := range []string{"reset", "disable", "unbind", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			s, m, q := setupLLM(t)
			e := groupEvent("g", "u", "/chat delayed")
			bindLLM(t, s, e, 42)
			_ = s.store.SetLLMGroup("g", true, s.now())
			s.process(context.Background(), e)
			j := pendingLLM(t, s)
			entered := make(chan struct{})
			release := make(chan struct{})
			m.hook = func(ctx context.Context) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.runLLMJob(ctx, j) }()
			<-entered
			switch action {
			case "reset":
				_ = s.store.ResetLLMSession(j.Session, s.now())
			case "disable":
				_ = s.store.SetLLMGroup("g", false, s.now())
			case "unbind":
				_, _ = s.store.UnbindByNewAPIID(42)
			case "shutdown":
				cancel()
			}
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("task did not stop")
			}
			q.mu.Lock()
			texts := len(q.messages)
			q.mu.Unlock()
			if texts != 0 {
				t.Fatal("revoked task replied")
			}
			session, _ := s.store.LLMSession(j.Session, s.now())
			if session.Ciphertext != "" {
				t.Fatal("revoked task wrote history")
			}
			updated, _ := s.store.LLMJob(j.ID)
			if updated.Status != "canceled" && updated.Status != "uncertain" {
				t.Fatal(updated.Status)
			}
		})
	}
}

func TestLLMTimeoutKeepsQuotaAndNoHistory(t *testing.T) {
	s, m, q := setupLLM(t)
	s.cfg.LLM.TimeoutSeconds = 1
	m.hook = func(ctx context.Context) { <-ctx.Done() }
	e := c2cEvent("u", "/chat delayed")
	bindLLM(t, s, e, 42)
	j := processLLMNow(t, s, e)
	if j.Status != "sent" || !strings.Contains(lastReply(t, q), "超时") {
		t.Fatal(j.Status, lastReply(t, q))
	}
	session, _ := s.store.LLMSession(j.Session, s.now())
	if session.Ciphertext != "" {
		t.Fatal("failed turn persisted")
	}
	rate, _ := s.store.LLMRate(j.Actor, llmDay(s.now()), s.now())
	if rate.Count != 1 {
		t.Fatal(rate)
	}
}

func TestLLMHistoryTrimAndSourcesSurviveTruncation(t *testing.T) {
	cfg := llm.DefaultConfig()
	cfg.HistoryTurns = 2
	turns := []llmTurn{{User: "one", Assistant: "one"}, {User: "two", Assistant: "two"}, {User: "three", Assistant: "three"}}
	got := trimLLMTurns(turns, cfg)
	if len(got) != 2 || got[0].User != "two" {
		t.Fatal(got)
	}
	cfg.HistoryBytes = 4096
	got = trimLLMTurns([]llmTurn{{User: strings.Repeat("字", 3000), Assistant: "huge"}, {User: "small", Assistant: "small"}}, cfg)
	if len(got) != 1 || got[0].User != "small" {
		t.Fatal(got)
	}
	s, m, _ := setupLLM(t)
	s.cfg.LLM.SearchBackend = "tavily"
	s.qq = &sequenceLLMQQ{}
	m.responses = []llm.Completion{toolCompletion("web_search", `{"query":"query"}`), {Message: llm.Message{Content: strings.Repeat("字", 10000)}}}
	e := c2cEvent("u", "/chat search")
	bindLLM(t, s, e, 42)
	j := processLLMNow(t, s, e)
	data, err := s.secure.Decrypt(j.Result)
	if err != nil {
		t.Fatal(err)
	}
	var result llmSavedResult
	_ = json.Unmarshal([]byte(data), &result)
	if len([]rune(result.Text)) > 6000 || !strings.Contains(result.Text, "https://example.test/source") {
		t.Fatal("truncation removed real citation")
	}
}

func TestLLMBusinessPrivacyTaintSurvivesFollowups(t *testing.T) {
	s, m, _ := setupLLM(t)
	e := c2cEvent("u", "/chat balance")
	bindLLM(t, s, e, 42)
	m.responses = []llm.Completion{
		toolCompletion("account", "{}"),
		{Message: llm.Message{Content: "PRIVATE ORIGINAL"}},
		{Message: llm.Message{Content: "COPIED PRIVATE DATA"}},
		{Message: llm.Message{Content: "new account"}},
	}
	processLLMNow(t, s, e)
	e.Message.ID = "followup"
	e.Message.Content = "/chat summarize previous answer"
	processLLMNow(t, s, e)
	if _, err := s.store.UnbindByNewAPIID(42); err != nil {
		t.Fatal(err)
	}
	bindLLM(t, s, e, 43)
	e.Message.ID = "newaccount"
	e.Message.Content = "/chat context"
	processLLMNow(t, s, e)
	for _, msg := range m.inputs[3] {
		if strings.Contains(msg.Content, "PRIVATE") {
			t.Fatal("followup laundered private history across bindings")
		}
	}
}

func TestLLMAdminHistoryNotReusedAfterPrivilegeRemoval(t *testing.T) {
	s, m, _ := setupLLM(t)
	e := c2cEvent("admin", "/chat admin report")
	bindLLM(t, s, e, 42)
	m.responses = []llm.Completion{toolCompletion("admin_report", "{}"), {Message: llm.Message{Content: "ADMIN PRIVATE REPORT"}}, {Message: llm.Message{Content: "ordinary"}}}
	processLLMNow(t, s, e)
	delete(s.cfg.QQAdminOpenIDs, "user:admin")
	e.Message.ID = "ordinary"
	e.Message.Content = "/chat repeat the report"
	processLLMNow(t, s, e)
	for _, msg := range m.inputs[2] {
		if strings.Contains(msg.Content, "ADMIN PRIVATE REPORT") {
			t.Fatal("revoked administrator history reused")
		}
	}
}

func TestLLMOutputDoesNotExecuteQQControlMarkup(t *testing.T) {
	s, m, q := setupLLM(t)
	m.responses = []llm.Completion{{Message: llm.Message{Content: `<qqbot-at-everyone /> <qqbot-at-user id="target" /> ordinary <T>`}}}
	e := c2cEvent("u", "/chat echo markup")
	bindLLM(t, s, e, 42)
	processLLMNow(t, s, e)
	if text := lastReply(t, q); strings.Contains(text, "<qqbot-") || !strings.Contains(text, "ordinary <T>") {
		t.Fatal(text)
	}
}
