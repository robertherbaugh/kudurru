package config

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
)

// DnsmasqIssue identifies input that cannot be represented as forwarding rules.
// The report never includes the original line or an upstream credential.
type DnsmasqIssue struct {
	Line   int
	Reason string
}

// DnsmasqImport is a bounded, deterministic translation into canonical groups.
// Callers must handle Issues before installing the result.
type DnsmasqImport struct {
	Groups      map[string]UpstreamGroup
	Conditional map[string]string
	Default     []string
	Issues      []DnsmasqIssue
}

// DnsmasqMaxBytes bounds each imported source, including comments.
const (
	DnsmasqMaxBytes = 4 << 20
	dnsmasqMaxRules = 65536
)

// ParseDnsmasq reads dnsmasq server= rules and dnsproxy bracket/plain rules.
// Repeated suffixes combine their ordered server lists; conflicting default
// exceptions are rejected. Unsupported directives are reported, never applied.
func ParseDnsmasq(r io.Reader) (*DnsmasqImport, error) {
	result := &DnsmasqImport{Groups: map[string]UpstreamGroup{}, Conditional: map[string]string{}}
	limited := &io.LimitedReader{R: r, N: DnsmasqMaxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 65536)
	routes := map[string][]string{}
	var order []string
	lineNo, rules, assignments, expandedBytes := 0, 0, 0, 0
	issue := func(reason string) error {
		if len(result.Issues) >= 64 {
			return fmt.Errorf("dnsmasq: more than 64 unsupported directives")
		}
		result.Issues = append(result.Issues, DnsmasqIssue{Line: lineNo, Reason: reason})
		return nil
	}
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rules++
		if rules > dnsmasqMaxRules {
			return nil, fmt.Errorf("dnsmasq: more than %d rules", dnsmasqMaxRules)
		}
		var domains, servers []string
		switch {
		case strings.HasPrefix(line, "server="):
			value := strings.TrimSpace(strings.TrimPrefix(line, "server="))
			if strings.HasPrefix(value, "/") {
				last := strings.LastIndexByte(value, '/')
				if last == 0 {
					return nil, fmt.Errorf("dnsmasq line %d: unterminated domain list", lineNo)
				}
				for _, domain := range strings.Split(value[1:last], "/") {
					if domain == "" || domain != "." && !IsDomain(domain) || strings.ContainsAny(domain, "*#") {
						return nil, fmt.Errorf("dnsmasq line %d: unsupported domain selector", lineNo)
					}
					domains = append(domains, strings.ToLower(strings.TrimSuffix(domain, "."))+".")
				}
				value = strings.TrimSpace(value[last+1:])
			}
			if value == "" {
				if err := issue("local-answer rules require an explicit zone or policy"); err != nil {
					return nil, err
				}
				continue
			}
			if host, port, ok := strings.Cut(value, "#"); ok && host != "" && !strings.Contains(value, "://") {
				value = net.JoinHostPort(host, port)
			}
			servers = []string{value}
		case strings.HasPrefix(line, "[/"):
			end := strings.Index(line, "/]")
			if end < 2 {
				return nil, fmt.Errorf("dnsmasq line %d: unterminated domain list", lineNo)
			}
			for _, domain := range strings.Split(line[2:end], "/") {
				if domain == "" || domain != "." && !IsDomain(domain) || strings.ContainsAny(domain, "*#") {
					return nil, fmt.Errorf("dnsmasq line %d: unsupported domain selector", lineNo)
				}
				domains = append(domains, strings.ToLower(strings.TrimSuffix(domain, "."))+".")
			}
			servers = strings.Fields(line[end+2:])
			if len(servers) == 0 {
				if err := issue("local-answer rules require an explicit zone or policy"); err != nil {
					return nil, err
				}
				continue
			}
		default:
			if strings.Contains(line, "=") && !strings.Contains(line, "://") {
				if err := issue("unsupported dnsmasq directive"); err != nil {
					return nil, err
				}
				continue
			}
			servers = strings.Fields(line)
		}
		for _, server := range servers {
			if server == "#" && len(domains) > 0 && len(servers) == 1 {
				continue
			}
			if !isUpstream(server) || strings.ContainsAny(server, "@\x00\r\n") || strings.Contains(server, "=") {
				return nil, fmt.Errorf("dnsmasq line %d: unsupported server address", lineNo)
			}
		}
		if len(domains) == 0 {
			for _, server := range servers {
				if !slices.Contains(result.Default, server) {
					if len(result.Default) >= 1024 {
						return nil, fmt.Errorf("dnsmasq: more than 1024 default servers")
					}
					expandedBytes += len(server)
					if expandedBytes > DnsmasqMaxBytes {
						return nil, fmt.Errorf("dnsmasq: expanded server data exceeds 4 MiB")
					}
					result.Default = append(result.Default, server)
				}
			}
			continue
		}
		for _, domain := range domains {
			previous, known := routes[domain]
			if !known {
				if len(order) >= dnsmasqMaxRules {
					return nil, fmt.Errorf("dnsmasq: too many domain selectors")
				}
				order = append(order, domain)
			}
			if known && (slices.Contains(previous, "#") != slices.Contains(servers, "#")) {
				return nil, fmt.Errorf("dnsmasq line %d: conflicting default exception", lineNo)
			}
			for _, server := range servers {
				if !slices.Contains(previous, server) {
					if len(previous) >= 256 {
						return nil, fmt.Errorf("dnsmasq line %d: more than 256 servers per suffix", lineNo)
					}
					assignments++
					expandedBytes += len(server)
					if assignments > dnsmasqMaxRules || expandedBytes > DnsmasqMaxBytes {
						return nil, fmt.Errorf("dnsmasq: expanded routing exceeds 65536 server assignments or 4 MiB")
					}
					previous = append(previous, server)
				}
			}
			routes[domain] = previous
		}
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("dnsmasq line %d: unreadable input or line exceeds 64 KiB", lineNo+1)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("dnsmasq: input exceeds 4 MiB")
	}
	byServers := map[string]string{}
	for _, domain := range order {
		servers := routes[domain]
		group := "default"
		if len(servers) != 1 || servers[0] != "#" {
			key := strings.Join(servers, " ")
			group = byServers[key]
			if group == "" {
				if len(result.Groups) >= 4096 {
					return nil, fmt.Errorf("dnsmasq: more than 4096 server groups")
				}
				group = fmt.Sprintf("dnsmasq-%d", len(result.Groups)+1)
				byServers[key] = group
				result.Groups[group] = UpstreamGroup{Servers: servers, Strategy: "strict"}
			}
		}
		result.Conditional[domain] = group
	}
	return result, nil
}
