package freepool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/sagernet/sing-box/adapter"
)

var (
	// ErrAlreadyRunning is returned when a manual refresh is triggered while one is active.
	ErrAlreadyRunning = errors.New("free pool refresh is already running")
)

// RelayPicker resolves a front relay outbound for screening candidates and
// reports how many healthy Resin chain_relay nodes currently exist.
type RelayPicker interface {
	HealthyRelayCount() int
	PickRelay(mode string, platformName string, rng *rand.Rand) (adapter.Outbound, string, error)
}

// SyncResult holds the IDs of Resin resources updated during subscription sync.
type SyncResult struct {
	SubscriptionID string
	PlatformID     string
}

// SubscriptionSyncer writes the screened sing-box JSON subscription into Resin's
// managed local subscription (and optionally ensures a matching Platform).
type SubscriptionSyncer interface {
	SyncFreePool(cfg Config, content string) (SyncResult, error)
}

// SourceRunStat tracks fetch/parse counts for one configured source.
type SourceRunStat struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Enabled       bool   `json:"enabled"`
	RawParsed     int    `json:"raw_parsed"`
	Sanitized     int    `json:"sanitized"`
	Error         string `json:"error,omitempty"`
}

// RunStats captures live progress and final summary counters for a refresh run.
type RunStats struct {
	FetchedSources int             `json:"fetched_sources"`
	FailedSources  int             `json:"failed_sources"`
	RawParsed      int             `json:"raw_parsed"`
	Sanitized      int             `json:"sanitized"`
	Candidates     int             `json:"candidates"`
	Probed         int             `json:"probed"`
	HTTPSPassed    int             `json:"https_passed"`
	SSH22Passed    int             `json:"ssh22_passed"`
	Kept           int             `json:"kept"`
	UsedRelayDesc  string          `json:"used_relay_desc,omitempty"`
	Sources        []SourceRunStat `json:"sources,omitempty"`
}

// StatusResponse is the full JSON view returned by GET /api/v1/free-pool/status.
type StatusResponse struct {
	Config               Config         `json:"config"`
	Running              bool           `json:"running"`
	Phase                string         `json:"phase"`
	LastRunAt            string         `json:"last_run_at,omitempty"`
	LastSuccessAt        string         `json:"last_success_at,omitempty"`
	NextRunAt            string         `json:"next_run_at,omitempty"`
	LastError            string         `json:"last_error,omitempty"`
	RelayNodeCount       int            `json:"relay_node_count"`
	TargetSubscriptionID string         `json:"target_subscription_id,omitempty"`
	TargetPlatformID     string         `json:"target_platform_id,omitempty"`
	Stats                RunStats       `json:"stats"`
	Nodes                []VerifiedNode `json:"nodes"`
}

type persistedCache struct {
	LastRunAt        string         `json:"last_run_at,omitempty"`
	LastSuccessAt    string         `json:"last_success_at,omitempty"`
	LastError        string         `json:"last_error,omitempty"`
	TargetPlatformID string         `json:"target_platform_id,omitempty"`
	Stats            RunStats       `json:"stats"`
	Nodes            []VerifiedNode `json:"nodes"`
	SubscriptionJSON string         `json:"subscription_json,omitempty"`
}

// ServiceConfig wires dependencies for the Free Pool Maintainer.
type ServiceConfig struct {
	StateDir   string
	CacheDir   string
	Downloader netutil.Downloader
	Checker    *Checker
	Relays     RelayPicker
	Syncer     SubscriptionSyncer
}

// Service manages periodic and on-demand scraping, sanitization, front-chained
// verification (HTTPS anti-MITM + default port-22 SSH), and syncing into Resin.
type Service struct {
	configPath string
	cachePath  string
	downloader netutil.Downloader
	checker    *Checker
	relays     RelayPicker
	syncer     SubscriptionSyncer

	mu               sync.RWMutex
	cfg              Config
	running          bool
	phase            string
	runCancel        context.CancelFunc
	lastRunAt        time.Time
	lastSuccessAt    time.Time
	nextRunAt        time.Time
	lastError        string
	targetPlatformID string
	stats            RunStats
	nodes            []VerifiedNode
	subscriptionJSON string

	stopCh     chan struct{}
	reschedCh  chan struct{}
	wg         sync.WaitGroup
	started    atomic.Bool
	stopped    atomic.Bool
}

// NewService constructs a Service and loads persisted config/cache from disk.
func NewService(sc ServiceConfig) (*Service, error) {
	var configPath, cachePath string
	if sc.StateDir != "" {
		configPath = filepath.Join(sc.StateDir, "freepool.json")
	}
	if sc.CacheDir != "" {
		cachePath = filepath.Join(sc.CacheDir, "freepool_state.json")
	}

	cfg, err := loadConfigFile(configPath)
	if err != nil {
		log.Printf("FreePool: failed to load %s, using defaults: %v", configPath, err)
		cfg = DefaultConfig()
	}

	s := &Service{
		configPath: configPath,
		cachePath:  cachePath,
		downloader: sc.Downloader,
		checker:    sc.Checker,
		relays:     sc.Relays,
		syncer:     sc.Syncer,
		cfg:        cfg,
		phase:      "idle",
		nodes:      []VerifiedNode{},
		stopCh:     make(chan struct{}),
		reschedCh:  make(chan struct{}, 1),
	}
	s.loadCache()
	return s, nil
}

func (s *Service) loadCache() {
	if s.cachePath == "" {
		return
	}
	data, err := os.ReadFile(s.cachePath)
	if err != nil {
		return
	}
	var pc persistedCache
	if err := json.Unmarshal(data, &pc); err != nil {
		return
	}
	if t, err := time.Parse(time.RFC3339Nano, pc.LastRunAt); err == nil {
		s.lastRunAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, pc.LastSuccessAt); err == nil {
		s.lastSuccessAt = t
	}
	s.lastError = pc.LastError
	s.targetPlatformID = pc.TargetPlatformID
	s.stats = pc.Stats
	if pc.Nodes != nil {
		s.nodes = pc.Nodes
	}
	s.subscriptionJSON = pc.SubscriptionJSON
}

func (s *Service) saveCacheLocked() {
	if s.cachePath == "" {
		return
	}
	pc := persistedCache{
		LastError:        s.lastError,
		TargetPlatformID: s.targetPlatformID,
		Stats:            s.stats,
		Nodes:            s.nodes,
		SubscriptionJSON: s.subscriptionJSON,
	}
	if !s.lastRunAt.IsZero() {
		pc.LastRunAt = s.lastRunAt.UTC().Format(time.RFC3339Nano)
	}
	if !s.lastSuccessAt.IsZero() {
		pc.LastSuccessAt = s.lastSuccessAt.UTC().Format(time.RFC3339Nano)
	}
	if err := writeJSONAtomically(s.cachePath, pc); err != nil {
		log.Printf("FreePool: persist cache error: %v", err)
	}
}

// Start starts the background maintenance scheduler loop.
func (s *Service) Start() {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	s.wg.Add(1)
	go s.schedulerLoop()
}

// Stop cancels any active run and stops the scheduler.
func (s *Service) Stop() {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	s.CancelRun()
	close(s.stopCh)
	s.wg.Wait()
}

func (s *Service) schedulerLoop() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		enabled := s.cfg.Enabled
		interval := s.cfg.intervalDuration()
		lastRun := s.lastRunAt
		var wait time.Duration
		if enabled {
			if lastRun.IsZero() {
				wait = 10 * time.Second
			} else {
				next := lastRun.Add(interval)
				wait = time.Until(next)
				if wait < 5*time.Second {
					wait = 5 * time.Second
				}
			}
			s.nextRunAt = time.Now().Add(wait)
		} else {
			s.nextRunAt = time.Time{}
		}
		s.mu.Unlock()

		if !enabled {
			select {
			case <-s.stopCh:
				return
			case <-s.reschedCh:
				continue
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-s.stopCh:
			timer.Stop()
			return
		case <-s.reschedCh:
			timer.Stop()
			continue
		case <-timer.C:
			_, _ = s.triggerRun(true)
		}
	}
}

func (s *Service) signalReschedule() {
	select {
	case s.reschedCh <- struct{}{}:
	default:
	}
}

// Config returns a copy of the current configuration.
func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

// UpdateConfig validates and persists an updated configuration.
func (s *Service) UpdateConfig(next Config) (Config, error) {
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	s.mu.Lock()
	if next.TargetSubscriptionID == "" {
		next.TargetSubscriptionID = s.cfg.TargetSubscriptionID
	}
	if err := writeJSONAtomically(s.configPath, next); err != nil {
		s.mu.Unlock()
		return Config{}, fmt.Errorf("persist free pool config: %w", err)
	}
	s.cfg = next.clone()
	out := s.cfg.clone()
	s.mu.Unlock()
	s.signalReschedule()
	return out, nil
}

// Status returns a full snapshot of configuration, live progress, and last verified nodes.
func (s *Service) Status() StatusResponse {
	s.mu.RLock()
	cfg := s.cfg.clone()
	running := s.running
	phase := s.phase
	lastRunAt := s.lastRunAt
	lastSuccessAt := s.lastSuccessAt
	nextRunAt := s.nextRunAt
	lastError := s.lastError
	targetPlatformID := s.targetPlatformID
	stats := s.stats
	if stats.Sources != nil {
		cp := make([]SourceRunStat, len(stats.Sources))
		copy(cp, stats.Sources)
		stats.Sources = cp
	}
	nodes := make([]VerifiedNode, len(s.nodes))
	for i, n := range s.nodes {
		n.RawOptions = nil // keep status payload compact
		nodes[i] = n
	}
	s.mu.RUnlock()

	relayCount := 0
	if s.relays != nil {
		relayCount = s.relays.HealthyRelayCount()
	}

	resp := StatusResponse{
		Config:               cfg,
		Running:              running,
		Phase:                phase,
		LastError:            lastError,
		RelayNodeCount:       relayCount,
		TargetSubscriptionID: cfg.TargetSubscriptionID,
		TargetPlatformID:     targetPlatformID,
		Stats:                stats,
		Nodes:                nodes,
	}
	if !lastRunAt.IsZero() {
		resp.LastRunAt = lastRunAt.UTC().Format(time.RFC3339Nano)
	}
	if !lastSuccessAt.IsZero() {
		resp.LastSuccessAt = lastSuccessAt.UTC().Format(time.RFC3339Nano)
	}
	if !nextRunAt.IsZero() {
		resp.NextRunAt = nextRunAt.UTC().Format(time.RFC3339Nano)
	}
	return resp
}

// SubscriptionJSON returns the latest screened sing-box JSON subscription.
func (s *Service) SubscriptionJSON() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.subscriptionJSON != "" {
		return s.subscriptionJSON
	}
	return `{"outbounds":[]}`
}

// TriggerRunAsync starts a refresh run in the background.
func (s *Service) TriggerRunAsync() error {
	_, err := s.triggerRun(false)
	return err
}

// RunSync runs a complete refresh synchronously and returns when finished.
func (s *Service) RunSync(ctx context.Context) error {
	done, err := s.triggerRunWithContext(ctx, false)
	if err != nil {
		return err
	}
	select {
	case <-done:
		s.mu.RLock()
		lastErr := s.lastError
		s.mu.RUnlock()
		if lastErr != "" {
			return errors.New(lastErr)
		}
		return nil
	case <-ctx.Done():
		s.CancelRun()
		<-done
		return ctx.Err()
	}
}

// CancelRun cancels any active refresh run.
func (s *Service) CancelRun() bool {
	s.mu.Lock()
	cancel := s.runCancel
	running := s.running
	s.mu.Unlock()
	if running && cancel != nil {
		cancel()
		return true
	}
	return false
}

func (s *Service) triggerRun(scheduled bool) (<-chan struct{}, error) {
	return s.triggerRunWithContext(context.Background(), scheduled)
}

func (s *Service) triggerRunWithContext(parent context.Context, scheduled bool) (<-chan struct{}, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	cfg := s.cfg.clone()
	ctx, cancel := context.WithCancel(parent)
	s.running = true
	s.phase = "fetching"
	s.runCancel = cancel
	s.lastRunAt = time.Now()
	s.lastError = ""
	s.stats = RunStats{}
	s.mu.Unlock()

	done := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		defer cancel()
		s.executeRun(ctx, cfg, scheduled)
	}()
	return done, nil
}

func (s *Service) executeRun(ctx context.Context, cfg Config, scheduled bool) {
	var runErr error
	defer func() {
		s.mu.Lock()
		s.running = false
		s.runCancel = nil
		if runErr != nil {
			s.phase = "error"
			s.lastError = runErr.Error()
		} else {
			s.phase = "idle"
			s.lastError = ""
			s.lastSuccessAt = time.Now()
		}
		s.saveCacheLocked()
		s.mu.Unlock()
		if !scheduled {
			s.signalReschedule()
		}
	}()

	// 1. Fetch & parse enabled sources in parallel.
	enabledSources := make([]Source, 0, len(cfg.Sources))
	for _, src := range cfg.Sources {
		if src.Enabled {
			enabledSources = append(enabledSources, src)
		}
	}
	if len(enabledSources) == 0 {
		runErr = errors.New("no enabled free sources configured")
		return
	}

	bySource := make([][]Candidate, len(enabledSources))
	sourceStats := make([]SourceRunStat, len(enabledSources))
	var fetchWG sync.WaitGroup
	for i, src := range enabledSources {
		sourceStats[i] = SourceRunStat{
			ID:      src.ID,
			Name:    src.Name,
			Kind:    src.Kind,
			Enabled: src.Enabled,
		}
		fetchWG.Add(1)
		go func(idx int, src Source) {
			defer fetchWG.Done()
			if s.downloader == nil {
				sourceStats[idx].Error = "downloader unavailable"
				return
			}
			fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			body, err := s.downloader.Download(fetchCtx, src.URL)
			cancel()
			if err != nil {
				sourceStats[idx].Error = err.Error()
				return
			}
			cands, rawCount := ParseAndSanitizeSource(src, body, cfg.EnforceStrictTLS)
			bySource[idx] = cands
			sourceStats[idx].RawParsed = rawCount
			sourceStats[idx].Sanitized = len(cands)
		}(i, src)
	}
	fetchWG.Wait()
	if ctx.Err() != nil {
		runErr = ctx.Err()
		return
	}

	var fetchedOK, failedSrc, totalRaw, totalSanitized int
	for _, st := range sourceStats {
		if st.Error != "" {
			failedSrc++
		} else {
			fetchedOK++
		}
		totalRaw += st.RawParsed
		totalSanitized += st.Sanitized
	}

	candidates := SampleAndDeduplicate(bySource, cfg.MaxCandidates)

	s.mu.Lock()
	s.phase = "probing"
	s.stats = RunStats{
		FetchedSources: fetchedOK,
		FailedSources:  failedSrc,
		RawParsed:      totalRaw,
		Sanitized:      totalSanitized,
		Candidates:     len(candidates),
		Sources:        sourceStats,
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		if failedSrc == len(enabledSources) {
			runErr = errors.New("all enabled free sources failed to download")
		} else {
			runErr = errors.New("no valid candidates parsed from free sources")
		}
		return
	}

	// Verify at least one relay is available before spinning up workers (unless direct mode).
	relayDesc := "direct"
	if cfg.RelayMode != RelayModeDirect {
		if s.relays == nil {
			runErr = errors.New("relay picker unavailable")
			return
		}
		_, desc, err := s.relays.PickRelay(cfg.RelayMode, cfg.RelayPlatform, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())))
		if err != nil {
			runErr = fmt.Errorf("select front relay (%s): %w", cfg.RelayMode, err)
			return
		}
		relayDesc = desc
	}

	s.mu.Lock()
	s.stats.UsedRelayDesc = relayDesc
	s.mu.Unlock()

	// 2. Concurrent verification through front relay.
	workers := cfg.Concurrency
	if workers > len(candidates) {
		workers = len(candidates)
	}
	if workers < 1 {
		workers = 1
	}

	candCh := make(chan Candidate)
	earlyStopTarget := cfg.MaxKeepNodes + 20
	if halfMore := cfg.MaxKeepNodes * 3 / 2; halfMore > earlyStopTarget {
		earlyStopTarget = halfMore
	}
	var (
		probedCount atomic.Int64
		httpsCount  atomic.Int64
		ssh22Count  atomic.Int64
		passedCount atomic.Int64
		verifiedMu  sync.Mutex
		verified    []VerifiedNode
		probeWG     sync.WaitGroup
	)

	for w := 0; w < workers; w++ {
		probeWG.Add(1)
		go func(seed uint64) {
			defer probeWG.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(time.Now().UnixNano())))
			for cand := range candCh {
				if ctx.Err() != nil {
					return
				}
				var front adapter.Outbound
				if cfg.RelayMode != RelayModeDirect && s.relays != nil {
					ob, _, err := s.relays.PickRelay(cfg.RelayMode, cfg.RelayPlatform, rng)
					if err != nil {
						probedCount.Add(1)
						continue
					}
					front = ob
				}
				outcome := s.checker.Verify(ctx, cand, front, cfg)
				doneCount := int(probedCount.Add(1))
				if outcome.HTTPSOK {
					httpsCount.Add(1)
				}
				if outcome.SSH22OK {
					ssh22Count.Add(1)
				}
				if outcome.Err == nil {
					passedCount.Add(1)
					verifiedMu.Lock()
					verified = append(verified, VerifiedNode{
						Hash:       cand.Hash.Hex(),
						Tag:        cand.Tag,
						Protocol:   cand.Protocol,
						Server:     cand.Server,
						ServerPort: cand.ServerPort,
						SourceID:   cand.SourceID,
						SourceName: cand.SourceName,
						LatencyMs:  outcome.LatencyMs,
						SSH22OK:    outcome.SSH22OK,
						SSH22Ms:    outcome.SSH22Ms,
						VerifiedAt: time.Now().UTC().Format(time.RFC3339),
						RawOptions: cand.RawOptions,
					})
					verifiedMu.Unlock()
				}
				if doneCount%4 == 0 || doneCount == len(candidates) {
					s.mu.Lock()
					s.stats.Probed = doneCount
					s.stats.HTTPSPassed = int(httpsCount.Load())
					s.stats.SSH22Passed = int(ssh22Count.Load())
					s.mu.Unlock()
				}
			}
		}(uint64(w + 1))
	}

feedLoop:
	for _, cand := range candidates {
		if int(passedCount.Load()) >= earlyStopTarget {
			break feedLoop
		}
		select {
		case <-ctx.Done():
			break feedLoop
		case candCh <- cand:
		}
	}
	close(candCh)
	probeWG.Wait()

	if ctx.Err() != nil {
		runErr = ctx.Err()
		return
	}

	sort.SliceStable(verified, func(i, j int) bool {
		if verified[i].LatencyMs != verified[j].LatencyMs {
			return verified[i].LatencyMs < verified[j].LatencyMs
		}
		return verified[i].Hash < verified[j].Hash
	})
	if len(verified) > cfg.MaxKeepNodes {
		verified = verified[:cfg.MaxKeepNodes]
	}

	// 3. Render sing-box JSON subscription (optionally injecting detour tag if
	// user disabled chain_proxy and configured a specific detour tag).
	subJSON, err := renderSingboxSubscription(verified, cfg)
	if err != nil {
		runErr = fmt.Errorf("render subscription: %w", err)
		return
	}

	// 4. Sync into Resin managed local subscription.
	s.mu.Lock()
	s.phase = "syncing"
	s.stats.Probed = int(probedCount.Load())
	s.stats.HTTPSPassed = int(httpsCount.Load())
	s.stats.SSH22Passed = int(ssh22Count.Load())
	s.stats.Kept = len(verified)
	s.mu.Unlock()

	var syncRes SyncResult
	if s.syncer != nil && len(verified) > 0 {
		syncRes, err = s.syncer.SyncFreePool(cfg, subJSON)
		if err != nil {
			runErr = fmt.Errorf("sync subscription: %w", err)
			return
		}
	}

	s.mu.Lock()
	s.nodes = verified
	s.subscriptionJSON = subJSON
	if syncRes.SubscriptionID != "" && syncRes.SubscriptionID != s.cfg.TargetSubscriptionID {
		s.cfg.TargetSubscriptionID = syncRes.SubscriptionID
		_ = writeJSONAtomically(s.configPath, s.cfg)
	}
	if syncRes.PlatformID != "" {
		s.targetPlatformID = syncRes.PlatformID
	}
	s.mu.Unlock()

	log.Printf(
		"FreePool refresh complete: sources=%d/%d candidates=%d https=%d ssh22=%d kept=%d relay=%s",
		fetchedOK, len(enabledSources), len(candidates),
		httpsCount.Load(), ssh22Count.Load(), len(verified), relayDesc,
	)
}

func renderSingboxSubscription(nodes []VerifiedNode, cfg Config) (string, error) {
	outbounds := make([]json.RawMessage, 0, len(nodes))
	injectDetour := !cfg.SyncChainProxy && strings.TrimSpace(cfg.SyncDetourTag) != ""

	for _, n := range nodes {
		if len(n.RawOptions) == 0 {
			continue
		}
		if !injectDetour {
			outbounds = append(outbounds, n.RawOptions)
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(n.RawOptions, &obj); err != nil {
			continue
		}
		obj["detour"] = cfg.SyncDetourTag
		raw, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		outbounds = append(outbounds, raw)
	}

	payload := struct {
		Outbounds []json.RawMessage `json:"outbounds"`
	}{
		Outbounds: outbounds,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
