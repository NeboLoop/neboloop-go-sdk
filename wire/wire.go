// Package wire defines the JSON payload types for the NeboLoop comms binary
// protocol. Both the gateway and Go SDK import these — single source of truth.
package wire

import "encoding/json"

// ConnectPayload is the payload of a CONNECT frame (client -> server).
type ConnectPayload struct {
	BotID string `json:"botId,omitempty"`
	Token string `json:"token,omitempty"`
}

// AuthResultPayload is the payload of AUTH_OK / AUTH_FAIL (server -> client).
type AuthResultPayload struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	BotID  string `json:"botId,omitempty"`
	Plan   string `json:"plan,omitempty"`
	Token  string `json:"token,omitempty"` // rotated bot JWT (reconnect with this)
}

// SendPayload is the payload of a SEND_MESSAGE frame (client -> server).
type SendPayload struct {
	ConversationID string          `json:"conversationId"`
	Stream         string          `json:"stream"`
	Content        json.RawMessage `json:"content"`
}

// DeliveryPayload is what gets stored in message_log and fanned out to
// subscribers. The frame header carries msg_id, conversation_id, seq.
type DeliveryPayload struct {
	SenderID string          `json:"senderId"`
	Stream   string          `json:"stream"`
	Content  json.RawMessage `json:"content"`
}

// JoinPayload is the payload of a JOIN_CONVERSATION frame (client -> server).
// Either ConversationID OR (BotID + Stream) OR ChannelID must be set.
type JoinPayload struct {
	ConversationID string `json:"conversationId,omitempty"`
	BotID          string `json:"botId,omitempty"`
	Stream         string `json:"stream,omitempty"`
	ChannelID      string `json:"channelId,omitempty"`
	LastAckedSeq   uint64 `json:"lastAckedSeq,omitempty"`
}

// JoinResultPayload is sent back after a successful join (server -> client).
// Presence of AgentID distinguishes agent space joins from channel/stream joins.
type JoinResultPayload struct {
	ConversationID string `json:"conversationId"`
	BotID          string `json:"botId,omitempty"`
	Stream         string `json:"stream,omitempty"`
	ChannelID      string `json:"channelId,omitempty"`
	ChannelName    string `json:"channelName,omitempty"`
	LoopID         string `json:"loopId,omitempty"`
	AgentID        string `json:"agentId,omitempty"`
	AgentSlug      string `json:"agentSlug,omitempty"`
	ConvType       string `json:"type,omitempty"` // "agent_space", "loop_channel", "bot_stream"
	// DM joins — the peer on the other side of the conversation. These are
	// part of the gateway's join JSON (client.go's DM tracking reads them);
	// they were referenced but missing from this struct.
	PeerID   string `json:"peerId,omitempty"`
	PeerType string `json:"peerType,omitempty"` // "bot" | "person"
	// Agent-space chats: each desktop chat of an agent maps to its own loop
	// conversation. ChatID is the desktop chat identifier; ChatTitle is the
	// human title shown in chat lists. Empty for non-agent-space joins.
	ChatID    string `json:"chatId,omitempty"`
	ChatTitle string `json:"chatTitle,omitempty"`
}

// LeavePayload is the payload of a LEAVE_CONVERSATION frame (client -> server).
type LeavePayload struct {
	ConversationID string `json:"conversationId"`
}

// AckPayload is the payload of an ACK frame (client -> server).
type AckPayload struct {
	ConversationID string `json:"conversationId"`
	AckedSeq       uint64 `json:"ackedSeq"`
}

// ReplayPayload is the payload of a REPLAY frame (server -> client).
type ReplayPayload struct {
	ConversationID string `json:"conversationId"`
	FromSeq        uint64 `json:"fromSeq"`
	ToSeq          uint64 `json:"toSeq"`
	MessageCount   uint32 `json:"messageCount"`
}
