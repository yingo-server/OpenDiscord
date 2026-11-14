// Package router 是 usergw 的收发调度器（docs/user-frontend-design.md §1.1 router）：
//
//	上行：acceptor 的 Envelope → limiter 判定 → 本地白名单 or Wrap 内部帧 → forwarder
//	下行：forwarder 的内部帧 → Unwrap → 按 conn_id / user_id 定位 → session 投递
//
// 明确不做：不编解码（codec 负责）、不碰读写缓冲（acceptor/session 负责）、
// 不计算限流（limiter 负责）。
package router

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/session"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/bridge"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/limit"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// Session 是 router 依赖的连接表只读视图（测试用 fake session 实现）。
type Session interface {
	Lookup(connID uint64) session.Conn
	GetByUser(uid int64) []session.Conn
	Broadcast(uid int64, b []byte) int
}

// Router 上行/下行调度器。零值不可用，用 New。
type Router struct {
	sess   Session
	lim    *limit.Limiter
	pol    *policy.Policy
	fwd    *bridge.Forwarder
	flags  *featureflags.Flags
	logger *log.Logger

	// Metrics 可选钩子（nil 安全）。
	OnDrop    func(kind uint32, verdict limit.Verdict)
	OnForward func(kind uint32)
	OnDeliver func(kind uint32, connID uint64)
	OnKick    func(uid int64, reason string)

	// V3DirectEnabled 是双投递保护开关（docs/v3-bus-design.md §6 灰度切换，
	// universal-pull-spec §1.5「同一 uid 不允许两路径同时投递」）：
	// 返回 true = 允许旧逐成员直投；false = 该 uid 的连接已由 v3 deliverLoop
	// 从 mailbox 推送，本函数不得再写。nil = 恒允许（未装配 v3，行为与主干
	// 逐字节一致）。判定实现在装配层（chatd v3hooks.go，唯一路径真相源），
	// Router 只持有指针，不 import bus 包。
	V3DirectEnabled func(uid int64) bool

	// V3SkipCounter 是 v3 抑制的观测计数（双投递保护指标）：每跳过一次旧
	// 直投 +1。nil = 不计数（零开销）。
	V3SkipCounter func()

	// V3AckSink 是 kind 15 MAILBOX_ACK 上行处理回调（nil = 丢弃该帧，与
	// 未知 kind 同路径）：Router 解析 {cursor, ackedCount?} 后按连接定位
	// 转调 Plane.OnAck（→ hub.Ack → Store.MarkAcked，装配层闭包）。
	V3AckSink func(c session.Conn, cursor int64, ackedCount int)

	// CapSink 是旧客户端能力协商的登记回调（lts-1.4-acceptance §B）：
	// Router 在 HELLO(1) / CLIENT_CAPS(18) 上行帧里解析出 pull_capable
	// 后转调它，把能力记进连接注册表（session.SetPullCapable）。
	// nil = 不登记（等价于「所有客户端都是旧客户端」→ 一律 inline，
	// 与装配层未接入时的主干行为逐字节一致）。
	CapSink func(connID uint64, pullCapable bool)
}

// New 构造 router。
func New(sess Session, lim *limit.Limiter, pol *policy.Policy, fwd *bridge.Forwarder, flags *featureflags.Flags, logger *log.Logger) *Router {
	return &Router{sess: sess, lim: lim, pol: pol, fwd: fwd, flags: flags, logger: logger}
}

// UpEvent 是 router 的上行事件视图（acceptor 的 ws.FrameEvent 适配而来，
// 解耦测试与 net 层具体类型）。
type UpEvent struct {
	Conn   session.Conn
	Env    *chatpb.Envelope
	RecvAt time.Time
}

// RunUpstream 消费 acceptor 上行帧直到 ctx 取消。
func (r *Router) RunUpstream(ctx context.Context, upstream <-chan *ws.FrameEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-upstream:
			if ev == nil {
				continue
			}
			r.HandleUpstream(&UpEvent{Conn: ev.Conn, Env: ev.Env, RecvAt: ev.RecvAt})
		}
	}
}

// RunDownstream 消费桥接下行帧直到 ctx 取消，并监听重连 Resync。
func (r *Router) RunDownstream(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case fr := <-r.fwd.Downstream:
			r.HandleDownstream(fr)
		case <-r.fwd.Resync:
			r.onResync()
		}
	}
}

// HandleUpstream 处理一帧上行（纯函数式过滤/变换，无阻塞）。
func (r *Router) HandleUpstream(ev *UpEvent) {
	if ev == nil || ev.Conn == nil || ev.Env == nil {
		return
	}
	env := ev.Env
	kind := env.Kind
	bucketKind := kind
	if env.Flags&limit.PriorityFlag != 0 {
		bucketKind = limit.KindPriority
	}
	verdict := r.lim.Allow(ev.Conn.ID(), ev.Conn.UserID(), bucketKind, len(env.Payload))
	switch verdict {
	case limit.Drop:
		r.drop(kind, verdict)
		return
	case limit.Throttle:
		r.drop(kind, verdict)
		r.writeRateLimited(ev.Conn, kind)
		return
	case limit.Kill:
		r.drop(kind, verdict)
		if r.logger != nil {
			r.logger.Warn("rate limit kill", "uid", ev.Conn.UserID(), "conn", ev.Conn.ID(), "kind", kinds.KindName(kind))
		}
		_ = ev.Conn.Write(buildError(4290, "rate limit exceeded", 60000))
		_ = ev.Conn.Close()
		return
	}

	// 本地处理白名单（policy 数据驱动）。
	if r.isLocal(kind) {
		r.handleLocal(ev)
		return
	}
	r.forward(ev, env)
}

func (r *Router) isLocal(kind uint32) bool {
	if r.pol != nil {
		return r.pol.IsLocal(kind)
	}
	return policy.Default().LocalKinds[kind]
}

// handleLocal 处理白名单帧。
func (r *Router) handleLocal(ev *UpEvent) {
	env := ev.Env
	switch env.Kind {
	case kinds.KindPing:
		var ping chatpb.Ping
		if len(env.Payload) > 0 {
			_ = ping.Unmarshal(env.Payload)
		}
		out := &chatpb.Pong{ClientMs: ping.ClientMs, ServerMs: uint64(time.Now().UnixMilli())}
		payload, _ := out.Marshal()
		r.writeEnv(ev.Conn, &chatpb.Envelope{Kind: kinds.KindPong, AckId: env.Id, Payload: payload})
	case kinds.KindPong:
		// 客户端 pong：读超时已由 acceptor 的 pong handler 续期，这里无需处理。
	case kinds.KindHello:
		// 连接生命周期状态由 acceptor 持有；HELLO 只记录客户端版本 + 能力。
		if r.logger != nil {
			var hello chatpb.Hello
			_ = hello.Unmarshal(env.Payload)
			r.logger.Debug("hello", "conn", ev.Conn.ID(), "client", hello.ClientVersion, "device", ev.Conn.DeviceID())
		}
		// 旧客户端能力协商（§B）：HELLO 载荷里带 pull_capable / "pull_capable"
		// 能力串即视为「具备拉取能力」。缺省 false = 老客户端 → 保持 inline。
		r.recordCapability(ev.Conn, parseClientCaps(env.Payload))
	case kinds.KindClientCaps:
		// kind 18 CLIENT_CAPS（C→S）：客户端显式上报能力，payload =
		// JSON {"pull_capable":bool}（或 protobuf Hello）。纯记录，不回帧、
		// 不改变任何投递路径（投递判定在数据层 + universal_pull_enabled）。
		ok := parseClientCaps(env.Payload)
		r.recordCapability(ev.Conn, ok)
		if r.logger != nil {
			r.logger.Debug("client caps", "conn", ev.Conn.ID(), "pull_capable", ok)
		}
	case kinds.KindAuth:
		// upgrade 前已完成鉴权；帧内 AUTH 视为幂等确认。
		payload, _ := (&chatpb.AuthResult{Ok: true, UserId: uint64(ev.Conn.UserID())}).Marshal()
		r.writeEnv(ev.Conn, &chatpb.Envelope{Kind: kinds.KindAuthResult, AckId: env.Id, Payload: payload})
	case kinds.KindResume:
		// 补拉需要数据层 seq：本地不处理，转交桥接（RESUME 白名单只免限流）。
		r.forward(ev, env)
	case kinds.KindMailboxAck:
		// kind 15 MAILBOX_ACK（v3 §4.2 累积确认，C→S）：解析 {cursor,
		// ackedCount?} 后转调 V3AckSink（装配层闭包 → Plane.OnAck →
		// hub.Ack → Store.MarkAcked）。不新增 kind、不改 Envelope；
		// sink 为 nil（未装配 v3）时与未知 kind 同路径丢弃，零行为差异。
		if r.V3AckSink == nil {
			return
		}
		var raw struct {
			Cursor      uint64 `json:"cursor"`
			CursorSnake uint64 `json:"cursor_snake"`
			AckedCount  uint32 `json:"ackedCount"`
			AckedSnake  uint32 `json:"acked_count"`
		}
		if err := json.Unmarshal(env.Payload, &raw); err != nil {
			if r.logger != nil {
				r.logger.Debug("mailbox ack: bad payload", "conn", ev.Conn.ID())
			}
			return
		}
		cursor := raw.Cursor
		if cursor == 0 {
			cursor = raw.CursorSnake
		}
		n := int(raw.AckedCount)
		if n == 0 {
			n = int(raw.AckedSnake)
		}
		r.V3AckSink(ev.Conn, int64(cursor), n)
		return
	case kinds.KindFeatureFlags:
		// 订阅：直接回当前进程 flag 快照（写库 ack 由数据层负责）。
		flags := map[string]string{}
		version := uint32(1)
		if r.flags != nil {
			version = uint32(r.flags.Version())
			for k, v := range r.flags.Snapshot() {
				flags[k] = flagString(v)
			}
		}
		payload, _ := (&chatpb.FeatureFlags{Version: version, Flags: flags}).Marshal()
		r.writeEnv(ev.Conn, &chatpb.Envelope{Kind: kinds.KindFeatureFlags, AckId: env.Id, Payload: payload})
	case kinds.KindPresenceOnline, kinds.KindPresenceOffline:
		// 401/402 are USER-LAYER INTERNAL events (a connection opened/closed).
		// Accepting them from a client would let anyone forge another user's
		// online state (P0 in the security review), so they are refused here.
		r.writeEnv(ev.Conn, &chatpb.Envelope{Kind: kinds.KindError,
			Payload: mustMarshal(&chatpb.Error{Code: 4003, Reason: "presence events are server-originated"})})
	default:
		r.forward(ev, env)
	}
}

// recordCapability 把一条连接的能力登记到连接注册表（CapSink 为 nil 时
// 不做登记 —— 未装配 = 全部按旧客户端对待，行为与主干逐字节一致）。
func (r *Router) recordCapability(c session.Conn, pullCapable bool) {
	if r == nil || r.CapSink == nil || c == nil {
		return
	}
	r.CapSink(c.ID(), pullCapable)
}

// parseClientCaps 解析客户端能力上报，返回该连接是否具备拉取能力
// （pull_capable）。两种载荷形态都接受：
//
//	protobuf：chatpb.Hello{ pull_capable = 10 } 或 capabilities 里的
//	          "pull_capable" 能力串；
//	JSON：    {"pull_capable":bool} / {"pullCapable":bool} / {"capabilities":[...]}。
//
// 载荷以 '{' 开头按 JSON 解析，否则按 protobuf 解析。解析失败一律返回
// false（缺省 = 老客户端 → inline，宁可多发不漏消息）。
func parseClientCaps(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if payload[0] == '{' {
		var raw struct {
			PullCapable  *bool    `json:"pull_capable"`
			PullCapableC *bool    `json:"pullCapable"`
			Capabilities []string `json:"capabilities"`
		}
		if err := json.Unmarshal(payload, &raw); err != nil {
			return false
		}
		if raw.PullCapable != nil && *raw.PullCapable {
			return true
		}
		if raw.PullCapableC != nil && *raw.PullCapableC {
			return true
		}
		for _, c := range raw.Capabilities {
			if isPullCapabilityToken(c) {
				return true
			}
		}
		return false
	}
	var hello chatpb.Hello
	if err := hello.Unmarshal(payload); err != nil {
		return false
	}
	if hello.GetPullCapable() {
		return true
	}
	for _, c := range hello.GetCapabilities() {
		if isPullCapabilityToken(c) {
			return true
		}
	}
	return false
}

// isPullCapabilityToken 判定能力串是否表示「支持拉取模式（kind 14）」。
func isPullCapabilityToken(token string) bool {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "pull_capable", "pull-capable", "pull", "pull_mode", "pull-mode", "watermark":
		return true
	}
	return false
}

// mustMarshal is a tiny helper for locally built error payloads.
func mustMarshal(m interface{ Marshal() ([]byte, error) }) []byte {
	b, _ := m.Marshal()
	return b
}

// forward 把业务帧 Wrap 成内部帧交给 forwarder。
func (r *Router) forward(ev *UpEvent, env *chatpb.Envelope) {
	if r.fwd == nil {
		r.drop(env.Kind, limit.Drop)
		return
	}
	intf := frame.Wrap(env, traceID(env), ev.Conn.ID())
	intf.DstLayer = dstLayer(env.Kind)
	body, err := frame.MarshalInternal(intf)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("wrap internal frame failed", "kind", kinds.KindName(env.Kind), "err", err.Error())
		}
		r.drop(env.Kind, limit.Drop)
		return
	}
	if !r.fwd.Send(&bridge.UpstreamFrame{Kind: env.Kind, TraceID: traceID(env), ConnID: ev.Conn.ID(), Body: body}) {
		r.drop(env.Kind, limit.Drop)
		return
	}
	if r.OnForward != nil {
		r.OnForward(env.Kind)
	}
}

// HandleDownstream 处理一帧桥接下行。
func (r *Router) HandleDownstream(fr *proto.Frame) {
	if fr == nil || fr.Env == nil {
		return
	}
	env := fr.Env
	connID := uint64(0)
	if fr.Meta != nil {
		connID = fr.Meta.ConnID
	}
	// 下行优先按 InternalFrame 解包（bridge 统一封装路由头）。
	if intf, err := frame.UnmarshalInternal(env.Payload); err == nil && intf.Envelope != nil {
		env = intf.Envelope
		if intf.ConnId != 0 {
			connID = intf.ConnId
		}
	}

	if env.Kind == kinds.KindKicked {
		r.executeKicked(env)
		return
	}
	r.deliver(env, connID, fr.PreEncoded)
}

// deliver 定位并投递：显式 user_id 优先（数据面结果/事件按用户投递，
// 且 bridge 的 conn_id 属于来源层，不能当作 usergw 连接号使用）；
// 无 user_id 时再按 conn_id 精确定位。
func (r *Router) deliver(env *chatpb.Envelope, connID uint64, preEncoded []byte) {
	if env.Kind == 401 || env.Kind == 402 {
	}
	var b []byte
	if len(preEncoded) > 0 {
		b = appendEnvelopeTarget(preEncoded, env.GetConvId(), env.GetUserId())
		if len(b) > frame.MaxFrameBytes {
			return
		}
	} else {
		var err error
		b, err = frame.Encode(env)
		if err != nil {
			return
		}
	}
	// 双投递保护（v3 灰度切换）：灰度 uid 的连接已由 mailbox deliverLoop 推送
	// 数据帧，本函数跳过旧逐成员直投。判定在装配层（唯一路径真相源，flag
	// 快照 + InV3Bucket），nil 开关 = 未装配 v3 = 恒直投（逐字节主干行为）。
	//
	// 关键豁免：reply lane 的结果/ACK/控制帧（931 CONV_DATA_RESULT、103
	// MSG_ACK、10 ERROR、心跳、FEATURE_FLAGS 等）从不进 mailbox（它们不经
	// fanoutPlane.Offer），若一并抑制会全部丢失——客户端等不到 931/ACK 的
	// e2e 超时正源于此。只抑制「fanout 产出且已进 mailbox」的数据帧 kind，
	// 与 v3split.v3Eligible 对齐；其余一律直投。
	if r.V3DirectEnabled != nil && env.GetUserId() > 0 &&
		v3Suppressible(env.Kind) && !r.V3DirectEnabled(int64(env.GetUserId())) {
		if r.V3SkipCounter != nil {
			r.V3SkipCounter()
		}
		if r.logger != nil {
			r.logger.Debug("v3: legacy direct delivery suppressed", "uid", env.GetUserId(),
				"kind", kinds.KindName(env.Kind))
		}
		return
	}
	if uid := int64(env.GetUserId()); uid > 0 {
		if n := r.sess.Broadcast(uid, b); n > 0 {
			if r.OnDeliver != nil {
				r.OnDeliver(env.Kind, 0)
			}
			return
		}
	}
	if connID != 0 {
		if c := r.sess.Lookup(connID); c != nil {
			c.Write(b)
			if r.OnDeliver != nil {
				r.OnDeliver(env.Kind, connID)
			}
			return
		}
		// 连接已断（客户端可能已换新连接）：降级为按 user_id 广播。
	}
	if r.logger != nil {
		r.logger.Debug("downstream unroutable", "kind", kinds.KindName(env.Kind), "conn", connID)
	}
}

// executeKicked 执行 KICKED 策略：查 session → 写 Kicked 帧 → 断连。
func (r *Router) executeKicked(env *chatpb.Envelope) {
	uid := int64(env.GetUserId())
	if uid <= 0 {
		return
	}
	var kicked chatpb.Kicked
	if len(env.Payload) > 0 {
		_ = kicked.Unmarshal(env.Payload)
	}
	payload, _ := kicked.Marshal()
	frameBytes, err := frame.Encode(&chatpb.Envelope{Kind: kinds.KindKicked, Target: &chatpb.Envelope_UserId{UserId: uint64(uid)}, Payload: payload})
	if err != nil {
		return
	}
	conns := r.sess.GetByUser(uid)
	for _, c := range conns {
		c.Write(frameBytes)
		_ = c.Close()
	}
	if r.OnKick != nil {
		r.OnKick(uid, kicked.Reason)
	}
	if r.logger != nil {
		r.logger.Info("kicked", "uid", uid, "conns", len(conns), "reason", kicked.Reason)
	}
}

// onResync 桥接重连后：向全部连接下发 RESUME 补拉标记，让客户端按 last_seq 补齐缺口。
func (r *Router) onResync() {
	payload, _ := (&chatpb.Resume{}).Marshal()
	b, err := frame.Encode(&chatpb.Envelope{Kind: kinds.KindResume, Payload: payload})
	if err != nil {
		return
	}
	// Session 没有 All 方法时可退化为按已知 uid 广播；session 实现提供了 All。
	type aller interface{ All() []session.Conn }
	if a, ok := r.sess.(aller); ok {
		for _, c := range a.All() {
			c.Write(b)
		}
	}
	if r.logger != nil {
		r.logger.Info("bridge resynced, RESUME marks sent")
	}
}

func (r *Router) writeEnv(c session.Conn, env *chatpb.Envelope) {
	b, err := frame.Encode(env)
	if err != nil {
		return
	}
	c.Write(b)
}

func (r *Router) writeRateLimited(c session.Conn, kind uint32) {
	payload, _ := (&chatpb.RateLimited{Scope: "conn", Limit: 1, RetryAfterMs: 500}).Marshal()
	r.writeEnv(c, &chatpb.Envelope{Kind: kinds.KindRateLimited, Payload: payload})
}

func (r *Router) drop(kind uint32, v limit.Verdict) {
	if r.OnDrop != nil {
		r.OnDrop(kind, v)
	}
}

func buildError(code uint32, reason string, retryAfter uint32) []byte {
	payload, _ := (&chatpb.Error{Code: code, Reason: reason, RetryAfterMs: retryAfter}).Marshal()
	b, _ := frame.Encode(&chatpb.Envelope{Kind: kinds.KindError, Payload: payload})
	return b
}

func traceID(env *chatpb.Envelope) string {
	if env == nil {
		return ""
	}
	return env.RequestId
}

func dstLayer(kind uint32) string {
	if kinds.IsInternal(kind) {
		return "data"
	}
	return "data"
}

func flagString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// appendEnvelopeTarget materialises a per-recipient frame from the shared,
// recipient-independent body produced by the data layer fan-out (perf-spec
// §2.1). It copies the small shared body once and appends ONLY the outer target
// field (user_id or conv_id, a varint) — never re-serialising the payload.
//
// The resulting bytes are decode-identical to frame.Encode(env): protobuf
// decoders accept fields in any order, so emitting the target AFTER the payload
// (field 11/10 after field 15) decodes to the same Envelope. The target
// precedence mirrors installEmit: user_id wins, else conv_id.
func appendEnvelopeTarget(base []byte, convID, userID uint64) []byte {
	const (
		fieldConvID = 10
		fieldUserID = 11
	)
	switch {
	case userID != 0:
		out := make([]byte, len(base), len(base)+1+binary.MaxVarintLen64)
		copy(out, base)
		out = append(out, byte(fieldUserID<<3|0))
		return binary.AppendUvarint(out, userID)
	case convID != 0:
		out := make([]byte, len(base), len(base)+1+binary.MaxVarintLen64)
		copy(out, base)
		out = append(out, byte(fieldConvID<<3|0))
		return binary.AppendUvarint(out, convID)
	default:
		return base
	}
}

// v3Suppressible reports whether a downstream frame is produced by the fanout
// plane AND offered into the mailbox (v3split.v3Eligible). Only these are
// delivered by mailbox deliverLoop for greyed uids; reply-lane frames
// (results/ACKs/errors/heartbeats/feature flags) never enter the mailbox and
// must keep the legacy direct delivery, or the client never receives them.
func v3Suppressible(kind uint32) bool {
	switch kind {
	case uint32(kinds.KindMsgNew), // 102
		uint32(kinds.KindMsgRead),      // 104
		uint32(kinds.KindMsgDelivered), // 105
		uint32(kinds.KindMsgDelete),    // 107
		uint32(kinds.KindMsgRecall),    // 108
		uint32(kinds.KindWatermark),    // 14
		uint32(kinds.KindConvPayload),  // 202
		uint32(kinds.KindMemberUpdate):
		return true
	}
	return false
}
