package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fsykk/new-api-bot/internal/llm"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type llmOverrides map[string]json.RawMessage

func (s *Service) llmOverrides() (llmOverrides, error) {
	text, err := s.store.LLMConfig()
	if errors.Is(err, store.ErrNotFound) {
		return llmOverrides{}, nil
	}
	if err != nil {
		return nil, err
	}
	text, err = s.secure.Decrypt(text)
	if err != nil {
		return nil, errors.New("LLM 配置解密失败")
	}
	var values llmOverrides
	if err = json.Unmarshal([]byte(text), &values); err != nil {
		return nil, errors.New("LLM 配置格式损坏")
	}
	if values == nil {
		values = llmOverrides{}
	}
	return values, nil
}
func (s *Service) llmConfigSnapshot() (llm.Config, error) {
	s.llmConfigMu.Lock()
	defer s.llmConfigMu.Unlock()
	return s.llmConfigSnapshotLocked()
}
func (s *Service) llmConfigSnapshotLocked() (llm.Config, error) {
	cfg := s.cfg.LLM
	values := cfg.Values()
	overrides, err := s.llmOverrides()
	if err != nil {
		return cfg, err
	}
	for key, v := range overrides {
		if _, ok := values[key]; !ok || !json.Valid(v) {
			return cfg, errors.New("无效 LLM 配置覆盖")
		}
		values[key] = v
	}
	b, _ := json.Marshal(values)
	if json.Unmarshal(b, &cfg) != nil {
		return cfg, errors.New("LLM 配置类型损坏")
	}
	return cfg, cfg.Validate()
}
func (s *Service) handleLLMConfig(ctx context.Context, event qq.MessageEvent, content string) error {
	identity := identityFromEvent(event)
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "仅 Bot 管理员可查看或修改 LLM 配置。")
	}
	args, err := splitVendorConfigArgs(content)
	if err != nil {
		return s.reply(ctx, event, "参数格式错误，请使用带引号的完整值。")
	}
	args = args[1:]
	if len(args) == 1 && strings.EqualFold(args[0], "help") {
		return s.replyHelp(ctx, event, s.helpTextFor(identity, "/llm_config")+"\n"+llmConfigKeyHelp())
	}
	if len(args) == 2 && strings.EqualFold(args[1], "help") && (strings.EqualFold(args[0], "show") || strings.EqualFold(args[0], "set") || strings.EqualFold(args[0], "reset")) {
		return s.replyHelp(ctx, event, s.helpTextFor(identity, "/llm_config "+strings.ToLower(args[0]))+"\n"+llmConfigKeyHelp())
	}
	if len(args) == 0 {
		args = []string{"show"}
	}
	action := strings.ToLower(args[0])
	if action == "show" {
		if len(args) > 2 {
			return s.reply(ctx, event, "用法：/llm_config show [key]")
		}
		cfg, err := s.llmConfigSnapshot()
		if err != nil {
			return s.reply(ctx, event, "LLM 配置读取或解密失败。")
		}
		keys := llm.Keys()
		if len(args) == 2 {
			if _, ok := cfg.Values()[args[1]]; !ok {
				return s.reply(ctx, event, "未知配置键。可用键："+strings.Join(keys, ", "))
			}
			keys = []string{args[1]}
		}
		var lines []string
		for _, key := range keys {
			value := string(cfg.Values()[key])
			if llm.Secret(key) {
				if value == `""` {
					value = "未配置"
				} else {
					value = "已配置（不回显）"
				}
			}
			lines = append(lines, key+" = "+s.llmSafeText(value, cfg))
		}
		return s.sendLLMResult(ctx, event, strings.Join(lines, "\n"), llm.DefaultConfig())
	}
	if s.isReadOnlyAdmin(identity) {
		return s.reply(ctx, event, "只读管理员仅可查看 LLM 配置。")
	}
	if action != "set" && action != "reset" {
		return s.reply(ctx, event, "用法：/llm_config show [key]；set <key> <value>；reset <key|all>")
	}
	if (action == "set" && len(args) != 3) || (action == "reset" && len(args) != 2) {
		return s.reply(ctx, event, "参数格式错误；提示词请用引号包裹，空提示词使用 \"\"。")
	}
	key := args[1]
	if llm.Secret(key) && event.EventType != "C2C_MESSAGE_CREATE" {
		return s.reply(ctx, event, "密钥仅允许管理员在单聊中设置或重置。")
	}
	value := ""
	if action == "set" {
		value = args[2]
	}
	if err = s.updateLLMConfig(action, key, value); err != nil {
		return s.reply(ctx, event, err.Error())
	}
	_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: commandRuleActor(identity), Action: "llm.config." + action, Target: key, Success: true})
	s.wakeLLM()
	return s.reply(ctx, event, "LLM 配置 "+key+" 已更新；新对话使用新设置，密钥不会回显。")
}

// Keep network replies outside the config mutex. Configuration changes must
// not hold up all model authorization checks while QQ responds slowly.
func (s *Service) updateLLMConfig(action, key, value string) error {
	s.llmConfigMu.Lock()
	defer s.llmConfigMu.Unlock()
	cfg := s.cfg.LLM
	overrides := llmOverrides{}
	var err error
	if action != "reset" || key != "all" {
		cfg, err = s.llmConfigSnapshotLocked()
		if err != nil {
			return errors.New("LLM 配置读取失败，可使用 /llm_config reset all 恢复部署配置。")
		}
		overrides, err = s.llmOverrides()
		if err != nil {
			return errors.New("LLM 配置读取失败。")
		}
	}
	if action == "set" {
		if err = cfg.Set(key, value); err != nil {
			return err
		}
		overrides[key] = cfg.Values()[key]
	} else {
		if key == "all" {
			overrides = llmOverrides{}
			cfg = s.cfg.LLM
		} else {
			if _, ok := cfg.Values()[key]; !ok {
				return errors.New("未知配置键。")
			}
			delete(overrides, key)
			values := s.cfg.LLM.Values()
			for k, v := range overrides {
				values[k] = v
			}
			b, _ := json.Marshal(values)
			_ = json.Unmarshal(b, &cfg)
		}
		if err = cfg.Validate(); err != nil {
			return fmt.Errorf("重置会导致预算配置不一致：%w", err)
		}
	}
	b, _ := json.Marshal(overrides)
	encrypted, err := s.secure.Encrypt(string(b))
	if err == nil {
		err = s.store.PutLLMConfig(encrypted)
	}
	if err != nil {
		return errors.New("保存 LLM 配置失败。")
	}
	return nil
}

func llmHelpEntries() []helpEntry {
	return []helpEntry{
		{"/chat", "<内容>", "LLM 对话（已绑定用户）；群内也可 @机器人", false, false},
		{"/chat status", "", "查看对话配置及本人剩余次数", false, false},
		{"/chat reset", "", "清空当前历史；群聊需可写管理员", false, false},
		{"/chat on", "", "开启当前群对话，无需绑定", true, false},
		{"/chat off", "", "关闭当前群对话，无需绑定", true, false},
		{"/llm_config", "", "管理 LLM 配置与人格，无需绑定", true, true},
		{"/llm_config show", "[key]", "查询配置；密钥不回显", true, false},
		{"/llm_config set", "<key> <value>", "持久化热更新；密钥只允许单聊设置", true, false},
		{"/llm_config reset", "<key|all>", "恢复部署默认，不清除历史或群开关", true, false},
	}
}
func llmConfigWrite(fields []string) bool {
	return len(fields) > 1 && (strings.EqualFold(fields[1], "set") || strings.EqualFold(fields[1], "reset"))
}
func llmConfigKeyHelp() string {
	return fmt.Sprintf("可用配置键：%s", strings.Join(llm.Keys(), ", "))
}
