package vendorstatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Option struct {
	Key      string
	Label    string
	Kind     string
	Example  string
	Aliases  []string
	ReadOnly bool
	Secret   bool
}

// Options is the single command/help/coverage registry. The old provider ID is
// mapped to the native translator model; AstrBot routing becomes fixed QQ data.
var Options = []Option{
	{Key: "enabled", Label: "自动监控", Kind: "bool", Example: "on|off"},
	{Key: "platform_type", Label: "平台类型（固定）", Kind: "string", Example: "qq_official", ReadOnly: true},
	{Key: "platform_id", Label: "平台实例（固定 QQ AppID）", Kind: "string", Example: "show", ReadOnly: true},
	{Key: "group_whitelist", Label: "告警群白名单", Kind: "groups", Example: `["GROUP_ID"] | add|remove|clear`},
	{Key: "poll_interval_seconds", Label: "轮询间隔", Kind: "seconds", Example: "300|5m", Aliases: []string{"interval"}},
	{Key: "notify_maintenance", Label: "维护通知", Kind: "bool", Example: "on|off"},
	{Key: "history_lookback_hours", Label: "历史补报小时数", Kind: "int", Example: "0-168"},
	{Key: "notify_source_failures", Label: "采集异常通知", Kind: "bool", Example: "on|off"},
	{Key: "source_failure_threshold", Label: "连续失败阈值", Kind: "int", Example: "1-100"},
	{Key: "source_failure_cooldown_seconds", Label: "采集异常通知冷却", Kind: "seconds", Example: "3600|1h"},
	{Key: "notify_existing_on_first_startup", Label: "首次已有异常通知", Kind: "bool", Example: "on|off"},
	{Key: "display_language", Label: "图片语言", Kind: "language", Example: "bilingual|zh-CN|en-US", Aliases: []string{"language"}},
	{Key: "card_theme", Label: "图片主题", Kind: "theme", Example: "paper|midnight|porcelain|terminal|liquid_glass", Aliases: []string{"theme"}},
	{Key: "timezone", Label: "图片时区", Kind: "string", Example: `Asia/Shanghai|inherit|""`},
	{Key: "enable_ai_translation", Label: "AI 翻译", Kind: "bool", Example: "on|off"},
	{Key: "translation.model", Label: "翻译模型", Kind: "string", Example: `<model>|""`, Aliases: []string{"translation_provider_id"}},
	{Key: "sources", Label: "20 个内置状态源", Kind: "sources", Example: `openai on|off | {"openai":false}`},
	{Key: "custom_statuspage_sources", Label: "自定义状态源", Kind: "custom", Example: `add "My Service" https://status.example.com`, Aliases: []string{"custom"}},
	{Key: "python", Label: "Python 可执行文件", Kind: "string", Example: `"<absolute_path>"|python`},
	{Key: "http_timeout_seconds", Label: "状态 HTTP 超时", Kind: "seconds", Example: "15|15s"},
	{Key: "worker_timeout_seconds", Label: "任务总超时", Kind: "seconds", Example: "600|10m"},
	{Key: "font_path", Label: "中文字体文件", Kind: "string", Example: `"<absolute_path>"|auto|""`},
	{Key: "translation.base_url", Label: "翻译 API 根地址", Kind: "string", Example: `https://example.com/v1|""`},
	{Key: "translation.api_key", Label: "翻译 API 密钥（单聊）", Kind: "string", Example: `"API_KEY"|""`, Secret: true},
}

func FindOption(key string) (Option, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, option := range Options {
		if key == option.Key {
			return option, true
		}
		for _, alias := range option.Aliases {
			if key == strings.ToLower(alias) {
				return option, true
			}
		}
	}
	if strings.HasPrefix(key, "sources.") {
		id := strings.TrimPrefix(key, "sources.")
		for _, source := range SourceIDs {
			if id == source {
				return Option{Key: key, Label: "状态源 " + id, Kind: "bool", Example: "on|off"}, true
			}
		}
	}
	return Option{}, false
}

// ApplyOverrides returns a deep copy. Only registered, non-secret paths may
// live in the JSON overrides; the key is encrypted by the host separately.
func ApplyOverrides(base Config, overrides map[string]json.RawMessage) (Config, error) {
	data, err := json.Marshal(base)
	if err != nil {
		return Config{}, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return Config{}, err
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys) // Whole objects precede their dotted leaf overrides.
	for _, key := range keys {
		value := overrides[key]
		option, ok := FindOption(key)
		if !ok || option.Key != key || option.ReadOnly || option.Secret || !json.Valid(value) {
			return Config{}, fmt.Errorf("无效的厂商运行时配置项: %s", key)
		}
		if root, child, nested := strings.Cut(key, "."); nested {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(document[root], &object); err != nil {
				return Config{}, err
			}
			if object == nil {
				object = make(map[string]json.RawMessage)
			}
			object[child] = value
			document[root], err = json.Marshal(object)
			if err != nil {
				return Config{}, err
			}
		} else {
			document[key] = value
		}
	}
	data, err = json.Marshal(document)
	if err != nil {
		return Config{}, err
	}
	var result Config
	if err := DecodeConfig(data, &result); err != nil {
		return Config{}, err
	}
	return result, result.ValidateRuntime()
}

func (c Config) ValidateRuntime() error {
	// Commands configure a translator field-by-field. Incomplete credentials
	// safely use upstream's original-text fallback until all three are set.
	translation := c.Translation
	c.Translation = TranslationConfig{}
	var errs []error
	errs = append(errs, c.Validate())
	if c.PollIntervalSeconds > 86400 {
		errs = append(errs, errors.New("poll_interval_seconds 不能超过 86400（24h）"))
	}
	if translation.BaseURL != "" {
		probe := c
		probe.Translation = TranslationConfig{BaseURL: translation.BaseURL, Model: "probe", APIKey: "probe"}
		errs = append(errs, probe.Validate())
	}
	return errors.Join(errs...)
}
