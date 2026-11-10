package main

// wire.go: the three wire-level adapters the assembly owns.
//
//  1. newFrameSource  — *proto.Frame (bridge → data) → pipe.Req. The data layer
//     is wire-agnostic, so unwrapping the InternalFrame routing header and
//     resolving the acting uid happens here.
//  2. installEmit     — pipe env → *proto.Frame (data → bridge), chatpb-backed.
//  3. buildRoutes     — the bridge route table: which kind goes to which layer.
//
// Plus the key loading helpers (Ed25519 PEM + HMAC index key).

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/bridge/router"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
)

// uidResolver maps an entry connection id back to its authenticated uid.
// Supplied by the user layer's session table (the forwarder already carries
// conn_id end to end; the uid is never taken from the client envelope).
type uidResolver func(connID uint64) int64

// newFrameSource unwraps the InternalFrame header and fills pipe.Req.
func newFrameSource(resolve uidResolver, lg *log.Logger) pipe.FrameSource {
	return func(f *proto.Frame) pipe.Req {
		if f == nil {
			return pipe.Req{}
		}
		if f.Env != nil && len(f.Env.Payload) > 0 {
			if intf, err := frame.UnmarshalInternal(f.Env.Payload); err == nil && intf.GetEnvelope() != nil {
				env := intf.GetEnvelope()
				// Actor identity: for CLIENT frames the uid MUST come from the
				// session (resolved by conn id). The envelope's user_id is
				// client-controlled and used to be trusted here, which allowed
				// impersonation (recall others' messages, forge receipts).
				// Frames originated by the data/admin layers carry no conn id;
				// their user_id was written by the server itself and is trusted.
				src := ""
				if f.Meta != nil {
					src = f.Meta.SrcLayer
				}
				var uid int64
				if src == "user" && resolve != nil && intf.GetConnId() != 0 {
					// CLIENT frame: identity comes from the session only.
					uid = resolve(intf.GetConnId())
				} else if src == "user" && intf.GetConnId() == 0 {
					// USER-LAYER INTERNAL event (e.g. presence connect/disconnect):
					// it never travels through a client connection, so the uid
					// written by the server itself is authoritative.
					uid = int64(env.GetUserId())
				} else {
					uid = int64(env.GetUserId())
				}
				if uid == 0 && lg != nil {
					lg.Debug("frame without uid", "kind", kinds.KindName(env.Kind), "conn", intf.GetConnId(), "src", src)
				}
				kind := env.Kind
				payload := payloadForPipe(kind, env.Payload)
				// User-facing conversation kinds are translated into the data
				// layer's 930 CONV_DATA lane (ops 1/5/2/3). The translation is
				// deliberately here in the assembly: the pipe only understands
				// wire.ConvPayload/MemberOp, never client chatpb messages.
				if mappedKind, mappedPayload, ok := convKindToPipe(env); ok {
					kind, payload = mappedKind, mappedPayload
				}
				return pipe.Req{
					Kind:      kind,
					RequestID: env.GetRequestId(),
					ConvID:    env.GetConvId(),
					UserID:    uint64(uid),
					TraceID:   intf.GetTraceId(),
					Payload:   payload,
					Frame:     f,
				}
			}
		}
		return pipe.ShellFrameSource(f)
	}
}

// payloadForPipe converts the protobuf payload of the kinds the data pipe
// parses into the pipe's placeholder wire structs (data/pipe/wire, JSON).
//
// Why here: the pipe is deliberately wire-agnostic ("the assembly installs the
// chatpb-backed FrameSource; when chatpb lands the FrameSource is the only code
// that changes" — data/pipe/req.go). This function is that adapter. Kinds the
// pipe does not parse are passed through verbatim.
//
// NOTE: MSG_SEND content is stored in the same form the pipe received it
// (currently the wire JSON), because the pipe persists req.Payload verbatim.
func payloadForPipe(kind uint32, payload []byte) []byte {
	switch kind {
	case kinds.KindMsgSend:
		// Browser clients speak the JSON wire form (snake_case); native
		// clients speak protobuf. Accept both so MSG_SEND works end to end.
		if isJSONPayload(payload) {
			var raw struct {
				ClientMsgID      string `json:"client_msg_id"`
				ClientMsgIDCamel string `json:"clientMsgId"`
				Text             string `json:"text"`
				ReplyTo          uint64 `json:"reply_to"`
				ReplyToCamel     uint64 `json:"replyTo"`
				Media            []struct {
					MediaID    string `json:"media_id"`
					MediaIDCam string `json:"mediaId"`
					MType      int32  `json:"mtype"`
					Name       string `json:"name"`
					Size       int64  `json:"size"`
					Mime       string `json:"mime"`
					Width      uint32 `json:"width"`
					Height     uint32 `json:"height"`
					DurationMs uint32 `json:"duration_ms"`
					DurationMc uint32 `json:"durationMs"`
				} `json:"media"`
				Content *struct {
					Media []struct {
						MediaID    string `json:"media_id"`
						MediaIDCam string `json:"mediaId"`
						MType      int32  `json:"mtype"`
						Name       string `json:"name"`
						Size       int64  `json:"size"`
						Mime       string `json:"mime"`
						Width      uint32 `json:"width"`
						Height     uint32 `json:"height"`
						DurationMs uint32 `json:"duration_ms"`
						DurationMc uint32 `json:"durationMs"`
					} `json:"media"`
				} `json:"content"`
			}
			if err := json.Unmarshal(payload, &raw); err == nil {
				clientMsgID := raw.ClientMsgID
				if clientMsgID == "" {
					clientMsgID = raw.ClientMsgIDCamel
				}
				replyTo := raw.ReplyTo
				if replyTo == 0 {
					replyTo = raw.ReplyToCamel
				}
				media := make([]wire.Media, 0, len(raw.Media))
				for _, m := range raw.Media {
					id := m.MediaID
					if id == "" {
						id = m.MediaIDCam
					}
					dm := m.DurationMs
					if dm == 0 {
						dm = m.DurationMc
					}
					media = append(media, wire.Media{MediaID: id, MType: m.MType, Name: m.Name,
						Size: m.Size, Mime: m.Mime, Width: m.Width, Height: m.Height, DurationMs: dm})
				}
				if len(media) == 0 && raw.Content != nil {
					for _, m := range raw.Content.Media {
						id := m.MediaID
						if id == "" {
							id = m.MediaIDCam
						}
						dm := m.DurationMs
						if dm == 0 {
							dm = m.DurationMc
						}
						media = append(media, wire.Media{MediaID: id, MType: m.MType, Name: m.Name,
							Size: m.Size, Mime: m.Mime, Width: m.Width, Height: m.Height, DurationMs: dm})
					}
				}
				if out, err := json.Marshal(wire.MessagePayload{
					ClientMsgID: clientMsgID,
					Text:        raw.Text,
					Media:       media,
					ReplyTo:     replyTo,
				}); err == nil {
					return out
				}
			}
			return payload
		}
		var m chatpb.MessagePayload
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		pbMedia := m.GetMedia()
		if len(pbMedia) == 0 && m.GetContent() != nil {
			pbMedia = m.GetContent().GetMedia()
		}
		media := make([]wire.Media, 0, len(pbMedia))
		for _, mm := range pbMedia {
			media = append(media, wire.Media{
				MediaID: mm.GetMediaId(), MType: mm.GetMtype(), Name: mm.GetName(),
				Size: mm.GetSize(), Mime: mm.GetMime(), Width: mm.GetWidth(),
				Height: mm.GetHeight(), DurationMs: mm.GetDurationMs(),
			})
		}
		out, err := json.Marshal(wire.MessagePayload{
			ClientMsgID: m.GetClientMsgId(),
			Text:        m.GetText(),
			Media:       media,
			ReplyTo:     m.GetReplyTo(),
		})
		if err != nil {
			return payload
		}
		return out
	case kinds.KindMsgRead, kinds.KindMsgDelivered, kinds.KindMsgDelete, kinds.KindMsgRecall,
		kinds.KindTyping, kinds.KindPresenceUpdate:
		// Client chat kinds the pipe parses directly. Accept both wire forms:
		// browser JSON (snake_case per the front payload.ts contract) and
		// native protobuf. The pipe's wire structs use the same field names,
		// so JSON passes through verbatim; protobuf is converted.
		return convertChatPayload(kind, payload)
	case kinds.KindMsgEdit, kinds.KindMsgForward, kinds.KindReport:
		// FE-1a uplink kinds: the browser JSON forms share the pipe wire
		// structs' snake_case names and pass through verbatim; native
		// protobuf is normalised into those structs.
		return convertDomainV2Payload(kind, payload)
	case kinds.KindSyncRequest:
		// SYNC_REQUEST (8): normalize either the browser JSON form
		// ({convId,sinceSeq,limit} or snake_case) or the native protobuf
		// chatpb.SyncRequest into the pipe's wire.ReadHistory JSON shape that
		// onSyncRequest parses.
		if isJSONPayload(payload) {
			var raw struct {
				ConvID     uint64 `json:"convId"`
				ConvIDSnk  uint64 `json:"conv_id"`
				SinceSeq   uint64 `json:"sinceSeq"`
				SinceSnake uint64 `json:"since_seq"`
				Limit      uint32 `json:"limit"`
			}
			if err := json.Unmarshal(payload, &raw); err == nil {
				convID := raw.ConvID
				if convID == 0 {
					convID = raw.ConvIDSnk
				}
				since := raw.SinceSeq
				if since == 0 {
					since = raw.SinceSnake
				}
				if out, err := json.Marshal(wire.ReadHistory{ConvID: convID, SinceSeq: since, Limit: raw.Limit}); err == nil {
					return out
				}
			}
		}
		var sr chatpb.SyncRequest
		if err := sr.Unmarshal(payload); err == nil {
			if out, err := json.Marshal(wire.ReadHistory{ConvID: sr.GetConvId(), SinceSeq: sr.GetSinceSeq(), Limit: sr.GetLimit()}); err == nil {
				return out
			}
		}
		return payload
	case kinds.KindReadHistory:
		var r chatpb.ReadHistory
		if err := r.Unmarshal(payload); err != nil {
			return payload
		}
		out, err := json.Marshal(wire.ReadHistory{
			ConvID:    r.GetConvId(),
			SinceSeq:  r.GetSinceSeq(),
			Limit:     r.GetLimit(),
			ColdMerge: r.GetColdMerge(),
		})
		if err != nil {
			return payload
		}
		return out
	}
	return payload
}

// convertChatPayload normalizes the uplink payload of 104/105/107/108/230/400
// into the pipe's wire JSON form. JSON passes through (the wire structs share
// the proto field names); protobuf is decoded into the chatpb message and
// re-encoded as wire JSON.
func convertChatPayload(kind uint32, payload []byte) []byte {
	if isJSONPayload(payload) {
		return payload // wire struct field names == proto names == client JSON
	}
	switch kind {
	case kinds.KindMsgRead, kinds.KindMsgDelivered:
		var m chatpb.MessagePayload
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		out := wire.ReceiptPayload{}
		for _, r := range m.GetReceipts() {
			out.Receipts = append(out.Receipts, wire.Receipt{
				MsgID: r.GetMsgId(), UserID: r.GetUserId(), At: r.GetAt(), Rtype: r.GetRtype(),
			})
		}
		if b, err := json.Marshal(out); err == nil {
			return b
		}
	case kinds.KindMsgDelete, kinds.KindMsgRecall:
		var m chatpb.MsgRecall
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		if b, err := json.Marshal(wire.MsgMutation{MsgID: m.GetMsgId(), ConvID: m.GetConvId()}); err == nil {
			return b
		}
	case kinds.KindTyping:
		var m chatpb.Typing
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		if b, err := json.Marshal(wire.TypingSignal{ConvID: m.GetConvId(), Typing: m.GetTyping(), At: time.Now().UnixMilli()}); err == nil {
			return b
		}
	case kinds.KindPresenceUpdate:
		var m chatpb.PresenceUpdate
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		status := "offline"
		switch m.GetStatus() {
		case 1:
			status = "online"
		case 2:
			status = "away"
		}
		if b, err := json.Marshal(wire.PresenceSignal{UserID: m.GetUserId(), Status: status, At: int64(m.GetAt())}); err == nil {
			return b
		}
	}
	return payload
}

// convertDomainV2Payload normalises the uplink payload of the FE-1a kinds
// (106 MSG_EDIT / 109 MSG_FORWARD / 453 REPORT) into the pipe's wire JSON
// form. JSON passes through (shared field names); protobuf is decoded and
// re-encoded snake_case.
func convertDomainV2Payload(kind uint32, payload []byte) []byte {
	if isJSONPayload(payload) {
		return payload
	}
	switch kind {
	case kinds.KindMsgEdit:
		var m chatpb.MsgEdit
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		if b, err := json.Marshal(wire.MsgEditPayload{MsgID: m.GetMsgId(), ConvID: m.GetConvId(), Text: m.GetText()}); err == nil {
			return b
		}
	case kinds.KindMsgForward:
		var m chatpb.MsgForward
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		if b, err := json.Marshal(wire.MsgForwardPayload{
			MsgID:       m.GetMsgId(),
			FromConvID:  m.GetFromConvId(),
			ToConvID:    m.GetToConvId(),
			ForwardFrom: wire.ForwardFrom{MsgID: m.GetMsgId(), ConvID: m.GetFromConvId()},
		}); err == nil {
			return b
		}
	case kinds.KindReport:
		var m chatpb.ReportPayload
		if err := m.Unmarshal(payload); err != nil {
			return payload
		}
		if b, err := json.Marshal(wire.ReportPayload{TargetUser: m.GetUserId(), Reason: m.GetReason(), Detail: m.GetDetail()}); err == nil {
			return b
		}
	}
	return payload
}

// convKindToPipe maps the user-facing conversation kinds onto the data
// layer's 930 CONV_DATA request lane:
//
//	200 CONV_CREATE   → op 1 create
//	201 CONV_UPDATE   → op 5 getMeta
//	210 MEMBER_ADD    → op 2 addMember
//	211 MEMBER_REMOVE → op 3 removeMember
//
// Both protobuf (native) and JSON (browser) payloads are accepted; the
// resulting body is always the pipe's wire.ConvPayload / wire.MemberOp JSON.
func convKindToPipe(env *chatpb.Envelope) (uint32, []byte, bool) {
	if env == nil {
		return 0, nil, false
	}
	switch env.Kind {
	case kinds.KindConvCreate:
		var title string
		var convType uint32
		var memberIDs []uint64
		if isJSONPayload(env.Payload) {
			var raw struct {
				Title          string   `json:"title"`
				ConvType       uint32   `json:"conv_type"`
				ConvTypeCamel  uint32   `json:"convType"`
				MemberIDs      []uint64 `json:"member_ids"`
				MemberIDsCamel []uint64 `json:"memberIds"`
			}
			if err := json.Unmarshal(env.Payload, &raw); err != nil {
				return 0, nil, false
			}
			title = raw.Title
			convType = raw.ConvType
			if convType == 0 {
				convType = raw.ConvTypeCamel
			}
			memberIDs = raw.MemberIDs
			if len(memberIDs) == 0 {
				memberIDs = raw.MemberIDsCamel
			}
		} else {
			var p chatpb.ConvCreate
			if err := p.Unmarshal(env.Payload); err != nil {
				return 0, nil, false
			}
			title, convType, memberIDs = p.GetTitle(), p.GetConvType(), p.GetMemberIds()
		}
		members := make([]wire.Member, 0, len(memberIDs))
		for _, uid := range memberIDs {
			members = append(members, wire.Member{UserID: uid})
		}
		body, err := wire.Marshal(wire.ConvPayload{
			ConvID: env.GetConvId(), Title: title, ConvType: convType, Members: members,
		})
		if err != nil {
			return 0, nil, false
		}
		return uint32(kinds.KindConvDataRequest), wrapConvReq(
			convOpCreate, env.GetConvId(), env.GetRequestId(), body), true

	case kinds.KindConvUpdate:
		cid := env.GetConvId()
		if isJSONPayload(env.Payload) {
			var raw struct {
				ConvID      uint64 `json:"conv_id"`
				ConvIDCamel uint64 `json:"convId"`
			}
			if err := json.Unmarshal(env.Payload, &raw); err == nil && cid == 0 {
				cid = raw.ConvID
				if cid == 0 {
					cid = raw.ConvIDCamel
				}
			}
		} else {
			var p chatpb.ConvUpdate
			if err := p.Unmarshal(env.Payload); err == nil && cid == 0 {
				cid = p.GetConvId()
			}
		}
		body, _ := wire.Marshal(wire.ConvPayload{ConvID: cid})
		return uint32(kinds.KindConvDataRequest), wrapConvReq(
			convOpGetMeta, cid, env.GetRequestId(), body), true

	case kinds.KindMemberAdd, kinds.KindMemberRemove:
		cid := env.GetConvId()
		var userIDs []uint64
		var role uint32
		if isJSONPayload(env.Payload) {
			var raw struct {
				ConvID       uint64   `json:"conv_id"`
				ConvIDCamel  uint64   `json:"convId"`
				UserIDs      []uint64 `json:"user_ids"`
				UserIDsCamel []uint64 `json:"userIds"`
				Role         uint32   `json:"role"`
			}
			if err := json.Unmarshal(env.Payload, &raw); err != nil {
				return 0, nil, false
			}
			if cid == 0 {
				cid = raw.ConvID
				if cid == 0 {
					cid = raw.ConvIDCamel
				}
			}
			userIDs, role = raw.UserIDs, raw.Role
			if len(userIDs) == 0 {
				userIDs = raw.UserIDsCamel
			}
		} else {
			var p chatpb.MemberOp
			if err := p.Unmarshal(env.Payload); err != nil {
				return 0, nil, false
			}
			if cid == 0 {
				cid = p.GetConvId()
			}
			userIDs, role = p.GetUserIds(), p.GetRole()
		}
		op := uint32(convOpAddMember)
		if env.Kind == kinds.KindMemberRemove {
			op = convOpRemoveMember
		}
		body, err := wire.Marshal(wire.MemberOp{ConvID: cid, UserIDs: userIDs, Role: role})
		if err != nil {
			return 0, nil, false
		}
		return uint32(kinds.KindConvDataRequest), wrapConvReq(op, cid, env.GetRequestId(), body), true
	}
	return 0, nil, false
}

const (
	convOpCreate       = 1
	convOpAddMember    = 2
	convOpRemoveMember = 3
	convOpGetMeta      = 5
)

type convDataReq struct {
	Op        uint32 `json:"op,omitempty"`
	ConvID    uint64 `json:"convId,omitempty"`
	Body      []byte `json:"body,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

func wrapConvReq(op uint32, cid uint64, requestID string, body []byte) []byte {
	out, _ := json.Marshal(convDataReq{Op: op, ConvID: cid, Body: body, RequestID: requestID})
	return out
}

func isJSONPayload(b []byte) bool {
	return len(b) > 0 && b[0] == '{'
}

// installEmit installs the chatpb-backed outbound converter. SrcLayer is
// "data" (the verifier's capability table is keyed by it) and ConnID is left 0
// on purpose: the data layer does not know user connections, and a conn id from
// this process would collide with the gateway's own numbering.
func installEmit(p *pipe.Pipe) {
	p.SetEmit(func(kind uint32, requestID string, convID, userID uint64, payload []byte) *proto.Frame {
		env := &chatpb.Envelope{
			Kind:      kind,
			RequestId: requestID,
			Payload:   payload,
		}
		switch {
		case userID != 0:
			env.SetUserId(userID)
		case convID != 0:
			env.SetConvId(convID)
		}
		return &proto.Frame{
			Kind: kind,
			Env:  env,
			Meta: &proto.FrameMeta{SrcLayer: "data", TraceID: requestID},
		}
	})
}

// buildRoutes starts from the bridge defaults and fills the data-plane
// precision routes the default table does not carry:
//
//	client → data : requests the data layer executes
//	data → user   : results addressed back to one user
//
// Without these, kind 101 falls into the 100-499 range rule (→ user layer) and
// an ack with both conv_id and user_id would be delivered twice.
func buildRoutes() map[uint32][]router.Target {
	rt := router.DefaultTable()
	toData := func(topic string) []router.Target {
		return []router.Target{{Layer: "data", Topic: topic}}
	}
	toUser := func(topic string) []router.Target {
		return []router.Target{{Layer: "user", Topic: topic}}
	}

	// Requests executed by the data layer.
	rt[kinds.KindMsgSend] = toData("data.101")
	// SYNC_REQUEST (8): gap recovery (断线补拉); dispatched to pipe.onSyncRequest.
	rt[kinds.KindSyncRequest] = toData("data.8")
	// Conversation user kinds are translated to 930 by the FrameSource.
	rt[kinds.KindConvCreate] = toData("data.conv.create")
	rt[kinds.KindConvUpdate] = toData("data.conv.update")
	rt[kinds.KindMemberAdd] = toData("data.conv.member.add")
	rt[kinds.KindMemberRemove] = toData("data.conv.member.remove")
	rt[kinds.KindReadHistory] = toData("data.920")
	rt[kinds.KindConvDataRequest] = toData("data.930")
	rt[kinds.KindIdentityRequest] = toData("data.940")
	rt[kinds.KindSecurityRequest] = toData("data.960")
	rt[kinds.KindFlagGet] = toData("data.970")
	rt[kinds.KindFlagSet] = toData("data.971")
	rt[kinds.KindJobTrigger] = toData("data.980")

	// Call signaling requests executed by the data layer (client → data).
	// The SAME kinds flow back (data → user) and are disambiguated by source
	// layer via buildRoutesFromData; see routeFromData below.
	rt[kinds.KindCallOffer] = toData("data.call")
	rt[kinds.KindCallAnswer] = toData("data.call")
	rt[kinds.KindCallCandidate] = toData("data.call")
	rt[kinds.KindCallBusy] = toData("data.call")
	rt[kinds.KindCallHangup] = toData("data.call")

	// Transient signaling + recall requests executed by the data layer.
	rt[kinds.KindMsgRead] = toData("data.receipt")
	rt[kinds.KindMsgDelivered] = toData("data.receipt")
	rt[kinds.KindTyping] = toData("data.typing")
	rt[kinds.KindPresenceUpdate] = toData("data.presence")
	// 401/402: the USER LAYER emits connect/disconnect events addressed by
	// user_id; they ride the data lane so the pipe fans them out to peers.
	rt[kinds.KindPresenceOnline] = toData("data.presence")
	rt[kinds.KindPresenceOffline] = toData("data.presence")
	rt[kinds.KindMsgDelete] = toData("data.recall")
	rt[kinds.KindMsgRecall] = toData("data.recall")

	// Results addressed to a single user (delivery in the gateway is by uid).
	rt[kinds.KindMsgAck] = toUser("sys.ack")
	rt[kinds.KindError] = toUser("sys.error")
	rt[kinds.KindReadHistoryResult] = toUser("sys.data.result")
	rt[kinds.KindConvDataResult] = toUser("sys.data.result")
	// Membership-change events: one frame per member, addressed by user_id.
	rt[kinds.KindConvPayload] = toUser("sys.conv.payload")
	// 14 WATERMARK（v1.2 拉取模式）：数据层生成、按 user_id 逐成员下发的水位线。
	// 它落在 0-99 系统层，默认路由表不会把它送到 user 层，必须显式登记，
	// 否则帧在 bridge 里被判 "no route" 静默丢弃（事故根因 #2 同类）。
	rt[kinds.KindWatermark] = toUser("sys.watermark")
	rt[kinds.KindIdentityResult] = toUser("sys.data.result")
	rt[kinds.KindFlagResult] = toUser("sys.data.result")
	rt[kinds.KindJobStatus] = toData("data.981")

	// 910 SHUTDOWN_NOTICE: the default table also targets "admin"; embedded
	// attaches no admin frame layer (management speaks HTTP AdminAPI), so keep
	// only the peers that exist.
	rt[kinds.KindShutdownNotice] = []router.Target{
		{Layer: "user", Topic: "sys.broadcast"},
		{Layer: "data", Topic: "data.910"},
	}

	// Feature-flag fan-out. Only the user layer is attached in embedded mode
	// (the management plane speaks HTTP AdminAPI, not the frame bus), so a
	// second "admin" target would just log "no such layer" on every publish.
	rt[kinds.KindFeatureFlags] = toUser("sys.broadcast")
	return rt
}

// buildRoutesFromData is the source-layer=data override table. Call signaling
// reuses the same kinds on the way back, so the router must send data-origin
// frames of 350-354 to the USER layer (by user_id), never back to data.
func buildRoutesFromData() map[uint32][]router.Target {
	toUser := func(topic string) []router.Target {
		return []router.Target{{Layer: "user", Topic: topic}}
	}
	return map[uint32][]router.Target{
		kinds.KindCallOffer:     toUser("sys.call"),
		kinds.KindCallAnswer:    toUser("sys.call"),
		kinds.KindCallCandidate: toUser("sys.call"),
		kinds.KindCallBusy:      toUser("sys.call"),
		kinds.KindCallHangup:    toUser("sys.call"),
		// Transient fan-outs the data layer originates after processing an
		// uplink (receipts, typing, presence, recall broadcasts). Without
		// these entries the main table routes them BACK to data (same kinds
		// as the uplink) and the frame loops until dropped — the peer never
		// sees it. Same bug class as the call signaling loop, different kinds.
		kinds.KindMsgRead:      toUser("sys.receipt"),
		kinds.KindMsgDelivered: toUser("sys.receipt"),
		kinds.KindTyping:       toUser("sys.typing"),
		kinds.KindMsgDelete:    toUser("sys.mutation"),
		kinds.KindMsgRecall:    toUser("sys.mutation"),
		// FE-1a: 数据层产出的编辑/转发事件与 @ 提及定向通知下行（同 107/108
		// 回环类别：同一 kind 上行进 data，下行必须回 user，否则回环丢弃）。
		kinds.KindMsgEdit:         toUser("sys.mutation"),
		kinds.KindMsgForward:      toUser("sys.mutation"),
		kinds.KindConvPayload:     toUser("sys.conv.payload"),
		kinds.KindPresenceUpdate:  toUser("sys.presence"),
		kinds.KindPresenceOnline:  toUser("sys.presence"),
		kinds.KindPresenceOffline: toUser("sys.presence"),
		// 14 WATERMARK 只由数据层产生（客户端上行不可能出现），显式登记让
		// 数据源层覆盖表也一致地指向 user 层，避免将来新增 14 号上行时回环。
		kinds.KindWatermark: toUser("sys.watermark"),
	}
}

// ---------------------------------------------------------------- keys

// isProduction reports whether the deployment must NOT auto-generate secrets.
// Production is defined as APP_ENV != "dev" AND an HTTPS public origin: a
// localhost dev run (APP_ENV=dev) or a plain http origin stays in the lenient
// first-boot convenience mode. In production a missing key file is a startup
// error (exit 1), never a silent auto-generation.
func isProduction(cfg *config) bool {
	if cfg == nil {
		return false
	}
	if strings.TrimSpace(os.Getenv("APP_ENV")) == "dev" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(cfg.PublicOrigin), "https://")
}

// loadOrCreateSigningKey reads an Ed25519 PKCS#8 PEM private key. When the file
// is absent a fresh key is generated and written 0600 (first boot convenience;
// production provisions it with deploy/keygen and never regenerates it).
// When prod is set, a missing file is a hard error instead of auto-generation.
func loadOrCreateSigningKey(path string, lg *log.Logger, prod bool) (ed25519.PrivateKey, error) {
	body, err := os.ReadFile(path)
	if err == nil {
		key, perr := parsePrivateKey(body)
		if perr != nil {
			return nil, fmt.Errorf("parse signing key %s: %w", path, perr)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read signing key %s: %w", path, err)
	}
	if prod {
		return nil, fmt.Errorf("signing key %s is missing in production: provision it with deploy/keygen (refusing to auto-generate a secret in production)", path)
	}
	_, priv, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		return nil, fmt.Errorf("generate signing key: %w", gerr)
	}
	der, merr := x509.MarshalPKCS8PrivateKey(priv)
	if merr != nil {
		return nil, fmt.Errorf("marshal signing key: %w", merr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir key dir: %w", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("write signing key %s: %w", path, err)
	}
	if lg != nil {
		lg.Warn("generated a new Ed25519 signing key (provision one before production)", "path", path)
	}
	return priv, nil
}

func parsePrivateKey(body []byte) (ed25519.PrivateKey, error) {
	if block, _ := pem.Decode(body); block != nil {
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ed, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("not an Ed25519 private key")
		}
		return ed, nil
	}
	// Raw forms: 32-byte seed or 64-byte private key (optionally hex).
	trimmed := strings.TrimSpace(string(body))
	if raw, err := hex.DecodeString(trimmed); err == nil {
		switch len(raw) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(raw), nil
		case ed25519.PrivateKeySize:
			return ed25519.PrivateKey(raw), nil
		}
	}
	switch len(body) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(body), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(body), nil
	}
	return nil, errors.New("unsupported key format (want PKCS#8 PEM, 32-byte seed or 64-byte key)")
}

// loadOrCreateHMACKey reads the identifier index key (hex or raw). A missing
// file is generated and stored 0600 next to the JWT key. This key is separate
// from the signing key by design (security-spec §5.3: HMAC key != crypto key).
// When prod is set, a missing file is a hard error instead of auto-generation.
func loadOrCreateHMACKey(path string, lg *log.Logger, prod bool) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err == nil {
		key, perr := decodeKeyBytes(body)
		if perr != nil {
			return nil, fmt.Errorf("parse hmac key %s: %w", path, perr)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read hmac key %s: %w", path, err)
	}
	if prod {
		return nil, fmt.Errorf("hmac key %s is missing in production: provision it with deploy/keygen (refusing to auto-generate a secret in production)", path)
	}
	key := make([]byte, 32)
	if _, rerr := rand.Read(key); rerr != nil {
		return nil, fmt.Errorf("generate hmac key: %w", rerr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir key dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write hmac key %s: %w", path, err)
	}
	if lg != nil {
		lg.Warn("generated a new identifier HMAC key (provision one before production)", "path", path)
	}
	return key, nil
}

func decodeKeyBytes(body []byte) ([]byte, error) {
	if raw, err := hex.DecodeString(strings.TrimSpace(string(body))); err == nil && len(raw) >= 16 {
		return raw, nil
	}
	raw := []byte(strings.TrimRight(string(body), "\r\n"))
	if len(raw) < 16 {
		return nil, errors.New("key too short (want >= 16 bytes)")
	}
	// Normalize to 32 bytes so exported key files are interchangeable.
	sum := sha256.Sum256(raw)
	return sum[:], nil
}
