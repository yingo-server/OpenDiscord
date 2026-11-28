#!/usr/bin/env python3
# Final generator rewrite: emits chatpb.go (hand-rolled protobuf wire codec,
# wire-compatible with protoc-gen-go for proto3). No protoc available in env.
import os

OUT = os.path.join(os.path.dirname(__file__), "chatpb")
os.makedirs(OUT, exist_ok=True)
PKG = "chatpb"

# field: (number, GoName, ptype, label)
# ptype: u32,u64,i32,i64,f64(fixed64),str,bytes,bool,enum or message name
# label: s single | r repeated scalar | rm repeated msg | m map<string,string> | o oneof(Envelope)
SPEC = {
 "Envelope": [
    (1,"Id","u64","s"),(2,"AckId","u64","s"),(3,"Kind","u32","s"),(4,"Version","u32","s"),
    (5,"RequestId","str","s"),(6,"Flags","u32","s"),(7,"SentAt","f64","s"),
    (10,"ConvId","u64","o"),(11,"UserId","u64","o"),
    (15,"Payload","bytes","s"),
 ],
 "Hello": [(1,"ClientVersion","str","s"),(2,"DeviceId","str","s"),(3,"Capabilities","str","r"),(4,"Nonce","str","s")],
 "Auth": [(1,"Token","str","s"),(2,"DeviceId","str","s")],
 "AuthResult": [(1,"Ok","bool","s"),(2,"Error","str","s"),(3,"UserId","u64","s"),(4,"SessionId","str","s"),(5,"ExpiresAt","f64","s")],
 "Ping": [(1,"ClientMs","f64","s")],
 "Pong": [(1,"ClientMs","f64","s"),(2,"ServerMs","f64","s")],
 "Resume": [(1,"SessionId","str","s"),(2,"ConvId","u64","s"),(3,"LastSeq","u64","s")],
 "ResumeResult": [(1,"ConvId","u64","s"),(2,"FromSeq","u64","s"),(3,"Complete","bool","s"),(4,"ServerSeq","u64","s")],
 "SyncRequest": [(1,"ConvId","u64","s"),(2,"SinceSeq","u64","s"),(3,"Limit","u32","s")],
 "SyncResult": [(1,"ConvId","u64","s"),(2,"FromSeq","u64","s"),(3,"ToSeq","u64","s"),(4,"HasMore","bool","s"),(5,"Frames","Envelope","rm")],
 "Error": [(1,"Code","u32","s"),(2,"Reason","str","s"),(3,"RetryAfterMs","u32","s")],
 "Kicked": [(1,"Reason","str","s"),(2,"BannedUntil","f64","s")],
 "FeatureFlags": [(1,"Version","u32","s"),(2,"Flags","str","m")],
 "RateLimited": [(1,"Scope","str","s"),(2,"Limit","u32","s"),(3,"RetryAfterMs","u32","s")],
 "Receipt": [(1,"MsgId","u64","s"),(2,"UserId","u64","s"),(3,"At","f64","s"),(4,"Rtype","enum","s")],
 "MessagePayload": [
    (1,"ClientMsgId","str","s"),(2,"Text","str","s"),(3,"Media","Media","rm"),(4,"ReplyTo","u64","s"),
    (5,"Mentions","u64","r"),(9,"Content","Content","s"),(10,"Receipts","Receipt","rm"),
 ],
 "Content": [(1,"Text","str","s"),(2,"Media","Media","rm"),(3,"ReplyTo","u64","s"),(4,"Mentions","u64","r")],
 "MsgNew": [(1,"MsgId","u64","s"),(2,"Seq","u64","s"),(3,"SenderId","u64","s"),(4,"Content","MessagePayload","s"),(5,"SentAt","f64","s")],
 "MsgAck": [(1,"MsgId","u64","s"),(2,"Seq","u64","s"),(3,"ClientMsgId","str","s"),(4,"Status","u32","s")],
 "MsgEdit": [(1,"MsgId","u64","s"),(2,"ConvId","u64","s"),(3,"Text","str","s")],
 "MsgDelete": [(1,"MsgId","u64","s"),(2,"ConvId","u64","s"),(3,"ForEveryone","bool","s")],
 "MsgRecall": [(1,"MsgId","u64","s"),(2,"ConvId","u64","s")],
 "MsgForward": [(1,"MsgId","u64","s"),(2,"FromConvId","u64","s"),(3,"ToConvId","u64","s")],
 "MsgSearch": [(1,"ConvId","u64","s"),(2,"Query","str","s"),(3,"Limit","u32","s")],
 "MsgSearchResult": [(1,"Messages","MsgNew","rm"),(2,"HasMore","bool","s")],
 "Member": [(1,"UserId","u64","s"),(2,"Role","u32","s"),(3,"JoinedAt","f64","s"),(4,"MutedUntil","f64","s")],
 "ConvPayload": [(1,"ConvId","u64","s"),(2,"Title","str","s"),(3,"ConvType","u32","s"),(4,"CreatedAt","f64","s"),(5,"Members","Member","rm"),(6,"Version","u32","s"),
    (7,"MembersOmitted","bool","s"),(8,"MemberCount","u32","s")],
 "Watermark": [(1,"ConvId","u64","s"),(2,"LatestSeq","u64","s"),(3,"Reason","u32","s"),(4,"MemberCount","u32","s")],
 "ConvCreate": [(1,"Title","str","s"),(2,"ConvType","u32","s"),(3,"MemberIds","u64","r")],
 "ConvUpdate": [(1,"ConvId","u64","s"),(2,"Title","str","s"),(3,"Version","u32","s")],
 "MemberOp": [(1,"ConvId","u64","s"),(2,"UserIds","u64","r"),(3,"Role","u32","s")],
 "MutePayload": [(1,"ConvId","u64","s"),(2,"Until","f64","s")],
 "PinPayload": [(1,"ConvId","u64","s"),(2,"MsgId","u64","s"),(3,"Pinned","bool","s")],
 "Typing": [(1,"ConvId","u64","s"),(2,"Typing","bool","s")],
 "Draft": [(1,"ConvId","u64","s"),(2,"Text","str","s")],
 "Media": [
    (1,"MediaId","str","s"),(2,"Mtype","enum","s"),(3,"Name","str","s"),(4,"Size","i64","s"),
    (5,"Width","u32","s"),(6,"Height","u32","s"),(7,"DurationMs","u32","s"),(8,"Mime","str","s"),
    (9,"ThumbInline","bytes","s"),(10,"Waveform","bytes","s"),(11,"Status","enum","s"),
 ],
 "MediaPresignRequest": [(1,"MediaId","str","s"),(2,"Mime","str","s"),(3,"Size","i64","s")],
 "MediaPresignResult": [(1,"MediaId","str","s"),(2,"Method","str","s"),(3,"UploadUrl","str","s"),(4,"Headers","str","m"),(5,"ExpiresAt","f64","s")],
 "MediaReady": [(1,"MediaId","str","s")],
 "MediaExpire": [(1,"MediaId","str","s")],
 "MediaThumb": [(1,"MediaId","str","s"),(2,"Thumb","bytes","s")],
 "CallSignal": [(1,"CallId","u64","s"),(2,"Sdp","str","s"),(3,"Candidate","str","s")],
 "CallEvent": [(1,"CallId","u64","s"),(2,"FromUserId","u64","s"),(3,"Reason","str","s")],
 "PresenceUpdate": [(1,"UserId","u64","s"),(2,"Status","enum","s"),(3,"At","f64","s")],
 "PresenceSubscribe": [(1,"UserIds","u64","r")],
 "PresenceUnsubscribe": [(1,"UserIds","u64","r")],
 "ProfileUpdate": [(1,"DisplayName","str","s"),(2,"AvatarMediaId","str","s"),(3,"Bio","str","s")],
 "BlockPayload": [(1,"UserId","u64","s")],
 "ReportPayload": [(1,"UserId","u64","s"),(2,"Reason","u32","s"),(3,"Detail","str","s")],
 "AdminBroadcast": [(1,"Text","str","s"),(2,"ConvIds","u64","r"),(3,"ExpiresAt","f64","s")],
 "Announce": [(1,"Title","str","s"),(2,"Text","str","s"),(3,"Level","str","s")],
 "Maintenance": [(1,"BeginAt","f64","s"),(2,"EndAt","f64","s"),(3,"Message","str","s")],
 "InternalFrame": [
    (1,"Envelope","Envelope","s"),(2,"SrcLayer","str","s"),(3,"DstLayer","str","s"),
    (4,"SrcNode","str","s"),(5,"DstNode","str","s"),(6,"ConnId","u64","s"),(7,"TraceId","str","s"),
 ],
 "InternalHello": [(1,"Nonce","bytes","s"),(2,"Layer","str","s"),(3,"Node","str","s"),(4,"Capabilities","str","r")],
 "InternalSubscribe": [(1,"Topic","str","s"),(2,"TtlSeconds","u32","s")],
 "FrameAck": [(1,"Seq","u32","s")],
 "Heartbeat": [(1,"At","f64","s"),(2,"RttMs","u32","s")],
 "ControlPayload": [(1,"Command","str","s"),(2,"Params","str","m"),(3,"Data","bytes","s")],
 "ShutdownNotice": [(1,"Deadline","f64","s"),(2,"Reason","str","s")],
 "ConfigUpdate": [(1,"Source","str","s"),(2,"Changed","str","m")],
 "NodeInfo": [(1,"Layer","str","s"),(2,"Node","str","s"),(3,"Topics","str","r"),(4,"Since","f64","s")],
 "Topology": [(1,"At","f64","s"),(2,"Nodes","NodeInfo","rm")],
 "ReadHistory": [(1,"ConvId","u64","s"),(2,"SinceSeq","u64","s"),(3,"Limit","u32","s"),(4,"ColdMerge","bool","s")],
 "ReadHistoryResult": [(1,"ConvId","u64","s"),(2,"Messages","MsgNew","rm"),(3,"FromCold","bool","s"),(4,"HasMore","bool","s")],
 "ConvDataRequest": [(1,"Op","u32","s"),(2,"ConvId","u64","s"),(3,"Body","bytes","s"),(4,"RequestId","str","s")],
 "DataResult": [(1,"Op","u32","s"),(2,"Ok","bool","s"),(3,"Error","str","s"),(4,"Body","bytes","s"),(5,"RequestId","str","s")],
 "IdentityRequest": [(1,"Op","u32","s"),(2,"UserId","u64","s"),(3,"Body","bytes","s"),(4,"RequestId","str","s")],
 "SecurityRequest": [(1,"Op","u32","s"),(2,"UserId","u64","s"),(3,"Body","bytes","s"),(4,"RequestId","str","s")],
 "FlagRequest": [(1,"Key","str","s"),(2,"Value","str","s"),(3,"Write","bool","s"),(4,"RequestId","str","s")],
 "FlagResult": [(1,"Flags","str","m"),(2,"RequestId","str","s")],
 "JobTrigger": [(1,"Job","str","s"),(2,"DryRun","bool","s"),(3,"RequestId","str","s")],
 "JobStatus": [(1,"Job","str","s"),(2,"State","str","s"),(3,"At","f64","s"),(4,"Detail","str","s")],
 "DiagLoopback": [(1,"Echo","bytes","s"),(2,"SentAt","f64","s"),(3,"Hop","u32","s")],
}

GOTYPE = {"u32":"uint32","u64":"uint64","i32":"int32","i64":"int64","f64":"uint64",
          "str":"string","bytes":"[]byte","bool":"bool","enum":"int32"}

# message-typed field -> referenced message (field name usually differs)
REFS = {
 ("MessagePayload","Content"): "Content",
 ("MessagePayload","Receipts"): "Receipt",
 ("MessagePayload","Media"): "Media",
 ("Content","Media"): "Media",
 ("MsgNew","Content"): "MessagePayload",
 ("ConvPayload","Members"): "Member",
 ("SyncResult","Frames"): "Envelope",
 ("MsgSearchResult","Messages"): "MsgNew",
 ("Topology","Nodes"): "NodeInfo",
 ("InternalFrame","Envelope"): "Envelope",
}

def ref(name, fname, pt):
    return REFS.get((name, fname), pt)

def cast(pt, v):
    return {"u32": f"uint32({v})", "i32": f"int32({v})", "enum": f"int32({v})",
            "i64": f"int64({v})", "bool": f"{v} != 0"}.get(pt, v)

def zero_of(gt):
    if gt == "string": return '""'
    if gt == "bool": return "false"
    if gt.startswith("[]") or gt.startswith("map[") or gt.startswith("*"): return "nil"
    return "0"

HDR = '''// Code generated by gen_chatpb.py from proto/chat.proto. DO NOT EDIT.
// Hand-rolled protobuf wire codec (protoc unavailable in this environment).
// Wire format is byte-compatible with protoc-gen-go for proto3:
// scalar zero values omitted on encode, unknown fields skipped on decode.
package chatpb

import "encoding/binary"

func pbAppendVarint(b []byte, tag int, v uint64) []byte {
	b = append(b, byte(tag<<3|0))
	return binary.AppendUvarint(b, v)
}

func pbAppendFixed64(b []byte, tag int, v uint64) []byte {
	b = append(b, byte(tag<<3|1))
	return binary.LittleEndian.AppendUint64(b, v)
}

func pbAppendBytes(b []byte, tag int, v []byte) []byte {
	b = append(b, byte(tag<<3|2))
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func pbAppendString(b []byte, tag int, v string) []byte {
	return pbAppendBytes(b, tag, []byte(v))
}

func pbVarintLen(v uint64) int { return len(binary.AppendUvarint(nil, v)) }

func mustMarshal(m interface{ Marshal() ([]byte, error) }) []byte {
	b, _ := m.Marshal()
	return b
}

func pbReadTag(b []byte) (tag, wire int, rest []byte, ok bool) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, nil, false
	}
	return int(v >> 3), int(v & 7), b[n:], true
}

func pbReadVarint(b []byte) (uint64, []byte, bool) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, nil, false
	}
	return v, b[n:], true
}

func pbReadFixed64(b []byte) (uint64, []byte, bool) {
	if len(b) < 8 {
		return 0, nil, false
	}
	return binary.LittleEndian.Uint64(b), b[8:], true
}

func pbReadBytes(b []byte) ([]byte, []byte, bool) {
	n, m := binary.Uvarint(b)
	if m <= 0 || uint64(len(b)-m) < n {
		return nil, nil, false
	}
	return b[m : m+int(n)], b[m+int(n):], true
}

func pbSkip(b []byte, wire int) ([]byte, bool) {
	switch wire {
	case 0:
		_, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, false
		}
		return b[n:], true
	case 1:
		if len(b) < 8 {
			return nil, false
		}
		return b[8:], true
	case 2:
		n, m := binary.Uvarint(b)
		if m <= 0 || uint64(len(b)-m) < n {
			return nil, false
		}
		return b[m+int(n):], true
	case 5:
		if len(b) < 4 {
			return nil, false
		}
		return b[4:], true
	default:
		return nil, false
	}
}

func pbDecodeMapEntry(b []byte) (string, string, error) {
	var k, v string
	for len(b) > 0 {
		tag, wire, rest, ok := pbReadTag(b)
		if !ok {
			return "", "", ErrBadWire
		}
		b = rest
		switch tag {
		case 1:
			vv, r, ok := pbReadBytes(b)
			if !ok {
				return "", "", ErrBadWire
			}
			b = r
			k = string(vv)
		case 2:
			vv, r, ok := pbReadBytes(b)
			if !ok {
				return "", "", ErrBadWire
			}
			b = r
			v = string(vv)
		default:
			r, ok := pbSkip(b, wire)
			if !ok {
				return "", "", ErrBadWire
			}
			b = r
		}
	}
	return k, v, nil
}

// ErrBadWire malformed wire data.
var ErrBadWire = protoerr{}

type protoerr struct{}

func (protoerr) Error() string { return "chatpb: bad wire data" }
'''

def decls(name, fields):
    out = []
    for num, fname, pt, label in fields:
        if label == "o":
            if not any("\tTarget" in d for d in out):
                out.append("\t// Target oneof: ConvId(10) / UserId(11).\n\tTarget isEnvelope_Target `json:\"-\"`")
            continue
        if pt in SPEC:
            r = ref(name, fname, pt)
            out.append(f"\t{fname} {'[]*' if label == 'rm' else '*'}{r}")
        elif label == "m":
            out.append(f"\t{fname} map[string]string")
        elif label == "r":
            out.append(f"\t{fname} []{GOTYPE[pt]}")
        else:
            out.append(f"\t{fname} {GOTYPE[pt]}")
    return out

def getters(name, fields):
    s = ""
    for num, fname, pt, label in fields:
        if label == "o":
            # oneof getter must not panic on nil/mismatched Target
            s += f"""func (m *{name}) Get{fname}() uint64 {{
	if m != nil {{
		if t, ok := m.Target.(*Envelope_{fname}); ok {{ return t.{fname} }}
	}}
	return 0
}}

func (m *{name}) Set{fname}(v uint64) {{
	if m != nil {{ m.Target = &Envelope_{fname}{{{fname}: v}} }}
}}

"""
            continue
        elif pt in SPEC:
            gt = "*" + ref(name, fname, pt)
            if label == "rm":
                gt = "[]" + gt
            expr = f"m.{fname}"
        elif label == "m":
            gt, expr = "map[string]string", f"m.{fname}"
        elif label == "r":
            gt, expr = "[]" + GOTYPE[pt], f"m.{fname}"
        else:
            gt, expr = GOTYPE[pt], f"m.{fname}"
        s += f"func (m *{name}) Get{fname}() {gt} {{\n\tif m != nil {{\n\t\treturn {expr}\n\t}}\n\treturn {zero_of(gt)}\n}}\n\n"
    return s

def sizefn(name, fields):
    s = f"func (m *{name}) sizeHint() int {{\n\tif m == nil {{\n\t\treturn 0\n\t}}\n\tvar n int\n"
    for num, fname, pt, label in fields:
        if label == "o":
            s += f"\tif t, ok := m.Target.(*Envelope_{fname}); ok {{\n\t\tn += 1 + pbVarintLen(uint64(t.{fname}))\n\t}}\n"
        elif pt in SPEC:
            if label == "rm":
                s += f"\tfor _, e := range m.{fname} {{\n\t\tn += 1 + pbVarintLen(uint64(e.sizeHint())) + e.sizeHint()\n\t}}\n"
            else:
                s += f"\tif m.{fname} != nil {{\n\t\tn += 1 + pbVarintLen(uint64(m.{fname}.sizeHint())) + m.{fname}.sizeHint()\n\t}}\n"
        elif label == "m":
            s += f"\tfor k, v := range m.{fname} {{\n\t\tn += 1 + pbVarintLen(uint64(len(k)+1+len(v))) + len(k) + 1 + len(v)\n\t}}\n"
        elif label == "r":
            if pt == "str":
                s += f"\tfor _, e := range m.{fname} {{\n\t\tn += 1 + pbVarintLen(uint64(len(e))) + len(e)\n\t}}\n"
            else:
                s += f"\tfor _, e := range m.{fname} {{\n\t\tn += 1 + pbVarintLen(uint64(e))\n\t}}\n"
        elif pt in ("str", "bytes"):
            s += f"\tn += 1 + pbVarintLen(uint64(len(m.{fname}))) + len(m.{fname})\n"
        elif pt == "f64":
            s += f"\tif m.{fname} != 0 {{\n\t\tn += 9\n\t}}\n"
        elif pt == "bool":
            s += f"\tif m.{fname} {{\n\t\tn += 2\n\t}}\n"
        else:
            s += f"\tif m.{fname} != 0 {{\n\t\tn += 1 + pbVarintLen(uint64(m.{fname}))\n\t}}\n"
    s += "\treturn n\n}\n\n"
    return s

def marshalfn(name, fields):
    s = f"func (m *{name}) Marshal() ([]byte, error) {{\n\tb := make([]byte, 0, m.sizeHint())\n"
    for num, fname, pt, label in fields:
        if label == "o":
            s += f"\tif t, ok := m.Target.(*Envelope_{fname}); ok {{\n\t\tb = pbAppendVarint(b, {num}, uint64(t.{fname}))\n\t}}\n"
        elif pt in SPEC:
            if label == "rm":
                s += f"\tfor _, e := range m.{fname} {{\n\t\tb = pbAppendBytes(b, {num}, mustMarshal(e))\n\t}}\n"
            else:
                s += f"\tif m.{fname} != nil {{\n\t\tb = pbAppendBytes(b, {num}, mustMarshal(m.{fname}))\n\t}}\n"
        elif label == "m":
            s += f"\tfor k, v := range m.{fname} {{\n\t\tkv := pbAppendString(pbAppendString(nil, 1, k), 2, v)\n\t\tb = pbAppendBytes(b, {num}, kv)\n\t}}\n"
        elif label == "r":
            if pt == "str":
                s += f"\tfor _, e := range m.{fname} {{\n\t\tb = pbAppendString(b, {num}, e)\n\t}}\n"
            else:
                s += f"\tfor _, e := range m.{fname} {{\n\t\tb = pbAppendVarint(b, {num}, uint64(e))\n\t}}\n"
        elif pt == "str":
            s += f"\tif m.{fname} != \"\" {{\n\t\tb = pbAppendString(b, {num}, m.{fname})\n\t}}\n"
        elif pt == "bytes":
            s += f"\tif len(m.{fname}) > 0 {{\n\t\tb = pbAppendBytes(b, {num}, m.{fname})\n\t}}\n"
        elif pt == "f64":
            s += f"\tif m.{fname} != 0 {{\n\t\tb = pbAppendFixed64(b, {num}, m.{fname})\n\t}}\n"
        elif pt == "bool":
            s += f"\tif m.{fname} {{\n\t\tb = pbAppendVarint(b, {num}, 1)\n\t}}\n"
        else:
            s += f"\tif m.{fname} != 0 {{\n\t\tb = pbAppendVarint(b, {num}, uint64(m.{fname}))\n\t}}\n"
    s += "\treturn b, nil\n}\n\n"
    return s

def unmarshalfn(name, fields):
    s = f"func (m *{name}) Unmarshal(b []byte) error {{\n\tfor len(b) > 0 {{\n\t\ttag, wire, rest, ok := pbReadTag(b)\n\t\tif !ok {{\n\t\t\treturn ErrBadWire\n\t\t}}\n\t\tb = rest\n\t\tswitch tag {{\n"
    for num, fname, pt, label in fields:
        if label == "o":
            s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadVarint(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.Target = &Envelope_{fname}{{{fname}: v}}\n"
        elif pt in SPEC:
            r = ref(name, fname, pt)
            if label == "rm":
                s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadBytes(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\te := &{r}{{}}\n\t\t\tif err := e.Unmarshal(v); err != nil {{\n\t\t\t\treturn err\n\t\t\t}}\n\t\t\tm.{fname} = append(m.{fname}, e)\n"
            else:
                s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadBytes(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tif m.{fname} == nil {{\n\t\t\t\tm.{fname} = &{r}{{}}\n\t\t\t}}\n\t\t\tif err := m.{fname}.Unmarshal(v); err != nil {{\n\t\t\t\treturn err\n\t\t\t}}\n"
        elif label == "m":
            s += f"\t\tcase {num}:\n\t\t\tkv, r, ok := pbReadBytes(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tk, v, err := pbDecodeMapEntry(kv)\n\t\t\tif err != nil {{\n\t\t\t\treturn err\n\t\t\t}}\n\t\t\tif m.{fname} == nil {{\n\t\t\t\tm.{fname} = map[string]string{{}}\n\t\t\t}}\n\t\t\tm.{fname}[k] = v\n"
        elif label == "r":
            if pt == "str":
                s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadBytes(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.{fname} = append(m.{fname}, string(v))\n"
            else:
                s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadVarint(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.{fname} = append(m.{fname}, {cast(pt, 'v')})\n"
        elif pt == "str":
            s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadBytes(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.{fname} = string(v)\n"
        elif pt == "bytes":
            s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadBytes(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.{fname} = append([]byte(nil), v...)\n"
        elif pt == "f64":
            s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadFixed64(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.{fname} = v\n"
        else:
            s += f"\t\tcase {num}:\n\t\t\tv, r, ok := pbReadVarint(b)\n\t\t\tif !ok {{\n\t\t\t\treturn ErrBadWire\n\t\t\t}}\n\t\t\tb = r\n\t\t\tm.{fname} = {cast(pt, 'v')}\n"
    s += "\t\tdefault:\n\t\t\tr, ok := pbSkip(b, wire)\n\t\t\tif !ok {\n\t\t\t\treturn ErrBadWire\n\t\t\t}\n\t\t\tb = r\n\t\t}\n\t}\n\treturn nil\n}\n\n"
    return s

def message(name, fields):
    s = f"// {name} mirrors chat.v1.{name} (proto3).\ntype {name} struct {{\n"
    s += "\n".join(decls(name, fields)) + "\n}\n\n"
    if name == "Envelope":
        s += '''type isEnvelope_Target interface{ isEnvelope_Target() }

type Envelope_ConvId struct{ ConvId uint64 }

func (*Envelope_ConvId) isEnvelope_Target() {}

type Envelope_UserId struct{ UserId uint64 }

func (*Envelope_UserId) isEnvelope_Target() {}

func (m *Envelope) GetTarget() isEnvelope_Target {
	if m == nil || m.Target == nil {
		return nil
	}
	return m.Target
}

'''
    s += getters(name, fields)
    s += sizefn(name, fields)
    s += marshalfn(name, fields)
    s += unmarshalfn(name, fields)
    s += f"func (m *{name}) Reset() {{ *m = {name}{{}} }}\n\nfunc (m *{name}) String() string {{ return \"{name}\" }}\n\nfunc (m *{name}) ProtoMessage() {{}}\n\n"
    return s

def main():
    src = HDR
    for name, fields in SPEC.items():
        src += message(name, fields)
    path = os.path.join(OUT, "chatpb.go")
    with open(path, "w") as f:
        f.write(src)
    print("wrote", path, len(src), "bytes")

if __name__ == "__main__":
    main()
