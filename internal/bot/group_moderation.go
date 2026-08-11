package bot

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type groupJoinApprovalAPI interface {
	ReviewGroupJoinRequest(context.Context, string, string, string, string, string, bool) error
}

type groupMuteAPI interface {
	GetGroupMuteState(context.Context, string) (qq.GroupMuteState, error)
	SetGroupMemberMute(context.Context, string, string, string, time.Time) error
}

func (s *Service) handleJoinCommand(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string, content string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有配置入群自动审批的权限。")
	}
	group := strings.TrimSpace(event.Message.GroupOpenID)
	if group == "" {
		return s.reply(ctx, event, "该指令只能在群聊中使用。")
	}
	if len(fields) < 2 {
		return s.reply(ctx, event, joinCommandUsage())
	}
	setting, err := s.store.GetGroupJoinApproval(group)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return s.reply(ctx, event, "读取入群自动审批设置失败。")
	}
	setting.GroupOpenID = group
	switch strings.ToLower(fields[1]) {
	case "status":
		if len(fields) != 2 {
			return s.reply(ctx, event, joinCommandUsage())
		}
		status := "关闭"
		if setting.Enabled {
			status = "开启"
		}
		levelText := "不限制"
		if setting.MinQQLevel > 0 {
			levelText = fmt.Sprintf("至少 %d 级（事件缺少等级时转人工审核）", setting.MinQQLevel)
		}
		matchText := "不限制"
		if setting.MatchText != "" {
			matchText = strconv.Quote(setting.MatchText)
		}
		return s.reply(ctx, event, fmt.Sprintf("当前群 New API 账户入群自动审批：%s。\nQQ 等级限制：%s。\n申请内容匹配：%s。\n申请还必须匹配已启用的 New API 账户，且 QQ 未返回安全提示。", status, levelText, matchText))
	case "on":
		if len(fields) != 2 {
			return s.reply(ctx, event, joinCommandUsage())
		}
		setting.Enabled = true
	case "off":
		if len(fields) != 2 {
			return s.reply(ctx, event, joinCommandUsage())
		}
		setting.Enabled = false
	case "limit":
		if len(fields) != 3 {
			return s.reply(ctx, event, "格式错误。正确用法：/join limit <非负QQ等级数>；设为 0 表示不限制。")
		}
		level, parseErr := strconv.Atoi(fields[2])
		if parseErr != nil || level < 0 {
			return s.reply(ctx, event, "QQ 等级必须是非负整数；设为 0 表示不限制。")
		}
		setting.MinQQLevel = level
	case "check":
		matchText, parseErr := parseJoinCheckText(content)
		if parseErr != nil {
			return s.reply(ctx, event, "格式错误。正确用法：/join check \"<匹配字符串>\"；设为 \"\" 表示不限制。")
		}
		setting.MatchText = matchText
	default:
		return s.reply(ctx, event, joinCommandUsage())
	}
	if err := s.store.PutGroupJoinApproval(setting); err != nil {
		return s.reply(ctx, event, "保存入群自动审批设置失败。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "qq.join_approval.configure", Target: group, Success: true, Metadata: map[string]any{"enabled": setting.Enabled, "min_qq_level": setting.MinQQLevel, "match_text_configured": setting.MatchText != ""}})
	if strings.EqualFold(fields[1], "limit") {
		if setting.MinQQLevel == 0 {
			return s.reply(ctx, event, "当前群入群自动审批已取消 QQ 等级限制。")
		}
		return s.reply(ctx, event, fmt.Sprintf("当前群入群自动审批已设置为仅允许 QQ 等级至少 %d 级的申请；事件未提供等级时将等待人工审核。", setting.MinQQLevel))
	}
	if strings.EqualFold(fields[1], "check") {
		if setting.MatchText == "" {
			return s.reply(ctx, event, "当前群入群自动审批已取消申请内容匹配限制。")
		}
		return s.reply(ctx, event, "当前群入群自动审批仅允许申请内容包含 "+strconv.Quote(setting.MatchText)+" 的申请。")
	}
	if setting.Enabled {
		return s.reply(ctx, event, "当前群 New API 账户入群自动审批已开启。机器人必须是群管理员，并在 QQ 开放平台接收 GROUP_JOIN_REQUEST 事件。")
	}
	return s.reply(ctx, event, "当前群 New API 账户入群自动审批已关闭，后续申请将等待人工审核。")
}

func joinCommandUsage() string {
	return "格式错误。正确用法：/join on、/join off、/join status、/join limit <QQ等级数> 或 /join check \"<匹配字符串>\""
}

func parseJoinCheckText(content string) (string, error) {
	content = strings.TrimSpace(content)
	fields := strings.Fields(content)
	if len(fields) < 3 || !strings.EqualFold(fields[0], "/join") || !strings.EqualFold(fields[1], "check") {
		return "", errors.New("invalid join check command")
	}
	remainder := strings.TrimSpace(content[len(fields[0]):])
	if len(remainder) < len(fields[1]) || !strings.EqualFold(remainder[:len(fields[1])], fields[1]) {
		return "", errors.New("invalid join check command")
	}
	argument := strings.TrimSpace(remainder[len(fields[1]):])
	if len(argument) < 2 || argument[0] != '"' || argument[len(argument)-1] != '"' {
		return "", errors.New("join check argument must be quoted")
	}
	value, err := strconv.Unquote(argument)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *Service) handleGroupJoinRequest(ctx context.Context, event qq.MessageEvent) {
	request := event.JoinRequest
	setting, err := s.store.GetGroupJoinApproval(request.GroupOpenID)
	if err != nil || !setting.Enabled {
		s.logger.Info("收到入群申请，当前群未开启自动审批", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID)
		return
	}
	if request.AutoApproved != nil || request.Bot || strings.TrimSpace(request.RiskTips) != "" {
		s.logger.Info("入群申请不满足自动审批前置条件",
			"group_openid", request.GroupOpenID,
			"member_openid", request.MemberOpenID,
			"already_auto_approved", request.AutoApproved != nil,
			"applicant_is_bot", request.Bot,
			"risk_tips_present", strings.TrimSpace(request.RiskTips) != "",
		)
		return
	}
	if setting.MinQQLevel > 0 {
		level, present := request.UserLevel()
		if !present {
			s.logger.Info("入群申请事件缺少 QQ 等级，等待人工审核", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID, "required_qq_level", setting.MinQQLevel)
			return
		}
		if level < setting.MinQQLevel {
			s.logger.Info("入群申请 QQ 等级不足，等待人工审核", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID, "qq_level", level, "required_qq_level", setting.MinQQLevel)
			return
		}
	}
	if setting.MatchText != "" && !joinRequestContains(request, setting.MatchText) {
		s.logger.Info("入群申请内容未匹配自动审批字符串，等待人工审核", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID)
		return
	}
	api, ok := s.qq.(groupJoinApprovalAPI)
	if !ok {
		s.logger.Warn("当前 QQ 客户端不支持入群申请审批")
		return
	}
	user, matched, lookupErr := s.matchJoinRequestUser(ctx, request)
	if lookupErr != nil {
		s.logger.Warn("校验入群申请对应的 New API 用户失败", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID, "error", publicError(lookupErr))
		return
	}
	if !matched || user.Status != 1 {
		s.logger.Info("入群申请未匹配已启用的 New API 用户，等待人工审核", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID)
		return
	}
	err = api.ReviewGroupJoinRequest(ctx, request.GroupOpenID, request.MemberOpenID, request.JoinRequestID, "approve", "", false)
	_ = s.store.AddAudit(model.AuditRecord{
		At: time.Now(), Actor: "system:join-approval", Action: "qq.join_request.approve", Target: request.MemberOpenID, Success: err == nil,
		Description: publicQQError(err), Metadata: map[string]any{"group_openid": request.GroupOpenID, "newapi_user_id": user.ID, "apply_source": request.ApplySource, "verify_method": request.VerifyInfo.Method},
	})
	if err != nil {
		s.logger.Warn("自动通过入群申请失败", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID, "newapi_user_id", user.ID, "error", err)
		return
	}
	s.logger.Info("已自动通过匹配 New API 账户的入群申请", "group_openid", request.GroupOpenID, "member_openid", request.MemberOpenID, "newapi_user_id", user.ID)
}

func joinRequestContains(request qq.GroupJoinRequest, matchText string) bool {
	if strings.Contains(request.VerifyInfo.VerifyMessage, matchText) {
		return true
	}
	for _, item := range request.VerifyInfo.ReviewQAList {
		if strings.Contains(item.Answer, matchText) {
			return true
		}
	}
	return false
}

func (s *Service) matchJoinRequestUser(ctx context.Context, request qq.GroupJoinRequest) (newapi.User, bool, error) {
	answers := make([]string, 0, 1+len(request.VerifyInfo.ReviewQAList))
	if value := strings.TrimSpace(request.VerifyInfo.VerifyMessage); value != "" {
		answers = append(answers, value)
	}
	for _, item := range request.VerifyInfo.ReviewQAList {
		if value := strings.TrimSpace(item.Answer); value != "" {
			answers = append(answers, value)
		}
	}
	var lastErr error
	for _, answer := range answers {
		if id, err := strconv.Atoi(answer); err == nil && id > 0 {
			user, err := s.newAPI.GetUser(ctx, id)
			if err == nil && user.ID == id {
				return user, true, nil
			}
			if err != nil {
				lastErr = err
			}
			continue
		}
		address, err := mail.ParseAddress(answer)
		if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), answer) {
			continue
		}
		user, err := s.newAPI.FindUserByEmail(ctx, strings.ToLower(address.Address))
		if err == nil && user.ID > 0 && strings.EqualFold(strings.TrimSpace(user.Email), address.Address) {
			return user, true, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return newapi.User{}, false, lastErr
	}
	return newapi.User{}, false, nil
}

func (s *Service) handleMute(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有管理群成员禁言的权限。")
	}
	group := strings.TrimSpace(event.Message.GroupOpenID)
	if group == "" {
		return s.reply(ctx, event, "禁言指令只能在群聊中使用。")
	}
	api, ok := s.qq.(groupMuteAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持群禁言接口。")
	}
	if len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		state, err := api.GetGroupMuteState(ctx, group)
		if err != nil {
			return s.reply(ctx, event, "查询群禁言状态失败："+publicQQError(err))
		}
		lines := []string{fmt.Sprintf("群禁言状态：全员模式 %s，当前成员禁言 %d 人。", nonEmpty(state.GlobalRule.Mode, "none"), len(state.Members))}
		for index, member := range state.Members {
			if index >= 10 {
				lines = append(lines, fmt.Sprintf("另有 %d 人未显示。", len(state.Members)-index))
				break
			}
			lines = append(lines, fmt.Sprintf("%s：至 %s", nonEmpty(member.Username, maskOpenID(member.MemberOpenID)), formatRFC3339(member.MuteExpireAt, s.cfg.CheckinTimezone)))
		}
		return s.reply(ctx, event, strings.Join(lines, "\n"))
	}
	if len(fields) == 3 && strings.EqualFold(fields[1], "off") {
		member, label, err := resolveMuteTarget(event, fields[2])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		if err := api.SetGroupMemberMute(ctx, group, member, "del", time.Time{}); err != nil {
			return s.reply(ctx, event, "解除禁言失败："+publicQQError(err))
		}
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "qq.mute.remove", Target: member, Success: true, Metadata: map[string]any{"group_openid": group}})
		return s.reply(ctx, event, "已解除"+label+"的群禁言。")
	}
	if len(fields) != 3 {
		return s.reply(ctx, event, "格式错误。正确用法：/mute <@成员或member_openid> <时长>、/mute off <@成员> 或 /mute status")
	}
	member, label, err := resolveMuteTarget(event, fields[1])
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	duration, err := parseMuteDuration(fields[2])
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	operation := "add"
	if state, stateErr := api.GetGroupMuteState(ctx, group); stateErr == nil {
		for _, muted := range state.Members {
			if muted.MemberOpenID == member {
				operation = "update"
				break
			}
		}
	}
	expiresAt := time.Now().Add(duration)
	if err := api.SetGroupMemberMute(ctx, group, member, operation, expiresAt); err != nil {
		return s.reply(ctx, event, "设置禁言失败："+publicQQError(err))
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "qq.mute.set", Target: member, Success: true, Metadata: map[string]any{"group_openid": group, "duration": duration.String(), "expires_at": expiresAt.Format(time.RFC3339)}})
	return s.reply(ctx, event, fmt.Sprintf("已将%s禁言 %s，预计于 %s 解除。", label, humanDuration(duration), expiresAt.In(s.cfg.CheckinTimezone).Format("2006-01-02 15:04 MST")))
}

func resolveMuteTarget(event qq.MessageEvent, token string) (string, string, error) {
	mention, err := selectTargetMention(event.Message.Mentions, token)
	if err != nil {
		return "", "", err
	}
	member := firstNonEmpty(mention.MemberOpenID, mention.ID, mention.UserOpenID)
	if member == "" {
		return "", "", errors.New("QQ 事件未提供目标用户的 member_openid。")
	}
	label := "成员 " + maskOpenID(member)
	if strings.TrimSpace(mention.Username) != "" {
		label = "成员 " + mention.Username
	}
	return member, label, nil
}

func parseMuteDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	var duration time.Duration
	var err error
	if strings.HasSuffix(value, "d") {
		days, parseErr := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if parseErr != nil || days <= 0 {
			return 0, errors.New("禁言时长格式无效，例如 10m、2h 或 3d。")
		}
		duration = time.Duration(days) * 24 * time.Hour
	} else {
		duration, err = time.ParseDuration(value)
		if err != nil {
			return 0, errors.New("禁言时长格式无效，例如 10m、2h 或 3d。")
		}
	}
	if duration < time.Minute || duration > 30*24*time.Hour {
		return 0, errors.New("禁言时长必须在 1 分钟到 30 天之间。")
	}
	return duration, nil
}

func humanDuration(value time.Duration) string {
	if value%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d 天", int(value/(24*time.Hour)))
	}
	if value%time.Hour == 0 {
		return fmt.Sprintf("%d 小时", int(value/time.Hour))
	}
	return fmt.Sprintf("%d 分钟", int(value/time.Minute))
}

func formatRFC3339(value string, location *time.Location) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nonEmpty(value, "未知")
	}
	if location == nil {
		location = time.Local
	}
	return parsed.In(location).Format("2006-01-02 15:04 MST")
}

func maskOpenID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 10 {
		return value
	}
	return value[:6] + "…" + value[len(value)-4:]
}
