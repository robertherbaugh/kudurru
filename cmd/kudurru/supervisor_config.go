package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// These maps are the complete flag-to-setting contract for render/run. Output
// destinations remain invocation-only; aliases share the same flag storage.
//
//nolint:gosec // G101: maps flag names to setting paths, never credential values.
var edgeSettings = map[string]string{
	"backends":                   "edge.backends",
	"backends-from":              "edge.backendsFrom",
	"backend-port":               "edge.backendPort",
	"backend-port-name":          "edge.backendPortName",
	"internal-port":              "edge.internalPort",
	"pool-resolver":              "edge.poolResolver",
	"pool-authoritative":         "edge.poolAuthoritative",
	"tls-dir":                    "edge.tlsDir",
	"listen":                     "edge.listen",
	"protocols":                  "edge.protocols",
	"dot":                        "edge.dot",
	"doh":                        "edge.doh",
	"doq":                        "edge.doq",
	"doh3":                       "edge.doh3",
	"api":                        "edge.api",
	"console":                    "edge.console",
	"api-key-out":                "edge.apiKeyOut",
	"api-key-group":              "edge.apiKeyGroup",
	"proxy-protocol-secret-file": "edge.proxyProtocolSecretFile",
	"authoritative-cache":        "edge.authoritativeCache",
	"authoritative-cache-ttl":    "edge.authoritativeCacheTTL",
	"authoritative-zones":        "edge.authoritativeZones",
	"transfer-sources":           "edge.transferSources",
	"acl":                        "edge.acl",
	"cache-size":                 "edge.cacheSize",
	"dynblocks":                  "edge.dynblocks",
	"dynblocks-qps":              "edge.dynblocksQPS",
	"dynblocks-nxdomain-rate":    "edge.dynblocksNXDomainRate",
	"ebpf":                       "edge.ebpf",
	"renderer":                   "edge.renderer",
	"server-id":                  "edge.serverID",
	"threads":                    "edge.threads",
	"dnsdist-bin":                "edge.dnsdistBin",
	"dnsdist-user":               "edge.dnsdistUser",
	"config-dir":                 "edge.configDir",
	"health-address":             "edge.healthAddress",
	"debounce":                   "edge.debounce",
	"drain":                      "edge.drain",
	"zone-poll":                  "edge.zonePoll",
	"poll-zones":                 "edge.pollZones",
	"stop-timeout":               "edge.stopTimeout",
	"internal-token-file":        "edge.internalTokenFile",
	"internal-ca-file":           "edge.internalCAFile",
	"internal-server-name":       "edge.internalServerName",
	"log-level":                  "edge.logLevel",
	"log-format":                 "edge.logFormat",
	"readiness-address":          "edge.healthAddress",
}

var vipSettings = map[string]string{
	"vip":                  "vip.vips",
	"preempt-delay":        "vip.preemptDelay",
	"peers":                "vip.peers",
	"peers-file":           "vip.peersFile",
	"peers-from":           "vip.peersFrom",
	"priority-index":       "vip.priorityIndex",
	"node-count":           "vip.nodeCount",
	"unicast-src":          "vip.unicastSrc",
	"vrrp-interface":       "vip.vrrpInterface",
	"hmac-key-file":        "vip.hmacKeyFile",
	"legacy":               "vip.legacy",
	"track-dnsdist-api":    "vip.trackDNSDistAPI",
	"dnsdist-api-key-file": "vip.dnsdistAPIKeyFile",
	"require-backends":     "vip.requireBackends",
	"probe-command":        "vip.probeCommand",
	"run-dir":              "vip.runDir",
	"router-id":            "vip.routerID",
	"kudurru-bin":          "vip.kudurruBin",
	"keepalived-bin":       "vip.keepalivedBin",
	"config-path":          "vip.configPath",
	"health-address":       "vip.healthAddress",
	"stop-timeout":         "vip.stopTimeout",
	"log-level":            "vip.logLevel",
	"log-format":           "vip.logFormat",
	"node-name":            "vip.routerID",
}

// supervisorOverrides only reads explicitly changed flags. In particular,
// false, zero, empty and replacement collections retain their precedence.
func supervisorOverrides(cmd *cobra.Command, mapping map[string]string) (map[string]any, error) {
	out := map[string]any{}
	var firstErr error
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		path, ok := mapping[flag.Name]
		if !ok || firstErr != nil {
			return
		}
		var value any
		var err error
		switch path {
		case "edge.backends":
			text, e := cmd.Flags().GetString(flag.Name)
			err = e
			value = splitCSV(text)
		case "vip.vips":
			values, e := cmd.Flags().GetStringArray(flag.Name)
			err = e
			list := []map[string]any{}
			for _, spec := range values {
				if len(values) == 1 && spec == "" {
					break
				}
				v, e := keepalived.ParseVIP(spec)
				if e != nil {
					err = e
					break
				}
				list = append(list, map[string]any{"name": v.Name, "address": v.Prefix.String(), "interface": v.Interface, "vrid": v.VRID})
			}
			value = list
		case "vip.preemptDelay":
			values, e := cmd.Flags().GetStringArray(flag.Name)
			err = e
			delays := map[string]any{}
			for _, spec := range values {
				if len(values) == 1 && spec == "" {
					break
				}
				name, seconds, ok := strings.Cut(spec, "=")
				n, e := strconv.Atoi(seconds)
				if !ok || name == "" || e != nil {
					err = fmt.Errorf("--preempt-delay: want name=seconds")
					break
				}
				delays[name] = n
			}
			value = delays
		default:
			switch flag.Value.Type() {
			case "string":
				value, err = cmd.Flags().GetString(flag.Name)
			case "stringSlice":
				value, err = cmd.Flags().GetStringSlice(flag.Name)
			case "int":
				value, err = cmd.Flags().GetInt(flag.Name)
			case "bool":
				value, err = cmd.Flags().GetBool(flag.Name)
			case "duration":
				d, e := cmd.Flags().GetDuration(flag.Name)
				value, err = d.String(), e
			default:
				err = fmt.Errorf("unsupported supervisor flag --%s", flag.Name)
			}
		}
		if err != nil {
			firstErr = err
			return
		}
		out[path] = value
	})
	return out, firstErr
}

func splitCSV(text string) []string {
	if text == "" {
		return []string{}
	}
	parts := strings.Split(text, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func loadSupervisorConfig(cmd *cobra.Command, path string, mapping map[string]string) (*config.Config, error) {
	overrides, err := supervisorOverrides(cmd, mapping)
	if err != nil {
		return nil, err
	}
	explicit := cmd.Flags().Changed("config") || os.Getenv("KUDURRU_CONFIG") != ""
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) || explicit {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		path = ""
	}
	return config.LoadSupervisors(path, os.Environ(), overrides)
}

func (f *edgeFlags) load(cmd *cobra.Command, path string) error {
	cfg, err := loadSupervisorConfig(cmd, path, edgeSettings)
	if err != nil {
		return err
	}
	e := cfg.Edge
	f.backends = strings.Join(e.Backends, ",")
	f.backendsFrom = e.BackendsFrom
	f.backendPort = e.BackendPort
	f.backendPortName = e.BackendPortName
	f.internalPort = e.InternalPort
	f.poolResolver = e.PoolResolver
	f.poolAuthoritative = e.PoolAuthoritative
	f.tlsDir = e.TLSDir
	f.listen = e.Listen
	f.protocols = e.Protocols
	*f.frontendAddrs[edge.DoT] = e.DoT
	*f.frontendAddrs[edge.DoH] = e.DoH
	*f.frontendAddrs[edge.DoQ] = e.DoQ
	*f.frontendAddrs[edge.DoH3] = e.DoH3
	f.api = e.API
	f.console = e.Console
	f.apiKeyOut = e.APIKeyOut
	f.apiKeyGroup = e.APIKeyGroup
	f.proxySecretFile = e.ProxyProtocolSecretFile
	f.authCache = e.AuthoritativeCache
	f.authCacheTTL = e.AuthoritativeCacheTTL
	f.authZones = e.AuthoritativeZones
	f.transferSources = e.TransferSources
	f.acl = e.ACL
	f.resolverCacheSize = e.CacheSize
	f.dynblocks = e.Dynblocks
	f.dynQPS = e.DynblocksQPS
	f.dynNX = e.DynblocksNXDomainRate
	f.ebpf = e.EBPF
	f.renderer = e.Renderer
	f.serverID = e.ServerID
	f.threads = e.Threads
	f.dnsdistBin = e.DNSDistBin
	f.dnsdistUser = e.DNSDistUser
	f.configDir = e.ConfigDir
	f.healthAddr = e.HealthAddress
	f.debounce = e.Debounce.Std()
	f.drain = e.Drain.Std()
	f.zonePoll = e.ZonePoll.Std()
	f.pollZones = e.PollZones
	f.stopTimeout = e.StopTimeout.Std()
	f.internalTokenFile = e.InternalTokenFile
	f.internalCA = e.InternalCAFile
	f.internalSN = e.InternalServerName
	f.logLevel = e.LogLevel
	f.logFormat = e.LogFormat
	return nil
}

func (f *vipFlags) load(cmd *cobra.Command, path string) error {
	cfg, err := loadSupervisorConfig(cmd, path, vipSettings)
	if err != nil {
		return err
	}
	v := cfg.VIP
	f.peers = v.Peers
	f.peersFile = v.PeersFile
	f.peersFrom = v.PeersFrom
	f.priorityIndex = v.PriorityIndex
	f.nodeCount = v.NodeCount
	f.unicastSrc = v.UnicastSrc
	f.vrrpInterface = v.VRRPInterface
	f.hmacKeyFile = v.HMACKeyFile
	f.legacy = v.Legacy
	f.dnsdistAPI = v.TrackDNSDistAPI
	f.apiKeyFile = v.DNSDistAPIKeyFile
	f.requireBackends = v.RequireBackends
	f.probeCommand = v.ProbeCommand
	f.runDir = v.RunDir
	f.routerID = v.RouterID
	f.kudurruBin = v.KudurruBin
	f.keepalivedBin = v.KeepalivedBin
	f.configPath = v.ConfigPath
	f.healthAddr = v.HealthAddress
	f.stopTimeout = v.StopTimeout.Std()
	f.logLevel = v.LogLevel
	f.logFormat = v.LogFormat
	if f.routerID == "" {
		f.routerID = cfg.Node.Name
	}
	f.priorityConfigured = cfg.SupervisorConfigured("vip.priorityIndex")
	f.vips = nil
	for _, instance := range v.VIPs {
		f.vips = append(f.vips, fmt.Sprintf("%s=%s@%s#%d", instance.Name, instance.Address, instance.Interface, instance.VRID))
	}
	f.preempt = nil
	names := make([]string, 0, len(v.PreemptDelay))
	for name := range v.PreemptDelay {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f.preempt = append(f.preempt, fmt.Sprintf("%s=%d", name, v.PreemptDelay[name]))
	}
	return nil
}
