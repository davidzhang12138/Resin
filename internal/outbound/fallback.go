package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
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
	ProbeLandings  []fallbackLanding  `json:"probe_landings,omitempty"`
	ProbeQuorum    int                `json:"probe_quorum,omitempty"`
	ExpectedStatus string             `json:"expected_status,omitempty"`
	Interval       badoption.Duration `json:"interval,omitempty"`
	Timeout        badoption.Duration `json:"timeout,omitempty"`
	PrimaryCount   *int               `json:"primary_count,omitempty"`
	DialTimeout    badoption.Duration `json:"dial_timeout,omitempty"`
	DialBudget     badoption.Duration `json:"dial_budget,omitempty"`
}

func registerFallback(registry *sbOutbound.Registry) {
	sbOutbound.Register[fallbackOptions](registry, "fallback", newFallback)
}

type fallbackPath struct {
	front   int
	landing string
}

type fallbackPathHealth struct {
	delay time.Duration
	until time.Time
}

const (
	fallbackLatencyTolerance = 50 * time.Millisecond
	fallbackSwitchInterval   = 30 * time.Second
	fallbackPreferenceTTL    = 5 * time.Minute
)

type fallbackTier struct {
	landing string
	primary bool
}

type fallbackPreference struct {
	front      int
	selectedAt time.Time
	lastUsed   time.Time
}

type fallback struct {
	sbOutbound.Adapter
	ctx            context.Context
	cancel         context.CancelFunc
	manager        adapter.OutboundManager
	tags           []string
	probeOrder     []int
	urls           []*url.URL
	landings       []fallbackLanding
	landingIndices map[string]int
	probeTarget    M.Socksaddr
	quorum         int
	expected       map[int]bool
	interval       time.Duration
	timeout        time.Duration
	tiered         bool
	primaryCount   int
	dialTimeout    time.Duration
	dialBudget     time.Duration
	mu             sync.RWMutex
	healthy        []bool
	delays         []time.Duration
	landingDelays  [][]time.Duration
	paths          map[fallbackPath]fallbackPathHealth
	preferred      map[fallbackTier]fallbackPreference
	ready          chan struct{}
	readyOnce      sync.Once
	wg             sync.WaitGroup
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
	dialTimeout, dialBudget := time.Duration(options.DialTimeout), time.Duration(options.DialBudget)
	if dialTimeout == 0 {
		dialTimeout = timeout
	}
	if dialTimeout < 0 || dialBudget < 0 || (dialBudget > 0 && dialBudget <= dialTimeout) {
		return nil, errors.New("fallback dial budget must exceed the per-attempt timeout")
	}
	primaryCount := len(options.Outbounds)
	tiered := options.PrimaryCount != nil
	if tiered {
		primaryCount = *options.PrimaryCount
		if primaryCount < 0 || primaryCount > len(options.Outbounds) {
			return nil, errors.New("invalid fallback primary count")
		}
	}
	var urls []*url.URL
	var probeTarget M.Socksaddr
	landingIndices := make(map[string]int)
	if len(options.ProbeLandings) > 0 {
		target, err := url.Parse(options.URL)
		if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil {
			return nil, errors.New("authenticated fallback probes require an HTTP or HTTPS target")
		}
		port := target.Port()
		if port == "" {
			port = "443"
			if target.Scheme == "http" {
				port = "80"
			}
		}
		probeTarget = M.ParseSocksaddr(net.JoinHostPort(target.Hostname(), port))
		for i, landing := range options.ProbeLandings {
			if landing.Server == "" || landing.Port == 0 {
				return nil, errors.New("fallback probe landing requires a server and port")
			}
			landingIndices[landing.address().String()] = i
		}
	} else {
		links := options.ProbeURLs
		if len(links) == 0 {
			links = []string{options.URL}
		}
		for _, link := range links {
			u, err := url.Parse(link)
			if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
				return nil, errors.New("invalid fallback probe URL")
			}
			if u.Scheme == "socks5" && u.Port() == "" {
				return nil, errors.New("SOCKS5 probe requires an explicit port")
			}
			urls = append(urls, u)
		}
	}
	probeCount := len(urls) + len(options.ProbeLandings)
	quorum := options.ProbeQuorum
	if quorum == 0 {
		quorum = probeCount
	}
	if quorum < 1 || quorum > probeCount {
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

	// Interleave the tiers when starting probes. Selection below still gives
	// primary paths priority.
	probeOrder := make([]int, 0, len(options.Outbounds))
	for p, b := 0, primaryCount; p < primaryCount || b < len(options.Outbounds); {
		if p < primaryCount {
			probeOrder = append(probeOrder, p)
			p++
		}
		if b < len(options.Outbounds) {
			probeOrder = append(probeOrder, b)
			b++
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	return &fallback{
		Adapter: sbOutbound.NewAdapter("fallback", tag, []string{"tcp", "udp"}, options.Outbounds),
		ctx:     ctx, cancel: cancel, manager: service.FromContext[adapter.OutboundManager](ctx),
		tags: append([]string(nil), options.Outbounds...), probeOrder: probeOrder, urls: urls,
		landings: options.ProbeLandings, landingIndices: landingIndices, probeTarget: probeTarget,
		quorum: quorum, expected: expected, interval: interval, timeout: timeout,
		tiered: tiered, primaryCount: primaryCount, dialTimeout: dialTimeout, dialBudget: dialBudget,
		healthy: make([]bool, len(options.Outbounds)), delays: make([]time.Duration, len(options.Outbounds)),
		landingDelays: make([][]time.Duration, len(options.Outbounds)),
		paths:         make(map[fallbackPath]fallbackPathHealth), ready: make(chan struct{}),
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
	for _, i := range s.probeOrder {
		select {
		case sem <- struct{}{}:
		case <-s.ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ob, ok := s.manager.Outbound(s.tags[i])
			if ok {
				s.checkFront(i, ob)
			} else {
				s.mu.Lock()
				s.healthy[i] = false
				s.delays[i] = 0
				s.landingDelays[i] = nil
				s.mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	s.mu.Lock()
	now := time.Now()
	for path, health := range s.paths {
		if !now.Before(health.until) {
			delete(s.paths, path)
		}
	}
	for tier, preferred := range s.preferred {
		if now.Sub(preferred.lastUsed) >= fallbackPreferenceTTL {
			delete(s.preferred, tier)
		}
	}
	s.mu.Unlock()
}

func (s *fallback) checkFront(front int, ob adapter.Outbound) {
	if len(s.landings) > 0 {
		// Spread the initial checks across destinations, and publish each
		// result immediately so a slow landing cannot delay a working path.
		for offset := range s.landings {
			if s.ctx.Err() != nil {
				break
			}
			i := (front + offset) % len(s.landings)
			landing := s.landings[i]
			ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
			start := time.Now()
			conn, err := dialFallbackSOCKS(ctx, ob, landing.address(), s.probeTarget, landing.Username, landing.Password)
			cancel()
			delay := -time.Nanosecond
			if err == nil {
				_ = conn.Close()
				delay = time.Since(start)
			}
			s.recordLandingProbe(front, i, delay)
		}
		return
	}

	passed := 0
	var total time.Duration
	for i, u := range s.urls {
		start := time.Now()
		if s.probe(ob, u) {
			passed++
			total += time.Since(start)
		}
		if passed >= s.quorum || passed+len(s.urls)-i-1 < s.quorum {
			break
		}
	}
	s.mu.Lock()
	s.healthy[front] = passed >= s.quorum
	s.delays[front] = 0
	if passed > 0 {
		s.delays[front] = total / time.Duration(passed)
	}
	s.mu.Unlock()
	if passed >= s.quorum {
		s.readyOnce.Do(func() { close(s.ready) })
	}
}

func (s *fallback) recordLandingProbe(front, sample int, delay time.Duration) {
	s.mu.Lock()
	if s.landingDelays[front] == nil {
		s.landingDelays[front] = make([]time.Duration, len(s.landings))
	}
	s.landingDelays[front][sample] = delay
	passed := 0
	var total time.Duration
	for _, measured := range s.landingDelays[front] {
		if measured > 0 {
			passed++
			total += measured
		}
	}
	s.healthy[front] = passed >= s.quorum
	s.delays[front] = 0
	if passed > 0 {
		s.delays[front] = total / time.Duration(passed)
	}
	s.mu.Unlock()
	if delay > 0 {
		s.readyOnce.Do(func() { close(s.ready) })
	}
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
	candidates := s.rankedLocked("")
	if len(candidates) == 0 {
		return ""
	}
	return s.tags[candidates[0]]
}

func (s *fallback) All() []string { return append([]string(nil), s.tags...) }

// A successful actual dial or probe for the same landing is the best
// evidence. Failed samples never veto a different landing.
func (s *fallback) rankedLocked(landing string) []int {
	index, sampled := s.landingIndices[landing]
	now := time.Now()
	type candidate struct {
		front    int
		evidence int
		delay    time.Duration
	}
	var ranked []candidate
	for i := range s.tags {
		switch health := s.paths[fallbackPath{i, landing}]; {
		case now.Before(health.until):
			if health.delay > 0 {
				ranked = append(ranked, candidate{i, 0, health.delay})
			}
		case sampled && len(s.landingDelays[i]) > index && s.landingDelays[i][index] != 0:
			if delay := s.landingDelays[i][index]; delay > 0 {
				ranked = append(ranked, candidate{i, 0, delay})
			}
		case s.healthy[i]:
			ranked = append(ranked, candidate{i, 0, s.delays[i]})
		case landing != "" && len(s.landingDelays[i]) > 0:
			// A different landing may work even when all sample paths failed.
			ranked = append(ranked, candidate{i, 2, s.delays[i]})
		}
	}

	if s.tiered {
		slices.SortStableFunc(ranked, func(i, j candidate) int {
			mainI, mainJ := i.front < s.primaryCount, j.front < s.primaryCount
			if mainI != mainJ {
				if mainI {
					return -1
				}
				return 1
			}
			if i.evidence != j.evidence {
				return i.evidence - j.evidence
			}
			switch {
			case i.delay == j.delay:
				return 0
			case j.delay == 0 || (i.delay > 0 && i.delay < j.delay):
				return -1
			default:
				return 1
			}
		})
		// Apply stability within each tier. An unavailable path was already
		// excluded, and a recovered primary still comes before standby paths.
		if landing != "" {
			preferCurrent := func(tier []candidate, primary bool) {
				selected, ok := s.preferred[fallbackTier{landing: landing, primary: primary}]
				if !ok || len(tier) < 2 || now.Sub(selected.lastUsed) >= fallbackPreferenceTTL {
					return
				}
				best := tier[0]
				position := slices.IndexFunc(tier, func(c candidate) bool { return c.front == selected.front })
				if position <= 0 {
					return
				}
				current := tier[position]
				if current.evidence != 0 || current.delay <= 0 || best.delay <= 0 {
					return
				}
				if current.delay-best.delay > fallbackLatencyTolerance && now.Sub(selected.selectedAt) >= fallbackSwitchInterval {
					return
				}
				copy(tier[1:position+1], tier[:position])
				tier[0] = current
			}
			firstBackup := slices.IndexFunc(ranked, func(c candidate) bool { return c.front >= s.primaryCount })
			if firstBackup < 0 {
				firstBackup = len(ranked)
			}
			preferCurrent(ranked[:firstBackup], true)
			preferCurrent(ranked[firstBackup:], false)
		}
	}
	candidates := make([]int, len(ranked))
	for i, item := range ranked {
		candidates[i] = item.front
	}
	return candidates
}

func (s *fallback) candidates(ctx context.Context, landing string) ([]int, error) {
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
	candidates := s.rankedLocked(landing)
	if len(candidates) == 0 {
		return nil, errors.New("fallback has no healthy landing path")
	}
	return candidates, nil
}

func (s *fallback) recordPath(front int, landing string, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fallbackPath{front, landing}
	if _, exists := s.paths[key]; !exists && len(s.paths) >= 8192 {
		return
	}
	ttl := s.interval
	if delay == 0 {
		ttl = min(ttl, 10*time.Second)
	}
	now := time.Now()
	s.paths[key] = fallbackPathHealth{delay, now.Add(ttl)}
	if !s.tiered || delay <= 0 {
		return
	}
	tier := fallbackTier{landing: landing, primary: front < s.primaryCount}
	selected, exists := s.preferred[tier]
	if exists && selected.front != front && now.Add(-delay).Before(selected.selectedAt) {
		// A dial started before a newer selection must not move it back.
		return
	}
	if !exists || selected.front != front {
		selected = fallbackPreference{front: front, selectedAt: now}
	}
	selected.lastUsed = now
	if s.preferred == nil {
		s.preferred = make(map[fallbackTier]fallbackPreference)
	}
	s.preferred[tier] = selected
}

func (s *fallback) dial(ctx context.Context, landing M.Socksaddr, connect func(context.Context, adapter.Outbound) (net.Conn, error)) (net.Conn, error) {
	key := landing.String()
	choices, err := s.candidates(ctx, key)
	if err != nil {
		return nil, err
	}
	if s.dialBudget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.dialBudget)
		defer cancel()
	}
	primaryContext := ctx
	if s.dialBudget > 0 && choices[0] < s.primaryCount && choices[len(choices)-1] >= s.primaryCount {
		deadline, _ := ctx.Deadline()
		mainUntil := deadline.Add(-s.dialTimeout)
		// Preserve a primary attempt if the caller supplied a short deadline.
		if halfway := time.Now().Add(time.Until(deadline) / 2); mainUntil.Before(halfway) {
			mainUntil = halfway
		}
		var cancel context.CancelFunc
		primaryContext, cancel = context.WithDeadline(ctx, mainUntil)
		defer cancel()
	}
	for _, i := range choices {
		parent := ctx
		if i < s.primaryCount {
			parent = primaryContext
		}
		if parent.Err() != nil {
			err = parent.Err()
			continue
		}
		ob, ok := s.manager.Outbound(s.tags[i])
		if !ok {
			err = errors.New("fallback front is unavailable")
			continue
		}
		attempt, cancel := context.WithTimeout(parent, s.dialTimeout)
		start := time.Now()
		conn, dialErr := connect(attempt, ob)
		cancel()
		if dialErr == nil {
			if s.tiered {
				s.recordPath(i, key, time.Since(start))
			}
			return conn, nil
		}
		if errors.Is(dialErr, errFallbackTargetRejected) {
			return nil, dialErr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		err = dialErr
		s.recordPath(i, key, 0)
	}
	return nil, fmt.Errorf("fallback could not dial landing: %w", err)
}

func (s *fallback) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return s.dial(ctx, destination, func(attempt context.Context, front adapter.Outbound) (net.Conn, error) {
		return front.DialContext(attempt, network, destination)
	})
}

func (s *fallback) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	candidates, err := s.candidates(ctx, destination.String())
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
