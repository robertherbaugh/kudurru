// Package keepalived renders keepalived.conf and supervises keepalived
// (`kudurru vip run`, docs/architecture.md §3.6, D19, D46/D53, D51). It
// also holds the two small integration points keepalived offers: the
// track_file kudurru writes VRRP weights to and the notify FIFO consumer
// (`kudurru vip fifo`).
//
// keepalived is GPL-2.0+ and runs as a separate program; kudurru only
// writes its configuration, the track files and signals.
package keepalived

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// VIP is one virtual address managed by keepalived.
type VIP struct {
	// Name is the short VIP name ([a-z0-9_-]); it names the vrrp_instance
	// (VI_<name>), the check script (chk_<name>), the track file
	// (tf_<name>, <run dir>/weight-<name>) and metric labels.
	Name string
	// Prefix is the address with its prefix length (10.0.0.53/24).
	Prefix netip.Prefix
	// Interface is the device the address is added to.
	Interface string
	// VRID is the virtual router id (1-255). It is a hostOnly value: never
	// allocated or changed by kudurru.
	VRID int
	// PreemptDelay, when positive, replaces nopreempt with preempt_delay
	// (seconds) for this VIP: the operator's alternative to `kudurru vip
	// rebalance` for the second VIP of an active/active pair.
	PreemptDelay int
}

var vipSpec = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]{0,30})=([^@#]+)@([A-Za-z0-9_.:-]+)#([0-9]{1,3})$`)

// ParseVIP parses the --vip flag: name=addr/len@iface#vrid
// (dns=10.0.0.53/24@eth0#53).
func ParseVIP(s string) (VIP, error) {
	m := vipSpec.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return VIP{}, fmt.Errorf("keepalived: --vip %q: want name=addr/len@iface#vrid (dns=10.0.0.53/24@eth0#53)", s)
	}
	p, err := netip.ParsePrefix(m[2])
	if err != nil {
		return VIP{}, fmt.Errorf("keepalived: --vip %q: %w", s, err)
	}
	vrid, _ := strconv.Atoi(m[4])
	v := VIP{Name: m[1], Prefix: p, Interface: m[3], VRID: vrid}
	return v, v.validate()
}

func (v VIP) validate() error {
	if !vipSpec.MatchString(v.Name + "=x@" + v.Interface + "#1") {
		return fmt.Errorf("keepalived: VIP name %q or interface %q is invalid", v.Name, v.Interface)
	}
	if v.VRID < 1 || v.VRID > 255 {
		return fmt.Errorf("keepalived: VIP %s: VRID %d out of range 1-255", v.Name, v.VRID)
	}
	if !v.Prefix.IsValid() || v.Prefix.Addr().IsUnspecified() || v.Prefix.Addr().IsLoopback() {
		return fmt.Errorf("keepalived: VIP %s: invalid address %v", v.Name, v.Prefix)
	}
	if v.PreemptDelay < 0 || v.PreemptDelay > 1000 {
		return fmt.Errorf("keepalived: VIP %s: preempt delay %d out of range 0-1000", v.Name, v.PreemptDelay)
	}
	return nil
}

// Probe is the vrrp_script command line: the VRRP health probe probes the
// VIP on :53 (udp and tcp) while it is held locally, loopback otherwise,
// and requires RequireBackends healthy backends in the local dnsdist.
type Probe struct {
	// Command is the probe argv prefix; default DefaultProbeCommand.
	Command []string
	// DNSPort is the port probed on the VIP; default 53.
	DNSPort int
	// QName is the probe name; default health.kudurru.internal.
	QName string
	// DNSDistAPI is the local dnsdist API base URL; default
	// http://127.0.0.1:8083.
	DNSDistAPI string
	// DNSDistAPIKeyFile is the API key published by `kudurru edge run`;
	// default /run/kudurru/dnsdist-api.key.
	DNSDistAPIKeyFile string
	// RequireBackends is the minimum number of up backends; default 1.
	RequireBackends int
}

// DefaultProbeCommand is the VRRP check. §3.6 spells it `kudurru health
// --vrrp`; the health command belongs to the CLI package, so the probe
// ships as `kudurru vip probe` (same flags) and `health --vrrp` becomes an
// alias once cmd/kudurru wires installVRRPHealth into the health command
// (docs/m3/edge.md). --probe-command switches the rendered form.
var DefaultProbeCommand = []string{"/usr/local/bin/kudurru", "vip", "probe"}

// Config is everything keepalived.conf is rendered from.
type Config struct {
	// RouterID is global_defs router_id: the node name.
	RouterID string
	// VIPs in priority order: VIP i gets reversed priorities for odd i so
	// two VIPs run active/active.
	VIPs []VIP
	// PriorityIndex is this node's index (0 = preferred owner of the first
	// VIP); NodeCount the number of nodes (len(Peers)+1 by default).
	PriorityIndex int
	NodeCount     int
	// UnicastSrc is this node's address; Peers the other nodes'.
	UnicastSrc string
	Peers      []string
	// HMACKeyFile is the auth_hmac key file; "{vrid}" is replaced by the
	// VRID (/run/secrets/vrrp_hmac_{vrid}). Ignored in Legacy mode.
	HMACKeyFile string
	// Legacy renders for keepalived 2.3.x: no auth_hmac (adverts are
	// unauthenticated; `kudurru doctor` warns).
	Legacy bool
	// RunDir holds the FIFO, the track files and the FIFO state; default
	// /run/kudurru.
	RunDir string
	// Binary is the kudurru path used for the FIFO script; default
	// /usr/local/bin/kudurru.
	Binary string
	// GARPRefresh is vrrp_garp_master_refresh in seconds; default 30.
	GARPRefresh int
	// ScriptUser runs scripts under enable_script_security; default
	// keepalived_script.
	ScriptUser string
	Probe      Probe
}

// DefaultRunDir is the volume shared by kudurru, dnsdist and keepalived.
const DefaultRunDir = "/run/kudurru"

func (c *Config) defaults() {
	if c.RunDir == "" {
		c.RunDir = DefaultRunDir
	}
	if c.Binary == "" {
		c.Binary = "/usr/local/bin/kudurru"
	}
	if c.GARPRefresh == 0 {
		c.GARPRefresh = 30
	}
	if c.ScriptUser == "" {
		c.ScriptUser = "keepalived_script"
	}
	if c.NodeCount == 0 {
		c.NodeCount = len(c.Peers) + 1
	}
	if c.HMACKeyFile == "" {
		c.HMACKeyFile = "/run/secrets/vrrp_hmac_{vrid}"
	}
	p := &c.Probe
	if len(p.Command) == 0 {
		p.Command = DefaultProbeCommand
	}
	if p.DNSPort == 0 {
		p.DNSPort = 53
	}
	if p.QName == "" {
		p.QName = "health.kudurru.internal."
	}
	if p.DNSDistAPI == "" {
		p.DNSDistAPI = "http://127.0.0.1:8083"
	}
	if p.DNSDistAPIKeyFile == "" {
		p.DNSDistAPIKeyFile = filepath.Join(c.RunDir, "dnsdist-api.key")
	}
	if p.RequireBackends == 0 {
		p.RequireBackends = 1
	}
}

// Priority returns this node's priority for the VIP at index i:
// 200 - 10*idx, where idx is PriorityIndex for even i and the reversed
// index for odd i. Never 255 (address owner) and never below 1.
func (c *Config) Priority(i int) int {
	idx := c.PriorityIndex
	if i%2 == 1 {
		idx = c.NodeCount - 1 - c.PriorityIndex
	}
	return max(200-10*idx, 1)
}

// FIFOPath is the notify FIFO.
func (c *Config) FIFOPath() string { return filepath.Join(c.RunDir, "vrrp.fifo") }

// TrackFile is the track_file of a VIP.
func (c *Config) TrackFile(vip string) string { return TrackFilePath(c.RunDir, vip) }

// Validate reports every problem with the configuration.
func (c *Config) Validate() error {
	c.defaults()
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !safeToken(c.RouterID) {
		add("router id %q must be [A-Za-z0-9._-]", c.RouterID)
	}
	if len(c.VIPs) == 0 {
		add("no VIPs")
	}
	names, vrids := map[string]bool{}, map[int]string{}
	for _, v := range c.VIPs {
		if err := v.validate(); err != nil {
			errs = append(errs, err)
		}
		if names[v.Name] {
			add("VIP %s declared twice", v.Name)
		}
		names[v.Name] = true
		if prev, ok := vrids[v.VRID]; ok {
			add("VIPs %s and %s share VRID %d", prev, v.Name, v.VRID)
		}
		vrids[v.VRID] = v.Name
	}
	src, err := netip.ParseAddr(c.UnicastSrc)
	if err != nil {
		add("unicast source %q: %v", c.UnicastSrc, err)
	}
	if len(c.Peers) == 0 {
		add("no unicast peers: keepalived needs the other nodes' addresses (--peers)")
	}
	for _, p := range c.Peers {
		a, err := netip.ParseAddr(p)
		switch {
		case err != nil:
			add("peer %q: %v", p, err)
		case src.IsValid() && a.Is4() != src.Is4():
			add("peer %s and unicast source %s are of different address families", p, src)
		case a == src:
			add("peer %s is this node's own address", p)
		}
	}
	for _, v := range c.VIPs {
		if src.IsValid() && v.Prefix.Addr().Is4() != src.Is4() {
			add("VIP %s and unicast source %s are of different address families", v.Name, src)
		}
	}
	if c.PriorityIndex < 0 || c.PriorityIndex >= c.NodeCount {
		add("priority index %d out of range for %d nodes", c.PriorityIndex, c.NodeCount)
	}
	if c.NodeCount > 20 {
		add("%d nodes: priorities 200-10*idx support at most 20", c.NodeCount)
	}
	for _, s := range []string{c.RunDir, c.Binary, c.HMACKeyFile, c.Probe.DNSDistAPI, c.Probe.DNSDistAPIKeyFile, c.Probe.QName, c.ScriptUser} {
		if !safePath(s) {
			add("%q contains characters keepalived.conf cannot carry unquoted", s)
		}
	}
	for _, s := range c.Probe.Command {
		if !safePath(s) {
			add("probe command element %q is unsafe", s)
		}
	}
	return errors.Join(errs...)
}

func safeToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func safePath(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/:{}+=@", r)) {
			return false
		}
	}
	return true
}

// ProbeCommand returns the vrrp_script command of a VIP.
func (c *Config) ProbeCommand(v VIP) string {
	p := c.Probe
	args := append([]string{}, p.Command...)
	args = append(args,
		"--dns", netip.AddrPortFrom(v.Prefix.Addr(), uint16(p.DNSPort)).String(), //nolint:gosec // port validated by flag parsing
		"--qname", p.QName,
		"--dnsdist-api", p.DNSDistAPI,
		"--dnsdist-api-key-file", p.DNSDistAPIKeyFile,
		"--require-backends", strconv.Itoa(p.RequireBackends))
	return strings.Join(args, " ")
}

// FIFOScript is the vrrp_notify_fifo_script command; keepalived appends
// the FIFO path.
func (c *Config) FIFOScript() string {
	s := c.Binary + " vip fifo --node " + c.RouterID
	if c.RunDir != DefaultRunDir {
		s += " --run-dir " + c.RunDir
	}
	return s
}

// Render returns keepalived.conf.
func Render(c Config) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("keepalived: invalid configuration: %w", err)
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("# Generated by `kudurru vip run` (docs/architecture.md §3.6). Do not edit: the")
	w("# supervisor validates every render with `keepalived -t` and reloads with SIGHUP.")
	if c.Legacy {
		w("# LEGACY MODE (keepalived 2.3.x): no auth_hmac, VRRP adverts are unauthenticated.")
	}
	w("")
	w("global_defs {")
	w("    router_id %s", c.RouterID)
	w("    enable_script_security")
	w("    script_user %s", c.ScriptUser)
	w("    vrrp_version 3")
	w("    vrrp_garp_master_refresh %d", c.GARPRefresh)
	w("    vrrp_notify_fifo %s", c.FIFOPath())
	w("    vrrp_notify_fifo_script \"%s\"", c.FIFOScript())
	w("}")
	for _, v := range c.VIPs {
		w("")
		w("vrrp_script chk_%s {", v.Name)
		w("    script \"%s\"", c.ProbeCommand(v))
		w("    interval 1")
		w("    timeout 1")
		w("    fall 3")
		w("    rise 2")
		w("    weight 0")
		w("    user %s", c.ScriptUser)
		w("}")
		w("")
		w("track_file tf_%s {", v.Name)
		w("    file \"%s\"", c.TrackFile(v.Name))
		w("    weight -1")
		w("    init_file 0")
		w("}")
	}
	for i, v := range c.VIPs {
		w("")
		w("vrrp_instance VI_%s {", v.Name)
		w("    state BACKUP")
		w("    interface %s", v.Interface)
		w("    virtual_router_id %d", v.VRID)
		w("    priority %d", c.Priority(i))
		w("    advert_int 1")
		if v.PreemptDelay > 0 {
			w("    preempt_delay %d", v.PreemptDelay)
		} else {
			w("    nopreempt")
		}
		w("    unicast_src_ip %s", c.UnicastSrc)
		w("    unicast_peer {")
		for _, p := range c.Peers {
			w("        %s", p)
		}
		w("    }")
		w("    unicast_fault_no_peer")
		w("    check_unicast_src")
		if !c.Legacy {
			w("    auth_hmac {")
			w("        key 1 file:%s", strings.ReplaceAll(c.HMACKeyFile, "{vrid}", strconv.Itoa(v.VRID)))
			w("        active_key 1")
			w("        anti_replay monotonic")
			w("    }")
		}
		w("    virtual_ipaddress {")
		w("        %s dev %s", v.Prefix, v.Interface)
		w("    }")
		w("    track_script {")
		w("        chk_%s", v.Name)
		w("    }")
		w("    track_file {")
		w("        tf_%s", v.Name)
		w("    }")
		w("}")
	}
	return []byte(b.String()), nil
}
