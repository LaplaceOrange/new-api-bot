package resetradar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

type timelineResponse struct {
	UpdatedAt string          `json:"updated_at"`
	Events    []timelineEvent `json:"events"`
}

type timelineEvent struct {
	ID                      string                  `json:"id"`
	Date                    string                  `json:"date"`
	Type                    string                  `json:"type"`
	Group                   string                  `json:"group"`
	Summary                 string                  `json:"summary"`
	URL                     string                  `json:"url"`
	AnnouncedAt             string                  `json:"announced_at"`
	EffectiveAt             string                  `json:"effective_at"`
	OfficialWindow          *timelineOfficialWindow `json:"official_window"`
	Preview                 bool                    `json:"preview"`
	Scope                   string                  `json:"scope"`
	Source                  string                  `json:"source"`
	SourceLabel             string                  `json:"source_label"`
	AnnouncementState       string                  `json:"announcement_state"`
	ObservationResult       string                  `json:"observation_result"`
	ResetVerificationStatus string                  `json:"reset_verification_status"`
}

type timelineOfficialWindow struct {
	Label    string `json:"label"`
	StartAt  string `json:"start_at"`
	EndAt    string `json:"end_at"`
	TimeZone string `json:"time_zone"`
}

type parsedTimeline struct {
	updatedAt time.Time
	signals   []Signal
}

func parseTimeline(body []byte, now time.Time, maxAge time.Duration) (parsedTimeline, error) {
	var payload timelineResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return parsedTimeline{}, fmt.Errorf("解析 timeline JSON 失败: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("存在第二个 JSON 值")
		}
		return parsedTimeline{}, fmt.Errorf("timeline JSON 含有尾随内容: %w", err)
	}
	if payload.Events == nil {
		return parsedTimeline{}, errors.New("timeline API 缺少 events 字段")
	}

	now = now.UTC()
	result := parsedTimeline{updatedAt: parseRFC3339(payload.UpdatedAt)}
	byID := make(map[string]Signal, len(payload.Events))
	for _, event := range payload.Events {
		signal, ok := signalFromTimelineEvent(event, now, maxAge)
		if !ok {
			continue
		}
		mergeSignals(byID, []Signal{signal})
	}
	result.signals = sortedSignals(byID)
	return result, nil
}

func signalFromTimelineEvent(event timelineEvent, now time.Time, maxAge time.Duration) (Signal, bool) {
	group := normalizedEnum(event.Group)
	eventType := normalizedEnum(event.Type)
	scope := normalizedEnum(event.Scope)
	if group != "reset" || eventType != "reset" || scope != "" && scope != "global" {
		return Signal{}, false
	}

	eventID := timelineEventID(event.ID, event.URL)
	signalID := ""
	if eventID != "" {
		signalID = "x:" + eventID
	} else {
		signalID = fallbackTimelineID(event)
	}
	createdAt := timelineEventTime(event)
	if createdAt.IsZero() || createdAt.After(now.Add(5*time.Minute)) {
		return Signal{}, false
	}
	if maxAge > 0 && createdAt.Before(now.Add(-maxAge)) {
		return Signal{}, false
	}

	window := parseOfficialWindow(event.OfficialWindow)
	return Signal{
		ID:                 signalID,
		Source:             "codex-reset.com",
		Text:               strings.TrimSpace(event.Summary),
		URL:                strings.TrimSpace(event.URL),
		CreatedAt:          createdAt,
		Stage:              timelineStage(event, window, now),
		EventID:            eventID,
		Group:              group,
		Type:               eventType,
		Scope:              scope,
		Preview:            event.Preview,
		TimelineSource:     normalizedEnum(event.Source),
		SourceLabel:        strings.TrimSpace(event.SourceLabel),
		AnnouncementState:  normalizedEnum(event.AnnouncementState),
		ObservationResult:  normalizedEnum(event.ObservationResult),
		VerificationStatus: normalizedEnum(event.ResetVerificationStatus),
		OfficialWindow:     window,
	}, true
}

func timelineStage(event timelineEvent, window *OfficialWindow, now time.Time) Stage {
	observation := normalizedEnum(event.ObservationResult)
	verification := normalizedEnum(event.ResetVerificationStatus)
	if observation == "" {
		switch verification {
		case "confirmed", "reset_observed":
			observation = "reset_observed"
		case "rejected", "unchanged":
			observation = "unchanged"
		case "unverified", "expired":
			observation = verification
		case "pending", "hinted", "none":
			observation = "pending"
		}
	}
	switch observation {
	case "confirmed", "reset_observed":
		return StageConfirmed
	case "rejected", "unchanged", "expired", "unverified":
		return StageUnknown
	}

	if normalizedEnum(event.Source) == "archive" && !event.Preview && observation == "" && verification == "" {
		return StageConfirmed
	}
	announcement := normalizedEnum(event.AnnouncementState)
	if window != nil && !now.Before(window.StartAt) && !now.After(window.EndAt) && (announcement == "announced" || announcement == "hinted") {
		return StageImminent
	}
	return StagePossible
}

func parseOfficialWindow(value *timelineOfficialWindow) *OfficialWindow {
	if value == nil {
		return nil
	}
	startAt := parseRFC3339(value.StartAt)
	endAt := parseRFC3339(value.EndAt)
	if startAt.IsZero() || endAt.IsZero() || endAt.Before(startAt) {
		return nil
	}
	return &OfficialWindow{
		Label:    strings.TrimSpace(value.Label),
		StartAt:  startAt,
		EndAt:    endAt,
		TimeZone: strings.TrimSpace(value.TimeZone),
	}
}

func timelineEventTime(event timelineEvent) time.Time {
	if value := parseRFC3339(event.AnnouncedAt); !value.IsZero() {
		return value
	}
	if value := parseRFC3339(event.EffectiveAt); !value.IsZero() {
		return value
	}
	if value, err := time.Parse("2006-01-02", strings.TrimSpace(event.Date)); err == nil {
		return value.UTC()
	}
	return time.Time{}
}

func timelineEventID(rawID, rawURL string) string {
	if id := strings.TrimSpace(rawID); id != "" {
		return id
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index+1 < len(parts); index++ {
		if strings.EqualFold(parts[index], "status") {
			return strings.TrimSpace(parts[index+1])
		}
	}
	return ""
}

func parseRFC3339(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func normalizedEnum(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// fallbackTimelineID remains available for feeds that eventually add reset
// events without an X post ID. Current API events use timelineEventID so their
// keys remain compatible with records produced by the old X scanner.
func fallbackTimelineID(event timelineEvent) string {
	value := strings.Join([]string{
		normalizedEnum(event.Source),
		strings.TrimSpace(event.URL),
		strings.TrimSpace(event.AnnouncedAt),
		strings.TrimSpace(event.Summary),
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return "timeline:" + hex.EncodeToString(sum[:16])
}
