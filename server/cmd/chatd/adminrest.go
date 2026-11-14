package main

// adminrest.go: the management REST surface (docs/admin-data-design.md §8).
//
// Two loopback-only listeners, intentionally separate:
//
//	admin_listen      :7780  bridge AdminAPI  (POST /admin/inject)  ← frames
//	admin_rest_listen :7781  admin REST       (/api/v1/*)           ← HTTP+RBAC
//
// The REST layer never touches the frame bus directly: it goes through
// bridgeproxy → POST /admin/inject → bus, which keeps one injection path and
// one audit trail for every management action.
//
// Authentication between the two is HMAC (bridge/adminapi): the same key file
// authorises the injector and the REST layer's outbound proxy. Without it any
// local process could inject management frames.

import (
	"fmt"
	"path/filepath"

	"github.com/yingo-server/OpenDiscord/server/internal/admin"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/bridgeproxy"
	adminprod "github.com/yingo-server/OpenDiscord/server/internal/admin/prod"
	adminstore "github.com/yingo-server/OpenDiscord/server/internal/admin/store"
	"github.com/yingo-server/OpenDiscord/server/internal/bridge/adminapi"
	"github.com/yingo-server/OpenDiscord/server/internal/data/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/pg"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/sqlite"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
)

// adminKeyPath derives the layer-to-admin HMAC key location from the signing
// key directory, so a deployment only manages one keys directory.
func adminKeyPath(cfg *config) string {
	dir := filepath.Dir(cfg.JWTKeyPath)
	return filepath.Join(dir, "ipc_admin.key")
}

// newAdminREST builds the RBAC REST server. The dispatcher is the same
// bridgeproxy used by production admin code; cold may be nil (degraded mode is
// explicit and logged, never silently faked).
func newAdminREST(cfg *config, hot *sqlite.DB, adminDB *adminstore.DB, cold *pg.ColdDB,
	pol *policy.Store, key []byte, lg *log.Logger) (*admin.Server, error) {

	proxy, err := bridgeproxy.New(bridgeproxy.Config{
		BaseURL:    "http://" + cfg.AdminListen,
		Authorizer: adminapi.NewHMACSigner(key),
	})
	if err != nil {
		return nil, fmt.Errorf("chatd: admin bridge proxy: %w", err)
	}

	srv, err := adminprod.NewAdminServer(
		admin.Config{
			ListenAddr: cfg.AdminRESTListen,
			TurnURL:    cfg.TurnURL,
		},
		hot.DB,
		adminDB.DB,
		cold,
		proxy,
		pol,
		lg,
	)
	if err != nil {
		return nil, fmt.Errorf("chatd: admin REST: %w", err)
	}
	return srv, nil
}
