package pipe

// env is the pipe's outbound reply representation.
//
// IMPORTANT (assembly-layer contract): the canonical wire envelope is
// chatpb.Envelope produced by protobuf codegen. That generated package is
// not present in this build environment (no protoc; see wire/wire.go), so the
// pipe emits this struct. The ASSEMBLY layer (batch-3 cmd/*) installs
// Pipe.Emit to convert env → *proto.Frame with a real chatpb.Envelope before
// Peer.Send; the default Emit builds a Kind-only shell. The pipe's logic
// (sequencing, dedup, flush, ack, read routing) is independent of the codec.
type env struct {
	Kind      uint32
	RequestID string
	ConvID    uint64
	UserID    uint64
	Payload   []byte

	// isReply marks ACK/error/result frames: they take the reply lane's
	// replySendMu in sendOne (fan-out frames take no pipe-level lock at all,
	// perf-spec 补充模块 B). The sender goroutine sets it on every env it
	// forwards from outCh.
	isReply bool

	// base / shared are set only for fan-out frames (perf-spec §2.1). base is
	// the recipient-independent encoded envelope body (kind + request_id +
	// payload), serialised ONCE per message and shared by every member's env.
	// shared reference-counts how many members still hold it. Ordinary reply
	// envs leave both nil and take the classic per-frame Encode path.
	base   []byte
	shared *sharedFrame
}
