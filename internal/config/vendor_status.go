package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func loadVendorStatusConfig(defaultTimezone string) (vendorstatus.Config, error) {
	cfg := vendorstatus.DefaultConfig()
	cfg.Timezone = defaultTimezone
	if path := strings.TrimSpace(os.Getenv("VENDOR_STATUS_CONFIG_PATH")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("读取 VENDOR_STATUS_CONFIG_PATH 失败: %w", err)
		}
		if len(data) > 1<<20 {
			return cfg, errors.New("厂商状态配置文件不能超过 1 MiB")
		}
		if err := vendorstatus.DecodeConfig(data, &cfg); err != nil {
			return cfg, err
		}
	}
	var errs []error
	setString := func(key string, target *string) {
		if value, ok := os.LookupEnv(key); ok {
			*target = strings.TrimSpace(value)
		}
	}
	setBool := func(key string, target *bool) {
		if raw, ok := os.LookupEnv(key); ok {
			value, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s 必须为 true 或 false", key))
			} else {
				*target = value
			}
		}
	}
	setInt := func(key string, target *int) {
		if raw, ok := os.LookupEnv(key); ok {
			value, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s 必须为整数", key))
			} else {
				*target = value
			}
		}
	}
	setSeconds := func(key string, target *int) {
		if raw, ok := os.LookupEnv(key); ok {
			value, err := time.ParseDuration(strings.TrimSpace(raw))
			if err != nil || value <= 0 || value%time.Second != 0 || value > 24*time.Hour {
				errs = append(errs, fmt.Errorf("%s 必须为整秒的正数 Go duration，且不超过 24h", key))
			} else {
				*target = int(value / time.Second)
			}
		}
	}
	setBool("VENDOR_STATUS_ENABLED", &cfg.Enabled)
	setBool("VENDOR_STATUS_NOTIFY_MAINTENANCE", &cfg.NotifyMaintenance)
	setBool("VENDOR_STATUS_NOTIFY_EXISTING_ON_FIRST_STARTUP", &cfg.NotifyExistingOnFirstStartup)
	setBool("VENDOR_STATUS_NOTIFY_SOURCE_FAILURES", &cfg.NotifySourceFailures)
	setBool("VENDOR_STATUS_ENABLE_AI_TRANSLATION", &cfg.EnableAITranslation)
	setSeconds("VENDOR_STATUS_POLL_INTERVAL", &cfg.PollIntervalSeconds)
	setSeconds("VENDOR_STATUS_HTTP_TIMEOUT", &cfg.HTTPTimeoutSeconds)
	setSeconds("VENDOR_STATUS_WORKER_TIMEOUT", &cfg.WorkerTimeoutSeconds)
	setSeconds("VENDOR_STATUS_SOURCE_FAILURE_COOLDOWN", &cfg.SourceFailureCooldownSeconds)
	setInt("VENDOR_STATUS_HISTORY_LOOKBACK_HOURS", &cfg.HistoryLookbackHours)
	setInt("VENDOR_STATUS_SOURCE_FAILURE_THRESHOLD", &cfg.SourceFailureThreshold)
	setString("VENDOR_STATUS_PYTHON", &cfg.Python)
	setString("VENDOR_STATUS_PROXY", &cfg.Proxy)
	setString("VENDOR_STATUS_DISPLAY_LANGUAGE", &cfg.DisplayLanguage)
	setString("VENDOR_STATUS_PROGRESS_MODE", &cfg.ProgressMode)
	setString("VENDOR_STATUS_CARD_THEME", &cfg.CardTheme)
	setString("VENDOR_STATUS_TIMEZONE", &cfg.Timezone)
	setString("VENDOR_STATUS_FONT_PATH", &cfg.FontPath)
	setString("VENDOR_STATUS_TRANSLATION_BASE_URL", &cfg.Translation.BaseURL)
	setString("VENDOR_STATUS_TRANSLATION_API_KEY", &cfg.Translation.APIKey)
	setString("VENDOR_STATUS_TRANSLATION_MODEL", &cfg.Translation.Model)
	if raw, ok := os.LookupEnv("VENDOR_STATUS_GROUP_WHITELIST"); ok {
		cfg.GroupWhitelist = nil
		seen := map[string]bool{}
		for _, group := range strings.Split(raw, ",") {
			group = strings.TrimSpace(group)
			if group != "" && !seen[group] {
				cfg.GroupWhitelist = append(cfg.GroupWhitelist, group)
				seen[group] = true
			}
		}
	}
	errs = append(errs, cfg.Validate())
	return cfg, errors.Join(errs...)
}
