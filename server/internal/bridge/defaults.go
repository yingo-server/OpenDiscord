// Package bridge 静态默认能力表（Router/Verifier 的启动期数据）。
// 单独成文件只为让装配文件 pipeline.go 保持 <300 行；不得在此写逻辑。
package bridge

import "github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"

func defaultCaps() map[string][]kinds.Kind {
	return map[string][]kinds.Kind{
		"user": allUserKinds(),
		"data": {
			kinds.KindReadHistory, kinds.KindConvDataRequest, kinds.KindIdentityRequest,
			kinds.KindSecurityRequest, kinds.KindFlagGet, kinds.KindJobTrigger,
			kinds.KindShutdownNotice, kinds.KindFlagSync, kinds.KindConfigUpdate,
			// Conversation user kinds are translated to 930 by chatd's
			// FrameSource. They are listed on the data layer too so any
			// direct/embedded frame source with SrcLayer=data passes the
			// verifier instead of being dropped as "outside capability".
			kinds.KindConvCreate, kinds.KindConvUpdate, kinds.KindMemberAdd, kinds.KindMemberRemove,
			kinds.KindMemberUpdate,
			// 数据层回程：提交后的 ack / 错误 / 读结果必须能上行到 bridge，
			// 否则 101 的 ack 会被 Verifier 以「能力外」丢弃。
			kinds.KindMsgAck, kinds.KindMsgNew, kinds.KindError,
			// 14 WATERMARK 是数据层生成的下行控制帧（拉取模式，v1.2 §1.1）：
			// 不在能力表里会被 Verifier 以「能力外」丢弃，客户端永远收不到。
			kinds.KindWatermark,
			kinds.KindReadHistoryResult, kinds.KindConvDataResult, kinds.KindConvPayload,
			kinds.KindIdentityResult, kinds.KindFlagResult, kinds.KindJobStatus,
			// 回执/输入中/presence/撤回：上行请求与下行事件共用这些 kind
			// （能力表按层不按方向区分；回程帧 SrcLayer=data 也须通过）。
			kinds.KindMsgRead, kinds.KindMsgDelivered, kinds.KindMsgDelete, kinds.KindMsgRecall,
			// FE-1a 消息域：106/109 上行请求与下行事件共用（能力表按层不按方向）。
			kinds.KindMsgEdit, kinds.KindMsgForward,
			kinds.KindTyping, kinds.KindPresenceUpdate, kinds.KindPresenceOnline, kinds.KindPresenceOffline,
			// 通话信令回程（data → user）：OFFER/ANSWER/CANDIDATE/BUSY/HANGUP。
			kinds.KindCallOffer, kinds.KindCallAnswer, kinds.KindCallCandidate,
			kinds.KindCallBusy, kinds.KindCallHangup,
		},
		"admin": {
			kinds.KindAdminBroadcast, kinds.KindAnnounce, kinds.KindMaintenance, kinds.KindTopology,
			// 管理层注入的 flag 重下发（SIGHUP）与 flag 写库 ack。
			kinds.KindFeatureFlags, kinds.KindFlagSync,
			// 管理指令域（docs/admin-data-design.md §1.2）：安全域 960、flag 970-972、
			// 作业 980-981。管理面唯一入口是 AdminAPI，缺这些 kind 会让封禁/审查/作业
			// 在 Verifier 处被"能力外"丢弃（实测：封禁帧被拒）。
			kinds.KindSecurityRequest, kinds.KindSecurityResult,
			kinds.KindFlagGet, kinds.KindFlagSet, kinds.KindFlagResult,
			kinds.KindJobTrigger, kinds.KindJobStatus,
		},
	}
}

func allUserKinds() []kinds.Kind {
	return []kinds.Kind{
		kinds.KindMsgSend, kinds.KindSyncRequest, kinds.KindMsgNew, kinds.KindMsgAck, kinds.KindMsgRead, kinds.KindMsgDelivered,
		// 14 WATERMARK：数据层 → 用户层 → 客户端的水位线下行（v1.2 §1.1）。
		kinds.KindWatermark,
		kinds.KindMsgEdit, kinds.KindMsgDelete, kinds.KindMsgRecall, kinds.KindMsgForward, kinds.KindMsgSync, kinds.KindMsgSearch,
		kinds.KindConvCreate, kinds.KindConvUpdate, kinds.KindConvPayload, kinds.KindMemberAdd, kinds.KindMemberRemove,
		kinds.KindMemberUpdate, kinds.KindRoleSet, kinds.KindMute, kinds.KindPin, kinds.KindTyping, kinds.KindDraft,
		kinds.KindConvDataResult,
		kinds.KindMediaPresignRequest, kinds.KindMediaReady, kinds.KindMediaExpire, kinds.KindMediaThumb,
		kinds.KindPresenceUpdate, kinds.KindPresenceOnline, kinds.KindPresenceOffline,
		kinds.KindProfileUpdate, kinds.KindBlock, kinds.KindUnblock, kinds.KindReport,
		kinds.KindAdminBroadcast, kinds.KindAnnounce, kinds.KindMaintenance,
		// FE-1a 下行到客户端：106/109 事件与 202 提及定向通知。
		kinds.KindMsgEdit, kinds.KindMsgForward, kinds.KindConvPayload,
		kinds.KindShutdownNotice, kinds.KindFlagSync, kinds.KindConfigUpdate,
		// 通话信令上行（client → data）：OFFER/ANSWER/CANDIDATE/BUSY/HANGUP。
		kinds.KindCallOffer, kinds.KindCallAnswer, kinds.KindCallCandidate,
		kinds.KindCallBusy, kinds.KindCallHangup,
	}
}
