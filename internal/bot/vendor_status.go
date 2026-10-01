package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

type c2cFileAPI interface {
	SendC2CFile(context.Context, string, string, string, int, []byte) (qq.SentMessage, error)
}

func (s *Service) handleVendorStatus(ctx context.Context, event qq.MessageEvent, fields []string) error {
	return s.handleVendorStatusWithProgress(ctx, event, fields, 10*time.Second)
}

func (s *Service) handleVendorStatusWithProgress(ctx context.Context, event qq.MessageEvent, fields []string, interval time.Duration) error {
	if len(fields) != 1 {
		return s.reply(ctx, event, "用法：/vendor_status")
	}
	cfg, err := s.vendorConfigSnapshot()
	if err != nil {
		return s.reply(ctx, event, "读取厂商配置失败："+s.vendorSafeError(err, nil))
	}
	timeout := time.Duration(cfg.WorkerTimeoutSeconds) * time.Second
	queryCtx, cancel := s.backgroundCommandContext(ctx, timeout+30*time.Second)
	defer cancel()
	return s.runVendorQueryWithProgress(queryCtx, event, cfg, interval)
}

func (s *Service) handleVendorSubscription(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) || s.isReadOnlyAdmin(identity) {
		return s.reply(ctx, event, "仅 Bot 管理员可执行厂商订阅指令。")
	}
	group := strings.TrimSpace(event.Message.GroupOpenID)
	if group == "" || event.EventType == "C2C_MESSAGE_CREATE" {
		return s.reply(ctx, event, "厂商订阅指令需在群聊中使用。")
	}
	if len(fields) != 2 || !strings.EqualFold(fields[1], "on") && !strings.EqualFold(fields[1], "off") {
		return s.reply(ctx, event, "用法：/vendor_subscribe on|off")
	}
	enabled := strings.EqualFold(fields[1], "on")
	s.vendorConfigMu.Lock()
	cfg, err := s.vendorConfigSnapshot()
	var changed bool
	var count int
	if err == nil {
		changed, count, err = s.store.SetVendorSubscription(group, enabled, cfg.GroupWhitelist)
	}
	s.vendorConfigMu.Unlock()
	if err != nil {
		s.logger.Error("保存厂商订阅失败", "error", err)
		return s.reply(ctx, event, "保存厂商订阅失败，请稍后重试。")
	}
	if !changed {
		if enabled {
			return s.reply(ctx, event, "当前群组已订阅厂商状态自动推送，无需重复开启。")
		}
		return s.reply(ctx, event, "当前群组未订阅厂商状态自动推送，无需关闭。")
	}
	_ = s.store.AddAudit(model.AuditRecord{
		At: time.Now(), Actor: commandRuleActor(identity), Action: "vendor.subscription",
		Target: group, Success: true, Metadata: map[string]any{"enabled": enabled},
	})
	s.signalVendorConfigChanged()
	if !enabled {
		return s.reply(ctx, event, fmt.Sprintf("已为当前群组关闭厂商状态自动订阅，将不再收到主动告警。\n剩余订阅群组数：%d。", count))
	}
	text := fmt.Sprintf("已为当前群组开启厂商状态自动订阅，下一轮轮询后开始推送告警。\n当前订阅群组数：%d。", count)
	if !cfg.Enabled {
		text += "\n自动监控当前全局关闭；管理员可使用 /vendor_config enabled on 即时启用。"
	}
	return s.reply(ctx, event, text)
}

func (s *Service) runVendorStatus(ctx context.Context, operation string) (vendorstatus.Packet, error) {
	return s.runVendorStatusProgress(ctx, operation, nil)
}

func (s *Service) runVendorStatusProgress(ctx context.Context, operation string, progress func(vendorstatus.Packet)) (vendorstatus.Packet, error) {
	select {
	case s.vendorSemaphore <- struct{}{}:
		defer func() { <-s.vendorSemaphore }()
	case <-ctx.Done():
		return vendorstatus.Packet{}, ctx.Err()
	}
	cfg, err := s.vendorConfigSnapshot()
	if err != nil {
		return vendorstatus.Packet{}, err
	}
	if operation == "cycle" && !cfg.Enabled {
		return vendorstatus.Packet{}, nil
	}
	if progress != nil {
		progress(vendorstatus.Packet{Type: "progress", Stage: "startup", Text: "正在启动状态采集进程……"})
	}
	s.vendorDisplayTimezone(&cfg)
	values, err := s.store.VendorStatusValues()
	if err != nil {
		return vendorstatus.Packet{}, err
	}
	workerCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.WorkerTimeoutSeconds)*time.Second)
	defer cancel()
	packet, runErr := s.vendorRunner.Run(workerCtx, vendorstatus.Request{Operation: operation, Config: cfg, Values: values}, vendorstatus.Callbacks{
		Checkpoint: s.store.PutVendorStatusValue,
		Progress: func(packet vendorstatus.Packet) {
			if progress != nil {
				packet.Text = vendorstatus.Redact(packet.Text, cfg)
				progress(packet)
			}
		},
		Send: func(sendCtx context.Context, packet vendorstatus.Packet) error {
			// Re-check the durable subscription at delivery time, not only at
			// the beginning of a potentially long polling cycle.
			current, err := s.vendorConfigSnapshot()
			if err != nil {
				return err
			}
			if !current.Enabled {
				return errors.New("厂商自动监控已关闭")
			}
			allowed := false
			for _, group := range current.GroupWhitelist {
				if group == packet.Group {
					allowed = true
					break
				}
			}
			if !allowed {
				return errors.New("目标群已取消厂商状态订阅")
			}
			event := qq.MessageEvent{Message: qq.Message{GroupOpenID: packet.Group}}
			if len(packet.PNG) != 0 {
				err = s.sendVendorImage(sendCtx, event, packet.PNG)
			} else if packet.Text != "" {
				err = s.reply(sendCtx, event, packet.Text)
			} else {
				err = errors.New("厂商状态告警内容为空")
			}
			if err != nil {
				s.logger.Warn("厂商状态告警投递失败，保留待重试进度", "group_openid", packet.Group, "error", err)
			}
			return err
		},
	})
	packet.Text = vendorstatus.Redact(packet.Text, cfg)
	return packet, vendorstatus.RedactError(runErr, cfg)
}

func (s *Service) sendVendorImage(ctx context.Context, event qq.MessageEvent, data []byte) error {
	return s.sendVendorImageWithSequence(ctx, event, data, 1)
}

type sequencedGroupFileAPI interface {
	SendGroupFileWithSequence(context.Context, string, string, string, int, []byte, int) (qq.SentMessage, error)
}

type sequencedC2CFileAPI interface {
	SendC2CFileWithSequence(context.Context, string, string, string, int, []byte, int) (qq.SentMessage, error)
}

func (s *Service) sendVendorImageWithSequence(ctx context.Context, event qq.MessageEvent, data []byte, sequence int) error {
	if event.EventType == "C2C_MESSAGE_CREATE" {
		user := firstNonEmpty(event.Message.Author.UserOpenID, event.Message.Author.ID)
		if api, ok := s.qq.(sequencedC2CFileAPI); ok {
			_, err := api.SendC2CFileWithSequence(ctx, user, event.Message.ID, "vendor-status.png", 1, data, sequence)
			return err
		}
		api, ok := s.qq.(c2cFileAPI)
		if !ok {
			return errors.New("当前 QQ 客户端不支持单聊图片上传")
		}
		if sequence > 1 {
			event.Message.ID = ""
		}
		_, err := api.SendC2CFile(ctx, user, event.Message.ID, "vendor-status.png", 1, data)
		return err
	}
	var sent qq.SentMessage
	var err error
	if api, ok := s.qq.(sequencedGroupFileAPI); ok {
		sent, err = api.SendGroupFileWithSequence(ctx, event.Message.GroupOpenID, event.Message.ID, "vendor-status.png", 1, data, sequence)
	} else if api, ok := s.qq.(groupFileAPI); ok {
		if sequence > 1 {
			event.Message.ID = ""
		}
		sent, err = api.SendGroupFile(ctx, event.Message.GroupOpenID, event.Message.ID, "vendor-status.png", 1, data)
	} else {
		return errors.New("当前 QQ 客户端不支持群图片上传")
	}
	if err == nil && sent.ID != "" {
		if saveErr := s.store.PutSentBotMessage(model.SentBotMessage{
			GroupOpenID: event.Message.GroupOpenID, MessageID: sent.ID,
			MessageIdx: sceneValue(sent.MessageScene.Ext, "msg_idx"), SentAt: time.Now(),
		}); saveErr != nil {
			s.logger.Warn("记录厂商状态图片消息失败", "error", saveErr)
		}
	}
	return err
}

func (s *Service) runVendorStatusWorker(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-s.notifyStop:
			cancel()
		case <-ctx.Done():
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.vendorConfigWake:
			timer.Stop()
			timer.Reset(0)
		case <-timer.C:
			cfg, err := s.vendorConfigSnapshot()
			if err != nil {
				s.logger.Warn("读取厂商后台配置失败", "error", err)
				timer.Reset(time.Minute)
				continue
			}
			if !cfg.Enabled {
				// Dormant until an administrator changes the configuration.
				timer.Stop()
				continue
			}
			// Wait for the official Gateway before initial collection/delivery.
			if s.gatewayConnected != nil && !s.gatewayConnected() {
				timer.Reset(5 * time.Second)
				continue
			}
			pollCtx, finishPoll := s.beginVendorPoll(ctx)
			_, err = s.runVendorStatus(pollCtx, "cycle")
			finishPoll()
			if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("厂商状态监控轮次失败", "error", err)
			}
			timer.Reset(time.Duration(cfg.PollIntervalSeconds) * time.Second)
		}
	}
}
