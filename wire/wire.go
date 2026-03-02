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
type JoinResultPayload struct {
	ConversationID string `json:"conversationId"`
	BotID          string `json:"botId,omitempty"`
	Stream         string `json:"stream,omitempty"`
	ChannelID      string `json:"channelId,omitempty"`
	ChannelName    string `json:"channelName,omitempty"`
	LoopID         string `json:"loopId,omitempty"`
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
