package service

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/Resinat/Resin/internal/freepool"
	"github.com/Resinat/Resin/internal/subscription"
)

// ------------------------------------------------------------------
// Free Pool Maintainer
// ------------------------------------------------------------------

// GetFreePoolStatus returns the current status, config, and verified nodes of the free pool maintainer.
func (s *ControlPlaneService) GetFreePoolStatus() (*freepool.StatusResponse, error) {
	if s.FreePool == nil {
		return nil, internal("free pool service is not initialized", nil)
	}
	st := s.FreePool.Status()
	return &st, nil
}

// UpdateFreePoolConfig validates and updates the free pool maintainer configuration.
func (s *ControlPlaneService) UpdateFreePoolConfig(patchJSON json.RawMessage) (*freepool.StatusResponse, error) {
	if s.FreePool == nil {
		return nil, internal("free pool service is not initialized", nil)
	}
	current := s.FreePool.Config()
	if err := json.Unmarshal(patchJSON, &current); err != nil {
		return nil, invalidArg("invalid JSON body: " + err.Error())
	}
	if _, err := s.FreePool.UpdateConfig(current); err != nil {
		return nil, invalidArg(err.Error())
	}
	st := s.FreePool.Status()
	return &st, nil
}

// TriggerFreePoolRun triggers an asynchronous fetch/screen/sync cycle.
func (s *ControlPlaneService) TriggerFreePoolRun() error {
	if s.FreePool == nil {
		return internal("free pool service is not initialized", nil)
	}
	if err := s.FreePool.TriggerRunAsync(); err != nil {
		if errors.Is(err, freepool.ErrAlreadyRunning) {
			return conflict(err.Error())
		}
		return internal("trigger free pool refresh", err)
	}
	return nil
}

// CancelFreePoolRun cancels an in-flight free pool refresh run if running.
func (s *ControlPlaneService) CancelFreePoolRun() (bool, error) {
	if s.FreePool == nil {
		return false, internal("free pool service is not initialized", nil)
	}
	return s.FreePool.CancelRun(), nil
}

// GetFreePoolSubscriptionJSON returns the latest screened sing-box JSON subscription.
func (s *ControlPlaneService) GetFreePoolSubscriptionJSON() (string, error) {
	if s.FreePool == nil {
		return "", internal("free pool service is not initialized", nil)
	}
	return s.FreePool.SubscriptionJSON(), nil
}

// SyncFreePool implements freepool.SubscriptionSyncer by creating or updating
// Resin's managed local subscription and optionally ensuring a matching Platform.
func (s *ControlPlaneService) SyncFreePool(cfg freepool.Config, content string) (freepool.SyncResult, error) {
	if s.SubMgr == nil || s.Engine == nil {
		return freepool.SyncResult{}, errors.New("subscription manager unavailable")
	}

	subName := strings.TrimSpace(cfg.TargetSubscriptionName)
	if subName == "" {
		subName = "FreePool"
	}

	var targetSub *subscription.Subscription
	if cfg.TargetSubscriptionID != "" {
		if found := s.SubMgr.Lookup(cfg.TargetSubscriptionID); found != nil && found.SourceType() == subscription.SourceTypeLocal {
			targetSub = found
		}
	}
	if targetSub == nil {
		s.SubMgr.Range(func(_ string, sub *subscription.Subscription) bool {
			if sub.SourceType() == subscription.SourceTypeLocal && sub.Name() == subName {
				targetSub = sub
				return false
			}
			return true
		})
	}

	var subID string
	if targetSub == nil {
		sourceType := subscription.SourceTypeLocal
		interval := "24h"
		enabled := true
		ephemeral := cfg.SyncEphemeral
		incremental := cfg.SyncIncrementalAlive
		chainProxy := cfg.SyncChainProxy
		chainRelay := false
		evictDelay := cfg.SyncEvictDelay
		created, err := s.CreateSubscription(CreateSubscriptionRequest{
			Name:                    &subName,
			SourceType:              &sourceType,
			Content:                 &content,
			UpdateInterval:          &interval,
			Enabled:                 &enabled,
			Ephemeral:               &ephemeral,
			IncrementalAliveNodes:   &incremental,
			ChainProxy:              &chainProxy,
			ChainRelay:              &chainRelay,
			EphemeralNodeEvictDelay: &evictDelay,
		})
		if err != nil {
			return freepool.SyncResult{}, err
		}
		subID = created.ID
		if sub := s.SubMgr.Lookup(subID); sub != nil && s.Scheduler != nil {
			s.Scheduler.UpdateSubscription(sub)
		}
	} else {
		patchPayload := map[string]any{
			"name":                       subName,
			"content":                    content,
			"enabled":                    true,
			"ephemeral":                  cfg.SyncEphemeral,
			"incremental_alive_nodes":    cfg.SyncIncrementalAlive,
			"chain_proxy":                cfg.SyncChainProxy,
			"chain_relay":                false,
			"ephemeral_node_evict_delay": cfg.SyncEvictDelay,
		}
		rawPatch, err := json.Marshal(patchPayload)
		if err != nil {
			return freepool.SyncResult{}, err
		}
		updated, err := s.UpdateSubscription(targetSub.ID, rawPatch)
		if err != nil {
			return freepool.SyncResult{}, err
		}
		subID = updated.ID
	}

	res := freepool.SyncResult{SubscriptionID: subID}

	if cfg.EnsurePlatform {
		platName := strings.TrimSpace(cfg.TargetPlatformName)
		if platName == "" {
			platName = subName
		}
		plats, err := s.ListPlatforms()
		if err == nil {
			for _, p := range plats {
				if p.Name == platName {
					res.PlatformID = p.ID
					break
				}
			}
			if res.PlatformID == "" {
				stickyTTL := "1h"
				createdPlat, err := s.CreatePlatform(CreatePlatformRequest{
					Name:         &platName,
					StickyTTL:    &stickyTTL,
					RegexFilters: []string{"^" + regexp.QuoteMeta(subName) + "/"},
				})
				if err == nil && createdPlat != nil {
					res.PlatformID = createdPlat.ID
				}
			}
		}
	}

	return res, nil
}
