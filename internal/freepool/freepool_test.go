package freepool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	M "github.com/sagernet/sing/common/metadata"
)

func TestDefaultConfigIncludesBothSourcesAndSSHPort22(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig validation failed: %v", err)
	}
	if !cfg.RequireSSHPort22 {
		t.Fatal("expected RequireSSHPort22 to be enabled by default")
	}
	if !cfg.EnforceStrictTLS {
		t.Fatal("expected EnforceStrictTLS to be enabled by default")
	}
	if !cfg.SyncChainProxy {
		t.Fatal("expected SyncChainProxy to be enabled by default")
	}
	var hasSOCKS5, hasSub bool
	for _, s := range cfg.Sources {
		if s.Kind == SourceKindSOCKS5List && s.Enabled {
			hasSOCKS5 = true
		}
		if s.Kind == SourceKindSubscription && s.Enabled {
			hasSub = true
		}
	}
	if !hasSOCKS5 || !hasSub {
		t.Fatalf("expected both socks5_list and subscription default sources, got socks5=%v sub=%v", hasSOCKS5, hasSub)
	}
}

func TestParseAndSanitizeSOCKS5ListAndSecurityRules(t *testing.T) {
	rawList := strings.Join([]string{
		"# comment",
		"1.1.1.1:1080",
		"8.8.8.8:1081:alice:secret",
		"127.0.0.1:1080",   // loopback must be rejected
		"192.168.1.5:1080", // private must be rejected
		"10.0.0.2:1080",    // private must be rejected
		"[::1]:1080",       // loopback ipv6 rejected
		"http://9.9.9.9:8080",
	}, "\n")

	src := Source{ID: "s1", Name: "SOCKS5", Kind: SourceKindSOCKS5List, Enabled: true}
	cands, rawCount := ParseAndSanitizeSource(src, []byte(rawList), true)
	if rawCount < 2 {
		t.Fatalf("expected rawCount >= 2, got %d", rawCount)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 safe public SOCKS5 candidates, got %d", len(cands))
	}
	for _, c := range cands {
		if c.Protocol != "socks" {
			t.Fatalf("expected protocol socks, got %s", c.Protocol)
		}
		var obj map[string]any
		if err := json.Unmarshal(c.RawOptions, &obj); err != nil {
			t.Fatalf("unmarshal candidate: %v", err)
		}
		if obj["type"] != "socks" {
			t.Fatalf("expected sing-box type socks, got %v", obj["type"])
		}
	}
}

func TestParseAndSanitizeEnforcesStrictTLSAndStripsDetour(t *testing.T) {
	subJSON := `{
		"outbounds": [
			{
				"type": "trojan",
				"tag": "free-trojan",
				"server": "1.2.3.4",
				"server_port": 443,
				"password": "pw",
				"detour": "untrusted-detour",
				"tls": {
					"enabled": true,
					"server_name": "example.com",
					"insecure": true
				}
			}
		]
	}`
	src := Source{ID: "sub1", Name: "FreeSub", Kind: SourceKindSubscription, Enabled: true}
	cands, _ := ParseAndSanitizeSource(src, []byte(subJSON), true)
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	var obj map[string]any
	if err := json.Unmarshal(cands[0].RawOptions, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasDetour := obj["detour"]; hasDetour {
		t.Fatal("expected detour to be stripped")
	}
	tlsObj, ok := obj["tls"].(map[string]any)
	if !ok {
		t.Fatal("expected tls object")
	}
	if insecure, _ := tlsObj["insecure"].(bool); insecure {
		t.Fatal("expected tls.insecure to be forced to false")
	}
}

func TestSampleAndDeduplicateBalancesAcrossSources(t *testing.T) {
	makeCand := func(ip string) Candidate {
		raw := []byte(`{"type":"socks","tag":"t","server":"` + ip + `","server_port":1080,"version":"5"}`)
		src := Source{ID: "x", Name: "X", Kind: SourceKindSubscription, Enabled: true}
		cands, _ := ParseAndSanitizeSource(src, []byte(`{"outbounds":[`+string(raw)+`]}`), true)
		return cands[0]
	}
	a := []Candidate{makeCand("1.1.1.1"), makeCand("1.1.1.2"), makeCand("1.1.1.3")}
	b := []Candidate{makeCand("1.1.1.1"), makeCand("2.2.2.1"), makeCand("2.2.2.2")}

	sampled := SampleAndDeduplicate([][]Candidate{a, b}, 4)
	if len(sampled) != 4 {
		t.Fatalf("expected 4 deduplicated samples, got %d", len(sampled))
	}
	gotServers := []string{sampled[0].Server, sampled[1].Server, sampled[2].Server, sampled[3].Server}
	want := []string{"1.1.1.1", "2.2.2.1", "1.1.1.2", "2.2.2.2"}
	for i := range want {
		if gotServers[i] != want[i] {
			t.Fatalf("sample[%d] = %s, want %s (all=%v)", i, gotServers[i], want[i], gotServers)
		}
	}
}

// --- In-memory TLS + SSH-2.0 pipe mock for deterministic sandbox-safe tests ---

func generateTestTLSConfig(t *testing.T, dnsName string) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}},
	}
	clientTLS := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	return serverTLS, clientTLS
}

type pipeMockFactory struct {
	serverTLS    *tls.Config
	badServerTLS *tls.Config
	allowSSH     map[string]bool
	mitmHTTPS    map[string]bool
	failHTTPS    map[string]bool
	onUpstream   func()
}

func (f *pipeMockFactory) Build(raw json.RawMessage) (adapter.Outbound, error) {
	return f.build(raw, nil)
}

func (f *pipeMockFactory) BuildWithDetour(raw json.RawMessage, upstream adapter.Outbound) (adapter.Outbound, error) {
	if upstream != nil && f.onUpstream != nil {
		f.onUpstream()
	}
	return f.build(raw, upstream)
}

func (f *pipeMockFactory) build(raw json.RawMessage, _ adapter.Outbound) (adapter.Outbound, error) {
	var obj struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	srvTLS := f.serverTLS
	if f.mitmHTTPS[obj.Server] {
		srvTLS = f.badServerTLS
	}
	return &pipeMockOutbound{
		Adapter:   sbOutbound.NewAdapter("mock", obj.Server, []string{"tcp"}, nil),
		serverTLS: srvTLS,
		allowSSH:  f.allowSSH[obj.Server],
		failHTTPS: f.failHTTPS[obj.Server],
	}, nil
}

type pipeMockOutbound struct {
	sbOutbound.Adapter
	serverTLS *tls.Config
	allowSSH  bool
	failHTTPS bool
}

func (m *pipeMockOutbound) DialContext(_ context.Context, _ string, destination M.Socksaddr) (net.Conn, error) {
	if destination.Port == 22 {
		if !m.allowSSH {
			return nil, errors.New("port 22 blocked by free proxy")
		}
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			_, _ = io.WriteString(serverConn, "SSH-2.0-babeld-test\r\n")
		}()
		return clientConn, nil
	}
	if m.failHTTPS {
		return nil, errors.New("https unreachable")
	}
	clientConn, serverConn := net.Pipe()
	go func() {
		tlsConn := tls.Server(serverConn, m.serverTLS)
		defer tlsConn.Close()
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		buf := make([]byte, 1024)
		_, _ = tlsConn.Read(buf)
		_, _ = io.WriteString(tlsConn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()
	return clientConn, nil
}

func (m *pipeMockOutbound) ListenPacket(_ context.Context, _ M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("udp not implemented")
}

type mockDownloader struct {
	files map[string][]byte
}

func (d *mockDownloader) Download(_ context.Context, url string) ([]byte, error) {
	if b, ok := d.files[url]; ok {
		return b, nil
	}
	return nil, errors.New("not found: " + url)
}

type mockRelayPicker struct {
	count int
	ob    adapter.Outbound
}

func (p *mockRelayPicker) HealthyRelayCount() int { return p.count }
func (p *mockRelayPicker) PickRelay(mode string, _ string, _ *mathrand.Rand) (adapter.Outbound, string, error) {
	if mode == RelayModeDirect {
		return nil, "direct", nil
	}
	if p.ob == nil {
		return nil, "", errors.New("no relay")
	}
	return p.ob, "chain_relay (1)", nil
}

type mockSyncer struct {
	mu      sync.Mutex
	calls   int
	lastCfg Config
	content string
}

func (s *mockSyncer) SyncFreePool(cfg Config, content string) (SyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastCfg = cfg
	s.content = content
	return SyncResult{SubscriptionID: "sub-free-1", PlatformID: "plat-free-1"}, nil
}

func TestEndToEndScreeningWithFrontRelayAntiMITMAndPort22SSH(t *testing.T) {
	goodServerTLS, clientTLS := generateTestTLSConfig(t, "check.example.com")
	mitmServerTLS, _ := generateTestTLSConfig(t, "check.example.com") // untrusted cert!

	dir := t.TempDir()
	dl := &mockDownloader{
		files: map[string][]byte{
			"https://mock.local/socks5.txt": []byte("1.1.1.1:1080\n2.2.2.2:1080\n5.5.5.5:1080\n"),
			"https://mock.local/sub.json": []byte(`{"outbounds":[
				{"type":"trojan","tag":"good-trojan","server":"3.3.3.3","server_port":443,"password":"p","tls":{"enabled":true,"insecure":true}},
				{"type":"trojan","tag":"bad-https","server":"4.4.4.4","server_port":443,"password":"p","tls":{"enabled":true}}
			]}`),
		},
	}

	var factoryMu sync.Mutex
	sawUpstream := 0
	factory := &pipeMockFactory{
		serverTLS:    goodServerTLS,
		badServerTLS: mitmServerTLS,
		allowSSH: map[string]bool{
			"1.1.1.1": true,
			"2.2.2.2": false, // passes HTTPS, fails port-22 SSH
			"3.3.3.3": true,
			"4.4.4.4": true,
			"5.5.5.5": true, // fails TLS MITM check
		},
		mitmHTTPS: map[string]bool{
			"5.5.5.5": true,
		},
		failHTTPS: map[string]bool{
			"4.4.4.4": true,
		},
		onUpstream: func() {
			factoryMu.Lock()
			sawUpstream++
			factoryMu.Unlock()
		},
	}

	frontOb := &pipeMockOutbound{Adapter: sbOutbound.NewAdapter("trojan", "front-1", []string{"tcp"}, nil), allowSSH: true}
	syncer := &mockSyncer{}

	svc, err := NewService(ServiceConfig{
		StateDir:   filepath.Join(dir, "state"),
		CacheDir:   filepath.Join(dir, "cache"),
		Downloader: dl,
		Checker: &Checker{
			Factory:   factory,
			TLSConfig: clientTLS,
		},
		Relays: &mockRelayPicker{count: 1, ob: frontOb},
		Syncer: syncer,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ProbeTimeout = "2s"
	cfg.HTTPSCheckURL = "https://check.example.com/generate_204"
	cfg.SSHCheckTarget = "github.com:22"
	cfg.RequireSSHPort22 = true
	cfg.Sources = []Source{
		{ID: "s1", Name: "SOCKS5", URL: "https://mock.local/socks5.txt", Kind: SourceKindSOCKS5List, Enabled: true},
		{ID: "s2", Name: "Sub", URL: "https://mock.local/sub.json", Kind: SourceKindSubscription, Enabled: true},
	}
	if _, err := svc.UpdateConfig(cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	if err := svc.RunSync(context.Background()); err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	status := svc.Status()
	if status.Stats.Candidates != 5 {
		t.Fatalf("candidates = %d, want 5", status.Stats.Candidates)
	}
	// 1.1.1.1, 2.2.2.2, 3.3.3.3 pass HTTPS; 4.4.4.4 unreachable; 5.5.5.5 rejected by strict TLS MITM check.
	if status.Stats.HTTPSPassed != 3 {
		t.Fatalf("https_passed = %d, want 3", status.Stats.HTTPSPassed)
	}
	// Only 1.1.1.1 and 3.3.3.3 pass both HTTPS and port-22 SSH banner.
	if status.Stats.SSH22Passed != 2 {
		t.Fatalf("ssh22_passed = %d, want 2", status.Stats.SSH22Passed)
	}
	if status.Stats.Kept != 2 || len(status.Nodes) != 2 {
		t.Fatalf("kept = %d (nodes=%d), want 2", status.Stats.Kept, len(status.Nodes))
	}
	if sawUpstream != 5 {
		t.Fatalf("expected all 5 candidates to be built via front relay detour, got %d", sawUpstream)
	}
	if syncer.calls != 1 {
		t.Fatalf("expected syncer to be called once, got %d", syncer.calls)
	}
	if status.TargetSubscriptionID != "sub-free-1" {
		t.Fatalf("target_subscription_id = %q, want sub-free-1", status.TargetSubscriptionID)
	}

	// Verify rendered sing-box subscription has 2 outbounds and strict TLS on 3.3.3.3.
	var rendered struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(syncer.content), &rendered); err != nil {
		t.Fatalf("unmarshal synced subscription: %v", err)
	}
	if len(rendered.Outbounds) != 2 {
		t.Fatalf("synced outbounds = %d, want 2", len(rendered.Outbounds))
	}
	for _, ob := range rendered.Outbounds {
		if ob["server"] == "3.3.3.3" {
			tlsObj := ob["tls"].(map[string]any)
			if tlsObj["insecure"] != false {
				t.Fatalf("expected synced trojan node tls.insecure=false, got %v", tlsObj["insecure"])
			}
		}
	}

	// Verify persistence across reload.
	svc2, err := NewService(ServiceConfig{
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
	})
	if err != nil {
		t.Fatalf("reload NewService: %v", err)
	}
	st2 := svc2.Status()
	if len(st2.Nodes) != 2 || st2.TargetSubscriptionID != "sub-free-1" {
		t.Fatalf("reloaded state mismatch: nodes=%d subID=%q", len(st2.Nodes), st2.TargetSubscriptionID)
	}
}

var _ = http.StatusNoContent
