package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxBody = 2 << 20

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Function struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}
type Completion struct {
	Message          Message
	PromptTokens     int
	CompletionTokens int
	Sources          []Source
}
type Completer interface {
	Complete(context.Context, Config, []Message, []Tool) (Completion, error)
}
type Source struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}
type SearchResult struct {
	Sources []Source `json:"sources"`
	Summary string   `json:"summary,omitempty"`
}
type Searcher interface {
	Search(context.Context, Config, string) (SearchResult, error)
}

type Client struct{ HTTP *http.Client }

func NewClient() *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 8
	tr.MaxIdleConnsPerHost = 2
	tr.MaxConnsPerHost = 4
	return &Client{HTTP: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Close() { c.HTTP.CloseIdleConnections() }

// Errors intentionally exclude URLs, response bodies and transport diagnostics.
func (c *Client) request(ctx context.Context, method, endpoint, key string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("请求编码失败")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, errors.New("请求地址无效")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("模型或搜索服务网络请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("模型或搜索服务返回 HTTP %d（请检查配置、额度及工具支持）", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("读取响应失败")
	}
	if len(b) > maxBody {
		return nil, errors.New("响应超过 2 MiB 限制")
	}
	return b, nil
}
func (c *Client) Complete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Completion, error) {
	payload := map[string]any{"model": cfg.Model, "messages": messages, "stream": false, "max_tokens": cfg.MaxTokens}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
		payload["parallel_tool_calls"] = false
	}
	data, err := c.request(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions", cfg.APIKey, payload)
	if err != nil {
		return Completion{}, err
	}
	return decodeCompletion(data)
}
func decodeCompletion(data []byte) (Completion, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Message
				Annotations []struct {
					Type     string `json:"type"`
					Citation struct {
						URL   string `json:"url"`
						Title string `json:"title"`
					} `json:"url_citation"`
				} `json:"annotations"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil || len(response.Choices) == 0 {
		return Completion{}, errors.New("模型响应格式无效或没有答案")
	}
	m := response.Choices[0].Message
	if len(m.ToolCalls) > 20 {
		return Completion{}, errors.New("模型返回过多工具调用")
	}
	for _, t := range m.ToolCalls {
		if t.ID == "" || t.Type != "function" || len(t.Function.Arguments) > 8192 {
			return Completion{}, errors.New("模型工具调用格式无效")
		}
	}
	if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" {
		return Completion{}, errors.New("模型返回空答案")
	}
	result := Completion{Message: m.Message, PromptTokens: response.Usage.Prompt, CompletionTokens: response.Usage.Completion}
	result.Message.Role = "assistant"
	for _, a := range m.Annotations {
		if a.Type == "url_citation" && safeSourceURL(a.Citation.URL) {
			result.Sources = append(result.Sources, Source{URL: a.Citation.URL, Title: clip(a.Citation.Title, 200)})
		}
	}
	return result, nil
}
func toolContext(parent context.Context, cfg Config) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(cfg.ToolTimeoutSeconds)*time.Second)
}
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
