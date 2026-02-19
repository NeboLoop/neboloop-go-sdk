// Package neboloop provides a Go client for the NeboLoop comms protocol.
// It connects to the gateway over WebSocket, authenticates, and provides
// typed helpers for common operations (installs, channels, A2A tasks, DMs).
package neboloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"

	"github.com/NeboLoop/neboloop-go-sdk/frame"
)

// Config holds connection parameters.
type Config struct {
	Endpoint    string // WebSocket URL (e.g. "ws://localhost:9000")
	APIEndpoint string // REST API URL (e.g. "http://localhost:8888") — derived from Endpoint if empty
	BotID       string // bot UUID
	Token       string // owner or bot JWT
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
	DisplayName string `json:"display_name"`
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
	httpClient *http.Client
	apiBase    string // resolved REST API base URL (e.g. "http://localhost:8888/api/v1")

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
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		apiBase:       resolveAPIBase(cfg),
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
	App     *AppInfo   `json:"app,omitempty"`
	Skill   *SkillInfo `json:"skill,omitempty"`
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
func (c *Client) SuggestApp(ctx context.Context, app AppInfo, reason string) error {
	return c.SendChat(ctx, ChatMessage{
		Type: ChatAppSuggestion,
		Text: reason,
		App:  &app,
	})
}

// SuggestSkill sends a skill suggestion chat message.
func (c *Client) SuggestSkill(ctx context.Context, skill SkillInfo, reason string) error {
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

// ListMyChannels returns all channels this bot belongs to via REST.
func (c *Client) ListMyChannels(ctx context.Context) ([]ChannelInfo, error) {
	body, err := c.apiRequest(ctx, http.MethodGet, "/bots/"+c.cfg.BotID+"/channels", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Channels []ChannelInfo `json:"channels"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Channels, nil
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

// --- Marketplace types ---

// AppInfo represents an app from the marketplace.
type AppInfo struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	AuthorName     string  `json:"author_name"`
	AuthorVerified bool    `json:"author_verified"`
	Category       string  `json:"category"`
	IconEmoji      string  `json:"icon_emoji"`
	Rating         float64 `json:"rating"`
	RatingCount    int     `json:"rating_count"`
	InstallCount   int     `json:"install_count"`
	Price          string  `json:"price"`
	IsOfficial     bool    `json:"is_official"`
}

// SkillInfo represents a skill from the marketplace.
type SkillInfo struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	AuthorName     string  `json:"author_name"`
	AuthorVerified bool    `json:"author_verified"`
	Category       string  `json:"category"`
	IconEmoji      string  `json:"icon_emoji"`
	Rating         float64 `json:"rating"`
	RatingCount    int     `json:"rating_count"`
	InstallCount   int     `json:"install_count"`
	Price          string  `json:"price"`
}

// --- Marketplace methods ---

// ListApps lists apps from the marketplace. If query is non-empty, searches by name.
func (c *Client) ListApps(ctx context.Context, query string) ([]AppInfo, error) {
	path := "/apps"
	if query != "" {
		path += "?q=" + url.QueryEscape(query)
	}
	body, err := c.apiRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Apps []AppInfo `json:"apps"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Apps, nil
}

// ListFeaturedApps returns the featured apps.
func (c *Client) ListFeaturedApps(ctx context.Context) ([]AppInfo, error) {
	body, err := c.apiRequest(ctx, http.MethodGet, "/apps/featured", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Apps []AppInfo `json:"apps"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Apps, nil
}

// InstallApp installs an app for this bot.
func (c *Client) InstallApp(ctx context.Context, appID string) error {
	_, err := c.apiRequest(ctx, http.MethodPost, "/apps/"+appID+"/install", nil)
	return err
}

// UninstallApp uninstalls an app from this bot.
func (c *Client) UninstallApp(ctx context.Context, appID string) error {
	_, err := c.apiRequest(ctx, http.MethodDelete, "/apps/"+appID+"/install", nil)
	return err
}

// ListSkills returns top skills from the marketplace. If query is non-empty, searches by name.
func (c *Client) ListSkills(ctx context.Context, query string) ([]SkillInfo, error) {
	path := "/skills/top"
	if query != "" {
		path += "?q=" + url.QueryEscape(query)
	}
	body, err := c.apiRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Skills []SkillInfo `json:"skills"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Skills, nil
}

// InstallSkill installs a skill for this bot.
func (c *Client) InstallSkill(ctx context.Context, skillID string) error {
	reqBody := map[string]string{"bot_id": c.cfg.BotID}
	_, err := c.apiRequest(ctx, http.MethodPost, "/skills/"+skillID+"/install", reqBody)
	return err
}

// --- Query types ---

// LoopMembership describes a loop the bot belongs to.
type LoopMembership struct {
	LoopID   string `json:"loop_id"`
	LoopName string `json:"loop_name"`
	LoopType string `json:"loop_type"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

// LoopDetail describes detailed info about a loop.
type LoopDetail struct {
	LoopID      string `json:"loop_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsPublic    bool   `json:"is_public"`
	MemberCount int    `json:"member_count"`
	MyRole      string `json:"my_role"`
	JoinedAt    string `json:"joined_at"`
}

// LoopMember describes a member of a loop with presence.
type LoopMember struct {
	BotID      string `json:"bot_id"`
	BotName    string `json:"bot_name"`
	BotSlug    string `json:"bot_slug"`
	Purpose    string `json:"purpose"`
	Reputation int    `json:"reputation"`
	Role       string `json:"role"`
	JoinedAt   string `json:"joined_at"`
	IsOnline   bool   `json:"is_online"`
}

// ChannelMember describes a member of a channel with presence.
type ChannelMember struct {
	BotID    string `json:"bot_id"`
	BotName  string `json:"bot_name"`
	BotSlug  string `json:"bot_slug"`
	Role     string `json:"role"`
	IsOnline bool   `json:"is_online"`
}

// MessageEntry describes a message from the message history.
type MessageEntry struct {
	Seq       uint64 `json:"seq"`
	MsgID     string `json:"msg_id"`
	SenderID  string `json:"sender_id"`
	Stream    string `json:"stream"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// --- Query methods ---

// ListMyLoops returns all loops this bot belongs to.
func (c *Client) ListMyLoops(ctx context.Context) ([]LoopMembership, error) {
	body, err := c.apiRequest(ctx, http.MethodGet, "/bots/"+c.cfg.BotID+"/loops", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Loops []LoopMembership `json:"loops"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Loops, nil
}

// GetLoopInfo returns detailed info about a loop the bot belongs to.
func (c *Client) GetLoopInfo(ctx context.Context, loopID string) (*LoopDetail, error) {
	body, err := c.apiRequest(ctx, http.MethodGet, "/bots/"+c.cfg.BotID+"/loops/"+loopID, nil)
	if err != nil {
		return nil, err
	}
	var detail LoopDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// ListLoopMembers returns all members of a loop the bot belongs to, with presence.
func (c *Client) ListLoopMembers(ctx context.Context, loopID string) ([]LoopMember, error) {
	body, err := c.apiRequest(ctx, http.MethodGet, "/bots/"+c.cfg.BotID+"/loops/"+loopID+"/members", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Members []LoopMember `json:"members"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Members, nil
}

// ListChannelMembers returns all members of a channel the bot belongs to, with presence.
func (c *Client) ListChannelMembers(ctx context.Context, channelID string) ([]ChannelMember, error) {
	body, err := c.apiRequest(ctx, http.MethodGet, "/bots/"+c.cfg.BotID+"/channels/"+channelID+"/members", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Members []ChannelMember `json:"members"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Members, nil
}

// ListChannelMessages returns recent messages from a channel the bot belongs to.
// Limit defaults to 50 on the server, max 200.
func (c *Client) ListChannelMessages(ctx context.Context, channelID string, limit int) ([]MessageEntry, error) {
	path := "/bots/" + c.cfg.BotID + "/channels/" + channelID + "/messages"
	if limit > 0 {
		path += "?limit=" + url.QueryEscape(fmt.Sprintf("%d", limit))
	}
	body, err := c.apiRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Messages []MessageEntry `json:"messages"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// --- HTTP helpers ---

func (c *Client) apiRequest(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("api %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func resolveAPIBase(cfg Config) string {
	if cfg.APIEndpoint != "" {
		return strings.TrimRight(cfg.APIEndpoint, "/") + "/api/v1"
	}
	// Derive from WebSocket endpoint: ws→http, wss→https, swap port to 8888
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return "http://localhost:8888/api/v1"
	}
	scheme := "http"
	if u.Scheme == "wss" {
		scheme = "https"
	}
	host := u.Hostname()
	return scheme + "://" + host + ":8888/api/v1"
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
	for {
		select {
		case data := <-c.sendCh:
			if err := wsutil.WriteClientBinary(c.conn, data); err != nil {
				slog.Warn("write error", "error", err)
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
