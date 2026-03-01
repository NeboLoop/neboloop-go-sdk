// Package neboloop provides a Go client for the NeboLoop comms protocol.
// It connects to the gateway over WebSocket, authenticates, and provides
// typed helpers for common operations (installs, channels, A2A tasks, DMs).
package neboloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"

	"github.com/NeboLoop/neboloop-go-sdk/frame"
)

// Config holds connection parameters.
type Config struct {
	Endpoint      string        // WebSocket URL (e.g. "ws://localhost:9000")
	APIEndpoint   string        // REST API URL (e.g. "http://localhost:8888") — derived from Endpoint if empty
	BotID         string        // bot UUID
	Token         string        // owner or bot JWT
	PingInterval  time.Duration // WebSocket-level OpPing interval (default 15s)
}

// Message is a received message.
type Message struct {
	MsgID          string
	ConversationID string
	Seq            uint64
	SenderID       string
	Stream         string
	Content        json.RawMessage
}

// Handler is a callback for received messages.
type Handler func(Message)

// ChannelInfo describes a loop channel the bot belongs to.
type ChannelInfo struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	LoopID      string `json:"loop_id"`
	LoopName    string `json:"loop_name"`
}

// Client connects to the NeboLoop comms gateway.
type Client struct {
	cfg        Config
	conn       net.Conn
	mu         sync.Mutex
	done       chan struct{}
	sendCh     chan []byte
	handler    Handler
	// conversation mappings from join responses
	convMu       sync.RWMutex
	convByKey    map[string]string // "botId:stream" → conversationId
	pendingJoins []string          // tracks pending join keys for mapping responses

	// loop channel mappings (populated by auto-subscribe and JoinLoopChannel)
	channelConvs map[string]string      // channelID → conversationID
	channelByConv map[string]string     // conversationID → channelID
	channelMeta  map[string]ChannelInfo // channelID → metadata
}

// Connect establishes a WebSocket connection to the gateway and authenticates.
func Connect(ctx context.Context, cfg Config, handler Handler) (*Client, error) {
	c := &Client{
		cfg:           cfg,
		done:          make(chan struct{}),
		sendCh:        make(chan []byte, 256),
		handler:       handler,
		convByKey:     make(map[string]string),
		channelConvs:  make(map[string]string),
		channelByConv: make(map[string]string),
		channelMeta:   make(map[string]ChannelInfo),
	}

	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	go c.readLoop()
	go c.writeLoop()

	return c, nil
}

func (c *Client) connect(ctx context.Context) error {
	conn, _, _, err := ws.Dial(ctx, c.cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.conn = conn

	// Send CONNECT frame
	msg := map[string]string{"token": c.cfg.Token}
	if c.cfg.BotID != "" {
		msg["bot_id"] = c.cfg.BotID
	}
	connectPayload, _ := json.Marshal(msg)

	encoded, _ := frame.Encode(frame.Header{Type: frame.TypeConnect}, connectPayload)
	if err := wsutil.WriteClientBinary(conn, encoded); err != nil {
		conn.Close()
		return fmt.Errorf("send connect: %w", err)
	}

	// Read AUTH response
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	data, err := wsutil.ReadServerBinary(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("read auth: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	h, payload, err := frame.Decode(data)
	if err != nil {
		conn.Close()
		return fmt.Errorf("decode auth: %w", err)
	}

	if h.Type == frame.TypeAuthFail {
		var result struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal(payload, &result)
		conn.Close()
		return fmt.Errorf("auth failed: %s", result.Reason)
	}

	if h.Type != frame.TypeAuthOK {
		conn.Close()
		return fmt.Errorf("unexpected frame type %d", h.Type)
	}

	slog.Info("connected to gateway", "endpoint", c.cfg.Endpoint)
	return nil
}

// Conn returns the underlying network connection.
func (c *Client) Conn() net.Conn { return c.conn }

// Done returns a channel that closes when the client shuts down.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close disconnects from the gateway.
func (c *Client) Close() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	close(c.done)
	return c.conn.Close()
}

// Send publishes a message to a conversation.
func (c *Client) Send(ctx context.Context, conversationID uuid.UUID, stream string, content json.RawMessage) error {
	payload, _ := json.Marshal(map[string]any{
		"conversation_id": conversationID.String(),
		"stream":          stream,
		"content":         content,
	})

	encoded, err := frame.Encode(frame.Header{Type: frame.TypeSendMessage}, payload)
	if err != nil {
		return err
	}

	select {
	case c.sendCh <- encoded:
		return nil
	case <-c.done:
		return errors.New("client closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// JoinConversation subscribes to an existing conversation by ID.
func (c *Client) JoinConversation(conversationID string, lastAckedSeq uint64) {
	payload, _ := json.Marshal(map[string]any{
		"conversation_id": conversationID,
		"last_acked_seq":  lastAckedSeq,
	})
	encoded, _ := frame.Encode(frame.Header{Type: frame.TypeJoinConversation}, payload)
	select {
	case c.sendCh <- encoded:
	case <-c.done:
	}
}

// JoinBotStream subscribes to a bot's named stream (e.g. "chat", "tasks").
// The gateway resolves the bot_id + stream to a conversation_id and responds.
func (c *Client) JoinBotStream(botID string, stream string) {
	c.convMu.Lock()
	c.pendingJoins = append(c.pendingJoins, botID+":"+stream)
	c.convMu.Unlock()

	payload, _ := json.Marshal(map[string]any{
		"bot_id": botID,
		"stream": stream,
	})
	encoded, _ := frame.Encode(frame.Header{Type: frame.TypeJoinConversation}, payload)
	select {
	case c.sendCh <- encoded:
	case <-c.done:
	}
}

// Ack acknowledges receipt of messages up to seq in a conversation.
func (c *Client) Ack(conversationID string, ackedSeq uint64) {
	payload, _ := json.Marshal(map[string]any{
		"conversation_id": conversationID,
		"acked_seq":       ackedSeq,
	})
	encoded, _ := frame.Encode(frame.Header{Type: frame.TypeAck}, payload)
	select {
	case c.sendCh <- encoded:
	case <-c.done:
	}
}

// --- Typed helpers ---

// InstallEvent represents an app install/update/revoke notification.
type InstallEvent struct {
	Type    string          `json:"type"`    // "app_installed", "app_updated", "app_revoked", "app_uninstalled"
	AppID   string          `json:"app_id"`
	AppName string          `json:"app_name"`
	Payload json.RawMessage `json:"payload"`
}

// OnInstall subscribes to install events. Wraps the generic handler.
func (c *Client) OnInstall(handler func(InstallEvent)) {
	// Install events arrive on the "installs" stream (auto-subscribed by gateway)
	// Wrap the generic handler to filter and type-assert
	c.handler = chainHandler(c.handler, func(m Message) {
		if m.Stream != "installs" {
			return
		}
		var ev InstallEvent
		if err := json.Unmarshal(m.Content, &ev); err == nil {
			handler(ev)
		}
	})
}

// ChannelMessage represents a message from a channel bridge.
type ChannelMessage struct {
	ChannelType string `json:"channel_type"` // "telegram", "discord"
	ChannelID   string `json:"channel_id"`
	SenderName  string `json:"sender_name"`
	Text        string `json:"text"`
}

// SendChannelMessage sends a message to a channel bridge.
func (c *Client) SendChannelMessage(ctx context.Context, convID uuid.UUID, msg ChannelMessage) error {
	content, _ := json.Marshal(msg)
	return c.Send(ctx, convID, "channels/outbound", content)
}

// TaskSubmission represents an A2A task request.
type TaskSubmission struct {
	CorrelationID string          `json:"correlation_id"`
	FromBotID     string          `json:"from_bot_id"`
	Input         json.RawMessage `json:"input"`
}

// TaskResult represents an A2A task result.
type TaskResult struct {
	CorrelationID string          `json:"correlation_id"`
	Status        string          `json:"status"` // "completed", "failed"
	Output        json.RawMessage `json:"output,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// DirectMessage represents a direct message between bots.
type DirectMessage struct {
	Text     string          `json:"text,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// --- Chat message types ---

const (
	ChatText            = "text"
	ChatAppSuggestion   = "app_suggestion"
	ChatSkillSuggestion = "skill_suggestion"
	ChatInstallConfirm  = "install_confirmed"
)

// ChatMessage is a structured message on the chat stream.
type ChatMessage struct {
	Type    string     `json:"type"`
	Text    string     `json:"text,omitempty"`
	App     *AppItem   `json:"app,omitempty"`
	Skill   *SkillItem `json:"skill,omitempty"`
	AppID   string     `json:"app_id,omitempty"`
	AppName string     `json:"app_name,omitempty"`
}

// OnChat subscribes to chat messages. Wraps the generic handler.
func (c *Client) OnChat(handler func(ChatMessage)) {
	c.handler = chainHandler(c.handler, func(m Message) {
		if m.Stream != "chat" {
			return
		}
		var msg ChatMessage
		if err := json.Unmarshal(m.Content, &msg); err == nil {
			if msg.Type == "" {
				msg.Type = ChatText
			}
			handler(msg)
		}
	})
}

// SendChat sends a structured chat message on the bot's chat stream.
func (c *Client) SendChat(ctx context.Context, msg ChatMessage) error {
	if msg.Type == "" {
		msg.Type = ChatText
	}
	c.convMu.RLock()
	convID, ok := c.convByKey[c.cfg.BotID+":chat"]
	c.convMu.RUnlock()
	if !ok {
		return errors.New("chat conversation not joined")
	}
	parsed, err := uuid.Parse(convID)
	if err != nil {
		return fmt.Errorf("invalid conversation id: %w", err)
	}
	content, _ := json.Marshal(msg)
	return c.Send(ctx, parsed, "chat", content)
}

// SuggestApp sends an app suggestion chat message.
func (c *Client) SuggestApp(ctx context.Context, app AppItem, reason string) error {
	return c.SendChat(ctx, ChatMessage{
		Type: ChatAppSuggestion,
		Text: reason,
		App:  &app,
	})
}

// SuggestSkill sends a skill suggestion chat message.
func (c *Client) SuggestSkill(ctx context.Context, skill SkillItem, reason string) error {
	return c.SendChat(ctx, ChatMessage{
		Type:  ChatSkillSuggestion,
		Text:  reason,
		Skill: &skill,
	})
}

// --- Loop channel methods ---

// JoinLoopChannel subscribes to a loop channel by channel_id.
func (c *Client) JoinLoopChannel(channelID string) {
	payload, _ := json.Marshal(map[string]string{
		"channel_id": channelID,
	})
	encoded, _ := frame.Encode(frame.Header{Type: frame.TypeJoinConversation}, payload)
	select {
	case c.sendCh <- encoded:
	case <-c.done:
	}
}

// SendLoopMessage sends a message to a loop channel.
func (c *Client) SendLoopMessage(ctx context.Context, channelID string, content json.RawMessage) error {
	c.convMu.RLock()
	convIDStr, ok := c.channelConvs[channelID]
	c.convMu.RUnlock()
	if !ok {
		return errors.New("channel not joined: " + channelID)
	}
	convID, err := uuid.Parse(convIDStr)
	if err != nil {
		return fmt.Errorf("invalid conversation id: %w", err)
	}
	return c.Send(ctx, convID, "channel", content)
}

// OnLoopMessage registers a handler for loop channel messages.
func (c *Client) OnLoopMessage(handler func(channelID string, msg Message)) {
	c.handler = chainHandler(c.handler, func(m Message) {
		c.convMu.RLock()
		chID, ok := c.channelByConv[m.ConversationID]
		c.convMu.RUnlock()
		if ok {
			handler(chID, m)
		}
	})
}

// Channels returns the current channel→conversationID map (snapshot).
func (c *Client) Channels() map[string]string {
	c.convMu.RLock()
	defer c.convMu.RUnlock()
	out := make(map[string]string, len(c.channelConvs))
	for k, v := range c.channelConvs {
		out[k] = v
	}
	return out
}

// ChannelMetas returns metadata for all joined channels (snapshot).
func (c *Client) ChannelMetas() map[string]ChannelInfo {
	c.convMu.RLock()
	defer c.convMu.RUnlock()
	out := make(map[string]ChannelInfo, len(c.channelMeta))
	for k, v := range c.channelMeta {
		out[k] = v
	}
	return out
}

// --- Internal ---

func (c *Client) readLoop() {
	for {
		data, err := wsutil.ReadServerBinary(c.conn)
		if err != nil {
			select {
			case <-c.done:
			default:
				slog.Warn("read error, disconnecting", "error", err)
				c.Close()
			}
			return
		}

		h, payload, err := frame.Decode(data)
		if err != nil {
			slog.Debug("bad frame", "error", err)
			continue
		}

		if h.IsCompressed() {
			decompressed, err := frame.Decompress(payload)
			if err != nil {
				continue
			}
			payload = decompressed
		}

		switch h.Type {
		case frame.TypeMessageDelivery:
			var delivery struct {
				SenderID string          `json:"sender_id"`
				Stream   string          `json:"stream"`
				Content  json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(payload, &delivery); err != nil {
				continue
			}

			msg := Message{
				MsgID:          uuidFromBytes(h.MsgID[:]),
				ConversationID: uuidFromBytes(h.ConversationID[:]),
				Seq:            h.Seq,
				SenderID:       delivery.SenderID,
				Stream:         delivery.Stream,
				Content:        delivery.Content,
			}

			if c.handler != nil {
				c.handler(msg)
			}

		case frame.TypeJoinConversation:
			var result struct {
				ConversationID string `json:"conversation_id"`
				ChannelID      string `json:"channel_id,omitempty"`
				ChannelName    string `json:"channel_name,omitempty"`
				LoopID         string `json:"loop_id,omitempty"`
			}
			if err := json.Unmarshal(payload, &result); err == nil {
				c.convMu.Lock()
				if result.ChannelID != "" {
					// Loop channel join (auto or explicit)
					c.channelConvs[result.ChannelID] = result.ConversationID
					c.channelByConv[result.ConversationID] = result.ChannelID
					c.channelMeta[result.ChannelID] = ChannelInfo{
						ChannelID:   result.ChannelID,
						ChannelName: result.ChannelName,
						LoopID:      result.LoopID,
					}
				} else if len(c.pendingJoins) > 0 {
					// Bot stream join
					key := c.pendingJoins[0]
					c.pendingJoins = c.pendingJoins[1:]
					c.convByKey[key] = result.ConversationID
				}
				c.convMu.Unlock()
			}

		case frame.TypeReplay:
			// informational
		}
	}
}

func (c *Client) writeLoop() {
	// Send WebSocket-level pings to keep the connection alive through load
	// balancers and CDNs that have idle timeouts. Must be well under the
	// server's idle timeout — a ping at the boundary races and loses.
	// All writes go through this single goroutine to avoid concurrent write corruption.
	pingInterval := c.cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = 15 * time.Second
	}
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case data := <-c.sendCh:
			if err := wsutil.WriteClientBinary(c.conn, data); err != nil {
				slog.Warn("write error", "error", err)
				c.Close()
				return
			}
		case <-ticker.C:
			if err := wsutil.WriteClientMessage(c.conn, ws.OpPing, nil); err != nil {
				slog.Debug("ping write error, closing", "error", err)
				c.Close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func chainHandler(existing, additional Handler) Handler {
	return func(m Message) {
		if existing != nil {
			existing(m)
		}
		additional(m)
	}
}

func uuidFromBytes(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
