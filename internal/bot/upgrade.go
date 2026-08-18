package bot

import (
	"context"
	"errors"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/store"
)

const (
	upgradeStartingMessage = "机器人正在更新！"
	upgradeFinishedMessage = "机器人更新完毕！"
)

func (s *Service) AnnounceUpgradeStart(ctx context.Context) error {
	groups, err := s.store.ListKnownGroups()
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		s.logger.Info("没有已知 QQ 群，跳过升级开始通知")
		return nil
	}
	if err := s.store.PutUpgradeNotification(model.UpgradeNotification{StartedAt: s.now(), Groups: groups}); err != nil {
		return err
	}
	var firstErr error
	for _, group := range groups {
		if err := s.qq.ReplyGroup(ctx, group, "", upgradeStartingMessage); err != nil {
			s.logger.Warn("发送机器人升级开始通知失败", "group_openid", group, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Service) runUpgradeCompletionWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		completed, err := s.sendPendingUpgradeCompletion(ctx)
		if err != nil {
			s.logger.Warn("发送机器人升级完成通知失败，稍后重试", "error", err)
		}
		if completed {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) sendPendingUpgradeCompletion(ctx context.Context) (bool, error) {
	notification, err := s.store.GetUpgradeNotification()
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if s.gatewayConnected != nil && !s.gatewayConnected() {
		return false, nil
	}
	var firstErr error
	for _, group := range notification.Groups {
		if err := s.qq.ReplyGroup(ctx, group, "", upgradeFinishedMessage); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.store.CompleteUpgradeNotificationGroup(group); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return false, firstErr
	}
	return true, nil
}
