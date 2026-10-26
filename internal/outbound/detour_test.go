package outbound

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

type testPoolAccessor struct {
	entries map[node.Hash]*node.NodeEntry
}

func newTestPoolAccessor() *testPoolAccessor {
	return &testPoolAccessor{
		entries: make(map[node.Hash]*node.NodeEntry),
	}
}

func (p *testPoolAccessor) add(rawJSON string) *node.NodeEntry {
	raw := json.RawMessage(rawJSON)
	hash := node.HashFromRawOptions(raw)
	entry := &node.NodeEntry{
		Hash:       hash,
		RawOptions: raw,
		CreatedAt:  time.Now(),
	}
	p.entries[hash] = entry
	return entry
}

func (p *testPoolAccessor) GetEntry(hash node.Hash) (*node.NodeEntry, bool) {
	e, ok := p.entries[hash]
	return e, ok
}

func (p *testPoolAccessor) RangeNodes(fn func(node.Hash, *node.NodeEntry) bool) {
	for h, e := range p.entries {
		if !fn(h, e) {
			break
		}
	}
}

func TestDetour_DirectChaining(t *testing.T) {
	builder, err := NewSingboxBuilderWithConfig(SingboxBuilderConfig{
		DNSUpstreams: []string{"https://1.1.1.1/dns-query"},
	})
	if err != nil {
		t.Fatalf("NewSingboxBuilderWithConfig: %v", err)
	}
	defer builder.Close()

	pool := newTestPoolAccessor()
	frontEntry := pool.add(`{"type":"socks","tag":"front-node","server":"127.0.0.1","server_port":1080}`)
	landingEntry := pool.add(`{"type":"http","tag":"landing-node","server":"127.0.0.1","server_port":8080,"detour":"front-node"}`)

	mgr := NewOutboundManager(pool, builder)

	// Build landing node. It should trigger on-demand build of front-node via detour.
	mgr.EnsureNodeOutbound(landingEntry.Hash)

	if landingEntry.LastError != "" {
		t.Fatalf("landing node build failed: %s", landingEntry.LastError)
	}
	if landingEntry.Outbound.Load() == nil {
		t.Fatal("landing node outbound is nil")
	}

	// Front node should also have been built and stored
	if frontEntry.Outbound.Load() == nil {
		t.Fatal("front node outbound was not built via detour resolution")
	}

	ob := *landingEntry.Outbound.Load()
	if ob.Tag() != "landing-node" {
		t.Errorf("expected tag landing-node, got %s", ob.Tag())
	}
	deps := ob.Dependencies()
	if len(deps) != 1 || deps[0] != "front-node" {
		t.Errorf("expected dependency [front-node], got %v", deps)
	}
}

func TestDetour_CircularDependency(t *testing.T) {
	builder, err := NewSingboxBuilderWithConfig(SingboxBuilderConfig{
		DNSUpstreams: []string{"https://1.1.1.1/dns-query"},
	})
	if err != nil {
		t.Fatalf("NewSingboxBuilderWithConfig: %v", err)
	}
	defer builder.Close()

	pool := newTestPoolAccessor()
	entryA := pool.add(`{"type":"http","tag":"node-a","server":"127.0.0.1","server_port":8081,"detour":"node-b"}`)
	_ = pool.add(`{"type":"http","tag":"node-b","server":"127.0.0.1","server_port":8082,"detour":"node-a"}`)

	mgr := NewOutboundManager(pool, builder)

	// Should not deadlock or panic
	mgr.EnsureNodeOutbound(entryA.Hash)

	if entryA.Outbound.Load() != nil {
		t.Fatal("expected entryA build to fail due to circular detour")
	}
	if !strings.Contains(entryA.LastError, "detour") {
		t.Errorf("expected detour error in lastError, got: %s", entryA.LastError)
	}
}

func TestDetour_NotFound(t *testing.T) {
	builder, err := NewSingboxBuilderWithConfig(SingboxBuilderConfig{
		DNSUpstreams: []string{"https://1.1.1.1/dns-query"},
	})
	if err != nil {
		t.Fatalf("NewSingboxBuilderWithConfig: %v", err)
	}
	defer builder.Close()

	pool := newTestPoolAccessor()
	entry := pool.add(`{"type":"http","tag":"landing-node","server":"127.0.0.1","server_port":8080,"detour":"missing-front"}`)

	mgr := NewOutboundManager(pool, builder)
	mgr.EnsureNodeOutbound(entry.Hash)

	if entry.Outbound.Load() != nil {
		t.Fatal("expected build to fail when detour not found")
	}
	if !strings.Contains(entry.LastError, "outbound detour not found") {
		t.Errorf("expected 'outbound detour not found' error, got: %s", entry.LastError)
	}
}

func TestDetour_URLTestGroup(t *testing.T) {
	builder, err := NewSingboxBuilderWithConfig(SingboxBuilderConfig{
		DNSUpstreams: []string{"https://1.1.1.1/dns-query"},
	})
	if err != nil {
		t.Fatalf("NewSingboxBuilderWithConfig: %v", err)
	}
	defer builder.Close()

	pool := newTestPoolAccessor()
	front1 := pool.add(`{"type":"socks","tag":"front-1","server":"127.0.0.1","server_port":1081}`)
	front2 := pool.add(`{"type":"socks","tag":"front-2","server":"127.0.0.1","server_port":1082}`)
	group := pool.add(`{"type":"urltest","tag":"⚡ 前置优选","outbounds":["front-1","front-2"],"url":"https://cp.cloudflare.com/generate_204","interval":"300s","tolerance":50}`)
	landing := pool.add(`{"type":"http","tag":"landing-node","server":"127.0.0.1","server_port":8080,"detour":"⚡ 前置优选"}`)

	mgr := NewOutboundManager(pool, builder)

	// Build landing node. It should trigger on-demand build of group, which triggers front-1 and front-2.
	mgr.EnsureNodeOutbound(landing.Hash)

	if landing.LastError != "" {
		t.Fatalf("landing node build failed: %s", landing.LastError)
	}
	if landing.Outbound.Load() == nil {
		t.Fatal("landing node outbound is nil")
	}
	if group.Outbound.Load() == nil {
		t.Fatal("group outbound was not built")
	}
	if front1.Outbound.Load() == nil {
		t.Fatal("front1 outbound was not built")
	}
	if front2.Outbound.Load() == nil {
		t.Fatal("front2 outbound was not built")
	}
}
