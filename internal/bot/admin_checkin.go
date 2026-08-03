package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

func (s *Service) currentCheckinCredit() (string, error) {
	credit, err := s.store.GetCheckinCreditOverride()
	if err == nil {
		return strings.TrimSpace(credit), nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return s.cfg.CheckinCredit, nil
	}
	return "", err
}

func (s *Service) handleAdminCheckin(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if len(fields) == 2 {
		return s.showAdminCheckin(ctx, event)
	}
	if len(fields) != 4 || !strings.EqualFold(fields[2], "edit") {
		return s.reply(ctx, event, "用法：/admin checkin，或 /admin checkin edit <发放额度>")
	}
	credit := strings.TrimSpace(fields[3])
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rawQuota, err := newapi.DisplayToQuota(credit, status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "发放额度无效："+err.Error())
	}
	previous, err := s.currentCheckinCredit()
	if err != nil {
		return s.reply(ctx, event, "读取当前签到额度失败。")
	}
	if err := s.store.PutCheckinCreditOverride(credit); err != nil {
		return s.reply(ctx, event, "保存签到额度失败。")
	}
	_ = s.store.AddAudit(model.AuditRecord{
		At: time.Now(), Actor: canonical, Action: "checkin.credit.update", Target: "checkin_credit", Success: true,
		Metadata: map[string]any{"previous_credit": previous, "new_credit": credit, "raw_quota": rawQuota},
	})
	return s.reply(ctx, event, fmt.Sprintf("签到单次发放额度已从 %s 更新为 %s（原始 quota：%d），已立即生效。", previous, credit, rawQuota))
}

func (s *Service) showAdminCheckin(ctx context.Context, event qq.MessageEvent) error {
	now := time.Now().In(s.cfg.CheckinTimezone)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.cfg.CheckinTimezone)
	end := start.AddDate(0, 0, 1)
	records, err := s.store.ListCheckinsBetween(start, end)
	if err != nil {
		return s.reply(ctx, event, "读取今日签到统计失败。")
	}
	var completed, pending int
	var issuedQuota int64
	for _, record := range records {
		if record.Status == "completed" {
			completed++
			issuedQuota += record.RawQuota
		} else {
			pending++
		}
	}
	credit, err := s.currentCheckinCredit()
	if err != nil {
		return s.reply(ctx, event, "读取当前签到额度失败。")
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	lines := []string{
		"今日签到统计（" + start.Format("2006-01-02 MST") + "）：",
		fmt.Sprintf("签到人数：%d", completed),
		"已发放额度：" + newapi.QuotaToDisplay(issuedQuota, status.QuotaPerUnit),
		"当前单次发放额度：" + credit,
	}
	if pending > 0 {
		lines = append(lines, fmt.Sprintf("处理中签到：%d", pending))
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}
