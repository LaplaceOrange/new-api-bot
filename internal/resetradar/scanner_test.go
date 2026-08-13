package resetradar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScannerFetchUsesSingleTimelineRequest(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var accept string
	var group string
	var from string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		accept = request.Header.Get("Accept")
		group = request.URL.Query().Get("group")
		from = request.URL.Query().Get("from")
		_, _ = io.WriteString(response, `{"updated_at":"2026-08-13T12:00:00Z","events":[{"id":"10","type":"reset","group":"reset","summary":"observed","announced_at":"2026-08-13T11:00:00Z","scope":"global","source":"live","observation_result":"reset_observed"}]}`)
	}))
	defer server.Close()

	scanner := NewScanner(time.Second, 24*time.Hour)
	scanner.endpoint = server.URL
	scanner.now = func() time.Time { return now }
	t.Cleanup(scanner.Close)
	snapshot, err := scanner.Fetch(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || accept != "application/json" || group != "reset" || from != "2026-08-12" {
		t.Fatalf("calls=%d accept=%q group=%q from=%q", calls.Load(), accept, group, from)
	}
	if len(snapshot.SourceErrors) != 0 || len(snapshot.Signals) != 1 || snapshot.Signals[0].ID != "x:10" || snapshot.Signals[0].Stage != StageConfirmed {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestScannerFetchReportsHTTPAndParseErrors(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		want string
	}{
		{name: "http", code: http.StatusBadGateway, body: "upstream detail", want: "HTTP 502"},
		{name: "parse", code: http.StatusOK, body: `{"events":[]} trailing`, want: "尾随内容"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.code)
				_, _ = io.WriteString(response, test.body)
			}))
			defer server.Close()
			scanner := NewScanner(time.Second)
			scanner.endpoint = server.URL
			defer scanner.Close()
			snapshot, err := scanner.Fetch(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.SourceErrors) != 1 || !strings.Contains(snapshot.SourceErrors[0].Error(), test.want) {
				t.Fatalf("source errors=%#v", snapshot.SourceErrors)
			}
		})
	}
}

func TestScannerLatestReturnsNewestWithoutMaxAge(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var group string
	var from string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		group = request.URL.Query().Get("group")
		from = request.URL.Query().Get("from")
		_, _ = io.WriteString(response, `{"events":[
			{"id":"2","type":"reset","group":"reset","summary":"new","announced_at":"2026-07-01T11:00:00Z","scope":"global","source":"archive"},
			{"id":"1","type":"reset","group":"reset","summary":"old","announced_at":"2026-06-01T11:00:00Z","scope":"global","source":"archive"}
		]}`)
	}))
	defer server.Close()
	scanner := NewScanner(time.Second, time.Hour)
	scanner.endpoint = server.URL
	scanner.now = func() time.Time { return now }
	defer scanner.Close()
	latest, err := scanner.Latest(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != "x:2" || latest.Text != "new" {
		t.Fatalf("latest=%#v", latest)
	}
	if group != "reset" || from != "" {
		t.Fatalf("latest query group=%q from=%q", group, from)
	}
}

func TestScannerProxyRoutesTimelineAPI(t *testing.T) {
	var requests atomic.Int32
	var requestedURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		requestedURL = request.URL.String()
		_, _ = io.WriteString(response, `{"events":[]}`)
	}))
	defer proxy.Close()

	scanner := NewScanner(time.Second)
	scanner.endpoint = "http://timeline.invalid/api/timeline"
	scanner.now = func() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) }
	defer scanner.Close()
	snapshot, err := scanner.Fetch(context.Background(), proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.SourceErrors) != 0 || requests.Load() != 1 || requestedURL != scanner.endpoint+"?from=2026-08-12&group=reset" {
		t.Fatalf("requests=%d URL=%q snapshot=%#v", requests.Load(), requestedURL, snapshot)
	}
}

func TestTimelineRequestURLPreservesExistingQuery(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	raw, err := timelineRequestURL("https://example.test/api/timeline?preview=true&group=all", now, 36*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("preview") != "true" || query.Get("group") != "reset" || query.Get("from") != "2026-08-12" {
		t.Fatalf("query=%v", query)
	}
}

func TestScannerClientCacheConcurrentAccess(t *testing.T) {
	scanner := NewScanner(time.Second)
	defer scanner.Close()
	proxies := []*url.URL{
		nil,
		mustParseURL(t, "http://127.0.0.1:3128"),
		mustParseURL(t, "socks5://user:pass@127.0.0.1:1080"),
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(offset int) {
			defer workers.Done()
			for index := 0; index < 100; index++ {
				client, err := scanner.clientForProxy(proxies[(offset+index)%len(proxies)])
				if err != nil {
					t.Errorf("clientForProxy: %v", err)
					return
				}
				if client == nil {
					t.Error("clientForProxy returned nil")
					return
				}
			}
		}(worker)
	}
	workers.Wait()
}

func TestScannerLatestDoesNotWaitForFetch(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-request.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(response, `{"events":[{"id":"10","type":"reset","group":"reset","summary":"latest","announced_at":"2026-08-13T11:00:00Z","scope":"global","source":"archive"}]}`)
	}))
	defer server.Close()
	scanner := NewScanner(time.Second)
	scanner.endpoint = server.URL
	scanner.now = func() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) }
	defer scanner.Close()

	fetchDone := make(chan error, 1)
	go func() {
		_, err := scanner.Fetch(context.Background(), "")
		fetchDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("Fetch did not start")
	}
	latest, err := scanner.Latest(context.Background(), "")
	if err != nil || latest.ID != "x:10" {
		t.Fatalf("Latest=%#v err=%v", latest, err)
	}
	close(releaseFirst)
	select {
	case err := <-fetchDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fetch did not finish")
	}
}

func TestScannerCloseCancelsAndWaitsForActiveRequest(t *testing.T) {
	scanner := NewScanner(time.Second)
	ctx, finish, err := scanner.beginRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		scanner.Close()
		close(closed)
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel request")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before request finished")
	default:
	}
	finish()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for request")
	}
	if _, err := scanner.Fetch(context.Background(), ""); !errors.Is(err, errScannerClosed) {
		t.Fatalf("Fetch after Close error=%v", err)
	}
	if _, err := scanner.Latest(context.Background(), ""); !errors.Is(err, errScannerClosed) {
		t.Fatalf("Latest after Close error=%v", err)
	}
	scanner.Close()
}

func TestFetchBodyEnforcesLimitAndDrainsSmallErrorPrefix(t *testing.T) {
	oversize := &trackingReadCloser{Reader: bytes.NewReader(make([]byte, maxTimelineBody+1))}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: oversize, Header: make(http.Header), ContentLength: -1}, nil
	})}
	_, err := fetchBody(context.Background(), client, "http://example.test", maxTimelineBody)
	if err == nil || !strings.Contains(err.Error(), "512 KiB") || !oversize.closed {
		t.Fatalf("oversize error=%v closed=%v", err, oversize.closed)
	}

	errorBody := &trackingReadCloser{Reader: bytes.NewReader(make([]byte, errorBodyDrainLimit*2))}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: errorBody, Header: make(http.Header)}, nil
	})
	_, err = fetchBody(context.Background(), client, "http://example.test", maxTimelineBody)
	if err == nil || errorBody.read != errorBodyDrainLimit || !errorBody.closed {
		t.Fatalf("HTTP error=%v read=%d closed=%v", err, errorBody.read, errorBody.closed)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type trackingReadCloser struct {
	io.Reader
	read   int64
	closed bool
}

func (body *trackingReadCloser) Read(buffer []byte) (int, error) {
	read, err := body.Reader.Read(buffer)
	body.read += int64(read)
	return read, err
}

func (body *trackingReadCloser) Close() error {
	body.closed = true
	return nil
}
