package frame

import (
	"github.com/klauspost/compress/zstd"
)

const compressionThreshold = 1024 // only compress payloads > 1KB

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	decoder, _ = zstd.NewReader(nil)
)

// Compress compresses payload with zstd if it exceeds the threshold.
// Returns (compressed data, true) if compression helped, or (original, false).
func Compress(payload []byte) ([]byte, bool) {
	if len(payload) <= compressionThreshold {
		return payload, false
	}

	compressed := encoder.EncodeAll(payload, make([]byte, 0, len(payload)))

	// Only use compressed if it's actually smaller
	if len(compressed) >= len(payload) {
		return payload, false
	}

	return compressed, true
}

// Decompress decompresses a zstd-compressed payload.
func Decompress(data []byte) ([]byte, error) {
	return decoder.DecodeAll(data, nil)
}
