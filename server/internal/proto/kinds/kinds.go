// Package kinds 是全系统 kind 数值的唯一权威定义。
//
// 约束：本包手写、不依赖生成代码，是代码中唯一直接引用 kind 数值的地方；
// 其余代码（Router 表、dispatch 等）一律 import 本包的常量或使用
// KindName/号段归属函数，禁止出现裸数字字面量（CI 评审强制）。
//
// 号段总表见 docs/architecture.md「统一 kind 号段」。
package kinds

// Kind 表示 Envelope.kind 的取值。全局唯一真相源是本包 + proto/chat.proto。
type Kind = uint32

// -------------------------------------------------------------------
// 0-99 系统层：连接生命周期 / 鉴权 / 同步 / 错误
// -------------------------------------------------------------------
const (
	KindHello        Kind = 1 // C→S 连接后首帧
	KindAuth         Kind = 2 // C→S 鉴权
	KindAuthResult   Kind = 3 // S→C 鉴权结果
	KindPing         Kind = 4
	KindPong         Kind = 5
	KindResume       Kind = 6 // 断线重连补拉(last_seq)
	KindResumeResult Kind = 7
	KindSyncRequest  Kind = 8 // SYNC 请求
	KindSyncResult   Kind = 9 // SYNC 应答
	KindError        Kind = 10
	KindKicked       Kind = 11 // 全设备踢下线+断连
	KindFeatureFlags Kind = 12 // 广播到 sys.> 的策略开关
	KindRateLimited  Kind = 13
	// 14 = WATERMARK（拉取模式水位线，v1.2；docs/pull-mode-spec-v1.2.md §1.1）。
	// 注：14 号曾在 v1.2 短暂分配给 KindResync（RESYNC 慢消费者降级提示），
	// 因客户端未实现对应 handler 已回滚；本次复用前客户端 handler（P1）已合入
	// 并有单测（T-CL-1…T-CL-8），事故根因 #2 的前置条件已满足。
	// 见 docs/perf-postmortem-v1.2-draft.md。
	KindWatermark Kind = 14

	// v3 消息总线（docs/v3-bus-design.md §4.1；15-19 为 0-99 系统层新增号段）。
	// 客户端 handler（P1）与常量同 PR 合入（铁律 2：双端同 PR）。
	KindMailboxAck    Kind = 15 // C→S 累积 ACK {cursor}
	KindMailboxPause  Kind = 16 // S→C 仅诊断
	KindMailboxResume Kind = 17 // C→S 续传 {cursor, limit?}

	// 18 = CLIENT_CAPS（旧客户端能力协商，lts-1.4-acceptance §B）。
	// C→S：客户端上报自身能力，payload = JSON {"pull_capable":bool}。
	// 服务端把能力记进连接注册表；universal_pull_enabled=true 时只有
	// pull_capable 的连接走水位线，无能力连接保持 inline 逐成员推（不漏消息）。
	// 18/19 属 15-19 空号段，不改 Envelope 任何字段号。
	KindClientCaps Kind = 18
)

// -------------------------------------------------------------------
// 100-199 消息层
// -------------------------------------------------------------------
const (
	KindMsgSend      Kind = 101
	KindMsgNew       Kind = 102
	KindMsgAck       Kind = 103
	KindMsgRead      Kind = 104 // 回执，payload 用 MessagePayload.receipts
	KindMsgDelivered Kind = 105
	KindMsgEdit      Kind = 106
	KindMsgDelete    Kind = 107
	KindMsgRecall    Kind = 108
	KindMsgForward   Kind = 109
	KindMsgSync      Kind = 110 // 消息层 SYNC（走系统层 8/9 的会话内细化）
	KindMsgSearch    Kind = 111
)

// -------------------------------------------------------------------
// 200-299 会话层
// -------------------------------------------------------------------
const (
	KindConvCreate   Kind = 200
	KindConvUpdate   Kind = 201
	KindConvPayload  Kind = 202 // CONV 通用应答/事件体
	KindMemberAdd    Kind = 210
	KindMemberRemove Kind = 211
	KindMemberUpdate Kind = 212
	KindRoleSet      Kind = 213
	KindMute         Kind = 220
	KindPin          Kind = 221
	KindTyping       Kind = 230
	KindDraft        Kind = 231
)

// -------------------------------------------------------------------
// 300-349 媒体层
// -------------------------------------------------------------------
const (
	KindMediaPresignRequest Kind = 300
	KindMediaPresignResult  Kind = 301
	KindMediaReady          Kind = 302
	KindMediaExpire         Kind = 303
	KindMediaThumb          Kind = 304
)

// -------------------------------------------------------------------
// 350-399 通话预留（flag 关闭，常量已定义但不启用）
// -------------------------------------------------------------------
const (
	KindCallOffer     Kind = 350
	KindCallAnswer    Kind = 351
	KindCallCandidate Kind = 352
	KindCallBusy      Kind = 353
	KindCallHangup    Kind = 354
)

// -------------------------------------------------------------------
// 400-449 presence
// -------------------------------------------------------------------
const (
	KindPresenceUpdate Kind = 400

	// 401/402 are SERVER-emitted CONNECTION-LIFECYCLE events: the user layer
	// broadcasts "peer went online" / "peer went offline" to the peers who
	// share a conversation. They are NOT client-initiated presence
	// subscriptions — the proto message PresenceSubscribe{user_ids} (client
	// subscribing to others' presence) is a DIFFERENT, currently-unused concept.
	KindPresenceOnline  Kind = 401
	KindPresenceOffline Kind = 402
)

// Legacy aliases for 401/402. The VALUES are unchanged (401/402); only the Go
// identifiers now carry the correct online/offline semantics. Kept so any
// out-of-tree reference to the old names still compiles.
const (
	KindPresenceSubscribe   Kind = KindPresenceOnline
	KindPresenceUnsubscribe Kind = KindPresenceOffline
)

// -------------------------------------------------------------------
// 450-499 用户层
// -------------------------------------------------------------------
const (
	KindProfileUpdate Kind = 450
	KindBlock         Kind = 451
	KindUnblock       Kind = 452
	KindReport        Kind = 453
)

// -------------------------------------------------------------------
// 500-599 管理下行（AdminAPI 把 HTTP 翻译成 5xx 帧注入 Bus）
// -------------------------------------------------------------------
const (
	KindAdminBroadcast Kind = 500
	KindAnnounce       Kind = 501
	KindMaintenance    Kind = 502
)

// -------------------------------------------------------------------
// 600-899 全预留（未来业务，禁止占用）
// -------------------------------------------------------------------

// -------------------------------------------------------------------
// 900-909 层间控制：HELLO/SUBSCRIBE/FRAME_ACK/HEARTBEAT（不进 Router/Bus）
// -------------------------------------------------------------------
const (
	KindInternalHello     Kind = 902
	KindInternalSubscribe Kind = 903
	KindInternalFrameAck  Kind = 904
	KindInternalHeartbeat Kind = 905
)

// -------------------------------------------------------------------
// 910-919 层间系统
// -------------------------------------------------------------------
const (
	KindShutdownNotice Kind = 910 // SIGTERM 优雅排空广播
	KindFlagSync       Kind = 911 // FEATURE_FLAGS + 写库 ack
	KindConfigUpdate   Kind = 912 // SIGHUP 白名单热重载广播
	KindTopology       Kind = 913 // bridge.internal 指标/拓扑帧
)

// -------------------------------------------------------------------
// 920-929 数据读（readrouter 热/冷/merge 透明路由）
// -------------------------------------------------------------------
const (
	KindReadHistory       Kind = 920
	KindReadHistoryResult Kind = 921
)

// -------------------------------------------------------------------
// 930-939 会话数据
// -------------------------------------------------------------------
const (
	KindConvDataRequest Kind = 930
	KindConvDataResult  Kind = 931
)

// -------------------------------------------------------------------
// 940-959 身份数据（identity 域 users/identities/devices）
// -------------------------------------------------------------------
const (
	KindIdentityRequest Kind = 940
	KindIdentityResult  Kind = 941
)

// -------------------------------------------------------------------
// 960-969 安全域（封禁/审计/举报数据操作）
// -------------------------------------------------------------------
const (
	KindSecurityRequest Kind = 960
	KindSecurityResult  Kind = 961
)

// -------------------------------------------------------------------
// 970-979 系统 flag（feature flag 读写，flag 即策略表）
// -------------------------------------------------------------------
const (
	KindFlagGet    Kind = 970
	KindFlagSet    Kind = 971
	KindFlagResult Kind = 972
)

// -------------------------------------------------------------------
// 980-989 运维作业（archive/backup/trim/audit-verify 触发与状态）
// -------------------------------------------------------------------
const (
	KindJobTrigger Kind = 980
	KindJobStatus  Kind = 981
)

// -------------------------------------------------------------------
// 990-999 诊断（环回压测帧）
// -------------------------------------------------------------------
const (
	KindDiagLoopback Kind = 990
)

// kindNames 由 init 注册，避免本文件出现两份真源。
var kindNames = map[Kind]string{}

func register(k Kind, name string) {
	kindNames[k] = name
}

func init() {
	register(KindHello, "HELLO")
	register(KindAuth, "AUTH")
	register(KindAuthResult, "AUTH_RESULT")
	register(KindPing, "PING")
	register(KindPong, "PONG")
	register(KindResume, "RESUME")
	register(KindResumeResult, "RESUME_RESULT")
	register(KindSyncRequest, "SYNC_REQUEST")
	register(KindSyncResult, "SYNC_RESULT")
	register(KindError, "ERROR")
	register(KindKicked, "KICKED")
	register(KindFeatureFlags, "FEATURE_FLAGS")
	register(KindRateLimited, "RATE_LIMITED")
	register(KindWatermark, "WATERMARK")
	register(KindMailboxAck, "MAILBOX_ACK")
	register(KindMailboxPause, "MAILBOX_PAUSE")
	register(KindMailboxResume, "MAILBOX_RESUME")
	register(KindClientCaps, "CLIENT_CAPS")

	register(KindMsgSend, "MSG_SEND")
	register(KindMsgNew, "MSG_NEW")
	register(KindMsgAck, "MSG_ACK")
	register(KindMsgRead, "MSG_READ")
	register(KindMsgDelivered, "MSG_DELIVERED")
	register(KindMsgEdit, "MSG_EDIT")
	register(KindMsgDelete, "MSG_DELETE")
	register(KindMsgRecall, "MSG_RECALL")
	register(KindMsgForward, "MSG_FORWARD")
	register(KindMsgSync, "MSG_SYNC")
	register(KindMsgSearch, "MSG_SEARCH")

	register(KindConvCreate, "CONV_CREATE")
	register(KindConvUpdate, "CONV_UPDATE")
	register(KindConvPayload, "CONV_PAYLOAD")
	register(KindMemberAdd, "MEMBER_ADD")
	register(KindMemberRemove, "MEMBER_REMOVE")
	register(KindMemberUpdate, "MEMBER_UPDATE")
	register(KindRoleSet, "ROLE_SET")
	register(KindMute, "MUTE")
	register(KindPin, "PIN")
	register(KindTyping, "TYPING")
	register(KindDraft, "DRAFT")

	register(KindMediaPresignRequest, "MEDIA_PRESIGN_REQUEST")
	register(KindMediaPresignResult, "MEDIA_PRESIGN_RESULT")
	register(KindMediaReady, "MEDIA_READY")
	register(KindMediaExpire, "MEDIA_EXPIRE")
	register(KindMediaThumb, "MEDIA_THUMB")

	register(KindCallOffer, "CALL_OFFER")
	register(KindCallAnswer, "CALL_ANSWER")
	register(KindCallCandidate, "CALL_CANDIDATE")
	register(KindCallBusy, "CALL_BUSY")
	register(KindCallHangup, "CALL_HANGUP")

	register(KindPresenceUpdate, "PRESENCE_UPDATE")
	// 401/402 = server-side online/offline events. The DISPLAY labels keep the
	// historical "SUBSCRIBE/UNSUBSCRIBE" strings (same map slot 401/402) so the
	// wire logs and docs/ws-protocol.md stay stable — the Go identifiers above
	// carry the authoritative semantics.
	register(KindPresenceOnline, "PRESENCE_ONLINE")
	register(KindPresenceOffline, "PRESENCE_OFFLINE")
	register(KindPresenceSubscribe, "PRESENCE_SUBSCRIBE")     // == PRESENCE_ONLINE (401)
	register(KindPresenceUnsubscribe, "PRESENCE_UNSUBSCRIBE") // == PRESENCE_OFFLINE (402)

	register(KindProfileUpdate, "PROFILE_UPDATE")
	register(KindBlock, "BLOCK")
	register(KindUnblock, "UNBLOCK")
	register(KindReport, "REPORT")

	register(KindAdminBroadcast, "ADMIN_BROADCAST")
	register(KindAnnounce, "ANNOUNCE")
	register(KindMaintenance, "MAINTENANCE")

	register(KindInternalHello, "INTERNAL_HELLO")
	register(KindInternalSubscribe, "INTERNAL_SUBSCRIBE")
	register(KindInternalFrameAck, "INTERNAL_FRAME_ACK")
	register(KindInternalHeartbeat, "INTERNAL_HEARTBEAT")

	register(KindShutdownNotice, "SHUTDOWN_NOTICE")
	register(KindFlagSync, "FLAG_SYNC")
	register(KindConfigUpdate, "CONFIG_UPDATE")
	register(KindTopology, "TOPOLOGY")

	register(KindReadHistory, "READ_HISTORY")
	register(KindReadHistoryResult, "READ_HISTORY_RESULT")

	register(KindConvDataRequest, "CONV_DATA_REQUEST")
	register(KindConvDataResult, "CONV_DATA_RESULT")

	register(KindIdentityRequest, "IDENTITY_REQUEST")
	register(KindIdentityResult, "IDENTITY_RESULT")

	register(KindSecurityRequest, "SECURITY_REQUEST")
	register(KindSecurityResult, "SECURITY_RESULT")

	register(KindFlagGet, "FLAG_GET")
	register(KindFlagSet, "FLAG_SET")
	register(KindFlagResult, "FLAG_RESULT")

	register(KindJobTrigger, "JOB_TRIGGER")
	register(KindJobStatus, "JOB_STATUS")

	register(KindDiagLoopback, "DIAG_LOOPBACK")
}

// KindName 返回 kind 的可读名（日志/metrics 用）；未知返回 "KIND_<n>"。
func KindName(k Kind) string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return "KIND_" + itoa(uint64(k))
}

// IsInternal 报告 k 是否为层间内部帧（>=900，不出 Bridge/内部 socket）。
func IsInternal(k Kind) bool { return k >= 900 }

// 号段归属。Layer 返回 kind 所属的协议层名；未定义号段返回 ""。
func Layer(k Kind) string {
	switch {
	case k < 100:
		return "system"
	case k < 200:
		return "message"
	case k < 300:
		return "conv"
	case k < 350:
		return "media"
	case k < 400:
		return "call"
	case k < 450:
		return "presence"
	case k < 500:
		return "user"
	case k < 600:
		return "admin"
	case k < 900:
		return "reserved"
	case k < 910:
		return "internal.control"
	case k < 920:
		return "internal.system"
	case k < 930:
		return "internal.read"
	case k < 940:
		return "internal.convdata"
	case k < 960:
		return "internal.identity"
	case k < 970:
		return "internal.security"
	case k < 980:
		return "internal.flags"
	case k < 990:
		return "internal.jobs"
	default:
		return "internal.diag"
	}
}

// IsUserFacing 报告 k 是否为面向前端 WS 的帧（已定义号段且 <900）。
func IsUserFacing(k Kind) bool { return k < 600 }

// itoa 避免为一次格式化 import strconv 之外的依赖（strconv 本身无害，这里内联保持零依赖风格一致）。
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
