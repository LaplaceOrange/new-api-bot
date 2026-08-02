package bot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
)

const maxCommandRuleRunes = 64

func (s *Service) handleCommandRule(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, command, content string) error {
	tail := commandRuleTail(content)
	if strings.EqualFold(tail, "list") {
		return s.reply(ctx, event, s.commandRuleListText(command == "/enable"))
	}
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有管理命令启用状态的权限。")
	}
	keyword, err := parseCommandRuleKeyword(tail)
	if err != nil {
		return s.reply(ctx, event, fmt.Sprintf("格式错误。正确用法：%s \"关键词\"，或 %s list", command, command))
	}
	enabled := command == "/enable"
	actor := commandRuleActor(identity)
	if err := s.store.PutCommandRule(model.CommandRule{Keyword: keyword, Enabled: enabled, Actor: actor}); err != nil {
		return s.reply(ctx, event, "保存命令关键词状态失败，请稍后重试。")
	}
	action := "command.disable"
	message := fmt.Sprintf("已禁用包含关键词“%s”的命令；匹配到的命令将被静默忽略。", keyword)
	if enabled {
		action = "command.enable"
		message = fmt.Sprintf("已启用关键词“%s”对应的命令。", keyword)
	}
	_ = s.store.AddAudit(model.AuditRecord{
		At: time.Now(), Actor: actor, Action: action, Target: keyword, Success: true,
	})
	return s.reply(ctx, event, message)
}

func commandRuleTail(content string) string {
	content = strings.TrimSpace(content)
	if index := strings.IndexFunc(content, unicode.IsSpace); index >= 0 {
		return strings.TrimSpace(content[index:])
	}
	return ""
}

func parseCommandRuleKeyword(tail string) (string, error) {
	raw := strings.TrimSpace(tail)
	if raw == "" {
		return "", fmt.Errorf("关键词不能为空")
	}
	if raw[0] == '"' || raw[0] == '\'' {
		if len(raw) < 2 || raw[len(raw)-1] != raw[0] {
			return "", fmt.Errorf("引号不匹配")
		}
	}
	if len(raw) >= 2 && ((raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'')) {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	keyword := normalizeCommandFilter(raw)
	if keyword == "" || utf8.RuneCountInString(keyword) > maxCommandRuleRunes {
		return "", fmt.Errorf("关键词长度无效")
	}
	for _, r := range keyword {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("关键词包含控制字符")
		}
	}
	return keyword, nil
}

func normalizeCommandFilter(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "/")
	return strings.Join(strings.Fields(value), " ")
}

func commandRuleActor(identity model.QQIdentity) string {
	if candidates := identity.AdminCandidates(); len(candidates) > 0 {
		return candidates[0]
	}
	return "unknown"
}

func (s *Service) commandRuleListText(enabled bool) string {
	rules, err := s.store.ListCommandRules()
	if err != nil {
		s.logger.Error("读取命令关键词状态失败", "error", err)
		return "读取命令关键词状态失败，请稍后重试。"
	}
	state := "启用"
	if !enabled {
		state = "禁用"
	}
	lines := make([]string, 0, len(rules)+1)
	for _, rule := range rules {
		if rule.Enabled == enabled {
			lines = append(lines, fmt.Sprintf("%d. %s", len(lines)+1, rule.Keyword))
		}
	}
	if len(lines) == 0 {
		return "当前没有明确设置为" + state + "的关键词。"
	}
	return "当前" + state + "关键词：\n" + strings.Join(lines, "\n")
}

func (s *Service) disabledCommandKeywords() ([]string, error) {
	rules, err := s.store.ListCommandRules()
	if err != nil {
		return nil, err
	}
	keywords := make([]string, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			keywords = append(keywords, rule.Keyword)
		}
	}
	return keywords, nil
}

func (s *Service) matchDisabledCommand(content string) (string, bool) {
	keywords, err := s.disabledCommandKeywords()
	if err != nil {
		s.logger.Error("读取禁用命令关键词失败；本次不拦截命令", "error", err)
		return "", false
	}
	normalized := normalizeCommandFilter(content)
	for _, keyword := range keywords {
		if strings.Contains(normalized, keyword) {
			return keyword, true
		}
	}
	return "", false
}

func (s *Service) filteredHelpText() string {
	text := helpText(s.cfg)
	keywords, err := s.disabledCommandKeywords()
	if err != nil {
		s.logger.Error("读取禁用命令关键词失败；返回未过滤帮助", "error", err)
		return text
	}
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		normalized := normalizeCommandFilter(line)
		hidden := false
		for _, keyword := range keywords {
			if strings.Contains(normalized, keyword) {
				hidden = true
				break
			}
		}
		if !hidden {
			filtered = append(filtered, line)
		}
	}
	return strings.Join(filtered, "\n")
}
