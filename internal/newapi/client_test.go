package newapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDisplayToQuota(t *testing.T) {
	tests := []struct {
		display string
		unit    int64
		want    int64
		ok      bool
	}{
		{"1", 500000, 500000, true},
		{"0.01", 500000, 5000, true},
		{"1.234567", 1000000, 1234567, true},
		{"0.000001", 500000, 0, false},
		{"0", 500000, 0, false},
		{"-1", 500000, 0, false},
	}
	for _, test := range tests {
		got, err := DisplayToQuota(test.display, test.unit)
		if test.ok && err != nil {
			t.Fatalf("DisplayToQuota(%q): %v", test.display, err)
		}
		if !test.ok && err == nil {
			t.Fatalf("DisplayToQuota(%q) expected error", test.display)
		}
		if test.ok && got != test.want {
			t.Fatalf("DisplayToQuota(%q)=%d, want %d", test.display, got, test.want)
		}
	}
}

func TestQuotaToDisplay(t *testing.T) {
	if got := QuotaToDisplay(505000, 500000); got != "1.01" {
		t.Fatalf("got %q", got)
	}
}

func TestSubtractQuotaUsesSubtractMode(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/manage" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"","data":null}`))
	}))
	defer server.Close()

	client := New(server.URL, "test-token", 1, 3*time.Second)
	if err := client.SubtractQuota(context.Background(), 42, 500000); err != nil {
		t.Fatal(err)
	}
	if body["action"] != "add_quota" || body["mode"] != "subtract" || body["id"] != float64(42) || body["value"] != float64(500000) {
		t.Fatalf("unexpected body: %#v", body)
	}
}

func TestSubscriptionAdminEndpoints(t *testing.T) {
	var createPlanID int
	var invalidated bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/subscription/admin/users/42/subscriptions":
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":[{"subscription":{"id":9,"user_id":42,"plan_id":3,"status":"active","start_time":1700000000,"end_time":1701000000,"created_at":1700000000}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/subscription/admin/users/42/subscriptions":
			var body map[string]int
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			createPlanID = body["plan_id"]
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":null}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/subscription/admin/user_subscriptions/9/invalidate":
			invalidated = true
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":null}`))
		default:
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New(server.URL, "test-token", 1, 3*time.Second)
	records, err := client.ListUserSubscriptions(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Subscription.ID != 9 || records[0].Subscription.PlanID != 3 {
		t.Fatalf("unexpected subscriptions: %#v", records)
	}
	if err := client.CreateUserSubscription(context.Background(), 42, 3); err != nil {
		t.Fatal(err)
	}
	if createPlanID != 3 {
		t.Fatalf("plan_id=%d", createPlanID)
	}
	if err := client.InvalidateUserSubscription(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if !invalidated {
		t.Fatal("invalidate endpoint was not called")
	}
}

func TestInsightsEndpointsAndUserFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/2":
			_, _ = w.Write([]byte(`{"success":false,"message":"No permission to access users of same or higher level","data":null}`))
		case "/api/user/":
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":{"items":[{"id":2,"username":"admin","group":"default","quota":123}],"total":1}}`))
		case "/api/data/users":
			if r.URL.Query().Get("start_timestamp") == "" || r.URL.Query().Get("end_timestamp") == "" {
				t.Error("missing usage timestamps")
			}
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":[{"username":"admin","created_at":1700000000,"token_used":12,"count":2,"quota":500000}]}`))
		case "/api/data":
			if r.URL.Query().Get("username") != "admin" {
				t.Errorf("username=%q", r.URL.Query().Get("username"))
			}
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":[{"username":"admin","model_name":"gpt-test","count":2,"quota":500000}]}`))
		case "/api/log/":
			if r.URL.Query().Get("type") != "0" || r.URL.Query().Get("username") != "admin" {
				t.Errorf("unexpected log query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":{"items":[{"id":1,"user_id":2,"username":"admin","model_name":"gpt-test","prompt_tokens":10,"completion_tokens":5}],"total":1}}`))
		case "/api/channel/models_enabled":
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":["gpt-test","gpt-test","claude-test"]}`))
		default:
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New(server.URL, "test-token", 1, 3*time.Second)
	user, err := client.GetUser(context.Background(), 2)
	if err != nil || user.Username != "admin" || user.Group != "default" {
		t.Fatalf("fallback user=%#v err=%v", user, err)
	}
	start := time.Unix(1700000000, 0)
	end := start.Add(time.Hour)
	usage, err := client.ListUsageByUser(context.Background(), start, end)
	if err != nil || len(usage) != 1 || usage[0].TokenUsed != 12 {
		t.Fatalf("usage=%#v err=%v", usage, err)
	}
	modelsUsage, err := client.ListUsageByModel(context.Background(), start, end, "admin")
	if err != nil || len(modelsUsage) != 1 || modelsUsage[0].ModelName != "gpt-test" {
		t.Fatalf("model usage=%#v err=%v", modelsUsage, err)
	}
	logs, err := client.ListLogs(context.Background(), start, end, "admin", 1, 10)
	if err != nil || len(logs.Items) != 1 || logs.Items[0].CompletionTokens != 5 {
		t.Fatalf("logs=%#v err=%v", logs, err)
	}
	models, err := client.ListEnabledModels(context.Background())
	if err != nil || len(models) != 2 || models[0] != "claude-test" {
		t.Fatalf("models=%#v err=%v", models, err)
	}
}
