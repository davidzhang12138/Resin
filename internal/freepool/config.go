package freepool

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	SourceKindAuto         = "auto"
	SourceKindSOCKS5List   = "socks5_list"
	SourceKindSubscription = "subscription"

	RelayModeAuto      = "auto"
	RelayModeRelayOnly = "relay_only"
	RelayModePlatform  = "platform"
	RelayModeDirect    = "direct"
)

// Source describes a free proxy or free node aggregation source.
type Source struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Kind    string `json:"kind"` // auto | socks5_list | subscription
	Enabled bool   `json:"enabled"`
}

// Config holds persisted configuration for the free node pool maintainer.
type Config struct {
	Enabled                 bool     `json:"enabled"`
	Interval                string   `json:"interval"`
	Sources                 []Source `json:"sources"`
	MaxCandidates           int      `json:"max_candidates"`
	MaxKeepNodes            int      `json:"max_keep_nodes"`
	Concurrency             int      `json:"concurrency"`
	ProbeTimeout            string   `json:"probe_timeout"`
	MaxLatencyMs            int      `json:"max_latency_ms"`
	HTTPSCheckURL           string   `json:"https_check_url"`
	EnforceStrictTLS        bool     `json:"enforce_strict_tls"`
	RequireSSHPort22        bool     `json:"require_ssh_port22"`
	SSHCheckTarget          string   `json:"ssh_check_target"`
	RelayMode               string   `json:"relay_mode"`
	RelayPlatform           string   `json:"relay_platform"`
	TargetSubscriptionName  string   `json:"target_subscription_name"`
	TargetSubscriptionID    string   `json:"target_subscription_id"`
	SyncChainProxy          bool     `json:"sync_chain_proxy"`
	SyncDetourTag           string   `json:"sync_detour_tag"`
	SyncEphemeral           bool     `json:"sync_ephemeral"`
	SyncIncrementalAlive    bool     `json:"sync_incremental_alive"`
	SyncEvictDelay          string   `json:"sync_evict_delay"`
	EnsurePlatform          bool     `json:"ensure_platform"`
	TargetPlatformName      string   `json:"target_platform_name"`
}

// DefaultConfig returns sensible defaults with both SOCKS5 lists and free
// proxy protocol aggregators, strict TLS verification, and port-22 SSH checks
// enabled by default.
func DefaultConfig() Config {
	return Config{
		Enabled:  false,
		Interval: "30m",
		Sources: []Source{
			{
				ID:      "monosans-socks5",
				Name:    "monosans SOCKS5",
				URL:     "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt",
				Kind:    SourceKindSOCKS5List,
				Enabled: true,
			},
			{
				ID:      "proxifly-socks5",
				Name:    "proxifly SOCKS5",
				URL:     "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt",
				Kind:    SourceKindSOCKS5List,
				Enabled: true,
			},
			{
				ID:      "iplocate-socks5",
				Name:    "iplocate SOCKS5",
				URL:     "https://raw.githubusercontent.com/iplocate/free-proxy-list/main/protocols/socks5.txt",
				Kind:    SourceKindSOCKS5List,
				Enabled: true,
			},
			{
				ID:      "thespeedx-socks5",
				Name:    "TheSpeedX SOCKS5",
				URL:     "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/socks5.txt",
				Kind:    SourceKindSOCKS5List,
				Enabled: true,
			},
			{
				ID:      "nomorewalls",
				Name:    "NoMoreWalls",
				URL:     "https://raw.githubusercontent.com/peasoft/NoMoreWalls/master/list_raw.txt",
				Kind:    SourceKindSubscription,
				Enabled: true,
			},
			{
				ID:      "barryfar-v2ray-sub1",
				Name:    "barry-far V2Ray Sub1",
				URL:     "https://raw.githubusercontent.com/barry-far/V2ray-Config/main/Sub1.txt",
				Kind:    SourceKindSubscription,
				Enabled: true,
			},
			{
				ID:      "barryfar-vless",
				Name:    "barry-far VLESS",
				URL:     "https://raw.githubusercontent.com/barry-far/V2ray-Config/main/Splitted-By-Protocol/vless.txt",
				Kind:    SourceKindSubscription,
				Enabled: true,
			},
			{
				ID:      "mahdibland-v2ray",
				Name:    "V2RayAggregator",
				URL:     "https://raw.githubusercontent.com/mahdibland/V2RayAggregator/master/sub/sub_merge.txt",
				Kind:    SourceKindSubscription,
				Enabled: true,
			},
		},
		MaxCandidates:          2000,
		MaxKeepNodes:           100,
		Concurrency:            80,
		ProbeTimeout:           "5s",
		MaxLatencyMs:           4000,
		HTTPSCheckURL:          "https://cp.cloudflare.com/generate_204",
		EnforceStrictTLS:       true,
		RequireSSHPort22:       true,
		SSHCheckTarget:         "github.com:22",
		RelayMode:              RelayModeAuto,
		RelayPlatform:          "",
		TargetSubscriptionName: "FreePool",
		TargetSubscriptionID:   "",
		SyncChainProxy:         true,
		SyncDetourTag:          "",
		SyncEphemeral:          true,
		SyncIncrementalAlive:   true,
		SyncEvictDelay:         "2h",
		EnsurePlatform:         true,
		TargetPlatformName:     "FreePool",
	}
}

// Validate normalizes and validates a Config.
func (c *Config) Validate() error {
	c.Interval = strings.TrimSpace(c.Interval)
	if c.Interval == "" {
		c.Interval = "30m"
	}
	interval, err := time.ParseDuration(c.Interval)
	if err != nil || interval < 1*time.Minute {
		return errors.New("interval: must be a valid duration >= 1m")
	}

	c.ProbeTimeout = strings.TrimSpace(c.ProbeTimeout)
	if c.ProbeTimeout == "" {
		c.ProbeTimeout = "8s"
	}
	probeTimeout, err := time.ParseDuration(c.ProbeTimeout)
	if err != nil || probeTimeout < 1*time.Second || probeTimeout > 60*time.Second {
		return errors.New("probe_timeout: must be between 1s and 60s")
	}

	c.SyncEvictDelay = strings.TrimSpace(c.SyncEvictDelay)
	if c.SyncEvictDelay == "" {
		c.SyncEvictDelay = "2h"
	}
	evictDelay, err := time.ParseDuration(c.SyncEvictDelay)
	if err != nil || evictDelay < 0 {
		return errors.New("sync_evict_delay: must be a non-negative duration")
	}

	if c.MaxCandidates < 1 || c.MaxCandidates > 10000 {
		return errors.New("max_candidates: must be between 1 and 10000")
	}
	if c.MaxKeepNodes < 1 || c.MaxKeepNodes > 2000 {
		return errors.New("max_keep_nodes: must be between 1 and 2000")
	}
	if c.Concurrency < 1 || c.Concurrency > 256 {
		return errors.New("concurrency: must be between 1 and 256")
	}
	if c.MaxLatencyMs < 100 || c.MaxLatencyMs > 60000 {
		return errors.New("max_latency_ms: must be between 100 and 60000")
	}

	c.HTTPSCheckURL = strings.TrimSpace(c.HTTPSCheckURL)
	if c.HTTPSCheckURL == "" {
		c.HTTPSCheckURL = "https://cp.cloudflare.com/generate_204"
	}
	u, err := url.Parse(c.HTTPSCheckURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("https_check_url: must be a valid https:// URL")
	}

	c.SSHCheckTarget = strings.TrimSpace(c.SSHCheckTarget)
	if c.SSHCheckTarget == "" {
		c.SSHCheckTarget = "github.com:22"
	}
	if c.RequireSSHPort22 {
		host, port, err := net.SplitHostPort(c.SSHCheckTarget)
		if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
			return errors.New("ssh_check_target: must be host:port (e.g. github.com:22)")
		}
	}

	c.RelayMode = strings.ToLower(strings.TrimSpace(c.RelayMode))
	if c.RelayMode == "" {
		c.RelayMode = RelayModeAuto
	}
	switch c.RelayMode {
	case RelayModeAuto, RelayModeRelayOnly, RelayModePlatform, RelayModeDirect:
	default:
		return errors.New("relay_mode: must be auto, relay_only, platform, or direct")
	}

	c.RelayPlatform = strings.TrimSpace(c.RelayPlatform)
	c.TargetSubscriptionName = strings.TrimSpace(c.TargetSubscriptionName)
	if c.TargetSubscriptionName == "" {
		c.TargetSubscriptionName = "FreePool"
	}
	c.TargetSubscriptionID = strings.TrimSpace(c.TargetSubscriptionID)
	c.SyncDetourTag = strings.TrimSpace(c.SyncDetourTag)
	c.TargetPlatformName = strings.TrimSpace(c.TargetPlatformName)
	if c.TargetPlatformName == "" {
		c.TargetPlatformName = c.TargetSubscriptionName
	}

	seenID := make(map[string]bool, len(c.Sources))
	for i := range c.Sources {
		src := &c.Sources[i]
		src.ID = strings.TrimSpace(src.ID)
		src.Name = strings.TrimSpace(src.Name)
		src.URL = strings.TrimSpace(src.URL)
		src.Kind = strings.ToLower(strings.TrimSpace(src.Kind))
		if src.Kind == "" {
			src.Kind = SourceKindAuto
		}
		switch src.Kind {
		case SourceKindAuto, SourceKindSOCKS5List, SourceKindSubscription:
		default:
			return fmt.Errorf("sources[%d].kind: must be auto, socks5_list, or subscription", i)
		}
		if src.Name == "" {
			src.Name = fmt.Sprintf("Source %d", i+1)
		}
		if src.ID == "" {
			src.ID = fmt.Sprintf("src-%d", i+1)
		}
		if seenID[src.ID] {
			src.ID = fmt.Sprintf("%s-%d", src.ID, i+1)
		}
		seenID[src.ID] = true

		su, err := url.Parse(src.URL)
		if err != nil || (su.Scheme != "http" && su.Scheme != "https") || su.Hostname() == "" {
			return fmt.Errorf("sources[%d].url: must be a valid http(s) URL", i)
		}
	}

	return nil
}

func (c Config) intervalDuration() time.Duration {
	d, err := time.ParseDuration(c.Interval)
	if err != nil || d < time.Minute {
		return 30 * time.Minute
	}
	return d
}

func (c Config) probeTimeoutDuration() time.Duration {
	d, err := time.ParseDuration(c.ProbeTimeout)
	if err != nil || d < time.Second {
		return 8 * time.Second
	}
	return d
}

func (c Config) clone() Config {
	out := c
	if c.Sources != nil {
		out.Sources = make([]Source, len(c.Sources))
		copy(out.Sources, c.Sources)
	}
	return out
}

func loadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, err
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func writeJSONAtomically(path string, v any) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
