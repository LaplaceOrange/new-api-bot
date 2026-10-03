package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/llm"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type llmJobPayload struct {
	Event     qq.MessageEvent `json:"event"`
	Config    llm.Config      `json:"config"`
	Prompt    string          `json:"prompt"`
	Canonical string          `json:"canonical"`
	UserID    int             `json:"user_id"`
}
type llmTurn struct {
	User          string `json:"user"`
	Assistant     string `json:"assistant"`
	Owner         string `json:"owner"`
	Business      bool   `json:"business"`
	RequiresAdmin bool   `json:"requires_admin,omitempty"`
}
type llmSavedResult struct {
	Text          string `json:"text"`
	RequiresAdmin bool   `json:"requires_admin,omitempty"`
}

func (s *Service) initLLM() {
	if s.cfg.LLM.HistoryTTLSeconds == 0 {
		s.cfg.LLM = llm.DefaultConfig()
	}
	client := llm.NewClient()
	s.llmClient = client
	s.llmCompleter = client
	s.llmSearcher = client
	s.llmWake = make(chan struct{}, 1)
	s.llmActive = map[string]context.CancelFunc{}
}
func (s *Service) wakeLLM() {
	select {
	case s.llmWake <- struct{}{}:
	default:
	}
}
func (s *Service) llmSessionKey(event qq.MessageEvent, canonical string) string {
	if event.Message.GroupOpenID != "" {
		return "group:" + s.secure.MAC("llm-group", event.Message.GroupOpenID)
	}
	return "user:" + s.secure.MAC("llm-session", canonical)
}
func (s *Service) llmActor(canonical string) string { return s.secure.MAC("llm-rate", canonical) }
func llmDay(now time.Time) string {
	return now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
}

func (s *Service) handleChat(ctx context.Context, event qq.MessageEvent, content string) error {
	identity := identityFromEvent(event)
	tail := commandRuleTail(content)
	switch strings.ToLower(tail) {
	case "help":
		return s.replyHelp(ctx, event, s.helpTextFor(identity, "/chat")+"\n"+llmConfigKeyHelp())
	case "on help", "off help", "status help", "reset help":
		return s.replyHelp(ctx, event, s.helpTextFor(identity, "/chat "+strings.ToLower(strings.Fields(tail)[0])))
	case "on", "off":
		if !s.isAdmin(identity) || s.isReadOnlyAdmin(identity) {
			return s.reply(ctx, event, "仅可写 Bot 管理员可开启或关闭群对话。")
		}
		group := event.Message.GroupOpenID
		if group == "" {
			return s.reply(ctx, event, "该指令只能在群聊中使用。")
		}
		enabled := strings.EqualFold(tail, "on")
		if err := s.store.SetLLMGroup(group, enabled, s.now()); err != nil {
			return s.reply(ctx, event, "保存群对话设置失败。")
		}
		if !enabled {
			s.llmMu.Lock()
			if cancel := s.llmActive[s.llmSessionKey(event, "")]; cancel != nil {
				cancel()
			}
			s.llmMu.Unlock()
		}
		_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: commandRuleActor(identity), Action: "llm.group", Target: group, Success: true, Metadata: map[string]any{"enabled": enabled}})
		s.wakeLLM()
		return s.reply(ctx, event, "当前群对话已"+map[bool]string{true: "开启", false: "关闭"}[enabled]+"；全局启用及接口配置也需有效。")
	}
	canonical, err := s.store.ResolveCanonical(identity)
	if err != nil || canonical == "" {
		return s.reply(ctx, event, "你尚未绑定 New API 账户，请使用 /bind 完成绑定。")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "你尚未绑定 New API 账户，请使用 /bind 完成绑定。")
	}
	cfg, err := s.llmConfigSnapshot()
	if err != nil {
		return s.reply(ctx, event, "LLM 配置读取失败。")
	}
	if strings.EqualFold(tail, "status") {
		enabled := cfg.Enabled
		if event.Message.GroupOpenID != "" {
			groupEnabled, e := s.store.LLMGroupEnabled(event.Message.GroupOpenID)
			if e != nil {
				return s.reply(ctx, event, "群对话设置读取失败。")
			}
			enabled = enabled && groupEnabled
		}
		rate, err := s.store.LLMRate(s.llmActor(canonical), llmDay(s.now()), s.now())
		if err != nil {
			return s.reply(ctx, event, "次数读取失败。")
		}
		return s.reply(ctx, event, fmt.Sprintf("对话开启：%t\n接口就绪：%t\n模型：%s\n搜索后端：%s\n每日剩余：%d\n最近一分钟剩余：%d", enabled, cfg.Ready(), nonEmpty(cfg.Model, "未配置"), cfg.SearchBackend, max(0, cfg.DailyLimit-rate.Count), max(0, cfg.MinuteLimit-len(rate.Recent))))
	}
	if strings.EqualFold(tail, "reset") {
		if event.Message.GroupOpenID != "" && (!s.isAdmin(identity) || s.isReadOnlyAdmin(identity)) {
			return s.reply(ctx, event, "仅可写 Bot 管理员可清空群共享历史。")
		}
		if err := s.store.ResetLLMSession(s.llmSessionKey(event, canonical), s.now()); err != nil {
			return s.reply(ctx, event, "清空历史失败。")
		}
		return s.reply(ctx, event, "当前会话历史已清空。")
	}
	if tail == "" {
		return s.reply(ctx, event, "用法：/chat <内容>；/chat help")
	}
	if len(tail) > maxCommandBytes {
		return s.reply(ctx, event, "对话内容不能超过 4096 字节。")
	}
	if !cfg.Ready() {
		return s.reply(ctx, event, "LLM 对话尚未开启或接口、密钥、模型未配置。")
	}
	if event.Message.GroupOpenID != "" {
		enabled, e := s.store.LLMGroupEnabled(event.Message.GroupOpenID)
		if e != nil || !enabled {
			return s.reply(ctx, event, "当前群未开启对话，请管理员使用 /chat on。")
		}
	}
	if event.Message.ID == "" {
		return s.reply(ctx, event, "当前消息缺少唯一标识，请重新发送。")
	}
	payload := llmJobPayload{Event: event, Config: cfg, Prompt: tail, Canonical: canonical, UserID: binding.NewAPIID}
	b, _ := json.Marshal(payload)
	ciphertext, err := s.secure.Encrypt(string(b))
	if err != nil {
		return s.reply(ctx, event, "保存对话失败。")
	}
	now := s.now()
	kind := "group"
	if event.EventType == "C2C_MESSAGE_CREATE" {
		kind = "c2c"
	}
	id := s.secure.MAC("llm-job", s.llmSessionKey(event, canonical)+"|"+kind+"|"+event.Message.ID+"|"+sceneValue(event.Message.Scene.Ext, "msg_idx"))
	origin := qq.ReplyOrigin(event, now)
	expires := origin.Add(4 * time.Minute)
	if event.EventType == "C2C_MESSAGE_CREATE" {
		expires = origin.Add(59 * time.Minute)
	}
	if !expires.After(now) {
		return s.reply(ctx, event, "该对话消息已超过回复时效，请重新发送。")
	}
	_, err = s.store.AdmitLLMJob(store.LLMJob{ID: id, Session: s.llmSessionKey(event, canonical), Group: event.Message.GroupOpenID, Actor: s.llmActor(canonical), Payload: ciphertext, CreatedAt: now, ExpiresAt: expires}, llmDay(now), cfg.MinuteLimit, cfg.DailyLimit, cfg.QueueSize)
	if err != nil {
		if errors.Is(err, store.ErrLLMLimited) || errors.Is(err, store.ErrLLMQueueFull) {
			return s.reply(ctx, event, err.Error())
		}
		return s.reply(ctx, event, "保存对话任务失败。")
	}
	s.wakeLLM()
	return nil
}

func (s *Service) runLLMDispatcher(ctx context.Context) {
	defer s.workers.Done()
	defer s.llmClient.Close()
	defer func() {
		s.llmMu.Lock()
		for _, cancel := range s.llmActive {
			cancel()
		}
		s.llmMu.Unlock()
	}()
	if err := s.store.RecoverLLMJobs(s.now()); err != nil {
		s.logger.Error("LLM 任务恢复失败")
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	prune := time.NewTicker(time.Minute)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		default:
		}
		cfg, err := s.llmConfigSnapshot()
		if err == nil {
			jobs, e := s.store.PendingLLMJobs()
			if e == nil {
				for _, job := range jobs {
					if !job.ExpiresAt.After(s.now()) {
						_, _ = s.store.TransitionLLMJob(job.ID, job.Status, "expired", "", s.now())
						continue
					}
					s.llmMu.Lock()
					_, busy := s.llmActive[job.Session]
					if busy || len(s.llmActive) >= cfg.Concurrency {
						s.llmMu.Unlock()
						continue
					}
					taskCtx, cancel := context.WithCancel(ctx)
					s.llmActive[job.Session] = cancel
					s.workers.Add(1)
					s.llmMu.Unlock()
					go func(job store.LLMJob) {
						defer s.workers.Done()
						defer func() { cancel(); s.llmMu.Lock(); delete(s.llmActive, job.Session); s.llmMu.Unlock(); s.wakeLLM() }()
						s.runLLMJob(taskCtx, job)
					}(job)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notifyStop:
			return
		case <-s.llmWake:
		case <-ticker.C:
		case <-prune.C:
			if err := s.store.PruneLLM(s.now()); err != nil {
				s.logger.Warn("清理 LLM 过期数据失败")
			}
		}
	}
}

func (s *Service) llmJobAuthorized(payload llmJobPayload, job store.LLMJob) bool {
	identity := identityFromEvent(payload.Event)
	canonical, err := s.store.ResolveCanonical(identity)
	if err != nil || canonical != payload.Canonical {
		return false
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil || binding.NewAPIID != payload.UserID {
		return false
	}
	current, err := s.llmConfigSnapshot()
	if err != nil || !current.Enabled {
		return false
	}
	if job.Group != "" {
		enabled, e := s.store.LLMGroupEnabled(job.Group)
		if e != nil || !enabled {
			return false
		}
	}
	if !s.llmCommandAllowed("/chat") {
		return false
	}
	session, err := s.store.LLMSession(job.Session, s.now())
	return err == nil && session.Version == job.Version
}
func (s *Service) runLLMJob(parent context.Context, job store.LLMJob) {
	defer func() {
		if recover() != nil {
			_, _ = s.store.TransitionLLMJob(job.ID, "running", "uncertain", "", s.now())
			_, _ = s.store.TransitionLLMJob(job.ID, "sending", "uncertain", "", s.now())
			s.logger.Error("LLM 任务异常，未自动重试")
		}
	}()
	plain, err := s.secure.Decrypt(job.Payload)
	var payload llmJobPayload
	if err != nil || json.Unmarshal([]byte(plain), &payload) != nil || payload.Config.Validate() != nil {
		_, _ = s.store.TransitionLLMJob(job.ID, job.Status, "failed", "", s.now())
		return
	}
	if !s.llmJobAuthorized(payload, job) {
		_, _ = s.store.TransitionLLMJob(job.ID, job.Status, "canceled", "", s.now())
		return
	}
	if job.Status == "queued" {
		ok, err := s.store.TransitionLLMJob(job.ID, "queued", "running", "", s.now())
		if err != nil || !ok {
			return
		}
		started := time.Now()
		deadline := time.Now().Add(time.Duration(payload.Config.TimeoutSeconds) * time.Second)
		if job.ExpiresAt.Before(deadline) {
			deadline = job.ExpiresAt
		}
		ctx, cancel := context.WithDeadline(parent, deadline)
		text, turn, business, stats, err := s.generateLLM(ctx, job, payload)
		cancel()
		s.logger.Info("LLM 对话完成", "duration_ms", time.Since(started).Milliseconds(), "model", s.llmSafeText(payload.Config.Model, payload.Config), "prompt_tokens", stats.PromptTokens, "completion_tokens", stats.CompletionTokens, "success", err == nil)
		if parent.Err() != nil {
			_, _ = s.store.TransitionLLMJob(job.ID, "running", "uncertain", "", s.now())
			return
		}
		if !s.llmJobAuthorized(payload, job) {
			_, _ = s.store.TransitionLLMJob(job.ID, "running", "canceled", "", s.now())
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				text = "对话超时，请缩短问题后重试。"
			case errors.Is(err, context.Canceled):
				text = "对话已取消。"
			default:
				text = "对话失败：" + s.llmSafeText(err.Error(), payload.Config)
			}
		}
		if err == nil && (!business || job.Group == "") {
			session, e := s.store.LLMSession(job.Session, s.now())
			if e == nil && session.Version == job.Version {
				var turns []llmTurn
				if session.Ciphertext != "" {
					p, e := s.secure.Decrypt(session.Ciphertext)
					if e == nil {
						_ = json.Unmarshal([]byte(p), &turns)
					}
				}
				turn.Business = business
				turns = append(turns, turn)
				turns = trimLLMTurns(turns, payload.Config)
				b, _ := json.Marshal(turns)
				encrypted, e := s.secure.Encrypt(string(b))
				if e == nil {
					if e = s.store.PutLLMSession(job.Session, job.Version, encrypted, s.now().Add(time.Duration(payload.Config.HistoryTTLSeconds)*time.Second), s.now()); e != nil {
						s.logger.Warn("LLM 历史保存失败")
					}
				}
			}
		}
		text = s.llmSafeText(text, payload.Config)
		text = boundLLMReply(text, payload.Config.MaxReplyRunes)
		b, _ := json.Marshal(llmSavedResult{Text: text, RequiresAdmin: turn.RequiresAdmin})
		encrypted, e := s.secure.Encrypt(string(b))
		if e != nil {
			_, _ = s.store.TransitionLLMJob(job.ID, "running", "failed", "", s.now())
			return
		}
		ok, e = s.store.TransitionLLMJob(job.ID, "running", "ready", encrypted, s.now())
		if e != nil || !ok {
			return
		}
		job.Result = encrypted
		job.Status = "ready"
	}
	if !job.ExpiresAt.After(s.now()) || !s.llmJobAuthorized(payload, job) {
		_, _ = s.store.TransitionLLMJob(job.ID, "ready", "canceled", "", s.now())
		return
	}
	ok, err := s.store.TransitionLLMJob(job.ID, "ready", "sending", "", s.now())
	if err != nil || !ok {
		return
	}
	data, err := s.secure.Decrypt(job.Result)
	var result llmSavedResult
	if err != nil || json.Unmarshal([]byte(data), &result) != nil {
		_, _ = s.store.TransitionLLMJob(job.ID, "sending", "failed", "", s.now())
		return
	}
	if result.RequiresAdmin && !s.isAdmin(identityFromEvent(payload.Event)) {
		_, _ = s.store.TransitionLLMJob(job.ID, "sending", "canceled", "", s.now())
		return
	}
	sendCtx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	err = s.sendLLMResult(sendCtx, payload.Event, result.Text, payload.Config)
	state := "sent"
	if err != nil {
		state = "failed"
		s.logger.Warn("LLM 回复未全部送达，未重新生成或重发")
	}
	_, _ = s.store.TransitionLLMJob(job.ID, "sending", state, "", s.now())
}
func trimLLMTurns(turns []llmTurn, cfg llm.Config) []llmTurn {
	for len(turns) > cfg.HistoryTurns {
		turns = turns[1:]
	}
	for len(turns) > 0 {
		b, _ := json.Marshal(turns)
		if len(b) <= cfg.HistoryBytes {
			break
		}
		turns = turns[1:]
	}
	return turns
}
func (s *Service) llmSafeText(text string, cfg llm.Config) string {
	keys := []string{cfg.APIKey, cfg.TavilyKey, cfg.BingKey, cfg.SearchModelKey, s.cfg.LLM.APIKey, s.cfg.LLM.TavilyKey, s.cfg.LLM.BingKey, s.cfg.LLM.SearchModelKey, s.cfg.NewAPIAdminToken, s.cfg.QQAppSecret, s.cfg.SMTPPassword}
	for _, key := range keys {
		if key != "" {
			text = strings.ReplaceAll(text, key, "[已脱敏]")
		}
	}
	// Generated text is not permission to emit QQ control markup (notably
	// @everyone). Keep it literal, including inside generated code snippets.
	return strings.ReplaceAll(text, "<qqbot-", "&lt;qqbot-")
}
func boundLLMReply(text string, limit int) string {
	const marker = "\n…（回复已截短）"
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-len([]rune(marker)))]) + marker
	}
	return text
}
func (s *Service) llmCommandAllowed(content string) bool {
	rules, err := s.disabledCommandKeywords()
	if err != nil {
		return false
	}
	normalized := normalizeCommandFilter(content)
	for _, keyword := range rules {
		if strings.Contains(normalized, keyword) {
			return false
		}
	}
	return true
}
func (s *Service) sendLLMResult(ctx context.Context, event qq.MessageEvent, text string, cfg llm.Config) error {
	text = boundLLMReply(text, cfg.MaxReplyRunes)
	chunks := splitMessage(text, cfg.ChunkRunes)
	for i, chunk := range chunks {
		if i >= 4 {
			return errors.New("回复段数超过预算")
		}
		if event.EventType == "C2C_MESSAGE_CREATE" {
			user := firstNonEmpty(event.Message.Author.UserOpenID, event.Message.Author.ID)
			if api, ok := s.qq.(c2cTextSequenceAPI); ok {
				if _, err := api.SendC2CTextWithSequence(ctx, user, event.Message.ID, chunk, i+1); err != nil {
					return err
				}
			} else {
				if i > 0 {
					return errors.New("QQ 客户端不支持多段被动回复")
				}
				if err := s.qq.ReplyC2C(ctx, user, event.Message.ID, chunk); err != nil {
					return err
				}
			}
		} else {
			if _, ok := s.qq.(sequencedGroupMessageSender); !ok && i > 0 {
				return errors.New("QQ 客户端不支持多段被动回复")
			}
			if err := s.sendGroupReplyWithSequence(ctx, event.Message.GroupOpenID, event.Message.ID, chunk, i+1); err != nil {
				return err
			}
		}
	}
	return nil
}
