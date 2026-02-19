// Package frame implements the 47-byte binary header codec for the NeboLoop
// comms protocol. See proto/comms/v1/comms.proto for payload definitions.
//
// Header layout (47 bytes, big-endian):
//
//	[0]     proto_version   uint8
//	[1]     frame_type      uint8
//	[2]     flags           uint8  (bit0=compressed, bit1=encrypted, bit2=ephemeral)
//	[3-6]   payload_len     uint32
//	[7-22]  msg_id          16 bytes (ULID)
//	[23-38] conversation_id 16 bytes (UUID)
//	[39-46] seq             uint64
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	HeaderSize    = 47
	ProtoVersion  = 1
	MaxPayloadLen = 32 * 1024 // 32 KB hard limit
)

// Frame types. Must fit in uint8.
const (
	TypeConnect           uint8 = 1
	TypeAuthOK            uint8 = 2
	TypeAuthFail          uint8 = 3
	TypeJoinConversation  uint8 = 4
	TypeLeaveConversation uint8 = 5
	TypeSendMessage       uint8 = 6
	TypeMessageDelivery   uint8 = 7
	TypeAck               uint8 = 8
	TypePresence          uint8 = 9
	TypeTyping            uint8 = 10
	TypeSlowDown          uint8 = 11
	TypeReplay            uint8 = 12
	TypeClose             uint8 = 13
)

// Flag bits.
const (
	FlagCompressed uint8 = 1 << 0
	FlagEncrypted  uint8 = 1 << 1
	FlagEphemeral  uint8 = 1 << 2
)

var (
	ErrBadVersion   = errors.New("frame: unsupported protocol version")
	ErrPayloadTooLarge = errors.New("frame: payload exceeds maximum size")
	ErrShortRead    = errors.New("frame: short read")
)

// Header is the fixed 47-byte header preceding every frame.
type Header struct {
	Version        uint8
	Type           uint8
	Flags          uint8
	PayloadLen     uint32
	MsgID          [16]byte // ULID
	ConversationID [16]byte // UUID
	Seq            uint64
}

// pool for header buffers to avoid allocations on hot path.
var headerPool = sync.Pool{
	New: func() any {
		b := make([]byte, HeaderSize)
		return &b
	},
}

// Encode serialises a header and payload into a single byte slice.
func Encode(h Header, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayloadLen {
		return nil, ErrPayloadTooLarge
	}
	h.PayloadLen = uint32(len(payload))
	h.Version = ProtoVersion

	out := make([]byte, HeaderSize+len(payload))
	out[0] = h.Version
	out[1] = h.Type
	out[2] = h.Flags
	binary.BigEndian.PutUint32(out[3:7], h.PayloadLen)
	copy(out[7:23], h.MsgID[:])
	copy(out[23:39], h.ConversationID[:])
	binary.BigEndian.PutUint64(out[39:47], h.Seq)
	copy(out[HeaderSize:], payload)
	return out, nil
}

// Decode parses a byte slice into a header and payload.
func Decode(data []byte) (Header, []byte, error) {
	if len(data) < HeaderSize {
		return Header{}, nil, ErrShortRead
	}
	var h Header
	h.Version = data[0]
	if h.Version != ProtoVersion {
		return Header{}, nil, fmt.Errorf("%w: got %d, want %d", ErrBadVersion, h.Version, ProtoVersion)
	}
	h.Type = data[1]
	h.Flags = data[2]
	h.PayloadLen = binary.BigEndian.Uint32(data[3:7])
	copy(h.MsgID[:], data[7:23])
	copy(h.ConversationID[:], data[23:39])
	h.Seq = binary.BigEndian.Uint64(data[39:47])

	if h.PayloadLen > MaxPayloadLen {
		return Header{}, nil, ErrPayloadTooLarge
	}

	end := HeaderSize + int(h.PayloadLen)
	if len(data) < end {
		return Header{}, nil, ErrShortRead
	}

	return h, data[HeaderSize:end], nil
}

// ReadFrame reads exactly one frame from r.
func ReadFrame(r io.Reader) (Header, []byte, error) {
	bp := headerPool.Get().(*[]byte)
	defer headerPool.Put(bp)
	hdr := *bp

	if _, err := io.ReadFull(r, hdr); err != nil {
		return Header{}, nil, err
	}

	var h Header
	h.Version = hdr[0]
	if h.Version != ProtoVersion {
		return Header{}, nil, fmt.Errorf("%w: got %d, want %d", ErrBadVersion, h.Version, ProtoVersion)
	}
	h.Type = hdr[1]
	h.Flags = hdr[2]
	h.PayloadLen = binary.BigEndian.Uint32(hdr[3:7])
	copy(h.MsgID[:], hdr[7:23])
	copy(h.ConversationID[:], hdr[23:39])
	h.Seq = binary.BigEndian.Uint64(hdr[39:47])

	if h.PayloadLen > MaxPayloadLen {
		return Header{}, nil, ErrPayloadTooLarge
	}

	payload := make([]byte, h.PayloadLen)
	if h.PayloadLen > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Header{}, nil, err
		}
	}

	return h, payload, nil
}

// IsCompressed returns true if the compressed flag is set.
func (h Header) IsCompressed() bool { return h.Flags&FlagCompressed != 0 }

// IsEncrypted returns true if the encrypted flag is set.
func (h Header) IsEncrypted() bool { return h.Flags&FlagEncrypted != 0 }

// IsEphemeral returns true if the ephemeral flag is set.
func (h Header) IsEphemeral() bool { return h.Flags&FlagEphemeral != 0 }
