package resetradar

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	xTimelinePattern = regexp.MustCompile(`TimelineTimelineEntry:tweet-(\d+)`)
	xDetailsPattern  = regexp.MustCompile(`"client:(VHdlZXQ6[A-Za-z0-9+/]+=*):details":\$R\[\d+\]=\{`)
	xTextPattern     = regexp.MustCompile(`full_text:"((?:\\.|[^"\\])*)"`)
	xCreatedPattern  = regexp.MustCompile(`created_at_ms:(\d+)`)

	aggregatorStatePattern  = regexp.MustCompile(`status:"([^"]+)"\s*,updatedAt:"[^"]+"\s*,forecastStatus:"[^"]+"\s*,score:(\d+)`)
	aggregatorSignalPattern = regexp.MustCompile(`id:"(\d+)"\s*,kind:"([^"]+)"\s*,score:(\d+)\s*,text:"((?:\\.|[^"\\])*)"\s*,createdAt:"([^"]+)"\s*,sourceUrl:"((?:\\.|[^"\\])*)"`)
)

func parseXPublicPage(body []byte, username string, now time.Time, maxAge ...time.Duration) ([]Signal, error) {
	age := signalMaxAge(maxAge)
	// A profile timeline is small in normal operation. The cap prevents an
	// unexpected page from turning regex result slices into a large allocation.
	timelineMatches := xTimelinePattern.FindAllSubmatch(body, 256)
	if len(timelineMatches) == 0 {
		return nil, errors.New("公开页面未包含时间线数据")
	}
	timelineIDs := make(map[string]struct{}, len(timelineMatches))
	for _, match := range timelineMatches {
		timelineIDs[string(match[1])] = struct{}{}
	}

	details := xDetailsPattern.FindAllSubmatchIndex(body, 256)
	parsed := 0
	result := make([]Signal, 0, len(details))
	for index, match := range details {
		encodedKey := string(body[match[2]:match[3]])
		decoded, err := base64.StdEncoding.DecodeString(encodedKey)
		if err != nil || !strings.HasPrefix(string(decoded), "Tweet:") {
			continue
		}
		id := strings.TrimPrefix(string(decoded), "Tweet:")
		if _, exists := timelineIDs[id]; !exists {
			continue
		}
		objectStart := match[1]
		objectEnd := -1
		if relativeEnd := bytes.Index(body[objectStart:], []byte(`},"client:`)); relativeEnd >= 0 {
			objectEnd = objectStart + relativeEnd + 1
		} else if index+1 < len(details) {
			objectEnd = details[index+1][0]
		} else {
			objectEnd = len(body)
		}
		if objectEnd-objectStart > 2<<20 {
			objectEnd = objectStart + 2<<20
		}
		object := body[objectStart:objectEnd]
		textMatch := xTextPattern.FindSubmatch(object)
		createdMatch := xCreatedPattern.FindSubmatch(object)
		if len(textMatch) < 2 || len(createdMatch) < 2 {
			continue
		}
		createdMS, err := strconv.ParseInt(string(createdMatch[1]), 10, 64)
		if err != nil {
			continue
		}
		parsed++
		createdAt := time.UnixMilli(createdMS).UTC()
		if !isRecent(createdAt, now, age) {
			continue
		}
		text := decodeJSONString(textMatch[1])
		stage := Classify(text, "x-public", 100)
		if stage == StageUnknown {
			continue
		}
		result = append(result, Signal{
			ID:        "x:" + id,
			Source:    "X @" + username,
			Text:      text,
			URL:       "https://x.com/" + username + "/status/" + id,
			CreatedAt: createdAt,
			Stage:     stage,
		})
	}
	if parsed == 0 {
		return nil, errors.New("X 页面结构已变化，未找到帖子正文")
	}
	return result, nil
}

type aggregatorResult struct {
	status  string
	score   int
	signals []Signal
}

func parseAggregator(body []byte, now time.Time, maxAge ...time.Duration) (aggregatorResult, error) {
	age := signalMaxAge(maxAge)
	state := aggregatorStatePattern.FindSubmatch(body)
	if len(state) < 3 {
		return aggregatorResult{}, errors.New("聚合页未包含实时状态字段")
	}
	result := aggregatorResult{status: string(state[1])}
	result.score, _ = strconv.Atoi(string(state[2]))

	signalArea := body
	marker := []byte("activeSignals:")
	if start := bytes.Index(body, marker); start >= 0 {
		endOffset := bytes.Index(body[start:], []byte("],signals:"))
		if endOffset >= 0 {
			signalArea = body[start : start+endOffset+1]
		} else {
			end := start + 512<<10
			if end > len(body) {
				end = len(body)
			}
			signalArea = body[start:end]
		}
	}
	for _, match := range aggregatorSignalPattern.FindAllSubmatch(signalArea, -1) {
		createdAt, err := time.Parse(time.RFC3339Nano, string(match[5]))
		if err != nil || !isRecent(createdAt, now, age) {
			continue
		}
		score, _ := strconv.Atoi(string(match[3]))
		text := decodeJSONString(match[4])
		stage := Classify(text, string(match[2]), score)
		if stage == StageUnknown {
			continue
		}
		result.signals = append(result.signals, Signal{
			ID:        "x:" + string(match[1]),
			Source:    "codexreset.org",
			Text:      text,
			URL:       decodeJSONString(match[6]),
			CreatedAt: createdAt.UTC(),
			Stage:     stage,
		})
	}
	return result, nil
}

type statusResponse struct {
	Incidents []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Status          string `json:"status"`
		CreatedAt       string `json:"created_at"`
		UpdatedAt       string `json:"updated_at"`
		IncidentUpdates []struct {
			ID        string `json:"id"`
			Body      string `json:"body"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"incident_updates"`
	} `json:"incidents"`
}

func parseStatusJSON(body []byte, now time.Time, maxAge ...time.Duration) ([]Signal, error) {
	age := signalMaxAge(maxAge)
	var payload statusResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析状态 JSON 失败: %w", err)
	}
	if payload.Incidents == nil {
		return nil, errors.New("状态 API 缺少 incidents 字段")
	}
	result := make([]Signal, 0, 4)
	for _, incident := range payload.Incidents {
		if len(incident.IncidentUpdates) == 0 {
			createdAt := firstParsedTime(incident.CreatedAt, incident.UpdatedAt)
			result = appendStatusSignal(result, now, age, incident.ID, incident.Name, incident.Status, incident.Status, "", createdAt)
			continue
		}
		for _, update := range incident.IncidentUpdates {
			createdAt := firstParsedTime(update.CreatedAt, update.UpdatedAt, incident.CreatedAt, incident.UpdatedAt)
			status := update.Status
			if status == "" {
				status = incident.Status
			}
			result = appendStatusSignal(result, now, age, incident.ID, incident.Name, status, update.ID, update.Body, createdAt)
		}
	}
	return result, nil
}

func appendStatusSignal(dst []Signal, now time.Time, maxAge time.Duration, incidentID, incidentName, status, updateID, body string, createdAt time.Time) []Signal {
	if !isRecent(createdAt, now, maxAge) {
		return dst
	}
	combined := strings.TrimSpace(incidentName + " " + body)
	if !productPattern.MatchString(combined) || !quotaPattern.MatchString(combined) {
		return dst
	}
	stage := Classify(combined, "official-status", 100)
	if stage == StageUnknown && !strings.EqualFold(status, "resolved") {
		stage = StagePossible
	}
	if stage == StageUnknown {
		return dst
	}
	if updateID == "" {
		updateID = status
	}
	text := incidentName
	if strings.TrimSpace(body) != "" {
		text += "\n\n" + strings.TrimSpace(body)
	}
	return append(dst, Signal{
		ID:        "status:" + incidentID + ":" + updateID,
		Source:    "OpenAI status",
		Text:      text,
		URL:       "https://status.openai.com/incidents/" + incidentID,
		CreatedAt: createdAt.UTC(),
		Stage:     stage,
	})
}

func firstParsedTime(values ...string) time.Time {
	for _, value := range values {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func decodeJSONString(escaped []byte) string {
	quoted := make([]byte, 0, len(escaped)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, escaped...)
	quoted = append(quoted, '"')
	var result string
	if json.Unmarshal(quoted, &result) == nil {
		return strings.TrimSpace(result)
	}
	return strings.TrimSpace(string(escaped))
}

func isRecent(createdAt, now time.Time, maxAge time.Duration) bool {
	if createdAt.IsZero() {
		return false
	}
	createdAt = createdAt.UTC()
	now = now.UTC()
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	return !createdAt.Before(now.Add(-maxAge)) && !createdAt.After(now.Add(5*time.Minute))
}

func signalMaxAge(values []time.Duration) time.Duration {
	if len(values) > 0 && values[0] > 0 {
		return values[0]
	}
	return 24 * time.Hour
}
