package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func vendorConfigWriteCommand(fields []string) bool {
	if len(fields) <= 2 {
		return false
	}
	switch strings.ToLower(fields[1]) {
	case "show", "get", "list", "help":
		return false
	}
	if len(fields) == 3 {
		if option, ok := vendorstatus.FindOption(fields[1]); ok {
			if option.Kind == "sources" {
				if _, ok := vendorstatus.FindOption("sources." + fields[2]); ok {
					return false
				}
			}
			if option.Kind == "sources" || option.Kind == "custom" || option.Kind == "groups" {
				return !strings.EqualFold(fields[2], "list")
			}
		}
	}
	return true
}

func (s *Service) handleVendorConfig(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, content string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "仅 Bot 管理员可查看或修改厂商配置。")
	}
	args, err := splitVendorConfigArgs(content)
	if err != nil {
		return s.reply(ctx, event, "参数格式错误：引号未闭合，或包含无效的参数。")
	}
	args = args[1:]
	if len(args) > 0 {
		args[0] = strings.ToLower(args[0])
	}
	if len(args) == 1 && args[0] == "help" {
		return s.replyChunked(ctx, event, vendorConfigHelp(), 1500)
	}
	if len(args) == 0 || len(args) == 1 && isVendorShow(args[0]) {
		cfg, err := s.vendorConfigSnapshot()
		if err != nil {
			return s.reply(ctx, event, "读取厂商配置失败，请检查机器人日志和数据库。")
		}
		return s.replyChunked(ctx, event, s.vendorConfigOverview(cfg), 1500)
	}
	if args[0] == "reset" {
		if len(args) != 2 {
			return s.reply(ctx, event, "用法：/vendor_config reset <key|all>")
		}
		if s.isReadOnlyAdmin(identity) {
			return s.reply(ctx, event, "只读管理员仅可查看厂商配置。")
		}
		key := args[1]
		if strings.EqualFold(key, "all") {
			key = "all"
		} else {
			option, ok := vendorstatus.FindOption(key)
			if !ok {
				return s.reply(ctx, event, "未知配置项，请使用 /vendor_config help。")
			}
			if option.ReadOnly {
				return s.reply(ctx, event, "平台类型和实例由当前 QQ 机器人固定，不需要重置。")
			}
			key = option.Key
		}
		s.vendorConfigMu.Lock()
		err := s.setVendorConfigValue(key, nil, true)
		s.vendorConfigMu.Unlock()
		if err != nil {
			return s.reply(ctx, event, "重置厂商配置失败："+err.Error())
		}
		s.vendorConfigChanged(identity, key, "reset")
		return s.reply(ctx, event, "已清除 "+key+" 的命令覆盖，恢复部署配置（JSON/.env/内置默认值）。新查询和后台轮询使用新设置。")
	}
	viewOnly := isVendorShow(args[0])
	if viewOnly || args[0] == "set" {
		args = args[1:]
	}
	if len(args) == 0 {
		return s.reply(ctx, event, "用法：/vendor_config <key> [value]；/vendor_config help")
	}
	option, ok := vendorstatus.FindOption(args[0])
	if !ok {
		return s.reply(ctx, event, "未知配置项，请使用 /vendor_config help 查看全部配置。")
	}
	args = args[1:]
	// Both "sources.openai on" and "sources openai on" address one source.
	if option.Key == "sources" && len(args) >= 1 && !strings.EqualFold(args[0], "list") && !strings.HasPrefix(args[0], "{") {
		if leaf, ok := vendorstatus.FindOption("sources." + args[0]); ok {
			option, args = leaf, args[1:]
		}
	}
	if len(args) == 1 && strings.EqualFold(args[0], "list") && (option.Kind == "sources" || option.Kind == "custom" || option.Kind == "groups") {
		args = nil
	}
	if viewOnly && len(args) != 0 {
		return s.reply(ctx, event, "查看命令不接受设置值。用法：/vendor_config show <key>")
	}
	if len(args) == 0 {
		cfg, err := s.vendorConfigSnapshot()
		if err != nil {
			return s.reply(ctx, event, "读取厂商配置失败，请检查数据库或加密密钥。")
		}
		value := s.vendorOptionDisplay(cfg, option, false)
		if option.Kind == "custom" {
			value += "\n管理：custom add <name> <url> [on|off]；custom remove <name>；custom set <name> name|base_url|enabled <value>。"
		}
		return s.replyChunked(ctx, event, option.Key+"（"+option.Label+"）：\n"+value, 1500)
	}
	if s.isReadOnlyAdmin(identity) {
		return s.reply(ctx, event, "只读管理员仅可查看厂商配置。")
	}
	if option.ReadOnly {
		return s.reply(ctx, event, "本项目只有一个 QQ 官方实例：platform_type=qq_official，platform_id="+nonEmpty(s.cfg.QQAppID, "未配置 AppID")+"。不能通过厂商配置切换平台或更改 QQ 身份。")
	}
	if option.Secret && event.EventType != "C2C_MESSAGE_CREATE" {
		return s.reply(ctx, event, "翻译密钥仅允许在机器人单聊中配置，群聊不会保存该项。")
	}
	s.vendorConfigMu.Lock()
	cfg, err := s.vendorConfigSnapshot()
	if err == nil {
		var value any
		value, err = parseVendorOptionValue(option, args, cfg)
		if err == nil {
			err = s.setVendorConfigValue(option.Key, value, false)
		}
	}
	s.vendorConfigMu.Unlock()
	if err != nil {
		if option.Secret {
			return s.reply(ctx, event, "保存翻译密钥失败，请检查参数或数据库；密钥不会回显。")
		}
		return s.reply(ctx, event, "厂商配置未修改："+err.Error()+"\n用法：/vendor_config "+option.Key+" "+option.Example)
	}
	s.vendorConfigChanged(identity, option.Key, "set")
	if option.Secret {
		return s.reply(ctx, event, "已保存翻译密钥设置（加密存储、不回显）。新查询和下一轮采集生效。")
	}
	return s.reply(ctx, event, "已保存 "+option.Key+"，重启后保持。新查询使用新设置，已通知后台监控重新加载。")
}

func isVendorShow(value string) bool {
	switch strings.ToLower(value) {
	case "show", "get", "list":
		return true
	}
	return false
}

func (s *Service) vendorConfigChanged(identity model.QQIdentity, key, action string) {
	// Never store a setting value or an API key in the audit description.
	_ = s.store.AddAudit(model.AuditRecord{
		At: time.Now(), Actor: commandRuleActor(identity), Action: "vendor.config." + action,
		Target: key, Success: true,
	})
	s.signalVendorConfigChanged()
}

func parseVendorOptionValue(option vendorstatus.Option, args []string, cfg vendorstatus.Config) (any, error) {
	switch option.Kind {
	case "groups":
		return parseVendorGroups(args, cfg.GroupWhitelist)
	case "custom":
		return parseVendorCustomSources(args, cfg.CustomStatuspageSources)
	}
	if len(args) != 1 {
		return nil, errors.New("该项只接受一个值；含空格的值请使用引号")
	}
	raw := args[0]
	switch option.Kind {
	case "bool":
		return parseVendorBool(raw)
	case "int":
		value, err := strconv.Atoi(raw)
		if err != nil {
			return nil, errors.New("必须填写整数")
		}
		return value, nil
	case "seconds":
		value, err := strconv.Atoi(raw)
		if err == nil {
			return value, nil
		}
		duration, err := time.ParseDuration(raw)
		if err != nil || duration%time.Second != 0 || duration < 0 || duration > 24*time.Hour {
			return nil, errors.New("必须填写整数秒或整秒 Go duration，例如 300、5m")
		}
		return int(duration / time.Second), nil
	case "language":
		switch strings.ToLower(raw) {
		case "bilingual":
			return "bilingual", nil
		case "zh-cn":
			return "zh-CN", nil
		case "english", "en-us":
			return "en-US", nil
		}
		return nil, errors.New("语言只能是 bilingual、zh-CN、en-US")
	case "theme":
		themes := map[string]string{
			"paper": "paper", "midnight": "midnight", "porcelain": "porcelain",
			"terminal": "terminal", "liquid_glass": "liquid_glass",
		}
		if theme, ok := themes[strings.ToLower(raw)]; ok {
			return theme, nil
		}
		return nil, errors.New("无效主题")
	case "sources":
		var values map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
			return nil, errors.New("sources 必须是 JSON 对象，或使用 sources.<id> on|off")
		}
		sources := make(map[string]bool)
		for id, value := range values {
			if string(value) != "true" && string(value) != "false" {
				return nil, errors.New("每个状态源值必须是 true 或 false")
			}
			sources[id] = string(value) == "true"
		}
		// Match upstream's omitted-source default: a whole-object replacement
		// restores unspecified sources to enabled rather than stale overrides.
		return sources, nil
	case "string":
		if option.Key == "timezone" && (raw == "inherit" || raw == "default") {
			return "", nil
		}
		if option.Key == "font_path" && raw == "auto" {
			return "", nil
		}
		return raw, nil
	}
	return nil, errors.New("未知配置类型")
}

func parseVendorBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	}
	return false, errors.New("布尔值必须是 true|false、on|off 或 1|0")
}

func parseVendorGroups(args, current []string) ([]string, error) {
	if len(args) == 1 && strings.EqualFold(args[0], "clear") {
		return []string{}, nil
	}
	if len(args) == 1 && strings.HasPrefix(args[0], "[") {
		var groups []string
		if err := json.Unmarshal([]byte(args[0]), &groups); err != nil {
			return nil, errors.New("白名单必须是群 OpenID JSON 数组")
		}
		return uniqueVendorGroups(groups)
	}
	if len(args) == 2 {
		if _, err := uniqueVendorGroups([]string{args[1]}); err != nil {
			return nil, err
		}
		target := strings.TrimSpace(args[1])
		groups := append([]string(nil), current...)
		switch strings.ToLower(args[0]) {
		case "add":
			groups = append(groups, target)
		case "remove", "del":
			groups = nil
			for _, group := range current {
				if group != target {
					groups = append(groups, group)
				}
			}
		default:
			return nil, errors.New("白名单操作必须是 add、remove、clear 或 JSON 数组")
		}
		return uniqueVendorGroups(groups)
	}
	return nil, errors.New("白名单用法：add <group_openid>、remove <group_openid>、clear 或 JSON 数组")
}

func uniqueVendorGroups(groups []string) ([]string, error) {
	set := map[string]bool{}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" || strings.ContainsAny(group, ": \t\r\n") {
			return nil, errors.New("群白名单仅接受 QQ group_openid，不接受 UMO 或空值")
		}
		set[group] = true
	}
	result := make([]string, 0, len(set))
	for group := range set {
		result = append(result, group)
	}
	sort.Strings(result)
	return result, nil
}

func parseVendorCustomSources(args []string, current []vendorstatus.CustomSource) ([]vendorstatus.CustomSource, error) {
	if len(args) == 1 && strings.EqualFold(args[0], "clear") {
		return []vendorstatus.CustomSource{}, nil
	}
	if len(args) == 1 && strings.HasPrefix(args[0], "[") {
		cfg := vendorstatus.DefaultConfig()
		payload := []byte(`{"custom_statuspage_sources":` + args[0] + `}`)
		if err := vendorstatus.DecodeConfig(payload, &cfg); err != nil {
			return nil, errors.New("自定义源必须为包含 name、base_url、enabled 的 JSON 数组")
		}
		return validateVendorCustomSources(cfg.CustomStatuspageSources)
	}
	if len(args) < 2 {
		return nil, errors.New("自定义源用法：add <name> <url> [on|off]；remove <name>；set <name> name|base_url|enabled <value>")
	}
	sources := append([]vendorstatus.CustomSource(nil), current...)
	index := -1
	for i, source := range sources {
		if source.Name == args[1] {
			index = i
			break
		}
	}
	switch strings.ToLower(args[0]) {
	case "add":
		if len(args) != 3 && len(args) != 4 {
			return nil, errors.New("添加自定义源需要名称、URL 和可选开关")
		}
		if index >= 0 {
			return nil, errors.New("自定义源名称已存在")
		}
		enabled := true
		if len(args) == 4 {
			var err error
			enabled, err = parseVendorBool(args[3])
			if err != nil {
				return nil, err
			}
		}
		sources = append(sources, vendorstatus.CustomSource{Name: args[1], BaseURL: args[2], Enabled: enabled})
	case "remove", "del":
		if len(args) != 2 || index < 0 {
			return nil, errors.New("请指定存在的自定义源名称")
		}
		sources = append(sources[:index], sources[index+1:]...)
	case "enable", "disable":
		if len(args) != 2 || index < 0 {
			return nil, errors.New("请指定存在的自定义源名称")
		}
		sources[index].Enabled = strings.EqualFold(args[0], "enable")
	case "set":
		if len(args) != 4 || index < 0 {
			return nil, errors.New("设置用法：set <name> name|base_url|enabled <value>")
		}
		switch args[2] {
		case "name":
			sources[index].Name = args[3]
		case "base_url", "url":
			sources[index].BaseURL = args[3]
		case "enabled":
			enabled, err := parseVendorBool(args[3])
			if err != nil {
				return nil, err
			}
			sources[index].Enabled = enabled
		default:
			return nil, errors.New("自定义源字段只能为 name、base_url、enabled")
		}
	default:
		return nil, errors.New("未知自定义源操作")
	}
	return validateVendorCustomSources(sources)
}

func validateVendorCustomSources(sources []vendorstatus.CustomSource) ([]vendorstatus.CustomSource, error) {
	names := map[string]bool{}
	for i := range sources {
		sources[i].Name = strings.TrimSpace(sources[i].Name)
		if sources[i].Name == "" || names[sources[i].Name] {
			return nil, errors.New("自定义源名称不能为空或重复")
		}
		names[sources[i].Name] = true
	}
	cfg := vendorstatus.DefaultConfig()
	cfg.CustomStatuspageSources = sources
	return sources, cfg.Validate()
}

func vendorConfigHelp() string {
	lines := []string{
		"厂商配置（管理员，无需绑定；只读管理员只能查询）：",
		"/vendor_config show [key] - 显示当前有效设置",
		"/vendor_config <key> <value> - 持久化修改",
		"/vendor_config reset <key|all> - 清除命令覆盖，恢复部署配置",
		"全部配置项：",
	}
	for _, option := range vendorstatus.Options {
		suffix := option.Example
		if option.ReadOnly {
			suffix = "只读，本项目固定值"
		}
		lines = append(lines, option.Key+"： "+suffix+"（"+option.Label+"）")
	}
	lines = append(lines,
		"来源开关：/vendor_config sources.<id> on|off；也支持 sources <id> on|off",
		"内置 ID："+strings.Join(vendorstatus.SourceIDs, "、"),
		`自定义源：custom add "<name>" <url> [on|off]；custom remove "<name>"`,
		`自定义字段：custom set "<name>" name|base_url|enabled <value>`,
		"翻译模型也可用原配置名 translation_provider_id。",
		`值含空格请加双引号；"" 表示清空。优先级：命令覆盖 > 环境变量 > JSON > 内置默认。`,
		"enabled on|off 即时启停后台；已有查询沿用启动时快照，后续查询使用新设置。",
	)
	return strings.Join(lines, "\n")
}

func (s *Service) vendorConfigOverview(cfg vendorstatus.Config) string {
	lines := []string{"厂商当前有效配置（密钥不回显）："}
	for _, option := range vendorstatus.Options {
		lines = append(lines, option.Key+" = "+s.vendorOptionDisplay(cfg, option, true))
	}
	if cfg.Translation.BaseURL == "" || cfg.Translation.APIKey == "" || cfg.Translation.Model == "" {
		lines = append(lines, "翻译连接未完整配置，继续使用官方原文；可逐项配置 translation.base_url/api_key/model。")
	}
	lines = append(lines, "修改：/vendor_config <key> <value>；完整说明：/vendor_config help")
	return strings.Join(lines, "\n")
}

func (s *Service) vendorOptionDisplay(cfg vendorstatus.Config, option vendorstatus.Option, summary bool) string {
	if option.Key == "platform_type" {
		return "qq_official（固定）"
	}
	if option.Key == "platform_id" {
		return nonEmpty(s.cfg.QQAppID, "未配置 AppID") + "（固定）"
	}
	if option.Secret {
		if cfg.Translation.APIKey == "" {
			return "未设置"
		}
		return "已设置（隐藏）"
	}
	if summary {
		switch option.Kind {
		case "groups":
			return fmt.Sprintf("%d 个群；单独查看 group_whitelist 获取完整列表", len(cfg.GroupWhitelist))
		case "custom":
			return fmt.Sprintf("%d 个自定义源；单独查看 custom 获取完整列表", len(cfg.CustomStatuspageSources))
		case "sources":
			enabled := 0
			for _, id := range vendorstatus.SourceIDs {
				if value, set := cfg.Sources[id]; !set || value {
					enabled++
				}
			}
			return fmt.Sprintf("%d/20 启用；单独查看 sources 获取完整列表", enabled)
		}
	}
	if option.Kind == "sources" {
		values := map[string]bool{}
		for _, id := range vendorstatus.SourceIDs {
			value, set := cfg.Sources[id]
			values[id] = !set || value
		}
		data, _ := json.MarshalIndent(values, "", "  ")
		return string(data)
	}
	if strings.HasPrefix(option.Key, "sources.") {
		value, set := cfg.Sources[strings.TrimPrefix(option.Key, "sources.")]
		return strconv.FormatBool(!set || value)
	}
	data, _ := json.Marshal(cfg)
	var object map[string]any
	_ = json.Unmarshal(data, &object)
	var value any
	if root, leaf, nested := strings.Cut(option.Key, "."); nested {
		value = object[root].(map[string]any)[leaf]
	} else {
		value = object[option.Key]
	}
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

// A small shell-style parser preserves quoted names, empty strings, Windows
// paths and angle brackets in secrets. A final JSON object/array is one value.
func splitVendorConfigArgs(content string) ([]string, error) {
	runes := []rune(strings.TrimSpace(content))
	var args []string
	for i := 0; i < len(runes); {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		if runes[i] == '[' || runes[i] == '{' {
			args = append(args, strings.TrimSpace(string(runes[i:])))
			break
		}
		var value strings.Builder
		quote := rune(0)
		if runes[i] == '\'' || runes[i] == '"' {
			quote = runes[i]
			i++
		}
		closed := quote == 0
		for i < len(runes) {
			char := runes[i]
			if quote == 0 && unicode.IsSpace(char) {
				break
			}
			if quote != 0 && char == quote {
				i++
				closed = true
				if i < len(runes) && !unicode.IsSpace(runes[i]) {
					return nil, errors.New("quoted value must end at whitespace")
				}
				break
			}
			if quote != 0 && char == '\\' && i+1 < len(runes) && runes[i+1] == quote {
				i++
				char = runes[i]
			}
			value.WriteRune(char)
			i++
		}
		if !closed {
			return nil, errors.New("unterminated quote")
		}
		args = append(args, value.String())
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	return args, nil
}
