package platform

import (
	"net/netip"
	"regexp"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

// DefaultPlatformID is the well-known UUID of the built-in Default platform.
const DefaultPlatformID = "00000000-0000-0000-0000-000000000000"

// DefaultPlatformName is the built-in platform name.
const DefaultPlatformName = "Default"

// GeoLookupFunc resolves an IP address to a lowercase ISO country code.
type GeoLookupFunc func(netip.Addr) string

// PoolRangeFunc iterates all nodes in the global pool.
type PoolRangeFunc func(fn func(node.Hash, *node.NodeEntry) bool)

// GetEntryFunc retrieves a node entry from the global pool by hash.
type GetEntryFunc func(node.Hash) (*node.NodeEntry, bool)

// Platform represents a routing platform with its filtered routable view.
type Platform struct {
	ID   string
	Name string

	// Filter configuration.
	RegexFilters  []*regexp.Regexp
	RegionFilters []string // lowercase ISO codes, supports negation "!xx"

	// Other config fields.
	StickyTTLNs                      int64
	MaxNodeReferenceLatencyNs        *int64
	ReverseProxyMissAction           string
	ReverseProxyEmptyAccountBehavior string
	ReverseProxyFixedAccountHeader   string
	ReverseProxyFixedAccountHeaders  []string
	AllocationPolicy                 AllocationPolicy
	PassiveCircuitBreakerDisabled    bool

	systemMaxNodeReferenceLatency func() time.Duration
	latencyAuthorities            func() []string

	// Routable view & its lock.
	// viewMu serializes both FullRebuild and NotifyDirty.
	view   *RoutableView
	viewMu sync.Mutex
}

// NewPlatform creates a Platform with an empty routable view.
func NewPlatform(id, name string, regexFilters []*regexp.Regexp, regionFilters []string) *Platform {
	return &Platform{
		ID:            id,
		Name:          name,
		RegexFilters:  regexFilters,
		RegionFilters: regionFilters,
		view:          NewRoutableView(),
	}
}

// SetReferenceLatencyConfig attaches dynamic system-level reference latency
// settings used when this platform inherits the system cap.
func (p *Platform) SetReferenceLatencyConfig(maxLatency func() time.Duration, authorities func() []string) {
	p.systemMaxNodeReferenceLatency = maxLatency
	p.latencyAuthorities = authorities
}

// View returns the platform's routable view as a read-only interface.
// External callers cannot Add/Remove/Clear — only FullRebuild and NotifyDirty can mutate.
func (p *Platform) View() ReadOnlyView {
	return p.view
}

// FullRebuild clears the routable view and re-evaluates all nodes from the pool.
// Acquires viewMu — any concurrent NotifyDirty calls block until rebuild completes.
func (p *Platform) FullRebuild(
	poolRange PoolRangeFunc,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()

	p.view.Clear()
	poolRange(func(h node.Hash, entry *node.NodeEntry) bool {
		if p.evaluateNode(entry, subLookup, geoLookup) {
			p.view.Add(h)
		}
		return true
	})
}

// NotifyDirty re-evaluates a single node and adds/removes it from the view.
// Acquires viewMu — serialized with FullRebuild.
func (p *Platform) NotifyDirty(
	h node.Hash,
	getEntry GetEntryFunc,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()

	entry, ok := getEntry(h)
	if !ok {
		// Node was deleted from pool.
		p.view.Remove(h)
		return
	}

	if p.evaluateNode(entry, subLookup, geoLookup) {
		p.view.Add(h)
	} else {
		p.view.Remove(h)
	}
}

// evaluateNode checks all filter conditions for platform routability.
func (p *Platform) evaluateNode(
	entry *node.NodeEntry,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) bool {
	// 0. Disabled nodes are never routable.
	if entry.IsDisabledBySubscriptions(subLookup) {
		return false
	}

	// 1. Healthy for routing (outbound ready + circuit not open).
	if !entry.IsHealthy() {
		return false
	}

	// 2. Tag regex match.
	if !entry.MatchRegexs(p.RegexFilters, subLookup) {
		return false
	}

	// 3. Egress IP must be known.
	egressIP := entry.GetEgressIP()
	if !egressIP.IsValid() {
		return false
	}

	// 4. Region filter (when configured).
	if len(p.RegionFilters) > 0 {
		region := entry.GetRegion(geoLookup)
		if !MatchRegionFilter(region, p.RegionFilters) {
			return false
		}
	}

	// 5. Has at least one latency record.
	if !entry.HasLatency() {
		return false
	}

	// 6. Optional reference latency cap.
	if maxLatency := p.effectiveMaxNodeReferenceLatency(); maxLatency > 0 {
		referenceLatency, ok := node.AverageEWMAForDomains(entry, p.currentLatencyAuthorities())
		if !ok || referenceLatency > maxLatency {
			return false
		}
	}

	return true
}

func (p *Platform) effectiveMaxNodeReferenceLatency() time.Duration {
	if p.MaxNodeReferenceLatencyNs != nil {
		if *p.MaxNodeReferenceLatencyNs <= 0 {
			return 0
		}
		return time.Duration(*p.MaxNodeReferenceLatencyNs)
	}
	if p.systemMaxNodeReferenceLatency == nil {
		return 0
	}
	latency := p.systemMaxNodeReferenceLatency()
	if latency <= 0 {
		return 0
	}
	return latency
}

func (p *Platform) currentLatencyAuthorities() []string {
	if p.latencyAuthorities == nil {
		return nil
	}
	return p.latencyAuthorities()
}

// MatchRegionFilter applies include/exclude region filters.
// Positive entries (xx) build an include set; negative entries (!xx) build an exclude set.
// Unknown regions never match when region filters are configured.
// Final result is: region known AND (include empty OR region in include) AND (region not in exclude).
func MatchRegionFilter(region string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	if region == "" {
		return false
	}

	included := false
	hasInclude := false

	for _, filter := range filters {
		if len(filter) > 0 && filter[0] == '!' {
			if region == filter[1:] {
				return false
			}
			continue
		}
		hasInclude = true
		if region == filter {
			included = true
		}
	}

	if hasInclude && !included {
		return false
	}
	return true
}
