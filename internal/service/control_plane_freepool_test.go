package service

import (
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/freepool"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/topology"
)

func TestControlPlaneSyncFreePoolCreatesAndUpdatesSubscriptionAndPlatform(t *testing.T) {
	dir := t.TempDir()
	engine, closer, err := state.PersistenceBootstrap(
		filepath.Join(dir, "state"),
		filepath.Join(dir, "cache"),
	)
	if err != nil {
		t.Fatalf("PersistenceBootstrap: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})
	sched := topology.NewSubscriptionScheduler(topology.SchedulerConfig{
		SubManager: subMgr,
		Pool:       pool,
	})
	runtimeCfg := &atomic.Pointer[config.RuntimeConfig]{}
	runtimeCfg.Store(config.NewDefaultRuntimeConfig())

	cp := &ControlPlaneService{
		Engine:     engine,
		Pool:       pool,
		SubMgr:     subMgr,
		Scheduler:  sched,
		RuntimeCfg: runtimeCfg,
		EnvCfg: &config.EnvConfig{
			DefaultPlatformStickyTTL:                        7 * 24 * time.Hour,
			DefaultPlatformReverseProxyMissAction:           "TREAT_AS_EMPTY",
			DefaultPlatformReverseProxyEmptyAccountBehavior: "RANDOM",
			DefaultPlatformReverseProxyFixedAccountHeader:   "Authorization",
			DefaultPlatformAllocationPolicy:                 "BALANCED",
		},
	}

	cfg := freepool.DefaultConfig()
	cfg.TargetSubscriptionName = "FreePool"
	cfg.TargetPlatformName = "FreePool"
	cfg.SyncChainProxy = true
	cfg.SyncEphemeral = true
	cfg.SyncIncrementalAlive = true
	cfg.SyncEvictDelay = "2h"
	cfg.EnsurePlatform = true

	content1 := `{"outbounds":[{"type":"socks","tag":"[Free][socks] 1.1.1.1:1080","server":"1.1.1.1","server_port":1080,"version":"5"}]}`
	res1, err := cp.SyncFreePool(cfg, content1)
	if err != nil {
		t.Fatalf("first SyncFreePool: %v", err)
	}
	if res1.SubscriptionID == "" || res1.PlatformID == "" {
		t.Fatalf("expected non-empty sub and platform IDs, got %+v", res1)
	}

	sub, err := cp.GetSubscription(res1.SubscriptionID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.SourceType != "local" || !sub.ChainProxy || !sub.Ephemeral || !sub.IncrementalAliveNodes {
		t.Fatalf("unexpected synced subscription settings: %+v", sub)
	}
	if sub.NodeCount != 1 {
		t.Fatalf("expected 1 managed node after initial sync, got %d", sub.NodeCount)
	}

	plat, err := cp.GetPlatform(res1.PlatformID)
	if err != nil {
		t.Fatalf("GetPlatform: %v", err)
	}
	if plat.Name != "FreePool" || len(plat.RegexFilters) != 1 || plat.RegexFilters[0] != "^FreePool/" {
		t.Fatalf("unexpected synced platform: %+v", plat)
	}

	// Second sync updates the existing subscription in-place without duplicating it.
	cfg.TargetSubscriptionID = res1.SubscriptionID
	content2 := `{"outbounds":[{"type":"socks","tag":"[Free][socks] 8.8.8.8:1080","server":"8.8.8.8","server_port":1080,"version":"5"}]}`
	res2, err := cp.SyncFreePool(cfg, content2)
	if err != nil {
		t.Fatalf("second SyncFreePool: %v", err)
	}
	if res2.SubscriptionID != res1.SubscriptionID || res2.PlatformID != res1.PlatformID {
		t.Fatalf("expected in-place update, got res1=%+v res2=%+v", res1, res2)
	}
}
