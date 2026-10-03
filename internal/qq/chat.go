package qq

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var leadingBotMention = regexp.MustCompile(`^<@!?([^>]+)>\s*`)

// ChatContent distinguishes the at-only event from the all-message group
// event. Official GROUP_MESSAGE_CREATE can strip the bot prefix and exclude
// the bot from mentions; without explicit addressing evidence, only commands
// are accepted. Other users' mentions are not evidence of addressing our bot.
// QQAppID is the bot identity according to the official AppID contract.
func ChatContent(event MessageEvent, appID string) (string, bool) {
	if event.Message.Author.Bot {
		return "", false
	}
	content := strings.TrimSpace(event.Message.Content)
	at := event.EventType == "GROUP_AT_MESSAGE_CREATE"
	for _, m := range event.Message.Mentions {
		if m.Bot && appID != "" && (m.ID == appID || m.UserOpenID == appID || m.MemberOpenID == appID) {
			at = true
		}
	}
	if match := leadingBotMention.FindStringSubmatch(content); len(match) > 0 {
		if at || (appID != "" && match[1] == appID) {
			at = true
			content = strings.TrimSpace(content[len(match[0]):])
		}
	}
	if strings.HasPrefix(content, "/") {
		return content, true
	}
	if event.Message.GroupOpenID != "" && at {
		return content, true
	}
	return content, false
}

// ReplyOrigin uses the earliest observed time, so inbox replay cannot extend
// the provider's passive-reply window. Missing timestamps use local receipt.
func ReplyOrigin(event MessageEvent, now time.Time) time.Time {
	origin := event.ReceivedAt
	if origin.IsZero() || origin.After(now) {
		origin = now
	}
	if len(event.Message.Timestamp) > 0 {
		var raw string
		if json.Unmarshal(event.Message.Timestamp, &raw) != nil {
			raw = string(event.Message.Timestamp)
		}
		stamp, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			if unix, e := strconv.ParseInt(raw, 10, 64); e == nil && unix > 0 {
				if unix > 1e12 {
					stamp = time.UnixMilli(unix)
				} else {
					stamp = time.Unix(unix, 0)
				}
			}
		}
		if !stamp.IsZero() && stamp.Before(origin) {
			origin = stamp
		}
	}
	return origin
}
