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

func TestUploadPreparationAcceptsStringBlockSize(t *testing.T) {
	input := []byte(`{"upload_id":"upload-1","block_size":"1048576","parts":[{"index":0,"presigned_url":"https://example.com/upload","block_size":"524288"}]}`)
	var value struct {
		UploadID  string  `json:"upload_id"`
		BlockSize flexInt `json:"block_size"`
		Parts     []struct {
			Index     int     `json:"index"`
			BlockSize flexInt `json:"block_size"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(input, &value); err != nil {
		t.Fatalf("unmarshal upload_prepare response: %v", err)
	}
	if value.UploadID != "upload-1" || value.BlockSize != 1048576 || len(value.Parts) != 1 || value.Parts[0].BlockSize != 524288 {
		t.Fatalf("unexpected upload_prepare response: %#v", value)
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
