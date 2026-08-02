package bot

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type groupDiagnosticAPI interface {
	GetGroupBotState(context.Context, string) (qq.GroupBotState, error)
	GetGroupInfo(context.Context, string) (qq.GroupInfo, error)
	AccessToken(context.Context) (string, error)
}

type groupRecallAPI interface {
	RecallGroupMessage(context.Context, string, string) error
}
type groupFileAPI interface {
	SendGroupFile(context.Context, string, string, string, int, []byte) (qq.SentMessage, error)
}
type userAdminAPI interface {
	ManageUserStatus(context.Context, int, string) error
	ResetUser2FA(context.Context, int) error
	ResetUserPasskey(context.Context, int) error
}

func (s *Service) handleMemberAdd(ctx context.Context, event qq.MessageEvent) {
	setting, err := s.store.GetGroupWelcome(event.Member.GroupOpenID)
	if err != nil || !setting.Enabled {
		return
	}
	message := strings.TrimSpace(setting.Message)
	if message == "" {
		message = s.cfg.WelcomeDefault
	}
	if event.Member.MemberOpenID != "" {
		message = mentionMember(event.Member.MemberOpenID) + " " + message
	}
	if err := s.qq.ReplyGroup(ctx, event.Member.GroupOpenID, "", message); err != nil {
		s.logger.Warn("发送新成员欢迎消息失败", "error", err)
	}
}

func (s *Service) handleWelcome(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string, content string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有配置群欢迎消息的权限。")
	}
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "该指令只能在群聊中使用。")
	}
	if len(fields) < 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/welcome on、/welcome off、/welcome set <欢迎语>")
	}
	setting, _ := s.store.GetGroupWelcome(event.Message.GroupOpenID)
	setting.GroupOpenID = event.Message.GroupOpenID
	switch strings.ToLower(fields[1]) {
	case "on":
		setting.Enabled = true
		if strings.TrimSpace(setting.Message) == "" {
			setting.Message = s.cfg.WelcomeDefault
		}
	case "off":
		setting.Enabled = false
	case "set":
		parts := strings.SplitN(content, " ", 3)
		if len(parts) < 3 || strings.TrimSpace(parts[2]) == "" {
			return s.reply(ctx, event, "格式错误。正确用法：/welcome set <欢迎语>")
		}
		setting.Message = strings.TrimSpace(parts[2])
		setting.Enabled = true
	default:
		return s.reply(ctx, event, "格式错误。正确用法：/welcome on、/welcome off、/welcome set <欢迎语>")
	}
	if len([]rune(setting.Message)) > 500 {
		return s.reply(ctx, event, "欢迎语不能超过 500 个字符。")
	}
	if err := s.store.PutGroupWelcome(setting); err != nil {
		return s.reply(ctx, event, "保存欢迎消息设置失败。")
	}
	if setting.Enabled {
		return s.reply(ctx, event, "新成员欢迎已开启。当前欢迎语：\n"+setting.Message)
	}
	return s.reply(ctx, event, "新成员欢迎已关闭。")
}

func (s *Service) handleBotStatus(ctx context.Context, event qq.MessageEvent, fields []string) error {
	if len(fields) != 2 || !strings.EqualFold(fields[1], "status") {
		return s.reply(ctx, event, "格式错误。正确用法：/bot status")
	}
	lines := []string{"机器人状态诊断："}
	connected := false
	if s.gatewayConnected != nil {
		connected = s.gatewayConnected()
	}
	lines = append(lines, fmt.Sprintf("Gateway：%s", boolStatus(connected)))
	if api, ok := s.qq.(groupDiagnosticAPI); ok {
		_, tokenErr := api.AccessToken(ctx)
		lines = append(lines, "QQ Token："+boolStatus(tokenErr == nil))
		if event.Message.GroupOpenID != "" {
			state, err := api.GetGroupBotState(ctx, event.Message.GroupOpenID)
			if err != nil {
				lines = append(lines, "群内详细状态：接口未开放或查询失败（"+publicQQError(err)+"）")
			} else {
				lines = append(lines, "加入群聊时间："+formatUnixFlexible(state.JoinedAt, s.cfg.CheckinTimezone), "允许主动消息："+boolStatus(state.AllowProactiveMsg), "接收消息模式："+recvSettingText(state.RecvMsgSetting), "群内角色："+memberRoleText(state.MemberRole))
			}
			if info, err := api.GetGroupInfo(ctx, event.Message.GroupOpenID); err == nil {
				lines = append(lines, fmt.Sprintf("群信息：%s（%d 人）", nonEmpty(info.GroupName, "未命名群"), info.GroupMemberNum))
			}
		}
	} else {
		lines = append(lines, "QQ Token：客户端不支持诊断")
	}
	if status, err := s.newAPI.GetStatus(ctx, true); err != nil {
		lines = append(lines, "New API：异常（"+publicError(err)+"）")
	} else {
		lines = append(lines, fmt.Sprintf("New API：正常｜%s %s", status.SystemName, status.Version))
	}
	return s.reply(ctx, event, strings.Join(lines, "\n"))
}

func (s *Service) handleRecall(ctx context.Context, event qq.MessageEvent, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有撤回机器人消息的权限。")
	}
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "撤回功能仅支持群聊。")
	}
	if len(fields) > 2 {
		return s.reply(ctx, event, "格式错误。请回复机器人两分钟内发送的消息并输入 /recall；也可使用 /recall <消息ID>。")
	}
	reference := ""
	if len(fields) == 2 {
		reference = fields[1]
	}
	if reference == "" {
		reference = sceneValue(event.Message.Scene.Ext, "ref_msg_idx")
	}
	if reference == "" {
		reference = findReferenceIndex(event.Message.Elements)
	}
	sent, err := s.store.GetSentBotMessage(event.Message.GroupOpenID, reference)
	if err != nil {
		return s.reply(ctx, event, "未找到可撤回的机器人消息，请回复机器人刚刚发送的消息后重试。")
	}
	if time.Since(sent.SentAt) > 2*time.Minute {
		return s.reply(ctx, event, "该机器人消息发送已超过两分钟，QQ 不再允许撤回。")
	}
	api, ok := s.qq.(groupRecallAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持消息撤回。")
	}
	if err := api.RecallGroupMessage(ctx, sent.GroupOpenID, sent.MessageID); err != nil {
		return s.reply(ctx, event, "撤回失败："+publicQQError(err))
	}
	return s.reply(ctx, event, "机器人消息已撤回。")
}

func (s *Service) handleAdminUser(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有执行用户状态管理的权限。")
	}
	if len(fields) != 4 {
		return s.reply(ctx, event, "用法：/admin user status|enable|disable|reset2fa|resetpasskey <用户ID或@用户>")
	}
	action := strings.ToLower(fields[2])
	userID, description, err := s.resolveUserTarget(event, fields[3])
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	user, err := s.newAPI.GetUser(ctx, userID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	if action == "status" {
		return s.reply(ctx, event, fmt.Sprintf("%s New API 用户 %d\n用户名：%s\n邮箱：%s\n状态：%s\n角色：%d\n分组：%s", description, user.ID, user.Username, store.MaskEmail(user.Email), userStatusText(user.Status), user.Role, user.Group))
	}
	api, ok := s.newAPI.(userAdminAPI)
	if !ok {
		return s.reply(ctx, event, "当前 New API 版本不支持该用户管理操作。")
	}
	if action == "enable" {
		err = api.ManageUserStatus(ctx, userID, "enable")
		s.auditUserAction(canonical, action, userID, err)
		if err != nil {
			return s.reply(ctx, event, "启用用户失败："+publicError(err))
		}
		return s.reply(ctx, event, fmt.Sprintf("已启用%s New API 用户 %d。", description, userID))
	}
	if action != "disable" && action != "reset2fa" && action != "resetpasskey" {
		return s.reply(ctx, event, "用法：/admin user status|enable|disable|reset2fa|resetpasskey <用户ID或@用户>")
	}
	code, codeErr := randomCode(8)
	if codeErr != nil {
		return s.reply(ctx, event, "生成确认码失败，请稍后重试。")
	}
	pending := model.PendingAdminAction{Code: code, Actor: canonical, Action: action, TargetID: userID, TargetLabel: description, ExpiresAt: time.Now().Add(5 * time.Minute)}
	if err := s.store.PutPendingAdminAction(pending); err != nil {
		return s.reply(ctx, event, "保存待确认操作失败。")
	}
	return s.reply(ctx, event, fmt.Sprintf("该操作需要二次确认：%s New API 用户 %d。请在 5 分钟内输入 /confirm %s。", adminActionText(action), userID, code))
}

func (s *Service) handleConfirm(ctx context.Context, event qq.MessageEvent, canonical string, identity model.QQIdentity, fields []string) error {
	if !s.isAdmin(identity) {
		return s.reply(ctx, event, "你没有确认管理员操作的权限。")
	}
	if len(fields) != 2 {
		return s.reply(ctx, event, "格式错误。正确用法：/confirm <一次性操作码>")
	}
	pending, err := s.store.TakePendingAdminAction(strings.ToUpper(fields[1]), canonical, time.Now())
	if err != nil {
		return s.reply(ctx, event, "确认码无效、已过期或不属于当前管理员。")
	}
	api, ok := s.newAPI.(userAdminAPI)
	if !ok {
		return s.reply(ctx, event, "当前 New API 版本不支持该用户管理操作。")
	}
	switch pending.Action {
	case "disable":
		err = api.ManageUserStatus(ctx, pending.TargetID, "disable")
	case "reset2fa":
		err = api.ResetUser2FA(ctx, pending.TargetID)
	case "resetpasskey":
		err = api.ResetUserPasskey(ctx, pending.TargetID)
	default:
		err = fmt.Errorf("未知待确认操作")
	}
	s.auditUserAction(canonical, pending.Action, pending.TargetID, err)
	if err != nil {
		return s.reply(ctx, event, adminActionText(pending.Action)+"失败："+publicError(err))
	}
	return s.reply(ctx, event, fmt.Sprintf("%s成功：New API 用户 %d。", adminActionText(pending.Action), pending.TargetID))
}

func (s *Service) auditUserAction(actor, action string, target int, err error) {
	_ = s.store.AddAudit(model.AuditRecord{At: time.Now(), Actor: actor, Action: "user." + action, Target: strconv.Itoa(target), Success: err == nil, Description: errorText(err)})
}

func (s *Service) handleUsageChart(ctx context.Context, event qq.MessageEvent, canonical, duration string) error {
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "用量图表目前仅支持群聊发送。")
	}
	binding, err := s.store.GetBinding(canonical)
	if err != nil {
		return s.reply(ctx, event, "未找到当前用户的绑定信息。")
	}
	start, end, label, err := parseInsightRange(duration, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	user, err := s.newAPI.GetUser(ctx, binding.NewAPIID)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	records, err := s.newAPI.ListUsageByModel(ctx, start, end, user.Username)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	data, err := renderUsageChart(records, start, end, s.cfg.CheckinTimezone)
	if err != nil {
		return s.reply(ctx, event, "生成用量图表失败。")
	}
	api, ok := s.qq.(groupFileAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持图片上传。")
	}
	name := "usage-" + time.Now().Format("20060102-150405") + ".png"
	sent, err := api.SendGroupFile(ctx, event.Message.GroupOpenID, event.Message.ID, name, 1, data)
	if err != nil {
		return s.reply(ctx, event, "发送用量图表失败："+publicQQError(err))
	}
	if sent.ID != "" {
		_ = s.store.PutSentBotMessage(model.SentBotMessage{GroupOpenID: event.Message.GroupOpenID, MessageID: sent.ID, SentAt: time.Now()})
	}
	return s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, "", fmt.Sprintf("New API 用户 %d 的%s用量图表已生成（折线：每日额度；色块：模型占比）。", user.ID, label))
}

func (s *Service) handleAdminReportExport(ctx context.Context, event qq.MessageEvent, fields []string) error {
	if event.Message.GroupOpenID == "" {
		return s.reply(ctx, event, "CSV 报表导出仅支持群聊。")
	}
	if len(fields) > 4 {
		return s.reply(ctx, event, "格式错误。正确用法：/admin report export [时间长度]")
	}
	duration := "today"
	if len(fields) == 4 {
		duration = fields[3]
	}
	start, end, label, err := parseInsightRange(duration, time.Now(), s.cfg.CheckinTimezone)
	if err != nil {
		return s.reply(ctx, event, err.Error())
	}
	users, err := s.newAPI.ListUsers(ctx)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	rows, err := s.newAPI.ListUsageByUser(ctx, start, end)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	status, err := s.newAPI.GetStatus(ctx, false)
	if err != nil {
		return s.reply(ctx, event, publicError(err))
	}
	var buffer bytes.Buffer
	buffer.WriteString("\xEF\xBB\xBF")
	writer := csv.NewWriter(&buffer)
	_ = writer.Write([]string{"用户ID", "用户名", "请求次数", "Token", "消耗额度", "当前余额", "开始时间", "结束时间"})
	userMap := make(map[int]newapi.User, len(users))
	for _, user := range users {
		userMap[user.ID] = user
	}
	for _, item := range mergeUsersAndUsage(users, rows) {
		user := userMap[item.UserID]
		_ = writer.Write([]string{strconv.Itoa(item.UserID), item.Username, strconv.FormatInt(item.Count, 10), strconv.FormatInt(item.Tokens, 10), newapi.QuotaToDisplay(item.Quota, status.QuotaPerUnit), newapi.QuotaToDisplay(user.Quota, status.QuotaPerUnit), start.In(s.cfg.CheckinTimezone).Format(time.RFC3339), end.In(s.cfg.CheckinTimezone).Format(time.RFC3339)})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return s.reply(ctx, event, "生成 CSV 报表失败。")
	}
	api, ok := s.qq.(groupFileAPI)
	if !ok {
		return s.reply(ctx, event, "当前 QQ 客户端不支持文件上传。")
	}
	name := "newapi-report-" + time.Now().Format("20060102-150405") + ".csv"
	_, err = api.SendGroupFile(ctx, event.Message.GroupOpenID, event.Message.ID, name, 4, buffer.Bytes())
	if err != nil {
		return s.reply(ctx, event, "发送 CSV 报表失败："+publicQQError(err))
	}
	return s.qq.ReplyGroup(ctx, event.Message.GroupOpenID, "", "全站 "+label+" CSV 报表已发送。")
}

func renderUsageChart(records []newapi.UsageRecord, start, end time.Time, location *time.Location) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 1000, 600))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{248, 250, 252, 255}}, image.Point{}, draw.Src)
	days := int(end.Sub(start)/(24*time.Hour)) + 1
	if days < 2 {
		days = 2
	}
	if days > 32 {
		days = 32
	}
	daily := make([]int64, days)
	modelQuota := map[string]int64{}
	for _, record := range records {
		idx := days - 1
		if record.CreatedAt > 0 {
			d := time.Unix(record.CreatedAt, 0).In(location).Sub(start.In(location))
			idx = int(d / (24 * time.Hour))
			if idx < 0 {
				idx = 0
			}
			if idx >= days {
				idx = days - 1
			}
		}
		daily[idx] += record.Quota
		modelQuota[nonEmpty(record.ModelName, "unknown")] += record.Quota
	}
	axis := color.RGBA{71, 85, 105, 255}
	drawLine(img, 70, 40, 70, 350, axis)
	drawLine(img, 70, 350, 950, 350, axis)
	maxQ := int64(1)
	for _, q := range daily {
		if q > maxQ {
			maxQ = q
		}
	}
	prevX, prevY := 70, 350
	for i, q := range daily {
		x := 70 + i*880/(days-1)
		y := 350 - int(q*280/maxQ)
		drawLine(img, prevX, prevY, x, y, color.RGBA{37, 99, 235, 255})
		fillRect(img, x-3, y-3, x+4, y+4, color.RGBA{29, 78, 216, 255})
		prevX, prevY = x, y
	}
	type pair struct {
		name  string
		quota int64
	}
	pairs := make([]pair, 0, len(modelQuota))
	var total int64
	for name, q := range modelQuota {
		pairs = append(pairs, pair{name, q})
		total += q
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].quota > pairs[j].quota })
	colors := []color.RGBA{{16, 185, 129, 255}, {245, 158, 11, 255}, {139, 92, 246, 255}, {239, 68, 68, 255}, {6, 182, 212, 255}, {100, 116, 139, 255}}
	x := 70
	if total == 0 {
		total = 1
	}
	for i, p := range pairs {
		if i >= 6 {
			break
		}
		width := int(p.quota * 880 / total)
		if width < 2 {
			width = 2
		}
		fillRect(img, x, 430, min(950, x+width), 540, colors[i%len(colors)])
		x += width
	}
	var output bytes.Buffer
	err := png.Encode(&output, img)
	return output.Bytes(), err
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx := abs(x1 - x0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -abs(y1 - y0)
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	e := dx + dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}
func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	draw.Draw(img, image.Rect(x0, y0, x1, y1), &image.Uniform{c}, image.Point{}, draw.Src)
}
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func findReferenceIndex(elements []qq.MsgElement) string {
	for _, e := range elements {
		if e.MessageType == 103 && e.MsgIdx != "" {
			return e.MsgIdx
		}
		if nested := findReferenceIndex(e.Elements); nested != "" {
			return nested
		}
	}
	return ""
}
func boolStatus(value bool) string {
	if value {
		return "正常/是"
	}
	return "异常/否"
}
func recvSettingText(value int) string {
	switch value {
	case 0:
		return "全部消息"
	case 1:
		return "仅 @"
	case 2:
		return "@ 与上下文"
	default:
		return fmt.Sprintf("模式 %d", value)
	}
}
func memberRoleText(value int) string {
	switch value {
	case 1:
		return "普通成员"
	case 2:
		return "管理员"
	case 4:
		return "群主"
	default:
		return fmt.Sprintf("角色 %d", value)
	}
}
func formatUnixFlexible(value int64, loc *time.Location) string {
	if value <= 0 {
		return "未知"
	}
	if value > 1e12 {
		value /= 1000
	}
	return time.Unix(value, 0).In(loc).Format("2006-01-02 15:04:05")
}
func publicQQError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
	r := []rune(value)
	if len(r) > 180 {
		value = string(r[:180]) + "…"
	}
	return value
}
func adminActionText(action string) string {
	switch action {
	case "disable":
		return "禁用用户"
	case "reset2fa":
		return "重置 2FA"
	case "resetpasskey":
		return "重置 Passkey"
	case "enable":
		return "启用用户"
	default:
		return action
	}
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return publicError(err)
}
