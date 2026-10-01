package bot

import (
	"context"
	"strings"

	"github.com/fsykk/new-api-bot/internal/config"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

// A path contains command keywords only; arguments never create help levels.
// Containers are navigation entries, not executable commands.
type helpEntry struct {
	path        string
	args        string
	description string
	adminOnly   bool
	container   bool
}

func commandHelpEntries(cfg config.Config) []helpEntry {
	entries := []helpEntry{
		{"/help", "", "按权限查看一级命令帮助", false, false},
		{"/bind", "<邮箱或用户ID>", "在当前群发送绑定验证码", false, false},
		{"/bind verify", "<验证码>", "在当前群完成绑定", false, false},
		{"/bind status", "", "查看当前绑定信息", false, false},
		{"/unbind", "", "解除当前 QQ 身份绑定", false, false},
		{"/hongbao", "", "领取当前群红包，每个账户每轮限领一次", false, false},
		{"/hongbao new", "<总额度> <数量> [分组限制 ...]", "发放拼手气额度红包，多个分组以空格分隔", true, false},
		{"/hongbao stop", "", "停止当前群红包，已发放额度不受影响", true, false},
		{"/me", "", "查看账户与额度", false, false},
		{"/usage", "[时间长度]", "查看自己的用量，时间长度如 7d", false, false},
		{"/usage", "<时间长度> all", "查看全站请求、Token 与额度汇总", false, false},
		{"/usage", "<时间长度> <前N名>", "查看用量排行榜", false, false},
		{"/usage", "<用户ID或@用户> <时间长度>", "查看指定用户的用量", true, false},
		{"/logs", "[数量]", "查看自己的最近调用记录", false, false},
		{"/logs", "<用户ID或@用户> [数量]", "查看指定用户的最近调用记录", true, false},
		{"/models", "", "查看自己分组的可用模型", false, false},
		{"/models", "<用户ID或@用户>", "查看指定用户分组的可用模型", true, false},
		{"/plan", "", "查看或管理订阅", false, true},
		{"/plan view", "", "查看自己的全部订阅", false, false},
		{"/plan view", "<用户ID或@用户>", "查看指定用户的全部订阅", true, false},
		{"/plan add", "<套餐ID> <用户ID或@用户>", "给指定用户添加订阅", true, false},
		{"/plan sub", "<订阅编号> <用户ID或@用户>", "取消指定用户的订阅", true, false},
		{"/whoami", "", "查看当前 OpenID", false, false},
		{"/enable", "\"<关键词>\"", "管理命令关键词启用状态", true, false},
		{"/enable list", "", "查看明确启用的命令关键词", false, false},
		{"/disable", "\"<关键词>\"", "管理命令关键词禁用状态", true, false},
		{"/disable list", "", "查看明确禁用的命令关键词", false, false},
		{"/credit", "", "查询或管理用户额度", true, true},
		{"/credit show", "<用户ID或@用户>", "查询用户额度", true, false},
		{"/credit add", "<用户ID或@用户> <额度>", "增加用户额度", true, false},
		{"/credit sub", "<用户ID或@用户> <额度>", "扣除用户额度，不允许扣成负数", true, false},
		{"/admin", "", "查看管理报表或管理用户绑定", true, true},
		{"/admin bindings", "[页码]", "分页查看绑定列表", true, false},
		{"/admin unbind", "<用户ID或@用户>", "解除指定用户的绑定", true, false},
		{"/admin checkin", "", "查看今日签到统计与动态发放规则", true, false},
		{"/admin report", "[时间长度]", "查看全站用量摘要", true, false},
		{"/welcome", "", "配置当前群的新成员欢迎", true, true},
		{"/welcome on", "", "开启当前群的新成员欢迎", true, false},
		{"/welcome off", "", "关闭当前群的新成员欢迎", true, false},
		{"/welcome set", "<欢迎语>", "设置欢迎语并自动开启", true, false},
		{"/join", "", "配置当前群的入群自动审批", true, true},
		{"/join status", "", "查看当前群入群自动审批设置", true, false},
		{"/join on", "", "开启入群自动审批", true, false},
		{"/join off", "", "关闭入群自动审批", true, false},
		{"/join limit", "<QQ等级>", "设置自动审批最低 QQ 等级，0 表示不限制", true, false},
		{"/join check", "\"<匹配字符串>\"", "设置申请内容匹配条件，空字符串表示不限制", true, false},
		{"/mute", "<@成员或member_openid> <时长>", "管理群成员禁言", true, false},
		{"/mute off", "<@成员或member_openid>", "解除指定成员的禁言", true, false},
		{"/mute status", "", "查看群禁言状态", true, false},
		{"/recall", "[消息ID]", "撤回两分钟内的机器人消息，也可回复消息后执行", true, false},
		{"/bot", "", "查看机器人与群聊状态", false, true},
		{"/bot status", "", "查看机器人与群聊状态", false, false},
		{"/vendor_status", "", "查询全部启用厂商的最新状态总览，无需绑定", false, false},
		{"/vendor_subscribe", "", "配置当前群的厂商告警订阅，无需绑定", true, true},
		{"/vendor_subscribe on", "", "开启当前群的厂商告警", true, false},
		{"/vendor_subscribe off", "", "关闭当前群的厂商告警", true, false},
		{"/vendor_config", "", "查看或配置厂商监控，无需绑定", true, false},
		{"/vendor_config show", "[key]", "显示当前有效设置", true, false},
		{"/vendor_config get", "[key]", "显示当前有效设置（show 的别名）", true, false},
		{"/vendor_config list", "[key]", "显示当前有效设置（show 的别名）", true, false},
		{"/vendor_config reset", "<key|all>", "清除命令覆盖，恢复部署配置", true, false},
		{"/vendor_config set", "<key> <value>", "持久化修改厂商配置", true, false},
	}
	if cfg.CheckinEnabled {
		entries = append(entries,
			helpEntry{"/checkin", "", "签到并直接增加绑定账户额度", false, false},
			helpEntry{"/checkin status", "", "查看签到状态", false, false},
			helpEntry{"/checkin reset", "", "重置当前周期所有用户的签到状态", true, false},
		)
	}
	if cfg.UsageChartEnabled {
		entries = append(entries,
			helpEntry{"/usage chart", "[时间长度] [all]", "生成自己的用量图表，all 汇总本群已绑定成员", false, false},
			helpEntry{"/usage chart", "<时间长度> <用户ID或@用户>", "生成指定用户的用量图表", true, false},
		)
	}
	if cfg.NotifyEnabled {
		entries = append(entries,
			helpEntry{"/notify", "", "配置额度提醒与每日用量摘要", false, true},
			helpEntry{"/notify quota", "<额度>", "设置低额度提醒阈值", false, false},
			helpEntry{"/notify quota off", "", "关闭低额度提醒", false, false},
			helpEntry{"/notify daily", "", "配置每日用量摘要", false, true},
			helpEntry{"/notify daily on", "", "开启每日用量摘要", false, false},
			helpEntry{"/notify daily off", "", "关闭每日用量摘要", false, false},
			helpEntry{"/notify status", "", "查看提醒设置", false, false},
		)
	}
	if cfg.AdminReportExportEnabled {
		entries = append(entries, helpEntry{"/admin report export", "[时间长度]", "导出 CSV 全站报表", true, false})
	}
	if cfg.AdminUserManagementEnabled {
		entries = append(entries,
			helpEntry{"/admin user", "", "查询或管理 New API 用户状态", true, true},
			helpEntry{"/admin user status", "<用户ID或@用户>", "查看用户状态、角色和分组", true, false},
			helpEntry{"/admin user enable", "<用户ID或@用户>", "启用用户", true, false},
			helpEntry{"/admin user disable", "<用户ID或@用户>", "生成禁用用户的一次性确认码", true, false},
			helpEntry{"/admin user reset2fa", "<用户ID或@用户>", "生成重置用户 2FA 的确认码", true, false},
			helpEntry{"/admin user resetpasskey", "<用户ID或@用户>", "生成重置用户 Passkey 的确认码", true, false},
			helpEntry{"/confirm", "<一次性操作码>", "确认敏感管理操作", true, false},
		)
	}
	if cfg.BenefitEnabled {
		entries = append(entries, helpEntry{"/benefit", "<面额> <数量> <有效期(h)> <封禁时间(day)>", "发放一人限领一个的福利兑换码", true, false})
	}
	if cfg.ResetEnabled {
		entries = append(entries,
			helpEntry{"/reset", "", "查看重置状态或参与重置补偿抽奖", false, true},
			helpEntry{"/reset check", "", "查看当前群的重置状态", false, false},
			helpEntry{"/reset status", "", "查看当前群的重置状态（check 的别名）", false, false},
			helpEntry{"/reset last", "", "查看 Codex Reset API 最新重置事件及状态", false, false},
			helpEntry{"/reset join", "", "参加当前群正在进行的重置补偿抽奖", false, false},
			helpEntry{"/reset new", "", "按当前群设置手动开启新活动", true, false},
			helpEntry{"/reset stop", "", "停止当前活动，不抽奖、不发放额度", true, false},
			helpEntry{"/reset end", "", "提前截止当前活动并立即结算", true, false},
			helpEntry{"/reset set", "", "设置下一轮重置补偿活动", true, true},
			helpEntry{"/reset set duration", "<时长>", "设置下一轮活动有效期", true, false},
			helpEntry{"/reset set winners", "<人数>", "设置下一轮抽取人数", true, false},
			helpEntry{"/reset set lookback", "<时长>", "设置下一轮补偿回溯时间", true, false},
			helpEntry{"/reset proxy", "<代理链接|off>", "设置 Codex Reset API 检测代理", true, false},
		)
	}
	// Use the same option registry as the vendor configuration parser.
	for _, option := range vendorstatus.Options {
		args := option.Example
		switch option.Kind {
		case "sources":
			args = "<JSON对象>"
		case "custom", "groups":
			args = "<JSON数组>"
		}
		for _, key := range append([]string{option.Key}, option.Aliases...) {
			entries = append(entries, helpEntry{"/vendor_config " + key, "", "查看" + option.Label, true, false})
			if !option.ReadOnly {
				entries = append(entries, helpEntry{"/vendor_config " + key, args, "修改" + option.Label, true, false})
			}
		}
	}
	for _, id := range vendorstatus.SourceIDs {
		for _, path := range []string{"/vendor_config sources " + id, "/vendor_config sources." + id} {
			entries = append(entries,
				helpEntry{path, "", "查看状态源 " + id, true, false},
				helpEntry{path, "on|off", "开启或关闭状态源 " + id, true, false},
			)
		}
	}
	for _, path := range []string{"/vendor_config custom", "/vendor_config custom_statuspage_sources"} {
		entries = append(entries,
			helpEntry{path + " add", "\"<name>\" <url> [on|off]", "添加自定义状态源", true, false},
			helpEntry{path + " remove", "\"<name>\"", "删除自定义状态源", true, false},
			helpEntry{path + " del", "\"<name>\"", "删除自定义状态源（remove 的别名）", true, false},
			helpEntry{path + " set", "\"<name>\" name|base_url|enabled <value>", "修改自定义状态源字段", true, false},
			helpEntry{path + " enable", "\"<name>\"", "开启自定义状态源", true, false},
			helpEntry{path + " disable", "\"<name>\"", "关闭自定义状态源", true, false},
			helpEntry{path + " clear", "", "清空自定义状态源", true, false},
		)
	}
	entries = append(entries,
		helpEntry{"/vendor_config group_whitelist add", "<group_openid>", "添加告警群白名单", true, false},
		helpEntry{"/vendor_config group_whitelist remove", "<group_openid>", "删除告警群白名单", true, false},
		helpEntry{"/vendor_config group_whitelist del", "<group_openid>", "删除告警群白名单（remove 的别名）", true, false},
		helpEntry{"/vendor_config group_whitelist clear", "", "清空告警群白名单", true, false},
	)
	return entries
}

func (entry helpEntry) usage() string {
	if entry.args == "" {
		return entry.path
	}
	return entry.path + " " + entry.args
}

func helpDescendant(path, parent string) bool {
	return parent == "" || strings.HasPrefix(path, parent+" ")
}

func (s *Service) replyHelp(ctx context.Context, event qq.MessageEvent, text string) error {
	for _, chunk := range splitHelpText(text, 1500) {
		if err := s.replyWithAutoRecall(ctx, event, chunk); err != nil {
			return err
		}
		// Further chunks are independent messages. Recall the original command
		// only once, but keep automatic recall for each help reply.
		event.Message.ID = ""
	}
	return nil
}

// Keep complete command lines together instead of splitting their arguments.
func splitHelpText(text string, maxRunes int) []string {
	var chunks []string
	current := ""
	for _, line := range strings.Split(text, "\n") {
		candidate := line
		if current != "" {
			candidate = current + "\n" + line
		}
		if len([]rune(candidate)) <= maxRunes {
			current = candidate
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
		}
		parts := splitMessage(line, maxRunes)
		if len(parts) == 0 {
			current = ""
			continue
		}
		chunks = append(chunks, parts[:len(parts)-1]...)
		current = parts[len(parts)-1]
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func (s *Service) helpTextFor(identity model.QQIdentity, parent string) string {
	parent = strings.ToLower(strings.Join(strings.Fields(parent), " "))
	entries := commandHelpEntries(s.cfg)
	keywords, err := s.disabledCommandKeywords()
	if err != nil {
		s.logger.Error("读取禁用命令关键词失败；返回未过滤帮助", "error", err)
	}
	admin, readOnly := s.isAdmin(identity), s.isReadOnlyAdmin(identity)
	visible := make(map[string]bool)
	allowed := make([]helpEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.adminOnly && !admin {
			continue
		}
		// Rule-management commands are exempt from disabled rules in process.
		if !strings.HasPrefix(entry.path, "/enable") && !strings.HasPrefix(entry.path, "/disable") {
			hidden := false
			for _, keyword := range keywords {
				if strings.Contains(normalizeCommandFilter(entry.usage()), keyword) {
					hidden = true
					break
				}
			}
			if hidden {
				continue
			}
		}
		if entry.container {
			continue
		}
		fields := strings.Fields(entry.usage())
		if readOnly && readOnlyAdminWriteCommand(fields[0], fields) {
			continue
		}
		allowed = append(allowed, entry)
		// Keep a parent menu only when an executable descendant is available.
		for path := entry.path; path != ""; {
			visible[path] = true
			index := strings.LastIndexByte(path, ' ')
			if index < 0 {
				break
			}
			path = path[:index]
		}
	}
	if parent != "" && !visible[parent] {
		return "没有可用的命令帮助，请使用 /help 查看可用指令。"
	}
	title := "可用一级指令："
	if parent != "" {
		title = parent + " 帮助："
	}
	lines := []string{title}
	if parent != "" {
		for _, entry := range allowed {
			if entry.path == parent {
				lines = append(lines, "用法："+entry.usage()+" - "+entry.description)
			}
		}
	}
	shown := make(map[string]bool)
	var nextHelp []string
	for _, entry := range entries {
		if !visible[entry.path] || !helpDescendant(entry.path, parent) {
			continue
		}
		tail := strings.TrimPrefix(entry.path, parent)
		if strings.Contains(strings.TrimSpace(tail), " ") || shown[entry.path] {
			continue
		}
		shown[entry.path] = true
		description := entry.description
		// Do not advertise a denied root action when only its children are usable.
		if !admin && (entry.path == "/enable" || entry.path == "/disable") {
			description = "查看命令关键词状态"
		}
		if !admin || readOnly {
			switch entry.path {
			case "/plan":
				description = "查看账户订阅"
			case "/bind":
				if readOnly {
					description = "查看当前绑定信息"
				}
			case "/checkin":
				if readOnly {
					description = "查看签到状态"
				}
			case "/credit":
				description = "查询用户额度"
			case "/admin":
				description = "查看管理统计、报表与用户信息"
			case "/admin user":
				description = "查询 New API 用户状态"
			case "/join":
				description = "查看当前群的入群自动审批设置"
			case "/mute":
				description = "查看群禁言状态"
			case "/reset":
				if readOnly {
					description = "查看重置状态与最新重置事件"
				}
			}
		}
		if parent == "" {
			lines = append(lines, entry.path+" - "+description)
		} else {
			hasUsage := false
			for _, variant := range allowed {
				if variant.path == entry.path {
					lines = append(lines, variant.usage()+" - "+variant.description)
					hasUsage = true
				}
			}
			if !hasUsage {
				lines = append(lines, entry.path+" - "+description)
			}
		}
		for path := range visible {
			if helpDescendant(path, entry.path) {
				nextHelp = append(nextHelp, entry.path+" help")
				break
			}
		}
	}
	if len(shown) == 0 {
		lines = append(lines, "没有可用的下一级命令。")
	} else {
		if len(nextHelp) == 0 {
			for _, entry := range entries {
				if shown[entry.path] {
					nextHelp = append(nextHelp, entry.path+" help")
					break
				}
			}
		}
		if parent == "" {
			nextHelp = nextHelp[:1]
		}
		lines = append(lines, "查看下一级命令或详细用法：在对应命令末尾添加 help，例如 "+strings.Join(nextHelp, "、")+"。")
	}
	return strings.Join(lines, "\n")
}
