package kiro

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// Prelude size: total_len(4) + header_len(4) + prelude_crc(4) = 12 bytes
const preludeSize = 12

// Minimum message size: prelude(12) + message_crc(4) = 16 bytes
const minMessageSize = 16

// Maximum message size: 16 MB
const maxMessageSize = 16 * 1024 * 1024

// crc32Table is the CRC-32C (Castagnoli) table used by AWS Event Stream.
var crc32Table = crc32.MakeTable(crc32.IEEE)

// ParseFrame reads a single AWS Event Stream frame from the reader.
// Returns the parsed Frame or an error.
func ParseFrame(r io.Reader) (*Frame, error) {
	// Read prelude (12 bytes)
	prelude := make([]byte, preludeSize)
	if _, err := io.ReadFull(r, prelude); err != nil {
		return nil, fmt.Errorf("read prelude: %w", err)
	}

	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headerLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	// Validate prelude CRC
	computedPreludeCRC := crc32.Checksum(prelude[0:8], crc32Table)
	if computedPreludeCRC != preludeCRC {
		return nil, fmt.Errorf("prelude CRC mismatch: expected %08x, got %08x", preludeCRC, computedPreludeCRC)
	}

	// Validate sizes
	if totalLen < uint32(minMessageSize) {
		return nil, fmt.Errorf("message too small: %d bytes", totalLen)
	}
	if totalLen > maxMessageSize {
		return nil, fmt.Errorf("message too large: %d bytes", totalLen)
	}
	if headerLen > totalLen-uint32(minMessageSize) {
		return nil, fmt.Errorf("header length %d exceeds message bounds", headerLen)
	}

	// Read remaining bytes (headers + payload + message_crc)
	remainingLen := int(totalLen) - preludeSize
	remaining := make([]byte, remainingLen)
	if _, err := io.ReadFull(r, remaining); err != nil {
		return nil, fmt.Errorf("read message body: %w", err)
	}

	// Validate message CRC (covers prelude + headers + payload, excludes last 4 bytes)
	msgCRCOffset := len(remaining) - 4
	msgCRC := binary.BigEndian.Uint32(remaining[msgCRCOffset:])

	// Compute CRC over prelude + everything except the message CRC itself
	h := crc32.New(crc32Table)
	h.Write(prelude)
	h.Write(remaining[:msgCRCOffset])
	computedMsgCRC := h.Sum32()

	if computedMsgCRC != msgCRC {
		return nil, fmt.Errorf("message CRC mismatch: expected %08x, got %08x", msgCRC, computedMsgCRC)
	}

	// Parse headers
	headerData := remaining[:headerLen]
	headers, err := parseHeaders(headerData)
	if err != nil {
		return nil, fmt.Errorf("parse headers: %w", err)
	}

	// Extract payload (between headers and message CRC)
	payload := remaining[headerLen:msgCRCOffset]

	return &Frame{
		Headers: headers,
		Payload: payload,
	}, nil
}

// parseHeaders decodes AWS Event Stream headers from raw bytes.
// Header format: name_len(1) + name + type(1) + value_len(2) + value
// We only handle type 7 (string) which is what Kiro uses.
func parseHeaders(data []byte) (map[string]string, error) {
	headers := make(map[string]string)
	offset := 0

	for offset < len(data) {
		// Name length (1 byte)
		if offset >= len(data) {
			return nil, fmt.Errorf("unexpected end of headers at offset %d", offset)
		}
		nameLen := int(data[offset])
		offset++

		// Name
		if offset+nameLen > len(data) {
			return nil, fmt.Errorf("header name overflows at offset %d", offset)
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen

		// Type (1 byte)
		if offset >= len(data) {
			return nil, fmt.Errorf("missing header type for %q", name)
		}
		headerType := data[offset]
		offset++

		// Value based on type
		switch headerType {
		case 7: // String type
			if offset+2 > len(data) {
				return nil, fmt.Errorf("missing string length for header %q", name)
			}
			valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(data) {
				return nil, fmt.Errorf("string value overflows for header %q", name)
			}
			headers[name] = string(data[offset : offset+valueLen])
			offset += valueLen
		case 0: // Bool true
			headers[name] = "true"
		case 1: // Bool false
			headers[name] = "false"
		case 2: // Byte
			if offset >= len(data) {
				return nil, fmt.Errorf("missing byte value for header %q", name)
			}
			headers[name] = fmt.Sprintf("%d", data[offset])
			offset++
		case 3: // Short (2 bytes)
			if offset+2 > len(data) {
				return nil, fmt.Errorf("missing short value for header %q", name)
			}
			headers[name] = fmt.Sprintf("%d", binary.BigEndian.Uint16(data[offset:offset+2]))
			offset += 2
		case 4: // Int (4 bytes)
			if offset+4 > len(data) {
				return nil, fmt.Errorf("missing int value for header %q", name)
			}
			headers[name] = fmt.Sprintf("%d", binary.BigEndian.Uint32(data[offset:offset+4]))
			offset += 4
		case 5: // Long (8 bytes)
			if offset+8 > len(data) {
				return nil, fmt.Errorf("missing long value for header %q", name)
			}
			headers[name] = fmt.Sprintf("%d", binary.BigEndian.Uint64(data[offset:offset+8]))
			offset += 8
		case 6: // Bytes
			if offset+2 > len(data) {
				return nil, fmt.Errorf("missing bytes length for header %q", name)
			}
			valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(data) {
				return nil, fmt.Errorf("bytes value overflows for header %q", name)
			}
			// Store as string for simplicity
			headers[name] = string(data[offset : offset+valueLen])
			offset += valueLen
		case 8: // Timestamp (8 bytes)
			if offset+8 > len(data) {
				return nil, fmt.Errorf("missing timestamp value for header %q", name)
			}
			headers[name] = fmt.Sprintf("%d", binary.BigEndian.Uint64(data[offset:offset+8]))
			offset += 8
		case 9: // UUID (16 bytes)
			if offset+16 > len(data) {
				return nil, fmt.Errorf("missing uuid value for header %q", name)
			}
			headers[name] = fmt.Sprintf("%x", data[offset:offset+16])
			offset += 16
		default:
			return nil, fmt.Errorf("unknown header type %d for header %q", headerType, name)
		}
	}

	return headers, nil
}
