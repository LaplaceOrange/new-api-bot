package bot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
)

var redemptionLogPattern = regexp.MustCompile(`兑换码ID\s*(\d+)`)

func (s *Service) handleBenefit(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.cfg.BenefitEnabled {
		return s.reply(ctx, event, "福利兑换码功能当前已关闭。")
	}
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有发放福利兑换码的权限。")
	}
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "福利兑换码只能在群聊中发放。")
	}
	if len(fields) != 5 {
		return s.reply(ctx, event, benefitUsage())
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rawQuota, err := newapi.DisplayToQuota(fields[1], status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "福利面额必须是大于 0 的有效站点显示额度。")
	}
	if cmp, cmpErr := newapi.CompareDisplay(fields[1], s.cfg.CreditMaxPerCommand); cmpErr != nil || cmp > 0 {
		return s.reply(ctx, event, "单个福利兑换码面额不能超过 "+s.cfg.CreditMaxPerCommand+"。")
	}
	count, err := strconv.Atoi(fields[2])
	if err != nil || count < 1 || count > s.cfg.BenefitMaxCount {
		return s.reply(ctx, event, fmt.Sprintf("福利数量必须是 1 到 %d 的整数。", s.cfg.BenefitMaxCount))
	}
	validHours, err := strconv.Atoi(fields[3])
	if err != nil || validHours < 1 || validHours > 24*31 {
		return s.reply(ctx, event, "有效期必须是 1 到 744 的整数小时。")
	}
	banDays, err := strconv.Atoi(fields[4])
	if err != nil || banDays < 1 || banDays > s.cfg.BenefitMaxBanDays {
		return s.reply(ctx, event, fmt.Sprintf("封禁时间必须是 1 到 %d 的整数天。", s.cfg.BenefitMaxBanDays))
	}

	s.benefitMu.Lock()
	defer s.benefitMu.Unlock()
	campaignID := benefitCampaignID(event.Message.ID)
	if existing, getErr := s.store.GetBenefitCampaign(campaignID); getErr == nil {
		if existing.Status == "active" || existing.Status == "pending" {
			return s.announceBenefitCampaign(ctx, event, existing)
		}
		return s.reply(ctx, event, "该福利发放事件已经处理完成。")
	}
	now := time.Now()
	campaign := model.BenefitCampaign{ID: campaignID, Name: campaignID, Actor: canonical, GroupOpenID: event.Message.GroupOpenID, DisplayCredit: fields[1], RawQuota: rawQuota, Count: count, ValidHours: validHours, BanDays: banDays, CreatedAt: now, ExpiresAt: now.Add(time.Duration(validHours) * time.Hour), Status: "pending"}
	if err := s.store.PutBenefitCampaign(campaign); err != nil {
		return s.reply(ctx, event, "保存福利活动失败。")
	}
	campaign, err = s.prepareBenefitCampaign(ctx, campaign)
	if err != nil {
		return s.reply(ctx, event, "生成福利兑换码失败："+publicError(err)+"。活动已保存，后台会继续恢复。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "benefit.create", Target: campaign.ID, Success: true, Metadata: map[string]any{"count": count, "credit": fields[1], "valid_hours": validHours, "ban_days": banDays}})
	return s.announceBenefitCampaign(ctx, event, campaign)
}

func (s *Service) prepareBenefitCampaign(ctx context.Context, campaign model.BenefitCampaign) (model.BenefitCampaign, error) {
	if !campaign.ExpiresAt.IsZero() && time.Now().After(campaign.ExpiresAt) {
		return campaign, fmt.Errorf("福利活动已经超过有效期")
	}
	items, err := s.newAPI.SearchRedemptions(ctx, campaign.Name, campaign.Count)
	if err != nil {
		return campaign, err
	}
	if len(items) < campaign.Count {
		remaining := campaign.Count - len(items)
		if _, err := s.newAPI.CreateRedemptions(ctx, campaign.Name, remaining, campaign.RawQuota, campaign.ExpiresAt); err != nil {
			return campaign, err
		}
		for attempt := 0; attempt < 3; attempt++ {
			items, err = s.newAPI.SearchRedemptions(ctx, campaign.Name, campaign.Count)
			if err == nil && len(items) == campaign.Count {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
		}
	}
	if err != nil {
		return campaign, err
	}
	if len(items) != campaign.Count {
		return campaign, fmt.Errorf("兑换码记录数量不符：期望 %d，实际 %d", campaign.Count, len(items))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	campaign.RedemptionIDs = make([]int, 0, len(items))
	campaign.EncryptedCodes = make([]string, 0, len(items))
	for _, item := range items {
		if item.ID <= 0 || strings.TrimSpace(item.Key) == "" {
			return campaign, fmt.Errorf("兑换码记录缺少 ID 或 Key")
		}
		encrypted, encErr := s.secure.Encrypt(item.Key)
		if encErr != nil {
			return campaign, encErr
		}
		campaign.RedemptionIDs = append(campaign.RedemptionIDs, item.ID)
		campaign.EncryptedCodes = append(campaign.EncryptedCodes, encrypted)
	}
	campaign.Status = "active"
	if err := s.store.PutBenefitCampaign(campaign); err != nil {
		return campaign, err
	}
	return campaign, nil
}

func (s *Service) announceBenefitCampaign(ctx context.Context, event qq.MessageEvent, campaign model.BenefitCampaign) error {
	if campaign.Status != "active" {
		return s.reply(ctx, event, "福利活动正在后台恢复，请稍后查看群消息。")
	}
	codes, err := s.decryptBenefitCodes(campaign)
	if err != nil {
		return s.reply(ctx, event, "读取福利兑换码失败。")
	}
	message := benefitMessage(campaign, codes)
	if err := s.replyChunked(ctx, event, message, 1700); err != nil {
		return err
	}
	campaign.Announced = true
	_ = s.store.PutBenefitCampaign(campaign)
	return nil
}

func (s *Service) announceBenefitProactive(ctx context.Context, campaign model.BenefitCampaign) error {
	codes, err := s.decryptBenefitCodes(campaign)
	if err != nil {
		return err
	}
	if err := s.sendGroupChunked(ctx, campaign.GroupOpenID, benefitMessage(campaign, codes), 1700); err != nil {
		return err
	}
	campaign.Announced = true
	return s.store.PutBenefitCampaign(campaign)
}

func (s *Service) decryptBenefitCodes(campaign model.BenefitCampaign) ([]string, error) {
	result := make([]string, 0, len(campaign.EncryptedCodes))
	for _, encrypted := range campaign.EncryptedCodes {
		value, err := s.secure.Decrypt(encrypted)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func benefitMessage(campaign model.BenefitCampaign, codes []string) string {
	lines := []string{"<@everyone>", fmt.Sprintf("🎁 发放福利：每个 $%s，共 %d 个，有效期 %d h。", campaign.DisplayCredit, campaign.Count, campaign.ValidHours), fmt.Sprintf("⚠️ 每个用户限领一个；有效期内领取超过一个将被封禁 %d day。", campaign.BanDays)}
	return strings.Join(append(lines, codes...), "\n")
}

func (s *Service) sendGroupChunked(ctx context.Context, group, content string, maxRunes int) error {
	chunks := splitMessage(content, maxRunes)
	for _, chunk := range chunks {
		if err := s.qq.ReplyGroup(ctx, group, "", chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) runBenefitWorker(ctx context.Context) {
	interval := s.cfg.BenefitCheckInterval
	if interval <= 0 {
		interval = time.Minute
	}
	s.checkBenefitLifecycle()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		case <-ticker.C:
			s.checkBenefitLifecycle()
		}
	}
}

func (s *Service) checkBenefitLifecycle() {
	s.benefitMu.Lock()
	defer s.benefitMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.NewAPITimeout*4)
	defer cancel()
	if s.cfg.BenefitEnabled {
		campaigns, err := s.store.ListBenefitCampaigns()
		if err != nil {
			s.logger.Error("读取福利活动失败", "error", err)
		} else {
			for _, campaign := range campaigns {
				s.processBenefitCampaign(ctx, campaign)
			}
		}
	}
	s.processBenefitBans(ctx)
}

func (s *Service) processBenefitCampaign(ctx context.Context, campaign model.BenefitCampaign) {
	if campaign.Status == "pending" {
		if time.Now().After(campaign.ExpiresAt) {
			campaign.Status = "expired"
			_ = s.store.PutBenefitCampaign(campaign)
			return
		}
		recovered, err := s.prepareBenefitCampaign(ctx, campaign)
		if err != nil {
			s.logger.Warn("恢复福利活动失败", "campaign", campaign.ID, "error", publicError(err))
			return
		}
		campaign = recovered
		if !campaign.Announced {
			if err := s.announceBenefitProactive(ctx, campaign); err != nil {
				s.logger.Warn("补发福利活动消息失败", "campaign", campaign.ID, "error", err)
				return
			}
			campaign.Announced = true
		}
	}
	if campaign.Status != "active" {
		return
	}
	if !campaign.Announced {
		if err := s.announceBenefitProactive(ctx, campaign); err != nil {
			s.logger.Warn("重试发送福利活动消息失败", "campaign", campaign.ID, "error", err)
			return
		}
		campaign.Announced = true
	}
	if err := s.detectBenefitViolations(ctx, campaign); err != nil {
		s.logger.Warn("检测福利活动违规失败", "campaign", campaign.ID, "error", publicError(err))
		return
	}
	if time.Now().After(campaign.ExpiresAt) {
		campaign.Status = "expired"
		_ = s.store.PutBenefitCampaign(campaign)
	}
}

func (s *Service) detectBenefitViolations(ctx context.Context, campaign model.BenefitCampaign) error {
	idSet := make(map[int]struct{}, len(campaign.RedemptionIDs))
	for _, id := range campaign.RedemptionIDs {
		idSet[id] = struct{}{}
	}
	used := make(map[int]map[int]struct{})
	end := time.Now()
	if end.After(campaign.ExpiresAt) {
		end = campaign.ExpiresAt
	}
	if !campaign.CreatedAt.Before(end) {
		return nil
	}
	for page := 1; page <= 100; page++ {
		logs, err := s.newAPI.ListLogsByType(ctx, campaign.CreatedAt, end, "", 1, page, 100)
		if err != nil {
			return err
		}
		for _, entry := range logs.Items {
			match := redemptionLogPattern.FindStringSubmatch(entry.Content)
			if len(match) != 2 {
				continue
			}
			redemptionID, _ := strconv.Atoi(match[1])
			if _, ok := idSet[redemptionID]; !ok {
				continue
			}
			if used[entry.UserID] == nil {
				used[entry.UserID] = make(map[int]struct{})
			}
			used[entry.UserID][redemptionID] = struct{}{}
		}
		if len(logs.Items) < 100 || page*100 >= logs.Total {
			break
		}
	}
	for userID, ids := range used {
		if len(ids) <= 1 {
			continue
		}
		if _, err := s.store.GetBenefitBan(campaign.ID, userID); err == nil {
			continue
		}
		s.disableBenefitViolator(ctx, campaign, userID, len(ids))
	}
	return nil
}

func (s *Service) disableBenefitViolator(ctx context.Context, campaign model.BenefitCampaign, userID, count int) {
	now := time.Now()
	ban := model.BenefitBan{Key: campaign.ID + "|" + strconv.Itoa(userID), CampaignID: campaign.ID, GroupOpenID: campaign.GroupOpenID, UserID: userID, RedeemCount: count, Status: "disable_failed"}
	err := s.newAPI.ManageUserStatus(ctx, userID, "disable")
	result := "封禁操作暂时失败，机器人会自动重试。"
	if err == nil {
		ban.Status = "disabled"
		ban.DisabledAt = now
		ban.EnableAt = now.AddDate(0, 0, campaign.BanDays)
		result = fmt.Sprintf("已封禁 %d day，将于 %s 自动重新启用。", campaign.BanDays, ban.EnableAt.In(s.cfg.CheckinTimezone).Format("2006-01-02 15:04"))
	} else {
		ban.LastError = publicError(err)
	}
	_ = s.store.PutBenefitBan(ban)
	_ = s.store.AddAudit(model.AuditRecord{At: now, Actor: "benefit-worker", Action: "benefit.disable", Target: strconv.Itoa(userID), Success: err == nil, Description: ban.LastError, Metadata: map[string]any{"campaign": campaign.ID, "redeem_count": count, "ban_days": campaign.BanDays}})
	message := fmt.Sprintf("🚫 福利违规处理\n用户 ID：%d\n违反规则：同一用户在福利有效期内限领一个，实际领取 %d 个。\n封禁时长：%d day\n处理结果：%s", userID, count, campaign.BanDays, result)
	if sendErr := s.qq.ReplyGroup(ctx, campaign.GroupOpenID, "", message); sendErr != nil {
		s.logger.Warn("发送福利违规通知失败", "user_id", userID, "error", sendErr)
	}
}

func (s *Service) processBenefitBans(ctx context.Context) {
	bans, err := s.store.ListBenefitBans()
	if err != nil {
		s.logger.Error("读取福利封禁记录失败", "error", err)
		return
	}
	now := time.Now()
	for _, ban := range bans {
		switch ban.Status {
		case "disable_failed":
			err = s.newAPI.ManageUserStatus(ctx, ban.UserID, "disable")
			if err != nil {
				ban.LastError = publicError(err)
				_ = s.store.PutBenefitBan(ban)
				continue
			}
			campaign, campaignErr := s.store.GetBenefitCampaign(ban.CampaignID)
			days := 1
			if campaignErr == nil {
				days = campaign.BanDays
			}
			ban.Status = "disabled"
			ban.DisabledAt = now
			ban.EnableAt = now.AddDate(0, 0, days)
			ban.LastError = ""
			_ = s.store.PutBenefitBan(ban)
			_ = s.qq.ReplyGroup(ctx, ban.GroupOpenID, "", fmt.Sprintf("🚫 用户 ID %d 的福利违规封禁已执行，封禁 %d day。", ban.UserID, days))
		case "disabled", "enable_failed":
			if ban.EnableAt.IsZero() || now.Before(ban.EnableAt) {
				continue
			}
			err = s.newAPI.ManageUserStatus(ctx, ban.UserID, "enable")
			if err != nil {
				ban.Status = "enable_failed"
				ban.LastError = publicError(err)
				_ = s.store.PutBenefitBan(ban)
				continue
			}
			ban.Status = "enabled"
			ban.LastError = ""
			_ = s.store.PutBenefitBan(ban)
			_ = s.store.AddAudit(model.AuditRecord{At: now, Actor: "benefit-worker", Action: "benefit.enable", Target: strconv.Itoa(ban.UserID), Success: true, Metadata: map[string]any{"campaign": ban.CampaignID}})
			_ = s.qq.ReplyGroup(ctx, ban.GroupOpenID, "", fmt.Sprintf("✅ 福利封禁期已结束，用户 ID %d 已自动重新启用。", ban.UserID))
		}
	}
}

func benefitCampaignID(messageID string) string {
	sum := sha256.Sum256([]byte(messageID))
	return "bf-" + fmt.Sprintf("%x", sum[:6])
}
func benefitUsage() string {
	return "格式错误。正确用法：/benefit <面额> <数量> <有效期(h)> <违者封禁时间(day)>\n例如：/benefit 1 20 24 7"
}
