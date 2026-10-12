package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/spf13/cobra"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/edge"
	"github.com/robertherbaugh/kudurru/internal/edge/dnsdist"
)

// edgeFlags are the `kudurru edge render|run` flags (docs/architecture.md
// §3.6, §6.5).
type edgeFlags struct {
	backends, backendsFrom          string
	backendPort, internalPort       int
	backendPortName                 string
	poolResolver, poolAuthoritative string
	tlsDir                          string
	listen, protocols               []string
	frontendAddrs                   map[edge.Protocol]*string
	api, console                    string
	apiKeyOut, apiKeyGroup          string
	proxySecretFile                 string
	authCache                       bool
	authCacheTTL                    int
	authZones, transferSources, acl []string
	resolverCacheSize               int
	dynblocks                       bool
	dynQPS, dynNX                   int
	ebpf                            bool
	renderer, dnsdistBin, configDir string
	dnsdistUser                     string
	serverID                        string
	threads                         int
	// run only
	healthAddr                                string
	debounce, drain, zonePoll, stopTimeout    time.Duration
	pollZones                                 bool
	internalTokenFile, internalCA, internalSN string
	logLevel, logFormat                       string
	out                                       string
}

func (f *edgeFlags) register(cmd *cobra.Command, run bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.backends, "backends", "", "static backends: comma-separated [name=]host[:port] (port default --backend-port); EVERY edge lists ALL kudurru nodes")
	fl.StringVar(&f.backendsFrom, "backends-from", "static", "membership source: static (from --backends) or endpointslice:<namespace>/<service>")
	fl.IntVar(&f.backendPort, "backend-port", cluster.DefaultBackendPort, "kudurru DNS port for entries without a port and EndpointSlices without the named port")
	fl.StringVar(&f.backendPortName, "backend-port-name", "dns", "EndpointSlice port name carrying DNS")
	fl.IntVar(&f.internalPort, "internal-port", cluster.DefaultInternalPort, "kudurru internal listener port (zone serial polling); 0 disables")
	fl.StringVar(&f.poolResolver, "pool-resolver", "chashed", "load-balancing policy of the resolver pool")
	fl.StringVar(&f.poolAuthoritative, "pool-authoritative", "leastOutstanding", "load-balancing policy of the authoritative pool")
	fl.StringVar(&f.tlsDir, "tls-dir", "", "directory with tls.crt+tls.key (or fullchain.pem+privkey.pem): enables DoT/DoH/DoQ/DoH3, watched for rotation")
	fl.StringSliceVar(&f.listen, "listen", []string{"0.0.0.0"}, "addresses every frontend binds")
	fl.StringSliceVar(&f.protocols, "protocols", nil, "frontends (do53,dot,doh,doq,doh3); default do53 plus all TLS ones when --tls-dir is set")
	f.frontendAddrs = map[edge.Protocol]*string{}
	for _, p := range []edge.Protocol{edge.DoT, edge.DoH, edge.DoQ, edge.DoH3} {
		f.frontendAddrs[p] = fl.String(strings.ToLower(string(p)), "", fmt.Sprintf("enable the %s frontend on host:port (the host must be a --listen address; the chart's form); "+
			"with any of --dot/--doh/--doq/--doh3 the frontends are do53 plus those given", p))
	}
	fl.StringVar(&f.api, "api", "127.0.0.1:8083", "dnsdist webserver/API address (loopback only)")
	fl.StringVar(&f.console, "console", "127.0.0.1:5199", "dnsdist console address (loopback only)")
	fl.StringVar(&f.apiKeyOut, "api-key-out", "", "write the per-start dnsdist API key here (0640), e.g. /run/kudurru/dnsdist-api.key")
	fl.StringVar(&f.apiKeyGroup, "api-key-group", "", "group (name or gid) of the --api-key-out file, e.g. keepalived_script; default unchanged")
	fl.StringVar(&f.proxySecretFile, "proxy-protocol-secret-file", "", "PROXYv2 TLV 0xE0 shared secret (printable, >= 32 bytes: openssl rand -hex 32); required unless every backend is loopback")
	fl.BoolVar(&f.authCache, "authoritative-cache", false, "cache the authoritative pool briefly and flush it on zone serial changes")
	fl.IntVar(&f.authCacheTTL, "authoritative-cache-ttl", 30, "max TTL (s) of the authoritative cache")
	fl.StringSliceVar(&f.authZones, "authoritative-zones", nil, "zones routed to the authoritative pool, in addition to those polled from /internal/zones")
	fl.StringSliceVar(&f.transferSources, "transfer-sources", nil, "addresses/prefixes allowed to send AXFR/IXFR/NOTIFY/UPDATE through the edge")
	fl.StringSliceVar(&f.acl, "acl", nil, "client prefixes allowed to query (default: private and loopback ranges)")
	fl.IntVar(&f.resolverCacheSize, "cache-size", 200000, "resolver pool packet cache entries")
	fl.BoolVar(&f.dynblocks, "dynblocks", false, "enable dnsdist dynamic blocking")
	fl.IntVar(&f.dynQPS, "dynblocks-qps", 50, "per-client query rate (qps over 10 s) that triggers a dynamic block")
	fl.IntVar(&f.dynNX, "dynblocks-nxdomain-rate", 20, "per-client NXDOMAIN rate that triggers a dynamic block; 0 disables")
	fl.BoolVar(&f.ebpf, "ebpf", false, "push dynamic-block drops into an eBPF socket filter (needs CAP_SYS_ADMIN)")
	fl.StringVar(&f.renderer, "renderer", "yaml", "configuration format: yaml (dnsdist.yml + functions-only dnsdist.lua) or lua (dnsdist.conf)")
	fl.StringVar(&f.serverID, "server-id", "", "dnsdist general.server_id; default the host name")
	fl.IntVar(&f.threads, "threads", 2, "Do53 listener threads")
	if run {
		fl.StringVar(&f.dnsdistBin, "dnsdist-bin", "dnsdist", "dnsdist executable")
		fl.StringVar(&f.dnsdistUser, "dnsdist-user", "", "uid[:gid] dnsdist switches to after binding its sockets and creating the eBPF maps (dnsdist --uid/--gid), for a supervisor started as root; dnsdist keeps only capabilities_to_retain")
		fl.StringVar(&f.configDir, "config-dir", "/etc/dnsdist", "directory for the rendered configuration")
		fl.StringVar(&f.healthAddr, "health-address", ":8084", "supervisor listener: /healthz/ready, /healthz/live, /status, /metrics")
		fl.StringVar(&f.healthAddr, "readiness-address", ":8084", "alias of --health-address (the chart's form)")
		fl.DurationVar(&f.debounce, "debounce", 5*time.Second, "delay before re-rendering after membership or certificate changes")
		fl.DurationVar(&f.drain, "drain", time.Second, "readiness 503 period before a restart")
		fl.DurationVar(&f.zonePoll, "zone-poll", 5*time.Second, "interval of the /internal/zones serial poll")
		fl.BoolVar(&f.pollZones, "poll-zones", true, "poll backends' /internal/zones (needs KUDURRU_ENCRYPTION_KEY[_FILE] or --internal-token-file)")
		fl.DurationVar(&f.stopTimeout, "stop-timeout", 10*time.Second, "dnsdist SIGTERM grace period before SIGKILL")
		fl.StringVar(&f.internalTokenFile, "internal-token-file", "", "bearer token of the kudurru internal listener (default derived from KUDURRU_ENCRYPTION_KEY)")
		fl.StringVar(&f.internalCA, "internal-ca-file", "", "CA of the kudurru internal listener (default derived from KUDURRU_ENCRYPTION_KEY)")
		fl.StringVar(&f.internalSN, "internal-server-name", "", "TLS server name of the internal listener (default internal.kudurru.invalid)")
		fl.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn or error")
		fl.StringVar(&f.logFormat, "log-format", "json", "json or text")
	} else {
		fl.StringVar(&f.out, "out", "", "write the rendered files into this directory instead of stdout")
	}
}

func newEdgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edge",
		Short: "dnsdist supervisor: render the configuration or run dnsdist (PID 1 of the kudurru-dnsdist image)",
	}
	var rf, uf edgeFlags
	render := &cobra.Command{
		Use:   "render",
		Short: "Print the dnsdist configuration the supervisor would run (credentials are random per call)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runEdgeRender(c, &rf)
		},
	}
	rf.register(render, false)
	run := &cobra.Command{
		Use:   "run",
		Short: "Render, check, exec and supervise dnsdist (docs/architecture.md §3.6)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runEdgeRun(c, &uf)
		},
	}
	uf.register(run, true)
	cmd.AddCommand(render, run)
	return cmd
}

// options maps the flags to edge.Options.
func (f *edgeFlags) options() (edge.Options, error) {
	o := edge.Options{
		ServerID: f.serverID, Listen: f.listen,
		PoolResolver: f.poolResolver, PoolAuthoritative: f.poolAuthoritative,
		ResolverCacheSize:  f.resolverCacheSize,
		AuthoritativeCache: f.authCache, AuthoritativeCacheTTL: f.authCacheTTL,
		API: f.api, Console: f.console,
		ACL: f.acl, TransferSources: f.transferSources, AuthoritativeZones: f.authZones,
		Threads: f.threads,
		EBPF:    edge.EBPF{Enabled: f.ebpf},
	}
	if f.dynblocks {
		o.DynBlocks = edge.DynBlocks{Enabled: true, QueryRate: f.dynQPS, NXDomainRate: f.dynNX}
	}
	for _, p := range f.protocols {
		proto, err := edge.ParseProtocol(strings.TrimSpace(p))
		if err != nil {
			return o, err
		}
		o.Protocols = append(o.Protocols, proto)
	}
	if err := f.applyFrontendAddrs(&o); err != nil {
		return o, err
	}
	if f.tlsDir != "" {
		cert, key, err := edge.FindTLS(f.tlsDir)
		if err != nil {
			return o, err
		}
		o.CertFile, o.KeyFile = cert, key
	}
	if f.proxySecretFile != "" {
		secret, err := proxyproto.LoadSecretFile(f.proxySecretFile)
		if err != nil {
			return o, err
		}
		if err := edge.ValidSecret(secret); err != nil {
			return o, fmt.Errorf("--proxy-protocol-secret-file: %w", err)
		}
		o.ProxyProtocolSecret = string(secret)
	}
	return o, nil
}

// source builds the membership source.
func (f *edgeFlags) source() (cluster.MembershipSource, error) {
	spec, err := cluster.ParseSourceSpec(f.backendsFrom)
	if err != nil {
		return nil, err
	}
	switch spec.Kind {
	case "endpointslice":
		if f.backends != "" {
			return nil, errors.New("--backends and --backends-from endpointslice are mutually exclusive")
		}
		rc, err := ctrlconfig.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes client: %w", err)
		}
		dc, err := discoveryv1client.NewForConfig(rc)
		if err != nil {
			return nil, err
		}
		ip := f.internalPort
		if ip == 0 {
			ip = -1
		}
		return &cluster.EndpointSliceSource{Client: dc, Namespace: spec.Namespace, Service: spec.Service,
			PortName: f.backendPortName, DefaultPort: f.backendPort, InternalPort: ip}, nil
	default:
		eps, err := cluster.ParseStaticEndpoints(f.backends, f.backendPort, f.internalPort)
		if err != nil {
			return nil, fmt.Errorf("--backends: %w", err)
		}
		return &cluster.StaticSource{Endpoints: eps}, nil
	}
}

// requireSecret enforces D56: non-loopback backends need the TLV secret.
func requireSecret(o edge.Options, eps []cluster.Endpoint) error {
	if o.ProxyProtocolSecret != "" {
		return nil
	}
	for _, e := range eps {
		if !isLoopbackHostPort(e.Address) {
			return fmt.Errorf("backend %s is not loopback: --proxy-protocol-secret-file is required (D56: kudurru drops unauthenticated PROXYv2 headers from non-loopback peers)", e.Address)
		}
	}
	return nil
}

func runEdgeRender(c *cobra.Command, f *edgeFlags) error {
	o, err := f.options()
	if err != nil {
		return err
	}
	src, err := f.source()
	if err != nil {
		return err
	}
	eps, err := src.List(c.Context())
	if err != nil {
		return err
	}
	eps = cluster.FilterServing(eps)
	if err := requireSecret(o, eps); err != nil {
		return err
	}
	creds, err := edge.NewCredentials()
	if err != nil {
		return err
	}
	bes := make([]edge.Backend, 0, len(eps))
	for _, e := range eps {
		bes = append(bes, edge.Backend{Name: e.Name, Address: e.Address, InternalAddress: e.InternalAddress})
	}
	top, err := edge.Build(o, bes, creds)
	if err != nil {
		return err
	}
	r, err := dnsdist.NewRenderer(f.renderer)
	if err != nil {
		return err
	}
	files, err := r.Render(top)
	if err != nil {
		return err
	}
	for _, file := range files {
		if f.out != "" {
			if err := os.MkdirAll(f.out, 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(f.out, file.Name), file.Data, 0o600); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "# ---- %s ----\n%s", file.Name, file.Data); err != nil {
			return err
		}
	}
	return nil
}

// supervisorLogger builds the supervisors' structured logger.
func supervisorLogger(level, format string) (*slog.Logger, error) {
	switch format {
	case "json", "text":
	default:
		return nil, fmt.Errorf("--log-format %q: want json or text", format)
	}
	return newLogger(config.Log{Level: level, Format: format}), nil
}

// lookupGID resolves a group name or numeric gid; "" is -1 (unchanged).
func lookupGID(g string) (int, error) {
	if g == "" {
		return -1, nil
	}
	if n, err := strconv.Atoi(g); err == nil {
		return n, nil
	}
	grp, err := user.LookupGroup(g)
	if err != nil {
		return -1, fmt.Errorf("--api-key-group %q: %w", g, err)
	}
	return strconv.Atoi(grp.Gid)
}

// signalContext cancels on SIGTERM/SIGINT and forwards SIGHUP, SIGUSR1
// and SIGUSR2 to the child through forward.
func signalContext(parent context.Context, forward func(os.Signal)) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-ch:
				switch s {
				case syscall.SIGTERM, syscall.SIGINT:
					cancel()
					return
				default:
					forward(s)
				}
			}
		}
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

func runEdgeRun(c *cobra.Command, f *edgeFlags) error {
	logger, err := supervisorLogger(f.logLevel, f.logFormat)
	if err != nil {
		return err
	}
	o, err := f.options()
	if err != nil {
		return err
	}
	src, err := f.source()
	if err != nil {
		return err
	}
	if st, ok := src.(*cluster.StaticSource); ok {
		if err := requireSecret(o, st.Endpoints); err != nil {
			return err
		}
	} else if o.ProxyProtocolSecret == "" {
		return errors.New("--backends-from endpointslice needs --proxy-protocol-secret-file (pod IPs are never loopback; D56, D72)")
	}
	r, err := dnsdist.NewRenderer(f.renderer)
	if err != nil {
		return err
	}
	gid, err := lookupGID(f.apiKeyGroup)
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if os.Geteuid() == 0 && f.dnsdistUser == "" {
		logger.Warn("the supervisor runs as root and --dnsdist-user is not set: dnsdist keeps running as root")
	}
	cfg := dnsdist.Config{
		Options: o, Source: src, Renderer: r, ConfigDir: f.configDir, Binary: f.dnsdistBin, User: f.dnsdistUser,
		APIKeyOut: f.apiKeyOut, APIKeyGID: gid, TLSDir: f.tlsDir,
		Debounce: f.debounce, Drain: f.drain, ZonePoll: f.zonePoll, StopTimeout: f.stopTimeout,
		Logger: logger, Registerer: reg, Gatherer: reg,
	}
	if f.pollZones && f.internalPort > 0 {
		zl, err := f.zoneLister()
		switch {
		case err != nil:
			return err
		case zl == nil:
			logger.Warn("zone polling disabled: no KUDURRU_ENCRYPTION_KEY[_FILE] and no --internal-token-file; authoritative routing uses --authoritative-zones only")
		default:
			cfg.Zones = zl
		}
	}
	sup, err := dnsdist.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signalContext(c.Context(), func(s os.Signal) {
		logger.Info("forwarding signal to dnsdist", "signal", s.String())
		_ = sup.Signal(s)
	})
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Serve(ctx, f.healthAddr) }()
	runErr := sup.Run(ctx)
	stop()
	if err := <-errCh; err != nil && runErr == nil {
		runErr = fmt.Errorf("health listener: %w", err)
	}
	return runErr
}

// zoneLister returns the internal-API client for /internal/zones, or nil
// when no internal credentials are configured.
func (f *edgeFlags) zoneLister() (dnsdist.ZoneLister, error) {
	key, prev, err := readEncryptionKeys()
	if err != nil {
		return nil, err
	}
	if key == nil && f.internalTokenFile == "" {
		return nil, nil
	}
	host, _ := os.Hostname()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{
		Key: key, PreviousKey: prev, NodeName: host, TokenFile: f.internalTokenFile,
		CAFile: f.internalCA, ServerName: f.internalSN,
	})
	if err != nil {
		return nil, err
	}
	return internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 3 * time.Second}), nil
}

func isLoopbackHostPort(hp string) bool {
	host, _, err := net.SplitHostPort(hp)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// applyFrontendAddrs maps --dot/--doh/--doq/--doh3 host:port onto
// Options.Protocols and Options.Ports. Without --protocols the frontends
// become do53 plus the ones given.
func (f *edgeFlags) applyFrontendAddrs(o *edge.Options) error {
	given := false
	for _, p := range []edge.Protocol{edge.DoT, edge.DoH, edge.DoQ, edge.DoH3} {
		addr := f.frontendAddrs[p]
		if addr == nil || *addr == "" {
			continue
		}
		host, port, err := net.SplitHostPort(*addr)
		if err != nil {
			return fmt.Errorf("--%s %q: want host:port", strings.ToLower(string(p)), *addr)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("--%s %q: bad port", strings.ToLower(string(p)), *addr)
		}
		listen := o.Listen
		if len(listen) == 0 {
			listen = []string{"0.0.0.0"}
		}
		if host != "" && !slices.Contains(listen, host) {
			return fmt.Errorf("--%s %q: host %s is not a --listen address (%s)", strings.ToLower(string(p)), *addr, host, strings.Join(listen, ","))
		}
		if !given && len(f.protocols) == 0 {
			o.Protocols = []edge.Protocol{edge.Do53}
		}
		given = true
		if !slices.Contains(o.Protocols, p) {
			o.Protocols = append(o.Protocols, p)
		}
		if o.Ports == nil {
			o.Ports = map[edge.Protocol]int{}
		}
		o.Ports[p] = n
	}
	return nil
}
