// Package identity is the 940/941 IDENTITY_DATA domain handler
// (docs/admin-data-design.md §3.3: data/domain/{identity,conv,...}).
//
// Split out of data/pipe/conv.go (review P1-8): the pipe keeps the pipeline
// skeleton (dedup/seq/flush/read routing) and only dispatches; domain logic
// lives in domain packages. Identity is the least conv-coupled domain —
// user upsert/get and ban get/set over store.IdentityStore, plus identity
// cache invalidation.
//
// Authorization note: ops 1–3 are executed by the trusted data lane (the
// user-facing kinds never reach here directly); op 4 (SetBan) is invoked from
// the admin plane's 960 frame path. The store itself never re-checks.
package identity

import (
	"context"

	"github.com/yingo-server/UFOT/server/internal/data/cache"
	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// ops for the 940 IDENTITY_DATA lane.
const (
	OpUpsertUser  = 1
	OpGetUser     = 2
	OpGetBan      = 3
	OpSetBan      = 4
	// OpSearchUser returns public profiles matching a keyword. It exists so
	// a separately hosted client can pick a user to start a chat with.
	OpSearchUser = 5
)

// Domain serves kind 940 (IDENTITY_DATA_REQUEST) → 941 (IDENTITY_DATA_RESULT).
type Domain struct {
	id    store.IdentityStore
	cache *cache.Cache
}

// Result of one identity op (the 941 result body shape).
type Result struct {
	Op      uint32
	OK      bool
	Error   string
	Payload []byte
}

// New constructs the domain over the hot identity store + shared cache.
func New(id store.IdentityStore, c *cache.Cache) *Domain {
	return &Domain{id: id, cache: c}
}

// Handle dispatches one op. Behavior is identical to the former
// pipe.identityDomain.handle.
func (d *Domain) Handle(ctx context.Context, op uint32, uid uint64, body []byte) Result {
	switch op {
	case OpUpsertUser:
		var u store.User
		if err := wire.Unmarshal(body, &u); err != nil {
			return Result{Op: op, Error: "bad payload"}
		}
		if err := d.id.UpsertUser(ctx, &u); err != nil {
			return Result{Op: op, Error: err.Error()}
		}
		d.cache.InvalidateUser(cache.UserKey(uid))
		return Result{Op: op, OK: true}

	case OpGetUser:
		u, err := d.id.GetUser(ctx, int64(uid))
		if err != nil {
			return Result{Op: op, Error: err.Error()}
		}
		pl, _ := wire.Marshal(u)
		return Result{Op: op, OK: true, Payload: pl}

	case OpGetBan:
		b, err := d.id.GetBan(ctx, int64(uid))
		if err != nil {
			return Result{Op: op, Error: err.Error()}
		}
		var pl []byte
		if b != nil {
			pl, _ = wire.Marshal(b)
		}
		return Result{Op: op, OK: true, Payload: pl}

	case OpSetBan:
		var b store.Ban
		if err := wire.Unmarshal(body, &b); err != nil {
			return Result{Op: op, Error: "bad payload"}
		}
		b.UID = int64(uid)
		if err := d.id.SetBan(ctx, &b); err != nil {
			return Result{Op: op, Error: err.Error()}
		}
		// ban affects identity/profile cache lineage
		d.cache.InvalidateUser(cache.UserKey(uid))
		return Result{Op: op, OK: true}

	case OpSearchUser:
		// Payload shape: {"query":"...","limit":30} (JSON tags from the
		// wire convention). Only public columns leave the domain.
		var req struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := wire.Unmarshal(body, &req); err != nil {
			return Result{Op: op, Error: "bad payload"}
		}
		users, err := d.id.SearchUsers(ctx, req.Query, req.Limit)
		if err != nil {
			return Result{Op: op, Error: err.Error()}
		}
		pl, _ := wire.Marshal(struct {
			Items []store.User `json:"items,omitempty"`
		}{Items: users})
		return Result{Op: op, OK: true, Payload: pl}
	}
	return Result{Op: op, Error: "unknown op"}
}
