package handler

// api_diagnostics_test.go: the /api/v1/diagnostics/turn route (OPS-2 §3):
// unconfigured → ok=false "not configured"; configured → probe result shown.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/admin/middleware"
)

func TestTURNCheckUnconfigured(t *testing.T) {
	a := New(Config{}, Deps{Logger: testLogger()})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/turn", nil)
	rec := httptest.NewRecorder()
	a.handleTURNCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if ok, _ := out["ok"].(bool); ok {
		t.Fatal("unconfigured must answer ok=false")
	}
	if detail, _ := out["detail"].(string); detail == "" {
		t.Fatal("detail must explain 'not configured'")
	}
}

func TestTURNCheckConfiguredProbeRuns(t *testing.T) {
	a := New(Config{}, Deps{Logger: testLogger(), TurnURL: "turn:127.0.0.1:1?transport=tcp"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/turn", nil)
	rec := httptest.NewRecorder()
	a.handleTURNCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var out struct {
		OK        bool   `json:"ok"`
		Transport string `json:"transport"`
		Addr      string `json:"addr"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if out.OK {
		t.Fatal("probe to closed port must not report ok")
	}
	if out.Transport != "tcp" || out.Addr == "" {
		t.Fatalf("probe fields missing: %+v", out)
	}
	if out.Detail == "" {
		t.Fatal("detail must carry the failure reason")
	}
	_ = time.Now // keep import honest if the struct changes
}

// routeTableTurnCheck asserts the new route is registered with a permission.
func TestRouteTableTurnCheck(t *testing.T) {
	a := New(Config{}, Deps{Logger: testLogger()})
	found := false
	for _, rt := range a.Routes() {
		if rt.Pattern == "/api/v1/diagnostics/turn" {
			found = true
			if rt.Method != http.MethodGet || rt.Public {
				t.Fatal("turn check must be GET and authenticated")
			}
			if rt.Permission != middleware.PermMetricsRead {
				t.Fatalf("permission=%s", rt.Permission)
			}
		}
	}
	if !found {
		t.Fatal("/api/v1/diagnostics/turn not in route table")
	}
}
