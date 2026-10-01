package bot

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fsykk/new-api-bot/internal/config"
	"github.com/fsykk/new-api-bot/internal/mailer"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/resetradar"
	"github.com/fsykk/new-api-bot/internal/secure"
	"github.com/fsykk/new-api-bot/internal/store"
)

type NewAPI interface {
	GetStatus(context.Context, bool) (newapi.Status, error)
	GetUser(context.Context, int) (newapi.User, error)
	ListUsers(context.Context) ([]newapi.User, error)
	FindUserByEmail(context.Context, string) (newapi.User, error)
	AddQuota(context.Context, int, int64) error
	SubtractQuota(context.Context, int, int64) error
	ListUsageByUser(context.Context, time.Time, time.Time) ([]newapi.UsageRecord, error)
	ListUsageByModel(context.Context, time.Time, time.Time, string) ([]newapi.UsageRecord, error)
	ListLogs(context.Context, time.Time, time.Time, string, int, int) (newapi.LogPage, error)
	ListEnabledModels(context.Context) ([]string, error)
	ListUserSubscriptions(context.Context, int) ([]newapi.UserSubscriptionRecord, error)
	CreateUserSubscription(context.Context, int, int) error
	InvalidateUserSubscription(context.Context, int) error
	CreateRedemptions(context.Context, string, int, int64, time.Time) ([]string, error)
	SearchRedemptions(context.Context, string, int) ([]newapi.Redemption, error)
	ListLogsByType(context.Context, time.Time, time.Time, string, int, int, int) (newapi.LogPage, error)
	ManageUserStatus(context.Context, int, string) error
}

// usageByUsernameLister is implemented by New API clients that can query the
// model-level usage endpoint for one concrete username. It is deliberately
// kept optional so existing NewAPI test doubles and integrations remain
// source-compatible.
type usageByUsernameLister interface {
	ListUsageByUsername(context.Context, time.Time, time.Time, string) ([]newapi.UsageRecord, error)
}

type QQAPI interface {
	ReplyC2C(context.Context, string, string, string) error
	ReplyGroup(context.Context, string, string, string) error
}

type resetRadarClient interface {
	Fetch(context.Context, string) (resetradar.Snapshot, error)
	Latest(context.Context, string) (resetradar.Signal, error)
	Close()
}

const (
	maxCommandBytes         = 4 << 10
	maxPendingGatewayEvents = 512
)

type queuedGatewayEvent struct {
	key   string
	event qq.MessageEvent
}

type Service struct {
	cfg                     config.Config
	store                   *store.Store
	secure                  *secure.Box
	newAPI                  NewAPI
	qq                      QQAPI
	mailer                  mailer.Sender
	logger                  *slog.Logger
	queue                   chan queuedGatewayEvent
	workers                 sync.WaitGroup
	workersDone             chan struct{}
	checkins                keyedLocker[string]
	credits                 keyedLocker[int]
	plans                   keyedLocker[int]
	groupSettings           keyedLocker[string]
	notifyStop              chan struct{}
	dispatchStop            chan struct{}
	dispatchDone            chan struct{}
	inboxWake               chan struct{}
	started                 atomic.Bool
	stopOnce                sync.Once
	queueCloseOnce          sync.Once
	gatewayConnected        func() bool
	lifecycleCtx            context.Context
	inflightMu              sync.Mutex
	inflight                map[string]struct{}
	notifyMu                sync.Mutex
	groupLastNotify         map[string]time.Time
	benefitMu               sync.Mutex
	hongbaoGroups           keyedLocker[string]
	lastPruneAt             time.Time
	chartSemaphore          chan struct{}
	commandRulesMu          sync.Mutex
	commandRules            atomic.Pointer[[]model.CommandRule]
	resetRadar              resetRadarClient
	resetMu                 sync.Mutex
	resetNotifyMu           sync.Mutex
	resetGroups             keyedLocker[string]
	resetSettleWake         chan struct{}
	now                     func() time.Time
	randomCheckinMultiplier func() (int64, error)
	randomCheckinMaxCredit  func() (int64, error)
}

func New(cfg config.Config, storage *store.Store, box *secure.Box, newAPI NewAPI, qqAPI QQAPI, sender mailer.Sender, logger *slog.Logger) *Service {
	if cfg.GatewayQueueSize <= 0 {
		cfg.GatewayQueueSize = 64
	}
	if cfg.GatewayWorkers <= 0 {
		cfg.GatewayWorkers = 2
	}
	service := &Service{
		cfg: cfg, store: storage, secure: box, newAPI: newAPI, qq: qqAPI, mailer: sender, logger: logger,
		queue: make(chan queuedGatewayEvent, cfg.GatewayQueueSize), workersDone: make(chan struct{}), notifyStop: make(chan struct{}), dispatchStop: make(chan struct{}), dispatchDone: make(chan struct{}), inboxWake: make(chan struct{}, 1), lifecycleCtx: context.Background(), inflight: make(map[string]struct{}), groupLastNotify: make(map[string]time.Time), chartSemaphore: make(chan struct{}, 1), resetSettleWake: make(chan struct{}, 1), now: time.Now, randomCheckinMultiplier: randomCheckinMultiplier, randomCheckinMaxCredit: randomCheckinMaxCredit,
	}
	if cfg.ResetEnabled {
		service.resetRadar = resetradar.NewScanner(cfg.ResetHTTPTimeout, cfg.ResetSignalMaxAge)
	}
	return service
}

func (s *Service) SetGatewayConnectedFunc(fn func() bool) { s.gatewayConnected = fn }

func (s *Service) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	s.lifecycleCtx = ctx
	for i := 0; i < s.cfg.GatewayWorkers; i++ {
		s.workers.Add(1)
		go func(worker int) {
			defer s.workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case item, ok := <-s.queue:
					if !ok {
						return
					}
					s.processQueuedGatewayEvent(ctx, item)
				}
			}
		}(i)
	}
	if s.cfg.NotifyEnabled {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			s.runQuotaNotifier(ctx)
		}()
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		s.runBenefitWorker(ctx)
	}()
	if s.resetRadar != nil {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			defer s.resetRadar.Close()
			s.runResetPollWorker(ctx)
		}()
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			s.runResetSettlementWorker(ctx)
		}()
	}
	go s.runInboxDispatcher(ctx)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		s.runUpgradeCompletionWorker(ctx)
	}()
	go func() {
		s.workers.Wait()
		close(s.workersDone)
	}()
}

func (s *Service) Stop() {
	_ = s.StopContext(context.Background())
}

func (s *Service) StopContext(ctx context.Context) error {
	s.stopOnce.Do(func() {
		close(s.notifyStop)
		close(s.dispatchStop)
	})
	if !s.started.Load() {
		s.queueCloseOnce.Do(func() { close(s.queue) })
		return nil
	}
	select {
	case <-s.dispatchDone:
		s.queueCloseOnce.Do(func() { close(s.queue) })
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.workersDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backgroundCommandContext lets a long command outlive the short per-message
// deadline while still being canceled immediately when the service shuts down.
func (s *Service) backgroundCommandContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	stopLifecycle := context.AfterFunc(s.lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycle()
		cancel()
	}
}

func (s *Service) HandleGateway(ctx context.Context, event qq.MessageEvent) bool {
	group := firstNonEmpty(event.Message.GroupOpenID, event.Member.GroupOpenID, event.JoinRequest.GroupOpenID)
	if group != "" {
		if err := s.store.ObserveGroup(group); err != nil {
			s.logger.Warn("记录已知 QQ 群失败", "group_openid", group, "error", err)
		}
	}
	if event.Message.ID != "" {
		content := strings.TrimSpace(event.Message.Content)
		if content == "" || !strings.HasPrefix(content, "/") {
			s.logger.Debug("忽略非指令 QQ 消息", "event", event.EventType, "content_length", utf8.RuneCountInString(content))
			return true
		}
		if len(content) > maxCommandBytes {
			event.Message.Content = "/__command_too_long"
			event.Message.Mentions = nil
			event.Message.Elements = nil
		}
	}
	msgIndex := sceneValue(event.Message.Scene.Ext, "msg_idx")
	dedupKey := event.EventType + "|" + event.Message.ID + "|" + msgIndex
	if event.JoinRequest.JoinRequestID != "" {
		dedupKey = strings.Join([]string{event.EventType, event.JoinRequest.GroupOpenID, event.JoinRequest.MemberOpenID, event.JoinRequest.JoinRequestID}, "|")
	} else if event.Member.GroupOpenID != "" {
		dedupKey = fmt.Sprintf("%s|%s|%s|%d", event.EventType, event.Member.GroupOpenID, event.Member.MemberOpenID, event.Member.Timestamp)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		s.logger.Error("序列化 QQ Gateway 事件失败", "event", event.EventType, "error", err)
		return false
	}
	encryptedPayload, err := s.secure.Encrypt(string(payload))
	if err != nil {
		s.logger.Error("加密 QQ Gateway 事件失败", "event", event.EventType, "error", err)
		return false
	}
	pending, err := s.store.EnqueueGatewayEvent(dedupKey, []byte(encryptedPayload), time.Now(), s.cfg.MessageDedupTTL, maxPendingGatewayEvents)
	if err != nil {
		if errors.Is(err, store.ErrEventInboxFull) {
			s.logger.Warn("持久化命令收件箱已满，请求 Gateway 退避重投", "event", event.EventType)
		} else {
			s.logger.Error("持久化 QQ Gateway 事件失败", "event", event.EventType, "error", err)
		}
		return false
	}
	if !pending {
		return true
	}
	if !s.tryDispatchGatewayEvent(queuedGatewayEvent{key: dedupKey, event: event}) {
		s.wakeInboxDispatcher()
	}
	return true
}

func (s *Service) tryDispatchGatewayEvent(item queuedGatewayEvent) bool {
	s.inflightMu.Lock()
	if _, exists := s.inflight[item.key]; exists {
		s.inflightMu.Unlock()
		return true
	}
	s.inflight[item.key] = struct{}{}
	s.inflightMu.Unlock()
	select {
	case s.queue <- item:
		return true
	default:
		s.releaseGatewayEvent(item.key)
		return false
	}
}

func (s *Service) releaseGatewayEvent(key string) {
	s.inflightMu.Lock()
	delete(s.inflight, key)
	s.inflightMu.Unlock()
}

func (s *Service) wakeInboxDispatcher() {
	select {
	case s.inboxWake <- struct{}{}:
	default:
	}
}

func (s *Service) runInboxDispatcher(ctx context.Context) {
	defer close(s.dispatchDone)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		s.dispatchPendingGatewayEvents()
		select {
		case <-ctx.Done():
			return
		case <-s.dispatchStop:
			return
		case <-s.inboxWake:
		case <-ticker.C:
		}
	}
}

func (s *Service) dispatchPendingGatewayEvents() {
	limit := cap(s.queue) + s.cfg.GatewayWorkers + 1
	items, err := s.store.ListPendingGatewayEvents(limit)
	if err != nil {
		s.logger.Error("读取持久化命令收件箱失败", "error", err)
		return
	}
	for _, pending := range items {
		plaintext, err := s.secure.Decrypt(string(pending.Payload))
		if err != nil {
			s.logger.Error("解密持久化 QQ Gateway 事件失败，已移除损坏记录", "key", pending.Key, "error", err)
			_ = s.store.CompleteGatewayEvent(pending.Key)
			continue
		}
		var event qq.MessageEvent
		if err := json.Unmarshal([]byte(plaintext), &event); err != nil {
			s.logger.Error("解析持久化 QQ Gateway 事件失败，已移除损坏记录", "key", pending.Key, "error", err)
			_ = s.store.CompleteGatewayEvent(pending.Key)
			continue
		}
		if !s.tryDispatchGatewayEvent(queuedGatewayEvent{key: pending.Key, event: event}) {
			return
		}
	}
}

func (s *Service) processQueuedGatewayEvent(ctx context.Context, item queuedGatewayEvent) {
	if ctx.Err() != nil {
		s.releaseGatewayEvent(item.key)
		return
	}
	completed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("处理 QQ Gateway 事件时发生 panic，事件保留至下次重启恢复", "key", item.key, "panic", recovered)
		}
		if completed {
			s.releaseGatewayEvent(item.key)
			s.wakeInboxDispatcher()
		}
	}()
	s.process(ctx, item.event)
	if err := s.store.CompleteGatewayEvent(item.key); err != nil {
		s.logger.Error("完成命令后移除持久化收件箱记录失败", "key", item.key, "error", err)
		return
	}
	completed = true
}

func (s *Service) process(parent context.Context, event qq.MessageEvent) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout(s.cfg.NewAPITimeout, s.cfg.QQAPITimeout))
	defer cancel()
	if event.EventType == "GROUP_MEMBER_ADD" {
		s.handleMemberAdd(ctx, event)
		return
	}
	if event.EventType == "GROUP_JOIN_REQUEST" {
		s.handleGroupJoinRequest(ctx, event)
		return
	}
	content := strings.TrimSpace(event.Message.Content)
	if content == "" || !strings.HasPrefix(content, "/") {
		s.logger.Debug("忽略非指令 QQ 消息",
			"event", event.EventType,
			"content_length", utf8.RuneCountInString(content),
			"starts_with_slash", strings.HasPrefix(content, "/"),
		)
		return
	}
	// 兼容用户按帮助文本输入 <参数> 且未额外添加空格的情况。
	content = strings.TrimSpace(strings.NewReplacer("<", " ", ">", " ").Replace(content))
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return
	}
	command := strings.ToLower(fields[0])
	s.logger.Debug("开始处理 QQ 命令", "event", event.EventType, "command", command)
	identity := identityFromEvent(event)
	if s.isReadOnlyAdmin(identity) && readOnlyAdminWriteCommand(command, fields) {
		reply := s.reply
		if command == "/hongbao" && len(fields) > 1 && strings.EqualFold(fields[1], "new") {
			reply = s.replyHongbaoNotice
		}
		if err := reply(ctx, event, "只读管理员仅可执行查询类指令。"); err != nil {
			s.logger.Error("回复只读管理员权限拒绝失败", "command", command, "error", err)
		}
		return
	}
	if command == "/enable" || command == "/disable" {
		if err := s.handleCommandRule(ctx, event, identity, command, content); err != nil {
			s.logger.Error("处理命令关键词状态失败", "command", command, "error", err)
		}
		return
	}
	if keyword, blocked := s.matchDisabledCommand(content); blocked {
		s.logger.Info("命令命中禁用关键词，静默忽略", "command", command, "keyword", keyword)
		return
	}
	canonical, resolveErr := s.store.ResolveCanonical(identity)
	var err error
	switch command {
	case "/__command_too_long":
		err = s.replyWithAutoRecall(ctx, event, "指令内容过长，请缩短到 4096 字节以内后重试。")
	case "/help":
		if len(fields) != 1 {
			err = s.replyWithAutoRecall(ctx, event, "格式错误。正确用法：/help")
		} else {
			err = s.replyWithAutoRecall(ctx, event, s.filteredHelpText())
		}
	case "/whoami":
		if len(fields) != 1 {
			err = s.reply(ctx, event, "格式错误。正确用法：/whoami")
		} else {
			err = s.handleWhoAmI(ctx, event, identity)
		}
	case "/bind":
		err = s.handleBind(ctx, event, canonical, fields)
	case "/hongbao":
		err = s.handleHongbao(ctx, event, canonical, identity, fields)
	case "/reset":
		if len(fields) >= 2 && (strings.EqualFold(fields[1], "check") || strings.EqualFold(fields[1], "last")) {
			err = s.handleReset(ctx, event, canonical, identity, fields)
			break
		}
		fallthrough
	case "/link":
		if command == "/link" {
			err = s.reply(ctx, event, "当前已启用纯群聊模式，无需使用 /link；请直接在群内使用 /bind <邮箱或用户ID> 绑定。")
			break
		}
		if resolveErr != nil || canonical == "" {
			err = s.reply(ctx, event, "你尚未绑定 New API 账户，请在当前群内使用 /bind <邮箱或用户ID> 完成绑定。")
			break
		}
		if _, bindErr := s.store.GetBinding(canonical); bindErr != nil {
			err = s.reply(ctx, event, "你尚未绑定 New API 账户，请在当前群内使用 /bind <邮箱或用户ID> 完成绑定。")
			break
		}
		err = s.handleReset(ctx, event, canonical, identity, fields)
	case "/checkin", "/me", "/credit", "/plan", "/benefit", "/usage", "/logs", "/models",
		"/notify", "/welcome", "/join", "/mute", "/bot", "/recall", "/confirm", "/unbind", "/admin":
		// /checkin reset is an administrator-only operation and does not require
		// the issuer to have a personal binding.
		if command == "/checkin" && len(fields) >= 2 && strings.EqualFold(fields[1], "reset") {
			err = s.handleCheckinReset(ctx, event, identity, fields)
			break
		}
		if resolveErr != nil || canonical == "" {
			err = s.reply(ctx, event, "你尚未绑定 New API 账户，请在当前群内使用 /bind <邮箱或用户ID> 完成绑定。")
			break
		}
		if _, bindErr := s.store.GetBinding(canonical); bindErr != nil {
			err = s.reply(ctx, event, "你尚未绑定 New API 账户，请在当前群内使用 /bind <邮箱或用户ID> 完成绑定。")
			break
		}
		switch command {
		case "/checkin":
			err = s.handleCheckin(ctx, event, canonical, fields)
		case "/me":
			if len(fields) != 1 {
				err = s.reply(ctx, event, "格式错误。正确用法：/me")
			} else {
				err = s.handleMe(ctx, event, canonical)
			}
		case "/credit":
			err = s.handleCredit(ctx, event, canonical, identity, fields)
		case "/plan":
			err = s.handlePlan(ctx, event, canonical, identity, fields)
		case "/benefit":
			err = s.handleBenefit(ctx, event, canonical, identity, fields)
		case "/usage":
			err = s.handleUsage(ctx, event, canonical, identity, fields)
		case "/logs":
			err = s.handleLogs(ctx, event, canonical, identity, fields)
		case "/models":
			err = s.handleModels(ctx, event, canonical, identity, fields)
		case "/notify":
			if !s.cfg.NotifyEnabled {
				err = s.reply(ctx, event, "额度提醒功能当前已关闭。")
			} else {
				err = s.handleNotify(ctx, event, canonical, fields)
			}
		case "/welcome":
			err = s.handleWelcome(ctx, event, identity, fields, content)
		case "/join":
			err = s.handleJoinCommand(ctx, event, canonical, identity, fields, content)
		case "/mute":
			err = s.handleMute(ctx, event, canonical, identity, fields)
		case "/bot":
			err = s.handleBotStatus(ctx, event, fields)
		case "/recall":
			err = s.handleRecall(ctx, event, identity, fields)
		case "/confirm":
			if !s.cfg.AdminUserManagementEnabled {
				err = s.reply(ctx, event, "New API 用户状态管理功能当前已关闭。")
			} else {
				err = s.handleConfirm(ctx, event, canonical, identity, fields)
			}
		case "/unbind":
			err = s.handleUnbind(ctx, event, canonical, fields)
		case "/admin":
			err = s.handleAdmin(ctx, event, canonical, identity, fields)
		}
	default:
		err = s.replyWithAutoRecall(ctx, event, "未知指令，请使用 /help 查看可用指令。")
	}
	if err != nil {
		s.logger.Error("处理机器人命令失败", "command", command, "error", err)
	}
}

// commandTimeout leaves room for up to five sequential New API requests used
// by /checkin (status, user lookup, precise usage, compatibility usage, quota
// update) and the QQ reply.
func commandTimeout(newAPITimeout, qqAPITimeout time.Duration) time.Duration {
	const minimum = 25 * time.Second
	const replyReserve = 5 * time.Second
	if newAPITimeout <= 0 {
		newAPITimeout = 30 * time.Second
	}
	if qqAPITimeout <= 0 {
		qqAPITimeout = 10 * time.Second
	}
	timeout := newAPITimeout*5 + qqAPITimeout + replyReserve
	if timeout < minimum {
		return minimum
	}
	return timeout
}

func identityFromEvent(event qq.MessageEvent) model.QQIdentity {
	return model.QQIdentity{
		UnionOpenID:  event.Message.Author.UnionOpenID,
		UserOpenID:   event.Message.Author.UserOpenID,
		MemberOpenID: event.Message.Author.MemberOpenID,
		GroupOpenID:  event.Message.GroupOpenID,
	}
}

func (s *Service) handleWhoAmI(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity) error {
	candidates := identity.AdminCandidates()
	if len(candidates) == 0 {
		return s.reply(ctx, event, "当前事件没有可用的 OpenID 标识。")
	}
	return s.reply(ctx, event, "当前可用 OpenID：\n"+strings.Join(candidates, "\n"))
}

func (s *Service) handleBind(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if canonical == "" {
		return s.reply(ctx, event, "无法识别当前群成员身份，请稍后重试。")
	}
	if len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return s.handleBindStatus(ctx, event, canonical)
	}
	if len(fields) >= 2 && strings.EqualFold(fields[1], "verify") {
		if len(fields) != 3 || !isSixDigits(fields[2]) {
			return s.reply(ctx, event, "格式错误。正确用法：/bind verify <6位验证码>")
		}
		pending, err := s.store.GetPendingBind(canonical)
		if err != nil {
			return s.reply(ctx, event, "当前没有待验证的绑定请求，请先使用 /bind <邮箱或New API用户ID> 获取验证码。")
		}
		return s.verifyBinding(ctx, event, pending, fields[2])
	}
	if len(fields) >= 2 && strings.EqualFold(fields[1], "vertify") {
		return s.reply(ctx, event, "vertify 拼写错误，请使用：/bind verify <6位验证码>")
	}
	if len(fields) != 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/bind <邮箱或New API用户ID>；收到邮件后使用 /bind verify <6位验证码>。")
	}
	argument := strings.TrimSpace(fields[1])
	if strings.HasPrefix(argument, "@") || strings.HasPrefix(argument, "<@") {
		return s.reply(ctx, event, "/bind 不支持使用 @群成员，请填写目标账户的邮箱或正整数 New API 用户 ID。")
	}
	if _, err := s.store.GetBinding(canonical); err == nil {
		return s.reply(ctx, event, "当前 QQ 身份已经完成绑定，如需改绑请联系机器人管理员。")
	}

	var user newapi.User
	var err error
	if strings.Contains(argument, "@") {
		user, err = s.newAPI.FindUserByEmail(ctx, strings.ToLower(argument))
	} else {
		id, parseErr := strconv.Atoi(argument)
		if parseErr != nil || id <= 0 {
			return s.reply(ctx, event, "请输入有效的邮箱地址或正整数 New API 用户 ID。")
		}
		user, err = s.newAPI.GetUser(ctx, id)
	}
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	if user.Status != 1 {
		return s.reply(ctx, event, "该 New API 用户当前未启用，无法绑定。")
	}
	if strings.TrimSpace(user.Email) == "" {
		return s.reply(ctx, event, "该 New API 用户没有绑定邮箱，无法进行邮箱验证。")
	}
	if _, err := s.store.GetBindingByNewAPIID(user.ID); err == nil {
		return s.reply(ctx, event, "该 New API 用户已经被其他 QQ 身份绑定。")
	}

	now := time.Now()
	rateKeys := []string{"actor:" + canonical, "target:" + strconv.Itoa(user.ID)}
	wait, err := s.store.EmailRateRemaining(rateKeys, now, s.cfg.BindEmailWindow, s.cfg.BindEmailLimit)
	if err != nil {
		return s.reply(ctx, event, "检查邮件发送频率失败，请稍后重试。")
	}
	if wait > 0 {
		minutes := int(math.Ceil(wait.Minutes()))
		if minutes < 1 {
			minutes = 1
		}
		return s.reply(ctx, event, fmt.Sprintf("你已触发速率限制，请%d分钟后重试！", minutes))
	}

	code, err := randomDigits(6)
	if err != nil {
		return s.reply(ctx, event, "生成验证码失败，请稍后重试。")
	}
	pending := model.PendingBind{
		CanonicalID: canonical,
		NewAPIID:    user.ID,
		Email:       strings.ToLower(strings.TrimSpace(user.Email)),
		CodeMAC:     s.secure.MAC("bind:"+canonical, code),
		ExpiresAt:   now.Add(s.cfg.BindCodeTTL),
		CreatedAt:   now,
	}
	if err := s.store.PutPendingBind(pending); err != nil {
		return s.reply(ctx, event, "保存绑定请求失败，请稍后重试。")
	}
	status, statusErr := s.newAPI.GetStatus(ctx, false)
	systemName := "New API"
	if statusErr == nil && status.SystemName != "" {
		systemName = status.SystemName
	}
	if err := s.mailer.SendVerification(ctx, pending.Email, code, s.cfg.BindCodeTTL, systemName); err != nil {
		_ = s.store.DeletePendingBind(canonical)
		s.logger.Warn("发送绑定验证码邮件失败", "canonical", canonical, "error", err)
		return s.reply(ctx, event, "验证码邮件发送失败，请检查账户邮箱或联系管理员。")
	}
	if err := s.store.RecordEmailSent(rateKeys, now, s.cfg.BindEmailWindow); err != nil {
		s.logger.Warn("记录邮件限流状态失败", "error", err)
	}
	return s.reply(ctx, event, "验证码已发送至 "+store.MaskEmail(pending.Email)+"，请在有效期内直接在当前群发送 /bind verify <6位验证码> 完成绑定。")
}

func (s *Service) handleBindStatus(ctx context.Context, event qq.MessageEvent, canonical string) error {
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "当前 QQ 身份尚未绑定 New API 账户。正确用法：/bind <邮箱或New API用户ID>")
	}
	lines := []string{
		"当前绑定信息：",
		fmt.Sprintf("New API 用户 ID：%d", binding.NewAPIID),
	}
	if strings.TrimSpace(binding.Email) != "" {
		lines = append(lines, "绑定邮箱："+store.MaskEmail(binding.Email))
	}
	user, userErr := s.newAPI.GetUser(ctx, binding.NewAPIID)
	if userErr == nil {
		lines = append(lines,
			"用户名："+nonEmpty(user.DisplayName, user.Username),
			"账户状态："+userStatusText(user.Status),
		)
		if strings.TrimSpace(user.Email) != "" && strings.TrimSpace(binding.Email) == "" {
			lines = append(lines, "账户邮箱："+store.MaskEmail(user.Email))
		}
	} else {
		lines = append(lines, "账户详情：New API 拒绝查询，但本地绑定仍然有效。")
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}

func (s *Service) handleUnbind(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if len(fields) != 1 {
		return s.reply(ctx, event, "格式错误。正确用法：/unbind")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "当前 QQ 身份尚未绑定 New API 账户。")
	}
	removed, err := s.store.UnbindByNewAPIID(binding.NewAPIID)
	if err != nil {
		return s.reply(ctx, event, "解除绑定失败，请稍后重试。")
	}
	_ = s.store.DeleteQuotaNotification(canonical)
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "binding.self_delete", Target: strconv.Itoa(removed.NewAPIID), Success: true})
	return s.reply(ctx, event, fmt.Sprintf("已解除当前 QQ 身份与 New API 用户 %d 的绑定。", removed.NewAPIID))
}

func (s *Service) verifyBinding(ctx context.Context, event qq.MessageEvent, pending model.PendingBind, code string) error {
	now := time.Now()
	if now.After(pending.ExpiresAt) {
		_ = s.store.DeletePendingBind(pending.CanonicalID)
		return s.reply(ctx, event, "验证码已经过期，请重新执行 /bind <邮箱或用户ID> 获取验证码。")
	}
	if !s.secure.VerifyMAC("bind:"+pending.CanonicalID, code, pending.CodeMAC) {
		attempts, _ := s.store.IncrementPendingAttempts(pending.CanonicalID)
		remaining := s.cfg.BindCodeMaxAttempts - attempts
		if remaining <= 0 {
			_ = s.store.DeletePendingBind(pending.CanonicalID)
			return s.reply(ctx, event, "验证码错误次数过多，本次绑定请求已失效，请重新发送验证码。")
		}
		return s.reply(ctx, event, fmt.Sprintf("验证码不匹配，还可尝试 %d 次。", remaining))
	}
	binding := model.Binding{
		CanonicalID: pending.CanonicalID,
		NewAPIID:    pending.NewAPIID,
		Email:       pending.Email,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.CreateBinding(binding); err != nil {
		return s.reply(ctx, event, err.Error())
	}
	_ = s.store.AddAudit(model.AuditRecord{At: now, Actor: pending.CanonicalID, Action: "binding.create", Target: strconv.Itoa(pending.NewAPIID), Success: true})
	return s.reply(ctx, event, fmt.Sprintf("绑定成功：New API 用户 ID %d，邮箱 %s。", pending.NewAPIID, store.MaskEmail(pending.Email)))
}

func (s *Service) handleLink(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if event.EventType == "C2C_MESSAGE_CREATE" {
		if canonical == "" {
			return s.reply(ctx, event, "无法识别当前单聊身份。")
		}
		if _, err := s.store.GetBinding(canonical); err != nil {
			return s.reply(ctx, event, "请先使用 /bind 完成账户绑定，再生成群聊关联码。")
		}
		if len(fields) != 1 {
			return s.reply(ctx, event, "请直接发送 /link 获取群聊关联码。")
		}
		code, err := randomCode(8)
		if err != nil {
			return s.reply(ctx, event, "生成关联码失败，请稍后重试。")
		}
		mac := s.secure.MAC("link", strings.ToUpper(code))
		challenge := model.LinkChallenge{CodeMAC: mac, CanonicalID: canonical, ExpiresAt: time.Now().Add(s.cfg.LinkCodeTTL)}
		if err := s.store.PutLinkChallenge(mac, challenge); err != nil {
			return s.reply(ctx, event, "保存关联码失败，请稍后重试。")
		}
		return s.reply(ctx, event, "群聊关联码："+code+"\n请在目标群中发送 /link "+code+"。关联码仅可使用一次。")
	}
	if len(fields) != 2 || identity.GroupAlias() == "" {
		return s.reply(ctx, event, "请先私聊机器人发送 /link 获取关联码，然后在群内发送 /link <关联码>。")
	}
	code := strings.ToUpper(strings.TrimSpace(fields[1]))
	mac := s.secure.MAC("link", code)
	linkedCanonical, err := s.store.ConsumeLinkChallenge(mac, identity.GroupAlias(), time.Now())
	if err != nil {
		return s.reply(ctx, event, "关联码无效、已过期或已被使用，请重新私聊机器人获取。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: linkedCanonical, Action: "identity.link", Target: identity.GroupAlias(), Success: true})
	return s.reply(ctx, event, "群聊身份关联成功，现在可以在本群使用 /checkin 等指令。")
}

func (s *Service) handleMe(ctx context.Context, event qq.MessageEvent, canonical string) error {
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "未找到绑定信息。")
	}
	user, err := s.newAPI.GetUser(ctx, binding.NewAPIID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	available := user.Quota
	return s.reply(ctx, event, fmt.Sprintf("New API 用户：%d\n用户名：%s\n邮箱：%s\n可用额度：%s\n已用额度：%s", user.ID, nonEmpty(user.DisplayName, user.Username), store.MaskEmail(user.Email), newapi.QuotaToDisplay(available, status.QuotaPerUnit), newapi.QuotaToDisplay(user.UsedQuota, status.QuotaPerUnit)))
}

func (s *Service) handleCheckin(ctx context.Context, event qq.MessageEvent, canonical string, fields []string) error {
	if len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return s.handleCheckinStatus(ctx, event, canonical)
	}
	if len(fields) != 1 {
		return s.replyWithAutoRecall(ctx, event, "用法：/checkin 或 /checkin status")
	}
	if !s.cfg.CheckinEnabled {
		return s.replyWithAutoRecall(ctx, event, "签到功能当前未启用。")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.replyWithAutoRecall(ctx, event, "未找到绑定信息。")
	}
	now := s.now()
	period, next := periodKey(now, s.cfg.CheckinPeriod, s.cfg.CheckinTimezone)
	lockKey := canonical + "|" + period
	unlock := s.checkins.Lock(lockKey)
	defer unlock()
	if existing, existingErr := s.store.GetCheckin(canonical, period); existingErr == nil {
		return s.replyExistingCheckin(ctx, event, existing, next)
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		return s.replyWithAutoRecall(ctx, event, "读取签到状态失败，请稍后重试。")
	}

	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.replyWithAutoRecall(ctx, event, publicError(err))
	}
	rawQuota, usageQuota, multiplierTenths, err := s.dynamicCheckinQuota(ctx, binding.NewAPIID, now, status.QuotaPerUnit)
	if err != nil {
		return s.replyWithAutoRecall(ctx, event, "计算签到额度失败："+publicError(err))
	}
	credit := newapi.QuotaToDisplay(rawQuota, status.QuotaPerUnit)
	record := model.CheckinRecord{
		CanonicalID: canonical, NewAPIID: binding.NewAPIID, PeriodKey: period,
		RawQuota: rawQuota, DisplayCredit: credit, YesterdayUsage: usageQuota, YesterdayUsageDisplay: newapi.QuotaToDisplay(usageQuota, status.QuotaPerUnit), CreatedAt: now, UpdatedAt: now, Status: "pending",
	}
	record, created, err := s.store.ReserveCheckin(record)
	if err != nil {
		return s.replyWithAutoRecall(ctx, event, "保存签到状态失败，请稍后重试。")
	}
	if !created {
		return s.replyExistingCheckin(ctx, event, record, next)
	}
	if err := s.newAPI.AddQuota(ctx, binding.NewAPIID, rawQuota); err != nil {
		if isAmbiguousQuotaWrite(err) {
			record.Status = "pending_confirmation"
			record.UpdatedAt = time.Now()
			record.LastError = publicError(err)
			if saveErr := s.store.FinalizeCheckin(record); saveErr != nil {
				s.logger.Error("签到结果待确认状态保存失败", "canonical", canonical, "newapi_user_id", binding.NewAPIID, "error", saveErr)
			}
			_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: false, Description: "额度写入结果待确认：" + publicError(err), Metadata: map[string]any{"period": period, "quota": rawQuota, "yesterday_usage_quota": usageQuota, "random_multiplier_tenths": multiplierTenths}})
			return s.replyWithAutoRecall(ctx, event, "签到额度请求超时，发放结果待确认。请勿重复签到；如长时间未到账请联系管理员核查。")
		}
		_ = s.store.DeletePendingCheckin(record)
		_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: false, Description: publicError(err), Metadata: map[string]any{"period": period, "quota": rawQuota, "yesterday_usage_quota": usageQuota, "random_multiplier_tenths": multiplierTenths}})
		return s.replyWithAutoRecall(ctx, event, publicError(err))
	}
	record.Status = "completed"
	record.UpdatedAt = time.Now()
	if err := s.store.FinalizeCheckin(record); err != nil {
		s.logger.Error("签到额度已发放但保存完成状态失败", "canonical", canonical, "newapi_user_id", binding.NewAPIID, "error", err)
		return s.replyWithAutoRecall(ctx, event, "额度已经发放，但本地签到状态保存失败，请联系管理员核查，勿重复签到。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: true, Metadata: map[string]any{"period": period, "quota": rawQuota, "display_credit": credit, "yesterday_usage_quota": usageQuota, "random_multiplier_tenths": multiplierTenths}})
	return s.replyWithAutoRecall(ctx, event, fmt.Sprintf("🎉 签到成功！昨日用量：%s，获取额度：%s", newapi.QuotaToDisplay(usageQuota, status.QuotaPerUnit), credit))
}

func (s *Service) handleCheckinReset(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行管理员指令的权限。")
	}
	if len(fields) != 2 {
		return s.reply(ctx, event, "用法：/checkin reset")
	}
	period, _ := periodKey(s.now(), s.cfg.CheckinPeriod, s.cfg.CheckinTimezone)
	removed, err := s.store.ResetCheckins(period)
	if err != nil {
		return s.reply(ctx, event, "重置签到状态失败，请稍后重试。")
	}
	actor := strings.Join(identity.AdminCandidates(), ",")
	_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: actor, Action: "checkin.reset", Target: period, Success: true, Description: fmt.Sprintf("重置签到记录 %d 条", removed)})
	return s.reply(ctx, event, fmt.Sprintf("已重置当前签到周期（%s），共清除 %d 位用户的签到状态。", period, removed))
}

func (s *Service) replyExistingCheckin(ctx context.Context, event qq.MessageEvent, record model.CheckinRecord, next time.Time) error {
	if record.Status == "completed" {
		return s.replyWithAutoRecall(ctx, event, fmt.Sprintf("🎉 今日已签到。今日获取额度：%s", record.DisplayCredit))
	}
	if record.Status == "pending_confirmation" {
		return s.replyWithAutoRecall(ctx, event, "本周期签到额度发放结果待确认，请勿重复签到；如长时间未到账请联系管理员核查。")
	}
	return s.replyWithAutoRecall(ctx, event, "本周期签到请求正在处理中，请勿重复提交；如长时间未到账请联系管理员核查。")
}

func (s *Service) dynamicCheckinQuota(ctx context.Context, userID int, now time.Time, quotaPerUnit int64) (reward, yesterdayUsage, multiplierTenths int64, err error) {
	if quotaPerUnit <= 0 {
		return 0, 0, 0, errors.New("quota_per_unit 必须大于 0")
	}
	start, end := previousNaturalDay(now, s.cfg.CheckinTimezone)
	yesterdayUsage, err = s.checkinYesterdayUsage(ctx, start, end, userID)
	if err != nil {
		return 0, 0, 0, err
	}
	multiplierTenths, err = s.randomCheckinMultiplier()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("生成随机签到倍数失败: %w", err)
	}
	if multiplierTenths < 10 || multiplierTenths > 30 {
		return 0, 0, 0, errors.New("随机签到倍数无效")
	}
	maxCredit, err := s.randomCheckinMaxCredit()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("生成签到额度上限失败: %w", err)
	}
	if maxCredit < 5 || maxCredit > 10 {
		return 0, 0, 0, errors.New("随机签到额度上限无效")
	}
	// Keep the calculation in display-credit tenths. The quotient is the
	// exact value of (yesterday usage * multiplier) * 10 in display units.
	usageProduct := new(big.Int).Mul(big.NewInt(yesterdayUsage), big.NewInt(multiplierTenths))
	displayTenths := roundPositiveQuotient(usageProduct, big.NewInt(quotaPerUnit))
	if !displayTenths.IsInt64() {
		return 0, 0, 0, errors.New("签到额度超出支持范围")
	}
	displayTenthsValue := displayTenths.Int64()
	if displayTenthsValue < 10 {
		displayTenthsValue = 10
	}
	if displayTenthsValue > maxCredit*10 {
		displayTenthsValue = maxCredit * 10
	}
	// Convert the one-decimal display value back to the integer quota used by
	// New API, rounding half up only at the final storage boundary.
	rewardBig := new(big.Int).Mul(big.NewInt(displayTenthsValue), big.NewInt(quotaPerUnit))
	rewardBig = roundPositiveQuotient(rewardBig, big.NewInt(10))
	if !rewardBig.IsInt64() {
		return 0, 0, 0, errors.New("签到额度超出支持范围")
	}
	reward = rewardBig.Int64()
	return reward, yesterdayUsage, multiplierTenths, nil
}

// checkinYesterdayUsage prefers the username-filtered data endpoint when the
// client exposes it. The /api/data/users aggregation omits user_id on New API
// deployments, so matching those rows by the bound account's username is only
// a compatibility fallback.
func (s *Service) checkinYesterdayUsage(ctx context.Context, start, end time.Time, userID int) (int64, error) {
	username := ""
	userResolved := false
	var userErr error
	_, hasPreciseEndpoint := s.newAPI.(usageByUsernameLister)
	if hasPreciseEndpoint {
		user, resolveErr := s.newAPI.GetUser(ctx, userID)
		userResolved = true
		userErr = resolveErr
		if resolveErr == nil {
			username = strings.TrimSpace(user.Username)
			if username != "" {
				rows, queryErr := s.listCheckinUsageByUsername(ctx, start, end, username)
				if queryErr == nil {
					total, matched, sumErr := sumCheckinUsage(rows, userID, username)
					if sumErr != nil {
						return 0, sumErr
					}
					if matched {
						return total, nil
					}
				}
			}
		}
	}

	rows, err := s.newAPI.ListUsageByUser(ctx, start, end)
	if err != nil {
		return 0, err
	}
	total, matched, err := sumCheckinUsage(rows, userID, "")
	if err != nil {
		return 0, err
	}
	if matched {
		return total, nil
	}

	if !userResolved {
		user, resolveErr := s.newAPI.GetUser(ctx, userID)
		userResolved = true
		userErr = resolveErr
		if resolveErr == nil {
			username = strings.TrimSpace(user.Username)
		}
	}
	if username != "" {
		total, matched, err = sumCheckinUsage(rows, userID, username)
		if err != nil {
			return 0, err
		}
		if matched {
			return total, nil
		}
		// Older NewAPI implementations may not expose the optional method,
		// but they still support the existing model-level endpoint.
		if !hasPreciseEndpoint {
			modelRows, modelErr := s.newAPI.ListUsageByModel(ctx, start, end, username)
			if modelErr == nil {
				total, matched, err = sumCheckinUsage(modelRows, userID, username)
				if err != nil {
					return 0, err
				}
				if matched || len(modelRows) == 0 {
					return total, nil
				}
			}
		}
	} else if len(rows) > 0 && userErr != nil {
		return 0, fmt.Errorf("解析昨日用量所属用户失败: %w", userErr)
	}
	return 0, nil
}

func (s *Service) listCheckinUsageByUsername(ctx context.Context, start, end time.Time, username string) ([]newapi.UsageRecord, error) {
	if lister, ok := s.newAPI.(usageByUsernameLister); ok {
		return lister.ListUsageByUsername(ctx, start, end, username)
	}
	return s.newAPI.ListUsageByModel(ctx, start, end, username)
}

func sumCheckinUsage(rows []newapi.UsageRecord, userID int, username string) (total int64, matched bool, err error) {
	// Prefer exact IDs whenever the response contains them. This avoids double
	// counting a richer row together with a username-only compatibility row.
	if userID > 0 {
		hasExactID := false
		for _, row := range rows {
			if row.UserID != userID {
				continue
			}
			hasExactID = true
			matched = true
			if row.Quota <= 0 {
				continue
			}
			if row.Quota > math.MaxInt64-total {
				return 0, false, errors.New("昨日用量额度超出支持范围")
			}
			total += row.Quota
		}
		if hasExactID {
			return total, matched, nil
		}
	}

	username = strings.TrimSpace(username)
	if username == "" {
		return 0, false, nil
	}
	for _, row := range rows {
		if !strings.EqualFold(strings.TrimSpace(row.Username), username) {
			continue
		}
		matched = true
		if row.Quota <= 0 {
			continue
		}
		if row.Quota > math.MaxInt64-total {
			return 0, false, errors.New("昨日用量额度超出支持范围")
		}
		total += row.Quota
	}
	return total, matched, nil
}

func previousNaturalDay(now time.Time, location *time.Location) (time.Time, time.Time) {
	local := now.In(location)
	end := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return end.AddDate(0, 0, -1), end
}

func randomCheckinMultiplier() (int64, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(21))
	if err != nil {
		return 0, err
	}
	return value.Int64() + 10, nil
}

func randomCheckinMaxCredit() (int64, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(6))
	if err != nil {
		return 0, err
	}
	return value.Int64() + 5, nil
}

func roundPositiveQuotient(numerator, denominator *big.Int) *big.Int {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() > 0 && new(big.Int).Lsh(remainder, 1).Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func (s *Service) handleCheckinStatus(ctx context.Context, event qq.MessageEvent, canonical string) error {
	period, next := periodKey(time.Now(), s.cfg.CheckinPeriod, s.cfg.CheckinTimezone)
	record, err := s.store.GetCheckin(canonical, period)
	if err != nil {
		return s.replyWithAutoRecall(ctx, event, "本周期尚未签到。下次周期开始时间："+next.Format("2006-01-02 15:04 MST"))
	}
	status := "处理中"
	if record.Status == "completed" {
		status = "已签到"
	} else if record.Status == "pending_confirmation" {
		status = "待确认"
	}
	return s.replyWithAutoRecall(ctx, event, fmt.Sprintf("当前周期：%s\n签到状态：%s\n已发放额度：%s\n绑定用户 ID：%d\n下个周期：%s", period, status, record.DisplayCredit, record.NewAPIID, next.Format("2006-01-02 15:04 MST")))
}

// isAmbiguousQuotaWrite identifies a non-idempotent request which may have
// reached New API and been applied, but whose response was not received.
func isAmbiguousQuotaWrite(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var apiErr *newapi.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 0 || apiErr.StatusCode == http.StatusRequestTimeout || apiErr.StatusCode >= 500 {
			return true
		}
		if apiErr.StatusCode >= 200 && apiErr.StatusCode < 300 && apiErr.Cause != nil {
			return true
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "awaiting headers") || strings.Contains(text, "context deadline exceeded")
}

func (s *Service) handleCredit(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行额度管理指令的权限。")
	}
	if len(fields) < 3 {
		return s.reply(ctx, event, "用法：/credit add|sub <用户ID或@用户> <额度>，或 /credit show <用户ID或@用户>")
	}
	action := strings.ToLower(fields[1])
	userID, targetDescription, err := s.resolveUserTarget(event, fields[2])
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	switch action {
	case "show":
		if len(fields) != 3 {
			return s.reply(ctx, event, "用法：/credit show <用户ID或@用户>")
		}
		user, err := s.newAPI.GetUser(ctx, userID)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		status, err := s.newAPI.GetStatus(ctx, false)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		return s.reply(ctx, event, fmt.Sprintf("%s，New API 用户 %d（%s）可用额度：%s，已用额度：%s", targetDescription, user.ID, nonEmpty(user.DisplayName, user.Username), newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit), newapi.QuotaToDisplay(user.UsedQuota, status.QuotaPerUnit)))
	case "add", "sub":
		if len(fields) != 4 {
			return s.reply(ctx, event, fmt.Sprintf("格式错误。正确用法：/credit %s <用户ID或@用户> <额度>", action))
		}
		if compare, err := newapi.CompareDisplay(fields[3], s.cfg.CreditMaxPerCommand); err != nil || compare > 0 {
			return s.reply(ctx, event, "额度必须为正数且不能超过单次上限 "+s.cfg.CreditMaxPerCommand+"。")
		}
		status, err := s.newAPI.GetStatus(ctx, false)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		rawQuota, err := newapi.DisplayToQuota(fields[3], status.QuotaPerUnit)
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		unlock := s.credits.Lock(userID)
		defer unlock()
		if action == "sub" {
			user, err := s.newAPI.GetUser(ctx, userID)
			if err != nil {
				return s.reply(ctx, event, publicError(err))
			}
			if rawQuota > user.Quota {
				return s.reply(ctx, event, fmt.Sprintf("扣除失败：%s 当前可用额度为 %s，不能扣除 %s，否则余额将为负数。本次未执行任何扣除。", targetDescription, newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit), fields[3]))
			}
			if err := s.newAPI.SubtractQuota(ctx, userID, rawQuota); err != nil {
				_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "credit.sub", Target: strconv.Itoa(userID), Success: false, Description: publicError(err)})
				if isAmbiguousQuotaWrite(err) {
					return s.reply(ctx, event, "额度扣除结果待确认，请先使用 /credit show 查询当前余额，确认后再决定是否重试。")
				}
				return s.reply(ctx, event, publicError(err))
			}
			remaining := user.Quota - rawQuota
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "credit.sub", Target: strconv.Itoa(userID), Success: true, Metadata: map[string]any{"display_credit": fields[3], "quota": rawQuota, "remaining_quota": remaining}})
			return s.reply(ctx, event, fmt.Sprintf("已从%s绑定的 New API 用户 %d 扣除额度 %s，剩余额度 %s。", targetDescription, userID, fields[3], newapi.QuotaToDisplay(remaining, status.QuotaPerUnit)))
		}
		if err := s.newAPI.AddQuota(ctx, userID, rawQuota); err != nil {
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "credit.add", Target: strconv.Itoa(userID), Success: false, Description: publicError(err)})
			if isAmbiguousQuotaWrite(err) {
				return s.reply(ctx, event, "额度增加结果待确认，请先使用 /credit show 查询当前余额，确认后再决定是否重试。")
			}
			return s.reply(ctx, event, publicError(err))
		}
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "credit.add", Target: strconv.Itoa(userID), Success: true, Metadata: map[string]any{"display_credit": fields[3], "quota": rawQuota}})
		return s.reply(ctx, event, fmt.Sprintf("已为%s绑定的 New API 用户 %d 增加额度 %s。", targetDescription, userID, fields[3]))
	default:
		return s.reply(ctx, event, "用法：/credit add|sub <用户ID或@用户> <额度>，或 /credit show <用户ID或@用户>")
	}
}

func (s *Service) resolveUserTarget(event qq.MessageEvent, token string) (int, string, error) {
	if userID, err := strconv.Atoi(token); err == nil && userID > 0 {
		return userID, "指定目标", nil
	}
	if !strings.HasPrefix(token, "@") && !strings.HasPrefix(token, "<@") {
		return 0, "", errors.New("目标用户必须是正整数 New API 用户 ID，或当前群内被 @ 的已绑定用户。")
	}
	if event.Message.GroupOpenID == "" {
		return 0, "", errors.New("@用户作为目标仅支持群聊消息。")
	}
	mention, err := selectTargetMention(event.Message.Mentions, token)
	if err != nil {
		return 0, "", err
	}
	memberOpenID := firstNonEmpty(mention.MemberOpenID, mention.ID, mention.UserOpenID)
	if memberOpenID == "" {
		return 0, "", errors.New("QQ 事件未提供被 @ 用户的 member_openid，无法查询其绑定账户。")
	}
	targetIdentity := model.QQIdentity{
		UnionOpenID:  mention.UnionOpenID,
		UserOpenID:   mention.UserOpenID,
		MemberOpenID: memberOpenID,
		GroupOpenID:  event.Message.GroupOpenID,
	}
	canonical, err := s.store.ResolveCanonical(targetIdentity)
	if err != nil || canonical == "" {
		return 0, "", errors.New("无法识别被 @ 用户的 QQ 身份。")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return 0, "", errors.New("被 @ 的用户尚未绑定 New API 账户。")
	}
	return binding.NewAPIID, "被 @ 用户", nil
}

func selectTargetMention(mentions []qq.MessageAuthor, token string) (qq.MessageAuthor, error) {
	candidates := make([]qq.MessageAuthor, 0, len(mentions))
	for _, mention := range mentions {
		if !mention.Bot {
			candidates = append(candidates, mention)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	normalized := strings.TrimSpace(token)
	normalized = strings.TrimPrefix(normalized, "<@!")
	normalized = strings.TrimPrefix(normalized, "<@")
	normalized = strings.TrimPrefix(normalized, "@")
	normalized = strings.TrimSuffix(normalized, ">")
	for _, mention := range candidates {
		for _, value := range []string{mention.MemberOpenID, mention.ID, mention.UserOpenID, mention.Username} {
			if value != "" && strings.EqualFold(value, normalized) {
				return mention, nil
			}
		}
	}
	if len(candidates) == 0 && normalized != "" {
		return qq.MessageAuthor{MemberOpenID: normalized}, nil
	}
	if len(candidates) > 1 {
		return qq.MessageAuthor{}, errors.New("指令中存在多个被 @ 用户，请每次只操作一个目标用户。")
	}
	return qq.MessageAuthor{}, errors.New("没有从 QQ 消息事件中识别到被 @ 的目标用户。")
}

func (s *Service) handlePlan(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if len(fields) < 2 {
		return s.reply(ctx, event, planUsage())
	}
	action := strings.ToLower(fields[1])
	switch action {
	case "view":
		var userID int
		var targetDescription string
		if len(fields) == 2 {
			binding, err := s.store.GetBinding(canonical)
			if err != nil {
				return s.reply(ctx, event, "当前 QQ 身份尚未绑定 New API 账户，请先使用 /bind 完成绑定。")
			}
			userID = binding.NewAPIID
			targetDescription = "你的账户"
		} else if len(fields) == 3 {
			if !s.isAdmin(identity) {
				return s.reply(ctx, event, "你没有查看其他用户订阅的权限；可使用 /plan view 查看自己的订阅。")
			}
			var err error
			userID, targetDescription, err = s.resolveUserTarget(event, fields[2])
			if err != nil {
				return s.reply(ctx, event, err.Error())
			}
		} else {
			return s.reply(ctx, event, "格式错误。正确用法：/plan view，管理员可使用 /plan view <用户ID或@用户>")
		}
		records, err := s.newAPI.ListUserSubscriptions(ctx, userID)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		return s.replySubscriptionList(ctx, event, userID, targetDescription, records)

	case "add":
		if !s.isAdmin(identity) {
			return s.reply(ctx, event, "你没有添加用户订阅的权限。")
		}
		if len(fields) != 4 {
			return s.reply(ctx, event, "格式错误。正确用法：/plan add <订阅套餐ID> <用户ID或@用户>")
		}
		planID, err := strconv.Atoi(fields[2])
		if err != nil || planID <= 0 {
			return s.reply(ctx, event, "订阅套餐 ID 必须是正整数。")
		}
		userID, targetDescription, err := s.resolveUserTarget(event, fields[3])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		unlock := s.plans.Lock(userID)
		defer unlock()
		before, err := s.newAPI.ListUserSubscriptions(ctx, userID)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		if err := s.newAPI.CreateUserSubscription(ctx, userID, planID); err != nil {
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "plan.add", Target: strconv.Itoa(userID), Success: false, Description: publicError(err), Metadata: map[string]any{"plan_id": planID}})
			if isAmbiguousQuotaWrite(err) {
				return s.reply(ctx, event, "添加订阅结果待确认，请先使用 /plan view 查询当前订阅，确认后再决定是否重试。")
			}
			return s.reply(ctx, event, "添加订阅失败："+publicError(err))
		}
		after, listErr := s.newAPI.ListUserSubscriptions(ctx, userID)
		subscriptionID := findCreatedSubscriptionID(before, after, planID)
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "plan.add", Target: strconv.Itoa(userID), Success: true, Metadata: map[string]any{"plan_id": planID, "subscription_id": subscriptionID}})
		if listErr != nil || subscriptionID <= 0 {
			return s.reply(ctx, event, fmt.Sprintf("订阅添加成功：%s绑定的 New API 用户 %d 已获得套餐 %d；但读取当前订阅编号失败，请使用 /plan view 查询。", targetDescription, userID, planID))
		}
		return s.reply(ctx, event, fmt.Sprintf("订阅添加成功！\n目标：%s（New API 用户 %d）\n套餐 ID：%d\n当前订阅编号：%d", targetDescription, userID, planID, subscriptionID))

	case "sub":
		if !s.isAdmin(identity) {
			return s.reply(ctx, event, "你没有取消用户订阅的权限。")
		}
		if len(fields) != 4 {
			return s.reply(ctx, event, "格式错误。正确用法：/plan sub <订阅编号> <用户ID或@用户>")
		}
		subscriptionID, err := strconv.Atoi(fields[2])
		if err != nil || subscriptionID <= 0 {
			return s.reply(ctx, event, "订阅编号必须是正整数。")
		}
		userID, targetDescription, err := s.resolveUserTarget(event, fields[3])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		unlock := s.plans.Lock(userID)
		defer unlock()
		records, err := s.newAPI.ListUserSubscriptions(ctx, userID)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		record, found := findSubscription(records, subscriptionID)
		if !found {
			return s.reply(ctx, event, fmt.Sprintf("取消订阅失败：订阅编号 %d 不属于%s绑定的 New API 用户 %d，本次未执行取消操作。", subscriptionID, targetDescription, userID))
		}
		if record.Subscription.Status != "active" {
			return s.reply(ctx, event, fmt.Sprintf("取消订阅失败：订阅编号 %d 当前状态为%s，无需重复取消。", subscriptionID, subscriptionStatusText(record.Subscription.Status)))
		}
		if err := s.newAPI.InvalidateUserSubscription(ctx, subscriptionID); err != nil {
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "plan.sub", Target: strconv.Itoa(userID), Success: false, Description: publicError(err), Metadata: map[string]any{"subscription_id": subscriptionID}})
			if isAmbiguousQuotaWrite(err) {
				return s.reply(ctx, event, "取消订阅结果待确认，请先使用 /plan view 查询当前订阅状态，确认后再决定是否重试。")
			}
			return s.reply(ctx, event, "取消订阅失败："+publicError(err))
		}
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "plan.sub", Target: strconv.Itoa(userID), Success: true, Metadata: map[string]any{"subscription_id": subscriptionID, "plan_id": record.Subscription.PlanID}})
		return s.reply(ctx, event, fmt.Sprintf("取消订阅成功！\n目标：%s（New API 用户 %d）\n当前订阅编号：%d\n订阅状态：已取消", targetDescription, userID, subscriptionID))
	default:
		return s.reply(ctx, event, planUsage())
	}
}

func findCreatedSubscriptionID(before, after []newapi.UserSubscriptionRecord, planID int) int {
	existing := make(map[int]struct{}, len(before))
	for _, record := range before {
		existing[record.Subscription.ID] = struct{}{}
	}
	bestID := 0
	for _, record := range after {
		sub := record.Subscription
		if sub.PlanID != planID {
			continue
		}
		if _, ok := existing[sub.ID]; ok {
			continue
		}
		if sub.ID > bestID {
			bestID = sub.ID
		}
	}
	return bestID
}

func findSubscription(records []newapi.UserSubscriptionRecord, subscriptionID int) (newapi.UserSubscriptionRecord, bool) {
	for _, record := range records {
		if record.Subscription.ID == subscriptionID {
			return record, true
		}
	}
	return newapi.UserSubscriptionRecord{}, false
}

func (s *Service) replySubscriptionList(ctx context.Context, event qq.MessageEvent, userID int, targetDescription string, records []newapi.UserSubscriptionRecord) error {
	sort.SliceStable(records, func(i, j int) bool {
		left := records[i].Subscription
		right := records[j].Subscription
		leftTime := left.CreatedAt
		if leftTime == 0 {
			leftTime = left.StartTime
		}
		rightTime := right.CreatedAt
		if rightTime == 0 {
			rightTime = right.StartTime
		}
		if leftTime != rightTime {
			return leftTime > rightTime
		}
		return left.ID > right.ID
	})
	if len(records) == 0 {
		return s.reply(ctx, event, fmt.Sprintf("%s（New API 用户 %d）当前没有订阅记录。", targetDescription, userID))
	}
	lines := []string{fmt.Sprintf("%s（New API 用户 %d）的全部订阅，共 %d 条（从新到旧）：", targetDescription, userID, len(records))}
	for _, record := range records {
		sub := record.Subscription
		lines = append(lines,
			fmt.Sprintf("\n订阅编号：%d｜套餐 ID：%d", sub.ID, sub.PlanID),
			"开始时间："+formatSubscriptionTime(sub.StartTime, s.cfg.CheckinTimezone),
			"结束时间："+formatSubscriptionTime(sub.EndTime, s.cfg.CheckinTimezone),
			"订阅状态："+subscriptionStatusText(sub.Status),
		)
	}
	return s.replyChunked(ctx, event, strings.Join(lines, "\n"), 1700)
}

func formatSubscriptionTime(timestamp int64, location *time.Location) string {
	if timestamp <= 0 {
		return "-"
	}
	return time.Unix(timestamp, 0).In(location).Format("2006-01-02 15:04:05 MST")
}

func subscriptionStatusText(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active":
		return "生效中（active）"
	case "expired":
		return "已过期（expired）"
	case "cancelled", "canceled":
		return "已取消（cancelled）"
	default:
		return nonEmpty(status, "未知")
	}
}

func planUsage() string {
	return "用法：\n/plan view - 查看自己的全部订阅\n管理员：/plan view <用户ID或@用户>\n管理员：/plan add <订阅套餐ID> <用户ID或@用户>\n管理员：/plan sub <订阅编号> <用户ID或@用户>"
}

func (s *Service) handleAdmin(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行管理员指令的权限。")
	}
	if len(fields) < 2 {
		return s.reply(ctx, event, "用法：/admin bindings [页码]、/admin unbind <用户ID或@用户>、/admin report [时间长度] 或 /admin checkin")
	}
	switch strings.ToLower(fields[1]) {
	case "bindings":
		if len(fields) != 2 && len(fields) != 3 {
			return s.reply(ctx, event, "格式错误。正确用法：/admin bindings [页码]")
		}
		page := 1
		if len(fields) == 3 {
			parsed, err := strconv.Atoi(fields[2])
			if err != nil || parsed < 1 {
				return s.reply(ctx, event, "页码必须是正整数。")
			}
			page = parsed
		}
		items, total, err := s.store.ListBindings(page, 10)
		if err != nil {
			return s.reply(ctx, event, "读取绑定列表失败。")
		}
		if len(items) == 0 {
			return s.reply(ctx, event, fmt.Sprintf("第 %d 页没有绑定记录，总数 %d。", page, total))
		}
		lines := []string{fmt.Sprintf("绑定列表 第 %d 页 / 共 %d 条：", page, total)}
		for _, item := range items {
			lines = append(lines, fmt.Sprintf("- 用户 %d | %s | %s", item.NewAPIID, store.MaskEmail(item.Email), item.CanonicalID))
		}
		return s.reply(ctx, event, strings.Join(lines, "\n"))
	case "unbind":
		if len(fields) != 3 {
			return s.reply(ctx, event, "用法：/admin unbind <用户ID或@用户>")
		}
		id, targetDescription, err := s.resolveUserTarget(event, fields[2])
		if err != nil {
			return s.reply(ctx, event, err.Error())
		}
		removed, err := s.store.UnbindByNewAPIID(id)
		if err != nil {
			return s.reply(ctx, event, "未找到该用户的绑定记录。")
		}
		_ = s.store.DeleteQuotaNotification(removed.CanonicalID)
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "binding.delete", Target: strconv.Itoa(id), Success: true, Metadata: map[string]any{"old_identity": removed.CanonicalID}})
		return s.reply(ctx, event, fmt.Sprintf("已解除%s绑定的 New API 用户 %d 的 QQ 绑定。", targetDescription, id))
	case "report":
		if len(fields) >= 3 && strings.EqualFold(fields[2], "export") {
			if !s.cfg.AdminReportExportEnabled {
				return s.reply(ctx, event, "CSV 管理报表导出功能当前已关闭。")
			}
			return s.handleAdminReportExport(ctx, event, fields)
		}
		return s.handleAdminReport(ctx, event, fields)
	case "checkin":
		return s.handleAdminCheckin(ctx, event, canonical, fields)
	case "user":
		if !s.cfg.AdminUserManagementEnabled {
			return s.reply(ctx, event, "New API 用户状态管理功能当前已关闭。")
		}
		return s.handleAdminUser(ctx, event, canonical, identity, fields)
	default:
		return s.reply(ctx, event, "用法：/admin bindings [页码]、/admin unbind <用户ID或@用户>、/admin report [时间长度] 或 /admin checkin")
	}
}

func (s *Service) isAdmin(identity model.QQIdentity) bool {
	for _, candidate := range identity.AdminCandidates() {
		if _, ok := s.cfg.QQAdminOpenIDs[candidate]; ok {
			return true
		}
		if _, ok := s.cfg.QQReadOnlyAdminOpenIDs[candidate]; ok {
			return true
		}
	}
	return false
}

func (s *Service) isReadOnlyAdmin(identity model.QQIdentity) bool {
	for _, candidate := range identity.AdminCandidates() {
		if _, full := s.cfg.QQAdminOpenIDs[candidate]; full {
			return false
		}
	}
	for _, candidate := range identity.AdminCandidates() {
		if _, readOnly := s.cfg.QQReadOnlyAdminOpenIDs[candidate]; readOnly {
			return true
		}
	}
	return false
}

// readOnlyAdminWriteCommand identifies commands that mutate bot, QQ, or New API
// state. Read-only administrators may continue to use query/report commands.
func readOnlyAdminWriteCommand(command string, fields []string) bool {
	command = strings.ToLower(strings.TrimSpace(command))
	arg := ""
	if len(fields) > 1 {
		arg = strings.ToLower(strings.TrimSpace(fields[1]))
	}
	switch command {
	case "/enable", "/disable":
		return arg != "list"
	case "/bind":
		return arg != "status"
	case "/unbind", "/checkin", "/notify", "/welcome", "/benefit", "/hongbao", "/recall":
		if command == "/notify" && arg == "status" {
			return false
		}
		if command == "/checkin" && arg == "status" {
			return false
		}
		return true
	case "/credit":
		return arg == "add" || arg == "sub"
	case "/plan":
		return arg == "add" || arg == "sub"
	case "/join":
		return arg != "status"
	case "/mute":
		return arg != "status"
	case "/confirm":
		return true
	case "/reset":
		return arg != "check" && arg != "status" && arg != "last"
	case "/admin":
		if arg == "" || arg == "bindings" || arg == "report" || arg == "checkin" {
			if arg == "report" && len(fields) > 2 && strings.EqualFold(fields[2], "export") {
				return false
			}
			return false
		}
		if arg == "user" {
			return len(fields) < 3 || !strings.EqualFold(fields[2], "status")
		}
		return true
	default:
		return false
	}
}

// groupMessageSender is implemented by QQ clients that expose the official
// "send group message" endpoint and return the created message metadata.
type groupMessageSender interface {
	SendGroupText(context.Context, string, string, string) (qq.SentMessage, error)
}

type sequencedGroupMessageSender interface {
	SendGroupTextWithSequence(context.Context, string, string, string, int) (qq.SentMessage, error)
}

func (s *Service) reply(ctx context.Context, event qq.MessageEvent, content string) error {
	if event.EventType == "C2C_MESSAGE_CREATE" {
		openID := event.Message.Author.UserOpenID
		if openID == "" {
			openID = event.Message.Author.ID
		}
		return s.qq.ReplyC2C(ctx, openID, event.Message.ID, content)
	}
	return s.sendGroupReply(ctx, event.Message.GroupOpenID, event.Message.ID, content)
}

// sendGroupReply sends a group text message through the official message
// endpoint when available and records it for /recall, otherwise falling back
// to the legacy ReplyGroup path.
func (s *Service) sendGroupReply(ctx context.Context, groupOpenID, replyTo, content string) error {
	return s.sendGroupReplyWithSequence(ctx, groupOpenID, replyTo, content, 1)
}

func (s *Service) sendGroupReplyWithSequence(ctx context.Context, groupOpenID, replyTo, content string, sequence int) error {
	var sent qq.SentMessage
	var err error
	if sender, ok := s.qq.(sequencedGroupMessageSender); ok {
		sent, err = sender.SendGroupTextWithSequence(ctx, groupOpenID, replyTo, content, sequence)
	} else {
		// Legacy clients cannot send a second reply sequence. Use the existing
		// independent group-message fallback rather than duplicate sequence 1.
		if sequence > 1 {
			replyTo = ""
		}
		if sender, ok := s.qq.(groupMessageSender); ok {
			sent, err = sender.SendGroupText(ctx, groupOpenID, replyTo, content)
		} else {
			return s.qq.ReplyGroup(ctx, groupOpenID, replyTo, content)
		}
	}
	if err == nil && sent.ID != "" {
		_ = s.store.PutSentBotMessage(model.SentBotMessage{GroupOpenID: groupOpenID, MessageID: sent.ID, MessageIdx: sceneValue(sent.MessageScene.Ext, "msg_idx"), SentAt: time.Now()})
	}
	return err
}

// replyWithAutoRecall sends a reply and schedules an automatic group recall
// after CheckinAutoRecallAfter. C2C replies are never recalled because the
// official QQ bot API only documents message recall for group chats.
func (s *Service) replyWithAutoRecall(ctx context.Context, event qq.MessageEvent, content string) error {
	return s.replyWithAutoRecallAfter(ctx, event, content, s.cfg.CheckinAutoRecallAfter)
}

// replyWithAutoRecallAfter permits command-specific recall delays without
// changing the check-in/help recall configuration.
func (s *Service) replyWithAutoRecallAfter(ctx context.Context, event qq.MessageEvent, content string, delay time.Duration) error {
	if event.EventType == "C2C_MESSAGE_CREATE" {
		return s.reply(ctx, event, content)
	}
	if delay <= 0 {
		return s.reply(ctx, event, content)
	}
	sender, ok := s.qq.(groupMessageSender)
	if !ok {
		return s.reply(ctx, event, content)
	}
	groupOpenID := event.Message.GroupOpenID
	sent, err := sender.SendGroupText(ctx, groupOpenID, event.Message.ID, content)
	if err == nil && sent.ID != "" {
		_ = s.store.PutSentBotMessage(model.SentBotMessage{GroupOpenID: groupOpenID, MessageID: sent.ID, MessageIdx: sceneValue(sent.MessageScene.Ext, "msg_idx"), SentAt: time.Now()})
		s.scheduleGroupMessageRecall(groupOpenID, delay, sent.ID, event.Message.ID)
	}
	return err
}

// scheduleGroupMessageRecall recalls the given group messages after the delay
// with the QQ official "delete group message" endpoint. It is used to withdraw
// the bot reply together with the user command that triggered it. QQ only
// allows recalling messages within two minutes of sending, so callers should
// keep the delay well below that bound. Recall is best-effort; individual
// failures are only logged and never retried.
func (s *Service) scheduleGroupMessageRecall(groupOpenID string, after time.Duration, messageIDs ...string) {
	if groupOpenID == "" || after <= 0 {
		return
	}
	targets := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		if strings.TrimSpace(id) != "" {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return
	}
	api, ok := s.qq.(groupRecallAPI)
	if !ok {
		s.logger.Debug("当前 QQ 客户端不支持自动消息撤回，跳过定时撤回", "group_openid", groupOpenID, "message_count", len(targets))
		return
	}
	go func() {
		timer := time.NewTimer(after)
		defer timer.Stop()
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-timer.C:
		}
		for _, messageID := range targets {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.lifecycleCtx), s.cfg.QQAPITimeout)
			err := api.RecallGroupMessage(ctx, groupOpenID, messageID)
			cancel()
			if err != nil {
				s.logger.Warn("自动撤回群消息失败", "group_openid", groupOpenID, "message_id", messageID, "recall_after", after.String(), "error", err)
			}
		}
	}()
}

func (s *Service) replyChunked(ctx context.Context, event qq.MessageEvent, content string, maxRunes int) error {
	if maxRunes < 200 || len([]rune(content)) <= maxRunes {
		return s.reply(ctx, event, content)
	}
	chunks := splitMessage(content, maxRunes)
	for index, chunk := range chunks {
		if index == 0 {
			if err := s.reply(ctx, event, chunk); err != nil {
				return err
			}
			continue
		}
		if event.EventType == "C2C_MESSAGE_CREATE" {
			openID := firstNonEmpty(event.Message.Author.UserOpenID, event.Message.Author.ID)
			if err := s.qq.ReplyC2C(ctx, openID, "", chunk); err != nil {
				return err
			}
		} else if err := s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, "", chunk); err != nil {
			return err
		}
	}
	return nil
}

func splitMessage(content string, maxRunes int) []string {
	lines := strings.Split(content, "\n")
	chunks := make([]string, 0, 2)
	current := make([]rune, 0, maxRunes)
	flush := func() {
		if len(current) == 0 {
			return
		}
		chunks = append(chunks, string(current))
		current = current[:0]
	}
	for _, line := range lines {
		runes := []rune(line)
		for len(runes) > 0 {
			separator := 0
			if len(current) > 0 {
				separator = 1
			}
			remaining := maxRunes - len(current) - separator
			if remaining <= 0 {
				flush()
				continue
			}
			if separator == 1 {
				current = append(current, '\n')
			}
			if len(runes) <= remaining {
				current = append(current, runes...)
				runes = nil
				continue
			}
			current = append(current, runes[:remaining]...)
			runes = runes[remaining:]
			flush()
		}
		if len(runes) == 0 && len(line) == 0 {
			if len(current) < maxRunes {
				current = append(current, '\n')
			} else {
				flush()
			}
		}
	}
	flush()
	return chunks
}

func periodKey(now time.Time, period string, location *time.Location) (string, time.Time) {
	local := now.In(location)
	switch period {
	case "weekly":
		year, week := local.ISOWeek()
		weekday := int(local.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start := time.Date(local.Year(), local.Month(), local.Day()-(weekday-1), 0, 0, 0, 0, location)
		return fmt.Sprintf("%04d-W%02d", year, week), start.AddDate(0, 0, 7)
	case "monthly":
		start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
		return local.Format("2006-01"), start.AddDate(0, 1, 0)
	default:
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		return local.Format("2006-01-02"), start.AddDate(0, 0, 1)
	}
}

func randomDigits(length int) (string, error) {
	const alphabet = "0123456789"
	return randomFromAlphabet(length, alphabet)
}

func randomCode(length int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	return randomFromAlphabet(length, alphabet)
}

func randomFromAlphabet(length int, alphabet string) (string, error) {
	result := make([]byte, length)
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	for i := range result {
		result[i] = alphabet[int(buffer[i])%len(alphabet)]
	}
	return string(result), nil
}

func isSixDigits(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func sceneValue(ext []string, key string) string {
	prefix := key + "="
	for _, value := range ext {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}

func publicError(err error) string {
	if err == nil {
		return "操作失败"
	}
	text := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " "))
	if strings.Contains(strings.ToLower(text), "no permission to update users of same or higher permission level") {
		return "该用户已经是管理员。"
	}
	if len([]rune(text)) > 350 {
		text = string([]rune(text)[:350]) + "…"
	}
	return text
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "-"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func userStatusText(status int) string {
	switch status {
	case 1:
		return "正常"
	case 2:
		return "已禁用"
	default:
		return fmt.Sprintf("状态码 %d", status)
	}
}

func helpText(cfg config.Config) string {
	lines := []string{
		"可用指令：",
		"/bind <邮箱或用户ID> - 在当前群发送绑定验证码",
		"/bind verify <验证码> - 在当前群完成绑定",
		"/bind status - 查看当前绑定信息",
		"/unbind - 解除当前 QQ 身份绑定",
		"/checkin - 签到并直接增加绑定账户额度",
		"/checkin status - 查看签到状态",
		"/hongbao - 领取当前群红包，每个账户每轮限领一次",
		"管理员：/hongbao new <总额度> <数量> [分组限制 ...] - 发放拼手气额度红包，多个分组以空格分隔",
		"管理员：/checkin reset - 重置当前周期所有用户的签到状态",
		"/me - 查看账户与额度",
		"/usage [时间长度] - 查看自己的用量，例如 /usage 7d",
		"/usage <时间长度> all - 查看全站请求、Token 与额度汇总",
		"/usage <时间长度> <前N名> - 查看用量排行榜，例如 /usage 7d 10",
		"/logs [数量] - 查看自己的最近调用记录",
		"/models [用户ID或@用户] - 查看用户分组可用模型",
		"/plan view - 查看自己的全部订阅",
		"/whoami - 查看当前 OpenID",
		"/enable list、/disable list - 查看命令关键词状态；管理员可启用或禁用关键词",
		"管理员：/credit add、/credit sub、/credit show（用户ID可替换为@群成员）",
		"管理员：/plan add、/plan sub、/plan view <用户ID或@群成员>",
		"管理员：/admin bindings、/admin unbind <用户ID或@群成员>",
		"管理员：/admin checkin - 查看今日签到统计与动态发放规则",
		"管理员：/admin report [时间长度] - 查看全站用量摘要",
		"管理员：/welcome on|off|set <欢迎语>、/recall",
		"管理员：/join on|off|status、/join limit <QQ等级>、/join check \"<匹配字符串>\" - 配置入群自动审批",
		"管理员：/mute <@成员> <时长>、/mute off <@成员>、/mute status",
		"/bot status - 查看机器人与群聊状态",
	}
	if cfg.UsageChartEnabled {
		lines = append(lines, "/usage chart <时间长度> [@用户|用户ID|all] - 生成用量图表；指定用户仅管理员可用，all 汇总本群已绑定成员")
	}
	if cfg.NotifyEnabled {
		lines = append(lines, "/notify quota <额度>|off、/notify daily on|off、/notify status")
	}
	if cfg.AdminReportExportEnabled {
		lines = append(lines, "管理员：/admin report export [时间长度] - 导出 CSV")
	}
	if cfg.AdminUserManagementEnabled {
		lines = append(lines, "管理员：/admin user status|enable|disable|reset2fa|resetpasskey <用户>")
	}
	if cfg.BenefitEnabled {
		lines = append(lines, "管理员：/benefit <面额> <数量> <有效期(h)> <封禁时间(day)> - 发放限领福利")
	}
	if cfg.ResetEnabled {
		lines = append(lines,
			"/reset check - 查看当前群的重置状态",
			"/reset last - 查看 Codex Reset API 最新重置事件及状态",
			"/reset join - 参加当前群正在进行的重置补偿抽奖",
			"管理员：/reset new - 按当前群设置手动开启新活动",
			"管理员：/reset stop - 停止当前活动，不抽奖、不发放额度",
			"管理员：/reset end - 提前截止当前活动并立即结算",
			"管理员：/reset set duration <时长> - 设置下一轮活动有效期",
			"管理员：/reset set winners <人数> - 设置下一轮抽取人数",
			"管理员：/reset set lookback <时长> - 设置下一轮补偿回溯时间",
			"管理员：/reset proxy <代理链接|off> - 设置 Codex Reset API 检测代理",
		)
	}
	return strings.Join(lines, "\n")
}
