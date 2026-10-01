package bot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

const (
	maxHongbaoCount          = 10000
	hongbaoNoticeRecallAfter = 30 * time.Second
)

func (s *Service) replyHongbaoNotice(ctx context.Context, event qq.MessageEvent, content string) error {
	return s.replyWithAutoRecallAfter(ctx, event, content, hongbaoNoticeRecallAfter)
}

func hongbaoUsage() string {
	return "🧧 使用方式：/hongbao 领取当前群红包；管理员可使用 /hongbao new <总额度> <数量> [分组限制 ...] 发放红包。总额度以站点显示额度为单位，多个分组名称以空格分隔；不填写分组时不限制。"
}

func (s *Service) handleHongbao(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if event.Message.GroupOpenID == "" || event.EventType == "C2C_MESSAGE_CREATE" {
		return s.reply(ctx, event, "🧧 红包功能仅限群聊使用。")
	}
	if len(fields) != 1 && (len(fields) < 4 || !strings.EqualFold(fields[1], "new")) {
		return s.reply(ctx, event, hongbaoUsage())
	}
	unlock := s.hongbaoGroups.Lock(event.Message.GroupOpenID)
	defer unlock()
	if len(fields) >= 4 {
		if !s.isAdmin(identity) || s.isReadOnlyAdmin(identity) {
			return s.replyHongbaoNotice(ctx, event, "🧧 仅具备完整管理权限的管理员可以发放红包。")
		}
		return s.createHongbao(ctx, event, identity, fields)
	}
	return s.claimHongbao(ctx, event, canonical)
}

func (s *Service) createHongbao(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string) error {
	count, err := strconv.Atoi(fields[3])
	if err != nil || count < 1 || count > maxHongbaoCount {
		return s.reply(ctx, event, fmt.Sprintf("🧧 红包个数须为 1 至 %d 的整数。", maxHongbaoCount))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, "🧧 获取站点额度配置失败："+publicError(err))
	}
	total, err := newapi.DisplayToQuota(fields[2], status.QuotaPerUnit)
	if err != nil {
		return s.reply(ctx, event, "🧧 总金额须为可精确换算的正数额度："+err.Error())
	}
	if cmp, cmpErr := newapi.CompareDisplay(fields[2], s.cfg.CreditMaxPerCommand); cmpErr != nil || cmp > 0 {
		return s.reply(ctx, event, "🧧 红包总金额不得超过单次额度上限 "+s.cfg.CreditMaxPerCommand+"。")
	}
	if total < int64(count) {
		return s.reply(ctx, event, "🧧 总金额不足以分配给指定数量的红包，每份至少需要 1 quota。")
	}
	group := event.Message.GroupOpenID
	sum := sha256.Sum256([]byte(group + "|" + event.Message.ID))
	id := fmt.Sprintf("%x", sum[:])
	existing, err := s.store.GetHongbao(group)
	if err == nil {
		if existing.ID == id {
			return s.reply(ctx, event, "🧧 本次红包发放请求已处理，请勿重复提交。")
		}
		if existing.CompletedAt.IsZero() {
			return s.reply(ctx, event, "🧧 当前群仍有未结束的红包，请待本轮领取及额度确认完成后再发放。")
		}
		if !existing.SummarySent {
			if err := s.announceHongbaoSummary(ctx, existing, event.Message.ID); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return s.reply(ctx, event, "🧧 读取红包状态失败，请稍后重试。")
	}
	packet := model.Hongbao{
		ID: id, GroupOpenID: group, Actor: commandRuleActor(identity),
		AllowedGroups: uniqueHongbaoGroups(fields[4:]),
		QuotaPerUnit:  status.QuotaPerUnit, TotalQuota: total, TotalCount: count,
		RemainingQuota: total, RemainingCount: count,
		Claims: make(map[int]model.HongbaoClaim), CreatedAt: s.now(),
	}
	if err := s.store.PutHongbao(packet); err != nil {
		return s.reply(ctx, event, "🧧 保存红包失败，请稍后重试。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: packet.Actor, Action: "hongbao.create", Target: id, Success: true,
		Metadata: map[string]any{"group": group, "quota": total, "count": count, "allowed_groups": packet.AllowedGroups}})
	groupText := ""
	if len(packet.AllowedGroups) > 0 {
		groupText = "领取分组：" + strings.Join(packet.AllowedGroups, "、") + "\n"
	}
	return s.reply(ctx, event, fmt.Sprintf("🧧 红包已发放\n总额度：%s\n红包数量：%d 个\n%s已绑定账户可发送 /hongbao 领取，每个账户本轮限领一次。", newapi.QuotaToDisplay(total, packet.QuotaPerUnit), count, groupText))
}

func uniqueHongbaoGroups(groups []string) []string {
	var result []string
	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if _, exists := seen[group]; exists {
			continue
		}
		seen[group] = struct{}{}
		result = append(result, group)
	}
	return result
}

func (s *Service) claimHongbao(ctx context.Context, event qq.MessageEvent, canonical string) error {
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "🧧 请先使用 /bind <邮箱或用户ID> 完成账户绑定，再领取红包。")
	}
	packet, err := s.store.GetHongbao(event.Message.GroupOpenID)
	if errors.Is(err, store.ErrNotFound) {
		return s.reply(ctx, event, "🧧 当前群暂无可领取的红包。")
	}
	if err != nil {
		return s.reply(ctx, event, "🧧 读取红包状态失败，请稍后重试。")
	}
	if len(packet.AllowedGroups) > 0 {
		user, err := s.newAPI.GetUser(ctx, binding.NewAPIID)
		if err != nil {
			return s.reply(ctx, event, "🧧 读取账户分组失败，请稍后重试："+publicError(err))
		}
		if !slices.Contains(packet.AllowedGroups, user.Group) {
			return s.replyHongbaoNotice(ctx, event, "🧧 无权限领取本轮红包，您的账户分组不符合领取条件。")
		}
	}
	if claim, exists := packet.Claims[binding.NewAPIID]; exists {
		if claim.Status != "granted" {
			return s.replyHongbaoNotice(ctx, event, "🧧 本次领取的额度发放结果尚待确认，请勿重复领取；如长时间未到账，请联系管理员核查。")
		}
		replyErr := s.replyHongbaoNotice(ctx, event, hongbaoClaimText(packet, claim.RawQuota, true))
		return errors.Join(replyErr, s.announceHongbaoSummary(ctx, packet, event.Message.ID))
	}
	if !packet.CompletedAt.IsZero() {
		replyErr := s.reply(ctx, event, "🧧 本轮红包已全部领取，请等待下一轮发放。")
		return errors.Join(replyErr, s.announceHongbaoSummary(ctx, packet, event.Message.ID))
	}
	if packet.RemainingCount == 0 {
		return s.reply(ctx, event, "🧧 本轮红包已全部分配，部分额度发放结果尚待确认。")
	}
	quota, err := randomHongbaoQuota(packet.RemainingQuota, packet.RemainingCount)
	if err != nil {
		return s.reply(ctx, event, "🧧 分配红包额度失败，请稍后重试。")
	}
	if packet.Claims == nil {
		packet.Claims = make(map[int]model.HongbaoClaim)
	}
	claim := model.HongbaoClaim{CanonicalID: canonical, RawQuota: quota, Status: "pending_confirmation"}
	packet.Claims[binding.NewAPIID] = claim
	packet.RemainingCount--
	packet.RemainingQuota -= quota
	// Reserve durably BEFORE AddQuota. A crash or ambiguous write must not retry.
	if err := s.store.PutHongbao(packet); err != nil {
		return s.reply(ctx, event, "🧧 保存领取记录失败，本次未发放额度，请稍后重试。")
	}
	unlockCredit := s.credits.Lock(binding.NewAPIID)
	err = s.newAPI.AddQuota(ctx, binding.NewAPIID, quota)
	unlockCredit()
	if err != nil {
		if !isAmbiguousQuotaWrite(err) {
			delete(packet.Claims, binding.NewAPIID)
			packet.RemainingCount++
			packet.RemainingQuota += quota
			if saveErr := s.store.PutHongbao(packet); saveErr != nil {
				s.logger.Error("红包领取回滚失败", "hongbao", packet.ID, "user", binding.NewAPIID, "error", saveErr)
				return s.reply(ctx, event, "🧧 额度发放失败，领取记录恢复异常，请联系管理员核查。")
			}
		}
		_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: canonical, Action: "hongbao.claim", Target: packet.ID, Success: false, Description: publicError(err),
			Metadata: map[string]any{"user": binding.NewAPIID, "quota": quota}})
		if isAmbiguousQuotaWrite(err) {
			return s.reply(ctx, event, "🧧 额度发放结果尚待确认，本次红包已保留，请勿重复领取；如长时间未到账，请联系管理员核查。")
		}
		return s.reply(ctx, event, "🧧 额度发放失败，本次红包未领取，可稍后重试："+publicError(err))
	}
	claim.Status = "granted"
	packet.Claims[binding.NewAPIID] = claim
	packet.GrantedCount++
	if packet.GrantedCount == packet.TotalCount {
		packet.CompletedAt = s.now()
	}
	if err := s.store.PutHongbao(packet); err != nil {
		s.logger.Error("红包额度已发放但保存完成状态失败", "hongbao", packet.ID, "user", binding.NewAPIID, "error", err)
		return s.reply(ctx, event, "🧧 红包额度已发放，但领取状态保存失败，请联系管理员核查，勿重复领取。")
	}
	_ = s.store.AddAudit(model.AuditRecord{At: s.now(), Actor: canonical, Action: "hongbao.claim", Target: packet.ID, Success: true,
		Metadata: map[string]any{"user": binding.NewAPIID, "quota": quota}})
	replyErr := s.reply(ctx, event, hongbaoClaimText(packet, quota, false))
	// Try the summary even if the individual claim reply failed.
	return errors.Join(replyErr, s.announceHongbaoSummary(ctx, packet, event.Message.ID))
}

// Double-mean allocation in integer quota units; the last claimant receives
// the remainder. big.Int avoids overflow when total quota approaches MaxInt64.
func randomHongbaoQuota(remaining int64, count int) (int64, error) {
	if count <= 0 || remaining < int64(count) {
		return 0, errors.New("红包剩余额度不足")
	}
	if count == 1 {
		return remaining, nil
	}
	upper := new(big.Int).Mul(big.NewInt(remaining), big.NewInt(2))
	upper.Div(upper, big.NewInt(int64(count)))
	max := big.NewInt(remaining - int64(count-1))
	if upper.Cmp(max) > 0 {
		upper.Set(max)
	}
	value, err := rand.Int(rand.Reader, upper)
	if err != nil {
		return 0, err
	}
	return value.Int64() + 1, nil
}

func hongbaoClaimText(packet model.Hongbao, quota int64, repeated bool) string {
	title := "🧧 红包领取成功"
	if repeated {
		title = "🧧 本轮红包已领取，请勿重复领取"
	}
	return fmt.Sprintf("%s\n领取额度：%s\n剩余红包：%d 个\n剩余额度：%s", title,
		newapi.QuotaToDisplay(quota, packet.QuotaPerUnit), packet.RemainingCount,
		newapi.QuotaToDisplay(packet.RemainingQuota, packet.QuotaPerUnit))
}

func (s *Service) announceHongbaoSummary(ctx context.Context, packet model.Hongbao, replyTo string) error {
	if packet.CompletedAt.IsZero() || packet.SummarySent {
		return nil
	}
	elapsed := packet.CompletedAt.Sub(packet.CreatedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	text := fmt.Sprintf("🎉 本轮红包已全部领取\n红包总数：%d 个\n发放总额度：%s\n领取用时：%.2f 秒",
		packet.TotalCount, newapi.QuotaToDisplay(packet.TotalQuota, packet.QuotaPerUnit), elapsed)
	// The claim uses sequence 1; the separate summary uses sequence 2 so QQ
	// does not deduplicate it. Background retries use proactive messages.
	if err := s.sendGroupReplyWithSequence(ctx, packet.GroupOpenID, replyTo, text, 2); err != nil {
		return err
	}
	packet.SummarySent = true
	return s.store.PutHongbao(packet)
}

func (s *Service) retryHongbaoSummaries(ctx context.Context) {
	packets, err := s.store.ListPendingHongbaoSummaries()
	if err != nil {
		s.logger.Error("读取待发送红包总结失败", "error", err)
		return
	}
	for _, packet := range packets {
		unlock := s.hongbaoGroups.Lock(packet.GroupOpenID)
		current, err := s.store.GetHongbao(packet.GroupOpenID)
		if err == nil && current.ID == packet.ID {
			err = s.announceHongbaoSummary(ctx, current, "")
		}
		unlock()
		if err != nil {
			s.logger.Warn("发送红包总结失败，稍后重试", "hongbao", packet.ID, "error", err)
		}
	}
}
