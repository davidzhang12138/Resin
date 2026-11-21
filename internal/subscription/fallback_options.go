package subscription

import (
	"encoding/json"
	"fmt"
)

type fallbackProbeLanding struct {
	Type     string `json:"type,omitempty"`
	Server   string `json:"server"`
	Port     uint16 `json:"server_port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func addTieredFallback(group map[string]any, valid map[string]ParsedNode, options map[string]any) error {
	if _, tiered := group["primary-proxies"]; !tiered {
		return nil
	}

	seen := make(map[string]bool)
	var tags []string
	add := func(list string) {
		for _, tag := range getStringSlice(group, list) {
			if _, exists := valid[tag]; exists && !seen[tag] {
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
	}
	add("primary-proxies")
	options["primary_count"] = len(tags)
	add("backup-proxies")
	if len(tags) == 0 {
		return fmt.Errorf("fallback group has no usable tier members")
	}
	options["outbounds"] = tags

	if timeout, ok := getUint(group, "dial-timeout"); ok && timeout > 0 {
		options["dial_timeout"] = fmt.Sprintf("%dms", timeout)
	}
	if budget, ok := getUint(group, "dial-budget"); ok && budget > 0 {
		options["dial_budget"] = fmt.Sprintf("%dms", budget)
	}

	landings := getStringSlice(group, "probe-landings")
	probes := make([]fallbackProbeLanding, 0, len(landings))
	for _, tag := range landings {
		landing, found := valid[tag]
		var probe fallbackProbeLanding
		if !found || json.Unmarshal(landing.RawOptions, &probe) != nil ||
			probe.Type != "socks" || probe.Server == "" || probe.Port == 0 {
			return fmt.Errorf("fallback probe landing is not a valid SOCKS5 node")
		}
		probe.Type = ""
		probes = append(probes, probe)
	}
	if len(probes) > 0 {
		options["probe_landings"] = probes
	}
	return nil
}
