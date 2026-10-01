package vendorstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProtocolCommitsBeforeSendingAndContinuesAfterFailedTarget(t *testing.T) {
	output := strings.NewReader(
		`{"type":"checkpoint","key":"monitor_state_v1","value":{"version":1}}` + "\n" +
			`{"type":"send","group":"first","text":"one"}` + "\n" +
			`{"type":"send","group":"second","text":"two"}` + "\n" +
			`{"type":"done"}` + "\n")
	var input bytes.Buffer
	var calls []string
	_, err := consumePackets(context.Background(), output, json.NewEncoder(&input), "cycle", Callbacks{
		Checkpoint: func(key string, value json.RawMessage) error {
			calls = append(calls, "commit:"+key)
			return nil
		},
		Send: func(ctx context.Context, packet Packet) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Fatal("delivery was not deadline bounded")
			}
			calls = append(calls, "send:"+packet.Group)
			if packet.Group == "first" {
				return errors.New("network failure")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"commit:" + StateKey, "send:first", "send:second"}) {
		t.Fatal(calls)
	}
	if got := input.String(); got != "{\"ok\":true}\n{\"ok\":false}\n{\"ok\":true}\n" {
		t.Fatal(got)
	}
}

func TestProtocolStorageFailureCannotSend(t *testing.T) {
	output := strings.NewReader(`{"type":"checkpoint","key":"monitor_state_v1","value":{}}` + "\n" +
		`{"type":"send","group":"group","text":"alert"}` + "\n")
	var input bytes.Buffer
	_, err := consumePackets(context.Background(), output, json.NewEncoder(&input), "cycle", Callbacks{
		Checkpoint: func(string, json.RawMessage) error { return errors.New("disk full") },
		Send:       func(context.Context, Packet) error { t.Fatal("sent before commit"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") || input.Len() != 0 {
		t.Fatal(err, input.String())
	}
}

func TestProtocolRejectsInvalidOrMutatingQueryPackets(t *testing.T) {
	for _, line := range []string{
		`{"type":"checkpoint","key":"monitor_state_v1","value":{}}`,
		`{"type":"checkpoint","key":"other","value":{}}`,
		`{"type":"send","group":"group","text":"alert"}`,
		`{"type":"unknown"}`,
		`{"type":"error","error":"ValueError"}`,
		`not-json`,
		"",
		"{\"type\":\"done\"}\n{\"type\":\"done\"}",
	} {
		t.Run(line, func(t *testing.T) {
			var input bytes.Buffer
			_, err := consumePackets(context.Background(), strings.NewReader(line+"\n"), json.NewEncoder(&input), "query", Callbacks{})
			if err == nil {
				t.Fatal("invalid protocol accepted")
			}
		})
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(SourceIDs) != 20 {
		t.Fatal(SourceIDs)
	}
	if err := DecodeConfig([]byte(`{"sources":{"openai":false},"custom_statuspage_sources":[{"name":"Test","base_url":"https://status.test"}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.CustomStatuspageSources[0].Enabled || cfg.Sources["openai"] || !cfg.NotifyExistingOnFirstStartup {
		t.Fatal(cfg)
	}
	for _, data := range []string{
		`{"display_language":"bad"}`, `{"card_theme":"bad"}`,
		`{"poll_interval_seconds":59}`, `{"history_lookback_hours":169}`,
		`{"source_failure_threshold":0}`, `{"http_timeout_seconds":0}`,
		`{"worker_timeout_seconds":0}`, `{"sources":{"typo":true}}`,
		`{"group_whitelist":["1:GroupMessage:100"]}`, `{"timezone":"Not/AZone"}`,
		`{"translation":{"base_url":"https://host/v1","api_key":"key"}}`,
		`{"custom_statuspage_sources":[{"name":"Test","base_url":"file:///test"}]}`,
		`{"misspelled_option":true}`, `null`,
	} {
		cfg := DefaultConfig()
		if err := DecodeConfig([]byte(data), &cfg); err == nil && cfg.Validate() == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestWorkerDoesNotInheritBotSecrets(t *testing.T) {
	got := workerEnvironment([]string{
		"PATH=/bin", "http_proxy=http://proxy", "SSL_CERT_FILE=/cert.pem",
		"QQ_APP_SECRET=qq-secret", "NEWAPI_ADMIN_TOKEN=admin-secret",
		"BOT_DATA_KEY=data-secret", "SMTP_PASSWORD=smtp-secret",
		"VENDOR_STATUS_TRANSLATION_API_KEY=translation-secret",
		"UNRELATED_SERVICE_SECRET=other-secret", "PYTHONPATH=/untrusted",
	})
	want := []string{
		"PATH=/bin", "http_proxy=http://proxy", "SSL_CERT_FILE=/cert.pem",
		"PYTHONUTF8=1", "PYTHONDONTWRITEBYTECODE=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// This exercises the actual embedded worker against a local status endpoint;
// no QQ messages, translation requests or live vendor requests are performed.
func TestEmbeddedPythonWorkerEndToEnd(t *testing.T) {
	python := os.Getenv("VENDOR_STATUS_TEST_PYTHON")
	if python == "" {
		t.Skip("set VENDOR_STATUS_TEST_PYTHON to run the embedded Python integration")
	}
	var active atomic.Bool
	active.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/summary.json":
			if active.Load() {
				_, _ = w.Write([]byte(`{"status":{"indicator":"minor"},"components":[],"incidents":[{"id":"incident","name":"API outage","status":"investigating","impact":"minor","updated_at":"2026-10-01T00:00:00Z","incident_updates":[{"body":"Investigating","status":"investigating","created_at":"2026-10-01T00:00:00Z"}]}]}`))
			} else {
				_, _ = w.Write([]byte(`{"status":{"indicator":"none"},"components":[],"incidents":[]}`))
			}
		case "/api/v2/incidents.json":
			if active.Load() {
				_, _ = w.Write([]byte(`{"incidents":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"incidents":[{"id":"incident","name":"API outage","status":"resolved","impact":"minor","updated_at":"2026-10-01T00:10:00Z","resolved_at":"2026-10-01T00:10:00Z","incident_updates":[{"body":"Fixed","status":"resolved","created_at":"2026-10-01T00:10:00Z"}]}]}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.Python = python
	cfg.EnableAITranslation = false
	cfg.GroupWhitelist = []string{"100", "200"}
	for _, id := range SourceIDs {
		cfg.Sources[id] = false
	}
	cfg.CustomStatuspageSources = []CustomSource{{Name: "Local vendor", BaseURL: server.URL, Enabled: true}}
	values := map[string]json.RawMessage{}
	failSecond := true
	var sent []string
	callbacks := Callbacks{
		Checkpoint: func(key string, value json.RawMessage) error {
			values[key] = append(json.RawMessage(nil), value...)
			return nil
		},
		Send: func(_ context.Context, packet Packet) error {
			if _, err := png.DecodeConfig(bytes.NewReader(packet.PNG)); err != nil {
				t.Fatal("worker did not render PNG", err)
			}
			sent = append(sent, packet.Group)
			if packet.Group == "200" && failSecond {
				failSecond = false
				return errors.New("simulate QQ failure")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(operation string) Packet {
		t.Helper()
		result, err := (PythonRunner{}).Run(ctx, Request{Operation: operation, Config: cfg, Values: values}, callbacks)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	run("cycle")
	if !reflect.DeepEqual(sent, []string{"100", "200"}) || len(values[StateKey]) == 0 {
		t.Fatal(sent, values)
	}
	sent = nil
	run("cycle") // New process reloads the checkpoint and retries only 200.
	if !reflect.DeepEqual(sent, []string{"200"}) {
		t.Fatal(sent)
	}
	before := append([]byte(nil), values[StateKey]...)
	result := run("query")
	if _, err := png.DecodeConfig(bytes.NewReader(result.PNG)); err != nil || !bytes.Equal(before, values[StateKey]) {
		t.Fatal("query mutated the monitor state or returned no PNG", err)
	}
	active.Store(false)
	sent = nil
	run("cycle")
	if !reflect.DeepEqual(sent, []string{"100", "200"}) {
		t.Fatal("explicit recovery did not reach both targets", sent)
	}
	sent = nil
	run("cycle")
	if len(sent) != 0 {
		t.Fatal("recovery duplicated", sent)
	}
}
