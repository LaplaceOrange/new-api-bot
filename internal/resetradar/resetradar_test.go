package resetradar

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		kind  string
		score int
		want  Stage
	}{
		{
			name: "confirmed",
			text: "We have reset usage limits for all paid Codex and ChatGPT Work users.",
			want: StageConfirmed,
		},
		{
			name: "imminent",
			text: "New usage reset for paid Codex and ChatGPT Work users lands in the next hour.",
			want: StageImminent,
		},
		{
			name: "future reset before limits",
			text: "A Codex reset for weekly limits is coming later today.",
			want: StageImminent,
		},
		{
			name: "expected reset",
			text: "We expect the Codex usage limits to reset in a few hours.",
			want: StageImminent,
		},
		{
			name: "prediction with reset noun",
			text: "The next Codex usage reset is predicted for tomorrow.",
			want: StageImminent,
		},
		{
			name: "probabilistic future",
			text: "OpenAI may reset Codex usage limits later tonight.",
			want: StageImminent,
		},
		{
			name: "scheduled future",
			text: "Codex weekly limits are scheduled to reset this week.",
			want: StageImminent,
		},
		{
			name: "in progress wording with future date",
			text: "OpenAI is resetting Codex weekly limits next week.",
			want: StageImminent,
		},
		{
			name: "future plan otherwise resembles confirmation",
			text: "We have decided to reset Codex usage limits.",
			want: StageImminent,
		},
		{
			name: "scheduled reset noun",
			text: "The Codex usage reset has been scheduled for tomorrow.",
			want: StageImminent,
		},
		{
			name: "chinese forecast",
			text: "预计 Codex 使用额度将在今晚重置。",
			want: StageImminent,
		},
		{
			name: "chinese single future marker",
			text: "Codex 使用额度将在明天重置。",
			want: StageImminent,
		},
		{
			name: "completed wins over unrelated future wording",
			text: "Codex usage limits have been reset and the new allocation is now live; more details will follow tomorrow.",
			want: StageConfirmed,
		},
		{
			name: "reset now live",
			text: "The Codex usage reset is now live for ChatGPT Work accounts.",
			want: StageConfirmed,
		},
		{
			name: "just reset wins over next reset date",
			text: "We just reset Codex usage limits; the next reset will be tomorrow.",
			want: StageConfirmed,
		},
		{
			name: "chinese completed",
			text: "Codex 使用额度已完成重置，现已生效。",
			want: StageConfirmed,
		},
		{
			name:  "aggregator hint",
			text:  "Codex and ChatGPT Work usage limits may change tomorrow.",
			kind:  "hint",
			score: 94,
			want:  StageImminent,
		},
		{
			name: "possible public post",
			text: "Thinking about another Codex usage limit reset.",
			kind: "x-public",
			want: StagePossible,
		},
		{
			name:  "negative",
			text:  "There will be no Codex usage limit resets today.",
			kind:  "hint",
			score: 99,
			want:  StageUnknown,
		},
		{
			name: "off topic",
			text: "Image generation limits will reset soon.",
			kind: "x-public",
			want: StageUnknown,
		},
		{
			name: "resolved status is not reset",
			text: "Codex usage limits incident. All services have recovered.",
			kind: "official-status",
			want: StageUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Classify(test.text, test.kind, test.score); got != test.want {
				t.Fatalf("Classify()=%s, want %s", got, test.want)
			}
		})
	}
}

func TestProxyValidationAndMask(t *testing.T) {
	valid := []string{
		"",
		"off",
		"http://127.0.0.1:8080",
		"http://user:secret@example.com:3128",
		"socks5://user:p%40ss@localhost:1080",
	}
	for _, value := range valid {
		if err := ValidateProxyURL(value); err != nil {
			t.Fatalf("ValidateProxyURL(%q): %v", value, err)
		}
	}
	invalid := []string{
		"https://localhost:8080",
		"socks5://localhost",
		"http://:8080",
		"http://localhost:70000",
		"http://localhost:8080/path",
	}
	for _, value := range invalid {
		if err := ValidateProxyURL(value); err == nil {
			t.Fatalf("ValidateProxyURL(%q) expected error", value)
		}
	}
	masked := MaskedProxy("socks5://alice:plain-secret@proxy.example:1080")
	if strings.Contains(masked, "plain-secret") || !strings.Contains(masked, "xxxxx") {
		t.Fatalf("proxy credentials were not masked: %q", masked)
	}
}

func TestProxyValidationErrorDoesNotExposeInput(t *testing.T) {
	values := []string{
		"http://alice:plain-secret@localhost:not-a-port",
		"http://alice:another-secret@local%zz:8080",
	}
	for _, value := range values {
		err := ValidateProxyURL(value)
		if err == nil {
			t.Fatalf("ValidateProxyURL(%q) expected error", value)
		}
		message := err.Error()
		for _, secret := range []string{value, "alice", "plain-secret", "another-secret"} {
			if strings.Contains(message, secret) {
				t.Fatalf("validation error exposed proxy input %q: %q", secret, message)
			}
		}
	}
}

func TestParseStatusJSON(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	body := []byte(`{
		"incidents":[
			{"id":"inc-1","name":"Codex usage limits depleting faster than expected","status":"investigating","created_at":"2026-08-12T10:00:00Z","incident_updates":[{"id":"upd-1","status":"investigating","created_at":"2026-08-12T10:30:00Z","body":"We are investigating reports affecting Codex usage limits."}]},
			{"id":"inc-2","name":"Codex usage limits incident","status":"resolved","created_at":"2026-08-12T08:00:00Z","incident_updates":[{"id":"upd-2","status":"resolved","created_at":"2026-08-12T11:00:00Z","body":"We have reset usage limits for all Codex and ChatGPT Work users."}]},
			{"id":"old","name":"Codex usage limits incident","status":"investigating","created_at":"2026-08-10T08:00:00Z"},
			{"id":"other","name":"Image generation errors","status":"investigating","created_at":"2026-08-12T11:00:00Z"}
		]
	}`)
	signals, err := parseStatusJSON(body, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 {
		t.Fatalf("signals=%d, want 2: %#v", len(signals), signals)
	}
	if signals[0].Stage != StagePossible || signals[0].ID != "status:inc-1:upd-1" {
		t.Fatalf("unexpected possible signal: %#v", signals[0])
	}
	if signals[1].Stage != StageConfirmed || signals[1].CreatedAt.Hour() != 11 {
		t.Fatalf("unexpected confirmed signal: %#v", signals[1])
	}
}

func TestParseAggregator(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	body := []byte(`prefix status:"degraded",updatedAt:"2026-08-12T11:00:00Z",forecastStatus:"current",score:91,activeSignals:$R[1]=[$R[2]={id:"2087423996115681767",kind:"hint",score:94,text:"Codex and ChatGPT Work usage limit reset is coming tomorrow.\nHold tight.",createdAt:"2026-08-12T06:20:37.000Z",sourceUrl:"https://x.com/thsottiaux/status/2087423996115681767"}],signals:suffix`)
	result, err := parseAggregator(body, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.status != "degraded" || result.score != 91 || len(result.signals) != 1 {
		t.Fatalf("unexpected aggregate result: %#v", result)
	}
	if result.signals[0].Stage != StageImminent || !strings.Contains(result.signals[0].Text, "\n") {
		t.Fatalf("unexpected signal: %#v", result.signals[0])
	}
}

func TestParseXPublicPage(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	id := "2081940052154933696"
	key := base64.StdEncoding.EncodeToString([]byte("Tweet:" + id))
	body := []byte(`TimelineTimelineEntry:tweet-` + id + `","client:` + key + `:details":$R[116]={__id:"client:` + key + `:details",__typename:"TBirdData",full_text:"The usage limits have been reset for all paid users of Codex and ChatGPT Work.",created_at_ms:1786532400000},"client:next":$R[117]={value:1}`)
	signals, err := parseXPublicPage(body, "thsottiaux", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].ID != "x:"+id || signals[0].Stage != StageConfirmed {
		t.Fatalf("unexpected signals: %#v", signals)
	}
}

func TestMergeSignalsKeepsHighestStage(t *testing.T) {
	byID := make(map[string]Signal)
	now := time.Now()
	mergeSignals(byID, []Signal{{ID: "x:1", Stage: StagePossible, CreatedAt: now}})
	mergeSignals(byID, []Signal{{ID: "x:1", Stage: StageConfirmed, CreatedAt: now.Add(-time.Minute)}})
	if byID["x:1"].Stage != StageConfirmed {
		t.Fatalf("stage=%s, want confirmed", byID["x:1"].Stage)
	}
}

func TestConfigurableSignalMaxAge(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"incidents":[{"id":"inc","name":"Codex usage limits incident","status":"investigating","created_at":"2026-08-12T10:30:00Z"}]}`)
	signals, err := parseStatusJSON(body, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Fatalf("signals=%d, want 0 for one-hour max age", len(signals))
	}
	signals, err = parseStatusJSON(body, now, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 {
		t.Fatalf("signals=%d, want 1 for two-hour max age", len(signals))
	}
}
