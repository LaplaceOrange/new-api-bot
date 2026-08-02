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

func (s *Service) handleUsage(ctx context.Context, event qq.MessageEvent, canonical string, _ model.QQIdentity, fields []string) error {
	if len(fields) > 3 {
		return s.reply(ctx, event, usageHelp())
	}
	durationArg := "24h"
	if len(fields) >= 2 {
		durationArg = fields[1]
	}
	start, end, label, err := parseInsightRange(durationArg, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
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
	status, statusErr := s.newAPI.GetStatus(ctx, false)
	if statusErr != nil {
		return s.reply(ctx, event, publicError(statusErr))
	}
	lines := []string{
		fmt.Sprintf("%s用量（%s，New API 用户 %d）", targetDescription, label, userID),
		"时间范围：" + formatInsightRange(start, end, s.cfg.CheckinTimezone),
		"请求次数：" + strconv.FormatInt(total.Count, 10),
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
		lines = append(lines, fmt.Sprintf("Token：%d（输入 %d / 输出 %d）｜额度：%s｜耗时：%ds", item.PromptTokens+item.CompletionTokens, item.PromptTokens, item.CompletionTokens, newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit), item.UseTime))
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1700)
}

func (s *Service) handleModels(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if len(fields) != 1 {
		return s.reply(ctx, event, "格式错误。正确用法：/models")
	}
	models, err := s.newAPI.ListEnabledModels(ctx)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	binding, _ := s.store.GetBinding(canonical)
	user, userErr := s.newAPI.GetUser(ctx, binding.NewAPIID)
	header := fmt.Sprintf("站点当前已启用模型，共 %d 个", len(models))
	if userErr == nil && strings.TrimSpace(user.Group) != "" {
		header += "；你的用户分组：" + user.Group
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
			return s.reply(ctx, event, "当前未启用额度提醒。用法：/notify quota <额度>")
		}
		status := "等待额度低于阈值"
		if preference.Alerted {
			status = "本轮低额度提醒已发送，充值超过阈值后会自动重新启用"
		}
		return s.reply(ctx, event, fmt.Sprintf("额度提醒：已启用\n阈值：%s\nNew API 用户：%d\n状态：%s", preference.Threshold, preference.NewAPIID, status))
	}
	if len(fields) != 3 || !strings.EqualFold(fields[1], "quota") {
		return s.reply(ctx, event, "用法：/notify quota <额度>、/notify quota off 或 /notify status")
	}
	if strings.EqualFold(fields[2], "off") {
		_ = s.store.DeleteQuotaNotification(canonical)
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
	preference := model.QuotaNotification{
		CanonicalID: canonical, NewAPIID: binding.NewAPIID,
		GroupOpenID: event.Message.GroupOpenID, MemberOpenID: event.Message.Author.MemberOpenID,
		Threshold: fields[2], Enabled: true, Alerted: user.Quota <= rawThreshold,
	}
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
	for _, preference := range preferences {
		user, exists := byID[preference.NewAPIID]
		if !exists {
			continue
		}
		threshold, thresholdErr := newapi.DisplayToQuota(preference.Threshold, status.QuotaPerUnit)
		if thresholdErr != nil {
			continue
		}
		if user.Quota > threshold {
			if preference.Alerted {
				preference.Alerted = false
				preference.LastAlertAt = time.Time{}
				_ = s.store.PutQuotaNotification(preference)
			}
			continue
		}
		if preference.Alerted {
			continue
		}
		message := fmt.Sprintf("⚠️ 额度提醒\nNew API 用户 %d（%s）当前余额 %s，已低于或等于设置阈值 %s。", user.ID, nonEmpty(user.DisplayName, user.Username), newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit), preference.Threshold)
		if err := s.qq.ReplyGroup(ctx, preference.GroupOpenID, "", message); err != nil {
			s.logger.Warn("发送群内额度提醒失败", "newapi_user_id", user.ID, "error", err)
			continue
		}
		preference.Alerted = true
		preference.LastAlertAt = time.Now()
		_ = s.store.PutQuotaNotification(preference)
	}
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
	return "用法：/usage [时间长度] 查看自己；/usage <时间长度> all 查看全站汇总；/usage <时间长度> <前N名> 查看排行榜，例如 /usage 7d 10。时间示例：24h、7d、4w、today、month。"
}

func logsHelp() string {
	return "用法：/logs [数量]；管理员可使用 /logs <用户ID或@用户> [数量]，数量范围 1-20。"
}
