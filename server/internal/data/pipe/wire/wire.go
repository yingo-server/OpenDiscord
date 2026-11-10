// Package wire is the data layer's minimal payload codec for internal frames.
//
// Why it exists: the canonical contract source is proto/chat.proto, but the
// generated chatpb package is not present in this environment (no protoc /
// no module cache). This package provides hand-written, JSON-codec structs
// matching the proto field numbers' SEMANTICS for exactly the payloads the
// data layer needs. When chatpb is generated, this package is deleted and the
// domain handlers swap the struct types for the generated ones — the handler
// logic, store calls and frame flow do not change.
//
// Field names match the proto messages so the JSON wire form (protojson
// lowerCamelCase) is byte-compatible with the generated codec for these
// messages.
package wire

import "encoding/json"

// Envelope mirrors the frame's envelope header (subset the data layer reads).
// Frames arrive as *proto.Frame from transport; domain handlers rebuild this
// view from Frame.Env where needed.
type Envelope struct {
	ID        uint64 `json:"id,omitempty"`
	Kind      uint32 `json:"kind,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	ConvID    uint64 `json:"convId,omitempty"`
	UserID    uint64 `json:"userId,omitempty"`
	Payload   []byte `json:"payload,omitempty"`
}

// Media mirrors chat.v1.Media (the subset the data layer needs to persist and
// re-emit; bytes never ride the frame). camelCase tags match protojson so the
// pipeline's stored content is byte-compatible with the generated codec.
type Media struct {
	MediaID    string `json:"mediaId,omitempty"`
	MType      int32  `json:"mtype,omitempty"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Mime       string `json:"mime,omitempty"`
	Width      uint32 `json:"width,omitempty"`
	Height     uint32 `json:"height,omitempty"`
	DurationMs uint32 `json:"durationMs,omitempty"`
}

// MessagePayload mirrors chat.v1.MessagePayload (MSG_SEND 101).
type MessagePayload struct {
	ClientMsgID string  `json:"clientMsgId,omitempty"`
	Text        string  `json:"text,omitempty"`
	Media       []Media `json:"media,omitempty"`
	ReplyTo     uint64  `json:"replyTo,omitempty"`
	// Mentions is the @mention uid list (FE-1a #3). The name is identical in
	// snake_case and camelCase, so both wire forms share one field.
	Mentions []uint64        `json:"mentions,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// MsgAck mirrors chat.v1.MsgAck (MSG_ACK 103).
type MsgAck struct {
	MsgID       uint64 `json:"msgId,omitempty"`
	Seq         uint64 `json:"seq,omitempty"`
	ClientMsgID string `json:"clientMsgId,omitempty"`
	Status      uint32 `json:"status,omitempty"`
}

// Error mirrors chat.v1.Error.
type Error struct {
	Code         uint32 `json:"code,omitempty"`
	Reason       string `json:"reason,omitempty"`
	RetryAfterMS uint32 `json:"retryAfterMs,omitempty"`
}

// ReadHistory mirrors chat.v1.ReadHistory (920).
type ReadHistory struct {
	ConvID   uint64 `json:"convId,omitempty"`
	SinceSeq uint64 `json:"sinceSeq,omitempty"`
	// BeforeSeq selects the "older" direction: return messages with seq
	// strictly less than BeforeSeq (keyset cursor walking backwards into
	// history). It is additive and opt-in: when it is 0 the legacy SinceSeq
	// behaviour (kind 8 SYNC catch-up and old clients) is unchanged. When set,
	// the server replies in ASCENDING seq order so a client can prepend the
	// page without re-sorting.
	BeforeSeq uint64 `json:"beforeSeq,omitempty"`
	Limit     uint32 `json:"limit,omitempty"`
	ColdMerge bool   `json:"coldMerge,omitempty"`
}

// MsgNew mirrors chat.v1.MsgNew (used inside ReadHistoryResult 921).
type MsgNew struct {
	MsgID    uint64 `json:"msgId,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`
	SenderID uint64 `json:"senderId,omitempty"`
	SentAt   int64  `json:"sentAt,omitempty"`
	// Recalled mirrors the hot row's soft-delete column: 0 = not recalled,
	// otherwise the unix-second timestamp of the recall. It is a TIMESTAMP
	// (not a bool) so the client can tell "recalled at" from "never recalled"
	// and so the value survives a re-read of an already-recalled row.
	// Without it, a device that pulls history after the recall broadcast
	// (new phone / reinstall) would resurrect the original text.
	Recalled int64 `json:"recalled,omitempty"`
	// Content is the opaque Envelope payload, base64-encoded on the wire
	// (same as protobuf bytes -> JSON). Never embed raw bytes as JSON.
	Content []byte `json:"content,omitempty"`
}

// ReadHistoryResult mirrors chat.v1.ReadHistoryResult (921).
type ReadHistoryResult struct {
	ConvID   uint64   `json:"convId,omitempty"`
	Messages []MsgNew `json:"messages,omitempty"`
	FromCold bool     `json:"fromCold,omitempty"`
	HasMore  bool     `json:"hasMore,omitempty"`
}

// DataResult mirrors chat.v1.DataResult (931/941/961…).
type DataResult struct {
	Op        uint32 `json:"op,omitempty"`
	Ok        bool   `json:"ok,omitempty"`
	Error     string `json:"error,omitempty"`
	Body      []byte `json:"body,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// FlagRequest mirrors chat.v1.FlagRequest (970/971).
type FlagRequest struct {
	Key       string `json:"key,omitempty"`
	Value     string `json:"value,omitempty"`
	Write     bool   `json:"write,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// FlagResult mirrors chat.v1.FlagResult (972).
type FlagResult struct {
	Flags     map[string]string `json:"flags,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
}

// JobTrigger mirrors chat.v1.JobTrigger (980).
type JobTrigger struct {
	Job       string `json:"job,omitempty"`
	DryRun    bool   `json:"dryRun,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// JobStatus mirrors chat.v1.JobStatus (981).
type JobStatus struct {
	Job    string `json:"job,omitempty"`
	State  string `json:"state,omitempty"`
	At     int64  `json:"at,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// CallSignal is the unified payload for the five call-signaling kinds
// (350 OFFER / 351 ANSWER / 352 CANDIDATE / 353 BUSY / 354 HANGUP). The wire
// form is one flat camelCase JSON object; each kind uses the subset it needs.
// Field names are the front/back shared contract — DO NOT rename. The server
// always overwrites FromUserID with the authenticated sender uid on relay.
type CallSignal struct {
	CallID    string `json:"callId,omitempty"`
	ConvID    uint64 `json:"convId,omitempty"`
	ToUserID  uint64 `json:"toUserId,omitempty"`
	SDP       string `json:"sdp,omitempty"`
	Media     string `json:"media,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	SDPMid    string `json:"sdpMid,omitempty"`
	// SDPMLineIndex is a pointer so a legitimate 0 survives the round-trip
	// (omitempty on a plain int would drop it).
	SDPMLineIndex *uint32 `json:"sdpMLineIndex,omitempty"`
	Reason        string  `json:"reason,omitempty"`
	FromUserID    uint64  `json:"fromUserId,omitempty"`
}

// Member mirrors chat.v1.Member.
type Member struct {
	UserID     uint64 `json:"userId,omitempty"`
	Role       uint32 `json:"role,omitempty"`
	JoinedAt   int64  `json:"joinedAt,omitempty"`
	MutedUntil int64  `json:"mutedUntil,omitempty"`
}

// ---------------------------------------------------------------------------
// 瞬态信令（回执/输入中/presence）与消息撤回 —— 与前端 payload.ts 的
// JSON 形态一一对应（snake_case 沿用 proto/chat.proto 字段名）。
// ---------------------------------------------------------------------------

// Receipt mirrors chat.v1.Receipt (MSG_READ 104 / MSG_DELIVERED 105).
// rtype: 0 = read, 1 = delivered. The server ALWAYS overwrites user_id with
// the authenticated sender; the client's value is never trusted.
type Receipt struct {
	MsgID  uint64 `json:"msg_id,omitempty"`
	UserID uint64 `json:"user_id,omitempty"`
	At     uint64 `json:"at,omitempty"`
	Rtype  int32  `json:"rtype,omitempty"`
}

// ReceiptPayload mirrors MessagePayload.receipts: one frame may carry several
// receipts, or a single flat receipt (the client's encodeReceipt form).
type ReceiptPayload struct {
	Receipts []Receipt `json:"receipts,omitempty"`
	// 单条形态（前端 encodeReceipt 的 flat JSON）——归一化进 Receipts。
	MsgID  uint64 `json:"msg_id,omitempty"`
	UserID uint64 `json:"user_id,omitempty"`
	At     uint64 `json:"at,omitempty"`
	Rtype  int32  `json:"rtype,omitempty"`
}

// TypingSignal mirrors chat.v1.Typing + the client's TypingPayload extras.
// Typing has NO omitempty: a "stopped typing" frame must survive the
// round-trip as {"typing": false}, not an absent field.
type TypingSignal struct {
	ConvID uint64 `json:"conv_id,omitempty"`
	UserID uint64 `json:"user_id,omitempty"`
	Typing bool   `json:"typing"`
	At     int64  `json:"at,omitempty"`
}

// PresenceSignal is the unified presence payload. Two forms share it:
//   - client uplink 400 (PRESENCE_UPDATE): {user_id, status, at}, status is
//     the client-side string enum "online"|"away"|"offline"
//   - user-layer internal 401/402 (connect/disconnect): {user_id, online}
type PresenceSignal struct {
	UserID uint64 `json:"user_id,omitempty"`
	Status string `json:"status,omitempty"`
	// Online is a pointer so a legitimate false survives the round-trip.
	Online *bool `json:"online,omitempty"`
	At     int64 `json:"at,omitempty"`
}

// MsgMutation mirrors chat.v1.MsgDelete/MsgRecall (106/107/108). The recall
// request and the recall broadcast share this flat snake_case shape.
type MsgMutation struct {
	MsgID       uint64 `json:"msg_id,omitempty"`
	ConvID      uint64 `json:"conv_id,omitempty"`
	Text        string `json:"text,omitempty"`
	ForEveryone bool   `json:"for_everyone,omitempty"`
}

// MsgEditPayload is the 106 MSG_EDIT uplink shape. Field names follow the
// existing client msg-mutation snake_case convention (same family as
// MsgMutation), so a browser payload passes through unmarshalled as-is.
type MsgEditPayload struct {
	MsgID  uint64 `json:"msg_id,omitempty"`
	ConvID uint64 `json:"conv_id,omitempty"`
	Text   string `json:"text,omitempty"`
}

// ForwardFrom records the provenance of a forwarded message (FE-1a #2): the
// original msg id and conversation. It is stored inside the new message's
// content under the forward_from key; ids only, no text duplication.
type ForwardFrom struct {
	MsgID  uint64 `json:"msg_id,omitempty"`
	ConvID uint64 `json:"conv_id,omitempty"`
}

// MsgForwardPayload is the 109 MSG_FORWARD uplink: which message, from which
// conversation, into which target conversation (the envelope conv_id is
// authoritative when to_conv_id is absent).
type MsgForwardPayload struct {
	MsgID       uint64      `json:"msg_id,omitempty"`
	FromConvID  uint64      `json:"from_conv_id,omitempty"`
	ToConvID    uint64      `json:"to_conv_id,omitempty"`
	ForwardFrom ForwardFrom `json:"forward_from,omitempty"`
}

// ReportPayload is the 453 REPORT uplink. Detail is an enum/string the CLIENT
// supplies; the server never stores message bodies or free-form user text in
// it beyond a bounded truncation (PII red line: ids + reason enum only).
type ReportPayload struct {
	TargetMsg  uint64 `json:"target_msg,omitempty"`
	TargetUser uint64 `json:"target_user,omitempty"`
	Reason     uint32 `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// MemberUpdate is the 212 MEMBER_UPDATE event body. camelCase (the conv
// payload wire convention) so a future client handler can decode it with
// decodeConvPayload-compatible tooling; op ∈ "add"|"remove"|"role".
type MemberUpdate struct {
	ConvID   uint64  `json:"convId,omitempty"`
	Op       string  `json:"op,omitempty"`
	Member   *Member `json:"member,omitempty"`
	ByUserID uint64  `json:"byUserId,omitempty"`
}

// ConvPayload mirrors chat.v1.ConvPayload.
//
// The three watermark fields below (Version / LastSeq / Unread) are NOT in
// the proto yet, but the client contract (apps/client decodeConvPayload)
// already reads them by these exact camelCase names. They are what makes a
// brand-new device notice it is missing history:
//
//   - LastSeq: the conversation's current high-water mark (conv_seq.next_seq
//     minus the reserved-but-unwritten tail; i.e. the highest committed seq).
//     The client compares it with its local cursor and triggers a SYNC gap
//     pull when the server is ahead.
//   - Version: a monotonic revision of this snapshot, bumped on every
//     membership/title change. The client uses it to detect that a
//     CONV_PAYLOAD event supersedes an older cached snapshot.
//   - Unread: the server's estimate of how many messages the recipient has
//     not read yet (0 when the server does not track read state — it is a
//     hint, never an authorization input).
type ConvPayload struct {
	ConvID    uint64   `json:"convId,omitempty"`
	Title     string   `json:"title,omitempty"`
	ConvType  uint32   `json:"convType,omitempty"`
	CreatedAt int64    `json:"createdAt,omitempty"`
	Members   []Member `json:"members,omitempty"`
	Version   uint64   `json:"version,omitempty"`
	LastSeq   uint64   `json:"lastSeq,omitempty"`
	Unread    uint64   `json:"unread,omitempty"`
	// MembersOmitted / MemberCount 是 proto 的字段 7/8（v1.2 拉取模式，§1.4）：
	// 大群不再推全量成员快照，members 为空、成员数由 MemberCount 给出，成员
	// 列表由客户端按需走 930/931 拉取。旧客户端忽略未知字段，等同于"成员
	// 列表为空"，不会解析失败。
	MembersOmitted bool   `json:"membersOmitted,omitempty"`
	MemberCount    uint32 `json:"memberCount,omitempty"`
}

// Watermark mirrors chat.v1.Watermark (kind 14, 拉取模式水位线).
//
// latest_seq 只引用已经完成 seq 分配且已落库（可被 920/8 读到）的值；客户端
// 拉取 [lastSeq+1, latest_seq] 必有数据。reason 仅用于诊断与测试断言，
// 客户端逻辑不得依赖其取值（§1.1）。因此 reason 不用 omitempty：0 也是一个
// 有意义的取值，必须在帧里显式出现。
type Watermark struct {
	ConvID      uint64 `json:"convId,omitempty"`
	LatestSeq   uint64 `json:"latestSeq,omitempty"`
	Reason      uint32 `json:"reason"`
	MemberCount uint32 `json:"memberCount,omitempty"`
}

// MemberOp mirrors chat.v1.MemberOp (generic member ops inside ConvData).
type MemberOp struct {
	ConvID  uint64   `json:"convId,omitempty"`
	UserIDs []uint64 `json:"userIds,omitempty"`
	Role    uint32   `json:"role,omitempty"`
}

// IdentityRequest mirrors chat.v1.IdentityRequest (940).
type IdentityRequest struct {
	Op        uint32 `json:"op,omitempty"`
	UserID    uint64 `json:"userId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// SecurityRequest mirrors chat.v1.SecurityRequest (960).
type SecurityRequest struct {
	Op        uint32 `json:"op,omitempty"`
	UserID    uint64 `json:"userId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Marshal is json.Marshal with the error passed through.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Unmarshal is json.Unmarshal with the error passed through.
func Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
