// Package llm implements bounded Chat Completions and search transports.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Config is snapshotted at job admission. Never log or persist it unencrypted.
type Config struct {
	Enabled            bool   `json:"enabled"`
	BaseURL            string `json:"base_url"`
	APIKey             string `json:"api_key"`
	Model              string `json:"model"`
	SystemPrompt       string `json:"system_prompt"`
	SearchBackend      string `json:"search_backend"`
	TavilyURL          string `json:"tavily_url"`
	TavilyKey          string `json:"tavily_key"`
	SearXNGURL         string `json:"searxng_url"`
	BingURL            string `json:"bing_url"`
	BingKey            string `json:"bing_key"`
	SearchModelURL     string `json:"search_model_url"`
	SearchModelKey     string `json:"search_model_key"`
	SearchModel        string `json:"search_model"`
	MinuteLimit        int    `json:"minute_limit"`
	DailyLimit         int    `json:"daily_limit"`
	Concurrency        int    `json:"concurrency"`
	QueueSize          int    `json:"queue_size"`
	MaxTools           int    `json:"max_tools"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
	ToolTimeoutSeconds int    `json:"tool_timeout_seconds"`
	MaxTokens          int    `json:"max_tokens"`
	MaxReplyRunes      int    `json:"max_reply_runes"`
	ChunkRunes         int    `json:"chunk_runes"`
	HistoryTurns       int    `json:"history_turns"`
	HistoryBytes       int    `json:"history_bytes"`
	HistoryTTLSeconds  int    `json:"history_ttl_seconds"`
}

func DefaultConfig() Config {
	return Config{SearchBackend: "off", TavilyURL: "https://api.tavily.com/search",
		BingURL: "https://serpapi.com/search.json", MinuteLimit: 6, DailyLimit: 100,
		Concurrency: 2, QueueSize: 32, MaxTools: 6, TimeoutSeconds: 120,
		ToolTimeoutSeconds: 20, MaxTokens: 2000, MaxReplyRunes: 6000, ChunkRunes: 1500,
		HistoryTurns: 20, HistoryBytes: 32 << 10, HistoryTTLSeconds: 86400}
}

func Secret(key string) bool {
	switch key {
	case "api_key", "tavily_key", "bing_key", "search_model_key":
		return true
	}
	return false
}

func (c Config) Values() map[string]json.RawMessage {
	b, _ := json.Marshal(c)
	var values map[string]json.RawMessage
	_ = json.Unmarshal(b, &values)
	return values
}

func Keys() []string {
	var keys []string
	for key := range DefaultConfig().Values() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Set uses the default value's type as a closed registry; unknown fields fail.
func (c *Config) Set(key, value string) error {
	values := c.Values()
	v, ok := values[key]
	if !ok {
		return errors.New("未知 LLM 配置键")
	}
	var typed any
	switch {
	case len(v) > 0 && v[0] == '"':
		typed = value
	case string(v) == "true" || string(v) == "false":
		switch strings.ToLower(value) {
		case "true", "on":
			typed = true
		case "false", "off":
			typed = false
		default:
			return errors.New("配置值必须是 true/false 或 on/off")
		}
	default:
		n, err := strconv.Atoi(value)
		if err != nil {
			return errors.New("配置值必须是整数")
		}
		typed = n
	}
	b, _ := json.Marshal(typed)
	values[key] = b
	b, _ = json.Marshal(values)
	var next Config
	if err := json.Unmarshal(b, &next); err != nil {
		return errors.New("配置类型无效")
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (c Config) Validate() error {
	for _, key := range []string{"base_url", "tavily_url", "searxng_url", "bing_url", "search_model_url"} {
		var value string
		_ = json.Unmarshal(c.Values()[key], &value)
		if value != "" && !validURL(value) {
			return fmt.Errorf("%s 必须是无凭据和查询参数的 HTTP(S) 地址", key)
		}
	}
	switch c.SearchBackend {
	case "off", "tavily", "searxng", "bing_serpapi", "model_native":
	default:
		return errors.New("search_backend 必须是 off/tavily/searxng/bing_serpapi/model_native")
	}
	bounds := map[string][3]int{
		"minute_limit": {c.MinuteLimit, 1, 1000}, "daily_limit": {c.DailyLimit, 1, 100000},
		"concurrency": {c.Concurrency, 1, 16}, "queue_size": {c.QueueSize, 1, 512},
		"max_tools": {c.MaxTools, 1, 20}, "timeout_seconds": {c.TimeoutSeconds, 1, 180},
		"tool_timeout_seconds": {c.ToolTimeoutSeconds, 1, 120}, "max_tokens": {c.MaxTokens, 1, 16000},
		"max_reply_runes": {c.MaxReplyRunes, 200, 6000}, "chunk_runes": {c.ChunkRunes, 200, 1500},
		"history_turns": {c.HistoryTurns, 1, 100}, "history_bytes": {c.HistoryBytes, 4096, 262144},
		"history_ttl_seconds": {c.HistoryTTLSeconds, 60, 2592000},
	}
	for key, b := range bounds {
		if b[0] < b[1] || b[0] > b[2] {
			return fmt.Errorf("%s 必须在 %d 至 %d 之间", key, b[1], b[2])
		}
	}
	if c.MaxReplyRunes > c.ChunkRunes*4 {
		return errors.New("max_reply_runes 不能超过 chunk_runes 的四倍（被动回复预算）")
	}
	if len(c.SystemPrompt) > 16384 {
		return errors.New("system_prompt 不能超过 16384 字节")
	}
	for _, v := range []string{c.APIKey, c.TavilyKey, c.BingKey, c.SearchModelKey, c.Model, c.SearchModel} {
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n") {
			return errors.New("密钥或模型名称格式无效")
		}
	}
	return nil
}

func (c Config) Ready() bool { return c.Enabled && c.BaseURL != "" && c.APIKey != "" && c.Model != "" }

func LoadEnvironment() (Config, error) {
	c := DefaultConfig()
	// Validate all fields together: related budgets can be overridden in any order.
	values := c.Values()
	for _, key := range Keys() {
		raw, ok := os.LookupEnv("LLM_" + strings.ToUpper(key))
		if !ok || (raw == "" && key != "system_prompt") {
			continue
		}
		var v any
		if len(values[key]) > 0 && values[key][0] == '"' {
			v = raw
		} else if key == "enabled" {
			b, e := strconv.ParseBool(raw)
			if e != nil {
				return c, errors.New("LLM_ENABLED 必须是 true/false")
			}
			v = b
		} else {
			n, e := strconv.Atoi(raw)
			if e != nil {
				return c, fmt.Errorf("LLM_%s 必须是整数", strings.ToUpper(key))
			}
			v = n
		}
		values[key], _ = json.Marshal(v)
	}
	b, _ := json.Marshal(values)
	_ = json.Unmarshal(b, &c)
	return c, c.Validate()
}
