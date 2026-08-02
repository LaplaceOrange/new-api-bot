package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	FindUserByEmail(context.Context, string) (newapi.User, error)
	AddQuota(context.Context, int, int64) error
	SubtractQuota(context.Context, int, int64) error
}

type QQAPI interface {
	ReplyC2C(context.Context, string, string, string) error
	ReplyGroup(context.Context, string, string, string) error
}

type Service struct {
	cfg      config.Config
	store    *store.Store
	secure   *secure.Box
	newAPI   NewAPI
	qq       QQAPI
	mailer   mailer.Sender
	logger   *slog.Logger
	queue    chan qq.MessageEvent
	workers  sync.WaitGroup
	checkins sync.Map
	credits  sync.Map
	stopOnce sync.Once
}

func New(cfg config.Config, storage *store.Store, box *secure.Box, newAPI NewAPI, qqAPI QQAPI, sender mailer.Sender, logger *slog.Logger) *Service {
	return &Service{
		cfg: cfg, store: storage, secure: box, newAPI: newAPI, qq: qqAPI, mailer: sender, logger: logger,
		queue: make(chan qq.MessageEvent, cfg.GatewayQueueSize),
	}
}

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
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.queue) })
	s.workers.Wait()
}

func (s *Service) HandleGateway(ctx context.Context, event qq.MessageEvent) {
	msgIndex := sceneValue(event.Message.Scene.Ext, "msg_idx")
	dedupKey := event.EventType + "|" + event.Message.ID + "|" + msgIndex
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
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	content := strings.TrimSpace(event.Message.Content)
	if content == "" || !strings.HasPrefix(content, "/") {
		s.logger.Info("忽略非指令 QQ 消息",
			"event", event.EventType,
			"content_length", len([]rune(content)),
			"starts_with_slash", strings.HasPrefix(content, "/"),
		)
		return
	}
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return
	}
	command := strings.ToLower(fields[0])
	s.logger.Info("开始处理 QQ 命令", "event", event.EventType, "command", command)
	identity := identityFromEvent(event)
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
			err = s.reply(ctx, event, helpText())
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
	if len(fields) >= 2 && strings.EqualFold(fields[1], "vertify") {
		if len(fields) != 3 || !isSixDigits(fields[2]) {
			return s.reply(ctx, event, "格式错误。正确用法：/bind vertify <6位验证码>")
		}
		pending, err := s.store.GetPendingBind(canonical)
		if err != nil {
			return s.reply(ctx, event, "当前没有待验证的绑定请求，请先使用 /bind <邮箱或New API用户ID> 获取验证码。")
		}
		return s.verifyBinding(ctx, event, pending, fields[2])
	}
	if len(fields) != 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/bind <邮箱或New API用户ID>；收到邮件后使用 /bind vertify <6位验证码>。")
	}
	argument := strings.TrimSpace(fields[1])
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
	return s.reply(ctx, event, "验证码已发送至 "+store.MaskEmail(pending.Email)+"，请在有效期内直接在当前群发送 /bind vertify <6位验证码> 完成绑定。")
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

	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rawQuota, err := newapi.DisplayToQuota(s.cfg.CheckinCredit, status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "签到额度配置无效，请联系管理员："+err.Error())
	}
	record := model.CheckinRecord{
		CanonicalID: canonical, NewAPIID: binding.NewAPIID, PeriodKey: period,
		RawQuota: rawQuota, DisplayCredit: s.cfg.CheckinCredit, CreatedAt: time.Now(), Status: "pending",
	}
	record, created, err := s.store.ReserveCheckin(record)
	if err != nil {
		return s.reply(ctx, event, "保存签到状态失败，请稍后重试。")
	}
	if !created {
		if record.Status == "completed" {
			return s.reply(ctx, event, fmt.Sprintf("本周期已经签到，额度 %s 已发放至绑定的 New API 用户 %d；下次可签到时间：%s", record.DisplayCredit, record.NewAPIID, next.Format("2006-01-02 15:04 MST")))
		}
		return s.reply(ctx, event, "本周期签到请求正在处理中，请勿重复提交；如长时间未到账请联系管理员核查。")
	}
	if err := s.newAPI.AddQuota(ctx, binding.NewAPIID, rawQuota); err != nil {
		_ = s.store.DeletePendingCheckin(record)
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: false, Description: publicError(err), Metadata: map[string]any{"period": period, "quota": rawQuota}})
		return s.reply(ctx, event, publicError(err))
	}
	record.Status = "completed"
	if err := s.store.FinalizeCheckin(record); err != nil {
		s.logger.Error("签到额度已发放但保存完成状态失败", "canonical", canonical, "newapi_user_id", binding.NewAPIID, "error", err)
		return s.reply(ctx, event, "额度已经发放，但本地签到状态保存失败，请联系管理员核查，勿重复签到。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "checkin.quota", Target: strconv.Itoa(binding.NewAPIID), Success: true, Metadata: map[string]any{"period": period, "quota": rawQuota, "display_credit": s.cfg.CheckinCredit}})
	return s.reply(ctx, event, fmt.Sprintf("🎉 签到成功！额度 %s 已直接发放至绑定的 New API 用户 %d。", s.cfg.CheckinCredit, binding.NewAPIID))
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
	}
	return s.reply(ctx, event, fmt.Sprintf("当前周期：%s\n签到状态：%s\n已发放额度：%s\n绑定用户 ID：%d\n下个周期：%s", period, status, record.DisplayCredit, record.NewAPIID, next.Format("2006-01-02 15:04 MST")))
}

func (s *Service) handleCredit(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行额度管理指令的权限。")
	}
	if len(fields) < 3 {
		return s.reply(ctx, event, "用法：/credit add|sub <用户ID或@用户> <额度>，或 /credit show <用户ID或@用户>")
	}
	action := strings.ToLower(fields[1])
	userID, targetDescription, err := s.resolveCreditTarget(event, fields[2])
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	switch action {
	case "show":
		if len(fields) != 3 {
			return s.reply(ctx, event, "用法：/credit show <用户ID>")
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

func (s *Service) resolveCreditTarget(event qq.MessageEvent, token string) (int, string, error) {
	if userID, err := strconv.Atoi(token); err == nil && userID > 0 {
		return userID, "指定目标", nil
	}
	if !strings.HasPrefix(token, "@") && !strings.HasPrefix(token, "<@") {
		return 0, "", errors.New("目标用户必须是正整数 New API 用户 ID，或当前群内被 @ 的已绑定用户。")
	}
	if event.Message.GroupOpenID == "" {
		return 0, "", errors.New("@用户作为额度目标仅支持群聊消息。")
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

func (s *Service) handleAdmin(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行管理员指令的权限。")
	}
	if len(fields) < 2 {
		return s.reply(ctx, event, "用法：/admin bindings [页码] 或 /admin unbind <用户ID>")
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
			return s.reply(ctx, event, "用法：/admin unbind <用户ID>")
		}
		id, err := strconv.Atoi(fields[2])
		if err != nil || id <= 0 {
			return s.reply(ctx, event, "用户 ID 必须是正整数。")
		}
		removed, err := s.store.UnbindByNewAPIID(id)
		if err != nil {
			return s.reply(ctx, event, "未找到该用户的绑定记录。")
		}
		_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: canonical, Action: "binding.delete", Target: strconv.Itoa(id), Success: true, Metadata: map[string]any{"old_identity": removed.CanonicalID}})
		return s.reply(ctx, event, fmt.Sprintf("已解除 New API 用户 %d 的 QQ 绑定。", id))
	default:
		return s.reply(ctx, event, "用法：/admin bindings [页码] 或 /admin unbind <用户ID>")
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
	return s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, event.Message.ID, content)
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

func helpText() string {
	return strings.Join([]string{
		"可用指令：",
		"/bind <邮箱或用户ID> - 在当前群发送绑定验证码",
		"/bind vertify <验证码> - 在当前群完成绑定",
		"/bind status - 查看当前绑定信息",
		"/unbind - 解除当前 QQ 身份绑定",
		"/checkin - 签到并直接增加绑定账户额度",
		"/checkin status - 查看签到状态",
		"/me - 查看账户与额度",
		"/whoami - 查看当前 OpenID",
		"管理员：/credit add、/credit sub、/credit show（用户ID可替换为@群成员）、/admin bindings、/admin unbind",
	}, "\n")
}
