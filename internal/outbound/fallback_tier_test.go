package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/varbin"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

type landingFailureFront struct{ *fallbackTestFront }

func (f landingFailureFront) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if destination.Fqdn == "offline-landing.test" {
		return nil, errors.New("offline landing")
	}
	return f.fallbackTestFront.DialContext(ctx, network, destination)
}

func buildTestFallback(t *testing.T, fronts map[string]adapter.Outbound, raw string) *fallback {
	t.Helper()
	builder := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = builder.Close() })
	builder.SetOutboundResolver(func(tag string) (adapter.Outbound, bool) {
		front, ok := fronts[tag]
		return front, ok
	})
	group, err := builder.Build(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = group.(io.Closer).Close() })
	return group.(*fallback)
}

func waitFallback(t *testing.T, group *fallback, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for group.Now() != want {
		if time.Now().After(deadline) {
			t.Fatalf("picked %q, want %q", group.Now(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestFallbackLandingFailureIsIsolated(t *testing.T) {
	fronts := map[string]adapter.Outbound{
		"main":   landingFailureFront{newFallbackTestFront(t, "main")},
		"backup": landingFailureFront{newFallbackTestFront(t, "backup")},
	}
	group := buildTestFallback(t, fronts, `{"type":"fallback","tag":"front","outbounds":["main","backup"],"url":"http://landing.test/probe","expected_status":"407","interval":"10s","timeout":"200ms"}`)
	waitFallback(t, group, "main")
	until := time.Now().Add(time.Second)
	for {
		group.mu.RLock()
		backupReady := group.healthy[1]
		group.mu.RUnlock()
		if backupReady {
			break
		}
		if time.Now().After(until) {
			t.Fatal("backup probe did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := group.DialContext(ctx, "tcp", M.ParseSocksaddr("offline-landing.test:1080")); err == nil {
		conn.Close()
		t.Fatal("offline landing unexpectedly accepted")
	}
	conn, err := group.DialContext(ctx, "tcp", M.ParseSocksaddr("healthy-landing.test:1080"))
	if err != nil {
		t.Fatalf("unrelated landing is now unavailable: %v", err)
	}
	conn.Close()
}

func TestFallbackFastestWithinTierAndAutomaticReturn(t *testing.T) {
	mainSlow := newFallbackTestFront(t, "main-slow")
	mainFast := newFallbackTestFront(t, "main-fast")
	backupSlow := newFallbackTestFront(t, "backup-slow")
	backupFast := newFallbackTestFront(t, "backup-fast")
	mainSlow.delay.Store(int64(100 * time.Millisecond))
	mainFast.delay.Store(int64(30 * time.Millisecond))
	backupSlow.delay.Store(int64(60 * time.Millisecond))
	fronts := map[string]adapter.Outbound{
		mainSlow.Tag(): mainSlow, mainFast.Tag(): mainFast,
		backupSlow.Tag(): backupSlow, backupFast.Tag(): backupFast,
	}
	group := buildTestFallback(t, fronts, `{"type":"fallback","tag":"front","outbounds":["main-slow","main-fast","backup-slow","backup-fast"],"primary_count":2,"url":"http://landing.test/probe","expected_status":"407","interval":"25ms","timeout":"200ms","dial_timeout":"100ms","dial_budget":"300ms"}`)
	waitFallback(t, group, "main-fast")
	mainFast.status.Store(503)
	waitFallback(t, group, "main-slow")
	mainSlow.status.Store(503)
	waitFallback(t, group, "backup-fast")
	mainFast.status.Store(407)
	waitFallback(t, group, "main-fast")
}

type loopbackSOCKSFront struct {
	sbOutbound.Adapter
	rejectFor string
}

func (f loopbackSOCKSFront) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("test front has no UDP")
}

func (f loopbackSOCKSFront) DialContext(_ context.Context, _ string, landing M.Socksaddr) (net.Conn, error) {
	client, peer := net.Pipe()
	go func() {
		defer peer.Close()
		reader := varbin.StubReader(peer)
		if _, err := socks5.ReadAuthRequest(reader); err != nil {
			return
		}
		if socks5.WriteAuthResponse(peer, socks5.AuthResponse{Method: socks5.AuthTypeUsernamePassword}) != nil {
			return
		}
		auth, err := socks5.ReadUsernamePasswordAuthRequest(reader)
		if err != nil {
			return
		}
		status := byte(socks5.UsernamePasswordStatusSuccess)
		if auth.Username != "test" || auth.Password != "test" || landing.Fqdn == f.rejectFor {
			status = 1
		}
		if socks5.WriteUsernamePasswordAuthResponse(peer, socks5.UsernamePasswordAuthResponse{Status: status}) != nil || status != 0 {
			return
		}
		request, err := socks5.ReadRequest(reader)
		if err != nil || request.Command != socks5.CommandConnect {
			return
		}
		if socks5.WriteResponse(peer, socks5.Response{ReplyCode: socks5.ReplyCodeSuccess, Bind: M.ParseSocksaddr("127.0.0.1:1080")}) == nil {
			_, _ = io.WriteString(peer, f.Tag())
		}
	}()
	return client, nil
}

func TestFallbackAuthenticatedChainRetriesOnlyAffectedLanding(t *testing.T) {
	builder := newTestSingboxBuilder(t)
	front := func(name, rejected string) adapter.Outbound {
		return &loopbackSOCKSFront{
			Adapter:   sbOutbound.NewAdapter("http", name, []string{"tcp"}, nil),
			rejectFor: rejected,
		}
	}
	outbounds := map[string]adapter.Outbound{
		"main":   front("main", "restricted-landing.test"),
		"backup": front("backup", ""),
	}
	var outboundsMu sync.RWMutex
	builder.SetOutboundResolver(func(tag string) (adapter.Outbound, bool) {
		outboundsMu.RLock()
		defer outboundsMu.RUnlock()
		ob, ok := outbounds[tag]
		return ob, ok
	})
	group, err := builder.Build(json.RawMessage(`{
		"type":"fallback","tag":"front","outbounds":["main","backup"],"primary_count":1,
		"probe_landings":[{"server":"sample-landing.test","server_port":1080,"username":"test","password":"test"}],
		"probe_quorum":1,"url":"https://probe.invalid/","interval":"2s","timeout":"1s","dial_timeout":"200ms","dial_budget":"500ms"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = group.(io.Closer).Close() })
	outboundsMu.Lock()
	outbounds["front"] = group
	outboundsMu.Unlock()
	f := group.(*fallback)
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.RLock()
		warm := f.healthy[0] && f.healthy[1]
		f.mu.RUnlock()
		if warm {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("authenticated probes did not warm both tiers")
		}
		time.Sleep(time.Millisecond)
	}
	dial := func(landing, want string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"type": "socks", "tag": landing, "server": landing, "server_port": 1080,
			"version": "5", "username": "test", "password": "test", "detour": "front",
		})
		ob, err := builder.Build(raw)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("service.test:443"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		body, err := io.ReadAll(conn)
		if err != nil || string(body) != want {
			t.Fatalf("landing used %q, want %q: %v", body, want, err)
		}
	}
	dial("restricted-landing.test", "backup")
	dial("sample-landing.test", "main")
}

func TestFallbackHoldsHealthyFrontForSmallLatencyChange(t *testing.T) {
	const landing = "example.test:1080"
	group := &fallback{
		tiered:       true,
		primaryCount: 2,
		tags:         []string{"current-primary", "alternate-primary", "backup"},
		healthy:      []bool{true, true, true},
		delays:       []time.Duration{210 * time.Millisecond, 215 * time.Millisecond, 20 * time.Millisecond},
		interval:     20 * time.Second,
		paths:        make(map[fallbackPath]fallbackPathHealth),
	}
	group.recordPath(0, landing, 210*time.Millisecond)
	group.delays[1] = 190 * time.Millisecond
	if got := group.rankedLocked(landing)[0]; got != 0 {
		t.Fatalf("20ms fluctuation replaced the current healthy primary with %q", group.tags[got])
	}
	if got := group.rankedLocked("other.test:1080")[0]; got != 1 {
		t.Fatalf("preference leaked to a separate landing: picked %q", group.tags[got])
	}
	group.recordPath(0, landing, 0)
	if got := group.rankedLocked(landing)[0]; got != 1 {
		t.Fatalf("the failed path prevented the same tier from switching: picked %q", group.tags[got])
	}
	group.recordPath(1, landing, 0)
	if got := group.rankedLocked(landing)[0]; got != 2 {
		t.Fatalf("backup not selected after both primary paths failed: picked %q", group.tags[got])
	}
	group.recordPath(2, landing, 20*time.Millisecond)
	delete(group.paths, fallbackPath{front: 1, landing: landing})
	if got := group.rankedLocked(landing)[0]; got != 1 {
		t.Fatalf("preference held onto backup despite recovery: picked %q", group.tags[got])
	}
}

func TestFallbackRateLimitsOnlySpeedBasedChanges(t *testing.T) {
	const landing = "stable.test:1080"
	group := &fallback{
		tiered:       true,
		primaryCount: 2,
		tags:         []string{"current", "fast"},
		healthy:      []bool{true, true},
		delays:       []time.Duration{210 * time.Millisecond, 100 * time.Millisecond},
		interval:     20 * time.Second,
		paths:        make(map[fallbackPath]fallbackPathHealth),
	}
	group.recordPath(0, landing, 210*time.Millisecond)
	if got := group.rankedLocked(landing)[0]; got != 0 {
		t.Fatal("speed-driven change ignored the minimum interval")
	}
	tier := fallbackTier{landing: landing, primary: true}
	preferred := group.preferred[tier]
	preferred.selectedAt = preferred.selectedAt.Add(-fallbackSwitchInterval - time.Second)
	group.preferred[tier] = preferred
	if got := group.rankedLocked(landing)[0]; got != 1 {
		t.Fatal("clear latency improvement was not used after the interval")
	}
	group.recordPath(1, landing, 100*time.Millisecond)
	group.recordPath(0, landing, 210*time.Millisecond)
	if got := group.preferred[tier].front; got != 1 {
		t.Fatal("an older in-flight connection replaced the later selection")
	}
}
