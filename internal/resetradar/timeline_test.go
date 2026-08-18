package resetradar

import (
	"strings"
	"testing"
	"time"
)

func TestTimelineStageMapping(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	activeWindow := &OfficialWindow{StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Minute)}
	futureWindow := &OfficialWindow{StartAt: now.Add(time.Minute), EndAt: now.Add(2 * time.Minute)}
	tests := []struct {
		name   string
		event  timelineEvent
		window *OfficialWindow
		want   Stage
	}{
		{name: "observed", event: timelineEvent{ObservationResult: "reset_observed", ResetVerificationStatus: "rejected"}, want: StageConfirmed},
		{name: "confirmed observation alias", event: timelineEvent{ObservationResult: "confirmed"}, want: StageConfirmed},
		{name: "unchanged wins over confirmed fallback", event: timelineEvent{ObservationResult: "unchanged", ResetVerificationStatus: "confirmed"}, want: StageUnknown},
		{name: "rejected fallback", event: timelineEvent{ResetVerificationStatus: "rejected"}, want: StageUnknown},
		{name: "confirmed fallback", event: timelineEvent{ResetVerificationStatus: "confirmed"}, want: StageConfirmed},
		{name: "pending", event: timelineEvent{ResetVerificationStatus: "pending"}, want: StagePossible},
		{name: "archive pending is not history", event: timelineEvent{Source: "archive", ResetVerificationStatus: "pending"}, want: StagePossible},
		{name: "archive unverified", event: timelineEvent{Source: "archive", ResetVerificationStatus: "unverified"}, want: StageUnknown},
		{name: "archive expired", event: timelineEvent{Source: "archive", ResetVerificationStatus: "expired"}, want: StageUnknown},
		{name: "observation unknown wins over confirmed fallback", event: timelineEvent{ObservationResult: "unknown", ResetVerificationStatus: "confirmed"}, want: StagePossible},
		{name: "announced active window", event: timelineEvent{AnnouncementState: "announced"}, window: activeWindow, want: StageImminent},
		{name: "hinted active window", event: timelineEvent{AnnouncementState: "hinted"}, window: activeWindow, want: StageImminent},
		{name: "none active window", event: timelineEvent{AnnouncementState: "none"}, window: activeWindow, want: StagePossible},
		{name: "future window", event: timelineEvent{AnnouncementState: "announced"}, window: futureWindow, want: StagePossible},
		{name: "archive history", event: timelineEvent{Source: "archive"}, want: StageConfirmed},
		{name: "archive preview", event: timelineEvent{Source: "archive", Preview: true}, want: StagePossible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := timelineStage(test.event, test.window, now); got != test.want {
				t.Fatalf("timelineStage()=%s, want %s", got, test.want)
			}
		})
	}
}

func TestParseTimelineFiltersRetainsUnknownAndSortsAscending(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	body := []byte(`{
		"updated_at":"2026-08-13T12:00:01Z",
		"rhythm":{"ignored":true},
		"events":[
			{"id":"new-rejected","date":"2026-08-13","type":"reset","group":"reset","summary":"not observed","url":"https://x.com/a/status/new-rejected","announced_at":"2026-08-13T11:00:00Z","scope":"global","source":"live","announcement_state":"announced","observation_result":"unchanged","reset_verification_status":"confirmed"},
			{"id":"old-archive","date":"2026-08-13","type":"reset","group":"reset","summary":"archive","url":"https://x.com/a/status/old-archive","announced_at":"2026-08-13T09:00:00Z","scope":"","source":"archive","preview":false},
			{"id":"middle-window","date":"2026-08-13","type":"reset","group":"reset","summary":"incoming","url":"https://x.com/a/status/middle-window","announced_at":"2026-08-13T10:00:00Z","scope":"global","source":"live","preview":true,"announcement_state":"hinted","reset_verification_status":"pending","official_window":{"label":"today","start_at":"2026-08-13T09:30:00Z","end_at":"2026-08-13T12:30:00Z"}},
			{"id":"too-old","type":"reset","group":"reset","announced_at":"2026-08-12T07:00:00Z","scope":"global","source":"archive"},
			{"id":"future","type":"reset","group":"reset","announced_at":"2026-08-13T12:06:00Z","scope":"global","source":"archive"},
			{"id":"credits","type":"credits","group":"credits","announced_at":"2026-08-13T11:30:00Z","scope":"global"},
			{"id":"local","type":"reset","group":"reset","announced_at":"2026-08-13T11:30:00Z","scope":"team"},
			{"id":"wrong-group","type":"reset","group":"boost","announced_at":"2026-08-13T11:30:00Z","scope":"global"}
		]
	}`)
	parsed, err := parseTimeline(body, now, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.updatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("updatedAt=%s", parsed.updatedAt)
	}
	if len(parsed.signals) != 3 {
		t.Fatalf("signals=%#v", parsed.signals)
	}
	wantIDs := []string{"x:old-archive", "x:middle-window", "x:new-rejected"}
	wantStages := []Stage{StageConfirmed, StageImminent, StageUnknown}
	for index := range wantIDs {
		if parsed.signals[index].ID != wantIDs[index] || parsed.signals[index].Stage != wantStages[index] {
			t.Fatalf("signal[%d]=%#v, want id=%s stage=%s", index, parsed.signals[index], wantIDs[index], wantStages[index])
		}
	}
	if parsed.signals[2].ObservationResult != "unchanged" || parsed.signals[2].VerificationStatus != "confirmed" {
		t.Fatalf("status metadata not retained: %#v", parsed.signals[2])
	}
}

func TestParseTimelineUsesAnnouncementTimeAndStableIDs(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"events":[
		{"id":"","type":"reset","group":"reset","summary":"url id","url":"https://x.com/person/status/12345?x=1","announced_at":"2026-08-13T10:00:00Z","effective_at":"2026-08-13T11:00:00Z","scope":"global","source":"archive"},
		{"id":"","type":"reset","group":"reset","summary":"no x id","url":"https://example.test/event/a","announced_at":"2026-08-13T11:30:00Z","scope":"global","source":"live"}
	]}`)
	first, err := parseTimeline(body, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseTimeline(body, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.signals) != 2 || first.signals[0].ID != "x:12345" {
		t.Fatalf("signals=%#v", first.signals)
	}
	if !first.signals[0].CreatedAt.Equal(time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("announcement time not used: %s", first.signals[0].CreatedAt)
	}
	if !strings.HasPrefix(first.signals[1].ID, "timeline:") || first.signals[1].ID != second.signals[1].ID {
		t.Fatalf("fallback ID is not stable: %q / %q", first.signals[1].ID, second.signals[1].ID)
	}
}

func TestTimelineEventTimeFallsBackWhenAnnouncementMissing(t *testing.T) {
	event := timelineEvent{EffectiveAt: "2026-08-13T11:00:00Z", Date: "2026-08-12"}
	if got := timelineEventTime(event); !got.Equal(time.Date(2026, 8, 13, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("effective fallback=%s", got)
	}
	event.EffectiveAt = ""
	if got := timelineEventTime(event); !got.Equal(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("date fallback=%s", got)
	}
}

func TestParseTimelineRejectsMalformedAndTrailingJSON(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "malformed", body: `{`, want: "解析 timeline JSON"},
		{name: "missing events", body: `{}`, want: "缺少 events"},
		{name: "second value", body: `{"events":[]} {"events":[]}`, want: "尾随内容"},
		{name: "garbage", body: `{"events":[]} trailing`, want: "尾随内容"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseTimeline([]byte(test.body), now, time.Hour)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
	if _, err := parseTimeline([]byte("{\"events\":[]} \r\n\t"), now, time.Hour); err != nil {
		t.Fatalf("JSON with whitespace suffix rejected: %v", err)
	}
}

func TestParseOfficialWindowRejectsInvalidRange(t *testing.T) {
	if got := parseOfficialWindow(&timelineOfficialWindow{StartAt: "bad", EndAt: "2026-08-13T12:00:00Z"}); got != nil {
		t.Fatalf("invalid time returned %#v", got)
	}
	if got := parseOfficialWindow(&timelineOfficialWindow{StartAt: "2026-08-13T13:00:00Z", EndAt: "2026-08-13T12:00:00Z"}); got != nil {
		t.Fatalf("reversed range returned %#v", got)
	}
}
