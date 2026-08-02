package newapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxResponseBody = 1 << 20

var ErrRedemptionNotFound = errors.New("未找到对应的兑换码记录")

type Client struct {
	baseURL     string
	adminToken  string
	adminUserID int
	httpClient  *http.Client
	statusMu    sync.RWMutex
	status      Status
	statusAt    time.Time
	lastSuccess atomic.Int64
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("New API 请求失败（HTTP %d）：%s", e.StatusCode, e.Message)
	}
	return "New API 请求失败：" + e.Message
}

type Status struct {
	SystemName   string `json:"system_name"`
	Version      string `json:"version"`
	QuotaPerUnit int64  `json:"quota_per_unit"`
}

type User struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Quota       int64  `json:"quota"`
	UsedQuota   int64  `json:"used_quota"`
	Status      int    `json:"status"`
	Role        int    `json:"role"`
}

type Redemption struct {
	ID          int    `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Status      int    `json:"status"`
	Quota       int64  `json:"quota"`
	ExpiredTime int64  `json:"expired_time"`
}

type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func New(baseURL, adminToken string, adminUserID int, timeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		adminToken:  adminToken,
		adminUserID: adminUserID,
		httpClient:  &http.Client{Transport: transport, Timeout: timeout},
	}
}

func (c *Client) LastSuccess() time.Time {
	value := c.lastSuccess.Load()
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}

func (c *Client) GetStatus(ctx context.Context, force bool) (Status, error) {
	c.statusMu.RLock()
	if !force && c.status.QuotaPerUnit > 0 && time.Since(c.statusAt) < 10*time.Minute {
		status := c.status
		c.statusMu.RUnlock()
		return status, nil
	}
	c.statusMu.RUnlock()

	env, err := c.do(ctx, http.MethodGet, "/api/status", nil, false)
	if err != nil {
		return Status{}, err
	}
	var raw struct {
		SystemName   string      `json:"system_name"`
		Version      string      `json:"version"`
		QuotaPerUnit json.Number `json:"quota_per_unit"`
	}
	if err := decodeRaw(env.Data, &raw); err != nil {
		return Status{}, fmt.Errorf("解析 New API 状态失败: %w", err)
	}
	quota, err := strconv.ParseInt(raw.QuotaPerUnit.String(), 10, 64)
	if err != nil || quota <= 0 {
		return Status{}, errors.New("New API /api/status 未返回有效 quota_per_unit")
	}
	status := Status{SystemName: raw.SystemName, Version: raw.Version, QuotaPerUnit: quota}
	c.statusMu.Lock()
	c.status = status
	c.statusAt = time.Now()
	c.statusMu.Unlock()
	return status, nil
}

func (c *Client) GetUser(ctx context.Context, id int) (User, error) {
	env, err := c.do(ctx, http.MethodGet, "/api/user/"+strconv.Itoa(id), nil, true)
	if err != nil {
		return User{}, err
	}
	var user User
	if err := decodeRaw(env.Data, &user); err != nil {
		return User{}, fmt.Errorf("解析用户信息失败: %w", err)
	}
	if user.ID == 0 {
		return User{}, errors.New("New API 返回的用户信息缺少 ID")
	}
	return user, nil
}

func (c *Client) FindUserByEmail(ctx context.Context, email string) (User, error) {
	path := "/api/user/search?keyword=" + url.QueryEscape(strings.TrimSpace(strings.ToLower(email))) + "&p=1&page_size=20"
	env, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return User{}, err
	}
	users, err := decodeUserItems(env.Data)
	if err != nil {
		return User{}, fmt.Errorf("解析用户搜索结果失败: %w", err)
	}
	var match *User
	for i := range users {
		if strings.EqualFold(strings.TrimSpace(users[i].Email), strings.TrimSpace(email)) {
			if match != nil {
				return User{}, errors.New("该邮箱匹配到多个 New API 用户，无法绑定")
			}
			copy := users[i]
			match = &copy
		}
	}
	if match == nil {
		return User{}, errors.New("未找到使用该邮箱的 New API 用户")
	}
	return *match, nil
}

func decodeUserItems(data json.RawMessage) ([]User, error) {
	var page struct {
		Items []User `json:"items"`
	}
	if err := decodeRaw(data, &page); err == nil && page.Items != nil {
		return page.Items, nil
	}
	var users []User
	if err := decodeRaw(data, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (c *Client) AddQuota(ctx context.Context, userID int, rawQuota int64) error {
	return c.adjustQuota(ctx, userID, rawQuota, "add")
}

func (c *Client) SubtractQuota(ctx context.Context, userID int, rawQuota int64) error {
	return c.adjustQuota(ctx, userID, rawQuota, "subtract")
}

func (c *Client) adjustQuota(ctx context.Context, userID int, rawQuota int64, mode string) error {
	body := map[string]any{
		"id":     userID,
		"action": "add_quota",
		"mode":   mode,
		"value":  rawQuota,
	}
	_, err := c.do(ctx, http.MethodPost, "/api/user/manage", body, true)
	return err
}

func (c *Client) CreateRedemption(ctx context.Context, name string, rawQuota int64, expiresAt time.Time) (string, error) {
	body := map[string]any{
		"name":         name,
		"count":        1,
		"quota":        rawQuota,
		"expired_time": expiresAt.Unix(),
	}
	env, err := c.do(ctx, http.MethodPost, "/api/redemption/", body, true)
	if err != nil {
		return "", err
	}
	var keys []string
	if err := decodeRaw(env.Data, &keys); err != nil {
		return "", fmt.Errorf("解析兑换码创建结果失败: %w", err)
	}
	if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" {
		return "", errors.New("New API 未返回新建兑换码")
	}
	return keys[0], nil
}

func (c *Client) FindRedemptionByName(ctx context.Context, name string) (Redemption, error) {
	path := "/api/redemption/search?keyword=" + url.QueryEscape(name) + "&p=1&page_size=20"
	env, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return Redemption{}, err
	}
	var page struct {
		Items []Redemption `json:"items"`
	}
	if err := decodeRaw(env.Data, &page); err != nil {
		return Redemption{}, err
	}
	for _, item := range page.Items {
		if item.Name == name {
			return item, nil
		}
	}
	return Redemption{}, ErrRedemptionNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body any, auth bool) (envelope, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return envelope{}, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.adminToken)
		req.Header.Set("New-Api-User", strconv.Itoa(c.adminUserID))
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return envelope{}, &APIError{Message: sanitizeMessage(err.Error())}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return envelope{}, &APIError{StatusCode: resp.StatusCode, Message: "读取响应失败"}
	}
	if len(data) > maxResponseBody {
		return envelope{}, &APIError{StatusCode: resp.StatusCode, Message: "响应体超过 1 MiB 限制"}
	}
	var env envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&env); err != nil {
		message := strings.TrimSpace(string(data))
		if len(message) > 200 {
			message = message[:200]
		}
		return envelope{}, &APIError{StatusCode: resp.StatusCode, Message: sanitizeMessage(message)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !env.Success {
		message := strings.TrimSpace(env.Message)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return envelope{}, &APIError{StatusCode: resp.StatusCode, Message: sanitizeMessage(message)}
	}
	c.lastSuccess.Store(time.Now().Unix())
	return env, nil
}

func decodeRaw(data json.RawMessage, target any) error {
	if len(data) == 0 || string(data) == "null" {
		return errors.New("响应 data 为空")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func DisplayToQuota(display string, quotaPerUnit int64) (int64, error) {
	value, ok := new(big.Rat).SetString(strings.TrimSpace(display))
	if !ok || value.Sign() <= 0 || quotaPerUnit <= 0 {
		return 0, errors.New("额度必须是大于 0 的十进制数")
	}
	value.Mul(value, new(big.Rat).SetInt64(quotaPerUnit))
	if !value.IsInt() {
		return 0, errors.New("额度换算后不是整数 quota，请减少小数位数")
	}
	if !value.Num().IsInt64() {
		return 0, errors.New("额度超出支持范围")
	}
	return value.Num().Int64(), nil
}

func CompareDisplay(a, b string) (int, error) {
	aRat, ok := new(big.Rat).SetString(strings.TrimSpace(a))
	if !ok {
		return 0, errors.New("无效额度")
	}
	bRat, ok := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok {
		return 0, errors.New("无效额度上限")
	}
	return aRat.Cmp(bRat), nil
}

func QuotaToDisplay(raw, quotaPerUnit int64) string {
	if quotaPerUnit <= 0 {
		return strconv.FormatInt(raw, 10)
	}
	value := new(big.Rat).SetFrac(big.NewInt(raw), big.NewInt(quotaPerUnit))
	text := value.FloatString(6)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" || text == "-0" {
		return "0"
	}
	return text
}

func sanitizeMessage(message string) string {
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	message = strings.TrimSpace(message)
	if len([]rune(message)) > 300 {
		message = string([]rune(message)[:300]) + "…"
	}
	return message
}
