// Package store defines the per-domain storage interfaces (docs §3.3).
//
// Rule of the house: handler 中出现 SQL 字符串即打回 — store 包外无 SQL.
// Interfaces are deliberately small and per-domain so that components like the
// archiver depend only on MsgReader + ArchiveTarget, and swapping the hot
// store implementation (or its schema) touches no other component.
//
// Ownership of the authorization red line: EVERY method here assumes the
// caller has already enforced membership/permission (dispatcher does
// cache.member.Check before anything enters the write pipeline). Store methods
// are NOT authorization boundaries and must never become ones.
package store

import "context"

// ---------------------------------------------------------------- msg domain

// PendingMsg is a message that has passed dedupguard and been assigned its seq
// range by seqalloc, awaiting flusher's group commit. Fields align 1:1 with
// msg_messages_hot columns.
type PendingMsg struct {
	MsgID       int64 // snowflake, assigned before flush
	Cid         int64
	Seq         int64
	SenderID    int64
	ClientMsgID string // dedup key; UNIQUE(cid,sender_id,client_msg_id)
	Content     []byte // Envelope payload verbatim; store never parses it
	SentAt      int64
	// Recalled mirrors msg_messages_hot.recalled: 0 = not recalled, else the
	// unix-second timestamp of the recall. The live downlink carries it so a
	// member that was offline during the recall still sees the tombstone.
	Recalled int64
}

// HotMsg is a message row read from the hot store. Content is opaque bytes;
// interpretation belongs to the envelope layer.
type HotMsg struct {
	MsgID       int64
	Cid         int64
	Seq         int64
	SenderID    int64
	ClientMsgID string
	Content     []byte
	SentAt      int64
	Edited      int64
	Recalled    int64
}

// MsgWriter is the hot write path, used by flusher (group commit) and seqalloc.
//
// Red line: AppendMessages performs NO membership checks — dispatcher/cache
// checked membership before the batch entered the pipeline. It is a single
// transaction per batch: all rows or none, acks hang off the commit.
type MsgWriter interface {
	// AppendMessages inserts a batch in one transaction. Partial inserts are
	// rolled back. Idempotency conflicts (duplicate cid,sender,client_msg_id)
	// return the existing rows as a conflict error, not a hard failure.
	AppendMessages(ctx context.Context, batch []PendingMsg) error

	// AllocateSeq atomically reserves the next n sequence numbers for cid and
	// returns the inclusive start (range = [start, start+n-1]). Implemented as
	// UPDATE conv_seq ... RETURNING; must be safe under concurrent writers.
	AllocateSeq(ctx context.Context, cid int64, n int) (start int64, err error)

	// MarkRecalled soft-deletes one hot message: sets recalled = at (unix sec)
	// when the (cid, msg_id, sender_id) row exists and is not already
	// recalled. Returns the matched row (or ErrNotFound). Authorization and
	// the time window are domain logic ABOVE the store — this is mechanism.
	MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*HotMsg, error)

	// MarkEdited soft-updates one hot message's content (FE-1a #1): rewrites
	// content and sets edited = at (unix sec) when the (cid, msg_id,
	// sender_id) row exists and is not recalled/already-edited-guarded by the
	// caller. Returns the matched row (or ErrNotFound). Authorization (sender
	// himself) and the time window are domain logic ABOVE the store.
	MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*HotMsg, error)
}

// MsgReader is the hot read path, used by readrouter and archiver.
type MsgReader interface {
	// ReadHot returns up to limit messages of cid ordered by seq.
	// asc=false (typical paging): rows with seq < beforeSeq, descending.
	// asc=true: rows with seq > beforeSeq, ascending (catch-up sync).
	// beforeSeq <= 0 means "no cursor" (from the respective end).
	ReadHot(ctx context.Context, cid int64, beforeSeq int64, limit int, asc bool) ([]HotMsg, error)

	// FirstSeqAfter returns the smallest seq in cid strictly greater than
	// since, or 0 when no such row exists. Used to locate archive/dedup
	// resume points.
	FirstSeqAfter(ctx context.Context, cid int64, since int64) (int64, error)
}

// ------------------------------------------------------------ identity domain

// User mirrors identity_users. Status: 0 normal, 1 banned, 2 deletion
// cooling-off, 3 deleted.
type User struct {
	UID           int64
	DisplayName   string
	AvatarMediaID int64
	Status        int
	CreatedAt     int64
	AnonymousAt   int64 // 0 = not anonymized (NULL in DDL; int64 zero sentinel)
}

// Identity mirrors identity_identities; IdValue is the HMAC index value.
type Identity struct {
	IDType    int
	IDValue   string
	UID       int64
	Verified  bool
	CreatedAt int64
}

// Ban mirrors identity_bans. Until=0 means permanent. Level tiers are policy,
// never hardcoded here.
type Ban struct {
	UID       int64
	Level     int
	Reason    string
	Until     int64
	CreatedBy string
	CreatedAt int64
}

// IdentityStore is the identity domain's storage surface.
//
// Red line: lookup by identity is by HMAC'd IdValue ONLY — implementations
// must never accept or store raw identifiers. SetBan trusts the caller for
// authorization (admin chain + RBAC); it only records.
type IdentityStore interface {
	// UpsertUser creates or refreshes a user profile row.
	UpsertUser(ctx context.Context, u *User) error

	// LookupByIdentity resolves (idType, hmacValue) to a uid. Returns a
	// not-found error when absent.
	LookupByIdentity(ctx context.Context, idType int, idValue string) (int64, error)

	// GetUser fetches one user profile; not-found error when absent.
	GetUser(ctx context.Context, uid int64) (*User, error)

	// GetBan returns the currently effective ban for uid, or nil when the
	// user is not banned. Expired bans are treated as absent (and may be
	// lazily ignored — callers re-check Until against now).
	GetBan(ctx context.Context, uid int64) (*Ban, error)

	// SetBan records (or, with Level<=0, lifts) a ban record. Append-style:
	// history rows are kept, effectiveness is derived by expiry.
	SetBan(ctx context.Context, b *Ban) error
}

// --------------------------------------------------------------- conv domain

// ConvMeta mirrors conv_conversations. CType: 1 direct, 2 group.
type ConvMeta struct {
	Cid           int64
	CType         int
	Title         string
	AvatarMediaID int64
	CreatedBy     int64
	CreatedAt     int64
}

// Member mirrors conv_members. Role: 0 member, 1 admin, 2 owner.
type Member struct {
	Cid        int64
	UID        int64
	Role       int
	JoinedAt   int64
	MutedUntil int64
}

// ConvStore is the conversation domain's storage surface.
//
// Red line: CheckMember is the primitive the dispatcher's cache is primed
// from; it answers "is uid in cid" and NOTHING more — mute/role enforcement is
// domain logic above the store. RemoveMember/SetMemberRole assume the caller
// proved actor authority (role checks live in domain/conv).
type ConvStore interface {
	// CreateConv inserts a conversation and its creator as owner member,
	// atomically. Returns the allocated cid.
	CreateConv(ctx context.Context, m *ConvMeta, creatorUID int64) (cid int64, err error)

	// AddMember inserts a membership row; idempotent (already-member is a
	// no-op success).
	AddMember(ctx context.Context, m *Member) error

	// RemoveMember deletes a membership row; removing a non-member succeeds
	// (idempotent). Removing the last owner is rejected by domain logic, not here.
	RemoveMember(ctx context.Context, cid, uid int64) error

	// CheckMember reports membership existence (and current role/mute state).
	CheckMember(ctx context.Context, cid, uid int64) (*Member, error)

	// ListMembers returns all members of cid.
	ListMembers(ctx context.Context, cid int64) ([]Member, error)

	// SetMemberRole updates one member's role. Authorization happened above.
	SetMemberRole(ctx context.Context, cid, uid int64, role int) error

	// GetConvMeta returns conversation metadata; not-found error when absent.
	GetConvMeta(ctx context.Context, cid int64) (*ConvMeta, error)

	// SingleChatBetween returns the cid of the direct (ctype=1) conversation
	// whose member set is exactly {uid1, uid2}, or ErrNotFound when none
	// exists. Read-only: it never creates anything.
	SingleChatBetween(ctx context.Context, uid1, uid2 int64) (int64, error)

	// NextSeq returns cid's next unconsumed sequence number WITHOUT reserving
	// it (peek). Range reservation belongs to MsgWriter.AllocateSeq; this
	// exists for readers/inspect tooling.
	NextSeq(ctx context.Context, cid int64) (int64, error)

	// MemberPeersOf returns the DEDUPLICATED set of other uids sharing at
	// least one conversation with uid — the recipient set for presence
	// fan-out (上线/下线广播给"该用户所有会话的成员去重集").
	MemberPeersOf(ctx context.Context, uid int64) ([]int64, error)
}

// -------------------------------------------------------------- media domain

// Media mirrors media_media. Status: 0 scanning, 1 usable, 2 violation.
type Media struct {
	MediaID    string // base64url, >= 22 chars
	UID        int64
	MType      int
	Name       string
	Size       int64
	Mime       string
	Width      int
	Height     int
	DurationMS int64
	Status     int
	SHA256     string
	CreatedAt  int64
	RefCount   int64
	// ThumbData is a tiny inline thumbnail: a base64 data-URI or bare base64
	// (≤8KB accepted). Waveform is a JSON array of 0-100 ints (≤32 elements)
	// used to render the audio scrubber without a second round-trip. Both are
	// metadata only — the media bytes themselves still live on disk.
	//
	// NEW (0003): ThumbData is LEGACY — new uploads keep the thumbnail in a
	// FILE (media_dir/thumbs/.../<sha>.thumb) and ThumbPath carries its path
	// (tens of bytes instead of ~10.7KB of TEXT per row). ThumbData is still
	// populated by readers as a FALLBACK for existing rows whose file was
	// never written; writers must write the file and set ThumbPath = "".
	ThumbData string
	// ThumbPath is the on-disk thumbnail path (relative to media_dir) when the
	// thumbnail is stored as a file. Empty for legacy rows.
	ThumbPath string
	Waveform  string
}

// MediaStore is the media domain's storage surface.
//
// Red line: callers must treat Status!=1 media as unusable; serving a file
// whose scan is pending or failed is a review-level violation. RefCount
// changes and file lifecycle (GC of zero-ref media) are separate mechanisms.
type MediaStore interface {
	// Insert registers a new media record (status 0 by convention).
	Insert(ctx context.Context, m *Media) error

	// Get fetches one media record; not-found error when absent.
	Get(ctx context.Context, mediaID string) (*Media, error)

	// UpdateStatus transitions scan state (0->1 usable, ->2 violation).
	UpdateStatus(ctx context.Context, mediaID string, status int) error

	// IncRefCount adjusts the reference count by delta (may be negative).
	IncRefCount(ctx context.Context, mediaID string, delta int64) error

	// AddRef is shorthand for IncRefCount(+1) after a message referencing the
	// media commits; implementations may assert existence.
	AddRef(ctx context.Context, mediaID string) error
}

// MediaQuota is the quota accounting surface (SUM(size) over media_media).
// The LIMITS themselves are policy/config (mediasvc), never store logic.
type MediaQuota interface {
	// UsageOf returns the total stored bytes owned by uid.
	UsageOf(ctx context.Context, uid int64) (int64, error)

	// TotalUsage returns the total stored bytes across all users.
	TotalUsage(ctx context.Context) (int64, error)
}

// ---------------------------------------------------------------- sys domain

// Presence mirrors sys_presence.
type Presence struct {
	UID      int64
	Online   bool
	LastSeen int64
}

// Flag mirrors sys_feature_flags (972 flag chain = policy-as-flag storage).
type Flag struct {
	Key     string
	Value   string // JSON
	Version int
}

// RateWindow is a fixed-window counter result.
type RateWindow struct {
	Subject      string
	Action       string
	WindowStart  int64
	Count        int64
	WindowLength int64 // seconds, supplied by caller (policy)
}

// Penalty mirrors sys_rate_limit_penalty (e.g. subject-wide cooldowns).
type Penalty struct {
	Subject string
	Penalty string // e.g. "msg", "login"
	Until   int64
}

// SysStore is the system domain's storage surface.
//
// Red line: RateLimitIncr is ONLY a counter primitive. Whether a count is
// "too many" is decided by the caller reading policy — never by the store.
// ------------------------------------------------------------ reports domain

// ReportStore is the sys_reports write sink (FE-1a #4). Implemented by the
// hot SQLite DB; the admin plane reads the same table.
type ReportStore interface {
	// InsertReport appends one pending report row. reason is an enum,
	// detail is a bounded, caller-sanitised string (ids/enum only — no PII).
	InsertReport(ctx context.Context, reporter, targetMsg, targetUser int64, reason int, detail string, at int64) error
}

type SysStore interface {
	// UpsertFlag stores a feature flag / policy-flag document, bumping its
	// version for incremental frame fan-out.
	UpsertFlag(ctx context.Context, f *Flag) error

	// AllFlags returns every flag, for bridge cold start.
	AllFlags(ctx context.Context) ([]Flag, error)

	// UpsertPresence refreshes a user's presence row (upsert by uid).
	UpsertPresence(ctx context.Context, p *Presence) error

	// GetPresence returns a presence row; zero-value result (offline) when
	// the uid has never been seen is acceptable.
	GetPresence(ctx context.Context, uid int64) (*Presence, error)

	// RateLimitIncr atomically increments the (subject, action, windowStart)
	// bucket and returns the post-increment state. Limit evaluation is the
	// caller's job, driven by policy.
	RateLimitIncr(ctx context.Context, subject, action string, windowStart int64) (*RateWindow, error)

	// GetPenalty returns the active penalty for (subject, penalty) or nil.
	GetPenalty(ctx context.Context, subject, penalty string) (*Penalty, error)

	// SetPenalty upserts (or, with Until<=now semantics chosen by caller,
	// replaces) a penalty row. Durations/levels are policy, not store logic.
	SetPenalty(ctx context.Context, p *Penalty) error
}

// ------------------------------------------------------- archive (cold) source

// ArchiveSource is the COLD side of the archiver: hot rows are copied into
// it. The archiver knows ONLY this interface plus the hot-side ports — swap
// the cold schema, swap one impl.
//
// Red line: an archive source NEVER deletes hot rows. The hot side is
// deleted by a HotPruner (the hot store), after the copy committed.
type ArchiveSource interface {
	// CopyBatch inserts rows into the cold archive idempotently
	// (ON CONFLICT DO NOTHING semantics); returns how many rows were newly
	// moved. Retries must be safe: re-copying a batch must not duplicate.
	CopyBatch(ctx context.Context, rows []HotMsg) (int64, error)
}

// HotPruner is the HOT side of the archiver: it deletes rows from the hot
// store. Only the hot store can implement this — the hot messages live in
// SQLite, so a cold (Postgres) handle must NOT expose a delete-hot method.
//
// The ordering is the archiver's contract: prune only after the copy of
// exactly those rows COMMITTED.
type HotPruner interface {
	// DeleteHotBefore removes hot rows whose sent_at is strictly older than
	// the cutoff; returns the number of rows removed.
	DeleteHotBefore(ctx context.Context, before int64) (int64, error)
}

// ------------------------------------------------------------- trim (expiry)

// TrimTarget deletes expired rows from ONE whitelisted table. The table name
// is a KEY, never SQL text: implementations map it to a fixed statement
// (like orderSQL does for ORDER BY) and reject anything unknown.
type TrimTarget interface {
	// TrimTable deletes rows of the named table that expired before the
	// cutoff; returns the number of rows removed.
	TrimTable(ctx context.Context, table string, olderThan int64) (int64, error)
}

// ------------------------------------------------------------ audit (cold)

// AuditEvent is one append to the audit journal. PrevHash/RecordHash chain
// integrity is computed by the implementation; the caller supplies the
// semantic fields only.
type AuditEvent struct {
	Actor   string // admin/bridge actor id
	Action  string // e.g. "user.ban"
	Subject string // affected resource, e.g. "user:123"
	Detail  []byte // JSONB payload
	At      int64
}

// AuditJournal is the SINGLE write entry point for the audit trail
// (sys.audit_log, INSERT-only + hash chain). Audit reads are NOT here —
// reading is a cursor stream over the log (journalctl semantics), served by
// the admin query layer.
//
// Red line: implementations must never expose UPDATE/DELETE on audit rows.
type AuditJournal interface {
	Append(ctx context.Context, ev AuditEvent) error
}
