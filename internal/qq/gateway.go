package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/fsykk/new-api-bot/internal/store"
)

const groupAndC2CIntent = 1 << 25

type Gateway struct {
	client    *Client
	store     *store.Store
	logger    *slog.Logger
	connected atomic.Bool
	lastEvent atomic.Int64
}

type FatalGatewayError struct {
	Code int
	Err  error
}

func (e *FatalGatewayError) Error() string {
	return fmt.Sprintf("QQ Gateway 致命关闭码 %d: %v", e.Code, e.Err)
}

func (e *FatalGatewayError) Unwrap() error { return e.Err }

type Payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type MessageEvent struct {
	EventType   string
	Sequence    int64
	Message     Message
	Member      GroupMemberEvent
	JoinRequest GroupJoinRequest
}

type Message struct {
	ID          string          `json:"id"`
	Content     string          `json:"content"`
	GroupOpenID string          `json:"group_openid"`
	Author      MessageAuthor   `json:"author"`
	Mentions    []MessageAuthor `json:"mentions"`
	Scene       MessageScene    `json:"message_scene"`
	Elements    []MsgElement    `json:"msg_elements"`
}

type MsgElement struct {
	MsgIdx      string        `json:"msg_idx"`
	MessageType int           `json:"message_type"`
	Content     string        `json:"content"`
	Author      MessageAuthor `json:"author"`
	Elements    []MsgElement  `json:"msg_elements"`
}

type GroupMemberEvent struct {
	Timestamp    int64  `json:"timestamp"`
	GroupOpenID  string `json:"group_openid"`
	MemberOpenID string `json:"member_openid"`
	UserOpenID   string `json:"user_openid"`
}

type GroupJoinRequest struct {
	GroupOpenID   string              `json:"group_openid"`
	JoinRequestID string              `json:"join_request_id"`
	RiskTips      string              `json:"risk_tips"`
	UnionOpenID   string              `json:"union_openid"`
	MemberOpenID  string              `json:"member_openid"`
	Username      string              `json:"username"`
	ApplyAt       string              `json:"apply_at"`
	ApplySource   string              `json:"apply_source"`
	InvitedBy     string              `json:"invited_by"`
	Bot           bool                `json:"bot"`
	QQLevel       OptionalInt         `json:"qq_level"`
	Level         OptionalInt         `json:"level"`
	VerifyInfo    GroupJoinVerifyInfo `json:"verify_info"`
	AutoApproved  *GroupAutoApproved  `json:"auto_approved,omitempty"`
}

// OptionalInt accepts both JSON numbers and numeric strings while retaining
// whether QQ actually supplied the field.
type OptionalInt struct {
	Value int
	Set   bool
}

func (i *OptionalInt) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if value == "" || value == "null" {
		return nil
	}
	value = strings.Trim(value, "\"")
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid optional integer %q", value)
	}
	i.Value = parsed
	i.Set = true
	return nil
}

func (r GroupJoinRequest) UserLevel() (int, bool) {
	if r.QQLevel.Set {
		return r.QQLevel.Value, true
	}
	if r.Level.Set {
		return r.Level.Value, true
	}
	return 0, false
}

type GroupJoinVerifyInfo struct {
	Method        string              `json:"method"`
	VerifyMessage string              `json:"verify_message"`
	ReviewQAList  []GroupJoinReviewQA `json:"review_qa_list"`
}

type GroupJoinReviewQA struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type GroupAutoApproved struct {
	StrategyID string `json:"strategy_id"`
}

type GroupJoinRequestPage struct {
	List       []GroupJoinRequest `json:"list"`
	NextCursor string             `json:"next_cursor"`
}

type MessageAuthor struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	Bot            bool   `json:"bot"`
	UnionOpenID    string `json:"union_openid"`
	UserOpenID     string `json:"user_openid"`
	MemberOpenID   string `json:"member_openid"`
	MemberRole     string `json:"member_role"`
	UnionAccountID string `json:"union_user_account"`
}

type MessageScene struct {
	Source string   `json:"source"`
	Ext    []string `json:"ext"`
}

func NewGateway(client *Client, storage *store.Store, logger *slog.Logger) *Gateway {
	return &Gateway{client: client, store: storage, logger: logger}
}

func (g *Gateway) Connected() bool { return g.connected.Load() }

func (g *Gateway) LastEvent() time.Time {
	v := g.lastEvent.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

func (g *Gateway) Run(ctx context.Context, handler func(context.Context, MessageEvent)) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := g.connect(ctx, handler)
		g.connected.Store(false)
		if ctx.Err() != nil {
			return nil
		}
		var fatal *FatalGatewayError
		if errors.As(err, &fatal) {
			return fatal
		}
		g.logger.Warn("QQ Gateway 连接中断", "error", err, "retry_in", backoff)
		jitter := time.Duration(rand.IntN(500)) * time.Millisecond
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff + jitter):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func (g *Gateway) connect(ctx context.Context, handler func(context.Context, MessageEvent)) error {
	info, err := g.client.Gateway(ctx)
	if err != nil {
		return err
	}
	token, err := g.client.AccessToken(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, info.URL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	helloCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, data, err := conn.Read(helloCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("读取 Gateway Hello 失败: %w", err)
	}
	var hello struct {
		Op int `json:"op"`
		D  struct {
			HeartbeatInterval int `json:"heartbeat_interval"`
		} `json:"d"`
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Op != 10 || hello.D.HeartbeatInterval <= 0 {
		return errors.New("Gateway Hello 响应无效")
	}

	state, stateErr := g.store.GetGatewayState()
	if stateErr == nil && state.SessionID != "" {
		err = writeJSON(ctx, conn, map[string]any{"op": 6, "d": map[string]any{"token": "QQBot " + token, "session_id": state.SessionID, "seq": state.Sequence}})
	} else {
		err = writeJSON(ctx, conn, map[string]any{"op": 2, "d": map[string]any{
			"token":   "QQBot " + token,
			"intents": groupAndC2CIntent,
			"shard":   []int{0, 1},
			"properties": map[string]string{
				"$os": "linux", "$browser": "new-api-bot", "$device": "new-api-bot",
			},
		}})
	}
	if err != nil {
		return err
	}

	var writeMu sync.Mutex
	var sequence atomic.Int64
	if stateErr == nil {
		sequence.Store(state.Sequence)
	}
	lastAck := atomic.Int64{}
	lastAck.Store(time.Now().UnixNano())
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go func() {
		interval := time.Duration(hello.D.HeartbeatInterval) * time.Millisecond
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastAck.Load())) > 2*interval {
					_ = conn.Close(websocket.StatusGoingAway, "heartbeat timeout")
					return
				}
				var heartbeatSequence any
				if current := sequence.Load(); current > 0 {
					heartbeatSequence = current
				}
				writeMu.Lock()
				err := writeJSON(heartbeatCtx, conn, map[string]any{"op": 1, "d": heartbeatSequence})
				writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			status := websocket.CloseStatus(err)
			switch {
			case status == 4006 || status == 4007 || (status >= 4900 && status <= 4913):
				_ = g.store.ClearGatewayState()
			case status == 4001 || status == 4002 || (status >= 4010 && status <= 4014) || status == 4914 || status == 4915:
				_ = g.store.ClearGatewayState()
				return &FatalGatewayError{Code: int(status), Err: err}
			}
			return err
		}
		var payload Payload
		if err := json.Unmarshal(data, &payload); err != nil {
			g.logger.Warn("忽略无法解析的 Gateway 消息", "error", err)
			continue
		}
		if payload.S != nil {
			sequence.Store(*payload.S)
			_ = g.store.PutGatewayState(store.GatewayState{SessionID: state.SessionID, Sequence: *payload.S})
		}
		switch payload.Op {
		case 11:
			lastAck.Store(time.Now().UnixNano())
		case 0:
			g.connected.Store(true)
			g.lastEvent.Store(time.Now().Unix())
			g.logger.Info("收到 QQ Gateway 事件", "event", payload.T, "sequence", sequence.Load())
			if payload.T == "READY" {
				var ready struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(payload.D, &ready) == nil && ready.SessionID != "" {
					state.SessionID = ready.SessionID
					_ = g.store.PutGatewayState(store.GatewayState{SessionID: ready.SessionID, Sequence: sequence.Load()})
				}
				continue
			}
			if payload.T == "RESUMED" {
				continue
			}
			if payload.T == "GROUP_MEMBER_ADD" || payload.T == "GROUP_MEMBER_REMOVE" {
				var member GroupMemberEvent
				if err := json.Unmarshal(payload.D, &member); err != nil {
					g.logger.Warn("解析群成员事件失败", "event", payload.T, "error", err)
					continue
				}
				g.logger.Info("收到 QQ 群成员事件", "event", payload.T, "group_openid_present", member.GroupOpenID != "", "member_openid_present", member.MemberOpenID != "")
				handler(ctx, MessageEvent{EventType: payload.T, Sequence: sequence.Load(), Member: member})
				continue
			}
			if payload.T == "GROUP_JOIN_REQUEST" {
				var request GroupJoinRequest
				if err := json.Unmarshal(payload.D, &request); err != nil {
					g.logger.Warn("解析用户入群申请事件失败", "error", err)
					continue
				}
				g.logger.Info("收到用户入群申请事件",
					"group_openid_present", request.GroupOpenID != "",
					"member_openid_present", request.MemberOpenID != "",
					"join_request_id_present", request.JoinRequestID != "",
					"verify_method", request.VerifyInfo.Method,
				)
				handler(ctx, MessageEvent{EventType: payload.T, Sequence: sequence.Load(), JoinRequest: request})
				continue
			}
			// QQ 当前生产环境的群消息事件名为 GROUP_MESSAGE_CREATE；
			// 同时保留旧文档/旧环境使用的 GROUP_AT_MESSAGE_CREATE 兼容性。
			if !isMessageCreateEvent(payload.T) {
				continue
			}
			var message Message
			if err := json.Unmarshal(payload.D, &message); err != nil {
				g.logger.Warn("解析 QQ 消息事件失败", "event", payload.T, "error", err)
				continue
			}
			g.logger.Info("收到 QQ 消息事件",
				"event", payload.T,
				"message_id_present", message.ID != "",
				"content_length", len([]rune(message.Content)),
				"group_present", message.GroupOpenID != "",
				"mention_count", len(message.Mentions),
			)
			handler(ctx, MessageEvent{EventType: payload.T, Sequence: sequence.Load(), Message: message})
		}
	}
}

func isMessageCreateEvent(eventType string) bool {
	switch eventType {
	case "C2C_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", "GROUP_AT_MESSAGE_CREATE":
		return true
	default:
		return false
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}
