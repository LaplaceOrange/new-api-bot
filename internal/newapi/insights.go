package newapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxUserPages = 100

type UsageRecord struct {
	UserID    int    `json:"user_id"`
	Username  string `json:"username"`
	ModelName string `json:"model_name"`
	CreatedAt int64  `json:"created_at"`
	TokenUsed int64  `json:"token_used"`
	Count     int64  `json:"count"`
	Quota     int64  `json:"quota"`
}

type LogRecord struct {
	ID               int    `json:"id"`
	UserID           int    `json:"user_id"`
	Username         string `json:"username"`
	CreatedAt        int64  `json:"created_at"`
	Type             int    `json:"type"`
	ModelName        string `json:"model_name"`
	Quota            int64  `json:"quota"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	UseTime          int64  `json:"use_time"`
	IsStream         bool   `json:"is_stream"`
	Group            string `json:"group"`
	Content          string `json:"content"`
}

type LogPage struct {
	Items []LogRecord `json:"items"`
	Total int         `json:"total"`
}

func (c *Client) CountLogOutcomes(ctx context.Context, start, end time.Time, username string) (success, failed int64, err error) {
	for page := 1; page <= maxUserPages; page++ {
		result, listErr := c.ListLogs(ctx, start, end, username, page, 100)
		if listErr != nil {
			return 0, 0, listErr
		}
		for _, item := range result.Items {
			if item.Type == 5 {
				failed++
			} else if item.Type == 2 {
				success++
			}
		}
		if len(result.Items) < 100 || int(success+failed) >= result.Total {
			return success, failed, nil
		}
	}
	return success, failed, errors.New("日志数量超过最大分页限制")
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	users := make([]User, 0, 128)
	for page := 1; page <= maxUserPages; page++ {
		path := "/api/user/?p=" + strconv.Itoa(page) + "&page_size=100"
		env, err := c.do(ctx, http.MethodGet, path, nil, true)
		if err != nil {
			return nil, err
		}
		var result struct {
			Items []User `json:"items"`
			Total int    `json:"total"`
		}
		if err := decodeRaw(env.Data, &result); err != nil {
			return nil, fmt.Errorf("解析用户列表失败: %w", err)
		}
		users = append(users, result.Items...)
		if len(result.Items) == 0 || (result.Total > 0 && len(users) >= result.Total) || len(result.Items) < 100 {
			return users, nil
		}
	}
	return nil, errors.New("New API 用户列表超过最大分页限制")
}

func (c *Client) ListUsageByUser(ctx context.Context, start, end time.Time) ([]UsageRecord, error) {
	return c.listUsage(ctx, "/api/data/users", start, end, "")
}

func (c *Client) ListUsageByModel(ctx context.Context, start, end time.Time, username string) ([]UsageRecord, error) {
	return c.listUsage(ctx, "/api/data", start, end, username)
}

func (c *Client) listUsage(ctx context.Context, endpoint string, start, end time.Time, username string) ([]UsageRecord, error) {
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return nil, errors.New("用量查询时间范围无效")
	}
	query := url.Values{}
	query.Set("start_timestamp", strconv.FormatInt(start.Unix(), 10))
	query.Set("end_timestamp", strconv.FormatInt(end.Unix(), 10))
	if strings.TrimSpace(username) != "" {
		query.Set("username", strings.TrimSpace(username))
	}
	env, err := c.do(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil, true)
	if err != nil {
		return nil, err
	}
	var records []UsageRecord
	if err := decodeRawAllowEmptyArray(env.Data, &records); err != nil {
		return nil, fmt.Errorf("解析用量统计失败: %w", err)
	}
	return records, nil
}

func (c *Client) ListLogs(ctx context.Context, start, end time.Time, username string, page, pageSize int) (LogPage, error) {
	return c.ListLogsByType(ctx, start, end, username, 0, page, pageSize)
}

func (c *Client) ListLogsByType(ctx context.Context, start, end time.Time, username string, logType, page, pageSize int) (LogPage, error) {
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return LogPage{}, errors.New("日志查询时间范围无效")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	if pageSize > 100 {
		pageSize = 100
	}
	query := url.Values{}
	query.Set("type", strconv.Itoa(logType))
	query.Set("start_timestamp", strconv.FormatInt(start.Unix(), 10))
	query.Set("end_timestamp", strconv.FormatInt(end.Unix(), 10))
	query.Set("username", strings.TrimSpace(username))
	query.Set("p", strconv.Itoa(page))
	query.Set("page_size", strconv.Itoa(pageSize))
	env, err := c.do(ctx, http.MethodGet, "/api/log/?"+query.Encode(), nil, true)
	if err != nil {
		return LogPage{}, err
	}
	var result LogPage
	if err := decodeRaw(env.Data, &result); err != nil {
		return LogPage{}, fmt.Errorf("解析调用日志失败: %w", err)
	}
	if result.Items == nil {
		result.Items = []LogRecord{}
	}
	return result, nil
}

func (c *Client) ListUserModels(ctx context.Context, group string) ([]string, error) {
	path := "/api/user/models"
	if strings.TrimSpace(group) != "" {
		path += "?group=" + url.QueryEscape(strings.TrimSpace(group))
	}
	env, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	var models []string
	if err := decodeRawAllowEmptyArray(env.Data, &models); err != nil {
		return nil, fmt.Errorf("解析用户可用模型失败: %w", err)
	}
	unique := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, name := range models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := unique[name]; ok {
			continue
		}
		unique[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (c *Client) ListEnabledModels(ctx context.Context) ([]string, error) {
	env, err := c.do(ctx, http.MethodGet, "/api/channel/models_enabled", nil, true)
	if err != nil {
		return nil, err
	}
	var models []string
	if err := decodeRawAllowEmptyArray(env.Data, &models); err != nil {
		return nil, fmt.Errorf("解析已启用模型失败: %w", err)
	}
	unique := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, name := range models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := unique[name]; exists {
			continue
		}
		unique[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}
