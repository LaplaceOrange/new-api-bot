package vendorstatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config keeps the upstream configuration names so source/theme settings can
// be migrated directly. Platform routing is owned by the QQ bot, not AstrBot.
type Config struct {
	Enabled                      bool              `json:"enabled"`
	GroupWhitelist               []string          `json:"group_whitelist"`
	PollIntervalSeconds          int               `json:"poll_interval_seconds"`
	NotifyMaintenance            bool              `json:"notify_maintenance"`
	HistoryLookbackHours         int               `json:"history_lookback_hours"`
	NotifySourceFailures         bool              `json:"notify_source_failures"`
	SourceFailureThreshold       int               `json:"source_failure_threshold"`
	SourceFailureCooldownSeconds int               `json:"source_failure_cooldown_seconds"`
	NotifyExistingOnFirstStartup bool              `json:"notify_existing_on_first_startup"`
	DisplayLanguage              string            `json:"display_language"`
	ProgressMode                 string            `json:"progress_mode"`
	CardTheme                    string            `json:"card_theme"`
	Timezone                     string            `json:"timezone"`
	EnableAITranslation          bool              `json:"enable_ai_translation"`
	Sources                      map[string]bool   `json:"sources"`
	CustomStatuspageSources      []CustomSource    `json:"custom_statuspage_sources"`
	Python                       string            `json:"python"`
	Proxy                        string            `json:"proxy"`
	HTTPTimeoutSeconds           int               `json:"http_timeout_seconds"`
	WorkerTimeoutSeconds         int               `json:"worker_timeout_seconds"`
	FontPath                     string            `json:"font_path"`
	Translation                  TranslationConfig `json:"translation"`
}

type CustomSource struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
}

type TranslationConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

var SourceIDs = []string{
	"openai", "openrouter", "claude", "google_vertex_gemini", "gemini_developer",
	"groq", "cohere", "moonshot", "minimax", "fireworks", "novita", "xai",
	"deepseek", "cursor", "cerebras", "aws", "azure", "github", "vercel", "cloudflare",
}

func DefaultConfig() Config {
	return Config{
		Enabled: true, PollIntervalSeconds: 300, HistoryLookbackHours: 24,
		NotifySourceFailures: true, SourceFailureThreshold: 3,
		SourceFailureCooldownSeconds: 3600, NotifyExistingOnFirstStartup: true,
		DisplayLanguage: "bilingual", ProgressMode: "detailed", CardTheme: "paper", Timezone: "Asia/Shanghai",
		EnableAITranslation: true, Python: "python", HTTPTimeoutSeconds: 15,
		WorkerTimeoutSeconds: 600, Sources: map[string]bool{},
	}
}

// DecodeConfig overlays JSON on defaults, including upstream's enabled=true
// default for custom sources whose enabled property was omitted.
func DecodeConfig(data []byte, cfg *Config) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	if document == nil {
		return errors.New("厂商状态配置必须是 JSON 对象")
	}
	// AstrBot-only routing/provider fields have no equivalent in this bot.
	for _, key := range []string{"platform_type", "platform_id", "translation_provider_id"} {
		delete(document, key)
	}
	clean, err := json.Marshal(document)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(clean)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return err
	}
	if raw, ok := document["custom_statuspage_sources"]; ok {
		var sources []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sources); err != nil {
			return err
		}
		for i, source := range sources {
			if _, ok := source["enabled"]; !ok {
				cfg.CustomStatuspageSources[i].Enabled = true
			}
		}
	}
	return nil
}

func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, message string) {
		if !ok {
			errs = append(errs, errors.New(message))
		}
	}
	check(strings.TrimSpace(c.Python) != "", "python 不能为空")
	errs = append(errs, ValidateProxy(c.Proxy))
	check(c.PollIntervalSeconds >= 60, "poll_interval_seconds 不能小于 60")
	check(c.HistoryLookbackHours >= 0 && c.HistoryLookbackHours <= 168, "history_lookback_hours 必须为 0–168")
	check(c.SourceFailureThreshold >= 1 && c.SourceFailureThreshold <= 100, "source_failure_threshold 必须为 1–100")
	check(c.SourceFailureCooldownSeconds >= 60 && c.SourceFailureCooldownSeconds <= 86400, "source_failure_cooldown_seconds 必须为 60–86400")
	check(c.HTTPTimeoutSeconds >= 1 && c.HTTPTimeoutSeconds <= 120, "http_timeout_seconds 必须为 1–120")
	check(c.WorkerTimeoutSeconds >= 30 && c.WorkerTimeoutSeconds <= 3600, "worker_timeout_seconds 必须为 30–3600")
	check(c.DisplayLanguage == "bilingual" || c.DisplayLanguage == "zh-CN" || c.DisplayLanguage == "en-US", "display_language 必须为 bilingual、zh-CN 或 en-US")
	check(c.ProgressMode == "detailed" || c.ProgressMode == "simple" || c.ProgressMode == "off", "progress_mode 必须为 detailed、simple 或 off")
	check(c.CardTheme == "paper" || c.CardTheme == "midnight" || c.CardTheme == "porcelain" || c.CardTheme == "terminal" || c.CardTheme == "liquid_glass", "card_theme 无效")
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("timezone 无效: %w", err))
		}
	}
	known := map[string]bool{}
	for _, id := range SourceIDs {
		known[id] = true
	}
	for id := range c.Sources {
		check(known[id], "未知状态源: "+id)
	}
	for _, group := range c.GroupWhitelist {
		check(strings.TrimSpace(group) != "" && !strings.ContainsAny(group, ": \t\r\n"), "group_whitelist 只接受 QQ 官方 group_openid，不接受 AstrBot UMO")
	}
	validateURL := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
	}
	for _, source := range c.CustomStatuspageSources {
		check(strings.TrimSpace(source.Name) != "" && validateURL(source.BaseURL), "自定义源需要名称和有效的 http/https base_url")
	}
	if c.Translation.BaseURL != "" || c.Translation.APIKey != "" || c.Translation.Model != "" {
		check(validateURL(c.Translation.BaseURL) && c.Translation.APIKey != "" && c.Translation.Model != "", "翻译配置需要有效 base_url、api_key 和 model")
	}
	return errors.Join(errs...)
}

func ValidateProxy(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") ||
		u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return errors.New("proxy 必须是 socks5:// 或 socks5h://[username:password@]host:port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("proxy 端口必须为 1–65535")
	}
	return nil
}
