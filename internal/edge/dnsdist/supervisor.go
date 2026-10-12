package dnsdist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/edge"
)

// ZoneLister reads a backend's authoritative zones and serials
// (internalapi.Client.Zones against GET /internal/zones).
type ZoneLister interface {
	Zones(ctx context.Context, addr string) (internalapi.ZoneList, error)
}

// Config configures a Supervisor.
type Config struct {
	// Options shape the Topology; AuthoritativeZones are merged with the
	// zones polled from backends.
	Options edge.Options
	// Source supplies the backends.
	Source cluster.MembershipSource
	// Renderer is YAMLRenderer (default) or LuaRenderer.
	Renderer Renderer
	// ConfigDir holds the rendered files; default /etc/dnsdist.
	ConfigDir string
	// Binary is the dnsdist executable; default "dnsdist". Tests point it
	// at a fake.
	Binary string
	// ChildEnv is appended to the child's environment (tests).
	ChildEnv []string
	// User, when set ("<uid>[:<gid>]", numbers or names), is passed to
	// dnsdist as --uid/--gid. A supervisor started as root (eBPF, host
	// networking without the unprivileged-port sysctl) lets dnsdist bind its
	// sockets and create the eBPF maps as root; dnsdist then switches to
	// this user and keeps only general.capabilities_to_retain.
	User string
	// APIKeyOut receives the per-start API key (mode 0640) for `kudurru
	// health --vrrp` and the local kudurru's `/edge` page; empty skips it.
	APIKeyOut string
	// APIKeyGID, when >= 0, is the group the key file is chowned to (the
	// keepalived_script group of the keepalived image).
	APIKeyGID int
	// TLSDir is watched for certificate changes (restart); empty disables.
	TLSDir string
	// Debounce delays re-rendering after membership and certificate
	// changes; default 5 s.
	Debounce time.Duration
	// Drain is how long readiness reports 503 before a restart; default 0.
	Drain time.Duration
	// Zones polls backends' /internal/zones every ZonePoll (default 5 s);
	// nil disables polling.
	Zones    ZoneLister
	ZonePoll time.Duration
	// StatusPoll is the dnsdist API poll interval feeding readiness and
	// kudurru_edge_backend_up; default 1 s.
	StatusPoll time.Duration
	// StopTimeout bounds dnsdist's SIGTERM grace period; default 10 s.
	StopTimeout time.Duration
	// NewCredentials generates the per-start credentials; default
	// edge.NewCredentials.
	NewCredentials func() (edge.Credentials, error)
	// Logger and Registerer default to slog.Default() and a private
	// registry.
	Logger     *slog.Logger
	Registerer prometheus.Registerer
	// Gatherer serves /metrics; default the private registry.
	Gatherer prometheus.Gatherer
}

// Supervisor is `kudurru edge run`: render → `dnsdist --check-config` →
// atomic swap → run dnsdist as a child, and on membership, zone or
// certificate changes re-render and restart (v0.1, D17).
type Supervisor struct {
	cfg   Config
	log   *slog.Logger
	api   *APIClient
	child *edge.Child
	creds edge.Credentials

	backendUp *prometheus.GaugeVec
	restarts  *prometheus.CounterVec
	renders   *prometheus.CounterVec
	flushes   prometheus.Counter
	rendered  prometheus.Gauge
	gatherer  prometheus.Gatherer

	mu         sync.Mutex
	status     *ServerStatus
	statusAt   time.Time
	statusErr  error
	restarting bool
	current    []File
	endpoints  []cluster.Endpoint
	zones      []string
}

// New validates cfg and returns a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	if cfg.Source == nil {
		return nil, errors.New("dnsdist: no membership source")
	}
	if cfg.Renderer == nil {
		cfg.Renderer = YAMLRenderer{}
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = "/etc/dnsdist"
	}
	if cfg.Binary == "" {
		cfg.Binary = "dnsdist"
	}
	if cfg.User != "" {
		u, g, _ := strings.Cut(cfg.User, ":")
		if u == "" || strings.ContainsAny(cfg.User, " \t") || (strings.Contains(cfg.User, ":") && g == "") {
			return nil, fmt.Errorf("dnsdist: user %q: want <uid>[:<gid>]", cfg.User)
		}
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 5 * time.Second
	}
	if cfg.ZonePoll <= 0 {
		cfg.ZonePoll = 5 * time.Second
	}
	if cfg.StatusPoll <= 0 {
		cfg.StatusPoll = time.Second
	}
	if cfg.NewCredentials == nil {
		cfg.NewCredentials = edge.NewCredentials
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Registerer == nil {
		reg := prometheus.NewRegistry()
		cfg.Registerer, cfg.Gatherer = reg, reg
	}
	if cfg.Gatherer == nil {
		cfg.Gatherer = prometheus.DefaultGatherer
	}
	if cfg.Options.API == "" {
		cfg.Options.API = "127.0.0.1:8083"
	}
	s := &Supervisor{
		cfg: cfg,
		log: cfg.Logger.With("component", "edge"),
		backendUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_edge_backend_up", Help: "1 when dnsdist reports the kudurru backend up.",
		}, []string{"backend"}),
		restarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_edge_dnsdist_restarts_total", Help: "dnsdist restarts by reason (membership, zones, certificates, exit).",
		}, []string{"reason"}),
		renders: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_edge_renders_total", Help: "Configuration renders by result (ok, unchanged, invalid, check_failed).",
		}, []string{"result"}),
		flushes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kudurru_edge_cache_flushes_total", Help: "Authoritative packet-cache flushes after zone serial changes.",
		}),
		rendered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_edge_backends", Help: "Backends in the running dnsdist configuration.",
		}),
		gatherer: cfg.Gatherer,
	}
	for _, c := range []prometheus.Collector{s.backendUp, s.restarts, s.renders, s.flushes, s.rendered} {
		if err := cfg.Registerer.Register(c); err != nil {
			return nil, fmt.Errorf("dnsdist: register metrics: %w", err)
		}
	}
	return s, nil
}

// MainPath is the configuration file dnsdist runs from.
func (s *Supervisor) MainPath() string {
	return filepath.Join(s.cfg.ConfigDir, s.cfg.Renderer.Main())
}

// Args returns the dnsdist command line of the child.
func (s *Supervisor) Args() []string {
	args := []string{"--supervised", "--disable-syslog"}
	if s.cfg.User != "" {
		u, g, ok := strings.Cut(s.cfg.User, ":")
		args = append(args, "--uid", u)
		if ok {
			args = append(args, "--gid", g)
		}
	}
	return append(args, "-C", s.MainPath())
}

// candidateName inserts ".candidate" before the extension:
// dnsdist.yml → dnsdist.candidate.yml, keeping the .yml/.lua sibling
// relationship dnsdist relies on.
func candidateName(name string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + ".candidate" + ext
}

// topology builds the topology for the given endpoints and zones.
func (s *Supervisor) topology(eps []cluster.Endpoint, zones []string) (*edge.Topology, error) {
	o := s.cfg.Options
	o.AuthoritativeZones = append(slices.Clone(o.AuthoritativeZones), zones...)
	bes := make([]edge.Backend, 0, len(eps))
	for _, e := range eps {
		bes = append(bes, edge.Backend{Name: e.Name, Address: e.Address, InternalAddress: e.InternalAddress})
	}
	return edge.Build(o, bes, s.creds)
}

// render renders, writes candidates, validates them with dnsdist and
// swaps them in. It reports whether the files changed.
func (s *Supervisor) render(ctx context.Context, eps []cluster.Endpoint, zones []string) (bool, error) {
	top, err := s.topology(eps, zones)
	if err != nil {
		s.renders.WithLabelValues("invalid").Inc()
		return false, err
	}
	files, err := s.cfg.Renderer.Render(top)
	if err != nil {
		s.renders.WithLabelValues("invalid").Inc()
		return false, err
	}
	s.mu.Lock()
	same := equalFiles(files, s.current)
	s.mu.Unlock()
	if same {
		s.renders.WithLabelValues("unchanged").Inc()
		return false, nil
	}
	if err := os.MkdirAll(s.cfg.ConfigDir, 0o750); err != nil {
		return false, err
	}
	var main string
	for _, f := range files {
		p := filepath.Join(s.cfg.ConfigDir, candidateName(f.Name))
		if err := edge.WriteFileAtomic(p, f.Data, 0o600); err != nil {
			return false, err
		}
		if f.Name == s.cfg.Renderer.Main() {
			main = p
		}
	}
	if out, err := edge.Check(ctx, s.cfg.Binary, "--check-config", "-C", main); err != nil {
		s.renders.WithLabelValues("check_failed").Inc()
		return false, err
	} else if out != "" {
		s.log.Debug("dnsdist --check-config", "output", out)
	}
	// Siblings first, the main file last: a crash in between leaves the
	// old main file, which never references a function that is missing.
	order := slices.Clone(files)
	slices.SortStableFunc(order, func(a, b File) int {
		switch {
		case a.Name == s.cfg.Renderer.Main():
			return 1
		case b.Name == s.cfg.Renderer.Main():
			return -1
		}
		return 0
	})
	for _, f := range order {
		if err := edge.SwapFile(filepath.Join(s.cfg.ConfigDir, candidateName(f.Name)), filepath.Join(s.cfg.ConfigDir, f.Name)); err != nil {
			return false, err
		}
	}
	s.mu.Lock()
	s.current = files
	s.endpoints = eps
	s.zones = zones
	s.mu.Unlock()
	s.rendered.Set(float64(len(top.Backends)))
	s.renders.WithLabelValues("ok").Inc()
	s.log.Info("rendered dnsdist configuration", "file", s.MainPath(), "renderer", s.cfg.Renderer.Name(),
		"backends", len(top.Backends), "authoritative_zones", len(top.Rules.AuthoritativeZones))
	return true, nil
}

func equalFiles(a, b []File) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// writeAPIKey publishes the per-start API key for the VRRP probe.
func (s *Supervisor) writeAPIKey() error {
	if s.cfg.APIKeyOut == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.APIKeyOut), 0o755); err != nil { //nolint:gosec // /run/kudurru is shared with the keepalived container
		return err
	}
	if err := edge.WriteFileAtomic(s.cfg.APIKeyOut, []byte(s.creds.APIKey+"\n"), 0o640); err != nil {
		return fmt.Errorf("dnsdist: write API key: %w", err)
	}
	if s.cfg.APIKeyGID >= 0 {
		if err := os.Chown(s.cfg.APIKeyOut, -1, s.cfg.APIKeyGID); err != nil {
			s.log.Warn("cannot chgrp the API key file; the keepalived probe may not read it", "file", s.cfg.APIKeyOut, "gid", s.cfg.APIKeyGID, "err", err)
		}
	}
	return nil
}

func (s *Supervisor) startChild() error {
	c := &edge.Child{Path: s.cfg.Binary, Args: s.Args(), Env: s.cfg.ChildEnv, StopTimeout: s.cfg.StopTimeout}
	if err := c.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.child = c
	s.status, s.statusErr = nil, nil
	s.mu.Unlock()
	s.log.Info("started dnsdist", "pid", c.Pid(), "cmd", s.cfg.Binary+" "+strings.Join(s.Args(), " "))
	return nil
}

func (s *Supervisor) stopChild() {
	s.mu.Lock()
	c := s.child
	s.mu.Unlock()
	if c != nil {
		c.Stop()
		s.log.Info("stopped dnsdist", "pid", c.Pid(), "exit", errString(c.Err()))
	}
}

func errString(err error) string {
	if err == nil {
		return "0"
	}
	return err.Error()
}

// restart drains, stops and starts dnsdist (v0.1 membership change path).
func (s *Supervisor) restart(ctx context.Context, reason string) error {
	s.mu.Lock()
	s.restarting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}()
	if s.cfg.Drain > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.Drain):
		}
	}
	s.stopChild()
	s.restarts.WithLabelValues(reason).Inc()
	return s.startChild()
}

// Signal forwards sig to dnsdist.
func (s *Supervisor) Signal(sig os.Signal) error {
	s.mu.Lock()
	c := s.child
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Signal(sig)
}

// Run renders, starts dnsdist and supervises it until ctx ends, then stops
// dnsdist (SIGTERM, StopTimeout, SIGKILL) and returns nil. It returns an
// error when the first configuration cannot be rendered or validated.
func (s *Supervisor) Run(ctx context.Context) error {
	creds, err := s.cfg.NewCredentials()
	if err != nil {
		return err
	}
	s.creds = creds
	s.api = &APIClient{BaseURL: "http://" + s.cfg.Options.API, Key: creds.APIKey}

	eps, err := s.cfg.Source.List(ctx)
	if err != nil {
		return fmt.Errorf("dnsdist: initial membership from %s: %w", s.cfg.Source.Name(), err)
	}
	eps = cluster.FilterServing(eps)
	if _, err := s.render(ctx, eps, nil); err != nil {
		return err
	}
	if err := s.writeAPIKey(); err != nil {
		return err
	}
	if err := s.startChild(); err != nil {
		return err
	}
	defer s.stopChild()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	members := make(chan []cluster.Endpoint, 1)
	wg.Go(func() {
		err := s.cfg.Source.Watch(ctx, func(e []cluster.Endpoint) {
			select {
			case <-members:
			default:
			}
			members <- e
		})
		if err != nil && ctx.Err() == nil {
			s.log.Error("membership watch ended", "source", s.cfg.Source.Name(), "err", err)
		}
	})
	certs := make(chan struct{}, 1)
	if s.cfg.TLSDir != "" {
		if err := s.watchDir(ctx, &wg, s.cfg.TLSDir, certs); err != nil {
			s.log.Warn("certificate watch disabled", "dir", s.cfg.TLSDir, "err", err)
		}
	}
	zones := make(chan []string, 1)
	if s.cfg.Zones != nil {
		wg.Go(func() { s.pollZones(ctx, zones) })
	}
	wg.Go(func() { s.pollStatus(ctx) })

	var (
		pendingEps   []cluster.Endpoint
		pendingZones []string
		pendingCert  bool
		haveEps      bool
		haveZones    bool
		timer        *time.Timer
		timerC       <-chan time.Time
		backoff      = 500 * time.Millisecond
	)
	s.mu.Lock()
	curZones := s.zones
	s.mu.Unlock()
	arm := func() {
		if timer == nil {
			timer = time.NewTimer(s.cfg.Debounce)
			timerC = timer.C
		}
	}
	for {
		s.mu.Lock()
		childDone := s.child.Done()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case e := <-members:
			pendingEps, haveEps = cluster.FilterServing(e), true
			arm()
		case z := <-zones:
			pendingZones, haveZones = z, true
			arm()
		case <-certs:
			pendingCert = true
			arm()
		case <-timerC:
			timer, timerC = nil, nil
			s.mu.Lock()
			nextEps, nextZones := s.endpoints, curZones
			s.mu.Unlock()
			if haveEps {
				nextEps = pendingEps
			}
			if haveZones {
				nextZones = pendingZones
			}
			reason := "membership"
			if haveZones && !haveEps {
				reason = "zones"
			}
			haveEps, haveZones = false, false
			changed, err := s.render(ctx, nextEps, nextZones)
			if err != nil {
				s.log.Error("re-render failed; dnsdist keeps the previous configuration", "err", err)
				continue
			}
			curZones = nextZones
			if !changed && !pendingCert {
				continue
			}
			if !changed {
				reason = "certificates"
				// Certificates are read at start; check the unchanged
				// file against the new pair before restarting.
				if _, err := edge.Check(ctx, s.cfg.Binary, "--check-config", "-C", s.MainPath()); err != nil {
					s.log.Error("certificate change failed --check-config; not restarting", "err", err)
					pendingCert = false
					continue
				}
			}
			pendingCert = false
			s.log.Info("restarting dnsdist", "reason", reason)
			if err := s.restart(ctx, reason); err != nil {
				s.log.Error("restart failed", "err", err)
			}
		case <-childDone:
			s.log.Error("dnsdist exited unexpectedly; restarting", "exit", errString(s.child.Err()), "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			s.restarts.WithLabelValues("exit").Inc()
			if err := s.startChild(); err != nil {
				s.log.Error("dnsdist restart failed", "err", err)
			}
			continue
		}
		if s.child.Running() {
			backoff = 500 * time.Millisecond
		}
	}
}

// watchDir sends on ch whenever a file in dir changes (Kubernetes Secret
// volumes swap a ..data symlink, which shows up as a create in dir).
func (s *Supervisor) watchDir(ctx context.Context, wg *sync.WaitGroup, dir string, ch chan<- struct{}) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}
	wg.Go(func() {
		defer func() { _ = w.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				s.log.Info("certificate directory changed", "file", ev.Name, "op", ev.Op.String())
				select {
				case ch <- struct{}{}:
				default:
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				s.log.Warn("certificate watch error", "err", err)
			}
		}
	})
	return nil
}

// pollStatus refreshes the dnsdist API status that readiness and
// kudurru_edge_backend_up read.
func (s *Supervisor) pollStatus(ctx context.Context) {
	t := time.NewTicker(s.cfg.StatusPoll)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := s.api.Status(rctx)
		cancel()
		s.mu.Lock()
		s.status, s.statusErr, s.statusAt = st, err, time.Now()
		s.mu.Unlock()
		s.backendUp.Reset()
		if st != nil {
			for _, b := range st.Servers {
				v := 0.0
				if b.Up() {
					v = 1
				}
				s.backendUp.WithLabelValues(b.Name).Set(v)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollZones polls every backend's /internal/zones. A serial change
// flushes the zone from the authoritative packet cache (when cached); a
// change of the zone set is sent on ch so the routing rule is re-rendered.
func (s *Supervisor) pollZones(ctx context.Context, ch chan []string) {
	t := time.NewTicker(s.cfg.ZonePoll)
	defer t.Stop()
	serials := map[string]map[string]uint32{} // backend address → zone → serial
	unsupported := map[string]bool{}
	var lastSet []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		eps := slices.Clone(s.endpoints)
		s.mu.Unlock()
		set := map[string]bool{}
		flush := map[string]bool{}
		polled := 0
		for _, e := range eps {
			if e.InternalAddress == "" {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			zl, err := s.cfg.Zones.Zones(rctx, e.InternalAddress)
			cancel()
			if err != nil {
				if errors.Is(err, internalapi.ErrZonesUnsupported) {
					if !unsupported[e.Address] {
						unsupported[e.Address] = true
						s.log.Info("backend has no /internal/zones yet; zone routing uses --authoritative-zones only", "backend", e.Address)
					}
				} else {
					s.log.Debug("zone poll failed", "backend", e.Address, "err", err)
				}
				continue
			}
			polled++
			prev := serials[e.Address]
			next := map[string]uint32{}
			for _, z := range zl.Zones {
				name := edge.NormalizeZones([]string{z.Name})
				if len(name) == 0 {
					continue
				}
				next[name[0]] = z.Serial
				set[name[0]] = true
				if old, ok := prev[name[0]]; ok && old != z.Serial {
					flush[name[0]] = true
				}
			}
			serials[e.Address] = next
		}
		if polled == 0 {
			continue
		}
		if s.cfg.Options.AuthoritativeCache {
			for z := range flush {
				rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := s.api.FlushCache(rctx, edge.PoolAuthoritative, z, true)
				cancel()
				if err != nil {
					s.log.Warn("authoritative cache flush failed", "zone", z, "err", err)
					continue
				}
				s.flushes.Inc()
				s.log.Info("flushed authoritative cache after serial change", "zone", z)
			}
		}
		names := make([]string, 0, len(set))
		for z := range set {
			names = append(names, z)
		}
		names = edge.NormalizeZones(names)
		if !slices.Equal(names, lastSet) {
			lastSet = names
			select {
			case <-ch:
			default:
			}
			ch <- names
		}
	}
}

// Ready reports whether dnsdist runs and its API reports at least one
// backend up (§3.6 step 3: Kubernetes probes the supervisor).
func (s *Supervisor) Ready() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.restarting:
		return false, "restarting dnsdist"
	case s.child == nil || !s.child.Running():
		return false, "dnsdist is not running"
	case s.statusErr != nil:
		return false, "dnsdist API: " + s.statusErr.Error()
	case s.status == nil:
		return false, "waiting for the dnsdist API"
	case time.Since(s.statusAt) > 5*s.cfg.StatusPoll+2*time.Second:
		return false, "dnsdist API status is stale"
	}
	if n := s.status.UpCount(); n < 1 {
		return false, fmt.Sprintf("0 of %d backends up", len(s.status.Servers))
	}
	return true, fmt.Sprintf("%d of %d backends up", s.status.UpCount(), len(s.status.Servers))
}

// Handler serves /healthz/live, /healthz/ready, /status and /metrics on
// the supervisor's port (:8084).
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, _ *http.Request) {
		ok, msg := s.Ready()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write([]byte(msg + "\n"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		body := map[string]any{"endpoints": s.endpoints, "authoritativeZones": s.zones, "config": s.MainPath()}
		if s.status != nil {
			body["dnsdist"] = s.status
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{}))
	return mux
}

// Serve runs Handler on addr until ctx ends.
func (s *Supervisor) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
