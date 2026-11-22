package freepool

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/topology"
	"github.com/sagernet/sing-box/adapter"
)

// ResinRelayPicker picks front-proxy outbounds from Resin's live pool/router
// so candidate free nodes are verified through real front proxies.
type ResinRelayPicker struct {
	Pool   *topology.GlobalNodePool
	Router *routing.Router
}

// HealthyRelayCount returns the number of healthy TCP chain_relay nodes in Resin.
func (p *ResinRelayPicker) HealthyRelayCount() int {
	if p == nil || p.Pool == nil {
		return 0
	}
	return p.Pool.RelayView("", "tcp").Size()
}

// PickRelay selects a live front outbound according to mode and platformName.
func (p *ResinRelayPicker) PickRelay(mode string, platformName string, rng *rand.Rand) (adapter.Outbound, string, error) {
	if p == nil || p.Pool == nil {
		return nil, "", errors.New("node pool unavailable")
	}
	if rng == nil {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}

	switch mode {
	case RelayModeDirect:
		return nil, "direct", nil

	case RelayModeRelayOnly:
		ob, count, err := p.pickChainRelay(rng)
		if err != nil {
			return nil, "", err
		}
		return ob, fmt.Sprintf("chain_relay (%d)", count), nil

	case RelayModePlatform:
		target := strings.TrimSpace(platformName)
		if target == "" {
			target = "Default"
		}
		ob, err := p.pickFromPlatform(target)
		if err != nil {
			return nil, "", err
		}
		return ob, "platform:" + target, nil

	default: // RelayModeAuto
		if ob, count, err := p.pickChainRelay(rng); err == nil {
			return ob, fmt.Sprintf("chain_relay (%d)", count), nil
		}
		if target := strings.TrimSpace(platformName); target != "" {
			if ob, err := p.pickFromPlatform(target); err == nil {
				return ob, "platform:" + target, nil
			}
		}
		// Fallback to any routable platform (preferring non-FreePool platforms such as Default).
		if ob, err := p.pickFromPlatform("Default"); err == nil {
			return ob, "platform:Default", nil
		}
		if ob, desc, err := p.pickAnyHealthyOrdinaryNode(rng); err == nil {
			return ob, desc, nil
		}
		return nil, "", errors.New("no healthy front relay available (enable chain_relay on an airport subscription or configure relay_platform)")
	}
}

func (p *ResinRelayPicker) pickChainRelay(rng *rand.Rand) (adapter.Outbound, int, error) {
	view := p.Pool.RelayView("", "tcp")
	count := view.Size()
	if count == 0 {
		return nil, 0, errors.New("no healthy chain_relay TCP nodes available")
	}
	for attempt := 0; attempt < 8; attempt++ {
		h, ok := view.RandomPick(rng)
		if !ok {
			break
		}
		if ob := p.liveOutbound(h); ob != nil {
			return ob, count, nil
		}
	}
	var found adapter.Outbound
	view.Range(func(h node.Hash) bool {
		if ob := p.liveOutbound(h); ob != nil {
			found = ob
			return false
		}
		return true
	})
	if found == nil {
		return nil, count, errors.New("chain_relay nodes have no ready outbound")
	}
	return found, count, nil
}

func (p *ResinRelayPicker) pickFromPlatform(platformName string) (adapter.Outbound, error) {
	if p.Router == nil {
		return nil, errors.New("router unavailable")
	}
	res, err := p.Router.RouteRequest(platformName, "", "")
	if err != nil {
		return nil, err
	}
	ob := p.liveOutbound(res.NodeHash)
	if ob == nil {
		return nil, fmt.Errorf("platform %q node outbound not ready", platformName)
	}
	return ob, nil
}

func (p *ResinRelayPicker) pickAnyHealthyOrdinaryNode(rng *rand.Rand) (adapter.Outbound, string, error) {
	var candidates []node.Hash
	p.Pool.RangeNodes(func(h node.Hash, e *node.NodeEntry) bool {
		if e == nil || !e.IsHealthy() || p.Pool.IsNodeDisabled(h) {
			return true
		}
		if e.RoutingRole() == node.RoleChain {
			return true
		}
		if ptr := e.Outbound.Load(); ptr != nil && *ptr != nil {
			candidates = append(candidates, h)
		}
		return true
	})
	if len(candidates) == 0 {
		return nil, "", errors.New("no healthy ordinary node available")
	}
	h := candidates[rng.IntN(len(candidates))]
	ob := p.liveOutbound(h)
	if ob == nil {
		return nil, "", errors.New("selected node outbound not ready")
	}
	return ob, fmt.Sprintf("pool (%d)", len(candidates)), nil
}

func (p *ResinRelayPicker) liveOutbound(h node.Hash) adapter.Outbound {
	e, ok := p.Pool.GetEntry(h)
	if !ok || e == nil || !e.IsHealthy() {
		return nil
	}
	ptr := e.Outbound.Load()
	if ptr == nil || *ptr == nil {
		return nil
	}
	return *ptr
}
