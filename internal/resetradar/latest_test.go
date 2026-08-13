package resetradar

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLatestXPostUsesCreationTimeInsteadOfTimelineOrder(t *testing.T) {
	body := xTimelineFixture(
		xTimelinePost{
			id:        "100",
			text:      "Codex usage limits have been reset for paid users.",
			createdAt: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		},
		xTimelinePost{
			id:        "200",
			text:      "A newer ordinary post with a quoted word: \"hello\".\nSecond line.",
			createdAt: time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC),
		},
	)

	post, err := parseLatestXPost(body, tiboUsername)
	if err != nil {
		t.Fatal(err)
	}
	if post.ID != "x:200" {
		t.Fatalf("latest ID=%q, want x:200", post.ID)
	}
	if post.Stage != StageUnknown {
		t.Fatalf("stage=%s, want unknown", post.Stage)
	}
	if post.Text != "A newer ordinary post with a quoted word: \"hello\".\nSecond line." {
		t.Fatalf("text=%q", post.Text)
	}
	if post.URL != "https://x.com/thsottiaux/status/200" {
		t.Fatalf("URL=%q", post.URL)
	}
	if !post.CreatedAt.Equal(time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Fatalf("CreatedAt=%s", post.CreatedAt)
	}
}

func TestParseLatestXPostDoesNotApplySignalMaxAge(t *testing.T) {
	old := time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)
	post, err := parseLatestXPost(xTimelineFixture(xTimelinePost{
		id:        "300",
		text:      "An old ordinary post.",
		createdAt: old,
	}), tiboUsername)
	if err != nil {
		t.Fatal(err)
	}
	if !post.CreatedAt.Equal(old) || post.Stage != StageUnknown {
		t.Fatalf("unexpected old post: %#v", post)
	}

	filtered, err := parseXPublicPage(
		xTimelineFixture(xTimelinePost{id: "300", text: "Codex usage limit reset is coming soon.", createdAt: old}),
		tiboUsername,
		time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("parseXPublicPage returned %d old signals, want 0", len(filtered))
	}
}

func TestParseLatestXPostClassifiesEveryStage(t *testing.T) {
	tests := []struct {
		name string
		text string
		want Stage
	}{
		{name: "unknown", text: "A regular product update.", want: StageUnknown},
		{name: "possible", text: "Thinking about another Codex usage limit reset.", want: StagePossible},
		{name: "imminent", text: "A Codex usage limit reset is coming tomorrow.", want: StageImminent},
		{name: "confirmed", text: "We have reset Codex usage limits for paid users.", want: StageConfirmed},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			post, err := parseLatestXPost(xTimelineFixture(xTimelinePost{
				id:        strconv.Itoa(500 + index),
				text:      test.text,
				createdAt: time.Date(2026, 8, 13, 0, 0, index, 0, time.UTC),
			}), tiboUsername)
			if err != nil {
				t.Fatal(err)
			}
			if post.Stage != test.want {
				t.Fatalf("stage=%s, want %s", post.Stage, test.want)
			}
		})
	}
}

func TestParseLatestXPostErrors(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "no timeline", body: []byte("ordinary HTML"), want: "未包含时间线数据"},
		{name: "missing details", body: []byte("TimelineTimelineEntry:tweet-123"), want: "未找到帖子正文"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseLatestXPost(test.body, tiboUsername)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestScannerLatestTiboMakesSingleTiboRequest(t *testing.T) {
	var calls atomic.Int32
	var requestedURL string
	body := xTimelineFixture(xTimelinePost{
		id:        "400",
		text:      "Thinking about another Codex usage limit reset.",
		createdAt: time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC),
	})
	scanner := NewScanner(time.Second)
	t.Cleanup(scanner.Close)
	scanner.xClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		requestedURL = request.URL.String()
		if request.UserAgent() == "" {
			t.Error("request did not use browser User-Agent")
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(string(body))),
			Header:        make(http.Header),
			ContentLength: int64(len(body)),
		}, nil
	})}

	post, err := scanner.LatestTibo(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if post.ID != "x:400" || post.Stage != StagePossible {
		t.Fatalf("unexpected post: %#v", post)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
	if requestedURL != tiboURL {
		t.Fatalf("requested URL=%q, want %q", requestedURL, tiboURL)
	}
}

func TestScannerLatestTiboDoesNotWaitForFullFetch(t *testing.T) {
	firstTiboStarted := make(chan struct{})
	releaseFirstTibo := make(chan struct{})
	var tiboCalls atomic.Int32
	body := xTimelineFixture(xTimelinePost{
		id:        "401",
		text:      "A regular latest post.",
		createdAt: time.Date(2026, 8, 13, 2, 3, 4, 0, time.UTC),
	})
	scanner := NewScanner(time.Second)
	t.Cleanup(scanner.Close)
	scanner.xClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == tiboURL && tiboCalls.Add(1) == 1 {
			close(firstTiboStarted)
			select {
			case <-releaseFirstTibo:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(string(body))),
			Header:        make(http.Header),
			ContentLength: int64(len(body)),
		}, nil
	})}
	scanner.directClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader("unavailable")),
			Header:     make(http.Header),
		}, nil
	})}

	fetchDone := make(chan error, 1)
	go func() {
		_, err := scanner.Fetch(context.Background(), "")
		fetchDone <- err
	}()
	select {
	case <-firstTiboStarted:
	case <-time.After(time.Second):
		t.Fatal("full fetch did not start its Tibo request")
	}

	latestCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	post, err := scanner.LatestTibo(latestCtx, "")
	if err != nil {
		t.Fatalf("LatestTibo was blocked by full Fetch: %v", err)
	}
	if post.ID != "x:401" || post.Stage != StageUnknown {
		t.Fatalf("unexpected latest post: %#v", post)
	}

	close(releaseFirstTibo)
	select {
	case err := <-fetchDone:
		if err != nil {
			t.Fatalf("Fetch returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full Fetch did not finish")
	}
}

func TestScannerLatestTiboPropagatesHTTPError(t *testing.T) {
	var calls atomic.Int32
	scanner := NewScanner(time.Second)
	t.Cleanup(scanner.Close)
	scanner.xClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader("upstream detail")),
			Header:     make(http.Header),
		}, nil
	})}

	_, err := scanner.LatestTibo(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error=%v, want HTTP 502", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
}

func TestScannerLatestTiboRejectsWorkAfterClose(t *testing.T) {
	scanner := NewScanner(time.Second)
	scanner.Close()
	if _, err := scanner.LatestTibo(context.Background(), ""); !errors.Is(err, errScannerClosed) {
		t.Fatalf("LatestTibo after Close error=%v, want %v", err, errScannerClosed)
	}
}

type xTimelinePost struct {
	id        string
	text      string
	createdAt time.Time
}

func xTimelineFixture(posts ...xTimelinePost) []byte {
	var body strings.Builder
	for _, post := range posts {
		fmt.Fprintf(&body, "TimelineTimelineEntry:tweet-%s,", post.id)
	}
	for index, post := range posts {
		key := base64.StdEncoding.EncodeToString([]byte("Tweet:" + post.id))
		fmt.Fprintf(
			&body,
			`"client:%s:details":$R[%d]={full_text:%s,created_at_ms:%d},`,
			key,
			index+1,
			strconv.Quote(post.text),
			post.createdAt.UnixMilli(),
		)
	}
	body.WriteString(`"client:next":$R[999]={value:1}`)
	return []byte(body.String())
}
