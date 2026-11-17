package subscription

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestClashFallbackPreservesPriorityAndProbeContract(t *testing.T) {
	data := []byte(`proxies:
  - {name: main, type: http, server: 127.0.0.1, port: 8001}
  - {name: backup, type: http, server: 127.0.0.1, port: 8002}
proxy-groups:
  - name: front
    type: fallback
    proxies: [main, missing, backup]
    url: http://landing.test:6227/probe
    expected-status: 407
    interval: 20
    timeout: 800
    probe-urls: [socks5://one.test:6240, socks5://two.test:6114, socks5://three.test:6137]
    probe-quorum: 2
`)
	nodes, err := ParseGeneralSubscription(data)
	if err != nil {
		t.Fatal(err)
	}
	var group map[string]any
	for _, n := range nodes {
		if n.Tag == "front" {
			if err := json.Unmarshal(n.RawOptions, &group); err != nil {
				t.Fatal(err)
			}
		}
	}
	if group["type"] != "fallback" {
		t.Fatalf("fallback became %v; priority failover must not become latency selection", group["type"])
	}
	for key, want := range map[string]any{
		"outbounds": []any{"main", "backup"},
		"url":       "http://landing.test:6227/probe", "expected_status": "407",
		"interval": "20s", "timeout": "800ms",
		"probe_urls":   []any{"socks5://one.test:6240", "socks5://two.test:6114", "socks5://three.test:6137"},
		"probe_quorum": float64(2),
	} {
		if !reflect.DeepEqual(group[key], want) {
			t.Errorf("%s = %#v, want %#v", key, group[key], want)
		}
	}
}
