package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	M "github.com/sagernet/sing/common/metadata"
)

type fallbackTestFront struct {
	sbOutbound.Adapter
	address  string
	status   atomic.Int32
	delay    atomic.Int64
	failDial atomic.Bool
}

func (f *fallbackTestFront) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if f.failDial.Load() {
		return nil, errors.New("front disconnected")
	}
	return (&net.Dialer{}).DialContext(ctx, network, f.address)
}

func (f *fallbackTestFront) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("test front has no UDP")
}

func newFallbackTestFront(t *testing.T, tag string) *fallbackTestFront {
	t.Helper()
	f := &fallbackTestFront{Adapter: sbOutbound.NewAdapter("http", tag, []string{"tcp"}, nil)}
	f.status.Store(407)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			time.Sleep(time.Duration(f.delay.Load()))
			w.WriteHeader(int(f.status.Load()))
			return
		}
		_, _ = io.WriteString(w, tag)
	}))
	f.address = srv.Listener.Addr().String()
	t.Cleanup(srv.Close)
	return f
}

func TestFallbackRealHTTPProbeFailoverAndRecovery(t *testing.T) {
	main := newFallbackTestFront(t, "main")
	backup := newFallbackTestFront(t, "backup")
	main.delay.Store(int64(15 * time.Millisecond)) // Backup is faster; main must still win.
	b := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = b.Close() })
	b.SetOutboundResolver(func(tag string) (adapter.Outbound, bool) {
		switch tag {
		case "main":
			return main, true
		case "backup":
			return backup, true
		default:
			return nil, false
		}
	})
	ob, err := b.Build(json.RawMessage(`{"type":"fallback","tag":"front","outbounds":["main","backup"],"url":"http://landing.test:6227/probe","expected_status":"407","interval":"30ms","timeout":"70ms"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ob.(io.Closer).Close() })
	group := ob.(interface{ Now() string })
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for group.Now() != want {
			if time.Now().After(deadline) {
				t.Fatalf("selected %q, want %q", group.Now(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	get := func(want string) {
		t.Helper()
		transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return ob.DialContext(ctx, network, M.ParseSocksaddr(address))
		}, DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond}
		resp, err := client.Get("http://service.test/value")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != want {
			t.Fatalf("traffic used %q, want %q", body, want)
		}
	}

	waitFor("main")
	get("main")
	main.status.Store(503) // Can answer HTTP but cannot reach landing.
	waitFor("backup")
	get("backup")
	main.status.Store(407)
	waitFor("main")
	get("main")
	main.delay.Store(int64(150 * time.Millisecond)) // Timeout also means unavailable.
	waitFor("backup")
	get("backup")
	main.delay.Store(0)
	waitFor("main")
	main.failDial.Store(true) // Immediate dial failure must try the already-healthy backup.
	get("backup")
	backup.status.Store(503)
	waitFor("")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("service.test:80")); err == nil {
		conn.Close()
		t.Fatal("all failed probes must produce an unavailable error")
	}
}

func TestFallbackSOCKS5RejectsBlockedLanding(t *testing.T) {
	newSOCKS := func(tag string) (*fallbackTestFront, *atomic.Bool) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		blocked := new(atomic.Bool)
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(time.Second))
					var greeting [4]byte
					if _, err := io.ReadFull(conn, greeting[:]); err != nil {
						return
					}
					if blocked.Load() {
						_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\n\r\n")
					} else {
						_, _ = conn.Write([]byte{5, 2})
					}
				}()
			}
		}()
		return &fallbackTestFront{Adapter: sbOutbound.NewAdapter("http", tag, []string{"tcp"}, nil), address: ln.Addr().String()}, blocked
	}
	main, blocked := newSOCKS("main")
	backup, _ := newSOCKS("backup")
	blocked.Store(true)
	b := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = b.Close() })
	b.SetOutboundResolver(func(tag string) (adapter.Outbound, bool) {
		if tag == "main" {
			return main, true
		}
		if tag == "backup" {
			return backup, true
		}
		return nil, false
	})
	ob, err := b.Build(json.RawMessage(`{"type":"fallback","tag":"front","outbounds":["main","backup"],"url":"socks5://landing.test:6240","interval":"20ms","timeout":"100ms"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ob.(io.Closer).Close() })
	group := ob.(interface{ Now() string })
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for group.Now() != want {
			if time.Now().After(deadline) {
				t.Fatalf("selected %q, want %q", group.Now(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("backup")
	blocked.Store(false)
	waitFor("main")
}

func TestFallbackDetourFollowsSubscriptionReplacement(t *testing.T) {
	main := newFallbackTestFront(t, "main")
	backup := newFallbackTestFront(t, "backup")
	b := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = b.Close() })
	var current atomic.Pointer[adapter.Outbound]
	b.SetOutboundResolver(func(tag string) (adapter.Outbound, bool) {
		switch tag {
		case "main":
			return main, true
		case "backup":
			return backup, true
		case "front":
			if p := current.Load(); p != nil {
				return *p, true
			}
		}
		return nil, false
	})
	build := func(member string) adapter.Outbound {
		t.Helper()
		raw := json.RawMessage(`{"type":"fallback","tag":"front","outbounds":["` + member + `"],"url":"http://landing.test/probe","expected_status":"407","timeout":"100ms"}`)
		ob, err := b.Build(raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ob.(io.Closer).Close() })
		return ob
	}
	first := build("main")
	current.Store(&first)
	cached := dialer.NewDetour(b.detourMgr, "front", false)
	if err := dialer.InitializeDetour(cached); err != nil {
		t.Fatal(err)
	}
	second := build("backup")
	current.Store(&second)
	_ = first.(io.Closer).Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return cached.DialContext(ctx, network, M.ParseSocksaddr(address))
	}}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("http://service.test/value")
	if err != nil {
		t.Fatalf("cached landing detour kept the retired group: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "backup" {
		t.Fatalf("cached detour used %q, want backup", body)
	}
}

func TestFallbackProbeQuorumToleratesOneDeadLanding(t *testing.T) {
	front := newFallbackTestFront(t, "main")
	front.status.Store(503)
	b := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = b.Close() })
	b.SetOutboundResolver(func(tag string) (adapter.Outbound, bool) { return front, tag == "main" })
	ob, err := b.Build(json.RawMessage(`{"type":"fallback","tag":"front","outbounds":["main"],"probe_urls":["http://landing.test/probe","http://landing.test/ok1","http://landing.test/ok2"],"probe_quorum":2,"expected_status":"200","interval":"20ms","timeout":"100ms"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ob.(io.Closer).Close() })
	group := ob.(interface{ Now() string })
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for group.Now() != want {
			if time.Now().After(deadline) {
				t.Fatalf("selected %q, want %q", group.Now(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("main")
	front.failDial.Store(true)
	waitFor("")
}
