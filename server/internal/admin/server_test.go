package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	adminstore "github.com/yingo-server/UFOT/server/internal/admin/store"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

type nopJournal struct{}

func (nopJournal) Append(context.Context, store.AuditEvent) error { return nil }

func TestNewMigratesAndServesHealth(t *testing.T) {
	db, err := adminstore.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bridge, err := bridgeproxy.New(bridgeproxy.Config{BaseURL: "http://127.0.0.1:1", Retries: -1})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := admin.New(admin.Config{ListenAddr: "127.0.0.1:0"}, admin.Deps{
		DB: db.DB, AuditJournal: nopJournal{}, Bridge: bridge,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("whoami without auth code=%d, want 401", rec.Code)
	}
	// Admin migration really ran: the DDL table is queryable.
	if _, err := adminstore.New(db.DB).GetAdminByUsername(context.Background(), "nobody"); err != store.ErrNotFound {
		t.Fatalf("GetAdminByUsername err = %v, want ErrNotFound", err)
	}
}

func TestNewRejectsPublicListen(t *testing.T) {
	db, err := adminstore.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bridge, _ := bridgeproxy.New(bridgeproxy.Config{BaseURL: "http://127.0.0.1:1", Retries: -1})
	if _, err := admin.New(admin.Config{ListenAddr: "0.0.0.0:7781"}, admin.Deps{
		DB: db.DB, AuditJournal: nopJournal{}, Bridge: bridge,
	}); err == nil {
		t.Fatal("public listen address accepted without AllowNonLoopback")
	}
}

func TestNewRequiresDeps(t *testing.T) {
	if _, err := admin.New(admin.Config{}, admin.Deps{}); err == nil {
		t.Fatal("New accepted missing DB")
	}
}
