package resetradar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScannerReusesXClientUntilProxyChanges(t *testing.T) {
	scanner := NewScanner(time.Second)
	t.Cleanup(scanner.Close)

	directClient, err := scanner.xClientForProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	reusedDirectClient, err := scanner.xClientForProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if reusedDirectClient != directClient {
		t.Fatal("same proxy configuration did not reuse X client")
	}

	proxyOne, err := url.Parse("http://alice:secret@127.0.0.1:3128")
	if err != nil {
		t.Fatal(err)
	}
	proxiedClient, err := scanner.xClientForProxy(proxyOne)
	if err != nil {
		t.Fatal(err)
	}
	if proxiedClient == directClient {
		t.Fatal("proxy change did not replace X client")
	}
	reusedProxiedClient, err := scanner.xClientForProxy(proxyOne)
	if err != nil {
		t.Fatal(err)
	}
	if reusedProxiedClient != proxiedClient {
		t.Fatal("same authenticated proxy did not reuse X client")
	}

	proxyTwo, err := url.Parse("socks5://bob:other@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	secondProxiedClient, err := scanner.xClientForProxy(proxyTwo)
	if err != nil {
		t.Fatal(err)
	}
	if secondProxiedClient == proxiedClient {
		t.Fatal("different proxy did not replace X client")
	}
}

func TestScannerProxyClientCacheConcurrentAccess(t *testing.T) {
	scanner := NewScanner(time.Second)
	t.Cleanup(scanner.Close)
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
				client, err := scanner.xClientForProxy(proxies[(offset+index)%len(proxies)])
				if err != nil {
					t.Errorf("xClientForProxy: %v", err)
					return
				}
				if client == nil {
					t.Error("xClientForProxy returned nil client")
					return
				}
			}
		}(worker)
	}
	workers.Wait()
}

func TestScannerCloseClosesIdlePoolsAndRejectsWork(t *testing.T) {
	var opened atomic.Int32
	var closed atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			opened.Add(1)
		case http.StateClosed:
			closed.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	scanner := NewScanner(time.Second)
	xClient, err := scanner.xClientForProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []*http.Client{scanner.directClient, xClient} {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := opened.Load(); got != 2 {
		t.Fatalf("opened connections=%d, want 2 independent pools", got)
	}

	scanner.Close()
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closed.Load(); got != 2 {
		t.Fatalf("closed connections=%d, want 2", got)
	}
	if _, err := scanner.Fetch(context.Background(), ""); !errors.Is(err, errScannerClosed) {
		t.Fatalf("Fetch after Close error=%v, want %v", err, errScannerClosed)
	}
	if _, err := scanner.xClientForProxy(nil); !errors.Is(err, errScannerClosed) {
		t.Fatalf("xClientForProxy after Close error=%v, want %v", err, errScannerClosed)
	}
	// Close is idempotent and must not block after all pools are gone.
	scanner.Close()
}

func TestScannerCloseCancelsAndWaitsForActiveFetch(t *testing.T) {
	scanner := NewScanner(time.Second)
	ctx, finish, err := scanner.beginFetch(context.Background())
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
		t.Fatal("Close did not cancel active fetch context")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before active fetch finished")
	default:
	}
	finish()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after active fetch finished")
	}
}

func TestFetchBodyDrainsOnlySmallPrefixOnHTTPError(t *testing.T) {
	body := &trackingReadCloser{Reader: bytes.NewReader(make([]byte, errorBodyDrainLimit*2))}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       body,
			Header:     make(http.Header),
		}, nil
	})}

	_, err := fetchBody(context.Background(), client, "http://example.test", 1<<20, false)
	if err == nil {
		t.Fatal("fetchBody expected HTTP status error")
	}
	if body.read != errorBodyDrainLimit {
		t.Fatalf("drained bytes=%d, want %d", body.read, errorBodyDrainLimit)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestResetRoundTimeout(t *testing.T) {
	tests := []struct {
		perSource time.Duration
		want      time.Duration
	}{
		{perSource: 0, want: 30 * time.Second},
		{perSource: 5 * time.Second, want: 30 * time.Second},
		{perSource: 20 * time.Second, want: 100 * time.Second},
	}
	for _, test := range tests {
		if got := resetRoundTimeout(test.perSource); got != test.want {
			t.Errorf("resetRoundTimeout(%s)=%s, want %s", test.perSource, got, test.want)
		}
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
