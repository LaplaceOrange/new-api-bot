package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
)

func (s *Service) handleAdminCheckin(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if len(fields) == 2 {
		return s.showAdminCheckin(ctx, event)
	}
	if len(fields) == 4 && strings.EqualFold(fields[2], "edit") {
		return s.reply(ctx, event, "签到额度已采用动态规则，/admin checkin edit 不再修改发放值：奖励=min(max(昨日消耗额度, 1), 随机上限5~10)。")
	}
	return s.reply(ctx, event, "用法：/admin checkin")
}

func (s *Service) showAdminCheckin(ctx context.Context, event qq.MessageEvent) error {
	now := s.now().In(s.cfg.CheckinTimezone)
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
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	lines := []string{
		"今日签到统计（" + start.Format("2006-01-02 MST") + "）：",
		fmt.Sprintf("签到人数：%d", completed),
		"已发放额度：" + newapi.QuotaToDisplay(issuedQuota, status.QuotaPerUnit),
		"发放规则：min(max(昨日消耗额度, 1), 随机上限5~10)",
	}
	if pending > 0 {
		lines = append(lines, fmt.Sprintf("处理中签到：%d", pending))
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}
