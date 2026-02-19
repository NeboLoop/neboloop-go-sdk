package frame

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	gen := NewULIDGen()
	msgID := gen.Next()

	var convID [16]byte
	rand.Read(convID[:])

	payload := []byte(`{"type":"test","hello":"world"}`)

	h := Header{
		Type:           TypeSendMessage,
		MsgID:          msgID,
		ConversationID: convID,
		Seq:            42,
	}

	encoded, err := Encode(h, payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if len(encoded) != HeaderSize+len(payload) {
		t.Fatalf("encoded length: got %d, want %d", len(encoded), HeaderSize+len(payload))
	}

	hDec, pDec, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if hDec.Version != ProtoVersion {
		t.Errorf("version: got %d, want %d", hDec.Version, ProtoVersion)
	}
	if hDec.Type != TypeSendMessage {
		t.Errorf("type: got %d, want %d", hDec.Type, TypeSendMessage)
	}
	if hDec.Seq != 42 {
		t.Errorf("seq: got %d, want 42", hDec.Seq)
	}
	if hDec.MsgID != msgID {
		t.Error("msg_id mismatch")
	}
	if hDec.ConversationID != convID {
		t.Error("conversation_id mismatch")
	}
	if !bytes.Equal(pDec, payload) {
		t.Error("payload mismatch")
	}
}

func TestRoundTripAllTypes(t *testing.T) {
	types := []uint8{
		TypeConnect, TypeAuthOK, TypeAuthFail,
		TypeJoinConversation, TypeLeaveConversation,
		TypeSendMessage, TypeMessageDelivery, TypeAck,
		TypePresence, TypeTyping, TypeSlowDown, TypeReplay, TypeClose,
	}

	for _, ft := range types {
		h := Header{Type: ft, Seq: 1}
		encoded, err := Encode(h, []byte("x"))
		if err != nil {
			t.Fatalf("encode type %d: %v", ft, err)
		}
		hDec, _, err := Decode(encoded)
		if err != nil {
			t.Fatalf("decode type %d: %v", ft, err)
		}
		if hDec.Type != ft {
			t.Errorf("type mismatch: got %d, want %d", hDec.Type, ft)
		}
	}
}

func TestOversizedPayload(t *testing.T) {
	big := make([]byte, MaxPayloadLen+1)
	_, err := Encode(Header{Type: TypeSendMessage}, big)
	if err != ErrPayloadTooLarge {
		t.Errorf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestVersionMismatch(t *testing.T) {
	data := make([]byte, HeaderSize)
	data[0] = 99 // bad version
	_, _, err := Decode(data)
	if err == nil {
		t.Error("expected error for bad version")
	}
}

func TestShortRead(t *testing.T) {
	_, _, err := Decode([]byte{1, 2, 3})
	if err != ErrShortRead {
		t.Errorf("expected ErrShortRead, got %v", err)
	}
}

func TestReadFrame(t *testing.T) {
	gen := NewULIDGen()
	h := Header{
		Type:  TypeMessageDelivery,
		MsgID: gen.Next(),
		Seq:   7,
	}
	payload := []byte("hello from ReadFrame")

	encoded, err := Encode(h, payload)
	if err != nil {
		t.Fatal(err)
	}

	hDec, pDec, err := ReadFrame(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if hDec.Type != TypeMessageDelivery {
		t.Errorf("type: got %d, want %d", hDec.Type, TypeMessageDelivery)
	}
	if !bytes.Equal(pDec, payload) {
		t.Error("payload mismatch")
	}
}

func TestFlags(t *testing.T) {
	h := Header{Flags: FlagCompressed | FlagEphemeral}
	if !h.IsCompressed() {
		t.Error("expected compressed")
	}
	if h.IsEncrypted() {
		t.Error("did not expect encrypted")
	}
	if !h.IsEphemeral() {
		t.Error("expected ephemeral")
	}
}

func TestCompression(t *testing.T) {
	// Small payload: should not compress
	small := []byte("hi")
	result, compressed := Compress(small)
	if compressed {
		t.Error("small payload should not compress")
	}
	if !bytes.Equal(result, small) {
		t.Error("small payload should be unchanged")
	}

	// Large payload: should compress (repeating data compresses well)
	large := bytes.Repeat([]byte("neboloop comms protocol test data "), 100)
	result, compressed = Compress(large)
	if !compressed {
		t.Error("large repeating payload should compress")
	}
	if len(result) >= len(large) {
		t.Errorf("compressed (%d) should be smaller than original (%d)", len(result), len(large))
	}

	// Decompress and verify
	decompressed, err := Decompress(result)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(decompressed, large) {
		t.Error("decompressed data doesn't match original")
	}
}

func TestULIDMonotonic(t *testing.T) {
	gen := NewULIDGen()
	prev := gen.Next()
	for i := 0; i < 1000; i++ {
		next := gen.Next()
		if ULIDToUint64(next) < ULIDToUint64(prev) {
			t.Fatalf("ULID not monotonic at iteration %d", i)
		}
		prev = next
	}
}

func TestULIDTimestamp(t *testing.T) {
	gen := NewULIDGen()
	before := time.Now()
	id := gen.Next()
	after := time.Now()

	ts := Timestamp(id)
	if ts.Before(before.Truncate(time.Millisecond)) || ts.After(after.Add(time.Millisecond)) {
		t.Errorf("timestamp %v not between %v and %v", ts, before, after)
	}
}

func TestDedupWindow(t *testing.T) {
	gen := NewULIDGen()
	d := NewDedupWindow()

	id1 := gen.Next()
	id2 := gen.Next()

	if d.IsDuplicate(id1) {
		t.Error("first id should not be duplicate")
	}
	if !d.IsDuplicate(id1) {
		t.Error("second check of same id should be duplicate")
	}
	if d.IsDuplicate(id2) {
		t.Error("different id should not be duplicate")
	}
	if d.Len() != 2 {
		t.Errorf("expected len 2, got %d", d.Len())
	}
}

func TestDedupWindowEviction(t *testing.T) {
	d := NewDedupWindow()
	gen := NewULIDGen()

	// Fill beyond capacity
	for i := 0; i < dedupWindowSize+100; i++ {
		d.IsDuplicate(gen.Next())
	}

	if d.Len() > dedupWindowSize {
		t.Errorf("window should not exceed %d, got %d", dedupWindowSize, d.Len())
	}
}
