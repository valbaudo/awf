package state

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
)

// Event is the on-disk record per phase-1 design §D. The JSON shape is the wire format; the
// frame (encodeFrame/decodeFrame) is the bytes that land on disk.
//
// Field semantics:
//   - Seq, Epoch, TS: assigned by the Log on Append (caller leaves them zero-valued).
//   - Path: opaque attribution — the engine's addressing path (Phase 2's engine/path package
//     produces it). Slice 1.5 never parses it.
//   - Type: opaque event-kind name. Phase 2's engine defines the vocabulary
//     (run.started, node.completed, branch.taken, …); slice 1.5 carries arbitrary strings.
//   - PayloadRef, Data: optional. PayloadRef is a content-addressed pointer into Blobs (large
//     payloads live there); Data is inline structured payload for small events. Both omitempty.
type Event struct {
	Seq        uint64          `json:"seq"`
	Epoch      uint32          `json:"epoch"`
	TS         time.Time       `json:"ts"`
	Path       string          `json:"path"`
	Type       string          `json:"type"`
	PayloadRef string          `json:"payload_ref,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// frameHeaderSize is the fixed 8-byte header (4 bytes length LE + 4 bytes CRC32C LE). Every
// record's header is 8-byte-aligned because record size is padded up to a multiple of 8,
// so the length field can't half-write across an unaligned boundary.
const frameHeaderSize = 8

// frameAlignment is the record-size alignment. Pad zero bytes after the payload to land here.
const frameAlignment = 8

// crcTable is the CRC32C (Castagnoli) table. Hardware-accelerated on SSE4.2 / ARM CRC32.
// Matches etcd's WAL CRC. Switching polynomials invalidates every existing log; the
// TestCRCTableIsCastagnoli test locks this with the standard "123456789" test vector.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Internal sentinels — surfaced via errors.Is in tests; the public Log treats short/CRC as
// torn-tail and truncates silently.
var (
	errShortFrame  = errors.New("state: short frame")
	errCRCMismatch = errors.New("state: frame CRC mismatch")
)

// encodeFrame wraps a payload in the design-spec frame:
//
//	[u32 payloadLen LE][u32 CRC32C(payload) LE][payload bytes][zero-pad to 8-byte multiple]
//
// Returns the encoded bytes. Total length is always a multiple of 8.
func encodeFrame(payload []byte) []byte {
	payloadLen := uint32(len(payload))
	crc := crc32.Checksum(payload, crcTable)

	total := frameHeaderSize + len(payload)
	pad := 0
	if rem := total % frameAlignment; rem != 0 {
		pad = frameAlignment - rem
	}
	buf := make([]byte, total+pad)
	binary.LittleEndian.PutUint32(buf[0:4], payloadLen)
	binary.LittleEndian.PutUint32(buf[4:8], crc)
	copy(buf[8:8+len(payload)], payload)
	// pad bytes already zero from make.
	return buf
}

// decodeFrame reads one frame from buf[0:]. Returns (payload, consumed-bytes, err) where err
// is errShortFrame (buf too small for header / payload) or errCRCMismatch (CRC didn't match).
// On error, `consumed` is unspecified — the caller (Fold) treats it as torn-tail and stops.
func decodeFrame(buf []byte) ([]byte, int, error) {
	if len(buf) < frameHeaderSize {
		return nil, 0, fmt.Errorf("%w: have %d bytes, need at least %d for header",
			errShortFrame, len(buf), frameHeaderSize)
	}
	payloadLen := binary.LittleEndian.Uint32(buf[0:4])
	wantCRC := binary.LittleEndian.Uint32(buf[4:8])
	end := frameHeaderSize + int(payloadLen)
	if len(buf) < end {
		return nil, 0, fmt.Errorf("%w: have %d bytes, need %d for payload of len %d",
			errShortFrame, len(buf), end, payloadLen)
	}
	payload := buf[frameHeaderSize:end]
	gotCRC := crc32.Checksum(payload, crcTable)
	if gotCRC != wantCRC {
		return nil, 0, fmt.Errorf("%w: header CRC %#x, computed %#x", errCRCMismatch, wantCRC, gotCRC)
	}
	pad := 0
	if rem := end % frameAlignment; rem != 0 {
		pad = frameAlignment - rem
	}
	consumed := end + pad
	if len(buf) < consumed {
		// Partial pad — treat as torn even though CRC was OK. (Spec: torn tail stops the fold;
		// a torn pad without the next record header is indistinguishable from a torn header.)
		return nil, 0, fmt.Errorf("%w: have %d bytes, need %d for pad", errShortFrame, len(buf), consumed)
	}
	return payload, consumed, nil
}

// marshalEvent encodes an Event to wire JSON. Wrapped so future codec swaps (CBOR, protobuf
// per design §D "codec seam") are a one-function change.
func marshalEvent(e Event) ([]byte, error) { //nolint:unused // called by Task 2's Log.Append
	return json.Marshal(e)
}

// unmarshalEvent is marshalEvent's inverse.
func unmarshalEvent(b []byte) (Event, error) { //nolint:unused // called by Task 2's Log.Fold
	var e Event
	if err := json.Unmarshal(b, &e); err != nil {
		return Event{}, err
	}
	return e, nil
}
