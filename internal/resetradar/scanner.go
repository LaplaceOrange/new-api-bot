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
	aggregatorURL       = "https://codexreset.org/"
	statusURL           = "https://status.openai.com/api/v2/incidents.json"
	maxXBody            = int64(4 << 20)
	maxAggregate        = int64(2 << 20)
	maxStatusBody       = int64(512 << 10)
	errorBodyDrainLimit = int64(4 << 10)
	minimumRoundTimeout = 30 * time.Second
)

var errScannerClosed = errors.New("reset radar scanner is closed")

var xProfiles = []struct {
	username string
	url      string
}{
	{username: "thsottiaux", url: "https://x.com/thsottiaux"},
	{username: "OpenAI", url: "https://x.com/OpenAI"},
	{username: "OpenAIDevs", url: "https://x.com/OpenAIDevs"},
}

type Scanner struct {
	timeout         time.Duration
	maxAge          time.Duration
	directTransport *http.Transport
	directClient    *http.Client
	now             func() time.Time

	// Fetch is intentionally serialized: a scan already visits every source,
	// and overlapping rounds only multiply memory and connection use.
	fetchMu sync.Mutex
	mu      sync.Mutex
	active  sync.WaitGroup
	closed  bool

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	closeOnce       sync.Once

	xProxyKey  string
	xTransport *http.Transport
	xClient    *http.Client
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
		directTransport: transport,
		directClient:    &http.Client{Transport: transport, Timeout: timeout},
		now:             time.Now,
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
		xTransport := s.xTransport
		s.mu.Unlock()

		closeIdleConnections(directTransport, xTransport)
		s.active.Wait()
		// A canceled in-flight request may return its connection after the
		// first close, so sweep both pools again once all scans have stopped.
		closeIdleConnections(directTransport, xTransport)
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
	roundCtx, finish, err := s.beginFetch(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer finish()

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	xClient, err := s.xClientForProxy(parsedProxy)
	if err != nil {
		return Snapshot{}, err
	}

	now := s.now().UTC()
	snapshot := Snapshot{CheckedAt: now}
	byID := make(map[string]Signal, 8)

	for _, profile := range xProfiles {
		body, fetchErr := fetchBody(roundCtx, xClient, profile.url, maxXBody, true)
		if fetchErr != nil {
			snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "X @" + profile.username, Err: fetchErr})
			continue
		}
		signals, parseErr := parseXPublicPage(body, profile.username, now, s.maxAge)
		if parseErr != nil {
			snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "X @" + profile.username, Err: parseErr})
			continue
		}
		mergeSignals(byID, signals)
	}

	body, fetchErr := fetchBody(roundCtx, s.directClient, aggregatorURL, maxAggregate, false)
	if fetchErr != nil {
		snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "codexreset.org", Err: fetchErr})
	} else {
		aggregate, parseErr := parseAggregator(body, now, s.maxAge)
		if parseErr != nil {
			snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "codexreset.org", Err: parseErr})
		} else {
			snapshot.AggregatorStatus = aggregate.status
			snapshot.AggregatorScore = aggregate.score
			mergeSignals(byID, aggregate.signals)
		}
	}

	body, fetchErr = fetchBody(roundCtx, s.directClient, statusURL, maxStatusBody, false)
	if fetchErr != nil {
		snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "OpenAI status", Err: fetchErr})
	} else {
		signals, parseErr := parseStatusJSON(body, now, s.maxAge)
		if parseErr != nil {
			snapshot.SourceErrors = append(snapshot.SourceErrors, SourceError{Source: "OpenAI status", Err: parseErr})
		} else {
			mergeSignals(byID, signals)
		}
	}

	snapshot.Signals = sortedSignals(byID)
	return snapshot, nil
}

func (s *Scanner) beginFetch(parent context.Context) (context.Context, func(), error) {
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
	timeout := resetRoundTimeout(s.timeout)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, timeout)
	stopLifecycleCancel := context.AfterFunc(lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycleCancel()
		cancel()
		s.active.Done()
	}, nil
}

func (s *Scanner) xClientForProxy(proxyURL *url.URL) (*http.Client, error) {
	proxyKey := ""
	if proxyURL != nil {
		proxyKey = proxyURL.String()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errScannerClosed
	}
	if s.xClient != nil && s.xProxyKey == proxyKey {
		client := s.xClient
		s.mu.Unlock()
		return client, nil
	}

	transport := newTransport(s.timeout)
	configureProxyTransport(transport, proxyURL, s.timeout)
	client := &http.Client{Transport: transport, Timeout: s.timeout}
	previousTransport := s.xTransport
	s.xProxyKey = proxyKey
	s.xTransport = transport
	s.xClient = client
	s.mu.Unlock()

	if previousTransport != nil {
		previousTransport.CloseIdleConnections()
	}
	return client, nil
}

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

func resetRoundTimeout(perSource time.Duration) time.Duration {
	if perSource <= 0 {
		return minimumRoundTimeout
	}
	sourceCount := time.Duration(len(xProfiles) + 2)
	// Every source has its own client timeout. The outer deadline only bounds
	// the whole serial round and must leave later direct sources a chance to run
	// when X is unavailable or blocked.
	if perSource > time.Duration(1<<63-1)/sourceCount {
		return time.Duration(1<<63 - 1)
	}
	roundTimeout := perSource * sourceCount
	if roundTimeout < minimumRoundTimeout {
		return minimumRoundTimeout
	}
	return roundTimeout
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
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		MaxConnsPerHost:       2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
}

func fetchBody(ctx context.Context, client *http.Client, rawURL string, limit int64, browserUA bool) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json,text/html;q=0.9,*/*;q=0.7")
	request.Header.Set("Accept-Language", "en-US,en;q=0.8")
	if browserUA {
		request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	} else {
		request.Header.Set("User-Agent", "new-api-bot-reset-radar/1.0")
	}
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

func formatBytes(value int64) string {
	if value%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", value>>20)
	}
	return fmt.Sprintf("%d bytes", value)
}
