package config

var dhcpManagementOptions = []Option{
	{Value: "file", Label: "Host file", Help: "Host configuration owns options and reservations; the DHCP API is read-only."},
	{Value: "api", Label: "DHCP API", Help: "Config file settings seed the first start; live options and reservations persist in the scope database. Mutable environment overrides are rejected while enabled; remove them or use file management."},
}

var dhcpRelayShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "sourcePort", Label: "Relay source port", Help: "Authorized UDP source port; zero means 67. Alternate ports require the relay port suboption.", Shape: Shape{Type: TypeInt, Min: new(0), Max: new(65535)}},
	{Name: "linkSelection", Label: "Link selection address", Help: "Exact authorized link selection address inside this client subnet; empty refuses the extension.", Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "serverIdentifier", Label: "Server identifier override", Help: "Exact authorized relay server identifier; empty refuses the extension.", Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "sourceIP", Label: "Relay source address", Help: "Exact IPv4 source address of the trusted relay.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "gatewayIP", Label: "Relay gateway address", Help: "Exact giaddr used by this relay for the client subnet; never allocated.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
}}

var dhcpScopeShape = &Shape{Type: TypeObject, Fields: append([]Field{
	{Name: "name", Label: "Scope name", Help: "Stable lowercase filename-safe scope identifier, at most 63 characters; default uses dhcp.db.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "management", Label: "Management owner", Help: "file keeps host ownership; api seeds once and permits durable live pool and reservation edits.", Shape: Shape{Type: TypeString, Enum: dhcpManagementOptions, Default: "file"}},
	{Name: "interface", Label: "LAN interface", Help: "Host interface receiving direct clients or trusted relays; restart to change.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "serverIP", Label: "Server IPv4 address", Help: "Static IPv4 address assigned to the host interface; restart to change.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "subnet", Label: "Client subnet", Help: "Canonical client IPv4 subnet; must not overlap another scope. Restart to change.", Required: true, Shape: Shape{Type: TypeString, Format: FormatCIDR}},
	{Name: "relayOnly", Label: "Relay-only scope", Help: "Only trusted relays serve this subnet; serverIP may be outside it.", Shape: Shape{Type: TypeBool, Default: false}},
	{Name: "trustedRelays", Label: "Trusted relays", Help: "Exact source and gateway pairs accepted on this scope's interface.", Shape: Shape{Type: TypeList, Items: dhcpRelayShape}},
	{Name: "rangeStart", Label: "Dynamic range start", Help: "First inclusive pool address; API-managed scopes persist edits in their lease database.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "rangeEnd", Label: "Dynamic range end", Help: "Last inclusive pool address; excludes server, router, relay gateways and reservations.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "router", Label: "Router", Help: "Usable IPv4 gateway in the client subnet, advertised to clients.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIPv4}},
	{Name: "dnsServers", Label: "DNS servers", Help: "Reachable IPv4 DNS servers advertised to clients.", Required: true, Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatIPv4}}},
	{Name: "domainName", Label: "Client domain", Help: "Optional DHCP domain, also used for local DNS records when registerDNS is enabled.", Shape: Shape{Type: TypeString, Format: FormatDomain}},
	{Name: "leaseDuration", Label: "Lease duration", Help: "Whole seconds from one minute through 30 days; existing lease ownership is never shortened.", Shape: Shape{Type: TypeDuration, Default: "12h"}},
	{Name: "reservations", Label: "Reservations", Help: "MAC-to-IP reservations; API-managed scopes persist edits in their lease database.", Shape: Shape{Type: TypeList, Items: dhcpReservationShape}},
	{Name: "registerDNS", Label: "Register local DNS records", Help: "Publish bound-lease A/PTR records in a dedicated domain, outside specific configured zones; the catch-all root forward zone permits them.", Shape: Shape{Type: TypeBool, Default: false}},
	{Name: "dnsTTL", Label: "Local DNS TTL", Help: "Whole seconds from 1s to 1h, clamped to the remaining lease duration.", Shape: Shape{Type: TypeDuration, Default: "30s"}},
}, dhcpExtendedFields...)}
