package rss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	http *http.Client
}

func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.Opaque != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("订阅地址必须是有效的 HTTP/HTTPS URL")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", errors.New("订阅地址端口无效")
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func ValidateProxyURL(raw string) error {
	if raw == "" || strings.EqualFold(raw, "off") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("RSS_PROXY_URL 必须是有效的 HTTP/HTTPS/SOCKS5/SOCKS5H 代理地址")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("RSS_PROXY_URL 端口无效")
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 {
			return errors.New("RSS_PROXY_URL 认证信息过长")
		}
	}
	return nil
}

func NewClient(timeout time.Duration, proxy string) (*Client, error) {
	if err := ValidateProxyURL(proxy); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 2, MaxIdleConnsPerHost: 1,
		MaxConnsPerHost: 2, IdleConnTimeout: time.Minute,
		TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	if strings.EqualFold(proxy, "off") {
		transport.Proxy = nil
	} else if proxy != "" {
		u, _ := url.Parse(proxy)
		transport.Proxy = http.ProxyURL(u)
	}
	return &Client{http: &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return errors.New("RSS 重定向超过 5 次")
			}
			if _, err := NormalizeURL(request.URL.String()); err != nil {
				return err
			}
			// Never forward feed authentication or validators across origins.
			if len(via) > 0 && (request.URL.Host != via[0].URL.Host || request.URL.Scheme != via[0].URL.Scheme) {
				request.Header.Del("Authorization")
				request.Header.Del("If-None-Match")
				request.Header.Del("If-Modified-Since")
			}
			return nil
		},
	}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Fetch(ctx context.Context, rawURL string, validators Validators) (Result, error) {
	normalized, err := NormalizeURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
	if err != nil {
		return Result{}, errors.New("无法创建 RSS 请求")
	}
	req.Header.Set("User-Agent", "new-api-bot-rss/1.0")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml")
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}
	response, err := c.http.Do(req)
	if err != nil {
		// net/http errors may contain both the feed URL and proxy credentials.
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{}, errors.New("RSS 请求超时")
		}
		return Result{}, errors.New("RSS 网络请求失败（请检查地址、网络或代理）")
	}
	defer response.Body.Close()
	result := Result{Validators: Validators{
		ETag: response.Header.Get("ETag"), LastModified: response.Header.Get("Last-Modified"),
	}}
	if response.StatusCode == http.StatusNotModified {
		if validators == (Validators{}) {
			return Result{}, errors.New("RSS 未提供内容却返回 304")
		}
		result.NotModified = true
		result.Validators = validators
		return result, nil
	}
	if response.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("RSS 请求失败（HTTP %d）", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	if err != nil {
		return Result{}, errors.New("读取 RSS 响应失败")
	}
	if len(body) > MaxBodyBytes {
		return Result{}, errors.New("RSS 响应超过 2 MiB")
	}
	result.Feed, err = Parse(body, response.Request.URL.String())
	return result, err
}
