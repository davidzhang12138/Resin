package service

import (
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
)

// ------------------------------------------------------------------
// Leases
// ------------------------------------------------------------------

// LeaseResponse is the API response for a lease.
type LeaseResponse struct {
	PlatformID   string `json:"platform_id"`
	Account      string `json:"account"`
	NodeHash     string `json:"node_hash"`
	NodeTag      string `json:"node_tag"`
	EgressIP     string `json:"egress_ip"`
	Expiry       string `json:"expiry"`
	LastAccessed string `json:"last_accessed"`
}

func leaseToResponse(lease model.Lease, nodeTag string) LeaseResponse {
	return LeaseResponse{
		PlatformID:   lease.PlatformID,
		Account:      lease.Account,
		NodeHash:     lease.NodeHash,
		NodeTag:      nodeTag,
		EgressIP:     lease.EgressIP,
		Expiry:       time.Unix(0, lease.ExpiryNs).UTC().Format(time.RFC3339Nano),
		LastAccessed: time.Unix(0, lease.LastAccessedNs).UTC().Format(time.RFC3339Nano),
	}
}

func (s *ControlPlaneService) resolveLeaseNodeTag(hash node.Hash) string {
	if s == nil || s.Pool == nil {
		return ""
	}
	return s.Pool.ResolveNodeDisplayTag(hash)
}

func (s *ControlPlaneService) resolveLeaseNodeTagFromHex(hashHex string) string {
	hash, err := node.ParseHex(hashHex)
	if err != nil {
		return ""
	}
	return s.resolveLeaseNodeTag(hash)
}

// ListLeases returns all leases for a platform.
func (s *ControlPlaneService) ListLeases(platformID string) ([]LeaseResponse, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}
	var result []LeaseResponse
	s.Router.RangeLeases(platformID, func(account string, lease routing.Lease) bool {
		result = append(result, leaseToResponse(model.Lease{
			PlatformID:     platformID,
			Account:        account,
			NodeHash:       lease.NodeHash.Hex(),
			EgressIP:       lease.EgressIP.String(),
			ExpiryNs:       lease.ExpiryNs,
			LastAccessedNs: lease.LastAccessedNs,
		}, s.resolveLeaseNodeTag(lease.NodeHash)))
		return true
	})
	if result == nil {
		result = []LeaseResponse{}
	}
	return result, nil
}

// GetLease returns a single lease.
func (s *ControlPlaneService) GetLease(platformID, account string) (*LeaseResponse, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}
	ml := s.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
	if ml == nil {
		return nil, notFound("lease not found")
	}
	resp := leaseToResponse(*ml, s.resolveLeaseNodeTagFromHex(ml.NodeHash))
	return &resp, nil
}

// ReassignLease moves an existing lease to a different routable node on the same platform.
// It recomputes the egress IP from the target node, renews the expiry to now + platform
// sticky TTL, and preserves the original account and created-at. The replacement is
// persisted via Router.UpsertLease (emits LeaseReplace -> marks dirty -> flushes cache.db).
func (s *ControlPlaneService) ReassignLease(platformID, account, targetNodeHash string) (*LeaseResponse, error) {
	plat, ok := s.Pool.GetPlatform(platformID)
	if !ok {
		return nil, notFound("platform not found")
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, invalidArg("account: must be non-empty")
	}
	targetNodeHash = strings.TrimSpace(targetNodeHash)
	if targetNodeHash == "" {
		return nil, invalidArg("node_hash: must be non-empty")
	}

	targetHash, err := node.ParseHex(targetNodeHash)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	if !plat.View().Contains(targetHash) {
		return nil, invalidArg("node not routable on this platform")
	}
	entry, ok := s.Pool.GetEntry(targetHash)
	if !ok {
		return nil, invalidArg("node not routable on this platform")
	}
	egressIP := entry.GetEgressIP()
	if !egressIP.IsValid() {
		return nil, invalidArg("target node has no egress IP")
	}

	current := s.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
	if current == nil {
		return nil, notFound("lease not found")
	}
	nowNs := time.Now().UnixNano()
	if current.ExpiryNs < nowNs {
		return nil, notFound("lease not found")
	}

	next := *current
	next.NodeHash = targetHash.Hex()
	next.EgressIP = egressIP.String()
	next.ExpiryNs = nowNs + plat.StickyTTLNs
	next.LastAccessedNs = nowNs

	if err := s.Router.UpsertLease(next); err != nil {
		return nil, internal("reassign lease", err)
	}

	resp := leaseToResponse(model.Lease{
		PlatformID:     next.PlatformID,
		Account:        next.Account,
		NodeHash:       next.NodeHash,
		EgressIP:       next.EgressIP,
		ExpiryNs:       next.ExpiryNs,
		LastAccessedNs: next.LastAccessedNs,
	}, s.resolveLeaseNodeTag(targetHash))
	return &resp, nil
}

// InheritLeaseByPlatformName copies a valid parent lease onto newAccount.
func (s *ControlPlaneService) InheritLeaseByPlatformName(platformName, parentAccount, newAccount string) error {
	platformName = strings.TrimSpace(platformName)
	if platformName == "" {
		return invalidArg("platform: must be non-empty")
	}
	parentAccount = strings.TrimSpace(parentAccount)
	if parentAccount == "" {
		return invalidArg("parent_account: must be non-empty")
	}
	newAccount = strings.TrimSpace(newAccount)
	if newAccount == "" {
		return invalidArg("new_account: must be non-empty")
	}
	if parentAccount == newAccount {
		return invalidArg("new_account: must differ from parent_account")
	}

	plat, ok := s.Pool.GetPlatformByName(platformName)
	if !ok || plat == nil {
		return notFound("platform not found")
	}

	parentLease := s.Router.ReadLease(model.LeaseKey{
		PlatformID: plat.ID,
		Account:    parentAccount,
	})
	nowNs := time.Now().UnixNano()
	if parentLease == nil || parentLease.ExpiryNs < nowNs {
		return notFound("parent lease not found")
	}

	next := *parentLease
	next.Account = newAccount
	if err := s.Router.UpsertLease(next); err != nil {
		return internal("inherit lease", err)
	}

	return nil
}

// DeleteLease removes a single lease.
func (s *ControlPlaneService) DeleteLease(platformID, account string) error {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return notFound("platform not found")
	}
	if !s.Router.DeleteLease(platformID, account) {
		return notFound("lease not found")
	}
	return nil
}

// DeleteAllLeases removes all leases for a platform.
func (s *ControlPlaneService) DeleteAllLeases(platformID string) error {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return notFound("platform not found")
	}
	s.Router.DeleteAllLeases(platformID)
	return nil
}

// IPLoadEntry is the API response for IP load stats.
type IPLoadEntry struct {
	EgressIP   string `json:"egress_ip"`
	LeaseCount int64  `json:"lease_count"`
}

// GetIPLoad returns IP load stats for a platform.
func (s *ControlPlaneService) GetIPLoad(platformID string) ([]IPLoadEntry, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}
	snapshot := s.Router.SnapshotIPLoad(platformID)
	result := make([]IPLoadEntry, 0, len(snapshot))
	for ip, count := range snapshot {
		result = append(result, IPLoadEntry{
			EgressIP:   ip.String(),
			LeaseCount: count,
		})
	}
	return result, nil
}
