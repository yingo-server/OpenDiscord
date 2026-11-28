package bridge

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/bridge/adminapi"
	"github.com/yingo-server/UFOT/server/internal/transport/embedded"
)

// The AdminAPI is the ONLY non-socket ingress. These tests pin the wiring
// between bridge.Config.AdminAuth and the handler returned by AdminAPI().

func adminInjectBody() string {
	return `{"kind":101,"conv_id":7,"payload_b64":"` +
		base64.StdEncoding.EncodeToString([]byte("hi")) + `"}`
}

func TestAdminAPINilAuthIsEmbeddedTrust(t *testing.T) {
	br := embedded.NewBridge(newStubLayer("data"))
	pl, err := Assemble(Config{}, br.Peer())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	pl.AdminAPI().Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/admin/inject", bytes.NewBufferString(adminInjectBody())))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("embedded trust code = %d body=%s, want 202", rec.Code, rec.Body.String())
	}
}

func TestAdminAPIRejectsUnsignedWhenAuthConfigured(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	br := embedded.NewBridge(newStubLayer("data"))
	pl, err := Assemble(Config{AdminAuth: adminapi.NewHMACAuthorizer([][]byte{key})}, br.Peer())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	pl.AdminAPI().Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/admin/inject", bytes.NewBufferString(adminInjectBody())))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned code = %d, want 401", rec.Code)
	}
}

func TestAdminAPIAcceptsSignedRequest(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	br := embedded.NewBridge(newStubLayer("data"))
	pl, err := Assemble(Config{
		AdminAuth: adminapi.NewHMACAuthorizer([][]byte{key}).SetClock(func() time.Time { return now }),
	}, br.Peer())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/inject", bytes.NewBufferString(adminInjectBody()))
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := "pipeline-test-nonce-1"
	mac := adminapi.Sign(key, adminapi.Canonical(req.Method, req.URL.Path, now.Unix()))
	req.Header.Set("Authorization", adminapi.AuthScheme+" "+ts+":"+nonce+":"+mac)

	rec := httptest.NewRecorder()
	pl.AdminAPI().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("signed code = %d body=%s, want 202", rec.Code, rec.Body.String())
	}
	// AdminAPI() is stable across calls (single instance, one counter).
	if pl.AdminAPI() != pl.AdminAPI() {
		t.Fatal("AdminAPI must return one shared instance")
	}
}
