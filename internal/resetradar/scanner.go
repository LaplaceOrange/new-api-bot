package resetradar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	timelineURL         = "https://codex-reset.com/api/timeline"
	maxTimelineBody     = int64(512 << 10)
	errorBodyDrainLimit = int64(4 << 10)
)

var errScannerClosed = errors.New("reset radar scanner is closed")

type Scanner struct {
	timeout  time.Duration
	maxAge   time.Duration
	endpoint string
	now      func() time.Time

	directTransport *http.Transport
	directClient    *http.Client
	fetchSlot       chan struct{}
	latestSlot      chan struct{}

	mu             sync.Mutex
	active         sync.WaitGroup
	closed         bool
	proxyKey       string
	proxyTransport *http.Transport
	proxyClient    *http.Client

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	closeOnce       sync.Once
}

func NewScanner(timeout time.Duration, maxAge ...time.Duration) *Scanner {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	signalMaxAge := 24 * time.Hour
	if len(maxAge) > 0 && maxAge[0] > 0 {
		signalMaxAge = maxAge[0]
	}
	transport := newTransport(timeout)
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &Scanner{
		timeout:         timeout,
		maxAge:          signalMaxAge,
		endpoint:        timelineURL,
		now:             time.Now,
		directTransport: transport,
		directClient:    &http.Client{Transport: transport, Timeout: timeout},
		fetchSlot:       make(chan struct{}, 1),
		latestSlot:      make(chan struct{}, 1),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
}

func (s *Scanner) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.lifecycleCancel()
		directTransport := s.directTransport
		proxyTransport := s.proxyTransport
		s.mu.Unlock()

		closeIdleConnections(directTransport, proxyTransport)
		s.active.Wait()
		closeIdleConnections(directTransport, proxyTransport)
	})
}

func (s *Scanner) Fetch(ctx context.Context, proxyURL string) (Snapshot, error) {
	if s == nil {
		return Snapshot{}, errors.New("reset radar scanner is nil")
	}
	parsedProxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return Snapshot{}, err
	}
	requestCtx, finish, err := s.beginRequest(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer finish()
	if err := acquireSlot(requestCtx, s.fetchSlot); err != nil {
		return Snapshot{}, err
	}
	defer releaseSlot(s.fetchSlot)

	now := s.now().UTC()
	snapshot := Snapshot{CheckedAt: now}
	client, err := s.clientForProxy(parsedProxy)
	if err != nil {
		return Snapshot{}, err
	}
	requestURL, err := timelineRequestURL(s.endpoint, now, s.maxAge)
	if err != nil {
		return Snapshot{}, err
	}
	body, err := fetchBody(requestCtx, client, requestURL, maxTimelineBody)
	if err != nil {
		snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "codex-reset.com timeline", Err: err})
		return snapshot, nil
	}
	timeline, err := parseTimeline(body, now, s.maxAge)
	if err != nil {
		snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "codex-reset.com timeline", Err: err})
		return snapshot, nil
	}
	snapshot.UpdatedAt = timeline.updatedAt
	snapshot.Signals = timeline.signals
	return snapshot, nil
}

// Latest returns the newest global reset event in the timeline. It does not
// apply maxAge so an interactive query can still show the latest known event.
func (s *Scanner) Latest(ctx context.Context, proxyURL string) (Signal, error) {
	if s == nil {
		return Signal{}, errors.New("reset radar scanner is nil")
	}
	parsedProxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return Signal{}, err
	}
	requestCtx, finish, err := s.beginRequest(ctx)
	if err != nil {
		return Signal{}, err
	}
	defer finish()
	if err := acquireSlot(requestCtx, s.latestSlot); err != nil {
		return Signal{}, err
	}
	defer releaseSlot(s.latestSlot)

	client, err := s.clientForProxy(parsedProxy)
	if err != nil {
		return Signal{}, err
	}
	requestURL, err := timelineRequestURL(s.endpoint, time.Time{}, 0)
	if err != nil {
		return Signal{}, err
	}
	body, err := fetchBody(requestCtx, client, requestURL, maxTimelineBody)
	if err != nil {
		return Signal{}, err
	}
	timeline, err := parseTimeline(body, s.now().UTC(), 0)
	if err != nil {
		return Signal{}, err
	}
	if len(timeline.signals) == 0 {
		return Signal{}, errors.New("timeline API 未包含全局重置事件")
	}
	return timeline.signals[len(timeline.signals)-1], nil
}

func (s *Scanner) beginRequest(parent context.Context) (context.Context, func(), error) {
	if parent == nil {
		parent = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errScannerClosed
	}
	s.active.Add(1)
	lifecycleCtx := s.lifecycleCtx
	timeout := s.timeout
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, timeout)
	stopLifecycleCancel := context.AfterFunc(lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycleCancel()
		cancel()
		s.active.Done()
	}, nil
}

func (s *Scanner) clientForProxy(proxyURL *url.URL) (*http.Client, error) {
	proxyKey := ""
	if proxyURL != nil {
		proxyKey = proxyURL.String()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errScannerClosed
	}
	if proxyURL == nil {
		client := s.directClient
		s.mu.Unlock()
		return client, nil
	}
	if s.proxyClient != nil && s.proxyKey == proxyKey {
		client := s.proxyClient
		s.mu.Unlock()
		return client, nil
	}

	transport := newTransport(s.timeout)
	configureProxyTransport(transport, proxyURL, s.timeout)
	client := &http.Client{Transport: transport, Timeout: s.timeout}
	previousTransport := s.proxyTransport
	s.proxyKey = proxyKey
	s.proxyTransport = transport
	s.proxyClient = client
	s.mu.Unlock()

	if previousTransport != nil {
		previousTransport.CloseIdleConnections()
	}
	return client, nil
}

func acquireSlot(ctx context.Context, slot chan struct{}) error {
	select {
	case slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseSlot(slot chan struct{}) { <-slot }

func configureProxyTransport(transport *http.Transport, proxyURL *url.URL, timeout time.Duration) {
	if proxyURL == nil {
		return
	}
	switch proxyURL.Scheme {
	case "http":
		transport.Proxy = http.ProxyURL(proxyURL)
	case "socks5":
		username := ""
		password := ""
		if proxyURL.User != nil {
			username = proxyURL.User.Username()
			password, _ = proxyURL.User.Password()
		}
		transport.DialContext = socks5Dialer{
			proxyAddress: proxyURL.Host,
			username:     username,
			password:     password,
			timeout:      timeout,
		}.DialContext
	}
}

func closeIdleConnections(transports ...*http.Transport) {
	for _, transport := range transports {
		if transport != nil {
			transport.CloseIdleConnections()
		}
	}
}

func newTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
		MaxConnsPerHost:       2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
}

func fetchBody(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "new-api-bot-reset-radar/2.0")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, errorBodyDrainLimit))
		return nil, fmt.Errorf("HTTP %d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("响应超过 %s 上限", formatBytes(limit))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("响应超过 %s 上限", formatBytes(limit))
	}
	return body, nil
}

func timelineRequestURL(rawURL string, now time.Time, maxAge time.Duration) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("解析 timeline API 地址失败: %w", err)
	}
	query := parsed.Query()
	query.Set("group", "reset")
	if maxAge > 0 && !now.IsZero() {
		query.Set("from", now.UTC().Add(-maxAge).Format("2006-01-02"))
	} else {
		query.Del("from")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func formatBytes(value int64) string {
	if value%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", value>>20)
	}
	if value%(1<<10) == 0 {
		return fmt.Sprintf("%d KiB", value>>10)
	}
	return fmt.Sprintf("%d bytes", value)
}
