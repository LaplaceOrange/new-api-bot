package bot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
)

const maxInsightRange = 31 * 24 * time.Hour

type usageTotal struct {
	UserID   int
	Username string
	Quota    int64
	Tokens   int64
	Count    int64
}

func (s *Service) handleUsage(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if len(fields) > 3 {
		return s.reply(ctx, event, usageHelp())
	}
	if len(fields) >= 2 && strings.EqualFold(fields[1], "chart") {
		if !s.cfg.UsageChartEnabled {
			return s.reply(ctx, event, "用量图表功能当前已关闭。")
		}
		duration := "7d"
		if len(fields) == 3 {
			duration = fields[2]
		}
		return s.handleUsageChart(ctx, event, canonical, duration)
	}
	durationArg := "today"
	if len(fields) >= 2 {
		durationArg = fields[1]
	}
	start, end, label, err := parseInsightRange(durationArg, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
		if len(fields) == 3 && s.isAdmin(identity) {
			userID, description, targetErr := s.resolveUserTarget(event, fields[1])
			if targetErr != nil {
				return s.reply(ctx, event, targetErr.Error())
			}
			start, end, label, err = parseInsightRange(fields[2], time.Now(), s.cfg.CheckinTimezone)
			if err != nil {
				return s.reply(ctx, event, err.Error()+"\n"+usageHelp())
			}
			return s.replyUserUsage(ctx, event, userID, description, start, end, label)
		}
		return s.reply(ctx, event, err.Error()+"\n"+usageHelp())
	}
	if len(fields) == 3 && strings.EqualFold(fields[2], "all") {
		return s.replyAllUsageSummary(ctx, event, start, end, label)
	}
	if len(fields) == 3 {
		rankCount, parseErr := parseUsageRank(fields[2])
		if parseErr != nil {
			return s.reply(ctx, event, parseErr.Error()+"\n"+usageHelp())
		}
		return s.replyUsageRanking(ctx, event, start, end, label, rankCount)
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "未找到当前用户的绑定信息。")
	}
	return s.replyUserUsage(ctx, event, binding.NewAPIID, "当前用户", start, end, label)
}

func (s *Service) replyUserUsage(ctx context.Context, event qq.MessageEvent, userID int, targetDescription string, start, end time.Time, label string) error {
	user, err := s.newAPI.GetUser(ctx, userID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rows, err := s.newAPI.ListUsageByUser(ctx, start, end)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	total := usageForUsername(rows, user.Username)
	models, modelErr := s.newAPI.ListUsageByModel(ctx, start, end, user.Username)
	var succeeded, failed int64
	if counter, ok := s.newAPI.(interface {
		CountLogOutcomes(context.Context, time.Time, time.Time, string) (int64, int64, error)
	}); ok {
		succeeded, failed, _ = counter.CountLogOutcomes(ctx, start, end, user.Username)
	}
	status, statusErr := s.newAPI.GetStatus(ctx, false)
	if statusErr != nil {
		return s.reply(ctx, event, publicError(statusErr))
	}
	lines := []string{
		fmt.Sprintf("%s用量（%s，New API 用户 %d）", targetDescription, label, userID),
		"时间范围：" + formatInsightRange(start, end, s.cfg.CheckinTimezone),
		"请求次数：" + strconv.FormatInt(total.Count, 10),
		fmt.Sprintf("成功 / 失败：%d / %d", succeeded, failed),
		"Token 用量：" + strconv.FormatInt(total.Tokens, 10),
		"消耗额度：" + newapi.QuotaToDisplay(total.Quota, status.QuotaPerUnit),
		"当前余额：" + newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit),
	}
	if modelErr == nil {
		top := aggregateModels(models)
		if len(top) > 0 {
			lines = append(lines, "常用模型：")
			for index, item := range top {
				if index >= 5 {
					break
				}
				lines = append(lines, fmt.Sprintf("%d. %s｜%d 次｜额度 %s", index+1, item.Username, item.Count, newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit)))
			}
		}
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}

func (s *Service) replyAllUsageSummary(ctx context.Context, event qq.MessageEvent, start, end time.Time, label string) error {
	rows, err := s.newAPI.ListUsageByUser(ctx, start, end)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	var total usageTotal
	activeUsernames := make(map[string]struct{})
	for _, row := range rows {
		total.Count += row.Count
		total.Tokens += row.TokenUsed
		total.Quota += row.Quota
		if row.Count > 0 || row.TokenUsed > 0 || row.Quota > 0 {
			activeUsernames[strings.ToLower(strings.TrimSpace(row.Username))] = struct{}{}
		}
	}
	lines := []string{
		fmt.Sprintf("全站用量汇总（%s）", label),
		"时间范围：" + formatInsightRange(start, end, s.cfg.CheckinTimezone),
		"请求次数：" + strconv.FormatInt(total.Count, 10),
		"Token 用量：" + strconv.FormatInt(total.Tokens, 10),
		"消耗额度：" + newapi.QuotaToDisplay(total.Quota, status.QuotaPerUnit),
		"活跃用户：" + strconv.Itoa(len(activeUsernames)),
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}

func (s *Service) replyUsageRanking(ctx context.Context, event qq.MessageEvent, start, end time.Time, label string, rankCount int) error {
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
	totals := mergeUsersAndUsage(users, rows)
	active := totals[:0]
	for _, item := range totals {
		if item.Count > 0 || item.Tokens > 0 || item.Quota > 0 {
			active = append(active, item)
		}
	}
	if len(active) == 0 {
		return s.reply(ctx, event, fmt.Sprintf("用量排行榜（%s）：该时间段内没有使用记录。", label))
	}
	if rankCount > len(active) {
		rankCount = len(active)
	}
	lines := []string{
		fmt.Sprintf("用量排行榜（%s，前 %d 名，按消耗额度排序）", label, rankCount),
		"时间范围：" + formatInsightRange(start, end, s.cfg.CheckinTimezone),
	}
	for index, item := range active[:rankCount] {
		lines = append(lines, fmt.Sprintf("%d. ID %d｜%s｜%d 次｜%d Token｜额度 %s", index+1, item.UserID, nonEmpty(item.Username, "未知用户"), item.Count, item.Tokens, newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit)))
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1700)
}

func (s *Service) handleLogs(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if len(fields) > 3 {
		return s.reply(ctx, event, logsHelp())
	}
	count := 10
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "未找到当前用户的绑定信息。")
	}
	userID := binding.NewAPIID
	targetDescription := "当前用户"
	if len(fields) == 2 {
		if parsed, parseErr := strconv.Atoi(fields[1]); parseErr == nil && parsed >= 1 && parsed <= 20 {
			count = parsed
		} else {
			if !s.isAdmin(identity) {
				return s.reply(ctx, event, "你没有查看其他用户调用记录的权限；可以使用 /logs [数量] 查看自己。")
			}
			userID, targetDescription, err = s.resolveUserTarget(event, fields[1])
			if err != nil {
				return s.reply(ctx, event, err.Error())
			}
		}
	}
	if len(fields) == 3 {
		if !s.isAdmin(identity) {
			return s.reply(ctx, event, "你没有查看其他用户调用记录的权限。")
		}
		userID, targetDescription, err = s.resolveUserTarget(event, fields[1])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		count, err = strconv.Atoi(fields[2])
		if err != nil || count < 1 || count > 20 {
			return s.reply(ctx, event, "日志数量必须是 1 到 20 的整数。")
		}
	}
	user, err := s.newAPI.GetUser(ctx, userID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	end := time.Now()
	page, err := s.newAPI.ListLogs(ctx, end.Add(-maxInsightRange), end, user.Username, 1, count)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	if len(page.Items) == 0 {
		return s.reply(ctx, event, fmt.Sprintf("%s（New API 用户 %d）最近 31 天没有调用记录。", targetDescription, userID))
	}
	lines := []string{fmt.Sprintf("%s（New API 用户 %d）最近 %d 条调用记录：", targetDescription, userID, len(page.Items))}
	for index, item := range page.Items {
		lines = append(lines, fmt.Sprintf("\n%d. %s｜%s", index+1, formatLogTime(item.CreatedAt, s.cfg.CheckinTimezone), nonEmpty(item.ModelName, "未知模型")))
		callStatus := "成功"
		if item.Type == 5 {
			callStatus = "失败"
		}
		lines = append(lines, fmt.Sprintf("状态：%s｜Token：%d（输入 %d / 输出 %d）｜额度：%s｜耗时：%ds", callStatus, item.PromptTokens+item.CompletionTokens, item.PromptTokens, item.CompletionTokens, newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit), item.UseTime))
		if item.Type == 5 && strings.TrimSpace(item.Content) != "" {
			lines = append(lines, "错误："+nonEmpty(item.Content, "调用失败"))
		}
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1700)
}

func (s *Service) handleModels(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if len(fields) > 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/models [用户ID或@用户]")
	}
	binding, _ := s.store.GetBinding(canonical)
	userID := binding.NewAPIID
	if len(fields) == 2 {
		if !s.isAdmin(identity) {
			return s.reply(ctx, event, "你没有查看其他用户模型的权限。")
		}
		var err error
		userID, _, err = s.resolveUserTarget(event, fields[1])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
	}
	user, userErr := s.newAPI.GetUser(ctx, userID)
	if userErr != nil {
		return s.reply(ctx, event, publicError(userErr))
	}
	models := []string{}
	exact := false
	if provider, ok := s.newAPI.(interface {
		ListUserModels(context.Context, string) ([]string, error)
	}); ok {
		if result, err := provider.ListUserModels(ctx, user.Group); err == nil {
			models = result
			exact = true
		}
	}
	if !exact {
		var err error
		models, err = s.newAPI.ListEnabledModels(ctx)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
	}
	header := fmt.Sprintf("站点当前已启用模型，共 %d 个", len(models))
	if strings.TrimSpace(user.Group) != "" {
		header += "；你的用户分组：" + user.Group
	}
	if !exact {
		header += "（站点级回退结果）"
	}
	if len(models) == 0 {
		return s.reply(ctx, event, header+"。")
	}
	lines := []string{header + "："}
	for index, name := range models {
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, name))
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1700)
}

func (s *Service) handleNotify(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		preference, err := s.store.GetQuotaNotification(canonical)
		if err != nil {
			return s.reply(ctx, event, "当前未启用提醒。用法：/notify quota <额度> 或 /notify daily on")
		}
		status := "等待额度低于阈值"
		if preference.Alerted {
			status = "本轮低额度提醒已发送，充值超过阈值后会自动重新启用"
		}
		quotaState := "关闭"
		if preference.Enabled {
			quotaState = "开启，阈值 " + preference.Threshold + "，" + status
		}
		dailyState := "关闭"
		if preference.DailyEnabled {
			dailyState = "开启，每日 " + s.cfg.NotifyDailyTime
		}
		return s.reply(ctx, event, fmt.Sprintf("New API 用户：%d\n额度提醒：%s\n每日摘要：%s", preference.NewAPIID, quotaState, dailyState))
	}
	if len(fields) == 3 && strings.EqualFold(fields[1], "daily") {
		if event.Message.GroupOpenID == "" {
			return s.reply(ctx, event, "每日摘要只能在目标群内设置。")
		}
		binding, err := s.store.GetBinding(canonical)
		if err != nil {
			return s.reply(ctx, event, "未找到当前用户的绑定信息。")
		}
		preference, _ := s.store.GetQuotaNotification(canonical)
		preference.CanonicalID = canonical
		preference.NewAPIID = binding.NewAPIID
		preference.GroupOpenID = event.Message.GroupOpenID
		preference.MemberOpenID = event.Message.Author.MemberOpenID
		switch strings.ToLower(fields[2]) {
		case "on":
			preference.DailyEnabled = true
		case "off":
			preference.DailyEnabled = false
		default:
			return s.reply(ctx, event, "格式错误。正确用法：/notify daily on 或 /notify daily off")
		}
		if !preference.Enabled && !preference.DailyEnabled {
			_ = s.store.DeleteQuotaNotification(canonical)
		} else if err := s.store.PutQuotaNotification(preference); err != nil {
			return s.reply(ctx, event, "保存每日摘要设置失败。")
		}
		if preference.DailyEnabled {
			return s.reply(ctx, event, "每日用量摘要已开启，将在每天 "+s.cfg.NotifyDailyTime+"（"+s.cfg.CheckinTimezoneName+"）发送到当前群。")
		}
		return s.reply(ctx, event, "每日用量摘要已关闭。")
	}
	if len(fields) != 3 || !strings.EqualFold(fields[1], "quota") {
		return s.reply(ctx, event, "用法：/notify quota <额度>、/notify quota off 或 /notify status")
	}
	if strings.EqualFold(fields[2], "off") {
		preference, err := s.store.GetQuotaNotification(canonical)
		if err == nil && preference.DailyEnabled {
			preference.Enabled = false
			preference.Alerted = false
			_ = s.store.PutQuotaNotification(preference)
		} else {
			_ = s.store.DeleteQuotaNotification(canonical)
		}
		return s.reply(ctx, event, "额度提醒已关闭。")
	}
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "额度提醒只能在目标群内设置，提醒消息将发送到当前群。")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "未找到当前用户的绑定信息。")
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rawThreshold, err := newapi.DisplayToQuota(fields[2], status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "提醒额度必须是大于 0 的有效数字。")
	}
	user, err := s.newAPI.GetUser(ctx, binding.NewAPIID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	preference, _ := s.store.GetQuotaNotification(canonical)
	preference.CanonicalID = canonical
	preference.NewAPIID = binding.NewAPIID
	preference.GroupOpenID = event.Message.GroupOpenID
	preference.MemberOpenID = event.Message.Author.MemberOpenID
	preference.Threshold = fields[2]
	preference.Enabled = true
	preference.Alerted = user.Quota <= rawThreshold
	if err := s.store.PutQuotaNotification(preference); err != nil {
		return s.reply(ctx, event, "保存额度提醒设置失败。")
	}
	message := fmt.Sprintf("额度提醒已启用：当 New API 用户 %d 的余额低于或等于 %s 时，机器人会在当前群发送一次提醒；充值超过阈值后会自动重新启用。", binding.NewAPIID, fields[2])
	if user.Quota <= rawThreshold {
		message += "\n当前余额已经低于或等于该阈值，本次设置消息即作为提醒。"
	}
	return s.reply(ctx, event, message)
}

func (s *Service) runQuotaNotifier(ctx context.Context) {
	interval := s.cfg.NotifyCheckInterval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		case <-ticker.C:
			s.checkQuotaNotifications()
		}
	}
}

func (s *Service) checkQuotaNotifications() {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.NewAPITimeout*2)
	defer cancel()
	preferences, err := s.store.ListQuotaNotifications()
	if err != nil || len(preferences) == 0 {
		if err != nil {
			s.logger.Error("读取额度提醒配置失败", "error", err)
		}
		return
	}
	users, err := s.newAPI.ListUsers(ctx)
	if err != nil {
		s.logger.Warn("额度提醒读取用户列表失败", "error", publicError(err))
		return
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		s.logger.Warn("额度提醒读取站点状态失败", "error", publicError(err))
		return
	}
	byID := make(map[int]newapi.User, len(users))
	for _, user := range users {
		byID[user.ID] = user
	}
	groupMessages := make(map[string][]string)
	groupAllowed := make(map[string]bool)
	updates := make(map[string]model.QuotaNotification)
	updateGroups := make(map[string]string)
	now := time.Now()
	dailyAt, _ := time.ParseInLocation("15:04", s.cfg.NotifyDailyTime, s.cfg.CheckinTimezone)
	local := now.In(s.cfg.CheckinTimezone)
	dailyDue := local.Hour() == dailyAt.Hour() && local.Minute() >= dailyAt.Minute()
	dailyKey := local.Format("2006-01-02")
	for _, preference := range preferences {
		user, exists := byID[preference.NewAPIID]
		if !exists {
			continue
		}
		if preference.Enabled {
			threshold, thresholdErr := newapi.DisplayToQuota(preference.Threshold, status.QuotaPerUnit)
			if thresholdErr == nil && user.Quota > threshold {
				if preference.Alerted {
					preference.Alerted = false
					preference.LastAlertAt = time.Time{}
					updates[preference.CanonicalID] = preference
				}
			} else if thresholdErr == nil && !preference.Alerted {
				if notificationGroupAllowed(s, preference.GroupOpenID, groupAllowed) {
					groupMessages[preference.GroupOpenID] = append(groupMessages[preference.GroupOpenID], fmt.Sprintf("⚠️ 额度提醒 %s New API 用户 %d（%s）当前余额 %s，阈值 %s。", mentionMember(preference.MemberOpenID), user.ID, nonEmpty(user.DisplayName, user.Username), newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit), preference.Threshold))
					preference.Alerted = true
					preference.LastAlertAt = now
					updates[preference.CanonicalID] = preference
					updateGroups[preference.CanonicalID] = preference.GroupOpenID
				}
			}
		}
		if preference.DailyEnabled && dailyDue && preference.LastDailyKey != dailyKey {
			start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.cfg.CheckinTimezone)
			rows, usageErr := s.newAPI.ListUsageByUser(ctx, start, now)
			if usageErr == nil && notificationGroupAllowed(s, preference.GroupOpenID, groupAllowed) {
				total := usageForUsername(rows, user.Username)
				groupMessages[preference.GroupOpenID] = append(groupMessages[preference.GroupOpenID], fmt.Sprintf("📊 %s 今日用量：%d 次请求，%d Token，消耗额度 %s，余额 %s。", mentionMember(preference.MemberOpenID), total.Count, total.Tokens, newapi.QuotaToDisplay(total.Quota, status.QuotaPerUnit), newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit)))
				preference.LastDailyKey = dailyKey
				updates[preference.CanonicalID] = preference
				updateGroups[preference.CanonicalID] = preference.GroupOpenID
			}
		}
	}
	sentGroups := make(map[string]bool)
	for group, lines := range groupMessages {
		if err := s.qq.ReplyGroup(ctx, group, "", strings.Join(lines, "\n")); err != nil {
			s.logger.Warn("发送群内合并通知失败", "error", err)
			continue
		}
		s.notifyMu.Lock()
		s.groupLastNotify[group] = time.Now()
		s.notifyMu.Unlock()
		sentGroups[group] = true
	}
	for canonical, preference := range updates {
		group := updateGroups[canonical]
		if group == "" || sentGroups[group] {
			_ = s.store.PutQuotaNotification(preference)
		}
	}
}

func mentionMember(openID string) string {
	if strings.TrimSpace(openID) == "" {
		return ""
	}
	return "<@" + openID + ">"
}

func notificationGroupAllowed(s *Service, group string, cache map[string]bool) bool {
	if allowed, ok := cache[group]; ok {
		return allowed
	}
	s.notifyMu.Lock()
	allowed := time.Since(s.groupLastNotify[group]) >= s.cfg.NotifyGroupCooldown
	s.notifyMu.Unlock()
	cache[group] = allowed
	return allowed
}

func (s *Service) handleAdminReport(ctx context.Context, event qq.MessageEvent, fields []string) error {
	if len(fields) > 3 {
		return s.reply(ctx, event, "格式错误。正确用法：/admin report [时间长度]")
	}
	durationArg := "24h"
	if len(fields) == 3 {
		durationArg = fields[2]
	}
	start, end, label, err := parseInsightRange(durationArg, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	users, err := s.newAPI.ListUsers(ctx)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	userRows, err := s.newAPI.ListUsageByUser(ctx, start, end)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	modelRows, err := s.newAPI.ListUsageByModel(ctx, start, end, "")
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	totals := mergeUsersAndUsage(users, userRows)
	var total usageTotal
	activeUsers := 0
	for _, item := range totals {
		total.Quota += item.Quota
		total.Tokens += item.Tokens
		total.Count += item.Count
		if item.Count > 0 {
			activeUsers++
		}
	}
	lines := []string{
		"全站用量报告（" + label + "）",
		"时间范围：" + formatInsightRange(start, end, s.cfg.CheckinTimezone),
		fmt.Sprintf("用户总数：%d｜活跃用户：%d", len(users), activeUsers),
		fmt.Sprintf("请求总数：%d｜Token 总量：%d", total.Count, total.Tokens),
		"消耗额度：" + newapi.QuotaToDisplay(total.Quota, status.QuotaPerUnit),
		"用量最高用户：",
	}
	for index, item := range totals {
		if index >= 10 || item.Count == 0 {
			break
		}
		lines = append(lines, fmt.Sprintf("%d. ID %d｜%s｜%d 次｜额度 %s", index+1, item.UserID, item.Username, item.Count, newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit)))
	}
	models := aggregateModels(modelRows)
	if len(models) > 0 {
		lines = append(lines, "热门模型：")
		for index, item := range models {
			if index >= 10 {
				break
			}
			lines = append(lines, fmt.Sprintf("%d. %s｜%d 次｜额度 %s", index+1, item.Username, item.Count, newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit)))
		}
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1700)
}

func parseInsightRange(value string, now time.Time, location *time.Location) (time.Time, time.Time, string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "24h"
	}
	localNow := now.In(location)
	switch value {
	case "today", "day":
		start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
		return start, now, "今天", nil
	case "month":
		start := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
		return start, now, "本月", nil
	case "week":
		value = "7d"
	}
	var duration time.Duration
	var err error
	if strings.HasSuffix(value, "d") || strings.HasSuffix(value, "w") {
		unit := 24 * time.Hour
		number := strings.TrimSuffix(value, "d")
		if strings.HasSuffix(value, "w") {
			unit = 7 * 24 * time.Hour
			number = strings.TrimSuffix(value, "w")
		}
		amount, parseErr := strconv.Atoi(number)
		if parseErr != nil || amount <= 0 {
			return time.Time{}, time.Time{}, "", errors.New("时间长度格式错误，支持 30m、24h、7d、4w、today、month")
		}
		duration = time.Duration(amount) * unit
	} else {
		duration, err = time.ParseDuration(value)
		if err != nil {
			return time.Time{}, time.Time{}, "", errors.New("时间长度格式错误，支持 30m、24h、7d、4w、today、month")
		}
	}
	if duration <= 0 || duration > maxInsightRange {
		return time.Time{}, time.Time{}, "", errors.New("时间长度必须大于 0 且不能超过 31 天")
	}
	return now.Add(-duration), now, "最近 " + value, nil
}

func usageForUsername(records []newapi.UsageRecord, username string) usageTotal {
	var total usageTotal
	for _, record := range records {
		if !strings.EqualFold(strings.TrimSpace(record.Username), strings.TrimSpace(username)) {
			continue
		}
		total.UserID = record.UserID
		total.Username = record.Username
		total.Quota += record.Quota
		total.Tokens += record.TokenUsed
		total.Count += record.Count
	}
	return total
}

func mergeUsersAndUsage(users []newapi.User, records []newapi.UsageRecord) []usageTotal {
	byName := make(map[string]*usageTotal, len(users))
	for _, user := range users {
		key := strings.ToLower(strings.TrimSpace(user.Username))
		item := &usageTotal{UserID: user.ID, Username: user.Username}
		byName[key] = item
	}
	for _, record := range records {
		key := strings.ToLower(strings.TrimSpace(record.Username))
		item, exists := byName[key]
		if !exists {
			item = &usageTotal{UserID: record.UserID, Username: record.Username}
			byName[key] = item
		}
		item.Quota += record.Quota
		item.Tokens += record.TokenUsed
		item.Count += record.Count
	}
	result := make([]usageTotal, 0, len(byName))
	for _, item := range byName {
		result = append(result, *item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Quota != result[j].Quota {
			return result[i].Quota > result[j].Quota
		}
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].UserID < result[j].UserID
	})
	return result
}

func aggregateModels(records []newapi.UsageRecord) []usageTotal {
	byModel := make(map[string]*usageTotal)
	for _, record := range records {
		name := strings.TrimSpace(record.ModelName)
		if name == "" {
			continue
		}
		item := byModel[name]
		if item == nil {
			item = &usageTotal{Username: name}
			byModel[name] = item
		}
		item.Quota += record.Quota
		item.Tokens += record.TokenUsed
		item.Count += record.Count
	}
	result := make([]usageTotal, 0, len(byModel))
	for _, item := range byModel {
		result = append(result, *item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Quota != result[j].Quota {
			return result[i].Quota > result[j].Quota
		}
		return result[i].Username < result[j].Username
	})
	return result
}

func parseUsageRank(value string) (int, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.TrimPrefix(normalized, "top")
	if strings.HasPrefix(normalized, "前") && strings.HasSuffix(normalized, "名") {
		normalized = strings.TrimSuffix(strings.TrimPrefix(normalized, "前"), "名")
	}
	count, err := strconv.Atoi(normalized)
	if err != nil || count < 1 || count > 100 {
		return 0, errors.New("排行榜名次必须是 1 到 100 的整数，例如 /usage 7d 10")
	}
	return count, nil
}

func formatInsightRange(start, end time.Time, location *time.Location) string {
	return start.In(location).Format("2006-01-02 15:04") + " ～ " + end.In(location).Format("2006-01-02 15:04 MST")
}

func formatLogTime(timestamp int64, location *time.Location) string {
	if timestamp <= 0 {
		return "未知时间"
	}
	return time.Unix(timestamp, 0).In(location).Format("01-02 15:04:05")
}

func usageHelp() string {
	return "用法：/usage [today|7d|month] 查看自己；管理员可用 /usage <用户ID或@用户> 7d；/usage <时间长度> all 查看全站汇总；/usage <时间长度> <前N名> 查看排行榜；/usage chart 7d 生成图表。"
}

func logsHelp() string {
	return "用法：/logs [数量]；管理员可使用 /logs <用户ID或@用户> [数量]，数量范围 1-20。"
}
