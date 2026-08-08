package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fsykk/new-api-bot/internal/config"
	"github.com/fsykk/new-api-bot/internal/mailer"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
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

type QQAPI interface {
	ReplyC2C(context.Context, string, string, string) error
	ReplyGroup(context.Context, string, string, string) error
}

type Service struct {
	cfg              config.Config
	store            *store.Store
	secure           *secure.Box
	newAPI           NewAPI
	qq               QQAPI
	mailer           mailer.Sender
	logger           *slog.Logger
	queue            chan qq.MessageEvent
	workers          sync.WaitGroup
	checkins         sync.Map
	credits          sync.Map
	plans            sync.Map
	notifyStop       chan struct{}
	stopOnce         sync.Once
	gatewayConnected func() bool
	notifyMu         sync.Mutex
	groupLastNotify  map[string]time.Time
	benefitMu        sync.Mutex
}

func New(cfg config.Config, storage *store.Store, box *secure.Box, newAPI NewAPI, qqAPI QQAPI, sender mailer.Sender, logger *slog.Logger) *Service {
	return &Service{
		cfg: cfg, store: storage, secure: box, newAPI: newAPI, qq: qqAPI, mailer: sender, logger: logger,
		queue: make(chan qq.MessageEvent, cfg.GatewayQueueSize), notifyStop: make(chan struct{}), groupLastNotify: make(map[string]time.Time),
	}
}

func (s *Service) SetGatewayConnectedFunc(fn func() bool) { s.gatewayConnected = fn }

func (s *Service) Start(ctx context.Context) {
	for i := 0; i < s.cfg.GatewayWorkers; i++ {
		s.workers.Add(1)
		go func(worker int) {
			defer s.workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-s.queue:
					if !ok {
						return
					}
					s.process(ctx, event)
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
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.queue)
		close(s.notifyStop)
	})
	s.workers.Wait()
}

func (s *Service) HandleGateway(ctx context.Context, event qq.MessageEvent) {
	msgIndex := sceneValue(event.Message.Scene.Ext, "msg_idx")
	dedupKey := event.EventType + "|" + event.Message.ID + "|" + msgIndex
	if event.Member.GroupOpenID != "" {
		dedupKey = fmt.Sprintf("%s|%s|%s|%d", event.EventType, event.Member.GroupOpenID, event.Member.MemberOpenID, event.Member.Timestamp)
	}
	duplicate, err := s.store.CheckAndMarkMessage(dedupKey, time.Now(), s.cfg.MessageDedupTTL)
	if err != nil {
		s.logger.Error("消息去重存储失败", "error", err)
		return
	}
	if duplicate {
		return
	}
	select {
	case s.queue <- event:
	default:
		s.logger.Warn("命令队列已满，拒绝消息", "event", event.EventType)
		replyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = s.reply(replyCtx, event, "机器人当前任务较多，请稍后重试。")
	}
}

func (s *Service) process(parent context.Context, event qq.MessageEvent) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout(s.cfg.NewAPITimeout, s.cfg.QQAPITimeout))
	defer cancel()
	if event.EventType == "GROUP_MEMBER_ADD" {
		s.handleMemberAdd(ctx, event)
		return
	}
	content := strings.TrimSpace(event.Message.Content)
	if content == "" || !strings.HasPrefix(content, "/") {
		s.logger.Info("忽略非指令 QQ 消息",
			"event", event.EventType,
			"content_length", len([]rune(content)),
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
	s.logger.Info("开始处理 QQ 命令", "event", event.EventType, "command", command)
	identity := identityFromEvent(event)
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
	if event.EventType == "C2C_MESSAGE_CREATE" && identity.UserOpenID != "" && canonical != "" {
		_ = s.store.PutContact(canonical, identity.UserOpenID)
	}

	var err error
	switch command {
	case "/help":
		if len(fields) != 1 {
			err = s.reply(ctx, event, "格式错误。正确用法：/help")
		} else {
			err = s.reply(ctx, event, s.filteredHelpText())
		}
	case "/whoami":
		if len(fields) != 1 {
			err = s.reply(ctx, event, "格式错误。正确用法：/whoami")
		} else {
			err = s.handleWhoAmI(ctx, event, identity)
		}
	case "/bind":
		err = s.handleBind(ctx, event, canonical, fields)
	case "/link":
		err = s.reply(ctx, event, "当前已启用纯群聊模式，无需使用 /link；请直接在群内使用 /bind <邮箱或用户ID> 绑定。")
	default:
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
		default:
			err = s.reply(ctx, event, "未知指令，请使用 /help 查看可用指令。")
		}
	}
	if err != nil {
		s.logger.Error("处理机器人命令失败", "command", command, "error", err)
	}
}

// commandTimeout leaves room for one New API request, a QQ reply, and an
// additional New API request for commands such as /checkin on a cold cache.
func commandTimeout(newAPITimeout, qqAPITimeout time.Duration) time.Duration {
	const minimum = 25 * time.Second
	const replyReserve = 5 * time.Second
	if newAPITimeout <= 0 {
		newAPITimeout = 30 * time.Second
	}
	if qqAPITimeout <= 0 {
		qqAPITimeout = 10 * time.Second
	}
	timeout := newAPITimeout*2 + qqAPITimeout + replyReserve
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
		return s.reply(ctx, event, "用法：/checkin 或 /checkin status")
	}
	if !s.cfg.CheckinEnabled {
		return s.reply(ctx, event, "签到功能当前未启用。")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "未找到绑定信息。")
	}
	period, next := periodKey(time.Now(), s.cfg.CheckinPeriod, s.cfg.CheckinTimezone)
	lockKey := canonical + "|" + period
	lockValue, _ := s.checkins.LoadOrStore(lockKey, &sync.Mutex{})
	mutex := lockValue.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	credit, err := s.currentCheckinCredit()
	if err != nil {
		return s.reply(ctx, event, "读取签到额度配置失败，请联系管理员。")
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rawQuota, err := newapi.DisplayToQuota(credit, status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "签到额度配置无效，请联系管理员："+err.Error())
	}
	record := model.CheckinRecord{
		CanonicalID: canonical, NewAPIID: binding.NewAPIID, PeriodKey: period,
		RawQuota: rawQuota, DisplayCredit: credit, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: "pending",
	}
	record, created, err := s.store.ReserveCheckin(record)
	if err != nil {
		return s.reply(ctx, event, "保存签到状态失败，请稍后重试。")
	}
	if !created {
		if record.Status == "completed" {
			return s.reply(ctx, event, fmt.Sprintf("本周期已经签到，额度 %s 已发放至绑定的 New API 用户 %d；下次可签到时间：%s", record.DisplayCredit, record.NewAPIID, next.Format("2006-01-02 15:04 MST")))
		}
		if record.Status == "pending_confirmation" {
			return s.reply(ctx, event, "本周期签到额度发放结果待确认，请勿重复签到；如长时间未到账请联系管理员核查。")
		}
		return s.reply(ctx, event, "本周期签到请求正在处理中，请勿重复提交；如长时间未到账请联系管理员核查。")
	}
	if err := s.newAPI.AddQuota(ctx, binding.NewAPIID, rawQuota); err != nil {
		if isAmbiguousQuotaWrite(err) {
			record.Status = "pending_confirmation"
			record.UpdatedAt = time.Now()
			record.LastError = publicError(err)
			if saveErr := s.store.FinalizeCheckin(record); saveErr != nil {
				s.logger.Error("签到结果待确认状态保存失败", "canonical", canonical, "newapi_user_id", binding.NewAPIID, "error", saveErr)
			}
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: false, Description: "额度写入结果待确认：" + publicError(err), Metadata: map[string]any{"period": period, "quota": rawQuota}})
			return s.reply(ctx, event, "签到额度请求超时，发放结果待确认。请勿重复签到；如长时间未到账请联系管理员核查。")
		}
		_ = s.store.DeletePendingCheckin(record)
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: false, Description: publicError(err), Metadata: map[string]any{"period": period, "quota": rawQuota}})
		return s.reply(ctx, event, publicError(err))
	}
	record.Status = "completed"
	record.UpdatedAt = time.Now()
	if err := s.store.FinalizeCheckin(record); err != nil {
		s.logger.Error("签到额度已发放但保存完成状态失败", "canonical", canonical, "newapi_user_id", binding.NewAPIID, "error", err)
		return s.reply(ctx, event, "额度已经发放，但本地签到状态保存失败，请联系管理员核查，勿重复签到。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: true, Metadata: map[string]any{"period": period, "quota": rawQuota, "display_credit": credit}})
	return s.reply(ctx, event, fmt.Sprintf("🎉 签到成功！额度 %s 已直接发放至绑定的 New API 用户 %d。", credit, binding.NewAPIID))
}

func (s *Service) handleCheckinStatus(ctx context.Context, event qq.MessageEvent, canonical string) error {
	period, next := periodKey(time.Now(), s.cfg.CheckinPeriod, s.cfg.CheckinTimezone)
	record, err := s.store.GetCheckin(canonical, period)
	if err != nil {
		return s.reply(ctx, event, "本周期尚未签到。下次周期开始时间："+next.Format("2006-01-02 15:04 MST"))
	}
	status := "处理中"
	if record.Status == "completed" {
		status = "已签到"
	} else if record.Status == "pending_confirmation" {
		status = "待确认"
	}
	return s.reply(ctx, event, fmt.Sprintf("当前周期：%s\n签到状态：%s\n已发放额度：%s\n绑定用户 ID：%d\n下个周期：%s", period, status, record.DisplayCredit, record.NewAPIID, next.Format("2006-01-02 15:04 MST")))
}

// isAmbiguousQuotaWrite identifies a request which may have reached New API
// and been applied, but whose response was not received by the bot.
func isAmbiguousQuotaWrite(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *newapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 0 {
		return false
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
		lockValue, _ := s.credits.LoadOrStore(userID, &sync.Mutex{})
		mutex := lockValue.(*sync.Mutex)
		mutex.Lock()
		defer mutex.Unlock()
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
				return s.reply(ctx, event, publicError(err))
			}
			remaining := user.Quota - rawQuota
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "credit.sub", Target: strconv.Itoa(userID), Success: true, Metadata: map[string]any{"display_credit": fields[3], "quota": rawQuota, "remaining_quota": remaining}})
			return s.reply(ctx, event, fmt.Sprintf("已从%s绑定的 New API 用户 %d 扣除额度 %s，剩余额度 %s。", targetDescription, userID, fields[3], newapi.QuotaToDisplay(remaining, status.QuotaPerUnit)))
		}
		if err := s.newAPI.AddQuota(ctx, userID, rawQuota); err != nil {
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "credit.add", Target: strconv.Itoa(userID), Success: false, Description: publicError(err)})
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
		lock := s.planLock(userID)
		lock.Lock()
		defer lock.Unlock()
		before, err := s.newAPI.ListUserSubscriptions(ctx, userID)
		if err != nil {
			return s.reply(ctx, event, publicError(err))
		}
		if err := s.newAPI.CreateUserSubscription(ctx, userID, planID); err != nil {
			_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "plan.add", Target: strconv.Itoa(userID), Success: false, Description: publicError(err), Metadata: map[string]any{"plan_id": planID}})
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
		lock := s.planLock(userID)
		lock.Lock()
		defer lock.Unlock()
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
			return s.reply(ctx, event, "取消订阅失败："+publicError(err))
		}
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "plan.sub", Target: strconv.Itoa(userID), Success: true, Metadata: map[string]any{"subscription_id": subscriptionID, "plan_id": record.Subscription.PlanID}})
		return s.reply(ctx, event, fmt.Sprintf("取消订阅成功！\n目标：%s（New API 用户 %d）\n当前订阅编号：%d\n订阅状态：已取消", targetDescription, userID, subscriptionID))
	default:
		return s.reply(ctx, event, planUsage())
	}
}

func (s *Service) planLock(userID int) *sync.Mutex {
	value, _ := s.plans.LoadOrStore(userID, &sync.Mutex{})
	return value.(*sync.Mutex)
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
		return s.reply(ctx, event, "用法：/admin bindings [页码]、/admin unbind <用户ID或@用户>、/admin report [时间长度] 或 /admin checkin [edit <发放额度>]")
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
		return s.reply(ctx, event, "用法：/admin bindings [页码]、/admin unbind <用户ID或@用户>、/admin report [时间长度] 或 /admin checkin [edit <发放额度>]")
	}
}

func (s *Service) isAdmin(identity model.QQIdentity) bool {
	for _, candidate := range identity.AdminCandidates() {
		if _, ok := s.cfg.QQAdminOpenIDs[candidate]; ok {
			return true
		}
	}
	return false
}

func (s *Service) reply(ctx context.Context, event qq.MessageEvent, content string) error {
	if event.EventType == "C2C_MESSAGE_CREATE" {
		openID := event.Message.Author.UserOpenID
		if openID == "" {
			openID = event.Message.Author.ID
		}
		return s.qq.ReplyC2C(ctx, openID, event.Message.ID, content)
	}
	if sender, ok := s.qq.(interface {
		SendGroupText(context.Context, string, string, string) (qq.SentMessage, error)
	}); ok {
		sent, err := sender.SendGroupText(ctx, event.Message.GroupOpenID, event.Message.ID, content)
		if err == nil && sent.ID != "" {
			_ = s.store.PutSentBotMessage(model.SentBotMessage{GroupOpenID: event.Message.GroupOpenID, MessageID: sent.ID, MessageIdx: sceneValue(sent.MessageScene.Ext, "msg_idx"), SentAt: time.Now()})
		}
		return err
	}
	return s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, event.Message.ID, content)
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
		"管理员：/admin checkin、/admin checkin edit <发放额度>",
		"管理员：/admin report [时间长度] - 查看全站用量摘要",
		"管理员：/welcome on|off|set <欢迎语>、/recall",
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
	return strings.Join(lines, "\n")
}
