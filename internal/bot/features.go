package bot

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type groupDiagnosticAPI interface {
	GetGroupBotState(context.Context, string) (qq.GroupBotState, error)
	GetGroupInfo(context.Context, string) (qq.GroupInfo, error)
	AccessToken(context.Context) (string, error)
}

type groupRecallAPI interface {
	RecallGroupMessage(context.Context, string, string) error
}
type groupFileAPI interface {
	SendGroupFile(context.Context, string, string, string, int, []byte) (qq.SentMessage, error)
}
type userAdminAPI interface {
	ManageUserStatus(context.Context, int, string) error
	ResetUser2FA(context.Context, int) error
	ResetUserPasskey(context.Context, int) error
}

func (s *Service) handleMemberAdd(ctx context.Context, event qq.MessageEvent) {
	setting, err := s.store.GetGroupWelcome(event.Member.GroupOpenID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.logger.Info("收到群成员加入事件，但当前群未开启欢迎", "group_openid", event.Member.GroupOpenID)
		} else {
			s.logger.Error("读取群欢迎设置失败", "group_openid", event.Member.GroupOpenID, "error", err)
		}
		return
	}
	if !setting.Enabled {
		s.logger.Info("收到群成员加入事件，但当前群欢迎已关闭", "group_openid", event.Member.GroupOpenID)
		return
	}
	if strings.TrimSpace(event.Member.GroupOpenID) == "" || strings.TrimSpace(event.Member.MemberOpenID) == "" {
		s.logger.Warn("群成员加入事件缺少群或成员 OpenID，无法发送带 @ 的欢迎消息", "group_openid_present", event.Member.GroupOpenID != "", "member_openid_present", event.Member.MemberOpenID != "")
		return
	}
	message := strings.TrimSpace(setting.Message)
	if message == "" {
		message = s.cfg.WelcomeDefault
	}
	message = mentionMember(event.Member.MemberOpenID) + " " + message
	if err := s.qq.ReplyGroup(ctx, event.Member.GroupOpenID, "", message); err != nil {
		s.logger.Warn("发送新成员欢迎消息失败", "group_openid", event.Member.GroupOpenID, "member_openid", event.Member.MemberOpenID, "error", err)
		return
	}
	s.logger.Info("已发送新成员欢迎消息", "group_openid", event.Member.GroupOpenID, "member_openid", event.Member.MemberOpenID)
}

func (s *Service) handleWelcome(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string, content string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有配置群欢迎消息的权限。")
	}
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "该指令只能在群聊中使用。")
	}
	unlock := s.groupSettings.Lock("welcome:" + event.Message.GroupOpenID)
	defer unlock()
	if len(fields) < 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/welcome on、/welcome off、/welcome set <欢迎语>")
	}
	setting, _ := s.store.GetGroupWelcome(event.Message.GroupOpenID)
	setting.GroupOpenID = event.Message.GroupOpenID
	switch strings.ToLower(fields[1]) {
	case "on":
		setting.Enabled = true
		if strings.TrimSpace(setting.Message) == "" {
			setting.Message = s.cfg.WelcomeDefault
		}
	case "off":
		setting.Enabled = false
	case "set":
		parts := strings.SplitN(content, " ", 3)
		if len(parts) < 3 || strings.TrimSpace(parts[2]) == "" {
			return s.reply(ctx, event, "格式错误。正确用法：/welcome set <欢迎语>")
		}
		setting.Message = strings.TrimSpace(parts[2])
		setting.Enabled = true
	default:
		return s.reply(ctx, event, "格式错误。正确用法：/welcome on、/welcome off、/welcome set <欢迎语>")
	}
	if utf8.RuneCountInString(setting.Message) > 500 {
		return s.reply(ctx, event, "欢迎语不能超过 500 个字符。")
	}
	if err := s.store.PutGroupWelcome(setting); err != nil {
		return s.reply(ctx, event, "保存欢迎消息设置失败。")
	}
	if setting.Enabled {
		return s.reply(ctx, event, "新成员欢迎已开启。当前欢迎语：\n"+setting.Message)
	}
	return s.reply(ctx, event, "新成员欢迎已关闭。")
}

func (s *Service) handleBotStatus(ctx context.Context, event qq.MessageEvent, fields []string) error {
	if len(fields) != 2 || !strings.EqualFold(fields[1], "status") {
		return s.reply(ctx, event, "格式错误。正确用法：/bot status")
	}
	lines := []string{"机器人状态诊断："}
	connected := false
	if s.gatewayConnected != nil {
		connected = s.gatewayConnected()
	}
	lines = append(lines, fmt.Sprintf("Gateway：%s", boolStatus(connected)))
	if api, ok := s.qq.(groupDiagnosticAPI); ok {
		_, tokenErr := api.AccessToken(ctx)
		lines = append(lines, "QQ Token："+boolStatus(tokenErr == nil))
		if event.Message.GroupOpenID != "" {
			state, err := api.GetGroupBotState(ctx, event.Message.GroupOpenID)
			if err != nil {
				lines = append(lines, "群内详细状态：接口未开放或查询失败（"+publicQQError(err)+"）")
			} else {
				lines = append(lines, "加入群聊时间："+formatUnixFlexible(state.JoinedAt, s.cfg.CheckinTimezone), "允许主动消息："+boolStatus(state.AllowProactiveMsg), "接收消息模式："+recvSettingText(state.RecvMsgSetting), "群内角色："+memberRoleText(state.MemberRole))
			}
			if info, err := api.GetGroupInfo(ctx, event.Message.GroupOpenID); err == nil {
				lines = append(lines, fmt.Sprintf("群信息：%s（%d 人）", nonEmpty(info.GroupName, "未命名群"), info.GroupMemberNum))
			}
		}
	} else {
		lines = append(lines, "QQ Token：客户端不支持诊断")
	}
	if status, err := s.newAPI.GetStatus(ctx, true); err != nil {
		lines = append(lines, "New API：异常（"+publicError(err)+"）")
	} else {
		lines = append(lines, fmt.Sprintf("New API：正常｜%s %s", status.SystemName, status.Version))
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}

func (s *Service) handleRecall(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有撤回机器人消息的权限。")
	}
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "撤回功能仅支持群聊。")
	}
	if len(fields) > 2 {
		return s.reply(ctx, event, "格式错误。请回复机器人两分钟内发送的消息并输入 /recall；也可使用 /recall <消息ID>。")
	}
	reference := ""
	if len(fields) == 2 {
		reference = fields[1]
	}
	if reference == "" {
		reference = sceneValue(event.Message.Scene.Ext, "ref_msg_idx")
	}
	if reference == "" {
		reference = findReferenceIndex(event.Message.Elements)
	}
	sent, err := s.store.GetSentBotMessage(event.Message.GroupOpenID, reference)
	if err != nil {
		return s.reply(ctx, event, "未找到可撤回的机器人消息，请回复机器人刚刚发送的消息后重试。")
	}
	if time.Since(sent.SentAt) > 2*time.Minute {
		return s.reply(ctx, event, "该机器人消息发送已超过两分钟，QQ 不再允许撤回。")
	}
	api, ok := s.qq.(groupRecallAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持消息撤回。")
	}
	if err := api.RecallGroupMessage(ctx, sent.GroupOpenID, sent.MessageID); err != nil {
		return s.reply(ctx, event, "撤回失败："+publicQQError(err))
	}
	return s.reply(ctx, event, "机器人消息已撤回。")
}

func (s *Service) handleAdminUser(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行用户状态管理的权限。")
	}
	if len(fields) != 4 {
		return s.reply(ctx, event, "用法：/admin user status|enable|disable|reset2fa|resetpasskey <用户ID或@用户>")
	}
	action := strings.ToLower(fields[2])
	userID, description, err := s.resolveUserTarget(event, fields[3])
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	user, err := s.newAPI.GetUser(ctx, userID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	if action == "status" {
		return s.reply(ctx, event, fmt.Sprintf("%s New API 用户 %d\n用户名：%s\n邮箱：%s\n状态：%s\n角色：%d\n分组：%s", description, user.ID, user.Username, store.MaskEmail(user.Email), userStatusText(user.Status), user.Role, user.Group))
	}
	api, ok := s.newAPI.(userAdminAPI)
	if !ok {
		return s.reply(ctx, event, "当前 New API 版本不支持该用户管理操作。")
	}
	if action == "enable" {
		err = api.ManageUserStatus(ctx, userID, "enable")
		s.auditUserAction(canonical, action, userID, err)
		if err != nil {
			return s.reply(ctx, event, "启用用户失败："+publicError(err))
		}
		return s.reply(ctx, event, fmt.Sprintf("已启用%s New API 用户 %d。", description, userID))
	}
	if action != "disable" && action != "reset2fa" && action != "resetpasskey" {
		return s.reply(ctx, event, "用法：/admin user status|enable|disable|reset2fa|resetpasskey <用户ID或@用户>")
	}
	code, codeErr := randomCode(8)
	if codeErr != nil {
		return s.reply(ctx, event, "生成确认码失败，请稍后重试。")
	}
	pending := model.PendingAdminAction{Code: code, Actor: canonical, Action: action, TargetID: userID, TargetLabel: description, ExpiresAt: time.Now().Add(5 * time.Minute)}
	if err := s.store.PutPendingAdminAction(pending); err != nil {
		return s.reply(ctx, event, "保存待确认操作失败。")
	}
	return s.reply(ctx, event, fmt.Sprintf("该操作需要二次确认：%s New API 用户 %d。请在 5 分钟内输入 /confirm %s。", adminActionText(action), userID, code))
}

func (s *Service) handleConfirm(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有确认管理员操作的权限。")
	}
	if len(fields) != 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/confirm <一次性操作码>")
	}
	pending, err := s.store.TakePendingAdminAction(strings.ToUpper(fields[1]), canonical, time.Now())
	if err != nil {
		return s.reply(ctx, event, "确认码无效、已过期或不属于当前管理员。")
	}
	api, ok := s.newAPI.(userAdminAPI)
	if !ok {
		return s.reply(ctx, event, "当前 New API 版本不支持该用户管理操作。")
	}
	switch pending.Action {
	case "disable":
		err = api.ManageUserStatus(ctx, pending.TargetID, "disable")
	case "reset2fa":
		err = api.ResetUser2FA(ctx, pending.TargetID)
	case "resetpasskey":
		err = api.ResetUserPasskey(ctx, pending.TargetID)
	default:
		err = fmt.Errorf("未知待确认操作")
	}
	s.auditUserAction(canonical, pending.Action, pending.TargetID, err)
	if err != nil {
		return s.reply(ctx, event, adminActionText(pending.Action)+"失败："+publicError(err))
	}
	return s.reply(ctx, event, fmt.Sprintf("%s成功：New API 用户 %d。", adminActionText(pending.Action), pending.TargetID))
}

func (s *Service) auditUserAction(actor, action string, target int, err error) {
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: actor, Action: "user." + action, Target: strconv.Itoa(target), Success: err == nil, Description: errorText(err)})
}

func (s *Service) handleUsageChart(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, duration, target string) error {
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "用量图表目前仅支持群聊发送。")
	}
	select {
	case s.chartSemaphore <- struct{}{}:
		defer func() { <-s.chartSemaphore }()
	default:
		return s.replyUsageChartResult(ctx, event, "当前已有用量图表正在生成，请稍后重试。")
	}
	start, end, label, err := parseInsightRange(duration, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	var records []newapi.UsageRecord
	targetText := "当前用户"
	memberCount := 1
	if strings.EqualFold(target, "all") {
		bindings, groupErr := s.store.ListGroupBindings(event.Message.GroupOpenID)
		if groupErr != nil {
			return s.reply(ctx, event, "读取本群绑定成员失败，请稍后重试。")
		}
		if len(bindings) == 0 {
			return s.reply(ctx, event, "当前群内尚无已绑定并被机器人识别的成员，无法生成汇总图表。")
		}
		memberCount = len(bindings)
		queryCtx, cancel := s.backgroundCommandContext(ctx, usageChartTimeout(s.cfg.NewAPITimeout, memberCount))
		defer cancel()
		ctx = queryCtx
		var queryErr error
		records, queryErr = s.listGroupUsageChartRecords(ctx, bindings, start, end)
		if queryErr != nil {
			return s.replyUsageChartResult(ctx, event, "生成群成员用量图表失败："+publicError(queryErr))
		}
		targetText = fmt.Sprintf("本群已绑定成员（%d 人）", len(bindings))
	} else {
		queryCtx, cancel := s.backgroundCommandContext(ctx, usageChartTimeout(s.cfg.NewAPITimeout, memberCount))
		defer cancel()
		ctx = queryCtx
		userID := 0
		if target == "" {
			binding, bindingErr := s.store.GetBinding(canonical)
			if bindingErr != nil {
				return s.reply(ctx, event, "未找到当前用户的绑定信息。")
			}
			userID = binding.NewAPIID
		} else {
			if !s.isAdmin(identity) {
				return s.reply(ctx, event, "你没有查看其他用户用量图表的权限。")
			}
			var resolveErr error
			userID, targetText, resolveErr = s.resolveUserTarget(event, target)
			if resolveErr != nil {
				return s.reply(ctx, event, resolveErr.Error())
			}
		}
		user, userErr := s.newAPI.GetUser(ctx, userID)
		if userErr != nil {
			return s.reply(ctx, event, publicError(userErr))
		}
		records, err = s.newAPI.ListUsageByModel(ctx, start, end, user.Username)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		targetText = fmt.Sprintf("%s（New API 用户 %d）", targetText, user.ID)
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.replyUsageChartResult(ctx, event, publicError(err))
	}
	data, err := renderUsageChart(records, start, end, s.cfg.CheckinTimezone, status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "生成用量图表失败。")
	}
	api, ok := s.qq.(groupFileAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持图片上传。")
	}
	name := "usage-" + time.Now().Format("20060102-150405") + ".png"
	sent, err := api.SendGroupFile(ctx, event.Message.GroupOpenID, event.Message.ID, name, 1, data)
	if err != nil {
		return s.replyUsageChartResult(ctx, event, "发送用量图表失败："+publicQQError(err))
	}
	if sent.ID != "" {
		_ = s.store.PutSentBotMessage(model.SentBotMessage{GroupOpenID: event.Message.GroupOpenID, MessageID: sent.ID, SentAt: time.Now()})
	}
	return s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, "", fmt.Sprintf("%s 的%s用量图表已生成（折线：每日额度；色块：模型占比）。", targetText, label))
}

func usageChartTimeout(requestTimeout time.Duration, members int) time.Duration {
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Second
	}
	if members < 1 {
		members = 1
	}
	// 两次全局查询通常即可完成；兼容接口缺少用户名时，按四个 worker 分批回退查询。
	batches := (members+3)/4 + 3
	timeout := requestTimeout * time.Duration(batches)
	if timeout < 45*time.Second {
		timeout = 45 * time.Second
	}
	if timeout > 2*time.Minute {
		timeout = 2 * time.Minute
	}
	return timeout
}

func (s *Service) replyUsageChartResult(ctx context.Context, event qq.MessageEvent, content string) error {
	replyTimeout := s.cfg.QQAPITimeout
	if replyTimeout <= 0 {
		replyTimeout = 10 * time.Second
	}
	replyCtx, cancel := s.backgroundCommandContext(ctx, replyTimeout)
	defer cancel()
	return s.reply(replyCtx, event, content)
}

// listGroupUsageChartRecords queries each bound account explicitly. The New API
// global /api/data response is aggregated by model on some deployments and may
// omit user_id, so filtering that response by user ID produces an all-zero chart.
func (s *Service) listGroupUsageChartRecords(ctx context.Context, bindings []model.Binding, start, end time.Time) ([]newapi.UsageRecord, error) {
	if len(bindings) == 0 {
		return []newapi.UsageRecord{}, nil
	}
	users, err := s.newAPI.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	usersByID := make(map[int]string, len(users))
	for _, user := range users {
		username := strings.TrimSpace(user.Username)
		if user.ID > 0 && username != "" {
			usersByID[user.ID] = username
		}
	}
	usernames := make([]string, 0, len(bindings))
	memberNames := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		username := usersByID[binding.NewAPIID]
		if username == "" {
			return nil, fmt.Errorf("New API 用户 %d 不存在或缺少用户名", binding.NewAPIID)
		}
		usernames = append(usernames, username)
		memberNames[strings.ToLower(username)] = struct{}{}
	}

	// 优先使用一次全局查询，并按用户名过滤。部分 New API 版本不返回 user_id，
	// 但仍会返回 username；这样可避免群成员数量增加时线性放大请求数。
	globalRecords, globalErr := s.newAPI.ListUsageByModel(ctx, start, end, "")
	if globalErr == nil {
		if len(globalRecords) == 0 {
			return []newapi.UsageRecord{}, nil
		}
		hasUsername := false
		filtered := make([]newapi.UsageRecord, 0, len(globalRecords))
		for _, record := range globalRecords {
			username := strings.ToLower(strings.TrimSpace(record.Username))
			if username == "" {
				continue
			}
			hasUsername = true
			if _, exists := memberNames[username]; exists {
				filtered = append(filtered, record)
			}
		}
		if hasUsername {
			return filtered, nil
		}
	} else if ctx.Err() != nil {
		return nil, globalErr
	}

	type result struct {
		records []newapi.UsageRecord
		err     error
	}
	workers := min(4, len(usernames))
	jobs := make(chan string)
	results := make(chan result, len(usernames))
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for username := range jobs {
				records, queryErr := s.newAPI.ListUsageByModel(ctx, start, end, username)
				results <- result{records: records, err: queryErr}
			}
		}()
	}
	go func() {
		for _, username := range usernames {
			jobs <- username
		}
		close(jobs)
		wait.Wait()
		close(results)
	}()

	all := make([]newapi.UsageRecord, 0)
	for result := range results {
		if result.err != nil {
			return nil, result.err
		}
		all = append(all, result.records...)
	}
	return all, nil
}

func (s *Service) handleAdminReportExport(ctx context.Context, event qq.MessageEvent, fields []string) error {
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "CSV 报表导出仅支持群聊。")
	}
	if len(fields) > 4 {
		return s.reply(ctx, event, "格式错误。正确用法：/admin report export [时间长度]")
	}
	duration := "today"
	if len(fields) == 4 {
		duration = fields[3]
	}
	start, end, label, err := parseInsightRange(duration, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	users, err := s.newAPI.ListUsers(ctx)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rows, err := s.newAPI.ListUsageByUser(ctx, start, end)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	var buffer bytes.Buffer
	buffer.WriteString("\xEF\xBB\xBF")
	writer := csv.NewWriter(&buffer)
	_ = writer.Write([]string{"用户ID", "用户名", "请求次数", "Token", "消耗额度", "当前余额", "开始时间", "结束时间"})
	userMap := make(map[int]newapi.User, len(users))
	for _, user := range users {
		userMap[user.ID] = user
	}
	for _, item := range mergeUsersAndUsage(users, rows) {
		user := userMap[item.UserID]
		_ = writer.Write([]string{strconv.Itoa(item.UserID), item.Username, strconv.FormatInt(item.Count, 10), strconv.FormatInt(item.Tokens, 10), newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit), newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit), start.In(s.cfg.CheckinTimezone).Format(time.RFC3339), end.In(s.cfg.CheckinTimezone).Format(time.RFC3339)})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return s.reply(ctx, event, "生成 CSV 报表失败。")
	}
	api, ok := s.qq.(groupFileAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持文件上传。")
	}
	name := "newapi-report-" + time.Now().Format("20060102-150405") + ".csv"
	_, err = api.SendGroupFile(ctx, event.Message.GroupOpenID, event.Message.ID, name, 4, buffer.Bytes())
	if err != nil {
		return s.reply(ctx, event, "发送 CSV 报表失败："+publicQQError(err))
	}
	return s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, "", "全站 "+label+" CSV 报表已发送。")
}

func renderUsageChart(records []newapi.UsageRecord, start, end time.Time, location *time.Location, quotaPerUnit int64) ([]byte, error) {
	const (
		canvasWidth  = 1280
		canvasHeight = 820
		plotLeft     = 150
		plotRight    = 1210
		plotTop      = 220
		plotBottom   = 500
	)
	img := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	background := color.RGBA{248, 250, 252, 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{background}, image.Point{}, draw.Src)

	days := int(end.Sub(start).Hours()/24 + 0.999)
	if days < 2 {
		days = 2
	}
	if days > 31 {
		days = 31
	}
	daily := make([]int64, days)
	modelQuota := map[string]int64{}
	var totalQuota, totalRequests, totalTokens int64
	for _, record := range records {
		idx := days - 1
		if record.CreatedAt > 0 {
			idx = int(time.Unix(record.CreatedAt, 0).In(location).Sub(start.In(location)).Hours() / 24)
			if idx < 0 {
				idx = 0
			}
			if idx >= days {
				idx = days - 1
			}
		}
		daily[idx] += record.Quota
		modelQuota[nonEmpty(record.ModelName, "UNKNOWN")] += record.Quota
		totalQuota += record.Quota
		totalRequests += record.Count
		totalTokens += record.TokenUsed
	}

	ink := color.RGBA{30, 41, 59, 255}
	muted := color.RGBA{100, 116, 139, 255}
	grid := color.RGBA{203, 213, 225, 255}
	blue := color.RGBA{37, 99, 235, 255}
	drawTinyText(img, 54, 58, "USAGE REPORT", 3, ink)
	rangeText := start.In(location).Format("2006-01-02 15:04") + " - " + end.In(location).Format("2006-01-02 15:04")
	drawTinyText(img, 56, 86, rangeText, 1, muted)
	drawTinyText(img, 56, 130, "TOTAL QUOTA: "+newapi.QuotaToDisplay(totalQuota, quotaPerUnit), 2, ink)
	drawTinyText(img, 460, 130, "REQUESTS: "+compactChartNumber(totalRequests), 2, ink)
	drawTinyText(img, 790, 130, "TOKENS: "+compactChartNumber(totalTokens), 2, ink)

	fillRect(img, 48, 178, 1232, 540, color.RGBA{255, 255, 255, 255})
	drawTinyText(img, plotLeft, 204, "DAILY QUOTA", 2, ink)
	fillRect(img, plotLeft+180, 195, plotLeft+194, 209, blue)
	drawTinyText(img, plotLeft+202, 204, "DAILY USAGE", 1, muted)

	maxQuota := int64(1)
	for _, quota := range daily {
		if quota > maxQuota {
			maxQuota = quota
		}
	}
	maxQuota = niceChartMax(maxQuota)
	for tick := 0; tick <= 4; tick++ {
		y := plotBottom - tick*(plotBottom-plotTop)/4
		drawLine(img, plotLeft, y, plotRight, y, grid)
		label := newapi.QuotaToDisplay(maxQuota*int64(tick)/4, quotaPerUnit)
		drawTinyTextRight(img, plotLeft-12, y+4, label, 1, muted)
	}
	drawLine(img, plotLeft, plotTop, plotLeft, plotBottom, color.RGBA{71, 85, 105, 255})
	drawLine(img, plotLeft, plotBottom, plotRight, plotBottom, color.RGBA{71, 85, 105, 255})

	labelStep := 1
	if days > 10 {
		labelStep = 2
	}
	if days > 20 {
		labelStep = 4
	}
	previousX, previousY := 0, 0
	for i, quota := range daily {
		x := plotLeft + i*(plotRight-plotLeft)/(days-1)
		y := plotBottom - int(quota*int64(plotBottom-plotTop)/maxQuota)
		if i > 0 {
			drawThickLine(img, previousX, previousY, x, y, blue, 2)
		}
		fillRect(img, x-4, y-4, x+5, y+5, color.RGBA{29, 78, 216, 255})
		if i%labelStep == 0 || i == days-1 {
			drawLine(img, x, plotBottom, x, plotBottom+6, muted)
			labelDay := start.In(location).AddDate(0, 0, i).Format("01/02")
			drawTinyTextCentered(img, x, plotBottom+24, labelDay, 1, muted)
		}
		previousX, previousY = x, y
	}

	drawTinyText(img, 56, 590, "MODEL SHARE", 2, ink)
	type pair struct {
		name  string
		quota int64
	}
	pairs := make([]pair, 0, len(modelQuota))
	for name, quota := range modelQuota {
		pairs = append(pairs, pair{name: name, quota: quota})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].quota != pairs[j].quota {
			return pairs[i].quota > pairs[j].quota
		}
		return pairs[i].name < pairs[j].name
	})
	colors := []color.RGBA{{16, 185, 129, 255}, {245, 158, 11, 255}, {139, 92, 246, 255}, {239, 68, 68, 255}, {6, 182, 212, 255}, {100, 116, 139, 255}}
	barLeft, barRight := 56, 1224
	fillRect(img, barLeft, 615, barRight, 655, color.RGBA{226, 232, 240, 255})
	if totalQuota > 0 {
		x := barLeft
		for i, pair := range pairs {
			if i == len(colors) {
				break
			}
			width := int(pair.quota * int64(barRight-barLeft) / totalQuota)
			if i == len(pairs)-1 || i == len(colors)-1 {
				width = barRight - x
			}
			if width > 0 {
				fillRect(img, x, 615, min(barRight, x+width), 655, colors[i])
				x += width
			}
		}
	}
	if len(pairs) == 0 {
		drawTinyText(img, 56, 700, "NO USAGE DATA IN THIS PERIOD", 2, muted)
	} else {
		for i, pair := range pairs {
			if i >= len(colors) {
				break
			}
			column, row := i%2, i/2
			x := 56 + column*590
			y := 700 + row*36
			fillRect(img, x, y-12, x+16, y+4, colors[i])
			percent := float64(pair.quota) * 100 / float64(totalQuota)
			text := truncateChartLabel(pair.name, 38) + "  " + fmt.Sprintf("%.1f%%", percent)
			drawTinyText(img, x+28, y, text, 1, ink)
		}
	}

	var output bytes.Buffer
	err := png.Encode(&output, img)
	return output.Bytes(), err
}

func niceChartMax(value int64) int64 {
	if value <= 1 {
		return 1
	}
	step := int64(1)
	for step*10 < value {
		step *= 10
	}
	for _, multiplier := range []int64{1, 2, 5, 10} {
		candidate := step * multiplier
		if value <= candidate {
			return candidate
		}
	}
	return step * 10
}

func compactChartNumber(value int64) string {
	negative := value < 0
	if negative {
		value = -value
	}
	prefix := ""
	if negative {
		prefix = "-"
	}
	switch {
	case value >= 1_000_000_000:
		return fmt.Sprintf("%s%.2fB", prefix, float64(value)/1_000_000_000)
	case value >= 1_000_000:
		return fmt.Sprintf("%s%.2fM", prefix, float64(value)/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%s%.1fK", prefix, float64(value)/1_000)
	default:
		return prefix + strconv.FormatInt(value, 10)
	}
}

func truncateChartLabel(value string, limit int) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:max(1, limit-3)]) + "..."
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx := abs(x1 - x0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -abs(y1 - y0)
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	e := dx + dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}

// tinyGlyphs keeps chart labels self-contained: the production image does not
// rely on OS fonts, which are intentionally absent from the distroless image.
var tinyGlyphs = map[rune][7]byte{
	'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11}, 'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E}, 'D': {0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1E},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F}, 'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F}, 'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E}, 'J': {0x07, 0x02, 0x02, 0x02, 0x12, 0x12, 0x0C},
	'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11}, 'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11}, 'N': {0x11, 0x19, 0x19, 0x15, 0x13, 0x13, 0x11},
	'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E}, 'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'Q': {0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D}, 'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E}, 'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E}, 'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x1B, 0x11}, 'X': {0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x0A, 0x04, 0x04, 0x04, 0x04}, 'Z': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F},
	'0': {0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E}, '1': {0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'2': {0x0E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F}, '3': {0x1E, 0x01, 0x01, 0x0E, 0x01, 0x01, 0x1E},
	'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02}, '5': {0x1F, 0x10, 0x10, 0x1E, 0x01, 0x01, 0x1E},
	'6': {0x0E, 0x10, 0x10, 0x1E, 0x11, 0x11, 0x0E}, '7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E}, '9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x01, 0x0E},
	'-': {0x00, 0x00, 0x00, 0x1F, 0x00, 0x00, 0x00}, '.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x0C, 0x0C},
	':': {0x00, 0x0C, 0x0C, 0x00, 0x0C, 0x0C, 0x00}, '/': {0x01, 0x02, 0x02, 0x04, 0x08, 0x08, 0x10},
	'|': {0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04}, '%': {0x19, 0x19, 0x02, 0x04, 0x08, 0x13, 0x13},
	'_': {0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1F}, '?': {0x0E, 0x11, 0x01, 0x02, 0x04, 0x00, 0x04},
}

func drawThickLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color, thickness int) {
	radius := max(0, thickness-1)
	for dx := -radius; dx <= radius; dx++ {
		for dy := -radius; dy <= radius; dy++ {
			if dx*dx+dy*dy <= radius*radius {
				drawLine(img, x0+dx, y0+dy, x1+dx, y1+dy, c)
			}
		}
	}
}

func drawTinyText(img *image.RGBA, x, y int, value string, scale int, c color.Color) {
	if scale < 1 {
		scale = 1
	}
	for _, char := range strings.ToUpper(value) {
		if char == ' ' {
			x += 4 * scale
			continue
		}
		glyph, exists := tinyGlyphs[char]
		if !exists {
			glyph = tinyGlyphs['?']
		}
		for row, bits := range glyph {
			for column := 0; column < 5; column++ {
				if bits&(1<<uint(4-column)) != 0 {
					fillRect(img, x+column*scale, y+row*scale, x+(column+1)*scale, y+(row+1)*scale, c)
				}
			}
		}
		x += 6 * scale
	}
}

func tinyTextWidth(value string, scale int) int {
	if scale < 1 {
		scale = 1
	}
	width := 0
	for range value {
		width += 6 * scale
	}
	return width
}

func drawTinyTextRight(img *image.RGBA, x, y int, value string, scale int, c color.Color) {
	drawTinyText(img, x-tinyTextWidth(value, scale), y, value, scale, c)
}

func drawTinyTextCentered(img *image.RGBA, x, y int, value string, scale int, c color.Color) {
	drawTinyText(img, x-tinyTextWidth(value, scale)/2, y, value, scale, c)
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	draw.Draw(img, image.Rect(x0, y0, x1, y1), &image.Uniform{c}, image.Point{}, draw.Src)
}
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func findReferenceIndex(elements []qq.MsgElement) string {
	for _, e := range elements {
		if e.MessageType == 103 && e.MsgIdx != "" {
			return e.MsgIdx
		}
		if nested := findReferenceIndex(e.Elements); nested != "" {
			return nested
		}
	}
	return ""
}
func boolStatus(value bool) string {
	if value {
		return "正常/是"
	}
	return "异常/否"
}
func recvSettingText(value int) string {
	switch value {
	case 0:
		return "全部消息"
	case 1:
		return "仅 @"
	case 2:
		return "@ 与上下文"
	default:
		return fmt.Sprintf("模式 %d", value)
	}
}
func memberRoleText(value int) string {
	switch value {
	case 1:
		return "普通成员"
	case 2:
		return "管理员"
	case 4:
		return "群主"
	default:
		return fmt.Sprintf("角色 %d", value)
	}
}
func formatUnixFlexible(value int64, loc *time.Location) string {
	if value <= 0 {
		return "未知"
	}
	if value > 1e12 {
		value /= 1000
	}
	return time.Unix(value, 0).In(loc).Format("2006-01-02 15:04:05")
}
func publicQQError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
	r := []rune(value)
	if len(r) > 180 {
		value = string(r[:180]) + "…"
	}
	return value
}
func adminActionText(action string) string {
	switch action {
	case "disable":
		return "禁用用户"
	case "reset2fa":
		return "重置 2FA"
	case "resetpasskey":
		return "重置 Passkey"
	case "enable":
		return "启用用户"
	default:
		return action
	}
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return publicError(err)
}
