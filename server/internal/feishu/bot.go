package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"lumen/server/internal/storage"
)

// Bot 通过出站长连接接收飞书消息，不开放公网回调端口。
type Bot struct {
	appID     string
	appSecret string
	allowed   map[string]bool
	qa        *QAService
	responder ReplySender
	store     *storage.Store
	pairing   bool
	logger    *slog.Logger
}

// ReplySender 是回复消息的最小接口，便于测试注入。
type ReplySender interface {
	SendText(ctx context.Context, userID, text string) error
	// Enabled 表示渠道是否具备发送条件。
	Enabled() bool
}

// NewBot 创建长连接机器人。
//
// pairing 为 true 时进入一次性配对模式：非白名单用户会收到自己的 open_id，
// 但仍然不查询任何数据、不调用模型。详见 config.FeishuPairingMode。
func NewBot(appID, appSecret string, allowedUserIDs []string, qa *QAService,
	responder ReplySender, store *storage.Store, pairing bool, logger *slog.Logger) *Bot {
	allowed := make(map[string]bool, len(allowedUserIDs))
	for _, id := range allowedUserIDs {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = true
		}
	}
	return &Bot{
		appID: appID, appSecret: appSecret, allowed: allowed,
		qa: qa, responder: responder, store: store, pairing: pairing, logger: logger,
	}
}

// Enabled 表示机器人是否具备启动条件。
// 配对模式下即使白名单为空也应启动，否则无法取得 open_id。
func (b *Bot) Enabled() bool {
	if b == nil || b.appID == "" || b.appSecret == "" || b.qa == nil {
		return false
	}
	return len(b.allowed) > 0 || b.pairing
}

// Run 启动长连接，阻塞直到 ctx 取消。
// 断线由 SDK 自动重连；已持久化的总结不依赖连接存活。
func (b *Bot) Run(ctx context.Context) error {
	if !b.Enabled() {
		return errors.New("飞书机器人未配置完整（需要 App ID、App Secret 和允许用户列表）")
	}

	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			b.handleMessage(ctx, event)
			// 始终返回 nil：单条消息处理失败不应让长连接重建。
			return nil
		})

	client := larkws.NewClient(b.appID, b.appSecret,
		larkws.WithEventHandler(handler),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithAutoReconnect(true),
	)

	if b.logger != nil {
		b.logger.Info("飞书长连接启动")
	}
	return client.Start(ctx)
}

// handleMessage 处理一条入站消息。
//
// 顺序很重要：先校验发送者身份，再判断重复，最后才检索数据与调用模型。
// 非白名单用户不会触发任何查询或 AI 调用。
func (b *Bot) handleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message
	senderID := openIDOf(event)
	messageID := deref(msg.MessageId)
	chatType := deref(msg.ChatType)
	msgType := deref(msg.MessageType)

	// 1. 身份校验：只响应白名单用户。
	if senderID == "" || !b.allowed[senderID] {
		// 配对模式：这是首次部署时取得用户 open_id 的唯一途径。
		// 只回复对方自己的 open_id，不查询任何数据、不调用模型。
		// 配对完成后必须关闭该模式（LUMEN_FEISHU_PAIRING_MODE=false）。
		if b.pairing && senderID != "" {
			b.logger.Info("配对模式：收到用户消息", "open_id", senderID)
			b.reply(ctx, senderID,
				"Lumen 配对模式\n\n你的 open_id 是：\n"+senderID+
					"\n\n请把它填入服务器的 LUMEN_FEISHU_ALLOWED_USER_IDS，然后关闭配对模式。")
			return
		}
		b.recordSecurity(ctx, "feishu_unauthorized", "非白名单用户消息被拒绝", maskID(senderID))
		return
	}

	// 2. 只处理文本消息。
	if msgType != "text" {
		b.reply(ctx, senderID, "目前只支持文本消息。")
		return
	}
	text := extractText(deref(msg.Content))

	// 3. 幂等：同一条 message_id 只处理一次。
	if messageID != "" {
		handled, err := b.qa.AlreadyHandled(ctx, messageID)
		if err == nil && handled {
			b.logger.Debug("忽略重复投递的消息", "message_id", messageID)
			return
		}
	}

	// 4. 群聊里必须 @ 机器人才响应，避免误触发。
	if chatType == "group" {
		if !mentionsBot(event) {
			return
		}
		text = stripMentions(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	query := ParseQuery(text)
	answer, err := b.qa.Handle(ctx, query)
	if err != nil {
		b.logger.Error("问答处理失败", "intent", query.Intent, "error", err.Error())
		answer = Answer{Text: "查询时出现内部错误，请稍后再试。", Intent: query.Intent, Status: "error"}
	}

	if messageID != "" {
		if err := b.qa.RecordConversation(ctx, messageID, senderID, query, answer); err != nil {
			b.logger.Warn("问答记录写入失败", "error", err.Error())
		}
	}
	b.reply(ctx, senderID, answer.Text)
}

func (b *Bot) reply(ctx context.Context, userID, text string) {
	if b.responder == nil || !b.responder.Enabled() {
		return
	}
	if err := b.responder.SendText(ctx, userID, text); err != nil {
		b.logger.Warn("飞书回复失败", "error", err.Error())
	}
}

func (b *Bot) recordSecurity(ctx context.Context, kind, detail, source string) {
	if b.logger != nil {
		b.logger.Warn("安全事件", "kind", kind, "source", source)
	}
	if b.store != nil {
		if err := b.store.RecordSecurityEvent(ctx, kind, detail, source); err != nil && b.logger != nil {
			b.logger.Warn("安全事件写入失败", "error", err.Error())
		}
	}
}

// ---- SDK 结构体取值辅助 ----

func openIDOf(event *larkim.P2MessageReceiveV1) string {
	if event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return ""
	}
	if event.Event.Sender.SenderId.OpenId != nil {
		return *event.Event.Sender.SenderId.OpenId
	}
	if event.Event.Sender.SenderId.UserId != nil {
		return *event.Event.Sender.SenderId.UserId
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// extractText 从飞书文本消息的 content 字段中取出正文。
func extractText(content string) string {
	// content 形如 {"text":"@"}，这里做最小解析，避免把整个 payload 落盘。
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return ""
	}
	return parsed.Text
}

// mentionsBot 判断群消息是否 @ 了机器人。
func mentionsBot(event *larkim.P2MessageReceiveV1) bool {
	if event.Event.Message == nil {
		return false
	}
	return len(event.Event.Message.Mentions) > 0
}

// stripMentions 去掉 @ 占位符，保留用户正文。
func stripMentions(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(text), "@_user_1", ""))
}

// maskID 只保留 ID 首尾少量字符，避免安全日志中留存完整用户标识。
func maskID(id string) string {
	if len(id) <= 6 {
		return "***"
	}
	return id[:3] + "***" + id[len(id)-3:]
}

// NewLarkClient 创建官方 SDK 客户端，供其他模块（如主动发送）复用。
func NewLarkClient(appID, appSecret string) *lark.Client {
	return lark.NewClient(appID, appSecret)
}
