package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/freepool"
	"github.com/Resinat/Resin/internal/service"
)

func TestFreePoolAPIStatusPatchAndSubscription(t *testing.T) {
	dir := t.TempDir()
	cp := &service.ControlPlaneService{}
	fpSvc, err := freepool.NewService(freepool.ServiceConfig{
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
		Syncer:   cp,
	})
	if err != nil {
		t.Fatalf("freepool.NewService: %v", err)
	}
	cp.FreePool = fpSvc

	runtimeCfg := &atomic.Pointer[config.RuntimeConfig]{}
	runtimeCfg.Store(config.NewDefaultRuntimeConfig())
	srv := NewServer(0, "test-admin-token", service.SystemInfo{Version: "test"}, runtimeCfg, &config.EnvConfig{}, cp, 1<<20, nil, nil)

	// 1. GET /api/v1/free-pool/status
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/free-pool/status", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var st freepool.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Config.RequireSSHPort22 {
		t.Fatal("expected require_ssh_port22 default true")
	}
	if !st.Config.EnforceStrictTLS {
		t.Fatal("expected enforce_strict_tls default true")
	}

	// 2. PATCH /api/v1/free-pool/config
	patchBody := []byte(`{"enabled":true,"max_keep_nodes":50,"require_ssh_port22":true,"interval":"15m"}`)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/free-pool/config", bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer test-admin-token")
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH config = %d, body=%s", rec.Code, rec.Body.String())
	}
	var updated freepool.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated status: %v", err)
	}
	if !updated.Config.Enabled || updated.Config.MaxKeepNodes != 50 || updated.Config.Interval != "15m" {
		t.Fatalf("unexpected updated config: %+v", updated.Config)
	}

	// 3. Invalid config rejected with 400
	badPatch := []byte(`{"interval":"10s"}`)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/free-pool/config", bytes.NewReader(badPatch))
	req.Header.Set("Authorization", "Bearer test-admin-token")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid interval, got %d", rec.Code)
	}

	// 4. GET /api/v1/free-pool/subscription
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/free-pool/subscription", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET subscription = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("expected Content-Type header")
	}
}
