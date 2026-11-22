package freepool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
)

// Candidate represents a sanitized, deduplicated outbound ready for probing.
type Candidate struct {
	Hash       node.Hash
	Protocol   string
	Server     string
	ServerPort int
	Tag        string
	SourceID   string
	SourceName string
	RawOptions json.RawMessage
}

// ParseAndSanitizeSource parses raw subscription or proxy-list bytes from a
// source, applies anti-MITM and SSRF safety rules, and returns clean candidates.
func ParseAndSanitizeSource(src Source, body []byte, enforceStrictTLS bool) ([]Candidate, int) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, 0
	}

	normalized := body
	if src.Kind == SourceKindSOCKS5List || (src.Kind == SourceKindAuto && looksLikeBareHostPortList(body) && strings.Contains(strings.ToLower(src.URL+src.Name), "socks")) {
		normalized = normalizeSOCKS5List(body)
	}

	parsed, err := subscription.ParseGeneralSubscription(normalized)
	if err != nil || len(parsed) == 0 {
		return nil, 0
	}

	rawCount := len(parsed)
	out := make([]Candidate, 0, len(parsed))
	for _, p := range parsed {
		cand, ok := sanitizeParsedNode(p, src, enforceStrictTLS)
		if !ok {
			continue
		}
		out = append(out, cand)
	}
	return out, rawCount
}

func looksLikeBareHostPortList(body []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	checked := 0
	bare := 0
	for scanner.Scan() && checked < 20 {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		checked++
		if strings.Contains(line, "://") || strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "proxies:") {
			return false
		}
		if _, _, ok := parseBareHostPortLine(line); ok {
			bare++
		}
	}
	return checked > 0 && bare == checked
}

// normalizeSOCKS5List converts bare "host:port" or "host:port:user:pass" lines
// into explicit "socks5://..." URIs so ParseGeneralSubscription parses them as
// SOCKS5 instead of HTTP.
func normalizeSOCKS5List(body []byte) []byte {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "socks5://") || strings.HasPrefix(lower, "socks5h://") {
			buf.WriteString(line)
			buf.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(lower, "socks4://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			continue
		}
		if host, port, userPass, ok := parseBareProxyLine(line); ok {
			if userPass != "" {
				fmt.Fprintf(&buf, "socks5://%s@%s\n", userPass, net.JoinHostPort(host, port))
			} else {
				fmt.Fprintf(&buf, "socks5://%s\n", net.JoinHostPort(host, port))
			}
		}
	}
	return buf.Bytes()
}

func parseBareHostPortLine(line string) (string, string, bool) {
	host, port, _, ok := parseBareProxyLine(line)
	return host, port, ok
}

func parseBareProxyLine(line string) (host, port, userPass string, ok bool) {
	// Strip inline comments or trailing whitespace.
	if idx := strings.IndexAny(line, " \t#"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	if line == "" {
		return "", "", "", false
	}

	// Bracketed IPv6: [addr]:port or [addr]:port:user:pass
	if strings.HasPrefix(line, "[") {
		end := strings.Index(line, "]")
		if end <= 1 || end+2 > len(line) || line[end+1] != ':' {
			return "", "", "", false
		}
		host = line[1:end]
		rest := strings.Split(line[end+2:], ":")
		if len(rest) == 1 {
			return host, rest[0], "", isValidPortString(rest[0])
		}
		if len(rest) == 3 {
			return host, rest[0], rest[1] + ":" + rest[2], isValidPortString(rest[0])
		}
		return "", "", "", false
	}

	parts := strings.Split(line, ":")
	switch len(parts) {
	case 2:
		if parts[0] != "" && isValidPortString(parts[1]) {
			return parts[0], parts[1], "", true
		}
	case 4:
		if parts[0] != "" && isValidPortString(parts[1]) && parts[2] != "" {
			return parts[0], parts[1], parts[2] + ":" + parts[3], true
		}
	}
	return "", "", "", false
}

func isValidPortString(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n >= 1 && n <= 65535
}

func sanitizeParsedNode(p subscription.ParsedNode, src Source, enforceStrictTLS bool) (Candidate, bool) {
	var obj map[string]any
	if err := json.Unmarshal(p.RawOptions, &obj); err != nil {
		return Candidate{}, false
	}

	proto, _ := obj["type"].(string)
	proto = strings.ToLower(strings.TrimSpace(proto))
	switch proto {
	case "socks", "shadowsocks", "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "anytls", "http":
	default:
		// Skip selector/urltest/fallback/direct/block/wireguard/shadowtls/ssh/tor.
		return Candidate{}, false
	}

	// For socks, require SOCKS5 (SOCKS4 lacks clean domain/TCP support).
	if proto == "socks" {
		if v, ok := obj["version"].(string); ok && v != "" && v != "5" {
			return Candidate{}, false
		}
	}

	server, _ := obj["server"].(string)
	server = strings.TrimSpace(server)
	if server == "" || isUnsafeServerHost(server) {
		return Candidate{}, false
	}

	portFloat, ok := obj["server_port"].(float64)
	port := int(portFloat)
	if !ok || port < 1 || port > 65535 {
		return Candidate{}, false
	}

	// Remove any untrusted detour from public lists; chaining is controlled by Resin.
	delete(obj, "detour")

	// Anti-MITM hardening: never allow untrusted free nodes to silently skip TLS verification.
	if enforceStrictTLS {
		if tlsObj, ok := obj["tls"].(map[string]any); ok {
			if enabled, _ := tlsObj["enabled"].(bool); enabled {
				tlsObj["insecure"] = false
			}
		}
	}

	// Build a clean deterministic tag without colliding across sources.
	cleanTag := buildCleanTag(proto, server, port, p.Tag)
	obj["tag"] = cleanTag

	rawBytes, err := json.Marshal(obj)
	if err != nil {
		return Candidate{}, false
	}

	return Candidate{
		Hash:       node.HashFromRawOptions(rawBytes),
		Protocol:   proto,
		Server:     server,
		ServerPort: port,
		Tag:        cleanTag,
		SourceID:   src.ID,
		SourceName: src.Name,
		RawOptions: rawBytes,
	}, true
}

func buildCleanTag(proto, server string, port int, origTag string) string {
	origTag = strings.TrimSpace(origTag)
	if origTag == "" {
		return fmt.Sprintf("[Free][%s] %s:%d", proto, server, port)
	}
	// Trim excessively long noisy tags from public scrapers.
	runes := []rune(origTag)
	if len(runes) > 48 {
		origTag = strings.TrimSpace(string(runes[:48]))
	}
	if strings.HasPrefix(origTag, "[Free]") {
		return origTag
	}
	return fmt.Sprintf("[Free][%s] %s", proto, origTag)
}

func isUnsafeServerHost(host string) bool {
	lower := strings.ToLower(strings.Trim(host, "[]"))
	if lower == "" || lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	addr, err := netip.ParseAddr(lower)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified()
}

// SampleAndDeduplicate deduplicates candidates across sources and selects up to
// maxCandidates evenly (round-robin across sources) so no single huge list
// starves smaller higher-quality sources.
func SampleAndDeduplicate(bySource [][]Candidate, maxCandidates int) []Candidate {
	if maxCandidates <= 0 {
		return nil
	}
	seen := make(map[node.Hash]bool)
	result := make([]Candidate, 0, maxCandidates)
	indices := make([]int, len(bySource))

	for len(result) < maxCandidates {
		progressed := false
		for sIdx, list := range bySource {
			for indices[sIdx] < len(list) {
				cand := list[indices[sIdx]]
				indices[sIdx]++
				if seen[cand.Hash] {
					continue
				}
				seen[cand.Hash] = true
				result = append(result, cand)
				progressed = true
				break
			}
			if len(result) >= maxCandidates {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return result
}
