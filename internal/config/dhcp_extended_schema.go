package config

var dhcpProbeModes = []Option{
	{Value: "auto", Label: "Automatic", Help: "Use ARP for direct scopes and ICMP when relays are configured."},
	{Value: "arp", Label: "ARP", Help: "Probe the directly connected Ethernet link before offering a new address."},
	{Value: "icmp", Label: "ICMP", Help: "Send a bounded ICMP echo probe, including across routed relay networks."},
	{Value: "disabled", Label: "Disabled", Help: "Skip pre-offer probing; client DECLINE quarantine remains enabled."},
}

var dhcpRouteShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "destination", Label: "Destination", Help: "Canonical IPv4 destination prefix; use 0.0.0.0/0 for a default route.", Required: true, Shape: Shape{Type: TypeString, Format: FormatCIDR}},
	{Name: "router", Label: "Next hop", Help: "IPv4 gateway for this destination, reachable by clients.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
}}
var dhcpBootShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "architecture", Label: "Client architecture", Help: "Numeric client architecture from DHCP option 93; each architecture may appear once.", Required: true, Shape: Shape{Type: TypeInt, Min: new(0), Max: new(65535)}},
	{Name: "bootServer", Label: "Boot server address", Help: "Optional IPv4 address for the BOOTP next-server field.", Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "bootServerName", Label: "Boot server name", Help: "Optional server name advertised as option 66.", Shape: Shape{Type: TypeString}},
	{Name: "bootFile", Label: "Boot file or URL", Help: "Boot filename or HTTP URL; at most 127 printable non-space ASCII bytes.", Shape: Shape{Type: TypeString}},
}}
var dhcpScopeServerShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "nodeID", Label: "Failover node", Help: "Stable voter ID from this node's DHCP failover setup.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "address", Label: "Server address", Help: "That voter's IPv4 server address for this scope; excluded from all allocations.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
}}
var dhcpFailoverPeerShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "id", Label: "Node ID", Help: "Stable lowercase ID identifying one of the three voters and its TLS certificate.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "address", Label: "Replication address", Help: "Advertised TCP host:port reachable by the other two voters, for example 192.0.2.2:9167.", Required: true, Shape: Shape{Type: TypeString, Format: FormatHostPort}},
	{Name: "apiURL", Label: "Dashboard URL", Help: "Optional HTTP(S) dashboard URL for this node.", Shape: Shape{Type: TypeString, Format: FormatURL}},
}}
var dhcpExtendedFields = []Field{
	{Name: "conflictDetection", Label: "Address conflict detection", Help: "Probe before offering a new address. Automatic chooses ARP on direct scopes and ICMP when relays are configured.", Shape: Shape{Type: TypeString, Enum: dhcpProbeModes}},
	{Name: "probeTimeout", Label: "Probe timeout", Help: "Time to wait for a conflict response, 50ms through 500ms in whole milliseconds; empty uses 200ms.", Shape: Shape{Type: TypeDuration}},
	{Name: "ntpServers", Label: "NTP servers", Help: "IPv4 time servers advertised with option 42, up to 63 addresses.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatIPv4}}},
	{Name: "domainSearch", Label: "Search domains", Help: "DNS search domains advertised with option 119; encoded list must fit 255 bytes.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatDomain}}},
	{Name: "classlessRoutes", Label: "Classless routes", Help: "IPv4 routes advertised with option 121 and, when requested, 249; include an explicit default route if needed.", Shape: Shape{Type: TypeList, Items: dhcpRouteShape}},
	{Name: "bootServer", Label: "Boot server address", Help: "Optional IPv4 next-server address used for network boot.", Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "bootServerName", Label: "Boot server name", Help: "Optional option 66 name, at most 253 printable non-space ASCII bytes.", Shape: Shape{Type: TypeString}},
	{Name: "bootFile", Label: "Boot file or URL", Help: "Default network boot file or HTTP URL, at most 127 printable non-space ASCII bytes.", Shape: Shape{Type: TypeString}},
	{Name: "bootProfiles", Label: "Architecture boot profiles", Help: "Replace the default boot settings for selected client architectures from option 93.", Shape: Shape{Type: TypeList, Items: dhcpBootShape}},
	{Name: "failoverServers", Label: "Failover server addresses", Help: "All three voters' server addresses for this scope; immutable allocation exclusions.", Shape: Shape{Type: TypeList, Items: dhcpScopeServerShape}},
}
