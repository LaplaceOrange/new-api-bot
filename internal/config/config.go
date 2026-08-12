package config

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	QQAppID                    string
	QQAppSecret                string
	NewAPIBaseURL              string
	NewAPIAdminToken           string
	NewAPIAdminUserID          int
	QQAdminOpenIDs             map[string]struct{}
	BotDataKey                 []byte
	SMTPHost                   string
	SMTPPort                   int
	SMTPUsername               string
	SMTPPassword               string
	SMTPFrom                   string
	SMTPTLSMode                string
	CheckinEnabled             bool
	CheckinCredit              string
	CheckinPeriod              string
	CheckinTimezone            *time.Location
	CheckinTimezoneName        string
	CheckinCodeTTL             time.Duration
	BindCodeTTL                time.Duration
	BindCodeMaxAttempts        int
	BindEmailLimit             int
	BindEmailWindow            time.Duration
	LinkCodeTTL                time.Duration
	CreditMaxPerCommand        string
	DataPath                   string
	ListenAddr                 string
	LogLevel                   slog.Level
	NewAPITimeout              time.Duration
	QQAPITimeout               time.Duration
	GatewayQueueSize           int
	GatewayWorkers             int
	MessageDedupTTL            time.Duration
	ReadinessFreshness         time.Duration
	NotifyCheckInterval        time.Duration
	NotifyDailyTime            string
	NotifyGroupCooldown        time.Duration
	WelcomeDefault             string
	UsageChartEnabled          bool
	NotifyEnabled              bool
	AdminReportExportEnabled   bool
	AdminUserManagementEnabled bool
	BenefitEnabled             bool
	BenefitMaxCount            int
	BenefitMaxBanDays          int
	BenefitCheckInterval       time.Duration
	ResetEnabled               bool
	ResetPollInterval          time.Duration
	ResetHTTPTimeout           time.Duration
	ResetSignalMaxAge          time.Duration
	ResetDefaultDuration       time.Duration
	ResetDefaultWinners        int
	ResetDefaultLookback       time.Duration
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, fmt.Errorf("读取 .env 失败: %w", err)
	}
	c := Config{
		QQAppID:             strings.TrimSpace(os.Getenv("QQ_APP_ID")),
		QQAppSecret:         strings.TrimSpace(os.Getenv("QQ_APP_SECRET")),
		NewAPIBaseURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("NEWAPI_BASE_URL")), "/"),
		NewAPIAdminToken:    strings.TrimSpace(os.Getenv("NEWAPI_ADMIN_TOKEN")),
		SMTPHost:            strings.TrimSpace(os.Getenv("SMTP_HOST")),
		SMTPUsername:        strings.TrimSpace(os.Getenv("SMTP_USERNAME")),
		SMTPPassword:        os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:            strings.TrimSpace(os.Getenv("SMTP_FROM")),
		SMTPTLSMode:         envString("SMTP_TLS_MODE", "starttls"),
		CheckinCredit:       envString("CHECKIN_CREDIT", "1"),
		CheckinPeriod:       envString("CHECKIN_PERIOD", "daily"),
		CheckinTimezoneName: envString("CHECKIN_TIMEZONE", "Asia/Shanghai"),
		CreditMaxPerCommand: envString("CREDIT_MAX_PER_COMMAND", "1000"),
		DataPath:            envString("DATA_PATH", "/data/bot.db"),
		ListenAddr:          envString("LISTEN_ADDR", ":8080"),
		GatewayQueueSize:    64,
		GatewayWorkers:      2,
		MessageDedupTTL:     24 * time.Hour,
		ReadinessFreshness:  5 * time.Minute,
		NotifyDailyTime:     envString("NOTIFY_DAILY_TIME", "09:00"),
		WelcomeDefault:      envString("WELCOME_DEFAULT_MESSAGE", "欢迎加入！如需使用机器人，请发送：/bind <邮箱或New API用户ID>"),
	}

	var errs []error
	require := func(name, value string) {
		if value == "" {
			errs = append(errs, fmt.Errorf("%s 为必填配置", name))
		}
	}
	require("QQ_APP_ID", c.QQAppID)
	require("QQ_APP_SECRET", c.QQAppSecret)
	require("NEWAPI_BASE_URL", c.NewAPIBaseURL)
	require("NEWAPI_ADMIN_TOKEN", c.NewAPIAdminToken)
	require("SMTP_HOST", c.SMTPHost)
	require("SMTP_USERNAME", c.SMTPUsername)
	require("SMTP_PASSWORD", c.SMTPPassword)
	require("SMTP_FROM", c.SMTPFrom)

	if c.NewAPIBaseURL != "" {
		u, err := url.Parse(c.NewAPIBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("NEWAPI_BASE_URL 必须是有效的 http/https 根地址"))
		}
	}
	if c.SMTPFrom != "" {
		if _, err := mail.ParseAddress(c.SMTPFrom); err != nil {
			errs = append(errs, fmt.Errorf("SMTP_FROM 格式无效: %w", err))
		}
	}

	parseInt := func(name string, def, min int) int {
		v, err := strconv.Atoi(envString(name, strconv.Itoa(def)))
		if err != nil || v < min {
			errs = append(errs, fmt.Errorf("%s 必须是大于等于 %d 的整数", name, min))
			return def
		}
		return v
	}
	parseDuration := func(name string, def time.Duration) time.Duration {
		v, err := time.ParseDuration(envString(name, def.String()))
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s 必须是正数 Go duration，例如 10m 或 24h", name))
			return def
		}
		return v
	}
	parseBool := func(name string, def bool) bool {
		v, err := strconv.ParseBool(envString(name, strconv.FormatBool(def)))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s 必须是 true 或 false", name))
			return def
		}
		return v
	}

	c.NewAPIAdminUserID = parseInt("NEWAPI_ADMIN_USER_ID", 0, 1)
	c.SMTPPort = parseInt("SMTP_PORT", 587, 1)
	c.BindCodeMaxAttempts = parseInt("BIND_CODE_MAX_ATTEMPTS", 5, 1)
	c.BindEmailLimit = parseInt("BIND_EMAIL_LIMIT", 2, 1)
	c.CheckinEnabled = parseBool("CHECKIN_ENABLED", true)
	c.UsageChartEnabled = parseBool("USAGE_CHART_ENABLED", true)
	c.NotifyEnabled = parseBool("NOTIFY_ENABLED", true)
	c.AdminReportExportEnabled = parseBool("ADMIN_REPORT_EXPORT_ENABLED", true)
	c.AdminUserManagementEnabled = parseBool("ADMIN_USER_MANAGEMENT_ENABLED", true)
	c.BenefitEnabled = parseBool("BENEFIT_ENABLED", true)
	c.ResetEnabled = parseBool("RESET_ENABLED", true)
	c.BenefitMaxCount = parseInt("BENEFIT_MAX_COUNT", 100, 1)
	if c.BenefitMaxCount > 100 {
		errs = append(errs, errors.New("BENEFIT_MAX_COUNT 不能超过 New API 单次上限 100"))
	}
	c.BenefitMaxBanDays = parseInt("BENEFIT_MAX_BAN_DAYS", 365, 1)
	c.BenefitCheckInterval = parseDuration("BENEFIT_CHECK_INTERVAL", time.Minute)
	c.ResetPollInterval = parseDuration("RESET_POLL_INTERVAL", 3*time.Minute)
	c.ResetHTTPTimeout = parseDuration("RESET_HTTP_TIMEOUT", 20*time.Second)
	c.ResetSignalMaxAge = parseDuration("RESET_SIGNAL_MAX_AGE", 24*time.Hour)
	c.ResetDefaultDuration = parseDuration("RESET_DEFAULT_DURATION", 5*time.Hour)
	c.ResetDefaultWinners = parseInt("RESET_DEFAULT_WINNERS", 5, 1)
	c.ResetDefaultLookback = parseDuration("RESET_DEFAULT_LOOKBACK", 24*time.Hour)
	c.CheckinCodeTTL = parseDuration("CHECKIN_CODE_TTL", 24*time.Hour)
	c.BindCodeTTL = parseDuration("BIND_CODE_TTL", 10*time.Minute)
	c.BindEmailWindow = parseDuration("BIND_EMAIL_WINDOW", time.Hour)
	c.LinkCodeTTL = parseDuration("LINK_CODE_TTL", 10*time.Minute)
	c.NewAPITimeout = parseDuration("NEWAPI_TIMEOUT", 30*time.Second)
	c.QQAPITimeout = parseDuration("QQ_API_TIMEOUT", 10*time.Second)
	c.NotifyCheckInterval = parseDuration("NOTIFY_CHECK_INTERVAL", 10*time.Minute)
	c.NotifyGroupCooldown = parseDuration("NOTIFY_GROUP_COOLDOWN", time.Minute)
	if c.ResetPollInterval < time.Minute {
		errs = append(errs, errors.New("RESET_POLL_INTERVAL 不能小于 1m"))
	}
	if c.ResetHTTPTimeout > 2*time.Minute {
		errs = append(errs, errors.New("RESET_HTTP_TIMEOUT 不能超过 2m"))
	}
	if c.ResetSignalMaxAge > 7*24*time.Hour {
		errs = append(errs, errors.New("RESET_SIGNAL_MAX_AGE 不能超过 168h"))
	}
	if c.ResetDefaultDuration > 7*24*time.Hour {
		errs = append(errs, errors.New("RESET_DEFAULT_DURATION 不能超过 168h"))
	}
	if c.ResetDefaultWinners > 100 {
		errs = append(errs, errors.New("RESET_DEFAULT_WINNERS 不能超过 100"))
	}
	if c.ResetDefaultLookback > 31*24*time.Hour {
		errs = append(errs, errors.New("RESET_DEFAULT_LOOKBACK 不能超过 744h"))
	}
	if _, err := time.Parse("15:04", c.NotifyDailyTime); err != nil {
		errs = append(errs, errors.New("NOTIFY_DAILY_TIME 必须是 HH:MM 格式，例如 09:00"))
	}

	switch c.SMTPTLSMode {
	case "starttls", "tls", "none":
	default:
		errs = append(errs, errors.New("SMTP_TLS_MODE 只能是 starttls、tls 或 none"))
	}
	switch c.CheckinPeriod {
	case "daily", "weekly", "monthly":
	default:
		errs = append(errs, errors.New("CHECKIN_PERIOD 只能是 daily、weekly 或 monthly"))
	}
	loc, err := time.LoadLocation(c.CheckinTimezoneName)
	if err != nil {
		errs = append(errs, fmt.Errorf("CHECKIN_TIMEZONE 不是有效的 IANA 时区: %w", err))
		loc = time.UTC
	}
	c.CheckinTimezone = loc

	level, err := parseLogLevel(envString("LOG_LEVEL", "info"))
	if err != nil {
		errs = append(errs, err)
	}
	c.LogLevel = level

	keyText := strings.TrimSpace(os.Getenv("BOT_DATA_KEY"))
	if keyText == "" {
		errs = append(errs, errors.New("BOT_DATA_KEY 为必填配置"))
	} else {
		key, err := base64.StdEncoding.DecodeString(keyText)
		if err != nil || len(key) != 32 {
			errs = append(errs, errors.New("BOT_DATA_KEY 必须是 Base64 编码的 32 字节随机值"))
		} else {
			c.BotDataKey = key
		}
	}

	c.QQAdminOpenIDs = make(map[string]struct{})
	for _, item := range strings.Split(os.Getenv("QQ_ADMIN_OPENIDS"), ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			c.QQAdminOpenIDs[item] = struct{}{}
		}
	}
	if len(c.QQAdminOpenIDs) == 0 {
		errs = append(errs, errors.New("QQ_ADMIN_OPENIDS 至少需要配置一个 OpenID 标识"))
	}

	if err := validatePositiveDecimal("CHECKIN_CREDIT", c.CheckinCredit); err != nil {
		errs = append(errs, err)
	}
	if err := validatePositiveDecimal("CREDIT_MAX_PER_COMMAND", c.CreditMaxPerCommand); err != nil {
		errs = append(errs, err)
	}

	return c, errors.Join(errs...)
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("第 %d 行缺少等号", lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("第 %d 行配置名为空", lineNumber)
		}
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func envString(name, def string) string {
	if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return def
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errors.New("LOG_LEVEL 只能是 debug、info、warn 或 error")
	}
}

func validatePositiveDecimal(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	dot := false
	digit := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.' && !dot:
			dot = true
		default:
			return fmt.Errorf("%s 必须是正十进制数", name)
		}
	}
	if !digit || strings.Trim(value, "0.") == "" {
		return fmt.Errorf("%s 必须大于 0", name)
	}
	return nil
}
