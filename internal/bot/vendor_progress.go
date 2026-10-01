package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

type vendorQueryOutcome struct {
	packet vendorstatus.Packet
	err    error
}

type vendorProgressState struct {
	text string
}

func (p *vendorProgressState) set(packet vendorstatus.Packet) bool {
	labels := map[string]string{
		"startup": "正在准备状态查询……", "fetch": "正在收集提供商信息……", "translate": "正在翻译事件信息……",
		"render": "正在生成状态总览图片……", "encode": "正在处理总览图片……",
		"upload": "正在发送状态总览图片……",
	}
	label := labels[packet.Stage]
	if label == "" {
		label = "正在生成状态总览……"
	}
	if packet.Text != "" {
		label = packet.Text
	}
	if runes := []rune(label); len(runes) > 160 {
		label = string(runes[:160]) + "……"
	}
	if packet.Total > 0 {
		label += fmt.Sprintf("（已完成 %d/%d）", packet.Completed, packet.Total)
	}
	if p.text == label {
		return false
	}
	p.text = label
	return true
}

func (p *vendorProgressState) message() string {
	return p.text
}

type c2cTextSequenceAPI interface {
	SendC2CTextWithSequence(context.Context, string, string, string, int) (qq.SentMessage, error)
}

// All text and media share a single reply sequence allocator. Long queries
// switch to independent messages rather than reuse an expired passive reply.
type vendorQueryReplies struct {
	s       *Service
	event   qq.MessageEvent
	started time.Time
	mu      sync.Mutex
	nextSeq int
	last    string
	stale   []string
}

func (r *vendorQueryReplies) reserve() (qq.MessageEvent, int) {
	return r.reserveWithin(4)
}

func (r *vendorQueryReplies) reserveResult() (qq.MessageEvent, int) {
	// Keep a passive reply available for the image/error even when frequent
	// step notifications need independent messages and proactive permission.
	return r.reserveWithin(5)
}

func (r *vendorQueryReplies) reserveWithin(limit int) (qq.MessageEvent, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event := r.event
	if r.nextSeq >= limit || time.Since(r.started) >= 2*time.Minute {
		event.Message.ID = ""
		return event, 1
	}
	r.nextSeq++
	return event, r.nextSeq
}

func (r *vendorQueryReplies) text(ctx context.Context, text string) (qq.SentMessage, error) {
	event, seq := r.reserve()
	return r.sendText(ctx, event, seq, text)
}

func (r *vendorQueryReplies) resultText(ctx context.Context, text string) (qq.SentMessage, error) {
	event, seq := r.reserveResult()
	return r.sendText(ctx, event, seq, text)
}

func (r *vendorQueryReplies) sendText(ctx context.Context, event qq.MessageEvent, seq int, text string) (qq.SentMessage, error) {
	if event.EventType == "C2C_MESSAGE_CREATE" {
		user := firstNonEmpty(event.Message.Author.UserOpenID, event.Message.Author.ID)
		if api, ok := r.s.qq.(c2cTextSequenceAPI); ok {
			return api.SendC2CTextWithSequence(ctx, user, event.Message.ID, text, seq)
		}
		if seq > 1 {
			event.Message.ID = ""
		}
		return qq.SentMessage{}, r.s.qq.ReplyC2C(ctx, user, event.Message.ID, text)
	}
	if api, ok := r.s.qq.(sequencedGroupMessageSender); ok {
		return api.SendGroupTextWithSequence(ctx, event.Message.GroupOpenID, event.Message.ID, text, seq)
	}
	if seq > 1 {
		event.Message.ID = ""
	}
	if api, ok := r.s.qq.(groupMessageSender); ok {
		return api.SendGroupText(ctx, event.Message.GroupOpenID, event.Message.ID, text)
	}
	return qq.SentMessage{}, r.s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, event.Message.ID, text)
}

func (r *vendorQueryReplies) update(ctx context.Context, text string) {
	sent, err := r.text(ctx, text)
	if err != nil {
		r.s.logger.Warn("发送厂商查询进度失败", "error", r.s.vendorSafeError(err, nil))
		return // Keep previous progress when its replacement did not send.
	}
	if sent.ID == "" || r.event.EventType == "C2C_MESSAGE_CREATE" {
		return
	}
	if r.last != "" {
		r.stale = append(r.stale, r.last)
	}
	r.last = sent.ID
	r.recall(ctx)
}

func (r *vendorQueryReplies) recall(ctx context.Context) {
	if api, ok := r.s.qq.(groupRecallAPI); ok {
		remaining := make([]string, 0, min(len(r.stale), 16))
		// Retract the immediately preceding progress first. Older failures
		// must not delay the latest replacement or consume its retry budget.
		for i := len(r.stale) - 1; i >= 0; i-- {
			id := r.stale[i]
			if err := api.RecallGroupMessage(ctx, r.event.Message.GroupOpenID, id); err != nil {
				r.s.logger.Warn("撤回厂商查询旧进度失败", "error", r.s.vendorSafeError(err, nil))
				if len(remaining) < 16 { // Bound repeated recall failures.
					remaining = append(remaining, id)
				}
			}
		}
		for left, right := 0, len(remaining)-1; left < right; left, right = left+1, right-1 {
			remaining[left], remaining[right] = remaining[right], remaining[left]
		}
		r.stale = remaining
	} else {
		r.stale = nil
	}
}

func (r *vendorQueryReplies) cleanup(ctx context.Context) {
	if r.last != "" {
		r.stale = append(r.stale, r.last)
		r.last = ""
	}
	r.recall(ctx)
}

func (s *Service) vendorSafeText(text string, cfg *vendorstatus.Config) string {
	base := s.vendorBaseConfig()
	if cfg != nil {
		base = *cfg
	}
	extra := []string{s.cfg.QQAppSecret, s.cfg.NewAPIAdminToken, s.cfg.SMTPPassword}
	// Configuration may have changed while this query waited for its slot.
	if current, err := s.vendorConfigSnapshot(); err == nil {
		extra = append(extra, current.Proxy, current.Translation.APIKey)
		text = vendorstatus.Redact(text, current)
	}
	text = vendorstatus.Redact(text, base, extra...)
	runes := []rune(text)
	if len(runes) > 1200 {
		text = string(runes[:700]) + "\n…（诊断已截短）…\n" + string(runes[len(runes)-400:])
	}
	return strings.TrimSpace(text)
}

func (s *Service) vendorSafeError(err error, cfg *vendorstatus.Config) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "查询超时（context deadline exceeded），请检查代理/网络，或增大 worker_timeout_seconds。"
	}
	if errors.Is(err, context.Canceled) {
		return "查询已取消（context canceled）。"
	}
	return s.vendorSafeText(err.Error(), cfg)
}

func (s *Service) runVendorQueryWithProgress(ctx context.Context, event qq.MessageEvent, cfg vendorstatus.Config, interval time.Duration) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	mode := cfg.ProgressMode
	if mode == "" {
		mode = "detailed"
	}
	progressEnabled := mode != "off"
	state := &vendorProgressState{text: "正在准备查询任务，等待执行槽位……"}
	if mode == "simple" {
		state.text = "正在生成中……"
	}
	replies := &vendorQueryReplies{s: s, event: event, started: time.Now()}
	// Short network contexts cannot truncate generation; only queryCtx owns it.
	notify := func(fn func(context.Context)) {
		sendCtx, sendCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer sendCancel()
		fn(sendCtx)
	}
	if progressEnabled {
		notify(func(sendCtx context.Context) { replies.update(sendCtx, state.message()) })
	}
	defer notify(replies.cleanup)
	// Preserve ordered step transitions instead of sampling only the latest
	// state every ten seconds. A bounded queue applies backpressure to the
	// worker without spawning unbounded message-delivery goroutines.
	updates := make(chan vendorstatus.Packet, 128)
	results := make(chan vendorQueryOutcome, 1)
	uploads := make(chan error, 1)
	go func() {
		packet, err := s.runVendorStatusProgress(queryCtx, "query", func(update vendorstatus.Packet) {
			if mode != "detailed" && update.Stage != "warning" {
				return
			}
			update.Text = s.vendorSafeText(update.Text, &cfg)
			select {
			case updates <- update:
			case <-queryCtx.Done():
			}
		})
		results <- vendorQueryOutcome{packet, err}
	}()
	sendWarning := func(text string) {
		notify(func(sendCtx context.Context) {
			if _, err := replies.text(sendCtx, "厂商查询提示（已脱敏）：\n"+text); err != nil {
				s.logger.Warn("回复厂商查询错误失败", "error", s.vendorSafeError(err, &cfg))
			}
		})
	}
	fail := func(err error) error {
		cancel()
		reason := s.vendorSafeError(err, &cfg)
		s.logger.Warn("厂商状态查询失败", "error", reason)
		var replyErr error
		notify(func(sendCtx context.Context) {
			_, replyErr = replies.resultText(sendCtx, "厂商状态查询失败（已脱敏）：\n"+reason)
		})
		return replyErr
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	var idle <-chan time.Time
	if progressEnabled {
		idle = timer.C
	} else {
		timer.Stop()
	}
	showCurrent := func() {
		notify(func(sendCtx context.Context) { replies.update(sendCtx, state.message()) })
		timer.Reset(interval)
	}
	applyUpdate := func(update vendorstatus.Packet) bool {
		if update.Stage == "warning" {
			sendWarning(update.Text)
			return false
		}
		if mode != "detailed" {
			return false
		}
		if state.set(update) {
			showCurrent()
			return true
		}
		return false
	}
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case update := <-updates:
			applyUpdate(update)
		case <-idle:
			// A new step takes precedence over refreshing an obsolete one.
			if len(updates) > 0 {
				if !applyUpdate(<-updates) {
					showCurrent()
				}
			} else {
				showCurrent()
			}
		case result := <-results:
			// Deliver every step/diagnostic received before the final packet.
			for len(updates) > 0 {
				if err := ctx.Err(); err != nil {
					return fail(err)
				}
				applyUpdate(<-updates)
			}
			if result.err != nil {
				return fail(result.err)
			}
			if len(result.packet.PNG) == 0 {
				return fail(errors.New(nonEmpty(result.packet.Text, "采集进程未返回 PNG 总览图片")))
			}
			applyUpdate(vendorstatus.Packet{Stage: "upload", Text: "正在发送状态总览图片……"})
			imageEvent, sequence := replies.reserveResult()
			go func() {
				uploads <- s.sendVendorImageWithSequence(queryCtx, imageEvent, result.packet.PNG, sequence)
			}()
			results = nil
		case err := <-uploads:
			if err != nil {
				return fail(err)
			}
			return nil
		}
	}
}
