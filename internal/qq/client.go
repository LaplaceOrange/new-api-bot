package qq

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiBaseURL     = "https://api.bot.qq.com"
	tokenURL       = "https://api.bot.qq.com/app/getAppAccessToken"
	maxQQBodyBytes = 1 << 20
)

type Client struct {
	appID      string
	appSecret  string
	httpClient *http.Client
	tokenMu    sync.Mutex
	token      string
	expiresAt  time.Time
	lastOK     atomic.Int64
}

type APIError struct {
	StatusCode int
	ErrCode    int64
	Message    string
	TraceID    string
}

func (e *APIError) Error() string {
	parts := []string{"QQ API 请求失败"}
	if e.StatusCode != 0 {
		parts = append(parts, "HTTP "+strconv.Itoa(e.StatusCode))
	}
	if e.ErrCode != 0 {
		parts = append(parts, "错误码 "+strconv.FormatInt(e.ErrCode, 10))
	}
	result := strings.Join(parts, "，")
	if e.Message != "" {
		result += "：" + e.Message
	}
	if e.TraceID != "" {
		result += "（TraceID: " + e.TraceID + "）"
	}
	return result
}

type GatewayInfo struct {
	URL    string `json:"url"`
	Shards int    `json:"shards"`
}

type SentMessage struct {
	ID           string `json:"id"`
	Timestamp    string `json:"timestamp"`
	MessageScene struct {
		Ext []string `json:"ext"`
	} `json:"message_scene"`
}
type GroupBotState struct {
	MemberOpenID      string `json:"member_openid"`
	JoinedAt          int64  `json:"joined_at"`
	AllowProactiveMsg bool   `json:"allow_proactive_msg"`
	RecvMsgSetting    int    `json:"recv_msg_setting"`
	MemberRole        int    `json:"member_role"`
}
type GroupInfo struct {
	GroupOpenID    string `json:"group_openid"`
	GroupName      string `json:"group_name"`
	GroupMemberNum int    `json:"group_member_num"`
}

func NewClient(appID, appSecret string, timeout time.Duration) *Client {
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
	return &Client{appID: appID, appSecret: appSecret, httpClient: &http.Client{Transport: transport, Timeout: timeout}}
}

func (c *Client) LastSuccess() time.Time {
	value := c.lastOK.Load()
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}

func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Until(c.expiresAt) > 60*time.Second {
		return c.token, nil
	}
	body, _ := json.Marshal(map[string]string{"appId": c.appID, "clientSecret": c.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxQQBodyBytes))
	if err != nil {
		return "", err
	}
	var result struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   flexInt `json:"expires_in"`
		Code        int64   `json:"code"`
		ErrCode     int64   `json:"err_code"`
		Message     string  `json:"message"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("解析 QQ Access Token 响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.AccessToken == "" {
		code := result.ErrCode
		if code == 0 {
			code = result.Code
		}
		return "", &APIError{StatusCode: resp.StatusCode, ErrCode: code, Message: sanitize(result.Message)}
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 7200
	}
	c.token = result.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	c.lastOK.Store(time.Now().Unix())
	return c.token, nil
}

type flexInt int

func (i *flexInt) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	value = strings.Trim(value, "\"")
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid integer %q", value)
	}
	*i = flexInt(parsed)
	return nil
}

func (c *Client) Gateway(ctx context.Context) (GatewayInfo, error) {
	var info GatewayInfo
	if err := c.request(ctx, http.MethodGet, "/gateway/bot", nil, &info); err != nil {
		return GatewayInfo{}, err
	}
	if info.URL == "" {
		return GatewayInfo{}, errors.New("QQ Gateway 响应缺少 URL")
	}
	return info, nil
}

func (c *Client) ReplyC2C(ctx context.Context, userOpenID, messageID, content string) error {
	body := map[string]any{"msg_type": 0, "content": content}
	if messageID != "" {
		body["msg_id"] = messageID
		body["msg_seq"] = 1
	}
	return c.request(ctx, http.MethodPost, "/v2/users/"+url.PathEscape(userOpenID)+"/messages", body, nil)
}

func (c *Client) ReplyGroup(ctx context.Context, groupOpenID, messageID, content string) error {
	_, err := c.SendGroupText(ctx, groupOpenID, messageID, content)
	return err
}

func (c *Client) SendGroupText(ctx context.Context, groupOpenID, messageID, content string) (SentMessage, error) {
	body := map[string]any{"msg_type": 0, "content": content}
	if messageID != "" {
		body["msg_id"] = messageID
		body["msg_seq"] = 1
	}
	var sent SentMessage
	err := c.request(ctx, http.MethodPost, "/v2/groups/"+url.PathEscape(groupOpenID)+"/messages", body, &sent)
	return sent, err
}

func (c *Client) GetGroupBotState(ctx context.Context, group string) (GroupBotState, error) {
	var value GroupBotState
	err := c.request(ctx, http.MethodGet, "/v2/groups/"+url.PathEscape(group)+"/bot_state", nil, &value)
	return value, err
}
func (c *Client) GetGroupInfo(ctx context.Context, group string) (GroupInfo, error) {
	var value GroupInfo
	err := c.request(ctx, http.MethodGet, "/v2/groups/"+url.PathEscape(group)+"/info", nil, &value)
	return value, err
}
func (c *Client) RecallGroupMessage(ctx context.Context, group, messageID string) error {
	return c.request(ctx, http.MethodDelete, "/v2/groups/"+url.PathEscape(group)+"/messages/"+url.PathEscape(messageID), nil, nil)
}

func (c *Client) SendGroupFile(ctx context.Context, group, replyTo, fileName string, fileType int, data []byte) (SentMessage, error) {
	if len(data) == 0 {
		return SentMessage{}, errors.New("上传文件内容为空")
	}
	md5sum := md5.Sum(data)
	sha1sum := sha1.Sum(data)
	md5Ten := md5.Sum(data[:min(len(data), 10<<20)])
	body := map[string]any{"file_type": fileType, "file_size": strconv.Itoa(len(data)), "file_name": fileName, "md5": fmt.Sprintf("%x", md5sum), "sha1": fmt.Sprintf("%x", sha1sum), "md5_10m": fmt.Sprintf("%x", md5Ten)}
	var prep struct {
		UploadID  string `json:"upload_id"`
		BlockSize int    `json:"block_size"`
		Parts     []struct {
			Index        int    `json:"index"`
			PresignedURL string `json:"presigned_url"`
			BlockSize    int    `json:"block_size"`
		} `json:"parts"`
	}
	if err := c.request(ctx, http.MethodPost, "/v2/groups/"+url.PathEscape(group)+"/upload_prepare", body, &prep); err != nil {
		return SentMessage{}, err
	}
	for _, part := range prep.Parts {
		start := part.Index * prep.BlockSize
		if start >= len(data) && part.Index > 0 {
			start = (part.Index - 1) * prep.BlockSize
		}
		if start < 0 || start >= len(data) {
			return SentMessage{}, errors.New("QQ 上传分片索引无效")
		}
		size := part.BlockSize
		if size <= 0 {
			size = prep.BlockSize
		}
		end := min(start+size, len(data))
		chunk := data[start:end]
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, part.PresignedURL, bytes.NewReader(chunk))
		if err != nil {
			return SentMessage{}, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return SentMessage{}, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxQQBodyBytes))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return SentMessage{}, fmt.Errorf("QQ 文件分片上传失败（HTTP %d）", resp.StatusCode)
		}
		finish := map[string]any{"upload_id": prep.UploadID, "part_index": part.Index, "block_size": strconv.Itoa(len(chunk)), "md5": fmt.Sprintf("%x", md5.Sum(chunk))}
		if err := c.request(ctx, http.MethodPost, "/v2/groups/"+url.PathEscape(group)+"/upload_part_finish", finish, nil); err != nil {
			return SentMessage{}, err
		}
	}
	var complete struct {
		FileInfo string `json:"file_info"`
	}
	if err := c.request(ctx, http.MethodPost, "/v2/groups/"+url.PathEscape(group)+"/files", map[string]any{"file_type": fileType, "srv_send_msg": false, "file_name": fileName, "upload_id": prep.UploadID}, &complete); err != nil {
		return SentMessage{}, err
	}
	msg := map[string]any{"msg_type": 7, "media": map[string]any{"file_info": complete.FileInfo}}
	if replyTo != "" {
		msg["msg_id"] = replyTo
		msg["msg_seq"] = 1
	}
	var sent SentMessage
	err := c.request(ctx, http.MethodPost, "/v2/groups/"+url.PathEscape(group)+"/messages", msg, &sent)
	return sent, err
}

func (c *Client) request(ctx context.Context, method, path string, body any, target any) error {
	token, err := c.AccessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxQQBodyBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxQQBodyBytes {
		return errors.New("QQ API 响应体超过 1 MiB 限制")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp.StatusCode, data)
	}
	if len(data) > 0 {
		var probe struct {
			ErrCode int64  `json:"err_code"`
			Message string `json:"message"`
			TraceID string `json:"trace_id"`
		}
		_ = json.Unmarshal(data, &probe)
		if probe.ErrCode != 0 {
			return &APIError{StatusCode: resp.StatusCode, ErrCode: probe.ErrCode, Message: sanitize(probe.Message), TraceID: sanitize(probe.TraceID)}
		}
		if target != nil {
			if err := json.Unmarshal(data, target); err != nil {
				return fmt.Errorf("解析 QQ API 响应失败: %w", err)
			}
		}
	}
	c.lastOK.Store(time.Now().Unix())
	return nil
}

func parseAPIError(status int, data []byte) error {
	var result struct {
		ErrCode int64  `json:"err_code"`
		Code    int64  `json:"code"`
		Message string `json:"message"`
		TraceID string `json:"trace_id"`
	}
	_ = json.Unmarshal(data, &result)
	if result.ErrCode == 0 {
		result.ErrCode = result.Code
	}
	if result.Message == "" {
		result.Message = http.StatusText(status)
	}
	return &APIError{StatusCode: status, ErrCode: result.ErrCode, Message: sanitize(result.Message), TraceID: sanitize(result.TraceID)}
}

func sanitize(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	if len([]rune(value)) > 200 {
		value = string([]rune(value)[:200]) + "…"
	}
	return value
}
