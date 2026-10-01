package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func TestRootHelpFiltersRolesAndListsEachRootOnce(t *testing.T) {
	for _, user := range []string{"ordinary", "admin", "readonly"} {
		t.Run(user, func(t *testing.T) {
			s, _, _, qqAPI, _ := testService(t)
			s.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
			s.process(context.Background(), c2cEvent(user, "/help"))
			text := lastReply(t, qqAPI)
			seen := make(map[string]bool)
			for _, line := range strings.Split(text, "\n") {
				if !strings.HasPrefix(line, "/") {
					continue
				}
				path, _, ok := strings.Cut(line, " - ")
				if !ok || strings.Contains(path, " ") || seen[path] {
					t.Fatalf("not a unique first-level command: %q", line)
				}
				seen[path] = true
			}
			for _, root := range []string{"/bind", "/checkin", "/plan", "/enable", "/disable", "/vendor_status"} {
				if !seen[root] {
					t.Fatalf("missing accessible root %s: %q", root, text)
				}
			}
			for _, root := range []string{"/admin", "/credit", "/vendor_config", "/join", "/mute"} {
				if seen[root] != (user != "ordinary") {
					t.Fatalf("incorrect role visibility for %s: %q", root, text)
				}
			}
			for _, root := range []string{"/welcome", "/recall", "/benefit", "/confirm", "/vendor_subscribe"} {
				if seen[root] != (user == "admin") {
					t.Fatalf("incorrect write visibility for %s: %q", root, text)
				}
			}
			if !strings.Contains(text, "末尾添加 help") {
				t.Fatalf("missing next-level instruction: %q", text)
			}
		})
	}
}

func TestHelpNavigatesOneLevelWithoutBinding(t *testing.T) {
	tests := []struct {
		user, command string
		want, absent  []string
	}{
		{"ordinary", "/checkin help", []string{"/checkin status", "用法：/checkin"}, []string{"/checkin reset"}},
		{"admin", "/checkin help", []string{"/checkin status", "/checkin reset"}, nil},
		{"ordinary", "/plan help", []string{"/plan view"}, []string{"/plan add", "/plan sub", "<用户ID或@用户>"}},
		{"admin", "/admin help", []string{"/admin bindings", "/admin user", "/admin report", "/admin user help"}, []string{"/admin user status", "/admin report export"}},
		{"admin", "/admin user help", []string{"/admin user status <用户ID或@用户>", "/admin user enable"}, []string{"/admin bindings"}},
		{"admin", "/admin report help", []string{"/admin report export [时间长度]"}, []string{"/admin user"}},
		{"admin", "/reset help", []string{"/reset set", "/reset set help"}, []string{"/reset set duration"}},
		{"admin", "/reset set help", []string{"/reset set duration <时长>", "/reset set winners <人数>", "/reset set lookback <时长>"}, []string{"/reset join"}},
		{"admin", "/reset set duration help", []string{"用法：/reset set duration <时长>", "没有可用的下一级"}, nil},
		{"ordinary", "/notify help", []string{"/notify daily"}, []string{"/notify daily on"}},
		{"ordinary", "/notify daily help", []string{"/notify daily on", "/notify daily off"}, nil},
		{"ordinary", "/CHECKIN   HeLp", []string{"/checkin status"}, []string{"/checkin reset"}},
		{"ordinary", "/enable help", []string{"/enable list"}, []string{"关键词>", "管理命令"}},
		{"admin", "/vendor_config custom help", []string{"/vendor_config custom add", "/vendor_config custom set"}, nil},
		{"admin", "/vendor_config sources openai help", []string{"/vendor_config sources openai on|off"}, nil},
		{"admin", "/vendor_config group_whitelist help", []string{"/vendor_config group_whitelist add <group_openid>", "/vendor_config group_whitelist clear"}, nil},
		{"admin", "/vendor_config custom_statuspage_sources help", []string{"/vendor_config custom_statuspage_sources enable", "/vendor_config custom_statuspage_sources clear"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.user+tt.command, func(t *testing.T) {
			s, _, api, qqAPI, mail := testService(t)
			s.process(context.Background(), c2cEvent(tt.user, tt.command))
			text := lastReply(t, qqAPI)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %q", want, text)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(text, absent) {
					t.Fatalf("unexpected %q: %q", absent, text)
				}
			}
			if api.quotaAdds != 0 || mail.code != "" {
				t.Fatal("help performed a write")
			}
		})
	}
}

func TestHelpRejectsInaccessibleAndUnknownPaths(t *testing.T) {
	for _, command := range []string{"/admin help", "/checkin reset help", "/reset set help", "/vendor_config help", "/unknown help", "/reset bogus help", "/plan view 42 help"} {
		t.Run(command, func(t *testing.T) {
			s, _, _, qqAPI, _ := testService(t)
			s.process(context.Background(), c2cEvent("ordinary", command))
			if text := lastReply(t, qqAPI); !strings.HasPrefix(text, "没有可用的命令帮助") {
				t.Fatalf("unexpected reply: %q", text)
			}
		})
	}
}

func TestReadOnlyHelpShowsOnlyQueriesAtEveryLevel(t *testing.T) {
	tests := []struct {
		path         string
		want, absent []string
	}{
		{"/checkin", []string{"/checkin status"}, []string{"用法：/checkin", "/checkin reset"}},
		{"/credit", []string{"/credit show"}, []string{"/credit add", "/credit sub"}},
		{"/bind", []string{"/bind status"}, []string{"/bind verify", "用法：/bind"}},
		{"/reset", []string{"/reset check", "/reset last"}, []string{"/reset join", "/reset set", "/reset new"}},
		{"/admin user", []string{"/admin user status"}, []string{"/admin user enable", "/admin user disable"}},
		{"/vendor_config", []string{"/vendor_config show", "/vendor_config enabled"}, []string{"/vendor_config reset", "/vendor_config set", "on|off", "修改"}},
		{"/vendor_config custom", []string{"查看自定义状态源"}, []string{"custom add", "custom remove", "custom set"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			s, _, _, _, _ := testService(t)
			s.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
			text := s.helpTextFor(model.QQIdentity{UserOpenID: "readonly"}, tt.path)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %q", want, text)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(text, absent) {
					t.Fatalf("unexpected %q: %q", absent, text)
				}
			}
			s.process(context.Background(), c2cEvent("readonly", tt.path+" help"))
		})
	}
}

func TestHelpRulesPruneUnavailableParentsAndPreserveSiblings(t *testing.T) {
	s, storage, _, qqAPI, _ := testService(t)
	admin := model.QQIdentity{UserOpenID: "admin"}
	for _, keyword := range []string{"reset set winners", "plan view", "bind", "enable", "off"} {
		if err := storage.PutCommandRule(model.CommandRule{Keyword: keyword, Enabled: false}); err != nil {
			t.Fatal(err)
		}
	}
	text := s.helpTextFor(admin, "/reset set")
	if strings.Contains(text, "/reset set winners") || !strings.Contains(text, "/reset set duration") {
		t.Fatalf("bad leaf filtering: %q", text)
	}
	text = s.helpTextFor(model.QQIdentity{UserOpenID: "ordinary"}, "")
	if strings.Contains(text, "/plan -") || strings.Contains(text, "/bind -") || !strings.Contains(text, "/enable -") {
		t.Fatalf("bad parent pruning: %q", text)
	}
	s.process(context.Background(), c2cEvent("ordinary", "/enable help"))
	if text = lastReply(t, qqAPI); !strings.Contains(text, "/enable list") {
		t.Fatalf("management help locked out by rule: %q", text)
	}
}

func TestHelpFilteringDoesNotMatchDescriptions(t *testing.T) {
	s, storage, _, _, _ := testService(t)
	if err := storage.PutCommandRule(model.CommandRule{Keyword: "查看", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if text := s.helpTextFor(model.QQIdentity{}, ""); !strings.Contains(text, "/me -") {
		t.Fatalf("description was treated as a command: %q", text)
	}
}

func TestHelpCoversVendorOptionsAndChunksLongReplies(t *testing.T) {
	s, _, _, qqAPI, _ := testService(t)
	text := s.helpTextFor(model.QQIdentity{UserOpenID: "admin"}, "/vendor_config")
	for _, option := range vendorstatus.Options {
		if !strings.Contains(text, "/vendor_config "+option.Key+" - ") {
			t.Fatalf("missing vendor option %s", option.Key)
		}
	}
	for _, hidden := range []string{"/vendor_config sources openai", "/vendor_config custom add", "/vendor_config group_whitelist add"} {
		if strings.Contains(text, hidden+" ") {
			t.Fatalf("vendor help expanded a grandchild: %q", hidden)
		}
	}
	s.process(context.Background(), c2cEvent("admin", "/vendor_config help"))
	qqAPI.mu.Lock()
	defer qqAPI.mu.Unlock()
	if len(qqAPI.messages) < 2 {
		t.Fatal("expected long vendor help to be chunked")
	}
	for _, message := range qqAPI.messages {
		if len([]rune(message)) > 1500 {
			t.Fatal("help chunk exceeds size limit")
		}
	}
	if strings.Join(qqAPI.messages, "\n") != text {
		t.Fatal("chunking lost help content")
	}
}

func TestAllHelpPathsStayWithinOneLevelAndRespectWriteGuards(t *testing.T) {
	s, _, _, _, _ := testService(t)
	s.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
	parents := map[string]bool{"": true}
	for _, entry := range commandHelpEntries(s.cfg) {
		parents[entry.path] = true
	}
	for _, user := range []string{"ordinary", "admin", "readonly"} {
		identity := model.QQIdentity{UserOpenID: user}
		for parent := range parents {
			text := s.helpTextFor(identity, parent)
			for _, line := range strings.Split(text, "\n") {
				usage := strings.TrimPrefix(line, "用法：")
				if !strings.HasPrefix(usage, "/") {
					continue
				}
				usage, _, _ = strings.Cut(usage, " - ")
				fields := strings.Fields(usage)
				if len(fields) == 0 {
					t.Fatal("empty command usage")
				}
				if user == "readonly" && strings.HasPrefix(line, "用法：") && readOnlyAdminWriteCommand(fields[0], fields) {
					t.Fatalf("%s exposed a read-only write: %q", parent, line)
				}
				for _, entry := range commandHelpEntries(s.cfg) {
					if usage != entry.path && !strings.HasPrefix(usage, entry.path+" ") {
						continue
					}
					if len(strings.Fields(entry.path)) > len(strings.Fields(parent))+1 {
						t.Fatalf("%s expanded a grandchild for %s: %q", parent, user, line)
					}
				}
			}
		}
	}
}

func TestSubcommandHelpAutoRecalls(t *testing.T) {
	s, _, _, qqAPI, _ := testService(t)
	recaller := &recallQQ{fakeQQ: qqAPI}
	s.qq = recaller
	s.cfg.CheckinAutoRecallAfter = 10 * time.Millisecond
	event := groupEvent("g", "ordinary", "/checkin help")
	s.process(context.Background(), event)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		recaller.mu.Lock()
		done := len(recaller.recalled) == 2
		recaller.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("subcommand help and its triggering command were not recalled")
}
