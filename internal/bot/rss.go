package bot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/rss"
	"github.com/fsykk/new-api-bot/internal/store"
)

type rssFeedClient interface {
	Fetch(context.Context, string, rss.Validators) (rss.Result, error)
	Close()
}

func (s *Service) handleRSS(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string) error {
	group := strings.TrimSpace(event.Message.GroupOpenID)
	if group == "" || event.EventType == "C2C_MESSAGE_CREATE" {
		return s.reply(ctx, event, "RSS 指令需在群聊中使用，各群订阅独立。")
	}
	if len(fields) < 2 {
		return s.reply(ctx, event, "请使用 /rss help 查看订阅管理命令。")
	}
	action := strings.ToLower(fields[1])
	expected := 3
	if action == "list" || action == "status" {
		expected = 2
	}
	switch action {
	case "add", "remove", "pause", "resume", "interval", "list", "status", "check":
	default:
		return s.reply(ctx, event, "未知 RSS 子命令，请使用 /rss help。")
	}
	if len(fields) != expected {
		return s.reply(ctx, event, "参数格式错误，请使用 /rss "+action+" help 查看用法。")
	}
	if action != "list" && action != "status" && !s.isAdmin(identity) {
		return s.reply(ctx, event, "仅 Bot 管理员可执行该 RSS 指令。")
	}
	if action != "list" && action != "status" && action != "check" && s.isReadOnlyAdmin(identity) {
		return s.reply(ctx, event, "只读管理员仅可执行查询类指令。")
	}
	if action == "list" || action == "status" {
		return s.replyRSSStatus(ctx, event, action == "status")
	}
	if action == "interval" {
		interval, err := time.ParseDuration(fields[2])
		if err != nil || interval < time.Minute || interval > 24*time.Hour {
			return s.reply(ctx, event, "RSS 检查间隔必须在 1m 至 24h 之间，例如 5m 或 1h。")
		}
		unlock := s.rssGroups.Lock(group)
		err = s.store.SetRSSGroupSettings(group, interval, time.Time{})
		unlock()
		if err != nil {
			return s.reply(ctx, event, "保存 RSS 检查间隔失败，请稍后重试。")
		}
		s.rssAudit(identity, group, action, interval.String())
		s.wakeRSS()
		return s.reply(ctx, event, "当前群 RSS 检查间隔已设为 "+interval.String()+"。")
	}
	// Serialize mutations against actual delivery, never against the poll's
	// network request. The persisted version rejects outdated fetch results.
	unlock := s.rssGroups.Lock(group)
	defer unlock()
	if action == "add" {
		address, err := rss.NormalizeURL(fields[2])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		subs, err := s.store.ListRSSSubscriptions(group)
		if err != nil {
			return s.reply(ctx, event, "读取 RSS 订阅失败，请稍后重试。")
		}
		for _, sub := range subs {
			if sub.URL == address {
				return s.reply(ctx, event, fmt.Sprintf("该源已订阅，编号：%s（%s）。", sub.ID, rss.CleanText(sub.Title, 200)))
			}
		}
		result, err := s.fetchRSS(ctx, address, rss.Validators{})
		if err != nil {
			return s.reply(ctx, event, "订阅失败："+err.Error())
		}
		sub, created, err := s.store.AddRSSSubscription(group, address, result, s.now())
		if err != nil {
			return s.reply(ctx, event, "保存 RSS 订阅失败，请稍后重试。")
		}
		if created {
			s.rssAudit(identity, group, action, sub.ID)
		}
		s.wakeRSS()
		return s.reply(ctx, event, fmt.Sprintf("已订阅 %s，编号：%s。\n已建立基线，只推送后续新文章。", rss.CleanText(sub.Title, 200), sub.ID))
	}
	sub, err := s.store.RSSSubscription(group, fields[2])
	if errors.Is(err, store.ErrNotFound) {
		return s.reply(ctx, event, "当前群不存在该 RSS 订阅编号。")
	}
	if err != nil {
		return s.reply(ctx, event, "读取 RSS 订阅失败，请稍后重试。")
	}
	if action == "check" {
		result, err := s.fetchRSS(ctx, sub.URL, rss.Validators{})
		if err != nil {
			return s.reply(ctx, event, "RSS 检查失败："+err.Error())
		}
		return s.reply(ctx, event, fmt.Sprintf("RSS 源可访问且解析成功：%s\n本次读取 %d 篇文章；未改变推送进度。", rss.CleanText(result.Title, 200), len(result.Articles)))
	}
	var baseline *rss.Result
	if action == "resume" {
		if sub.Enabled {
			return s.reply(ctx, event, "该 RSS 订阅已启用，无需恢复。")
		}
		result, err := s.fetchRSS(ctx, sub.URL, rss.Validators{})
		if err != nil {
			return s.reply(ctx, event, "恢复失败，订阅仍保持暂停："+err.Error())
		}
		baseline = &result
	}
	if action == "pause" && !sub.Enabled {
		return s.reply(ctx, event, "该 RSS 订阅已经暂停。")
	}
	if err := s.store.ChangeRSSSubscription(group, sub.ID, action == "remove", baseline, s.now()); err != nil {
		return s.reply(ctx, event, "保存 RSS 订阅设置失败，请稍后重试。")
	}
	s.rssAudit(identity, group, action, sub.ID)
	s.wakeRSS()
	text := "已暂停订阅，待发送文章已清除。"
	if action == "remove" {
		text = "已删除订阅及其待发送文章。"
	} else if action == "resume" {
		text = "已恢复订阅并重新建立基线，不补发暂停期间的文章。"
	}
	return s.reply(ctx, event, "RSS #"+sub.ID+"："+text)
}

func (s *Service) fetchRSS(ctx context.Context, address string, validators rss.Validators) (rss.Result, error) {
	if s.rssClient == nil {
		return rss.Result{}, errors.New("RSS 客户端配置无效")
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RSSHTTPTimeout)
	defer cancel()
	return s.rssClient.Fetch(requestCtx, address, validators)
}

func (s *Service) replyRSSStatus(ctx context.Context, event qq.MessageEvent, detailed bool) error {
	group := event.Message.GroupOpenID
	subscriptions, err := s.store.ListRSSSubscriptions(group)
	if err != nil {
		return s.reply(ctx, event, "读取 RSS 订阅失败，请稍后重试。")
	}
	settings, err := s.store.RSSGroupSettings(group)
	if err != nil {
		return s.reply(ctx, event, "读取 RSS 设置失败，请稍后重试。")
	}
	interval := settings.Interval
	if interval == 0 {
		interval = s.cfg.RSSPollInterval
	}
	lines := []string{fmt.Sprintf("当前群 RSS 订阅：%d 个；检查间隔：%s。", len(subscriptions), interval)}
	if !s.cfg.RSSEnabled {
		lines = append(lines, "RSS 后台抓取与推送已全局关闭，订阅设置仍保留。")
	}
	for _, sub := range subscriptions {
		state := "启用"
		if !sub.Enabled {
			state = "暂停"
		}
		lines = append(lines, fmt.Sprintf("#%s [%s] %s", sub.ID, state, rss.CleanText(sub.Title, 200)))
		if detailed {
			checked := "尚未检查"
			if !sub.LastChecked.IsZero() {
				checked = sub.LastChecked.Format(time.RFC3339)
			}
			lines = append(lines, "最近检查："+checked)
			if sub.LastError != "" {
				lines = append(lines, "采集错误："+sub.LastError)
			}
			if sub.DeliveryError != "" {
				lines = append(lines, sub.DeliveryError)
			}
		}
	}
	if detailed {
		_, count, err := s.store.RSSPending(group, 0)
		if err != nil {
			return s.reply(ctx, event, "读取 RSS 待发送状态失败。")
		}
		lines = append(lines, fmt.Sprintf("待发送：%d 篇。", count))
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1500)
}

func (s *Service) rssAudit(identity model.QQIdentity, group, action, id string) {
	metadata := map[string]any{"subscription_id": id}
	if action == "interval" {
		metadata = map[string]any{"interval": id}
	}
	_ = s.store.AddAudit(model.AuditRecord{
		At: s.now(), Actor: commandRuleActor(identity), Action: "rss." + action,
		Target: group, Success: true, Metadata: metadata,
	})
}

func (s *Service) wakeRSS() {
	select {
	case s.rssWake <- struct{}{}:
	default:
	}
}

func (s *Service) runRSSWorker(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-s.notifyStop:
			cancel()
		case <-ctx.Done():
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if s.cfg.RSSEnabled && (s.gatewayConnected == nil || s.gatewayConnected()) {
			if err := s.runRSSCycle(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("RSS 调度失败") // No raw network errors or URLs.
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.rssWake:
		}
	}
}

func (s *Service) runRSSCycle(ctx context.Context) error {
	if !s.cfg.RSSEnabled || (s.gatewayConnected != nil && !s.gatewayConnected()) {
		return nil
	}
	subs, err := s.store.ListRSSSubscriptions("")
	if err != nil {
		return err
	}
	byGroup := map[string][]rss.Subscription{}
	for _, sub := range subs {
		if sub.Enabled {
			byGroup[sub.Group] = append(byGroup[sub.Group], sub)
		}
	}
	now := s.now()
	dueGroups := []string{}
	byURL := map[string][]rss.Subscription{}
	for group, groupSubs := range byGroup {
		unlock := s.rssGroups.Lock(group)
		settings, err := s.store.RSSGroupSettings(group)
		if err != nil {
			unlock()
			return err
		}
		interval := settings.Interval
		if interval <= 0 {
			interval = s.cfg.RSSPollInterval
		}
		if !settings.LastCycle.IsZero() && now.Sub(settings.LastCycle) < interval {
			unlock()
			continue
		}
		err = s.store.SetRSSGroupSettings(group, 0, now)
		unlock()
		if err != nil {
			return err
		}
		dueGroups = append(dueGroups, group)
		for _, sub := range groupSubs {
			byURL[sub.URL] = append(byURL[sub.URL], sub)
		}
	}
	addresses := make([]string, 0, len(byURL))
	for address := range byURL {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		snapshots := byURL[address]
		validators := snapshots[0].Validators
		for _, sub := range snapshots[1:] {
			if sub.Validators != validators {
				validators = rss.Validators{}
				break
			}
		}
		// Only one response is kept in memory; due groups share its result.
		result, fetchErr := s.fetchRSS(ctx, address, validators)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, snapshot := range snapshots {
			if err := s.store.RecordRSSPoll(snapshot, result, fetchErr, now); err != nil {
				return err
			}
		}
	}
	sort.Strings(dueGroups)
	for _, group := range dueGroups {
		if err := s.deliverRSSGroup(ctx, group); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Warn("RSS 群推送失败，将在下一轮重试", "group_openid", group)
		}
	}
	return nil
}

func (s *Service) deliverRSSGroup(ctx context.Context, group string) error {
	pending, _, err := s.store.RSSPending(group, 10)
	if err != nil {
		return err
	}
	for _, delivery := range pending {
		settings, err := s.store.RSSGroupSettings(group)
		if err != nil {
			return err
		}
		if wait := time.Until(settings.LastSent.Add(2 * time.Second)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		// Hold the group lock across the final subscription check and send,
		// so completed pause/remove commands prevent subsequent deliveries.
		unlock := s.rssGroups.Lock(group)
		current, err := s.store.RSSDeliveryCurrent(delivery)
		if err != nil || !current {
			unlock()
			if err != nil {
				return err
			}
			continue
		}
		if ctx.Err() != nil || !s.cfg.RSSEnabled || (s.gatewayConnected != nil && !s.gatewayConnected()) {
			unlock()
			return ctx.Err()
		}
		if err := s.store.MarkRSSSend(group, time.Now()); err != nil {
			unlock()
			return err
		}
		lines := []string{"【RSS · " + rss.CleanText(delivery.Source, 200) + "】", rss.CleanText(delivery.Title, 200)}
		if delivery.Summary != "" {
			lines = append(lines, rss.CleanText(delivery.Summary, 300))
		}
		if delivery.URL != "" {
			lines = append(lines, delivery.URL)
		}
		timeout := s.cfg.QQAPITimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		sendCtx, cancel := context.WithTimeout(ctx, timeout)
		err = s.sendGroupReply(sendCtx, group, "", strings.Join(lines, "\n"))
		cancel()
		// Space the next article from completion as well as from the start of
		// this attempt, including slow successful requests.
		if markErr := s.store.MarkRSSSend(group, time.Now()); markErr != nil {
			unlock()
			return markErr
		}
		saveErr := s.store.CompleteRSSDelivery(delivery, err != nil)
		unlock()
		if err != nil {
			return errors.New("RSS QQ 推送失败")
		}
		if saveErr != nil {
			return saveErr
		}
	}
	return nil
}
