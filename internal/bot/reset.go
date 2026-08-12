package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/resetradar"
	"github.com/fsykk/new-api-bot/internal/store"
)

const (
	resetSettlementInterval    = 30 * time.Second
	resetNotificationBatchSize = 16
)

func (s *Service) handleReset(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.cfg.ResetEnabled || s.resetRadar == nil {
		return s.reply(ctx, event, "重置监测与补偿活动功能当前已关闭。")
	}
	group := strings.TrimSpace(event.Message.GroupOpenID)
	if group == "" {
		return s.reply(ctx, event, "重置监测与补偿活动只能在群聊中使用。")
	}
	if len(fields) < 2 {
		return s.reply(ctx, event, resetUsage())
	}
	if _, err := s.ensureResetSettings(group); err != nil {
		return s.reply(ctx, event, "保存当前群的重置监测设置失败，请稍后重试。")
	}

	switch strings.ToLower(fields[1]) {
	case "check", "status":
		if len(fields) != 2 {
			return s.reply(ctx, event, "格式错误。正确用法：/reset check")
		}
		if strings.EqualFold(fields[1], "status") {
			return s.reply(ctx, event, "请使用正确指令：/reset check")
		}
		return s.replyResetStatus(ctx, event, group)
	case "join":
		if len(fields) != 2 {
			return s.reply(ctx, event, "格式错误。正确用法：/reset join")
		}
		if canonical == "" {
			return s.reply(ctx, event, "你尚未绑定 New API 账户，请先使用 /bind <邮箱或用户ID> 完成绑定。")
		}
		return s.joinResetActivity(ctx, event, canonical, identity, group)
	case "set":
		if !s.isAdmin(identity) {
			return s.reply(ctx, event, "你没有修改重置活动设置的权限。")
		}
		return s.setResetOption(ctx, event, group, fields)
	case "proxy":
		if !s.isAdmin(identity) {
			return s.reply(ctx, event, "你没有修改重置检测代理的权限。")
		}
		return s.setResetProxy(ctx, event, fields)
	default:
		return s.reply(ctx, event, resetUsage())
	}
}

func (s *Service) ensureResetSettings(group string) (model.ResetSettings, error) {
	setting, err := s.store.GetResetSettings(group)
	if err == nil {
		if !setting.Subscribed {
			setting.Subscribed = true
			err = s.store.PutResetSettings(setting)
		}
		return setting, err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.ResetSettings{}, err
	}
	setting = model.ResetSettings{
		GroupOpenID: group,
		Duration:    s.cfg.ResetDefaultDuration,
		WinnerCount: s.cfg.ResetDefaultWinners,
		Lookback:    s.cfg.ResetDefaultLookback,
		Subscribed:  true,
	}
	if setting.Duration <= 0 {
		setting.Duration = model.DefaultResetDuration
	}
	if setting.WinnerCount <= 0 {
		setting.WinnerCount = model.DefaultResetWinnerCount
	}
	if setting.Lookback <= 0 {
		setting.Lookback = model.DefaultResetLookback
	}
	return setting, s.store.PutResetSettings(setting)
}

func (s *Service) replyResetStatus(ctx context.Context, event qq.MessageEvent, group string) error {
	state, err := s.store.GetResetGroupState(group)
	if err != nil {
		return s.reply(ctx, event, "读取当前重置状态失败，请稍后重试。")
	}
	lines := []string{"当前重置状态：" + resetStageText(state.Stage)}
	if state.Stage == model.ResetStageConfirmed {
		activity, activityErr := s.store.GetActiveResetActivity(group)
		if activityErr == nil {
			lines = append(lines,
				"活动结束时间："+activity.EndsAt.In(s.cfg.CheckinTimezone).Format("2006-01-02 15:04:05 MST"),
				fmt.Sprintf("已参加：%d 人", activity.ParticipantCount),
				fmt.Sprintf("抽取人数：%d 人", activity.WinnerCount),
				"补偿范围：获奖者结束前近 "+formatResetDuration(activity.Lookback)+" 的消耗额度",
			)
		}
	}
	if state.Summary != "" {
		lines = append(lines, "信号摘要："+truncateResetText(state.Summary, 180))
	}
	if state.Source != "" {
		lines = append(lines, "来源："+state.Source)
	}
	if state.SourceURL != "" {
		lines = append(lines, state.SourceURL)
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}

func (s *Service) joinResetActivity(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, group string) error {
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "你尚未绑定 New API 账户，请先使用 /bind <邮箱或用户ID> 完成绑定。")
	}
	activity, joined, err := s.store.JoinResetActivity(group, model.ResetParticipant{
		NewAPIID:     binding.NewAPIID,
		CanonicalID:  canonical,
		MemberOpenID: identity.MemberOpenID,
	}, time.Now())
	if errors.Is(err, store.ErrResetActivityInactive) {
		return s.reply(ctx, event, "当前没有正在进行的重置补偿抽奖，无法参加。请使用 /reset check 查看状态。")
	}
	if err != nil {
		return s.reply(ctx, event, "参加重置补偿抽奖失败，请稍后重试。")
	}
	if !joined {
		return s.reply(ctx, event, fmt.Sprintf("你已经参加本轮重置补偿抽奖，无需重复参加。当前共有 %d 人参加。", activity.ParticipantCount))
	}
	return s.reply(ctx, event, fmt.Sprintf("参加成功！本轮活动将于 %s 结束，届时从参与者中随机抽取最多 %d 人。当前共有 %d 人参加。", activity.EndsAt.In(s.cfg.CheckinTimezone).Format("2006-01-02 15:04:05 MST"), activity.WinnerCount, activity.ParticipantCount))
}

func (s *Service) setResetOption(ctx context.Context, event qq.MessageEvent, group string, fields []string) error {
	if len(fields) != 4 {
		return s.reply(ctx, event, "格式错误。正确用法：/reset set duration <时长>、/reset set winners <人数> 或 /reset set lookback <时长>")
	}
	setting, err := s.ensureResetSettings(group)
	if err != nil {
		return s.reply(ctx, event, "读取重置活动设置失败，请稍后重试。")
	}
	key := strings.ToLower(fields[2])
	switch key {
	case "duration":
		value, parseErr := time.ParseDuration(fields[3])
		if parseErr != nil || value <= 0 || value > 7*24*time.Hour {
			return s.reply(ctx, event, "活动有效期必须是大于 0 且不超过 168h 的 Go duration，例如 5h。")
		}
		setting.Duration = value
	case "winners":
		value, parseErr := strconv.Atoi(fields[3])
		if parseErr != nil || value < 1 || value > 100 {
			return s.reply(ctx, event, "抽取人数必须是 1 到 100 的整数。")
		}
		setting.WinnerCount = value
	case "lookback":
		value, parseErr := time.ParseDuration(fields[3])
		if parseErr != nil || value <= 0 || value > 31*24*time.Hour {
			return s.reply(ctx, event, "补偿回溯时间必须是大于 0 且不超过 744h 的 Go duration，例如 24h。")
		}
		setting.Lookback = value
	default:
		return s.reply(ctx, event, "未知设置项。可用设置项：duration、winners、lookback。")
	}
	if err := s.store.PutResetSettings(setting); err != nil {
		return s.reply(ctx, event, "保存重置活动设置失败，请稍后重试。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: commandRuleActor(identityFromEvent(event)), Action: "reset.settings.update", Target: group, Success: true, Metadata: map[string]any{"setting": key, "value": fields[3]}})
	return s.reply(ctx, event, fmt.Sprintf("重置活动设置已更新：有效期 %s，抽取 %d 人，补偿近 %s 的消耗额度。新设置将在下一轮活动生效。", formatResetDuration(setting.Duration), setting.WinnerCount, formatResetDuration(setting.Lookback)))
}

func (s *Service) setResetProxy(ctx context.Context, event qq.MessageEvent, fields []string) error {
	if len(fields) != 3 {
		return s.reply(ctx, event, "格式错误。正确用法：/reset proxy <http://或socks5://代理链接>；关闭代理使用 /reset proxy off")
	}
	raw := strings.TrimSpace(fields[2])
	if strings.EqualFold(raw, "off") {
		if err := s.store.PutResetProxy(""); err != nil {
			return s.reply(ctx, event, "关闭重置检测代理失败，请稍后重试。")
		}
		return s.reply(ctx, event, "重置检测代理已关闭，后续 X 检测将直接连接。")
	}
	if err := resetradar.ValidateProxyURL(raw); err != nil {
		return s.reply(ctx, event, "代理链接无效，请检查协议、主机、端口和 URL 编码。仅支持 http:// 与 socks5://。")
	}
	encrypted, err := s.secure.Encrypt(raw)
	if err != nil {
		return s.reply(ctx, event, "加密代理配置失败，请稍后重试。")
	}
	if err := s.store.PutResetProxy(encrypted); err != nil {
		return s.reply(ctx, event, "保存重置检测代理失败，请稍后重试。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: commandRuleActor(identityFromEvent(event)), Action: "reset.proxy.update", Success: true, Metadata: map[string]any{"proxy": resetradar.MaskedProxy(raw)}})
	return s.reply(ctx, event, "重置检测代理已更新："+resetradar.MaskedProxy(raw)+"。该代理仅用于访问 X。")
}

func (s *Service) runResetPollWorker(ctx context.Context) {
	pollInterval := s.cfg.ResetPollInterval
	if pollInterval < time.Minute {
		pollInterval = 3 * time.Minute
	}
	pollTimer := time.NewTimer(time.Second)
	defer pollTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		case <-pollTimer.C:
			s.pollResetSignals(ctx)
			s.processDueResetNotifications(ctx)
			pollTimer.Reset(pollInterval)
		}
	}
}

func (s *Service) runResetSettlementWorker(ctx context.Context) {
	ticker := time.NewTicker(resetSettlementInterval)
	defer ticker.Stop()
	s.processDueResetActivities(ctx)
	s.processDueResetNotifications(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		case <-ticker.C:
			s.processDueResetActivities(ctx)
			s.processDueResetNotifications(ctx)
		}
	}
}

func (s *Service) pollResetSignals(parent context.Context) {
	settings, err := s.store.ListSubscribedResetSettings()
	if err != nil {
		s.logger.Error("读取重置监测群列表失败", "error", err)
		return
	}
	if len(settings) == 0 {
		return
	}
	if _, err := s.store.ExpireResetGroupStates(time.Now().Add(-s.cfg.ResetSignalMaxAge), time.Now()); err != nil {
		s.logger.Warn("清理过期重置信号状态失败", "error", err)
	}
	proxyURL := ""
	if encrypted, proxyErr := s.store.GetResetProxy(); proxyErr == nil {
		proxyURL, proxyErr = s.secure.Decrypt(encrypted)
		if proxyErr != nil {
			s.logger.Error("解密重置检测代理失败", "error", proxyErr)
			return
		}
	} else if !errors.Is(proxyErr, store.ErrNotFound) {
		s.logger.Error("读取重置检测代理失败", "error", proxyErr)
		return
	}
	snapshot, err := s.resetRadar.Fetch(parent, proxyURL)
	if err != nil {
		s.logger.Warn("重置信号检测失败", "error", err)
		return
	}
	for _, sourceErr := range snapshot.SourceErrors {
		s.logger.Debug("部分重置信号来源检测失败", "source", sourceErr.Source, "error", sourceErr.Err)
	}
	for _, signal := range snapshot.Signals {
		stored := resetSignalFromRadar(signal, snapshot.CheckedAt)
		if _, err := s.store.RecordResetSignal(stored); err != nil {
			s.logger.Error("保存重置信号失败", "signal_id", stored.ID, "error", err)
			continue
		}
		for _, setting := range settings {
			s.processResetSignalForGroup(parent, setting.GroupOpenID, stored)
		}
	}
}

func (s *Service) processResetSignalForGroup(ctx context.Context, group string, signal model.ResetSignal) {
	switch signal.Stage {
	case model.ResetStagePossible, model.ResetStageImminent:
		changed, err := s.store.ApplyResetSignalToGroup(group, signal)
		if err != nil {
			s.logger.Error("更新群重置状态失败", "group_openid", group, "signal_id", signal.ID, "error", err)
			return
		}
		if !changed {
			return
		}
	case model.ResetStageConfirmed:
		_, created, err := s.store.CreateResetActivityFromSignal(group, signal, time.Now())
		if err != nil {
			s.logger.Error("创建重置补偿活动失败", "group_openid", group, "signal_id", signal.ID, "error", err)
			return
		}
		if !created {
			return
		}
	}
}

func (s *Service) processDueResetActivities(ctx context.Context) {
	s.resetMu.Lock()
	defer s.resetMu.Unlock()
	activities, err := s.store.ListDueResetActivities(time.Now())
	if err != nil {
		s.logger.Error("读取到期重置活动失败", "error", err)
		return
	}
	for _, activity := range activities {
		if ctx.Err() != nil {
			return
		}
		s.settleResetActivity(ctx, activity)
	}
}

func (s *Service) settleResetActivity(ctx context.Context, activity model.ResetActivity) {
	if activity.Status == model.ResetActivityActive {
		participants, err := s.store.ListResetParticipants(activity.ID)
		if err != nil {
			s.logger.Error("读取重置活动参与者失败", "activity_id", activity.ID, "error", err)
			return
		}
		winners, err := selectResetWinners(participants, activity.WinnerCount)
		if err != nil {
			s.logger.Error("抽取重置活动获奖者失败", "activity_id", activity.ID, "error", err)
			return
		}
		awards, err := s.calculateResetAwards(ctx, activity, winners)
		if err != nil {
			s.logger.Warn("计算重置活动补偿额度失败", "activity_id", activity.ID, "error", publicError(err))
			return
		}
		activity, _, err = s.store.BeginResetSettlement(activity.ID, awards, time.Now())
		if err != nil {
			s.logger.Error("保存重置活动抽奖结果失败", "activity_id", activity.ID, "error", err)
			return
		}
	}

	for _, award := range activity.Awards {
		if ctx.Err() != nil {
			return
		}
		switch award.Status {
		case model.ResetAwardZero, model.ResetAwardGranted, model.ResetAwardFailed, model.ResetAwardPendingConfirmation:
			continue
		case model.ResetAwardGranting:
			_, _ = s.store.UpdateResetAward(activity.ID, award.NewAPIID, model.ResetAwardPendingConfirmation, "服务在额度写入期间重启，结果需要人工核查", time.Now())
			continue
		}
		if _, err := s.store.UpdateResetAward(activity.ID, award.NewAPIID, model.ResetAwardGranting, "", time.Now()); err != nil {
			s.logger.Error("保存重置补偿发放中状态失败", "activity_id", activity.ID, "user_id", award.NewAPIID, "error", err)
			return
		}
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.NewAPITimeout)
		err := s.newAPI.AddQuota(requestCtx, award.NewAPIID, award.RawQuota)
		cancel()
		status := model.ResetAwardGranted
		lastError := ""
		if err != nil {
			lastError = publicError(err)
			if isAmbiguousQuotaWrite(err) {
				status = model.ResetAwardPendingConfirmation
			} else {
				status = model.ResetAwardFailed
			}
		}
		if _, saveErr := s.store.UpdateResetAward(activity.ID, award.NewAPIID, status, lastError, time.Now()); saveErr != nil {
			s.logger.Error("保存重置补偿发放结果失败", "activity_id", activity.ID, "user_id", award.NewAPIID, "error", saveErr)
			return
		}
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: "reset-worker", Action: "reset.award", Target: strconv.Itoa(award.NewAPIID), Success: err == nil, Description: lastError, Metadata: map[string]any{"activity_id": activity.ID, "quota": award.RawQuota, "status": status}})
	}
	activity, err := s.store.GetResetActivity(activity.ID)
	if err != nil {
		return
	}
	_, err = s.store.CompleteResetActivity(activity.ID, time.Now())
	if err != nil {
		s.logger.Error("完成重置活动失败", "activity_id", activity.ID, "error", err)
		return
	}
	s.processDueResetNotifications(ctx)
}

func (s *Service) calculateResetAwards(ctx context.Context, activity model.ResetActivity, winners []model.ResetParticipant) ([]model.ResetAward, error) {
	if len(winners) == 0 {
		return []model.ResetAward{}, nil
	}
	end := activity.EndsAt
	start := end.Add(-activity.Lookback)
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.NewAPITimeout)
	rows, err := s.newAPI.ListUsageByUser(requestCtx, start, end)
	cancel()
	if err != nil {
		return nil, err
	}
	quotaByID := make(map[int]int64, len(winners))
	winnerIDs := make(map[int]struct{}, len(winners))
	missingIDs := false
	for _, winner := range winners {
		winnerIDs[winner.NewAPIID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := winnerIDs[row.UserID]; ok && row.UserID > 0 {
			quotaByID[row.UserID] += row.Quota
		} else if row.UserID == 0 && strings.TrimSpace(row.Username) != "" {
			missingIDs = true
		}
	}
	if missingIDs {
		userIDByUsername := make(map[string]int, len(winners))
		for _, winner := range winners {
			requestCtx, cancel := context.WithTimeout(ctx, s.cfg.NewAPITimeout)
			user, getErr := s.newAPI.GetUser(requestCtx, winner.NewAPIID)
			cancel()
			if getErr != nil {
				return nil, getErr
			}
			if username := strings.ToLower(strings.TrimSpace(user.Username)); username != "" {
				userIDByUsername[username] = winner.NewAPIID
			}
		}
		for _, row := range rows {
			if row.UserID != 0 {
				continue
			}
			if userID := userIDByUsername[strings.ToLower(strings.TrimSpace(row.Username))]; userID > 0 {
				quotaByID[userID] += row.Quota
			}
		}
	}
	awards := make([]model.ResetAward, 0, len(winners))
	for _, winner := range winners {
		quota := quotaByID[winner.NewAPIID]
		if quota < 0 {
			quota = 0
		}
		awards = append(awards, model.ResetAward{NewAPIID: winner.NewAPIID, RawQuota: quota, Status: model.ResetAwardPending})
	}
	return awards, nil
}

func (s *Service) resetCompletionMessage(ctx context.Context, activity model.ResetActivity) string {
	lines := []string{"重置补偿抽奖已结束。"}
	if len(activity.Awards) == 0 {
		lines = append(lines, "本轮没有用户参加，因此未抽取获奖者。")
	} else {
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.NewAPITimeout)
		status, statusErr := s.newAPI.GetStatus(requestCtx, false)
		cancel()
		lines = append(lines, "获奖用户与补偿额度：")
		for index, award := range activity.Awards {
			label := fmt.Sprintf("New API 用户 %d", award.NewAPIID)
			if mention := mentionMember(award.MemberOpenID); mention != "" {
				label = mention + "（用户 " + strconv.Itoa(award.NewAPIID) + "）"
			}
			result := "已发放"
			switch award.Status {
			case model.ResetAwardZero:
				result = "近回溯期消耗为 0，无需加额"
			case model.ResetAwardFailed:
				result = "发放失败"
			case model.ResetAwardPendingConfirmation:
				result = "发放结果待管理员确认"
			}
			amount := strconv.FormatInt(award.RawQuota, 10) + " 原始 quota"
			if statusErr == nil && status.QuotaPerUnit > 0 {
				amount = newapi.QuotaToDisplay(award.RawQuota, status.QuotaPerUnit)
			}
			lines = append(lines, fmt.Sprintf("%d. %s：%s 额度（%s）", index+1, label, amount, result))
		}
	}
	lines = append(lines, "本轮活动结算时，群重置状态已恢复为：未知。")
	return strings.Join(lines, "\n")
}

func (s *Service) processDueResetNotifications(ctx context.Context) {
	s.processDueResetNotificationsAt(ctx, time.Now())
}

func (s *Service) processDueResetNotificationsAt(ctx context.Context, now time.Time) {
	s.resetNotifyMu.Lock()
	defer s.resetNotifyMu.Unlock()

	notifications, err := s.store.ListDueResetNotifications(now, resetNotificationBatchSize)
	if err != nil {
		s.logger.Error("读取待发送重置通知失败", "error", err)
		return
	}
	for _, notification := range notifications {
		if ctx.Err() != nil {
			return
		}
		s.processResetNotificationAt(ctx, notification, now)
	}
}

func (s *Service) processResetNotificationAt(ctx context.Context, notification model.ResetNotification, now time.Time) {
	if notification.Kind == model.ResetNotificationActivityStarted && notification.NextChunk == 0 {
		activity, err := s.store.GetResetActivity(notification.ActivityID)
		if err != nil {
			s.retryResetNotification(notification, now, "读取活动状态失败", err)
			return
		}
		if activity.Status != model.ResetActivityActive || !now.Before(activity.EndsAt) {
			if err := s.store.MarkResetNotificationSuperseded(notification.ID, now); err != nil {
				s.logger.Error("跳过过期重置活动通知失败", "notification_id", notification.ID, "error", err)
			}
			return
		}
	}
	if len(notification.Chunks) == 0 {
		message, err := s.renderResetNotification(ctx, notification)
		if err != nil {
			s.retryResetNotification(notification, now, "重建通知内容失败", err)
			return
		}
		chunks := splitMessage(message, 1700)
		notification, err = s.store.PrepareResetNotification(notification.ID, chunks, now)
		if err != nil {
			s.retryResetNotification(notification, now, "保存通知内容失败", err)
			return
		}
	}
	if notification.Status != model.ResetNotificationPending {
		return
	}
	if notification.NextChunk < 0 || notification.NextChunk >= len(notification.Chunks) {
		s.retryResetNotification(notification, now, "通知分块进度无效", errors.New("next chunk is outside the frozen chunk list"))
		return
	}

	timeout := s.cfg.QQAPITimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	for notification.NextChunk < len(notification.Chunks) {
		if ctx.Err() != nil {
			return
		}
		chunkIndex := notification.NextChunk
		sendCtx, cancel := context.WithTimeout(ctx, timeout)
		sendErr := s.qq.ReplyGroup(sendCtx, notification.GroupOpenID, "", notification.Chunks[chunkIndex])
		cancel()
		if sendErr != nil {
			s.retryResetNotification(notification, now, "QQ 群消息发送失败", sendErr)
			return
		}
		updated, err := s.store.MarkResetNotificationChunkSent(notification.ID, chunkIndex, time.Now())
		if err != nil {
			s.logger.Error("保存重置通知分块进度失败，下一次可能重复当前分块", "notification_id", notification.ID, "chunk_index", chunkIndex, "error", err)
			return
		}
		notification = updated
	}
}

func (s *Service) retryResetNotification(notification model.ResetNotification, now time.Time, reason string, cause error) {
	retryAt := now.Add(resetNotificationRetryDelay(notification.Attempts + 1))
	if err := s.store.MarkResetNotificationFailed(notification.ID, reason, retryAt, now); err != nil {
		s.logger.Error("保存重置通知重试状态失败", "notification_id", notification.ID, "error", err)
	}
	s.logger.Warn("处理重置通知失败，稍后自动重试", "notification_id", notification.ID, "group_openid", notification.GroupOpenID, "attempt", notification.Attempts+1, "reason", reason, "error", cause)
}

func (s *Service) renderResetNotification(ctx context.Context, notification model.ResetNotification) (string, error) {
	switch notification.Kind {
	case model.ResetNotificationSignal:
		signal, err := s.resetNotificationSignal(notification)
		if err != nil {
			return "", err
		}
		lines := []string{"检测到" + resetStageText(signal.Stage) + "信号。", "来源：" + signal.Source}
		if signal.Summary != "" {
			lines = append(lines, "摘要："+truncateResetText(signal.Summary, 240))
		}
		if signal.URL != "" {
			lines = append(lines, signal.URL)
		}
		return strings.Join(lines, "\n"), nil
	case model.ResetNotificationActivityStarted:
		activity, err := s.store.GetResetActivity(notification.ActivityID)
		if err != nil {
			return "", err
		}
		signal, err := s.resetNotificationSignal(notification)
		if err != nil {
			return "", err
		}
		lines := []string{
			"已确认 Codex 用量额度重置，重置补偿抽奖现已开始。",
			"活动有效期：" + formatResetDuration(activity.EndsAt.Sub(activity.StartedAt)),
			"结束时间：" + activity.EndsAt.In(s.cfg.CheckinTimezone).Format("2006-01-02 15:04:05 MST"),
			fmt.Sprintf("抽取人数：最多 %d 人", activity.WinnerCount),
			"补偿内容：获奖者结束前近 " + formatResetDuration(activity.Lookback) + " 的实际消耗额度",
			"参加方式：/reset join",
			"来源：" + signal.Source,
		}
		if signal.Summary != "" {
			lines = append(lines, "摘要："+truncateResetText(signal.Summary, 240))
		}
		if signal.URL != "" {
			lines = append(lines, signal.URL)
		}
		return strings.Join(lines, "\n"), nil
	case model.ResetNotificationActivityCompleted:
		activity, err := s.store.GetResetActivity(notification.ActivityID)
		if err != nil {
			return "", err
		}
		return s.resetCompletionMessage(ctx, activity), nil
	default:
		return "", fmt.Errorf("未知重置通知类型 %q", notification.Kind)
	}
}

func (s *Service) resetNotificationSignal(notification model.ResetNotification) (model.ResetSignal, error) {
	signal := model.ResetSignal{
		ID:      notification.SignalID,
		Stage:   notification.SignalStage,
		Source:  notification.SignalSource,
		Summary: notification.SignalSummary,
		URL:     notification.SignalURL,
	}
	if signal.Source != "" && signal.Stage != "" {
		return signal, nil
	}
	stored, err := s.store.GetResetSignal(notification.SignalID)
	if err != nil {
		return model.ResetSignal{}, err
	}
	if signal.Stage == "" {
		signal.Stage = stored.Stage
	}
	if signal.Source == "" {
		signal.Source = stored.Source
	}
	if signal.Summary == "" {
		signal.Summary = stored.Summary
	}
	if signal.URL == "" {
		signal.URL = stored.URL
	}
	return signal, nil
}

func resetNotificationRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * 30 * time.Second
}

func selectResetWinners(participants []model.ResetParticipant, count int) ([]model.ResetParticipant, error) {
	if count <= 0 || len(participants) == 0 {
		return []model.ResetParticipant{}, nil
	}
	values := append([]model.ResetParticipant(nil), participants...)
	for index := len(values) - 1; index > 0; index-- {
		choice, err := rand.Int(rand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			return nil, err
		}
		target := int(choice.Int64())
		values[index], values[target] = values[target], values[index]
	}
	if count > len(values) {
		count = len(values)
	}
	return values[:count], nil
}

func resetSignalFromRadar(signal resetradar.Signal, detectedAt time.Time) model.ResetSignal {
	return model.ResetSignal{
		ID:         signal.ID,
		ExternalID: strings.TrimPrefix(strings.TrimPrefix(signal.ID, "x:"), "status:"),
		Source:     signal.Source,
		Stage:      resetStageFromRadar(signal.Stage),
		Summary:    strings.TrimSpace(signal.Text),
		URL:        signal.URL,
		OccurredAt: signal.CreatedAt,
		DetectedAt: detectedAt,
	}
}

func resetStageFromRadar(stage resetradar.Stage) model.ResetStage {
	switch stage {
	case resetradar.StagePossible:
		return model.ResetStagePossible
	case resetradar.StageImminent:
		return model.ResetStageImminent
	case resetradar.StageConfirmed:
		return model.ResetStageConfirmed
	default:
		return model.ResetStageUnknown
	}
}

func resetStageText(stage model.ResetStage) string {
	switch stage {
	case model.ResetStagePossible:
		return "可能重置"
	case model.ResetStageImminent:
		return "即将重置"
	case model.ResetStageConfirmed:
		return "确认重置（抽奖进行中）"
	default:
		return "未知"
	}
}

func formatResetDuration(value time.Duration) string {
	if value%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(value/time.Hour))
	}
	return value.String()
}

func truncateResetText(value string, maxRunes int) string {
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}

func resetUsage() string {
	return strings.Join([]string{
		"格式错误。可用指令：",
		"/reset check - 查看当前重置状态",
		"/reset join - 参加正在进行的补偿抽奖",
		"管理员：/reset set duration <时长>",
		"管理员：/reset set winners <人数>",
		"管理员：/reset set lookback <时长>",
		"管理员：/reset proxy <http://或socks5://代理链接|off>",
	}, "\n")
}
