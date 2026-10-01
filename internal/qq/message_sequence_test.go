package qq

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type messageSequenceTransport struct {
	bodies []map[string]any
}

func (transport *messageSequenceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		return nil, err
	}
	transport.bodies = append(transport.bodies, body)
	return &http.Response{StatusCode: 200, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"id":"sent","timestamp":"0"}`)), Request: request}, nil
}

func TestGroupTextReplySequence(t *testing.T) {
	transport := &messageSequenceTransport{}
	client := &Client{httpClient: &http.Client{Transport: transport},
		token: "test-token", expiresAt: time.Now().Add(time.Hour)}
	ctx := context.Background()
	if _, err := client.SendGroupText(ctx, "g", "incoming", "claim"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendGroupTextWithSequence(ctx, "g", "incoming", "summary", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendGroupTextWithSequence(ctx, "g", "", "proactive summary", 2); err != nil {
		t.Fatal(err)
	}
	if len(transport.bodies) != 3 {
		t.Fatal(transport.bodies)
	}
	for i, want := range []float64{1, 2} {
		if body := transport.bodies[i]; body["msg_seq"] != want || body["msg_id"] != "incoming" {
			t.Fatalf("body = %v, want sequence %v", body, want)
		}
	}
	if body := transport.bodies[2]; body["msg_seq"] != nil || body["msg_id"] != nil {
		t.Fatalf("proactive body = %v", body)
	}
	if _, err := client.SendGroupTextWithSequence(ctx, "g", "incoming", "invalid", 0); err == nil || len(transport.bodies) != 3 {
		t.Fatal("invalid sequence was sent")
	}
}
