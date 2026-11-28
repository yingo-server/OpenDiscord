package chatpb

import (
	"bytes"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	m := &Envelope{
		Id: 7, AckId: 3, Kind: 101, Version: 1, RequestId: "req-1",
		Flags: 2, SentAt: 1234567890, Target: &Envelope_UserId{UserId: 42},
		Payload: []byte("hello payload"),
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got Envelope
	if err := got.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if got.Id != 7 || got.Kind != 101 || got.RequestId != "req-1" || got.SentAt != 1234567890 {
		t.Fatalf("scalar mismatch: %+v", &got)
	}
	if got.GetUserId() != 42 {
		t.Fatalf("oneof user_id mismatch: %v", got.GetTarget())
	}
	if !bytes.Equal(got.Payload, m.Payload) {
		t.Fatal("payload mismatch")
	}
}

func TestInternalFrameRoundTrip(t *testing.T) {
	m := &InternalFrame{
		Envelope: &Envelope{Kind: 102, Target: &Envelope_ConvId{ConvId: 9}, Payload: []byte("x")},
		SrcLayer: "user", DstLayer: "data", SrcNode: "u1", DstNode: "d1",
		ConnId: 77, TraceId: "trace-1",
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got InternalFrame
	if err := got.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if got.SrcLayer != "user" || got.ConnId != 77 || got.TraceId != "trace-1" {
		t.Fatalf("route header mismatch: %+v", &got)
	}
	if got.Envelope == nil || got.Envelope.GetConvId() != 9 {
		t.Fatalf("nested envelope mismatch: %+v", got.Envelope)
	}
}

func TestUnknownFieldsSkipped(t *testing.T) {
	// tag 99 varint (unknown) + valid field 1
	raw := []byte{0xf8, 0x06, 1, 0x08, 7} // tag 99 varint(1) + field1 varint(7)
	var got Envelope
	if err := got.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if got.Id != 7 {
		t.Fatalf("id mismatch: %d", got.Id)
	}
}

func TestBadWireRejected(t *testing.T) {
	var got Envelope
	if err := got.Unmarshal([]byte{0xff}); err == nil {
		t.Fatal("expected error for truncated varint")
	}
}

// TestConvPayloadMembersOmittedLegacyDecoder 是 T-SV-6 的 protobuf 侧：
// 旧客户端（v1.1.x 的解码器，只认识 ConvPayload 的字段 1-6）收到带
// members_omitted(7)/member_count(8) 的 202 时，必须跳过未知字段、正常解析，
// 不得报错。这里手写"旧解码器"，只用 pb 原语，不复用新的 ConvPayload.Unmarshal。
func TestConvPayloadMembersOmittedLegacyDecoder(t *testing.T) {
	m := &ConvPayload{
		ConvId: 3, Title: "big-group", ConvType: 2, CreatedAt: 11, Version: 9,
		MembersOmitted: true, MemberCount: 101,
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	// 断言新字段确实在线上（tag 7 = 0x38 varint，tag 8 = 0x40 varint）。
	seen := map[int]bool{}
	for rest := b; len(rest) > 0; {
		tag, wt, r, ok := pbReadTag(rest)
		if !ok {
			t.Fatal("bad tag")
		}
		seen[tag] = true
		r2, ok := pbSkip(r, wt)
		if !ok {
			t.Fatal("bad wire")
		}
		rest = r2
	}
	if !seen[7] || !seen[8] {
		t.Fatalf("encoded ConvPayload missing fields 7/8: tags=%v", seen)
	}

	// 旧解码器：只消费 1-6，其余走 skip（proto3 的未知字段语义）。
	var (
		convID  uint64
		title   string
		version uint32
	)
	rest := b
	for len(rest) > 0 {
		tag, wt, r, ok := pbReadTag(rest)
		if !ok {
			t.Fatal("legacy decode: bad tag")
		}
		rest = r
		switch tag {
		case 1:
			convID, rest, ok = pbReadVarint(rest)
		case 2:
			var v []byte
			v, rest, ok = pbReadBytes(rest)
			title = string(v)
		case 6:
			var v uint64
			v, rest, ok = pbReadVarint(rest)
			version = uint32(v)
		default:
			rest, ok = pbSkip(rest, wt)
		}
		if !ok {
			t.Fatalf("legacy decode failed at tag %d (unknown fields must be skipped)", tag)
		}
	}
	if convID != 3 || title != "big-group" || version != 9 {
		t.Fatalf("legacy view = convId %d title %q version %d", convID, title, version)
	}
}
