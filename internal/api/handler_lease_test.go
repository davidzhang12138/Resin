package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/service"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	"github.com/Resinat/Resin/internal/topology"
)

func newReassignHandlerService(t *testing.T) (*service.ControlPlaneService, *platform.Platform, node.Hash) {
	t.Helper()
	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})
	sub := subscription.NewSubscription("sub-1", "Sub1", "https://example.com/sub-1", true, false)
	subMgr.Register(sub)
	raw := []byte(`{"type":"ss","server":"203.0.113.40","port":443}`)
	targetHash := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(targetHash, subscription.ManagedNode{Tags: []string{"target"}})
	entry := node.NewNodeEntry(targetHash, raw, time.Now(), 16)
	entry.AddSubscriptionID(sub.ID)
	entry.SetEgressIP(netip.MustParseAddr("203.0.113.40"))
	entry.LatencyTable.LoadEntry("cloudflare.com", node.DomainLatencyStats{
		Ewma:        50 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	pool.LoadNodeFromBootstrap(entry)
	plat := platform.NewPlatform("11111111-1111-4111-8111-111111111111", "ReassignPlatform", nil, nil)
	plat.StickyTTLNs = int64(30 * time.Minute)
	pool.RegisterPlatform(plat)
	plat.FullRebuild(pool.Range, pool.MakeSubLookup(), func(netip.Addr) string { return "us" })
	router := routing.NewRouter(routing.RouterConfig{
		Pool:        pool,
		Authorities: func() []string { return []string{"cloudflare.com"} },
		P2CWindow:   func() time.Duration { return 10 * time.Minute },
	})
	cp := &service.ControlPlaneService{Pool: pool, SubMgr: subMgr, Router: router}

	now := time.Now().UnixNano()
	if err := router.UpsertLease(model.Lease{
		PlatformID: plat.ID, Account: "alice",
		NodeHash: node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex(),
		EgressIP: "198.51.100.7", CreatedAtNs: now, ExpiryNs: now + int64(time.Minute),
		LastAccessedNs: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return cp, plat, targetHash
}

func TestHandleReassignLease_Success(t *testing.T) {
	cp, plat, targetHash := newReassignHandlerService(t)
	body, _ := json.Marshal(map[string]string{"node_hash": targetHash.Hex()})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader(body))
	req.SetPathValue("id", plat.ID)
	req.SetPathValue("account", "alice")
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp service.LeaseResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.NodeHash != targetHash.Hex() {
		t.Fatalf("node_hash: got %q, want %q", resp.NodeHash, targetHash.Hex())
	}
	if resp.EgressIP != "203.0.113.40" {
		t.Fatalf("egress_ip: got %q, want 203.0.113.40", resp.EgressIP)
	}
}

func TestHandleReassignLease_EmptyNodeHash(t *testing.T) {
	cp, plat, _ := newReassignHandlerService(t)
	body, _ := json.Marshal(map[string]string{"node_hash": "  "})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader(body))
	req.SetPathValue("id", plat.ID)
	req.SetPathValue("account", "alice")
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleReassignLease_NodeNotRoutable(t *testing.T) {
	cp, plat, _ := newReassignHandlerService(t)
	other := node.HashFromRawOptions([]byte(`{"id":"not-routable"}`)).Hex()
	body, _ := json.Marshal(map[string]string{"node_hash": other})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader(body))
	req.SetPathValue("id", plat.ID)
	req.SetPathValue("account", "alice")
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleReassignLease_MalformedBody(t *testing.T) {
	cp, plat, _ := newReassignHandlerService(t)
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader([]byte(`{`)))
	req.SetPathValue("id", plat.ID)
	req.SetPathValue("account", "alice")
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}
