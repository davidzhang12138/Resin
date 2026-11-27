package freepool

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/transport/v2raywebsocket"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
)

// OutboundFactory abstracts SingboxBuilder for probing and unit tests.
type OutboundFactory interface {
	Build(rawOptions json.RawMessage) (adapter.Outbound, error)
	BuildWithDetour(rawOptions json.RawMessage, upstream adapter.Outbound) (adapter.Outbound, error)
}

// VerifiedNode records a candidate that passed all verification checks.
type VerifiedNode struct {
	Hash       string          `json:"hash"`
	Tag        string          `json:"tag"`
	Protocol   string          `json:"protocol"`
	Server     string          `json:"server"`
	ServerPort int             `json:"server_port"`
	SourceID   string          `json:"source_id"`
	SourceName string          `json:"source_name"`
	LatencyMs  int64           `json:"latency_ms"`
	SSH22OK    bool            `json:"ssh22_ok"`
	SSH22Ms    int64           `json:"ssh22_ms,omitempty"`
	VerifiedAt string          `json:"verified_at"`
	RawOptions json.RawMessage `json:"raw_options,omitempty"`
}

// ProbeOutcome holds the per-candidate check breakdown.
type ProbeOutcome struct {
	HTTPSOK   bool
	SSH22OK   bool
	LatencyMs int64
	SSH22Ms   int64
	Err       error
}

// Checker verifies candidate outbounds through an optional front relay.
type Checker struct {
	Factory OutboundFactory
	// DirectUpstream is an optional outbound used as the detour base in direct
	// mode so underlying sockets can be forcibly closed on timeout (preventing
	// tarpit SOCKS5 handshakes from hanging forever).
	DirectUpstream adapter.Outbound
	// TLSConfig is optional; if nil, strict default system roots + TLS 1.2+ are used.
	TLSConfig *tls.Config
}

// NewDirectUpstream creates a lightweight direct TCP/UDP adapter.Outbound so
// direct-mode probes can track and forcibly close stalled handshake sockets.
func NewDirectUpstream() adapter.Outbound {
	return &directUpstreamOutbound{
		Adapter: sbOutbound.NewAdapter("direct", "freepool-direct", []string{"tcp", "udp"}, nil),
	}
}

type directUpstreamOutbound struct {
	sbOutbound.Adapter
}

func (d *directUpstreamOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, destination.String())
}

func (d *directUpstreamOutbound) ListenPacket(ctx context.Context, _ M.Socksaddr) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, "udp", "")
}

// connTracker records active underlying connections for a single candidate probe
// so closing the tracker immediately unblocks protocols (like sing socks5) that
// ignore context cancellation during handshake reads.
type connTracker struct {
	mu      sync.Mutex
	closed  bool
	conns   map[io.Closer]struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{conns: make(map[io.Closer]struct{})}
}

func (t *connTracker) add(c io.Closer) bool {
	if c == nil {
		return true
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = c.Close()
		return false
	}
	t.conns[c] = struct{}{}
	t.mu.Unlock()
	return true
}

func (t *connTracker) remove(c io.Closer) {
	if c == nil {
		return
	}
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

func (t *connTracker) CloseAll() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	items := make([]io.Closer, 0, len(t.conns))
	for c := range t.conns {
		items = append(items, c)
	}
	clear(t.conns)
	t.mu.Unlock()
	for _, c := range items {
		_ = c.Close()
	}
}

type trackedConn struct {
	net.Conn
	tracker *connTracker
}

func (c *trackedConn) Close() error {
	c.tracker.remove(c.Conn)
	return c.Conn.Close()
}

type trackedPacketConn struct {
	net.PacketConn
	tracker *connTracker
}

func (c *trackedPacketConn) Close() error {
	c.tracker.remove(c.PacketConn)
	return c.PacketConn.Close()
}

type trackedUpstreamOutbound struct {
	adapter.Outbound
	tracker *connTracker
}

func (u *trackedUpstreamOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	conn, err := u.Outbound.DialContext(ctx, network, destination)
	if err != nil {
		return nil, err
	}
	if !u.tracker.add(conn) {
		return nil, net.ErrClosed
	}
	return &trackedConn{Conn: conn, tracker: u.tracker}, nil
}

func (u *trackedUpstreamOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	pc, err := u.Outbound.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	if !u.tracker.add(pc) {
		return nil, net.ErrClosed
	}
	return &trackedPacketConn{PacketConn: pc, tracker: u.tracker}, nil
}

func buildCandidateSafely(factory OutboundFactory, raw json.RawMessage, front adapter.Outbound) (ob adapter.Outbound, err error) {
	defer func() {
		if r := recover(); r != nil {
			if ob != nil {
				closeOutbound(ob)
			}
			ob = nil
			err = fmt.Errorf("outbound panic: %v", r)
		}
	}()
	if front != nil {
		return factory.BuildWithDetour(raw, front)
	}
	return factory.Build(raw)
}

func closeOutbound(ob adapter.Outbound) {
	if c, ok := ob.(io.Closer); ok {
		_ = c.Close()
	}
}

// Verify builds a temporary outbound (optionally chained over front), performs
// a strict-TLS HTTPS integrity/anti-hijack probe, and optionally verifies TCP
// port-22 SSH banner reachability.
func (c *Checker) Verify(ctx context.Context, cand Candidate, front adapter.Outbound, cfg Config) ProbeOutcome {
	if c == nil || c.Factory == nil {
		return ProbeOutcome{Err: errors.New("outbound factory is nil")}
	}
	timeout := cfg.probeTimeoutDuration()

	// Wrap upstream (front relay or direct dialer) with a per-probe connection
	// tracker so timing out or cancelling forcibly closes any tarpit socket.
	tracker := newConnTracker()
	defer tracker.CloseAll()

	baseUpstream := front
	if baseUpstream == nil {
		baseUpstream = c.DirectUpstream
	}
	var detourUpstream adapter.Outbound
	if baseUpstream != nil {
		detourUpstream = &trackedUpstreamOutbound{
			Outbound: baseUpstream,
			tracker:  tracker,
		}
	}

	ob, err := buildCandidateSafely(c.Factory, cand.RawOptions, detourUpstream)
	if err != nil {
		return ProbeOutcome{Err: err}
	}
	defer closeOutbound(ob)

	// 1. Strict HTTPS anti-MITM & latency check.
	httpsCtx, cancelHTTPS := context.WithTimeout(ctx, timeout)
	stopHTTPS := context.AfterFunc(httpsCtx, tracker.CloseAll)
	latencyMs, err := c.checkHTTPS(httpsCtx, ob, cfg.HTTPSCheckURL, timeout)
	stopHTTPS()
	cancelHTTPS()
	if err != nil {
		return ProbeOutcome{Err: err}
	}
	if cfg.MaxLatencyMs > 0 && latencyMs > int64(cfg.MaxLatencyMs) {
		return ProbeOutcome{
			HTTPSOK:   true,
			LatencyMs: latencyMs,
			Err:       fmt.Errorf("latency %dms exceeds limit %dms", latencyMs, cfg.MaxLatencyMs),
		}
	}

	outcome := ProbeOutcome{
		HTTPSOK:   true,
		LatencyMs: latencyMs,
	}

	// 2. Port-22 SSH banner verification (enabled by default).
	if cfg.RequireSSHPort22 {
		sshCtx, cancelSSH := context.WithTimeout(ctx, timeout)
		stopSSH := context.AfterFunc(sshCtx, tracker.CloseAll)
		sshMs, sshErr := checkSSHBanner(sshCtx, ob, cfg.SSHCheckTarget)
		stopSSH()
		cancelSSH()
		if sshErr != nil {
			outcome.Err = sshErr
			return outcome
		}
		outcome.SSH22OK = true
		outcome.SSH22Ms = sshMs
	}

	return outcome
}

func (c *Checker) checkHTTPS(ctx context.Context, ob adapter.Outbound, checkURL string, timeout time.Duration) (int64, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false,
	}
	if c.TLSConfig != nil {
		tlsCfg = c.TLSConfig.Clone()
		tlsCfg.InsecureSkipVerify = false
	}

	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			return ob.DialContext(dialCtx, network, M.ParseSocksaddr(addr))
		},
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// Do not follow redirects: captive portals / MITM proxies often inject 302s.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 1 {
		elapsed = 1
	}

	if strings.Contains(checkURL, "generate_204") {
		if resp.StatusCode != http.StatusNoContent {
			return elapsed, fmt.Errorf("unexpected status %d for generate_204 (possible hijack)", resp.StatusCode)
		}
		return elapsed, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return elapsed, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	return elapsed, nil
}

func checkSSHBanner(ctx context.Context, ob adapter.Outbound, target string) (int64, error) {
	start := time.Now()
	conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr(target))
	if err != nil {
		return 0, fmt.Errorf("ssh dial %s: %w", target, err)
	}
	defer conn.Close()

	// Reading before the first write waits for a WebSocket that Close cannot unblock.
	if ws, ok := common.Cast[*v2raywebsocket.EarlyWebsocketConn](conn); ok && ws.LazyHeadroom() {
		return 0, errors.New("ssh banner read: WebSocket handshake requires a client write")
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stop()

	reader := bufio.NewReader(io.LimitReader(conn, 256))
	line, err := reader.ReadString('\n')
	if err != nil && !strings.HasPrefix(line, "SSH-2.0-") {
		return 0, fmt.Errorf("ssh banner read: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "SSH-2.0-") {
		return 0, fmt.Errorf("invalid ssh banner %q", line)
	}
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	return elapsed, nil
}
