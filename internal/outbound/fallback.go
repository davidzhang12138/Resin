package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

type fallbackOptions struct {
	Outbounds      []string           `json:"outbounds"`
	URL            string             `json:"url"`
	ProbeURLs      []string           `json:"probe_urls,omitempty"`
	ProbeQuorum    int                `json:"probe_quorum,omitempty"`
	ExpectedStatus string             `json:"expected_status,omitempty"`
	Interval       badoption.Duration `json:"interval,omitempty"`
	Timeout        badoption.Duration `json:"timeout,omitempty"`
}

func registerFallback(registry *sbOutbound.Registry) {
	sbOutbound.Register[fallbackOptions](registry, "fallback", newFallback)
}

// fallback preserves subscription order. Probes validate the landing service,
// while traffic always uses the first healthy candidate, regardless of speed.
type fallback struct {
	sbOutbound.Adapter
	ctx       context.Context
	cancel    context.CancelFunc
	manager   adapter.OutboundManager
	tags      []string
	urls      []*url.URL
	quorum    int
	expected  map[int]bool
	interval  time.Duration
	timeout   time.Duration
	mu        sync.RWMutex
	healthy   []bool
	ready     chan struct{}
	readyOnce sync.Once
	wg        sync.WaitGroup
}

var _ adapter.OutboundGroup = (*fallback)(nil)

func newFallback(ctx context.Context, _ adapter.Router, _ log.ContextLogger, tag string, options fallbackOptions) (adapter.Outbound, error) {
	if len(options.Outbounds) == 0 {
		return nil, errors.New("fallback requires outbounds")
	}
	interval, timeout := time.Duration(options.Interval), time.Duration(options.Timeout)
	if interval == 0 {
		interval = 20 * time.Second
	}
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	if interval < 0 || timeout < 0 {
		return nil, errors.New("fallback interval and timeout must be positive")
	}
	links := options.ProbeURLs
	if len(links) == 0 {
		links = []string{options.URL}
	}
	var urls []*url.URL
	for _, link := range links {
		u, err := url.Parse(link)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
			return nil, fmt.Errorf("invalid fallback probe URL %q", link)
		}
		if u.Scheme == "socks5" && u.Port() == "" {
			return nil, errors.New("SOCKS5 probe requires an explicit port")
		}
		urls = append(urls, u)
	}
	quorum := options.ProbeQuorum
	if quorum == 0 {
		quorum = len(urls)
	}
	if quorum < 1 || quorum > len(urls) {
		return nil, errors.New("fallback probe quorum must be within probe URL count")
	}
	expected := make(map[int]bool)
	status := options.ExpectedStatus
	if status == "" {
		status = "200-399"
	}
	for _, part := range strings.Split(status, "/") {
		bounds := strings.Split(strings.TrimSpace(part), "-")
		lo, err := strconv.Atoi(bounds[0])
		if err != nil || len(bounds) > 2 {
			return nil, fmt.Errorf("invalid fallback expected status %q", status)
		}
		hi := lo
		if len(bounds) == 2 {
			hi, err = strconv.Atoi(bounds[1])
			if err != nil {
				return nil, fmt.Errorf("invalid fallback expected status %q", status)
			}
		}
		if lo < 100 || hi > 599 || lo > hi {
			return nil, fmt.Errorf("invalid fallback expected status %q", status)
		}
		for code := lo; code <= hi; code++ {
			expected[code] = true
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	return &fallback{
		Adapter: sbOutbound.NewAdapter("fallback", tag, []string{"tcp", "udp"}, options.Outbounds),
		ctx:     ctx, cancel: cancel, manager: service.FromContext[adapter.OutboundManager](ctx),
		tags: append([]string(nil), options.Outbounds...), urls: urls, quorum: quorum, expected: expected,
		interval: interval, timeout: timeout, healthy: make([]bool, len(options.Outbounds)), ready: make(chan struct{}),
	}, nil
}

func (s *fallback) Start(stage adapter.StartStage) error {
	if stage == adapter.StartStateStart {
		for _, tag := range s.tags {
			if _, ok := s.manager.Outbound(tag); !ok {
				return fmt.Errorf("fallback outbound not found: %s", tag)
			}
		}
	}
	if stage == adapter.StartStateStarted {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.check()
			s.readyOnce.Do(func() { close(s.ready) })
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.check()
				}
			}
		}()
	}
	return nil
}

func (s *fallback) check() {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, tag := range s.tags {
		select {
		case sem <- struct{}{}:
		case <-s.ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(i int, tag string) {
			defer wg.Done()
			defer func() { <-sem }()
			ob, ok := s.manager.Outbound(tag)
			healthy := false
			if ok {
				passed := 0
				for n, u := range s.urls {
					if s.probe(ob, u) {
						passed++
					}
					if passed >= s.quorum {
						healthy = true
						break
					}
					if passed+len(s.urls)-n-1 < s.quorum {
						break
					}
				}
			}
			s.mu.Lock()
			s.healthy[i] = healthy
			s.mu.Unlock()
			if healthy {
				s.readyOnce.Do(func() { close(s.ready) })
			}
		}(i, tag)
	}
	wg.Wait()
}

func (s *fallback) probe(ob adapter.Outbound, u *url.URL) bool {
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	if u.Scheme == "socks5" {
		conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr(u.Host))
		if err != nil {
			return false
		}
		defer conn.Close()
		deadline, _ := ctx.Deadline()
		_ = conn.SetDeadline(deadline)
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		if _, err := conn.Write([]byte{5, 2, 0, 2}); err != nil {
			return false
		}
		var reply [2]byte
		_, err = io.ReadFull(conn, reply[:])
		return err == nil && reply[0] == 5 && (reply[1] == 0 || reply[1] == 2)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return ob.DialContext(ctx, network, M.ParseSocksaddr(address))
		},
		DisableKeepAlives: true, TLSHandshakeTimeout: s.timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return s.expected[resp.StatusCode]
}

func (s *fallback) Now() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, ok := range s.healthy {
		if ok {
			return s.tags[i]
		}
	}
	return ""
}

func (s *fallback) All() []string { return append([]string(nil), s.tags...) }

func (s *fallback) candidates(ctx context.Context, network string) ([]int, error) {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var candidates []int
	for i, ok := range s.healthy {
		if ok {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("fallback has no healthy landing path")
	}
	return candidates, nil
}

func (s *fallback) failed(i int) { s.mu.Lock(); s.healthy[i] = false; s.mu.Unlock() }

func (s *fallback) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	candidates, err := s.candidates(ctx, network)
	if err != nil {
		return nil, err
	}
	for _, i := range candidates {
		ob, ok := s.manager.Outbound(s.tags[i])
		if !ok {
			s.failed(i)
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, s.timeout)
		conn, dialErr := ob.DialContext(attempt, network, destination)
		cancel()
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
		s.failed(i)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("fallback could not dial landing: %w", err)
}

func (s *fallback) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	candidates, err := s.candidates(ctx, "udp")
	if err != nil {
		return nil, err
	}
	for _, i := range candidates {
		ob, ok := s.manager.Outbound(s.tags[i])
		if !ok {
			continue
		}
		conn, packetErr := ob.ListenPacket(ctx, destination)
		if packetErr == nil {
			return conn, nil
		}
		err = packetErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("fallback could not open UDP: %w", err)
}

func (s *fallback) Close() error {
	s.cancel()
	s.wg.Wait()
	return nil
}
