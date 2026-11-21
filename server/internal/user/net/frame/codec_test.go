package frame

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

// TestEncodeDecodeRoundTrip 编解码往返。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	env := &chatpb.Envelope{
		Id: 9, Kind: 101, RequestId: "trace-1", SentAt: 1700000000000,
		Target:  &chatpb.Envelope_ConvId{ConvId: 88},
		Payload: []byte("payload"),
	}
	b, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Id != env.Id || got.Kind != env.Kind || got.RequestId != env.RequestId || got.GetConvId() != 88 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if !bytes.Equal(got.Payload, env.Payload) {
		t.Fatal("payload mismatch")
	}
}

// TestTooLargeRejected 超长帧必须拒绝（64KB 上限）。
func TestTooLargeRejected(t *testing.T) {
	env := &chatpb.Envelope{Kind: 101, Payload: bytes.Repeat([]byte("a"), MaxFrameBytes)}
	if _, err := Encode(env); err == nil {
		t.Fatal("oversize frame accepted")
	}
	// Decode 侧同样拒绝。
	if _, err := Decode(make([]byte, MaxFrameBytes+1)); err == nil {
		t.Fatal("oversize decode accepted")
	}
}

// TestUnknownFieldRejected 未知字段必须报错。
func TestUnknownFieldRejected(t *testing.T) {
	env := &chatpb.Envelope{Kind: 101}
	b, _ := env.Marshal()
	// 追加未知字段 tag=99 varint
	b = append(b, 0xF8, 0x06, 1)
	if _, err := Decode(b); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}

// TestBadDataRejected 坏数据/空帧必须报错。
func TestBadDataRejected(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("nil frame accepted")
	}
	if _, err := Decode([]byte{0xff}); err == nil {
		t.Fatal("truncated frame accepted")
	}
	if _, err := Decode([]byte{0x18, 0x00}); err == nil {
		t.Fatal("kind=0 frame accepted")
	}
}

// TestInternalFrameWrapUnwrap 内部帧 Wrap/Unwrap：src_layer=user、trace_id、conn_id。
func TestInternalFrameWrapUnwrap(t *testing.T) {
	env := &chatpb.Envelope{Kind: 101, Payload: []byte("x")}
	intf := Wrap(env, "trace-9", 42)
	if intf.SrcLayer != SrcLayerUser || intf.TraceId != "trace-9" || intf.ConnId != 42 {
		t.Fatalf("wrap = %+v", intf)
	}
	b, err := MarshalInternal(intf)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalInternal(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unwrap(back)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != 101 || !bytes.Equal(got.Payload, []byte("x")) {
		t.Fatalf("unwrap = %+v", got)
	}
}
