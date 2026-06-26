package kiro

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// buildTestFrame constructs a valid AWS Event Stream frame for testing.
func buildTestFrame(headers map[string]string, payload []byte) []byte {
	// Encode headers
	var headerBuf bytes.Buffer
	for name, value := range headers {
		headerBuf.WriteByte(byte(len(name)))
		headerBuf.WriteString(name)
		headerBuf.WriteByte(7) // string type
		binary.Write(&headerBuf, binary.BigEndian, uint16(len(value)))
		headerBuf.WriteString(value)
	}
	headerData := headerBuf.Bytes()

	totalLen := uint32(preludeSize + len(headerData) + len(payload) + 4) // +4 for msg CRC
	headerLen := uint32(len(headerData))

	// Build prelude
	var prelude bytes.Buffer
	binary.Write(&prelude, binary.BigEndian, totalLen)
	binary.Write(&prelude, binary.BigEndian, headerLen)
	preludeCRC := crc32.Checksum(prelude.Bytes(), crc32Table)
	binary.Write(&prelude, binary.BigEndian, preludeCRC)

	// Build full message (without msg CRC)
	var msg bytes.Buffer
	msg.Write(prelude.Bytes())
	msg.Write(headerData)
	msg.Write(payload)

	// Compute and append message CRC
	msgCRC := crc32.Checksum(msg.Bytes(), crc32Table)
	binary.Write(&msg, binary.BigEndian, msgCRC)

	return msg.Bytes()
}

func TestParseFrame_Simple(t *testing.T) {
	payload := []byte(`{"content":"hello"}`)
	headers := map[string]string{
		":message-type": "event",
		":event-type":   "assistantResponseEvent",
		":content-type": "application/json",
	}

	data := buildTestFrame(headers, payload)
	frame, err := ParseFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseFrame failed: %v", err)
	}

	if frame.MessageType() != "event" {
		t.Errorf("expected message-type=event, got %q", frame.MessageType())
	}
	if frame.EventType() != "assistantResponseEvent" {
		t.Errorf("expected event-type=assistantResponseEvent, got %q", frame.EventType())
	}
	if string(frame.Payload) != `{"content":"hello"}` {
		t.Errorf("unexpected payload: %s", string(frame.Payload))
	}
}

func TestParseFrame_EmptyPayload(t *testing.T) {
	headers := map[string]string{":message-type": "event"}
	data := buildTestFrame(headers, nil)
	frame, err := ParseFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseFrame failed: %v", err)
	}
	if len(frame.Payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(frame.Payload))
	}
}

func TestParseFrame_InvalidPreludeCRC(t *testing.T) {
	headers := map[string]string{":message-type": "event"}
	data := buildTestFrame(headers, []byte("test"))
	// Corrupt prelude CRC (byte at offset 8)
	data[8] ^= 0xFF
	_, err := ParseFrame(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected CRC error, got nil")
	}
}

func TestParseFrame_InvalidMessageCRC(t *testing.T) {
	headers := map[string]string{":message-type": "event"}
	data := buildTestFrame(headers, []byte("test"))
	// Corrupt message CRC (last 4 bytes)
	data[len(data)-1] ^= 0xFF
	_, err := ParseFrame(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected CRC error, got nil")
	}
}

func TestParseFrame_MultipleFrames(t *testing.T) {
	payload1 := []byte(`{"content":"first"}`)
	payload2 := []byte(`{"content":"second"}`)
	h := map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}

	var buf bytes.Buffer
	buf.Write(buildTestFrame(h, payload1))
	buf.Write(buildTestFrame(h, payload2))

	reader := bytes.NewReader(buf.Bytes())

	f1, err := ParseFrame(reader)
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if string(f1.Payload) != `{"content":"first"}` {
		t.Errorf("frame 1 payload: %s", string(f1.Payload))
	}

	f2, err := ParseFrame(reader)
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if string(f2.Payload) != `{"content":"second"}` {
		t.Errorf("frame 2 payload: %s", string(f2.Payload))
	}
}
