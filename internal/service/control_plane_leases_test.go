package service

import (
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	"github.com/Resinat/Resin/internal/topology"
)

func newLeaseInheritanceTestService() (*ControlPlaneService, *platform.Platform) {
	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})

	plat := platform.NewPlatform("plat-1", "Default", nil, nil)
	pool.RegisterPlatform(plat)

	router := routing.NewRouter(routing.RouterConfig{
		Pool:        pool,
		Authorities: func() []string { return []string{"cloudflare.com"} },
		P2CWindow:   func() time.Duration { return 10 * time.Minute },
	})

	return &ControlPlaneService{
		Pool:   pool,
		SubMgr: subMgr,
		Router: router,
	}, plat
}

func seedLease(t *testing.T, cp *ControlPlaneService, lease model.Lease) {
	t.Helper()
	if err := cp.Router.UpsertLease(lease); err != nil {
		t.Fatalf("UpsertLease: %v", err)
	}
}

func assertServiceErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) {
		t.Fatalf("error type: got %T, want *ServiceError", err)
	}
	if svcErr.Code != code {
		t.Fatalf("error code: got %q, want %q", svcErr.Code, code)
	}
}

func seedSharedNodeAcrossSubscriptions(
	t *testing.T,
	cp *ControlPlaneService,
	hash node.Hash,
	olderSubID string,
	olderSubName string,
	olderCreatedAtNs int64,
	olderTags []string,
	newerSubID string,
	newerSubName string,
	newerCreatedAtNs int64,
	newerTags []string,
) {
	t.Helper()

	older := subscription.NewSubscription(olderSubID, olderSubName, "https://example.com/"+olderSubID, true, false)
	older.CreatedAtNs = olderCreatedAtNs
	olderManaged := subscription.NewManagedNodes()
	olderManaged.StoreNode(hash, subscription.ManagedNode{Tags: olderTags})
	older.SwapManagedNodes(olderManaged)

	newer := subscription.NewSubscription(newerSubID, newerSubName, "https://example.com/"+newerSubID, true, false)
	newer.CreatedAtNs = newerCreatedAtNs
	newerManaged := subscription.NewManagedNodes()
	newerManaged.StoreNode(hash, subscription.ManagedNode{Tags: newerTags})
	newer.SwapManagedNodes(newerManaged)

	cp.SubMgr.Register(older)
	cp.SubMgr.Register(newer)

	raw := json.RawMessage(`{"type":"ss","server":"198.51.100.10","port":443}`)
	cp.Pool.AddNodeFromSub(hash, raw, older.ID)
	cp.Pool.AddNodeFromSub(hash, raw, newer.ID)
}

func TestGetLease_NodeTagUsesEarliestSubscriptionThenMinTag(t *testing.T) {
	cp, plat := newLeaseInheritanceTestService()

	hash := node.HashFromRawOptions([]byte(`{"type":"ss","server":"198.51.100.10","port":443}`))
	seedSharedNodeAcrossSubscriptions(
		t,
		cp,
		hash,
		"sub-old",
		"Z-Provider",
		100,
		[]string{"zz", "aa"},
		"sub-new",
		"A-Provider",
		200,
		[]string{"00"},
	)

	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID:     plat.ID,
		Account:        "alice",
		NodeHash:       hash.Hex(),
		EgressIP:       "203.0.113.10",
		CreatedAtNs:    now - int64(time.Minute),
		ExpiryNs:       now + int64(time.Minute),
		LastAccessedNs: now,
	})

	got, err := cp.GetLease(plat.ID, "alice")
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if got.NodeTag != "Z-Provider/aa" {
		t.Fatalf("node_tag: got %q, want %q", got.NodeTag, "Z-Provider/aa")
	}
}

func TestGetLease_NodeTagPrefersEarliestEnabledSubscription(t *testing.T) {
	cp, plat := newLeaseInheritanceTestService()

	hash := node.HashFromRawOptions([]byte(`{"type":"ss","server":"198.51.100.11","port":443}`))
	seedSharedNodeAcrossSubscriptions(
		t,
		cp,
		hash,
		"sub-old-disabled",
		"Z-Provider",
		100,
		[]string{"zz", "aa"},
		"sub-new-enabled",
		"A-Provider",
		200,
		[]string{"00"},
	)

	old := cp.SubMgr.Lookup("sub-old-disabled")
	if old == nil {
		t.Fatal("old subscription not found")
	}
	old.SetEnabled(false)

	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID:     plat.ID,
		Account:        "carol",
		NodeHash:       hash.Hex(),
		EgressIP:       "203.0.113.12",
		CreatedAtNs:    now - int64(time.Minute),
		ExpiryNs:       now + int64(time.Minute),
		LastAccessedNs: now,
	})

	got, err := cp.GetLease(plat.ID, "carol")
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if got.NodeTag != "A-Provider/00" {
		t.Fatalf("node_tag: got %q, want %q", got.NodeTag, "A-Provider/00")
	}
}

func TestListLeases_NodeTagUsesEarliestSubscriptionThenMinTag(t *testing.T) {
	cp, plat := newLeaseInheritanceTestService()

	hash := node.HashFromRawOptions([]byte(`{"type":"ss","server":"203.0.113.20","port":443}`))
	seedSharedNodeAcrossSubscriptions(
		t,
		cp,
		hash,
		"sub-old-list",
		"OldSub",
		50,
		[]string{"b", "a"},
		"sub-new-list",
		"NewSub",
		60,
		[]string{"0"},
	)

	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID:     plat.ID,
		Account:        "bob",
		NodeHash:       hash.Hex(),
		EgressIP:       "198.51.100.3",
		CreatedAtNs:    now - int64(time.Minute),
		ExpiryNs:       now + int64(time.Minute),
		LastAccessedNs: now,
	})

	leases, err := cp.ListLeases(plat.ID)
	if err != nil {
		t.Fatalf("ListLeases: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases len: got %d, want 1", len(leases))
	}
	if leases[0].NodeTag != "OldSub/a" {
		t.Fatalf("node_tag: got %q, want %q", leases[0].NodeTag, "OldSub/a")
	}
}

func TestInheritLeaseByPlatformName_Success(t *testing.T) {
	cp, plat := newLeaseInheritanceTestService()

	hash := node.HashFromRawOptions([]byte(`{"id":"parent-node"}`)).Hex()
	now := time.Now().UnixNano()
	parent := model.Lease{
		PlatformID:     plat.ID,
		Account:        "parent",
		NodeHash:       hash,
		EgressIP:       "203.0.113.10",
		CreatedAtNs:    now - int64(5*time.Minute),
		ExpiryNs:       now + int64(30*time.Minute),
		LastAccessedNs: now - int64(time.Minute),
	}
	seedLease(t, cp, parent)

	if err := cp.InheritLeaseByPlatformName(plat.Name, "parent", "child"); err != nil {
		t.Fatalf("InheritLeaseByPlatformName: %v", err)
	}

	child := cp.Router.ReadLease(model.LeaseKey{PlatformID: plat.ID, Account: "child"})
	if child == nil {
		t.Fatal("expected inherited lease for child")
	}
	if child.NodeHash != parent.NodeHash {
		t.Fatalf("child node_hash: got %q, want %q", child.NodeHash, parent.NodeHash)
	}
	if child.EgressIP != parent.EgressIP {
		t.Fatalf("child egress_ip: got %q, want %q", child.EgressIP, parent.EgressIP)
	}
	if child.ExpiryNs != parent.ExpiryNs {
		t.Fatalf("child expiry_ns: got %d, want %d", child.ExpiryNs, parent.ExpiryNs)
	}
	if child.CreatedAtNs != parent.CreatedAtNs {
		t.Fatalf("child created_at_ns: got %d, want %d", child.CreatedAtNs, parent.CreatedAtNs)
	}
	if child.LastAccessedNs != parent.LastAccessedNs {
		t.Fatalf("child last_accessed_ns: got %d, want %d", child.LastAccessedNs, parent.LastAccessedNs)
	}
}

func TestInheritLeaseByPlatformName_OverwritesExistingTargetLease(t *testing.T) {
	cp, plat := newLeaseInheritanceTestService()

	now := time.Now().UnixNano()
	parentHash := node.HashFromRawOptions([]byte(`{"id":"parent-node-overwrite"}`)).Hex()
	oldChildHash := node.HashFromRawOptions([]byte(`{"id":"old-child-node"}`)).Hex()

	parent := model.Lease{
		PlatformID:     plat.ID,
		Account:        "parent",
		NodeHash:       parentHash,
		EgressIP:       "198.51.100.1",
		CreatedAtNs:    now - int64(2*time.Minute),
		ExpiryNs:       now + int64(20*time.Minute),
		LastAccessedNs: now - int64(10*time.Second),
	}
	oldChild := model.Lease{
		PlatformID:     plat.ID,
		Account:        "child",
		NodeHash:       oldChildHash,
		EgressIP:       "198.51.100.2",
		CreatedAtNs:    now - int64(time.Minute),
		ExpiryNs:       now + int64(5*time.Minute),
		LastAccessedNs: now - int64(5*time.Second),
	}
	seedLease(t, cp, parent)
	seedLease(t, cp, oldChild)

	if err := cp.InheritLeaseByPlatformName(plat.Name, "parent", "child"); err != nil {
		t.Fatalf("InheritLeaseByPlatformName: %v", err)
	}

	child := cp.Router.ReadLease(model.LeaseKey{PlatformID: plat.ID, Account: "child"})
	if child == nil {
		t.Fatal("expected overwritten child lease")
	}
	if child.NodeHash != parent.NodeHash {
		t.Fatalf("child node_hash after overwrite: got %q, want %q", child.NodeHash, parent.NodeHash)
	}
	if child.EgressIP != parent.EgressIP {
		t.Fatalf("child egress_ip after overwrite: got %q, want %q", child.EgressIP, parent.EgressIP)
	}
	if child.ExpiryNs != parent.ExpiryNs {
		t.Fatalf("child expiry_ns after overwrite: got %d, want %d", child.ExpiryNs, parent.ExpiryNs)
	}
}

func TestInheritLeaseByPlatformName_ParentLeaseMissingOrExpired(t *testing.T) {
	cp, plat := newLeaseInheritanceTestService()

	err := cp.InheritLeaseByPlatformName(plat.Name, "missing-parent", "child")
	if err == nil {
		t.Fatal("expected NOT_FOUND for missing parent lease")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")

	now := time.Now().UnixNano()
	expiredParent := model.Lease{
		PlatformID:     plat.ID,
		Account:        "expired-parent",
		NodeHash:       node.HashFromRawOptions([]byte(`{"id":"expired-parent-node"}`)).Hex(),
		EgressIP:       "203.0.113.77",
		CreatedAtNs:    now - int64(time.Hour),
		ExpiryNs:       now - int64(time.Second),
		LastAccessedNs: now - int64(time.Minute),
	}
	seedLease(t, cp, expiredParent)

	err = cp.InheritLeaseByPlatformName(plat.Name, "expired-parent", "child")
	if err == nil {
		t.Fatal("expected NOT_FOUND for expired parent lease")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")
}

func TestInheritLeaseByPlatformName_InvalidArguments(t *testing.T) {
	cp, _ := newLeaseInheritanceTestService()

	cases := []struct {
		name         string
		platformName string
		parent       string
		child        string
	}{
		{
			name:         "empty platform",
			platformName: "",
			parent:       "parent",
			child:        "child",
		},
		{
			name:         "empty parent",
			platformName: "Default",
			parent:       "   ",
			child:        "child",
		},
		{
			name:         "empty child",
			platformName: "Default",
			parent:       "parent",
			child:        "   ",
		},
		{
			name:         "same parent and child",
			platformName: "Default",
			parent:       "same",
			child:        "same",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cp.InheritLeaseByPlatformName(tc.platformName, tc.parent, tc.child)
			if err == nil {
				t.Fatal("expected INVALID_ARGUMENT error")
			}
			assertServiceErrorCode(t, err, "INVALID_ARGUMENT")
		})
	}
}

func newReassignTestService(t *testing.T) (*ControlPlaneService, *platform.Platform, node.Hash) {
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

	// Seed a fully-routable target node into the pool.
	raw := []byte(`{"type":"ss","server":"203.0.113.40","port":443}`)
	targetHash := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(targetHash, subscription.ManagedNode{Tags: []string{"target"}})
	entry := node.NewNodeEntry(targetHash, raw, time.Now(), 16)
	entry.AddSubscriptionID(sub.ID)
	entry.SetEgressIP(netip.MustParseAddr("203.0.113.40"))
	// A latency record is required by Platform.evaluateNode (HasLatency).
	entry.LatencyTable.LoadEntry("cloudflare.com", node.DomainLatencyStats{
		Ewma:        50 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	pool.LoadNodeFromBootstrap(entry)

	plat := platform.NewPlatform("plat-reassign", "ReassignPlatform", nil, nil)
	plat.StickyTTLNs = int64(30 * time.Minute)
	pool.RegisterPlatform(plat)
	// Build the routable view so the target node is contained.
	plat.FullRebuild(pool.Range, pool.MakeSubLookup(), func(netip.Addr) string { return "us" })

	router := routing.NewRouter(routing.RouterConfig{
		Pool:        pool,
		Authorities: func() []string { return []string{"cloudflare.com"} },
		P2CWindow:   func() time.Duration { return 10 * time.Minute },
	})

	cp := &ControlPlaneService{
		Pool:   pool,
		SubMgr: subMgr,
		Router: router,
	}
	return cp, plat, targetHash
}

func TestReassignLease_Success(t *testing.T) {
	cp, plat, targetHash := newReassignTestService(t)

	// Seed an existing lease pointing at a different (non-routable) hash.
	oldHash := node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex()
	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID:     plat.ID,
		Account:        "alice",
		NodeHash:       oldHash,
		EgressIP:       "198.51.100.7",
		CreatedAtNs:    now - int64(5*time.Minute),
		ExpiryNs:       now + int64(time.Minute),
		LastAccessedNs: now - int64(time.Minute),
	})

	resp, err := cp.ReassignLease(plat.ID, "alice", targetHash.Hex())
	if err != nil {
		t.Fatalf("ReassignLease: %v", err)
	}
	if resp.NodeHash != targetHash.Hex() {
		t.Fatalf("node_hash: got %q, want %q", resp.NodeHash, targetHash.Hex())
	}
	if resp.EgressIP != "203.0.113.40" {
		t.Fatalf("egress_ip: got %q, want 203.0.113.40", resp.EgressIP)
	}
	if resp.Account != "alice" {
		t.Fatalf("account: got %q, want alice", resp.Account)
	}

	// Expiry should be renewed to roughly now + StickyTTLNs.
	got := cp.Router.ReadLease(model.LeaseKey{PlatformID: plat.ID, Account: "alice"})
	if got == nil {
		t.Fatal("expected lease to still exist")
	}
	if got.NodeHash != targetHash.Hex() {
		t.Fatalf("persisted node_hash: got %q, want %q", got.NodeHash, targetHash.Hex())
	}
	wantExpiry := now + plat.StickyTTLNs
	if got.ExpiryNs < wantExpiry-int64(5*time.Second) || got.ExpiryNs > wantExpiry+int64(5*time.Second) {
		t.Fatalf("expiry_ns: got %d, want ~%d", got.ExpiryNs, wantExpiry)
	}
	// Created-at is preserved from the original lease.
	if got.CreatedAtNs != now-int64(5*time.Minute) {
		t.Fatalf("created_at_ns: got %d, want %d", got.CreatedAtNs, now-int64(5*time.Minute))
	}

	// IP load: new egress IP gains a lease, old egress IP loses it.
	snapshot := cp.Router.SnapshotIPLoad(plat.ID)
	if n, ok := snapshot[netip.MustParseAddr("203.0.113.40")]; !ok || n < 1 {
		t.Fatalf("ip_load[new]: got (%d, %t), want >=1", n, ok)
	}
	if n, ok := snapshot[netip.MustParseAddr("198.51.100.7")]; ok && n != 0 {
		t.Fatalf("ip_load[old]: got %d, want 0 or absent", n)
	}
}

func TestReassignLease_NodeNotRoutable(t *testing.T) {
	cp, plat, _ := newReassignTestService(t)

	// A hash that is NOT in the platform's routable view.
	otherHash := node.HashFromRawOptions([]byte(`{"id":"not-routable-node"}`)).Hex()
	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID: plat.ID, Account: "alice",
		NodeHash: node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex(),
		EgressIP: "198.51.100.7", CreatedAtNs: now, ExpiryNs: now + int64(time.Minute),
		LastAccessedNs: now,
	})

	_, err := cp.ReassignLease(plat.ID, "alice", otherHash)
	if err == nil {
		t.Fatal("expected INVALID_ARGUMENT for non-routable node")
	}
	assertServiceErrorCode(t, err, "INVALID_ARGUMENT")
}

func TestReassignLease_LeaseMissingOrExpired(t *testing.T) {
	cp, plat, targetHash := newReassignTestService(t)

	// No lease seeded -> not found.
	_, err := cp.ReassignLease(plat.ID, "ghost", targetHash.Hex())
	if err == nil {
		t.Fatal("expected NOT_FOUND for missing lease")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")

	// Expired lease -> not found.
	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID: plat.ID, Account: "bob",
		NodeHash: node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex(),
		EgressIP: "198.51.100.8", CreatedAtNs: now - int64(time.Hour),
		ExpiryNs: now - int64(time.Second), LastAccessedNs: now - int64(time.Minute),
	})
	_, err = cp.ReassignLease(plat.ID, "bob", targetHash.Hex())
	if err == nil {
		t.Fatal("expected NOT_FOUND for expired lease")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")
}

func TestReassignLease_PlatformMissing(t *testing.T) {
	cp, _, targetHash := newReassignTestService(t)
	_, err := cp.ReassignLease("no-such-platform", "alice", targetHash.Hex())
	if err == nil {
		t.Fatal("expected NOT_FOUND for missing platform")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")
}
