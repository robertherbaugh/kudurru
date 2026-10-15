package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// vipFlags are the `kudurru vip render|run` flags (§3.6, §6.5).
type vipFlags struct {
	vips, preempt          []string
	peers                  []string
	peersFile, peersFrom   string
	priorityIndex          int
	nodeCount              int
	unicastSrc             string
	vrrpInterface          string
	hmacKeyFile            string
	legacy                 bool
	dnsdistAPI, apiKeyFile string
	requireBackends        int
	probeCommand           string
	runDir, routerID       string
	kudurruBin             string
	// run only
	keepalivedBin, configPath, healthAddr string
	stopTimeout                           time.Duration
	logLevel, logFormat                   string
}

func (f *vipFlags) register(cmd *cobra.Command, run bool) {
	fl := cmd.Flags()
	fl.StringArrayVar(&f.vips, "vip", nil, "VIP as name=addr/len@iface#vrid (repeatable; the second VIP gets reversed priorities); the VRID is host-only and never changes")
	fl.StringArrayVar(&f.preempt, "preempt-delay", nil, "name=seconds: preempt_delay instead of nopreempt for that VIP")
	fl.StringSliceVar(&f.peers, "peers", nil, "the other nodes' unicast addresses")
	fl.StringVar(&f.peersFile, "peers-file", "", "file with one peer address per line, watched: re-render + SIGHUP on change (overrides --peers)")
	fl.StringVar(&f.peersFrom, "peers-from", "", "nodes:<label selector>: peers are the selected Kubernetes Nodes' InternalIPs (this node excluded), re-listed every 30 s; the priority index defaults to this node's position by name (or its kudurru.io/vip-priority-index label)")
	fl.IntVar(&f.priorityIndex, "priority-index", 0, "this node's index: priority 200-10*index for the first VIP")
	fl.IntVar(&f.nodeCount, "node-count", 0, "number of nodes (default: peers + 1)")
	fl.StringVar(&f.unicastSrc, "unicast-src", "", "this node's VRRP source address (default: the first address of --vrrp-interface, or the first VIP's interface)")
	fl.StringVar(&f.vrrpInterface, "vrrp-interface", "", "interface for VRRP advertisements (default: each VIP's interface); VIP addresses stay on the interfaces in --vip")
	fl.StringVar(&f.hmacKeyFile, "hmac-key-file", "/run/secrets/vrrp_hmac_{vrid}", "auth_hmac key file; {vrid} is replaced by the VRID")
	fl.BoolVar(&f.legacy, "legacy", false, "keepalived 2.3.x: render without auth_hmac (adverts unauthenticated)")
	fl.StringVar(&f.dnsdistAPI, "track-dnsdist-api", "http://127.0.0.1:8083", "dnsdist API the VRRP probe checks")
	fl.StringVar(&f.apiKeyFile, "dnsdist-api-key-file", "/run/kudurru/dnsdist-api.key", "dnsdist API key file written by `kudurru edge run`")
	fl.IntVar(&f.requireBackends, "require-backends", 1, "backends that must be up for this node to hold a VIP")
	fl.StringVar(&f.probeCommand, "probe-command", strings.Join(keepalived.DefaultProbeCommand, " "), "VRRP check command prefix rendered into vrrp_script (e.g. \"/usr/local/bin/kudurru health --vrrp\")")
	fl.StringVar(&f.runDir, "run-dir", keepalived.DefaultRunDir, "shared run directory: FIFO, track files, state, dnsdist API key")
	fl.StringVar(&f.routerID, "router-id", "", "node name (router_id); default $KUDURRU_NODE__NAME or the host name")
	fl.StringVar(&f.routerID, "node-name", "", "alias of --router-id (the chart passes spec.nodeName)")
	fl.StringVar(&f.kudurruBin, "kudurru-bin", "/usr/local/bin/kudurru", "kudurru path used in rendered scripts (root-owned for enable_script_security)")
	if run {
		fl.StringVar(&f.keepalivedBin, "keepalived-bin", "keepalived", "keepalived executable")
		fl.StringVar(&f.configPath, "config-path", "/etc/keepalived/keepalived.conf", "rendered configuration")
		fl.StringVar(&f.healthAddr, "health-address", ":8085", "supervisor listener: /healthz/ready, /healthz/live, /status, /metrics")
		fl.DurationVar(&f.stopTimeout, "stop-timeout", 15*time.Second, "keepalived SIGTERM grace period (keep below the container stop grace period)")
		fl.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn or error")
		fl.StringVar(&f.logFormat, "log-format", "json", "json or text")
	}
}

func nodeName(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("KUDURRU_NODE__NAME"); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return h
}

// config maps the flags to keepalived.Config.
func (f *vipFlags) config() (keepalived.Config, error) {
	c := keepalived.Config{
		RouterID: nodeName(f.routerID), PriorityIndex: f.priorityIndex, NodeCount: f.nodeCount,
		Peers: f.peers, HMACKeyFile: f.hmacKeyFile, Legacy: f.legacy, RunDir: f.runDir, Binary: f.kudurruBin,
		UnicastSrc: f.unicastSrc, VRRPInterface: f.vrrpInterface,
		Probe: keepalived.Probe{
			Command: strings.Fields(f.probeCommand), DNSDistAPI: f.dnsdistAPI,
			DNSDistAPIKeyFile: f.apiKeyFile, RequireBackends: f.requireBackends,
		},
	}
	if len(f.vips) == 0 {
		return c, errors.New("at least one --vip is required")
	}
	for _, s := range f.vips {
		v, err := keepalived.ParseVIP(s)
		if err != nil {
			return c, err
		}
		c.VIPs = append(c.VIPs, v)
	}
	for _, s := range f.preempt {
		name, secs, ok := strings.Cut(s, "=")
		n, err := strconv.Atoi(secs)
		if !ok || err != nil {
			return c, fmt.Errorf("--preempt-delay %q: want name=seconds", s)
		}
		found := false
		for i := range c.VIPs {
			if c.VIPs[i].Name == name {
				c.VIPs[i].PreemptDelay, found = n, true
			}
		}
		if !found {
			return c, fmt.Errorf("--preempt-delay %q: no such VIP", s)
		}
	}
	if f.peersFile != "" {
		peers, err := keepalived.ReadPeersFile(f.peersFile)
		if err != nil {
			return c, err
		}
		c.Peers = peers
	}
	if c.UnicastSrc == "" {
		v := c.VIPs[0]
		if c.VRRPInterface != "" {
			v.Interface = c.VRRPInterface
		}
		src, err := interfaceAddress(v)
		if err != nil {
			return c, fmt.Errorf("--unicast-src not given and %w", err)
		}
		c.UnicastSrc = src
	}
	return c, nil
}

// interfaceAddress returns the first address of the VIP's interface in
// the VIP's family that is not the VIP itself.
func interfaceAddress(v keepalived.VIP) (string, error) {
	ifc, err := net.InterfaceByName(v.Interface)
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", v.Interface, err)
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil || p.Addr() == v.Prefix.Addr() || p.Addr().Is4() != v.Prefix.Addr().Is4() || p.Addr().IsLinkLocalUnicast() {
			continue
		}
		return p.Addr().String(), nil
	}
	return "", fmt.Errorf("interface %s has no usable address", v.Interface)
}

func newVIPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vip",
		Short: "keepalived supervisor and VIP tools (PID 1 of the kudurru-keepalived image)",
	}
	var rf, uf vipFlags
	render := &cobra.Command{
		Use:   "render",
		Short: "Print the keepalived.conf the supervisor would run",
		RunE: func(c *cobra.Command, _ []string) error {
			if _, _, err := rf.nodePeers(c, false); err != nil {
				return err
			}
			kc, err := rf.config()
			if err != nil {
				return err
			}
			b, err := keepalived.Render(kc)
			if err != nil {
				return err
			}
			_, err = c.OutOrStdout().Write(b)
			return err
		},
	}
	rf.register(render, false)
	run := &cobra.Command{
		Use:   "run",
		Short: "Render, keepalived -t, exec and supervise keepalived; SIGHUP on re-render",
		RunE: func(c *cobra.Command, _ []string) error {
			return runVIP(c, &uf)
		},
	}
	uf.register(run, true)
	cmd.AddCommand(render, run, newVIPFIFOCmd(), newVIPRebalanceCmd(), newVIPDrainCmd(), newVIPProbeCmd())
	return cmd
}

func runVIP(c *cobra.Command, f *vipFlags) error {
	logger, err := supervisorLogger(f.logLevel, f.logFormat)
	if err != nil {
		return err
	}
	if err := requireFeature(logger, license.FeatureEdge); err != nil {
		return err
	}
	np, initialPeers, err := f.nodePeers(c, true)
	if err != nil {
		return err
	}
	kc, err := f.config()
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	sup, err := keepalived.NewSupervisor(keepalived.SupervisorConfig{
		Config: kc, ConfigPath: f.configPath, Binary: f.keepalivedBin, PeersFile: f.peersFile,
		StopTimeout: f.stopTimeout, Logger: logger, Registerer: reg, Gatherer: reg,
	})
	if err != nil {
		return err
	}
	ctx, stop := signalContext(c.Context(), func(s os.Signal) {
		logger.Info("forwarding signal to keepalived", "signal", s.String())
		_ = sup.Signal(s)
	})
	defer stop()
	if np != nil {
		go np.Sync(ctx, f.peersFile, initialPeers, logger)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Serve(ctx, f.healthAddr) }()
	runErr := sup.Run(ctx)
	stop()
	if err := <-errCh; err != nil && runErr == nil {
		runErr = fmt.Errorf("health listener: %w", err)
	}
	return runErr
}

// newVIPFIFOCmd is the vrrp_notify_fifo_script: keepalived starts it with
// the FIFO path appended and it maintains <run dir>/vip-state.json.
func newVIPFIFOCmd() *cobra.Command {
	var node, runDir string
	cmd := &cobra.Command{
		Use:   "fifo [--node name] [--run-dir dir] <fifo>",
		Short: "Consume keepalived's notify FIFO (run by keepalived as vrrp_notify_fifo_script)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			cons := &keepalived.Consumer{RunDir: runDir, Node: nodeName(node), Log: newLogger(config.Log{Level: "info", Format: "json"})}
			return cons.ConsumeFIFO(ctx, args[0])
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "node name (keepalived router_id)")
	cmd.Flags().StringVar(&runDir, "run-dir", keepalived.DefaultRunDir, "directory of vip-state.json")
	return cmd
}

// newVIPRebalanceCmd moves VIPs this node holds without being their
// preferred owner (priority below 200) back: track file 300 (FAULT, the
// only weight that moves a VIP under nopreempt), wait until this node is
// no longer MASTER, then 0.
func newVIPRebalanceCmd() *cobra.Command {
	var (
		runDir  string
		only    []string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "rebalance",
		Short: "Hand VIPs this node holds but does not prefer back to their preferred node (writes 300, then 0)",
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := keepalived.ReadState(runDir)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			moved := 0
			for name, v := range st.VIPs {
				want := len(only) == 0 && v.State == keepalived.StateMaster && v.Priority < 200
				for _, o := range only {
					want = want || (o == name && v.State == keepalived.StateMaster)
				}
				if !want {
					continue
				}
				_, _ = fmt.Fprintf(out, "%s: MASTER at priority %d, moving it away\n", name, v.Priority)
				if err := keepalived.WriteWeight(runDir, name, keepalived.WeightFault); err != nil {
					return err
				}
				deadline := time.Now().Add(timeout)
				for {
					cur, err := keepalived.ReadState(runDir)
					if err == nil && cur.VIPs[name].State != keepalived.StateMaster {
						break
					}
					if time.Now().After(deadline) {
						_ = keepalived.WriteWeight(runDir, name, keepalived.WeightServing)
						return fmt.Errorf("%s: still MASTER after %s; weight reset to 0", name, timeout)
					}
					time.Sleep(200 * time.Millisecond)
				}
				if err := keepalived.WriteWeight(runDir, name, keepalived.WeightServing); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "%s: released; weight back to 0\n", name)
				moved++
			}
			if moved == 0 {
				_, _ = fmt.Fprintln(out, "nothing to rebalance")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runDir, "run-dir", keepalived.DefaultRunDir, "shared run directory")
	cmd.Flags().StringSliceVar(&only, "vip", nil, "move these VIPs (if held) regardless of priority")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "how long to wait for keepalived to leave MASTER")
	return cmd
}

// newVIPDrainCmd is the operator's explicit "move the VIP" (D51): 300 in
// the track file; --undo writes 0.
func newVIPDrainCmd() *cobra.Command {
	var (
		runDir string
		undo   bool
	)
	cmd := &cobra.Command{
		Use:   "drain <vip>...",
		Short: "Move VIPs off this node for maintenance (track file 300); --undo writes 0",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w := keepalived.WeightFault
			if undo {
				w = keepalived.WeightServing
			}
			for _, name := range args {
				if err := keepalived.WriteWeight(runDir, name, w); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(c.OutOrStdout(), "%s: weight %d\n", name, w)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runDir, "run-dir", keepalived.DefaultRunDir, "shared run directory")
	cmd.Flags().BoolVar(&undo, "undo", false, "end the drain (weight 0)")
	return cmd
}
