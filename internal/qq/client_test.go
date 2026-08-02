package qq

import (
	"encoding/json"
	"testing"
)

func TestFlexIntAcceptsStringAndNumber(t *testing.T) {
	for _, input := range []string{`{"expires_in":"7200"}`, `{"expires_in":7200}`} {
		var value struct {
			ExpiresIn flexInt `json:"expires_in"`
		}
		if err := json.Unmarshal([]byte(input), &value); err != nil {
			t.Fatalf("input %s: %v", input, err)
		}
		if value.ExpiresIn != 7200 {
			t.Fatalf("input %s: got %d", input, value.ExpiresIn)
		}
	}
}

func TestMessageCreateEventCompatibility(t *testing.T) {
	for _, eventType := range []string{"C2C_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", "GROUP_AT_MESSAGE_CREATE"} {
		if !isMessageCreateEvent(eventType) {
			t.Fatalf("expected %s to be accepted", eventType)
		}
	}
	if isMessageCreateEvent("READY") {
		t.Fatal("READY must not be treated as a message event")
	}
}
