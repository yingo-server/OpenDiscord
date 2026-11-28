package prod

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// banRecorder decorates the dispatcher so that every ACCEPTED 960 SECURITY
// ban/unban command is mirrored into the hot identity_bans journal.
//
// Why here: service.UserService deliberately writes nothing (the data layer
// owns enforcement) — but a deployment that runs the management plane without
// the data layer would then have no local ban history at all, and GetUser
// (which reads identity_bans) would keep reporting "not banned". The mirror is
// append-only, best-effort and never fails the admin action: the frame is the
// authority, the row is the local record.
//
// Kick/recall frames pass through untouched.
type banRecorder struct {
	next  admin.Dispatcher
	users *Users
	lg    *log.Logger
}

// securityCommand is the 960 payload subset this decorator cares about.
type securityCommand struct {
	Op     string `json:"op"`
	UID    int64  `json:"uid"`
	Level  int    `json:"level"`
	Reason string `json:"reason"`
	Until  int64  `json:"until"`
	By     string `json:"by"`
}

// ParseSecurityCommand decodes a 960 payload. Pure function (tested).
func ParseSecurityCommand(payload []byte) (securityCommand, bool) {
	var cmd securityCommand
	if len(payload) == 0 {
		return cmd, false
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return cmd, false
	}
	cmd.Op = normalizeOp(cmd.Op)
	if cmd.Op == "" {
		return cmd, false
	}
	return cmd, true
}

// normalizeOp lower-cases and strips the "user." prefix the CLI may send
// (user.ban / user.unban are the audit action names).
func normalizeOp(op string) string {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "ban", "user.ban":
		return "ban"
	case "unban", "user.unban":
		return "unban"
	}
	return ""
}

// InjectFrame implements admin.Dispatcher.
func (b *banRecorder) InjectFrame(ctx context.Context, kind uint32, target bridgeproxy.Target, payload []byte) (bridgeproxy.Result, error) {
	res, err := b.next.InjectFrame(ctx, kind, target, payload)
	if err != nil {
		return res, err
	}
	if kind != kinds.KindSecurityRequest {
		return res, nil
	}
	cmd, ok := ParseSecurityCommand(payload)
	if !ok {
		return res, nil
	}
	uid := cmd.UID
	if uid <= 0 {
		uid = int64(target.UserID)
	}
	if uid <= 0 {
		return res, nil
	}
	// Detached, short deadline: the journal row must survive a request context
	// that is cancelled immediately after the bridge ack.
	jctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var werr error
	switch cmd.Op {
	case "ban":
		level := cmd.Level
		if level <= 0 {
			level = 1
		}
		werr = b.users.RecordBan(jctx, uid, level, cmd.Reason, cmd.Until, cmd.By)
	case "unban":
		werr = b.users.RecordBanLift(jctx, uid, cmd.Reason, cmd.By)
	}
	if werr != nil && b.lg != nil {
		b.lg.Warn("identity_bans mirror failed (frame already accepted)",
			"uid", uid, "op", cmd.Op, "err", werr.Error())
	}
	return res, nil
}
