package bot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fsykk/new-api-bot/internal/newapi"
)

type hongbaoGroupLookupAPI struct {
	*fakeNewAPI
	getUserErr error
	userIDs    []int
}

func (f *hongbaoGroupLookupAPI) GetUser(ctx context.Context, id int) (newapi.User, error) {
	f.userIDs = append(f.userIDs, id)
	if f.getUserErr != nil {
		return newapi.User{}, f.getUserErr
	}
	return f.fakeNewAPI.GetUser(ctx, id)
}

func TestHongbaoGroupRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groups  []string
		current string
		allowed bool
	}{
		{"unrestricted", nil, "default", true},
		{"first-group", []string{"gpt-cheap", "gpt-smart"}, "gpt-cheap", true},
		{"second-group", []string{"gpt-cheap", "gpt-smart"}, "gpt-smart", true},
		{"other-group", []string{"gpt-cheap", "gpt-smart"}, "default", false},
		{"exact-not-prefix", []string{"gpt-cheap"}, "gpt-cheap-extra", false},
		{"case-sensitive", []string{"gpt-cheap"}, "GPT-cheap", false},
		{"empty-account-group", []string{"gpt-cheap"}, "", false},
		{"duplicate-names", []string{"gpt-cheap", "gpt-smart", "gpt-cheap"}, "gpt-smart", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, storage, api, qqAPI, _ := testService(t)
			lookup := &hongbaoGroupLookupAPI{fakeNewAPI: api}
			service.newAPI = lookup
			createTestHongbao(t, service, "10", "1", tc.groups...)
			before, err := storage.GetHongbao("g")
			if err != nil {
				t.Fatal(err)
			}
			wantGroups := uniqueHongbaoGroups(tc.groups)
			if !slices.Equal(before.AllowedGroups, wantGroups) {
				t.Fatalf("stored groups = %v, want %v", before.AllowedGroups, wantGroups)
			}
			if len(tc.groups) > 0 && !strings.Contains(lastReply(t, qqAPI), "领取分组："+strings.Join(wantGroups, "、")) {
				t.Fatal("creation announcement missing allowed groups")
			}
			if len(lookup.userIDs) != 0 {
				t.Fatal("creation incorrectly required creator's account group")
			}
			api.user.Group = tc.current
			bindHongbaoUser(t, storage, "g", "alice", 42)
			service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
			after, err := storage.GetHongbao("g")
			if err != nil {
				t.Fatal(err)
			}
			if len(tc.groups) == 0 {
				if len(lookup.userIDs) != 0 {
					t.Fatal("unrestricted packet performed unnecessary group lookup")
				}
			} else if !slices.Equal(lookup.userIDs, []int{42}) {
				t.Fatalf("queried users = %v, want bound account 42", lookup.userIDs)
			}
			if tc.allowed {
				if api.quotaAdds != 1 || after.Claims[42].Status != "granted" || !after.SummarySent {
					t.Fatalf("allowed claim not granted: adds=%d packet=%+v", api.quotaAdds, after)
				}
			} else {
				if api.quotaAdds != 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("denied claim consumed quota or count: before=%+v after=%+v", before, after)
				}
				if got := lastReply(t, qqAPI); !strings.Contains(got, "无权限领取本轮红包") {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestHongbaoGroupRestrictionUsesCurrentAccountGroupAfterRestart(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	lookup := &hongbaoGroupLookupAPI{fakeNewAPI: api}
	service.newAPI = lookup
	createTestHongbao(t, service, "1", "2", "gpt-cheap", "gpt-smart")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	api.user.Group = "default"
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 0 || !strings.Contains(lastReply(t, qqAPI), "无权限领取") {
		t.Fatal("ineligible group could claim")
	}
	restarted := New(service.cfg, storage, service.secure, lookup, qqAPI, service.mailer, service.logger)
	api.user.Group = "gpt-smart"
	restarted.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	packet, _ := storage.GetHongbao("g")
	if api.quotaAdds != 1 || packet.RemainingCount != 1 || packet.Claims[42].Status != "granted" {
		t.Fatal("eligible group after restart could not claim")
	}
	api.user.Group = "default"
	restarted.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 1 || !strings.Contains(lastReply(t, qqAPI), "无权限领取") {
		t.Fatal("group change was not revalidated")
	}
	api.user.Group = "gpt-cheap"
	restarted.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 1 || !strings.Contains(lastReply(t, qqAPI), "本轮红包已领取") {
		t.Fatal("group change allowed duplicate claim")
	}
	if len(lookup.userIDs) != 4 {
		t.Fatalf("group lookups = %d, want 4", len(lookup.userIDs))
	}
}

func TestHongbaoGroupLookupFailureDoesNotAllocate(t *testing.T) {
	service, storage, api, qqAPI, _ := testService(t)
	lookup := &hongbaoGroupLookupAPI{fakeNewAPI: api, getUserErr: errors.New("lookup failed")}
	service.newAPI = lookup
	createTestHongbao(t, service, "1", "1", "gpt-cheap")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	before, _ := storage.GetHongbao("g")
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	after, _ := storage.GetHongbao("g")
	if api.quotaAdds != 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("failed group lookup consumed a packet")
	}
	if !strings.Contains(lastReply(t, qqAPI), "读取账户分组失败") {
		t.Fatal(lastReply(t, qqAPI))
	}
	lookup.getUserErr = nil
	api.user.Group = "gpt-cheap"
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 1 {
		t.Fatal("group lookup failure prevented retry")
	}
}

func TestHongbaoAcceptsManyGroupArguments(t *testing.T) {
	service, storage, api, _, _ := testService(t)
	groups := make([]string, 256)
	for i := range groups {
		groups[i] = fmt.Sprintf("group-%03d", i)
	}
	createTestHongbao(t, service, "1", "1", groups...)
	packet, err := storage.GetHongbao("g")
	if err != nil || !slices.Equal(packet.AllowedGroups, groups) {
		t.Fatalf("stored groups = %v, err = %v", packet.AllowedGroups, err)
	}
	api.user.Group = groups[len(groups)-1]
	bindHongbaoUser(t, storage, "g", "alice", 42)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 1 {
		t.Fatal("last group in long argument list could not claim")
	}
}

func TestHongbaoUnrestrictedPacketDoesNotNeedGroupLookup(t *testing.T) {
	service, storage, api, _, _ := testService(t)
	lookup := &hongbaoGroupLookupAPI{fakeNewAPI: api, getUserErr: errors.New("group lookup unavailable")}
	service.newAPI = lookup
	createTestHongbao(t, service, "1", "1")
	bindHongbaoUser(t, storage, "g", "alice", 42)
	service.process(context.Background(), groupEvent("g", "alice", "/hongbao"))
	if api.quotaAdds != 1 || len(lookup.userIDs) != 0 {
		t.Fatal("legacy/unrestricted packet unexpectedly required group lookup")
	}
}
