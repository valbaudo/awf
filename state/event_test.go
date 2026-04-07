package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
	"time"
)

func TestEventJSONRoundTrip(t *testing.T) {
	// Lock the on-disk JSON shape per phase-1 design §D. Populated + omitted-optional forms
	// in one fixture so we know `omitempty` actually drops the absent keys.
	ts := time.Date(2026, 5, 24, 12, 34, 56, 0, time.UTC)
	in := Event{
		Seq:        7,
		Epoch:      2,
		TS:         ts,
		Path:       "graph[1].do[0]",
		Type:       "node.completed",
		PayloadRef: "awf-d1:sha256:" + strings.Repeat("a", 64),
		Data:       json.RawMessage(`{"outcome":"ok"}`),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`"seq":7`,
		`"epoch":2`,
		`"ts":"2026-05-24T12:34:56Z"`,
		`"path":"graph[1].do[0]"`,
		`"type":"node.completed"`,
		`"payload_ref":"awf-d1:sha256:`,
		`"data":{"outcome":"ok"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON %q missing %q", got, want)
		}
	}
	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Seq != in.Seq || out.Epoch != in.Epoch || !out.TS.Equal(in.TS) ||
		out.Path != in.Path || out.Type != in.Type || out.PayloadRef != in.PayloadRef ||
		string(out.Data) != string(in.Data) {
		t.Fatalf("roundtrip mismatch:\n got  %+v\n want %+v", out, in)
	}
}

func TestEventJSONOmitsAbsentOptionals(t *testing.T) {
	// PayloadRef="" and Data==nil must drop their keys entirely so a synthetic event without
	// any payload is byte-identical to one whose payload was explicitly omitted by the engine.
	e := Event{Seq: 1, Epoch: 0, TS: time.Unix(0, 0).UTC(), Path: "/", Type: "run.started"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `"payload_ref":`) {
		t.Errorf("expected omitempty to drop payload_ref; got %s", s)
	}
	if strings.Contains(s, `"data":`) {
		t.Errorf("expected omitempty to drop data; got %s", s)
	}
}

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	// Encode a known payload; decode it; assert bytes-equal and offset advance is what the
	// spec requires (8 + len rounded up to a multiple of 8).
	payload := []byte(`{"hello":"world"}`)
	frame := encodeFrame(payload)
	if len(frame)%8 != 0 {
		t.Errorf("encoded frame size %d is not a multiple of 8", len(frame))
	}
	wantSize := 8 + len(payload)
	if rem := wantSize % 8; rem != 0 {
		wantSize += 8 - rem
	}
	if len(frame) != wantSize {
		t.Errorf("frame size = %d, want %d", len(frame), wantSize)
	}
	got, n, err := decodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload roundtrip: got %q, want %q", got, payload)
	}
	if n != len(frame) {
		t.Errorf("decode advance = %d, want %d", n, len(frame))
	}
}

func TestEncodeFramePadsToEightByteBoundary(t *testing.T) {
	// Walk every payload length 0..15 to lock the pad math (rounds (8+len) up to next multiple of 8).
	for ln := 0; ln <= 15; ln++ {
		payload := bytes.Repeat([]byte{0xAB}, ln)
		frame := encodeFrame(payload)
		if len(frame)%8 != 0 {
			t.Errorf("len=%d → frame size %d not 8-aligned", ln, len(frame))
		}
		// header(8) + payload(ln) + pad in [0..7].
		expected := 8 + ln
		if rem := expected % 8; rem != 0 {
			expected += 8 - rem
		}
		if len(frame) != expected {
			t.Errorf("len=%d → frame size %d, want %d", ln, len(frame), expected)
		}
	}
}

func TestDecodeFrameDetectsCRCMismatch(t *testing.T) {
	// Mutate a CRC-protected byte; decode must surface ErrCRCMismatch (an internal sentinel —
	// the public Log surface treats this as torn-tail and truncates).
	payload := []byte(`{"corrupt":true}`)
	frame := encodeFrame(payload)
	// Flip a payload byte (offset 8 onward is the payload).
	frame[10] ^= 0xFF
	if _, _, err := decodeFrame(frame); !errors.Is(err, errCRCMismatch) {
		t.Errorf("decodeFrame on corrupted payload: err = %v, want errCRCMismatch", err)
	}
}

func TestDecodeFrameRejectsShortInput(t *testing.T) {
	// A buffer shorter than the 8-byte header is torn — decodeFrame must report short.
	if _, _, err := decodeFrame(make([]byte, 3)); !errors.Is(err, errShortFrame) {
		t.Errorf("decodeFrame on 3-byte input: err = %v, want errShortFrame", err)
	}
	// Header present but payload truncated.
	full := encodeFrame([]byte(`hello`))
	if _, _, err := decodeFrame(full[:9]); !errors.Is(err, errShortFrame) {
		t.Errorf("decodeFrame on truncated payload: err = %v, want errShortFrame", err)
	}
}

func TestCRCTableIsCastagnoli(t *testing.T) {
	// Lock the polynomial: this is the same CRC32C etcd uses; switching invalidates every
	// existing log. The check is implicit (we test the wire bytes), but an explicit lock
	// makes accidental polynomial swap a loud failure.
	if crcTable == nil {
		t.Fatal("crcTable is nil")
	}
	// Test vector: CRC32C("123456789") == 0xE3069283 (well-known).
	got := crc32.Checksum([]byte("123456789"), crcTable)
	if got != 0xE3069283 {
		t.Errorf("crc32c(\"123456789\") = %#x, want 0xE3069283 (Castagnoli)", got)
	}
	// And a sanity check on the framed CRC: encode → manually re-compute → must match header[4:8].
	payload := []byte("abc")
	frame := encodeFrame(payload)
	want := crc32.Checksum(payload, crcTable)
	gotCRC := binaryLittleEndianU32(frame[4:8])
	if gotCRC != want {
		t.Errorf("header CRC = %#x, want %#x", gotCRC, want)
	}
	// Hex of the encoded header has no bearing on correctness; it just helps debugging.
	_ = hex.EncodeToString(frame[:8])
}

func TestMarshalUnmarshalEventRoundTrip(t *testing.T) {
	// Lock the codec-seam contract: marshalEvent → unmarshalEvent must round-trip every field.
	// TestEventJSONRoundTrip exercises the JSON shape via direct encoding/json calls; this test
	// exercises it via the package-private wrappers that FileLog.Append and FileLog.Fold call.
	// If a future implementer swaps JSON for CBOR/protobuf in marshalEvent but forgets to update
	// unmarshalEvent, the direct-encoding test would still pass — this one would not.
	in := Event{
		Seq:        42,
		Epoch:      3,
		TS:         time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC),
		Path:       "graph[0]",
		Type:       "node.completed",
		PayloadRef: "awf-d1:sha256:" + strings.Repeat("b", 64),
		Data:       json.RawMessage(`{"k":"v"}`),
	}
	b, err := marshalEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := unmarshalEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != in.Seq || out.Epoch != in.Epoch || !out.TS.Equal(in.TS) ||
		out.Path != in.Path || out.Type != in.Type || out.PayloadRef != in.PayloadRef ||
		string(out.Data) != string(in.Data) {
		t.Fatalf("roundtrip mismatch:\n got  %+v\n want %+v", out, in)
	}
}

// binaryLittleEndianU32 is a test-local mirror so the test doesn't depend on encoding/binary
// (the production decoder uses encoding/binary; the test cross-checks via a tiny ad-hoc
// little-endian read so a wrong byte order in the production code can't pass).
func binaryLittleEndianU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
